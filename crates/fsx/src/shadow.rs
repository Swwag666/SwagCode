/*! Теневые снимки: откат там, где git нет.

`git::create_checkpoint` отдаёт `Ok(None)` вне репозитория (это записано
тестом `checkpoint_is_none_outside_repo`), то есть в обычной папке правки хода
необратимы полностью: агент правит файл, владелец видит результат и вернуть
ничего не может. Здесь тот же контракт, но копией дерева: снимок уезжает в
`~/.swagcod/shadow/<id>/tree/`, а восстановление возвращает содержимое файлов и
НЕ удаляет созданное после снимка - ровно как git-путь, где `restore` это не
`clean` и новое перечисляется в отчёте.

## Цена

Снимок делается на каждый ход, поэтому его стоимость ограничена тремя
пределами (файлы, размер файла, суммарный объём) и списком каталогов, которые
не копируются: `node_modules` и `target` - это гигабайты не-исходников, а
страховка нужна для правок агента, не для сборочного мусора. Упёрлась в предел -
снимок урезается, и `meta.json` это фиксирует, а не делает вид, что снимок полон.

## Жизнь

Каждый ход в не-git папке оставлял бы копию дерева навсегда, поэтому старые
снимки выметаются по возрасту. Старые точки отката исчезают - это цена, и она
названа здесь, а не выясняется в момент, когда откат понадобился.

## Корень - параметр, а не глобальная настройка

Функции с суффиксом `_in` принимают корень снимков явно, а `create`/`restore`
подставляют `shadow_root()`. Это не украшение: тесты идут параллельно в одном
процессе, и переменная окружения была бы общим изменяемым состоянием, из-за
которого один тест складывал бы снимки в корень другого.
!*/

use std::path::{Path, PathBuf};

use serde::{Deserialize, Serialize};

use crate::git::{RestoreReport, REPORT_FILE_CAP};
use crate::FsxError;

/// Префикс теневого снимка в поле `checkpoint_sha`.
pub const SHADOW_PREFIX: &str = "shadow:";

/// Каталоги, которые не копируются (по имени, без учёта регистра).
const SKIP_DIRS: &[&str] = &[
    ".git",
    "node_modules",
    "target",
    "dist",
    "build",
    ".next",
    ".nuxt",
    ".svelte-kit",
    ".venv",
    "venv",
    "__pycache__",
    ".cache",
    ".pytest_cache",
    ".mypy_cache",
    ".tox",
    "coverage",
    "vendor",
    ".idea",
    ".vscode",
];

/// Предел числа файлов в снимке.
pub const SHADOW_MAX_FILES: usize = 20_000;
/// Предел размера одного файла: больше - это не исходник.
pub const SHADOW_MAX_FILE_BYTES: u64 = 2 * 1024 * 1024;
/// Предел суммарного объёма снимка.
pub const SHADOW_MAX_TOTAL_BYTES: u64 = 128 * 1024 * 1024;
/// Сколько дней живут теневые снимки.
pub const SHADOW_KEEP_DAYS: u64 = 30;

#[derive(Debug, Serialize, Deserialize)]
struct ShadowMeta {
    cwd: String,
    created_ms: u64,
    files: usize,
    bytes: u64,
    /// Снимок урезан пределами: содержимое неполно.
    truncated: bool,
}

/// Корень теневых снимков: `~/.swagcod/shadow` или `SWAGCOD_SHADOW_DIR`.
pub fn shadow_root() -> PathBuf {
    if let Ok(d) = std::env::var("SWAGCOD_SHADOW_DIR") {
        if !d.trim().is_empty() {
            return PathBuf::from(d);
        }
    }
    let home = std::env::var("USERPROFILE")
        .or_else(|_| std::env::var("HOME"))
        .unwrap_or_else(|_| ".".into());
    PathBuf::from(home).join(".swagcod").join("shadow")
}

/// Похож ли идентификатор на теневой снимок.
///
/// Проверка формы обязательна: значение приходит из базы и подставляется в путь
/// на диске, поэтому `shadow:../../` не должен доехать до файловой системы.
pub fn is_shadow(id: &str) -> bool {
    shadow_dir_name(id).is_some()
}

