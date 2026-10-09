/*! Git-чекпоинты ходов (F-5): снимок worktree до хода и откат к нему.

Агент правит файлы. Если ход ушёл не туда, у владельца должен быть способ
вернуть всё «как было до хода» — без ручного `git checkout` по памяти и без
потери собственных правок, сделанных между ходами.

## Как делается снимок

`git stash create` (вариант из плана) не годится в одиночку: он снимает
только изменения отслеживаемых файлов, а агент регулярно создаёт новые.
Поэтому снимок собирается полным деревом, но так, чтобы не тронуть ничего
живого:

1. копия настоящего индекса во временный файл `<gitdir>/swagcod-ckpt-*.index`
   — копия даёт stat-кэш, поэтому `git add` хеширует только реально
   изменённые файлы, а не всё дерево (иначе снимок стоил бы как полный
   `git add` на каждом ходе);
2. `GIT_INDEX_FILE=<копия> git add -A -- .` — в снимок попадают и новые
   файлы (кроме gitignore), настоящий индекс не трогается;
3. `git write-tree` → `git commit-tree` (родитель — HEAD, если он есть) —
   объект коммита без единой ссылки: ни одна ветка, ни индекс, ни worktree
   не меняются. Объект висит dangling и выметается обычным `git gc`.

Чистый worktree отдельного снимка не получает: состояние «до хода» уже
зафиксировано git, поэтому чекпоинтом становится sha HEAD. None бывает
только когда откатывать некуда — не репозиторий, git недоступен или в
репозитории ещё нет ни одного коммита.

## Что делает откат

`git restore --source=<sha> --worktree -- .` — содержимое файлов из
снимка возвращается в worktree в границах cwd сессии (песочница та же, что
у файловых инструментов). HEAD и индекс не трогаются: откат не создаёт
коммитов и не теряет историю git.

Честное ограничение: файлы, созданные ПОСЛЕ снимка, не удаляются —
`restore` не `clean`. Удаление файлов, которых пользователь, возможно,
касался сам, было бы хуже неотменённого хода, поэтому они перечисляются в
отчёте (`untracked_left`), и человек решает сам.
!*/

use std::path::{Path, PathBuf};
use std::process::Stdio;
use std::time::Duration;

use crate::FsxError;

/// Таймаут тяжёлых операций снимка (add/write-tree) на больших репах.
const SNAPSHOT_TIMEOUT: Duration = Duration::from_secs(120);
/// Таймаут лёгких операций (status/diff/restore/rev-parse).
const QUICK_TIMEOUT: Duration = Duration::from_secs(30);
/// Потолок списков в отчёте: траектория не должна тонуть в перечислении.
const REPORT_FILE_CAP: usize = 200;

/// Команда git без всплывающего консольного окна на Windows.
fn git_cmd(cwd: &Path) -> tokio::process::Command {
    let mut cmd = tokio::process::Command::new("git");
    cmd.current_dir(cwd)
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    // Идентичность нужна только для commit-tree; своей не требуем —
    // снимок служебный, автор у него SwagCod.
    cmd.env("GIT_AUTHOR_NAME", "SwagCod")
        .env("GIT_AUTHOR_EMAIL", "swagcod@localhost")
        .env("GIT_COMMITTER_NAME", "SwagCod")
        .env("GIT_COMMITTER_EMAIL", "swagcod@localhost");
    #[cfg(windows)]
    cmd.creation_flags(0x0800_0000); // CREATE_NO_WINDOW
    cmd
}

/// Запустить git и вернуть stdout, либо ошибку с stderr внутри.
async fn git_out(cwd: &Path, args: &[&str], timeout: Duration) -> Result<String, FsxError> {
    let res = tokio::time::timeout(timeout, git_cmd(cwd).args(args).output()).await;
    let out = match res {
        Ok(Ok(o)) => o,
        Ok(Err(e)) => return Err(FsxError::Other(format!("git: {e}"))),
        Err(_) => {
            return Err(FsxError::Other(format!(
                "git: таймаут {} с на `{}`",
                timeout.as_secs(),
                args.join(" ")
            )))
        }
    };
    if !out.status.success() {
        let err = String::from_utf8_lossy(&out.stderr);
        let err = err.trim();
        return Err(FsxError::Other(format!(
            "git {}: {err}",
            args.first().copied().unwrap_or("?")
        )));
    }
    Ok(String::from_utf8_lossy(&out.stdout).into_owned())
}

