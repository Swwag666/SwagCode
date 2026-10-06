/*! Краш-логи (этап B-8): паника оставляет след на диске.

Паника в GUI-приложении без лога — это «оно закрылось» и ничего больше.
Hook пишет `%LOCALAPPDATA%\SwagCod\crash-<ts>.log` с сообщением, местом и
бэктрейсом. Никакой внешней телеметрии (план B-8): файл остаётся на машине
пользователя, а экспорт диагностики уже умеет собирать всё нужное руками.

Формат намеренно текстовый и простой: лог читают и человек, и `grep`.
*/

use std::path::{Path, PathBuf};

/// Каталог краш-логов: `%LOCALAPPDATA%\SwagCod`, вне Windows — temp/SwagCod.
pub fn crash_dir() -> PathBuf {
    match std::env::var_os("LOCALAPPDATA") {
        Some(local) if !local.is_empty() => PathBuf::from(local).join("SwagCod"),
        _ => std::env::temp_dir().join("SwagCod"),
    }
}

/// Имя файла: монотонная метка времени, чтобы два краша в одну секунду
/// не перезаписали друг друга (now_ms + pid делают имя уникальным).
pub fn crash_file_name(ts_ms: u64) -> String {
    format!("crash-{ts_ms}-{}.log", std::process::id())
}

/// Содержимое лога — чистая функция, тестируется без паник и файловой системы.
pub fn render(ts_ms: u64, payload: &str, location: Option<&str>, backtrace: &str) -> String {
    format!(
        "SwagCod crash\nversion: {}\nts_ms: {ts_ms}\npayload: {payload}\nlocation: {}\n\nbacktrace:\n{backtrace}\n",
        env!("CARGO_PKG_VERSION"),
        location.unwrap_or("unknown"),
    )
}

/// Записать лог в указанный каталог. Ошибки не возвращаем наружу в hook:
/// падение внутри обработчика паники — худший из возможных исходов.
pub fn write_to(
    dir: &Path,
    ts_ms: u64,
    payload: &str,
    location: Option<&str>,
    backtrace: &str,
) -> std::io::Result<PathBuf> {
    std::fs::create_dir_all(dir)?;
    let path = dir.join(crash_file_name(ts_ms));
    std::fs::write(&path, render(ts_ms, payload, location, backtrace))?;
    Ok(path)
}

/// Поставить глобальный panic-hook. Ставится ДО запуска tauri::Builder,
/// поэтому ловит и паники setup, и паники внутри tokio-задач (hook
/// глобальный на процесс, а не на рантайм).
pub fn install_hook() {
    std::panic::set_hook(Box::new(|info| {
        let payload = info
            .payload()
            .downcast_ref::<&str>()
            .map(|s| (*s).to_string())
            .or_else(|| info.payload().downcast_ref::<String>().cloned())
            .unwrap_or_else(|| "<паника без сообщения>".to_string());
        let location = info
            .location()
            .map(|l| format!("{}:{}:{}", l.file(), l.line(), l.column()));
        let backtrace = std::backtrace::Backtrace::force_capture().to_string();
        if let Err(e) = write_to(
            &crash_dir(),
            swagcod_core::bus::now_ms(),
            &payload,
            location.as_deref(),
            &backtrace,
        ) {
            // Последний рубеж: хоть в stderr, но след оставить.
            eprintln!("crashlog: не записал лог: {e}; payload: {payload}");
        }
    }));
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn render_contains_all_parts() {
        let text = render(1234, "everything broke", Some("src/lib.rs:10:5"), "stack here");
        assert!(text.contains("payload: everything broke"));
        assert!(text.contains("location: src/lib.rs:10:5"));
        assert!(text.contains("stack here"));
        assert!(text.contains("ts_ms: 1234"));
        assert!(text.contains("version:"));
    }

    #[test]
    fn render_survives_missing_location() {
        let text = render(1, "oops", None, "bt");
        assert!(text.contains("location: unknown"));
    }

    #[test]
    fn write_to_creates_file_in_dir() {
        let dir = std::env::temp_dir().join(format!("swagcod-crashlog-test-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        let path = write_to(&dir, 42, "boom", Some("a.rs:1:1"), "frames").unwrap();
        assert!(path.exists());
        assert!(path.file_name().unwrap().to_string_lossy().starts_with("crash-42-"));
        let content = std::fs::read_to_string(&path).unwrap();
        assert!(content.contains("payload: boom"));
        std::fs::remove_dir_all(&dir).unwrap();
    }

    #[test]
    fn crash_file_name_is_unique_per_process() {
        let a = crash_file_name(1);
        let b = crash_file_name(1);
        assert_eq!(a, b, "в одном процессе имя детерминировано");
        assert!(a.contains(&std::process::id().to_string()));
    }
}