/// Имя каталога снимка по идентификатору.
///
/// Идентификатор и имя каталога - разные вещи: в базе лежит `shadow:<hex>`, а на
/// диске каталог называется только `<hex>`. Двоеточие в имени каталога на
/// Windows запрещено (`os error 123`), и снимок падал бы на каждом ходу в
/// не-git папке. Это поймали тесты, а не владелец.
pub(crate) fn shadow_dir_name(id: &str) -> Option<&str> {
    let body = id.trim().strip_prefix(SHADOW_PREFIX)?;
    if body.len() == 16 && body.chars().all(|c| c.is_ascii_hexdigit()) {
        Some(body)
    } else {
        None
    }
}

fn shadow_id(cwd: &Path) -> String {
    use std::hash::{Hash, Hasher};
    let mut h = std::collections::hash_map::DefaultHasher::new();
    cwd.to_string_lossy().hash(&mut h);
    std::process::id().hash(&mut h);
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_nanos())
        .unwrap_or(0)
        .hash(&mut h);
    format!("{}{:016x}", SHADOW_PREFIX, h.finish())
}

fn now_ms() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_millis() as u64)
        .unwrap_or(0)
}

fn skip_dir(name: &str) -> bool {
    SKIP_DIRS.iter().any(|d| d.eq_ignore_ascii_case(name))
}

/// Относительный путь в виде, пригодном для отчёта и для сравнения.
fn rel_string(rel: &Path) -> String {
    rel.to_string_lossy().replace('\\', "/")
}

/// Сделать теневой снимок в корне по умолчанию.
pub fn create(cwd: &Path) -> Result<Option<String>, FsxError> {
    create_in(&shadow_root(), cwd)
}

/// Сделать теневой снимок дерева в заданном корне.
///
/// `Ok(None)` - снимать нечего: каталога нет. Ошибка возвращается только если
/// снимок начался и сломался, и тогда недоделанный снимок убирается: оставить
/// его значит записать ходу страховку, которой нет.
pub fn create_in(root: &Path, cwd: &Path) -> Result<Option<String>, FsxError> {
    if !cwd.is_dir() {
        return Ok(None);
    }
    let id = shadow_id(cwd);
    /* Каталог называется отпечатком без префикса: двоеточие в имени каталога на
    Windows запрещено, а идентификатор в базе обязан различать виды снимков. */
    let dir = root.join(id.strip_prefix(SHADOW_PREFIX).unwrap_or(&id));
    let tree = dir.join("tree");
    std::fs::create_dir_all(&tree)
        .map_err(|e| FsxError::Other(format!("тени: каталог {}: {e}", tree.display())))?;

    let (files, bytes, truncated) = match build(cwd, &tree) {
        Ok(counts) => counts,
        Err(e) => {
            let _ = std::fs::remove_dir_all(&dir);
            return Err(e);
        }
    };

    let meta = ShadowMeta {
        cwd: cwd.to_string_lossy().to_string(),
        created_ms: now_ms(),
        files,
        bytes,
        truncated,
    };
    let meta_text = serde_json::to_string(&meta)
        .map_err(|e| FsxError::Other(format!("тени: метаданные: {e}")))?;
    std::fs::write(dir.join("meta.json"), meta_text)
        .map_err(|e| FsxError::Other(format!("тени: запись meta.json: {e}")))?;

    prune_old_in(root);
    Ok(Some(id))
}

/// Скопировать дерево в снимок. Возвращает (файлов, байт, урезан ли).
fn build(cwd: &Path, tree: &Path) -> Result<(usize, u64, bool), FsxError> {
    let mut files = 0usize;
    let mut bytes = 0u64;
    let mut truncated = false;

    let walk = walkdir::WalkDir::new(cwd)
        .follow_links(false)
        .into_iter()
        .filter_entry(|e| {
            // Корень фильтровать нельзя: с него обход начинается.
            e.depth() == 0
                || !(e.file_type().is_dir() && skip_dir(&e.file_name().to_string_lossy()))
        });

    for entry in walk {
        let entry = match entry {
            Ok(e) => e,
            Err(_) => {
                /* Нечитаемый узел (права, оборванная ссылка) - не повод терять
                весь снимок, но и не повод считать его полным. */
                truncated = true;
                continue;
            }
        };
        if !entry.file_type().is_file() {
            continue;
        }
        let Ok(rel) = entry.path().strip_prefix(cwd) else {
            continue;
        };
        let len = entry.metadata().map(|m| m.len()).unwrap_or(0);
        if len > SHADOW_MAX_FILE_BYTES
            || bytes.saturating_add(len) > SHADOW_MAX_TOTAL_BYTES
            || files >= SHADOW_MAX_FILES
        {
            truncated = true;
            continue;
        }
        let dest = tree.join(rel);
        if let Some(parent) = dest.parent() {
            std::fs::create_dir_all(parent)
                .map_err(|e| FsxError::Other(format!("тени: каталог {}: {e}", parent.display())))?;
        }
        std::fs::copy(entry.path(), &dest)
            .map_err(|e| FsxError::Other(format!("тени: копия {}: {e}", rel.display())))?;
        files += 1;
        bytes = bytes.saturating_add(len);
    }

    Ok((files, bytes, truncated))
}