/// Это git-репозиторий (и git вообще доступен)?
pub async fn is_repo(cwd: &Path) -> bool {
    git_out(cwd, &["rev-parse", "--is-inside-work-tree"], QUICK_TIMEOUT)
        .await
        .map(|s| s.trim() == "true")
        .unwrap_or(false)
}

/// Абсолютный путь к .git (для временного индекса).
async fn git_dir(cwd: &Path) -> Result<PathBuf, FsxError> {
    let out = git_out(cwd, &["rev-parse", "--absolute-git-dir"], QUICK_TIMEOUT).await?;
    let p = PathBuf::from(out.trim());
    if p.as_os_str().is_empty() {
        return Err(FsxError::Other("git: пустой --absolute-git-dir".into()));
    }
    Ok(p)
}

/// Sha HEAD, если в репозитории есть хоть один коммит.
async fn head_sha(cwd: &Path) -> Option<String> {
    git_out(cwd, &["rev-parse", "--verify", "HEAD"], QUICK_TIMEOUT)
        .await
        .ok()
        .map(|s| s.trim().to_string())
        .filter(|s| looks_like_sha(s))
}

/// Worktree грязный? Проверяем без записи в индекс (`--no-optional-locks`):
/// снимок не имеет права менять состояние репозитория даже побочно.
async fn is_dirty(cwd: &Path) -> Result<bool, FsxError> {
    let out = git_out(
        cwd,
        &["--no-optional-locks", "status", "--porcelain", "--", "."],
        QUICK_TIMEOUT,
    )
    .await?;
    Ok(!out.trim().is_empty())
}

/// Валиден ли sha как объект коммита (проверка до подстановки в аргументы).
pub fn looks_like_sha(s: &str) -> bool {
    let t = s.trim();
    (40..=64).contains(&t.len()) && t.chars().all(|c| c.is_ascii_hexdigit())
}

/// Снимок worktree до хода.
///
/// Грязное дерево → собственный dangling-коммит со срезом всего дерева.
/// Чистое дерево → sha HEAD: состояние «до хода» уже зафиксировано git,
/// дублировать его объектом незачем, а откат к нему работает так же.
/// `Ok(None)` — снимка нет: не репозиторий, git недоступен или в репозитории
/// ещё нет ни одного коммита (нечего откатывать).
///
/// Ошибка возвращается только если git начал работу и сломался: молча
/// проглотить её значит оставить ход без страховки и сделать вид, что она есть.
pub async fn create_checkpoint(cwd: &Path) -> Result<Option<String>, FsxError> {
    if !is_repo(cwd).await {
        return Ok(None);
    }
    if !is_dirty(cwd).await? {
        return Ok(head_sha(cwd).await);
    }
    let dir = git_dir(cwd).await?;

    /* Временный индекс: копия настоящего (ради stat-кэша) или пустой файл,
    если настоящего нет (свежий репозиторий без первого коммита). */
    let stamp = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_nanos())
        .unwrap_or(0);
    let tmp_index = dir.join(format!("swagcod-ckpt-{}-{stamp}.index", std::process::id()));
    let real_index = dir.join("index");
    if real_index.exists() {
        if let Err(e) = std::fs::copy(&real_index, &tmp_index) {
            return Err(FsxError::Other(format!(
                "чекпоинт: копия индекса {}: {e}",
                tmp_index.display()
            )));
        }
    }

    let result = snapshot_with_index(cwd, &tmp_index).await;
    // Временный индекс убираем всегда: мусор в .git переживает ходы.
    let _ = std::fs::remove_file(&tmp_index);
    result
}

