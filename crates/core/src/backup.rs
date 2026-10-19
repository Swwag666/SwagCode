/*! Резервные копии базы: политика без базы данных.

Сама копия делается средствами SQLite (`SqliteStore::backup_to`, `VACUUM INTO`),
а здесь живёт то, что решается до и после неё: когда копию пора делать, как её
назвать и сколько копий держать.

## Зачем

История сессий - единственное, что в SwagCod нельзя восстановить из репозитория.
Ночной автономный прогон пишет в неё часами, и испорченная база означала бы
потерю всей истории сразу. Один файл копии в сутки стоит копейки на диске и
снимает этот класс потерь целиком.

## Почему интервал, а не «на каждый запуск»

Приложение запускают по нескольку раз в день, и копия на каждый запуск - это
десятки одинаковых файлов. Интервал по умолчанию - сутки: при частых запусках
копия обновляется раз в день, при редких - при первом же запуске после срока.
!*/

use std::path::{Path, PathBuf};
use std::time::{Duration, SystemTime};

use crate::store::StoreResult;

/// Префикс имени копии. По нему же копии отличаются от прочих файлов в
/// каталоге, чтобы уборка не тронула чужое.
pub const BACKUP_PREFIX: &str = "swagcod-backup-";
/// Расширение копии.
pub const BACKUP_EXT: &str = "db";
/// Сколько копий держим по умолчанию.
pub const BACKUP_KEEP: usize = 7;
/// Минимальный интервал между копиями.
pub const BACKUP_INTERVAL: Duration = Duration::from_secs(24 * 60 * 60);

/// Имя файла копии по времени создания (мс от эпохи).
///
/// Время в имени - фиксированной ширины (13 цифр до 2286 года), поэтому
/// сортировка по имени совпадает с сортировкой по времени, и ротация может
/// обойтись без чтения метаданных.
pub fn backup_name(created_ms: u64) -> String {
    format!("{BACKUP_PREFIX}{created_ms:013}.{BACKUP_EXT}")
}

/// Пора ли делать копию.
///
/// `newest_mtime` - время самой свежей существующей копии (`None` - копий нет).
/// Копий нет - делаем всегда: пустая история копии не требует, но и запрещать
/// её нельзя, иначе первая копия появится только через сутки после установки.
pub fn should_backup(
    newest_mtime: Option<SystemTime>,
    now: SystemTime,
    interval: Duration,
) -> bool {
    let Some(newest) = newest_mtime else {
        return true;
    };
    match now.duration_since(newest) {
        // Копия «из будущего» (сбитые часы, скопированный каталог) не повод
        // ждать сутки: считаем срок истёкшим.
        Err(_) => true,
        Ok(age) => age >= interval,
    }
}

/// Каталог копий рядом с базой: `<каталог базы>/backups`.
pub fn default_dir(db_path: &Path) -> PathBuf {
    match db_path.parent() {
        Some(dir) if !dir.as_os_str().is_empty() => dir.join("backups"),
        _ => PathBuf::from("backups"),
    }
}

/// Существующие копии в каталоге, от старых к новым.
pub fn list(dir: &Path) -> StoreResult<Vec<PathBuf>> {
    let mut found: Vec<PathBuf> = Vec::new();
    let Ok(entries) = std::fs::read_dir(dir) else {
        // Каталога нет - копий нет. Это не ошибка: он создаётся первой копией.
        return Ok(found);
    };
    for entry in entries.filter_map(Result::ok) {
        let name = entry.file_name().to_string_lossy().to_string();
        if name.starts_with(BACKUP_PREFIX) && name.ends_with(&format!(".{BACKUP_EXT}")) {
            found.push(entry.path());
        }
    }
    found.sort();
    Ok(found)
}

/// Время самой свежей копии, если копии есть.
pub fn newest_mtime(dir: &Path) -> Option<SystemTime> {
    let files = list(dir).ok()?;
    files
        .iter()
        .filter_map(|p| std::fs::metadata(p).ok()?.modified().ok())
        .max()
}

/// Оставить `keep` самых свежих копий, остальные удалить.
///
/// Возвращает число удалённых. Ошибка удаления отдельного файла не роняет
/// уборку: одна неудачная копия не повод оставить все остальные.
pub fn rotate(dir: &Path, keep: usize) -> StoreResult<usize> {
    let files = list(dir)?;
    if files.len() <= keep {
        return Ok(0);
    }
    let drop_count = files.len() - keep;
    let mut removed = 0usize;
    for path in files.iter().take(drop_count) {
        if std::fs::remove_file(path).is_ok() {
            removed += 1;
        }
    }
    Ok(removed)
}