/// Вернуть содержимое файлов из теневого снимка в корне по умолчанию.
pub fn restore(cwd: &Path, id: &str) -> Result<RestoreReport, FsxError> {
    restore_in(&shadow_root(), cwd, id)
}

/// Вернуть содержимое файлов из теневого снимка заданного корня.
///
/// Созданное после снимка НЕ удаляется: откат не `clean`, и это сказано в
/// отчёте списком, а не умолчанием.
pub fn restore_in(root: &Path, cwd: &Path, id: &str) -> Result<RestoreReport, FsxError> {
    let id = id.trim();
    let Some(body) = shadow_dir_name(id) else {
        return Err(FsxError::Other(format!(
            "откат: {id:?} не похож на теневой снимок"
        )));
    };
    let tree = root.join(body).join("tree");
    if !tree.is_dir() {
        return Err(FsxError::Other(format!(
            "откат: теневой снимок {id} не найден в {}",
            tree.display()
        )));
    }
    if !cwd.is_dir() {
        return Err(FsxError::Other(format!(
            "откат: рабочая директория {} недоступна",
            cwd.display()
        )));
    }

    let mut restored: Vec<String> = Vec::new();
    for entry in walkdir::WalkDir::new(&tree)
        .into_iter()
        .filter_map(Result::ok)
    {
        if !entry.file_type().is_file() {
            continue;
        }
        let Ok(rel) = entry.path().strip_prefix(&tree) else {
            continue;
        };
        let dest = cwd.join(rel);
        /* Сравнение содержимым, а не временем правки: ход мог перезаписать файл
        тем же содержимым, и тогда возврат ничего не меняет - писать его в
        отчёт как возвращённый значило бы завышать отчёт. */
        let unchanged = match (std::fs::read(entry.path()), std::fs::read(&dest)) {
            (Ok(a), Ok(b)) => a == b,
            _ => false,
        };
        if unchanged {
            continue;
        }
        if let Some(parent) = dest.parent() {
            std::fs::create_dir_all(parent).map_err(|e| {
                FsxError::Other(format!("откат: каталог {}: {e}", parent.display()))
            })?;
        }
        std::fs::copy(entry.path(), &dest)
            .map_err(|e| FsxError::Other(format!("откат: возврат {}: {e}", rel.display())))?;
        restored.push(rel_string(rel));
    }

    // Что появилось после снимка: остаётся на месте и перечисляется честно.
    let walk = walkdir::WalkDir::new(cwd)
        .follow_links(false)
        .into_iter()
        .filter_entry(|e| {
            e.depth() == 0
                || !(e.file_type().is_dir() && skip_dir(&e.file_name().to_string_lossy()))
        })
        .filter_map(Result::ok);
    let mut untracked_left: Vec<String> = Vec::new();
    for entry in walk {
        if !entry.file_type().is_file() {
            continue;
        }
        let Ok(rel) = entry.path().strip_prefix(cwd) else {
            continue;
        };
        if !tree.join(rel).is_file() {
            untracked_left.push(rel_string(rel));
        }
    }

    restored.sort();
    untracked_left.sort();
    let truncated = restored.len() > REPORT_FILE_CAP || untracked_left.len() > REPORT_FILE_CAP;
    restored.truncate(REPORT_FILE_CAP);
    untracked_left.truncate(REPORT_FILE_CAP);

    Ok(RestoreReport {
        sha: id.to_string(),
        restored,
        untracked_left,
        truncated,
    })
}