async fn snapshot_with_index(cwd: &Path, tmp_index: &Path) -> Result<Option<String>, FsxError> {
    // 1. Наполнить временный индекс всем деревом (в границах cwd).
    let mut add = git_cmd(cwd);
    add.env("GIT_INDEX_FILE", tmp_index)
        .args(["add", "-A", "--", "."]);
    let add_out = tokio::time::timeout(SNAPSHOT_TIMEOUT, add.output())
        .await
        .map_err(|_| FsxError::Other("чекпоинт: таймаут git add".into()))?
        .map_err(|e| FsxError::Other(format!("чекпоинт: git add: {e}")))?;
    if !add_out.status.success() {
        return Err(FsxError::Other(format!(
            "чекпоинт: git add: {}",
            String::from_utf8_lossy(&add_out.stderr).trim()
        )));
    }

    // 2. Дерево из временного индекса.
    let mut wt = git_cmd(cwd);
    wt.env("GIT_INDEX_FILE", tmp_index).arg("write-tree");
    let tree_out = tokio::time::timeout(QUICK_TIMEOUT, wt.output())
        .await
        .map_err(|_| FsxError::Other("чекпоинт: таймаут git write-tree".into()))?
        .map_err(|e| FsxError::Other(format!("чекпоинт: git write-tree: {e}")))?;
    if !tree_out.status.success() {
        return Err(FsxError::Other(format!(
            "чекпоинт: git write-tree: {}",
            String::from_utf8_lossy(&tree_out.stderr).trim()
        )));
    }
    let tree = String::from_utf8_lossy(&tree_out.stdout).trim().to_string();
    if tree.is_empty() {
        return Err(FsxError::Other(
            "чекпоинт: write-tree вернул пустое дерево".into(),
        ));
    }

    // 3. Объект коммита. Родитель — HEAD, если он есть: так снимок видно
    // в `git log --all`/`git fsck` как потомка текущей ветки, и gc не
    // унесёт его раньше времени. Refs не создаём — это не ветка.
    let head = head_sha(cwd).await;
    let mut args: Vec<&str> = vec!["commit-tree", tree.as_str()];
    if let Some(h) = head.as_deref() {
        args.push("-p");
        args.push(h);
    }
    args.push("-m");
    args.push("swagcod: checkpoint before turn");
    let sha = git_out(cwd, &args, QUICK_TIMEOUT).await?;
    let sha = sha.trim().to_string();
    if !looks_like_sha(&sha) {
        return Err(FsxError::Other(format!(
            "чекпоинт: commit-tree вернул не sha: {sha:?}"
        )));
    }
    Ok(Some(sha))
}

/// Отчёт об откате: что возвращено и что осталось нетронутым.
#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
pub struct RestoreReport {
    /// Sha снимка, к которому откатились.
    pub sha: String,
    /// Файлы, содержимое которых возвращено из снимка.
    pub restored: Vec<String>,
    /// Файлы, созданные после снимка: НЕ удалены (restore — не clean).
    pub untracked_left: Vec<String>,
    /// Списки обрезаны до [`REPORT_FILE_CAP`].
    pub truncated: bool,
}

impl RestoreReport {
    /// Человекочитаемая строка для траектории и flash-статуса.
    pub fn message(&self) -> String {
        let mut out = format!(
            "откат к снимку {}: возвращено файлов {}",
            &self.sha[..self.sha.len().min(10)],
            self.restored.len()
        );
        if !self.untracked_left.is_empty() {
            out.push_str(&format!(
                "; создано после снимка и НЕ удалено: {} ({})",
                self.untracked_left.len(),
                self.untracked_left
                    .iter()
                    .take(3)
                    .cloned()
                    .collect::<Vec<_>>()
                    .join(", ")
            ));
        }
        if self.truncated {
            out.push_str("; списки обрезаны");
        }
        out
    }
}

/// Откатить worktree к снимку.
///
/// HEAD и индекс не меняются: откат не создаёт коммит и не теряет историю.
/// `sha` проверяется дважды — на форму (hex 40..64) и на тип объекта
/// (`cat-file -t` = commit), потому что значение приходит из базы и
/// подставлять его в аргументы git непроверенным нельзя.
pub async fn restore_checkpoint(cwd: &Path, sha: &str) -> Result<RestoreReport, FsxError> {
    let sha = sha.trim();
    if !looks_like_sha(sha) {
        return Err(FsxError::Other(format!(
            "откат: {sha:?} не похож на sha коммита"
        )));
    }
    if !is_repo(cwd).await {
        return Err(FsxError::Other(
            "откат: рабочая директория сессии — не git-репозиторий".into(),
        ));
    }
    let kind = git_out(cwd, &["cat-file", "-t", sha], QUICK_TIMEOUT).await?;
    if kind.trim() != "commit" {
        return Err(FsxError::Other(format!(
            "откат: объект {sha} — {}, а не коммит снимка",
            kind.trim()
        )));
    }

    // Что именно изменится: список до restore, потому что после него
    // отличий от снимка уже нет.
    let diff_args: &[&str] = &["--no-optional-locks", "diff", "--name-only", sha, "--", "."];
    let restored = split_paths(
        &git_out(cwd, diff_args, QUICK_TIMEOUT)
            .await
            .unwrap_or_default(),
    );

    let source = format!("--source={sha}");
    git_out(
        cwd,
        &["restore", &source, "--worktree", "--", "."],
        SNAPSHOT_TIMEOUT,
    )
    .await?;

    // Новые файлы остаются: сообщаем честно, а не делаем вид, что откат
    // полный. Берём их из status после restore.
    let status = git_out(
        cwd,
        &["--no-optional-locks", "status", "--porcelain", "--", "."],
        QUICK_TIMEOUT,
    )
    .await
    .unwrap_or_default();
    let untracked_left: Vec<String> = status
        .lines()
        .filter(|l| l.starts_with("??"))
        .filter_map(|l| l.get(3..).map(|p| p.trim().trim_matches('"').to_string()))
        .collect();

    let truncated = restored.len() > REPORT_FILE_CAP || untracked_left.len() > REPORT_FILE_CAP;
    Ok(RestoreReport {
        sha: sha.to_string(),
        restored: cap(restored),
        untracked_left: cap(untracked_left),
        truncated,
    })
}

