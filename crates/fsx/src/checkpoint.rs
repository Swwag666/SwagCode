/*! Фасад снимков хода: git, если он есть, иначе тень.

Два пути отката живут за одним вызовом, потому что для вызывающего (драйвера
хода и команды отката) разницы быть не должно: снимок либо есть, либо его нет.
Различие видно в самом идентификаторе - сорок hex-символов у git-коммита и
`shadow:` у теневой копии, - и по нему же `restore` выбирает путь.

Порядок именно такой: git-снимок дешевле (дерево уже в объектной базе) и
переживает `git gc` дольше, чем живут теневые снимки, поэтому в репозитории
тень не создаётся вовсе.
!*/

use std::path::Path;

use crate::git::RestoreReport;
use crate::FsxError;

/// Пометка теневого снимка в `checkpoint_sha`.
pub use crate::shadow::SHADOW_PREFIX;

/// Снять состояние worktree до хода: git-объект или теневая копия.
///
/// `Ok(None)` остаётся тем же, чем был: снимать нечего (каталога нет).
pub async fn create(cwd: &Path) -> Result<Option<String>, FsxError> {
    if let Some(sha) = crate::git::create_checkpoint(cwd).await? {
        return Ok(Some(sha));
    }
    /* Копия дерева - синхронная файловая работа, и в асинхронном рантайме она
    заблокировала бы поток: уносим её в blocking-пул. */
    let cwd = cwd.to_path_buf();
    tokio::task::spawn_blocking(move || crate::shadow::create(&cwd))
        .await
        .map_err(|e| FsxError::Other(format!("тени: задача снимка упала: {e}")))?
}

/// Вернуть worktree к снимку, выбрав путь по виду идентификатора.
pub async fn restore(cwd: &Path, id: &str) -> Result<RestoreReport, FsxError> {
    if !crate::shadow::is_shadow(id) {
        return crate::git::restore_checkpoint(cwd, id).await;
    }
    let cwd = cwd.to_path_buf();
    let id = id.to_string();
    tokio::task::spawn_blocking(move || crate::shadow::restore(&cwd, &id))
        .await
        .map_err(|e| FsxError::Other(format!("тени: задача отката упала: {e}")))?
}

#[cfg(test)]
mod tests {
    use super::*;

    fn tmp_dir(tag: &str) -> std::path::PathBuf {
        let dir = std::env::temp_dir().join(format!(
            "swagcod-ckpt-{tag}-{}-{}",
            std::process::id(),
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .map(|d| d.as_millis())
                .unwrap_or(0)
        ));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).expect("тестовый каталог");
        dir
    }

    #[test]
    fn id_shape_decides_the_path() {
        /* Ошибка здесь увела бы откат не туда: сорок hex - git, `shadow:` - тень. */
        assert!(!crate::shadow::is_shadow(&"a".repeat(40)));
        assert!(crate::shadow::is_shadow("shadow:0123456789abcdef"));
    }

    #[tokio::test]
    async fn non_repo_folder_gets_a_snapshot_and_a_way_back() {
        /* Главная гарантия rev64: вне git откат есть. Тест идёт через фасад и
        настоящий корень снимков (в тесте его не подменить, не тронув
        глобальную переменную окружения), поэтому созданный снимок
        убирается за собой явно. */
        let dir = tmp_dir("nonrepo");
        std::fs::write(dir.join("a.txt"), b"before").unwrap();

        let id = create(&dir)
            .await
            .unwrap()
            .expect("в обычной папке снимок всё равно есть");
        assert!(
            crate::shadow::is_shadow(&id),
            "вне репозитория снимок теневой: {id}"
        );

        std::fs::write(dir.join("a.txt"), b"after").unwrap();
        let report = restore(&dir, &id).await.unwrap();
        assert_eq!(std::fs::read(dir.join("a.txt")).unwrap(), b"before");
        assert!(report.restored.contains(&"a.txt".to_string()));

        let name = crate::shadow::shadow_dir_name(&id).expect("имя каталога снимка");
        let _ = std::fs::remove_dir_all(crate::shadow::shadow_root().join(name));
        let _ = std::fs::remove_dir_all(&dir);
    }
}