/// Вымести снимки старше [`SHADOW_KEEP_DAYS`].
///
/// Ошибки глотаются намеренно: уборка не должна ронять снимок хода. Но она
/// входит в цену решения хранить копии деревьев, поэтому вызывается на каждом
/// создании, а не «когда-нибудь потом».
fn prune_old_in(root: &Path) {
    let Ok(entries) = std::fs::read_dir(root) else {
        return;
    };
    let Some(cutoff) = std::time::SystemTime::now()
        .checked_sub(std::time::Duration::from_secs(SHADOW_KEEP_DAYS * 86_400))
    else {
        return;
    };
    for entry in entries.filter_map(Result::ok) {
        let Ok(meta) = entry.metadata() else {
            continue;
        };
        let Ok(modified) = meta.modified() else {
            continue;
        };
        if modified < cutoff {
            let _ = std::fs::remove_dir_all(entry.path());
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Свой каталог на каждый вызов: тесты идут параллельно, общий каталог
    /// означал бы, что один тест видит файлы другого.
    fn tmp_dir(tag: &str) -> PathBuf {
        let dir = std::env::temp_dir().join(format!(
            "swagcod-shadow-{tag}-{}-{}",
            std::process::id(),
            now_ms()
        ));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).expect("тестовый каталог");
        dir
    }

    fn read_meta(root: &Path, id: &str) -> ShadowMeta {
        let name = shadow_dir_name(id).expect("id снимка");
        let text = std::fs::read_to_string(root.join(name).join("meta.json")).expect("meta.json");
        serde_json::from_str(&text).expect("meta.json разбирается")
    }

    #[test]
    fn shadow_id_shape_is_checked() {
        let id = shadow_id(Path::new("D:/work"));
        assert!(is_shadow(&id), "свой же id должен опознаваться: {id}");
        assert_eq!(id.len(), SHADOW_PREFIX.len() + 16);
        /* Форма проверяется до подстановки в путь: иначе значение из базы
        уехало бы в файловую систему. */
        assert!(!is_shadow("shadow:../../etc"));
        assert!(!is_shadow("shadow:zzzzzzzzzzzzzzzz"));
        assert!(!is_shadow("shadow:"));
        assert!(!is_shadow("0123456789abcdef0123456789abcdef01234567"));
    }

    #[test]
    fn dir_name_carries_no_prefix_because_windows_forbids_colon() {
        /* Регрессия, пойманная тестами на первом прогоне: с префиксом в имени
        каталога `create_dir_all` падал на Windows с os error 123, то есть
        снимок не создался бы ни на одном ходу в не-git папке. */
        let id = shadow_id(Path::new("D:/work"));
        let name = shadow_dir_name(&id).expect("имя каталога снимка");
        assert_eq!(name.len(), 16);
        assert!(
            !name.contains(':'),
            "двоеточие в имени каталога на Windows запрещено: {name}"
        );
        assert!(
            !name.contains(SHADOW_PREFIX),
            "каталог называется отпечатком без префикса: {name}"
        );
    }

    #[test]
    fn create_and_restore_bring_content_back() {
        let root = tmp_dir("root-restore");
        let dir = tmp_dir("restore-work");
        std::fs::write(dir.join("a.txt"), b"before").unwrap();
        std::fs::create_dir_all(dir.join("sub")).unwrap();
        std::fs::write(dir.join("sub/b.txt"), b"nested").unwrap();

        let id = create_in(&root, &dir)
            .unwrap()
            .expect("в обычной папке снимок всё равно есть");
        assert!(is_shadow(&id));

        std::fs::write(dir.join("a.txt"), b"after").unwrap();
        std::fs::remove_file(dir.join("sub/b.txt")).unwrap();

        let report = restore_in(&root, &dir, &id).unwrap();
        assert_eq!(std::fs::read(dir.join("a.txt")).unwrap(), b"before");
        assert_eq!(std::fs::read(dir.join("sub/b.txt")).unwrap(), b"nested");
        assert!(report.restored.contains(&"a.txt".to_string()));
        assert!(report.restored.contains(&"sub/b.txt".to_string()));
        assert!(!report.truncated);
        let _ = std::fs::remove_dir_all(&dir);
        let _ = std::fs::remove_dir_all(&root);
    }

    #[test]
    fn restore_does_not_delete_files_created_after_snapshot() {
        let root = tmp_dir("root-keep-new");
        let dir = tmp_dir("keep-new-work");
        std::fs::write(dir.join("a.txt"), b"x").unwrap();
        let id = create_in(&root, &dir).unwrap().unwrap();

        std::fs::write(dir.join("fresh.txt"), b"created later").unwrap();
        let report = restore_in(&root, &dir, &id).unwrap();

        assert!(
            dir.join("fresh.txt").exists(),
            "откат не clean: созданное после снимка не удаляется"
        );
        assert!(
            report.untracked_left.contains(&"fresh.txt".to_string()),
            "но и не молчит о нём: {:?}",
            report.untracked_left
        );
        let _ = std::fs::remove_dir_all(&dir);
        let _ = std::fs::remove_dir_all(&root);
    }

    #[test]
    fn unchanged_content_is_not_reported_as_restored() {
        let root = tmp_dir("root-unchanged");
        let dir = tmp_dir("unchanged-work");
        std::fs::write(dir.join("a.txt"), b"same").unwrap();
        std::fs::write(dir.join("b.txt"), b"before").unwrap();
        let id = create_in(&root, &dir).unwrap().unwrap();

        std::fs::write(dir.join("b.txt"), b"after").unwrap();
        let report = restore_in(&root, &dir, &id).unwrap();

        assert!(report.restored.contains(&"b.txt".to_string()));
        assert!(
            !report.restored.contains(&"a.txt".to_string()),
            "файл, который не менялся, не возвращался - и не должен попасть в отчёт: {:?}",
            report.restored
        );
        let _ = std::fs::remove_dir_all(&dir);
        let _ = std::fs::remove_dir_all(&root);
    }

    #[test]
    fn skipped_dirs_are_not_copied() {
        let root = tmp_dir("root-skip");
        let dir = tmp_dir("skip-work");
        std::fs::create_dir_all(dir.join("node_modules/pkg")).unwrap();
        std::fs::write(dir.join("node_modules/pkg/index.js"), b"junk").unwrap();
        std::fs::write(dir.join("src.rs"), b"real").unwrap();

        let id = create_in(&root, &dir).unwrap().unwrap();
        let tree = root.join(shadow_dir_name(&id).unwrap()).join("tree");
        assert!(
            tree.join("src.rs").is_file(),
            "исходник должен быть в снимке"
        );
        assert!(
            !tree.join("node_modules").exists(),
            "сборочный мусор не копируется: снимок идёт на каждый ход"
        );
        let _ = std::fs::remove_dir_all(&dir);
        let _ = std::fs::remove_dir_all(&root);
    }

    #[test]
    fn oversize_file_is_left_out_and_marked() {
        let root = tmp_dir("root-oversize");
        let dir = tmp_dir("oversize-work");
        let big = vec![0u8; (SHADOW_MAX_FILE_BYTES + 1) as usize];
        std::fs::write(dir.join("big.bin"), &big).unwrap();
        std::fs::write(dir.join("small.txt"), b"ok").unwrap();

        let id = create_in(&root, &dir).unwrap().unwrap();
        let tree = root.join(shadow_dir_name(&id).unwrap()).join("tree");
        assert!(tree.join("small.txt").is_file());
        assert!(
            !tree.join("big.bin").exists(),
            "файл сверх предела не копируется"
        );

        /* Урезанность видна в метаданных, а не додумывается: снимок не должен
        выдавать себя за полный. */
        let meta = read_meta(&root, &id);
        assert!(meta.truncated, "снимок обязан отметить, что он урезан");
        assert_eq!(meta.files, 1);
        let _ = std::fs::remove_dir_all(&dir);
        let _ = std::fs::remove_dir_all(&root);
    }

    #[test]
    fn missing_snapshot_is_an_error_not_a_silent_noop() {
        let root = tmp_dir("root-missing");
        let dir = tmp_dir("missing-work");
        std::fs::write(dir.join("a.txt"), b"x").unwrap();

        let err = restore_in(&root, &dir, "shadow:0123456789abcdef").unwrap_err();
        let text = format!("{err}");
        assert!(text.contains("не найден"), "честная причина: {text}");
        assert_eq!(
            std::fs::read(dir.join("a.txt")).unwrap(),
            b"x",
            "неудачный откат не должен трогать файлы"
        );
        let _ = std::fs::remove_dir_all(&dir);
        let _ = std::fs::remove_dir_all(&root);
    }

    #[test]
    fn a_bad_id_is_refused_before_it_reaches_the_disk() {
        let root = tmp_dir("root-bad-id");
        let dir = tmp_dir("bad-id-work");
        let err = restore_in(&root, &dir, "shadow:../../secret").unwrap_err();
        let text = format!("{err}");
        assert!(
            text.contains("не похож"),
            "форма проверяется первой: {text}"
        );
        let _ = std::fs::remove_dir_all(&dir);
        let _ = std::fs::remove_dir_all(&root);
    }
}