fn split_paths(raw: &str) -> Vec<String> {
    raw.lines()
        .map(|l| l.trim().trim_matches('"').to_string())
        .filter(|l| !l.is_empty())
        .collect()
}

fn cap(mut v: Vec<String>) -> Vec<String> {
    v.truncate(REPORT_FILE_CAP);
    v
}

#[cfg(test)]
mod tests {
    use super::*;
    // creation_flags у std::process::Command — из этого трейта.
    #[cfg(windows)]
    use std::os::windows::process::CommandExt;

    fn git_available() -> bool {
        std::process::Command::new("git")
            .arg("--version")
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .status()
            .map(|s| s.success())
            .unwrap_or(false)
    }

    struct Repo {
        dir: PathBuf,
    }

    impl Repo {
        async fn new(tag: &str) -> Self {
            let dir = std::env::temp_dir().join(format!(
                "swagcod-git-{tag}-{}-{:?}",
                std::process::id(),
                std::time::SystemTime::now()
                    .duration_since(std::time::UNIX_EPOCH)
                    .unwrap()
                    .as_nanos()
            ));
            std::fs::create_dir_all(&dir).unwrap();
            let init = |args: &[&str]| {
                let mut c = std::process::Command::new("git");
                c.current_dir(&dir).args(args);
                #[cfg(windows)]
                c.creation_flags(0x0800_0000);
                let out = c.output().unwrap();
                assert!(
                    out.status.success(),
                    "git {args:?}: {}",
                    String::from_utf8_lossy(&out.stderr)
                );
            };
            init(&["init", "-q"]);
            init(&["config", "user.email", "test@swagcod.local"]);
            init(&["config", "user.name", "SwagCod Test"]);
            init(&["config", "commit.gpgsign", "false"]);
            /* Глобальный core.autocrlf=true (норма на Windows) превратил бы
            LF в CRLF при restore — тест обязан быть детерминированным и не
            зависеть от настроек машины. */
            init(&["config", "core.autocrlf", "false"]);
            Self { dir }
        }

        fn write(&self, name: &str, body: &str) {
            let p = self.dir.join(name);
            if let Some(parent) = p.parent() {
                std::fs::create_dir_all(parent).unwrap();
            }
            std::fs::write(p, body).unwrap();
        }

        fn read(&self, name: &str) -> String {
            std::fs::read_to_string(self.dir.join(name)).unwrap_or_default()
        }

        fn commit_all(&self, msg: &str) {
            for args in [vec!["add", "-A"], vec!["commit", "-qm", msg]] {
                let mut c = std::process::Command::new("git");
                c.current_dir(&self.dir).args(&args);
                #[cfg(windows)]
                c.creation_flags(0x0800_0000);
                let out = c.output().unwrap();
                assert!(
                    out.status.success(),
                    "git {args:?}: {}",
                    String::from_utf8_lossy(&out.stderr)
                );
            }
        }
    }

    impl Drop for Repo {
        fn drop(&mut self) {
            let _ = std::fs::remove_dir_all(&self.dir);
        }
    }