/// Плановая копия: решает, нужна ли она сейчас, и делает её.
///
/// `Ok(None)` - копия не нужна или копировать нечего (базы ещё нет, бэкенд
/// удалённый). Ошибка возвращается только тогда, когда копию пора было снять и
/// она не снялась: молчаливый пропуск выдал бы отсутствие копии за «всё в
/// порядке».
pub fn run(
    db_path: &Path,
    dir: &Path,
    keep: usize,
    interval: Duration,
    now: SystemTime,
) -> StoreResult<Option<PathBuf>> {
    if !db_path.is_file() {
        return Ok(None);
    }
    if !should_backup(newest_mtime(dir), now, interval) {
        return Ok(None);
    }
    let created_ms = now
        .duration_since(SystemTime::UNIX_EPOCH)
        .map(|d| d.as_millis() as u64)
        .unwrap_or(0);
    let store = crate::store::SqliteStore::open(db_path)?;
    let dest = dir.join(backup_name(created_ms));
    store.backup_to(&dest)?;
    rotate(dir, keep)?;
    Ok(Some(dest))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn tmp_dir(tag: &str) -> PathBuf {
        let ms = SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map(|d| d.as_millis())
            .unwrap_or(0);
        let dir =
            std::env::temp_dir().join(format!("swagcod-backup-{tag}-{}-{ms}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).expect("тестовый каталог");
        dir
    }

    #[test]
    fn name_sorts_in_time_order() {
        let a = backup_name(1_700_000_000_000);
        let b = backup_name(1_700_000_000_001);
        assert!(a < b, "имя должно сортироваться по времени: {a} vs {b}");
        assert!(a.starts_with(BACKUP_PREFIX));
        assert!(a.ends_with(".db"));
    }

    #[test]
    fn first_backup_always_happens() {
        let now = SystemTime::UNIX_EPOCH + Duration::from_secs(1_700_000_000);
        assert!(
            should_backup(None, now, BACKUP_INTERVAL),
            "без копий копия делается сразу, а не через сутки после установки"
        );
    }

    #[test]
    fn interval_is_respected() {
        let now = SystemTime::UNIX_EPOCH + Duration::from_secs(1_700_000_000);
        let hour_ago = now - Duration::from_secs(3_600);
        let two_days_ago = now - Duration::from_secs(2 * 86_400);
        assert!(!should_backup(Some(hour_ago), now, BACKUP_INTERVAL));
        assert!(should_backup(Some(two_days_ago), now, BACKUP_INTERVAL));
    }

    #[test]
    fn backup_from_the_future_does_not_block_for_a_day() {
        /* Сбитые часы или скопированный каталог: ждать сутки молча нельзя,
        иначе копий не будет вовсе. */
        let now = SystemTime::UNIX_EPOCH + Duration::from_secs(1_700_000_000);
        let tomorrow = now + Duration::from_secs(86_400);
        assert!(should_backup(Some(tomorrow), now, BACKUP_INTERVAL));
    }

    #[test]
    fn rotation_keeps_the_newest_and_touches_nothing_else() {
        let dir = tmp_dir("rotate");
        for i in 0..4u64 {
            std::fs::write(dir.join(backup_name(1_700_000_000_000 + i)), b"x").unwrap();
        }
        // Чужой файл в том же каталоге уборка трогать не должна.
        std::fs::write(dir.join("notes.txt"), b"keep me").unwrap();

        let removed = rotate(&dir, 2).unwrap();
        assert_eq!(removed, 2);

        let left = list(&dir).unwrap();
        let names: Vec<String> = left
            .iter()
            .map(|p| p.file_name().unwrap().to_string_lossy().to_string())
            .collect();
        assert_eq!(
            names,
            vec![
                backup_name(1_700_000_000_002),
                backup_name(1_700_000_000_003)
            ],
            "остаются самые свежие"
        );
        assert!(dir.join("notes.txt").exists(), "чужие файлы не трогаются");
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn rotation_is_a_noop_when_within_limit() {
        let dir = tmp_dir("rotate-noop");
        std::fs::write(dir.join(backup_name(1)), b"x").unwrap();
        assert_eq!(rotate(&dir, 2).unwrap(), 0);
        assert_eq!(list(&dir).unwrap().len(), 1);
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn missing_directory_means_no_backups_not_an_error() {
        let dir = std::env::temp_dir().join("swagcod-backup-does-not-exist-xyz");
        let _ = std::fs::remove_dir_all(&dir);
        assert!(list(&dir).unwrap().is_empty());
        assert!(newest_mtime(&dir).is_none());
    }

    #[test]
    fn copy_is_a_real_database_and_not_an_empty_file() {
        /* Смысл копии - возможность открыть её. Проверяем не «файл создан», а
        «в файле лежит схема»: пустой файл прошёл бы первую проверку. */
        let dir = tmp_dir("roundtrip");
        let db = dir.join("swagcod.db");
        let store = crate::store::SqliteStore::open(&db).unwrap();

        let dest = dir.join("backups").join(backup_name(1_700_000_000_000));
        store.backup_to(&dest).unwrap();
        assert!(dest.is_file(), "копия создана");
        assert!(
            std::fs::metadata(&dest).unwrap().len() > 0,
            "копия не пустая"
        );

        let conn = rusqlite::Connection::open(&dest).unwrap();
        let tables: i64 = conn
            .query_row(
                "SELECT count(*) FROM sqlite_master WHERE type = 'table'",
                [],
                |r| r.get(0),
            )
            .unwrap();
        assert!(tables > 0, "в копии есть схема: таблиц {tables}");
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn existing_copy_is_not_overwritten() {
        let dir = tmp_dir("no-overwrite");
        let db = dir.join("swagcod.db");
        let store = crate::store::SqliteStore::open(&db).unwrap();
        let dest = dir.join(backup_name(1));

        store.backup_to(&dest).unwrap();
        let before = std::fs::metadata(&dest).unwrap().len();
        let err = store.backup_to(&dest).unwrap_err();
        assert!(
            format!("{err}").contains("уже существует"),
            "причина названа: {err}"
        );
        assert_eq!(
            std::fs::metadata(&dest).unwrap().len(),
            before,
            "прежняя копия не тронута"
        );
        let _ = std::fs::remove_dir_all(&dir);
    }
}