    #[test]
    fn sha_shape_is_validated() {
        assert!(looks_like_sha(&"a".repeat(40)));
        assert!(looks_like_sha(&"0123456789abcdef".repeat(4)));
        assert!(!looks_like_sha("HEAD"));
        assert!(!looks_like_sha("--source=x"));
        assert!(!looks_like_sha(&"g".repeat(40)));
        assert!(!looks_like_sha(&"a".repeat(39)));
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn checkpoint_is_none_outside_repo() {
        if !git_available() {
            return;
        }
        let dir = std::env::temp_dir().join(format!("swagcod-norepo-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        // Не репозиторий: честно None, а не ошибка.
        let out = create_checkpoint(&dir).await.unwrap();
        assert_eq!(out, None);
        assert!(!is_repo(&dir).await);
        std::fs::remove_dir_all(&dir).ok();
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn clean_worktree_checkpoints_to_head() {
        if !git_available() {
            return;
        }
        let repo = Repo::new("clean").await;
        repo.write("a.txt", "one\n");
        repo.commit_all("init");
        assert!(is_repo(&repo.dir).await);
        let head = git_out(&repo.dir, &["rev-parse", "HEAD"], QUICK_TIMEOUT)
            .await
            .unwrap();
        // Чистое дерево: отдельный объект не создаём, чекпоинт = HEAD.
        let sha = create_checkpoint(&repo.dir).await.unwrap().unwrap();
        assert_eq!(sha, head.trim());
        // И откат к нему работает (ничего не меняя).
        let report = restore_checkpoint(&repo.dir, &sha).await.unwrap();
        assert!(report.restored.is_empty(), "{:?}", report.restored);
        assert_eq!(repo.read("a.txt"), "one\n");
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn repo_without_commits_has_no_checkpoint() {
        if !git_available() {
            return;
        }
        let dir = std::env::temp_dir().join(format!("swagcod-emptyrepo-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        let mut c = std::process::Command::new("git");
        c.current_dir(&dir).args(["init", "-q"]);
        #[cfg(windows)]
        c.creation_flags(0x0800_0000);
        assert!(c.output().unwrap().status.success());
        // Ни коммитов, ни файлов: откатывать некуда — честный None.
        assert_eq!(create_checkpoint(&dir).await.unwrap(), None);
        std::fs::remove_dir_all(&dir).ok();
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn checkpoint_captures_dirty_state_and_restore_reverts_it() {
        if !git_available() {
            return;
        }
        let repo = Repo::new("dirty").await;
        repo.write("a.txt", "до хода\n");
        repo.write("keep.txt", "не трогать\n");
        repo.commit_all("init");

        // Ход начался: есть грязь (правка + новый файл).
        repo.write("a.txt", "после хода\n");
        let sha = create_checkpoint(&repo.dir)
            .await
            .unwrap()
            .expect("грязный worktree обязан дать снимок");
        assert!(looks_like_sha(&sha));

        /* Снимок не тронул ни worktree, ни индекс, ни refs: файл всё ещё
        «после хода», HEAD на месте, ветка не появилась. */
        assert_eq!(repo.read("a.txt"), "после хода\n");
        let head = git_out(&repo.dir, &["rev-parse", "HEAD"], QUICK_TIMEOUT)
            .await
            .unwrap();
        assert_ne!(head.trim(), sha, "снимок — не HEAD");
        let branches = git_out(&repo.dir, &["branch", "--list"], QUICK_TIMEOUT)
            .await
            .unwrap();
        assert!(!branches.contains("swagcod"), "{branches}");

        // Ход «напортачил» дальше: правка, новый файл, удаление.
        repo.write("a.txt", "совсем сломали\n");
        repo.write("new.txt", "создан после снимка\n");
        std::fs::remove_file(repo.dir.join("keep.txt")).unwrap();

        let report = restore_checkpoint(&repo.dir, &sha).await.unwrap();
        assert_eq!(report.sha, sha);
        assert!(
            report.restored.iter().any(|p| p == "a.txt"),
            "a.txt в списке возвращённых: {:?}",
            report.restored
        );
        assert_eq!(repo.read("a.txt"), "после хода\n", "содержимое вернулось");
        assert_eq!(
            repo.read("keep.txt"),
            "не трогать\n",
            "удалённый файл воскрешён"
        );
        // Созданный после снимка файл НЕ удалён — и честно перечислен.
        assert_eq!(repo.read("new.txt"), "создан после снимка\n");
        assert!(
            report.untracked_left.iter().any(|p| p.contains("new.txt")),
            "{:?}",
            report.untracked_left
        );
        assert!(
            report.message().contains("откат к снимку"),
            "{}",
            report.message()
        );
        assert!(report.message().contains("new.txt"), "{}", report.message());

        // HEAD не сдвинулся: откат не создаёт коммитов.
        let head_after = git_out(&repo.dir, &["rev-parse", "HEAD"], QUICK_TIMEOUT)
            .await
            .unwrap();
        assert_eq!(head_after.trim(), head.trim());
        // Индекс не тронут откатом: staged-состояния не появилось.
        let staged = git_out(
            &repo.dir,
            &["--no-optional-locks", "diff", "--cached", "--name-only"],
            QUICK_TIMEOUT,
        )
        .await
        .unwrap();
        assert!(
            staged.trim().is_empty(),
            "индекс должен остаться пустым: {staged}"
        );
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn checkpoint_includes_untracked_file_content() {
        if !git_available() {
            return;
        }
        let repo = Repo::new("untracked").await;
        repo.write("tracked.txt", "base\n");
        repo.commit_all("init");
        // Файл не в git, но существовал ДО хода — снимок обязан его взять,
        // иначе правка агента в нём стала бы неотменяемой.
        repo.write("notes.md", "до хода\n");

        let sha = create_checkpoint(&repo.dir)
            .await
            .unwrap()
            .expect("неотслеживаемый файл — тоже грязь");
        repo.write("notes.md", "агент переписал\n");
        restore_checkpoint(&repo.dir, &sha).await.unwrap();
        assert_eq!(repo.read("notes.md"), "до хода\n");
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn restore_rejects_bad_sha_and_foreign_object() {
        if !git_available() {
            return;
        }
        let repo = Repo::new("badsha").await;
        repo.write("a.txt", "x\n");
        repo.commit_all("init");

        let e = restore_checkpoint(&repo.dir, "HEAD")
            .await
            .unwrap_err()
            .to_string();
        assert!(e.contains("не похож на sha"), "{e}");

        let e = restore_checkpoint(&repo.dir, &"0".repeat(40))
            .await
            .unwrap_err()
            .to_string();
        assert!(e.contains("git cat-file"), "{e}");

        // Blob (не коммит) — отклоняется, а не подставляется в restore.
        let blob = git_out(&repo.dir, &["hash-object", "-w", "a.txt"], QUICK_TIMEOUT).await;
        if let Ok(sha) = blob {
            let sha = sha.trim();
            if looks_like_sha(sha) {
                let e = restore_checkpoint(&repo.dir, sha)
                    .await
                    .unwrap_err()
                    .to_string();
                assert!(e.contains("а не коммит"), "{e}");
            }
        }
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn checkpoint_leaves_no_temp_index_behind() {
        if !git_available() {
            return;
        }
        let repo = Repo::new("tmpindex").await;
        repo.write("a.txt", "1\n");
        repo.commit_all("init");
        repo.write("a.txt", "2\n");
        let sha = create_checkpoint(&repo.dir).await.unwrap().unwrap();
        let dir = git_dir(&repo.dir).await.unwrap();
        let left: Vec<_> = std::fs::read_dir(&dir)
            .unwrap()
            .filter_map(|e| e.ok())
            .map(|e| e.file_name().to_string_lossy().to_string())
            .filter(|n| n.starts_with("swagcod-ckpt-"))
            .collect();
        assert!(left.is_empty(), "временный индекс не убран: {left:?}");
        // Снимок живёт в объектной базе и читается.
        let kind = git_out(&repo.dir, &["cat-file", "-t", &sha], QUICK_TIMEOUT)
            .await
            .unwrap();
        assert_eq!(kind.trim(), "commit");
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn checkpoint_ignores_gitignored_files() {
        if !git_available() {
            return;
        }
        let repo = Repo::new("ignored").await;
        repo.write(".gitignore", "build/\n");
        repo.write("a.txt", "1\n");
        repo.commit_all("init");
        repo.write("build/out.bin", "мусор\n");
        repo.write("a.txt", "2\n");
        let sha = create_checkpoint(&repo.dir).await.unwrap().unwrap();
        let tree = git_out(
            &repo.dir,
            &["ls-tree", "-r", "--name-only", &sha],
            QUICK_TIMEOUT,
        )
        .await
        .unwrap();
        assert!(tree.contains("a.txt"), "{tree}");
        assert!(
            !tree.contains("build/out.bin"),
            "gitignore обязан работать: {tree}"
        );
    }
}
