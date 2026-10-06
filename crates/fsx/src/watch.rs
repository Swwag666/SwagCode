/*! Watch рабочей директории с debounce 200 мс (этап B-4).

`notify` шлёт сырые события пачками: один save файла — это create+modify,
git-операция — десятки событий. Debounce-поток копит пути и отдаёт один
батч, когда поток событий притих на 200 мс: шина и UI не тонут в шуме,
а файловое дерево оживает без опроса.

Остановка — дроп [`WatchHandle`]: watcher закрывает канал, поток уходит сам.
*/

use std::path::{Path, PathBuf};
use std::sync::mpsc::{channel, Receiver, Sender};
use std::time::Duration;

use notify::{Event, RecommendedWatcher, RecursiveMode, Watcher};

use crate::FsxError;

/// Пауза тишины, после которой батч уходит потребителю (план B-4).
pub const DEBOUNCE_MS: u64 = 200;

/// Потолок путей в одном батче: git checkout тысяч файлов не должен
/// породить мегабайтное событие шины.
pub const MAX_BATCH_PATHS: usize = 256;

/// Живой watcher. Дроп ручки останавливает поток debounce.
pub struct WatchHandle {
    _watcher: RecommendedWatcher,
}

/// Следить за каталом: батчи относительных путей приходят в `out`.
pub fn watch(root: &Path, out: Sender<Vec<PathBuf>>) -> Result<WatchHandle, FsxError> {
    if !root.is_dir() {
        return Err(FsxError::NotFound(root.to_path_buf()));
    }
    let (tx, rx): (Sender<notify::Result<Event>>, Receiver<notify::Result<Event>>) = channel();
    let mut watcher = notify::recommended_watcher(move |res| {
        let _ = tx.send(res);
    })?;
    watcher.watch(root, RecursiveMode::Recursive)?;
    let root_owned = root.to_path_buf();
    std::thread::Builder::new()
        .name("fsx-debounce".into())
        .spawn(move || debounce_loop(&root_owned, rx, out))
        .map_err(FsxError::Io)?;
    Ok(WatchHandle { _watcher: watcher })
}

/// Копить пути, пока события идут, и отдавать батч после паузы тишины.
fn debounce_loop(root: &Path, rx: Receiver<notify::Result<Event>>, out: Sender<Vec<PathBuf>>) {
    let quiet = Duration::from_millis(DEBOUNCE_MS);
    let mut batch: Vec<PathBuf> = Vec::new();
    loop {
        let res = if batch.is_empty() {
            // Пустой батч: ждём первое событие сколь угодно долго.
            match rx.recv() {
                Ok(r) => Some(r),
                Err(_) => break, // watcher дропнут — работа закончена
            }
        } else {
            match rx.recv_timeout(quiet) {
                Ok(r) => Some(r),
                Err(std::sync::mpsc::RecvTimeoutError::Timeout) => {
                    // Тишина 200 мс: батч созрел.
                    let ready = std::mem::take(&mut batch);
                    if out.send(ready).is_err() {
                        break; // потребитель закрыт
                    }
                    continue;
                }
                Err(std::sync::mpsc::RecvTimeoutError::Disconnected) => break,
            }
        };
        if let Some(res) = res {
            collect_paths(res, root, &mut batch);
        }
    }
}

fn collect_paths(res: notify::Result<Event>, root: &Path, batch: &mut Vec<PathBuf>) {
    let ev = match res {
        Ok(e) => e,
        Err(_) => return,
    };
    for p in ev.paths {
        // Относительные пути: шина не должна знать абсолютные корневые.
        let rel = match p.strip_prefix(root) {
            Ok(r) if !r.as_os_str().is_empty() => r.to_path_buf(),
            _ => continue,
        };
        if !batch.contains(&rel) {
            batch.push(rel);
            if batch.len() >= MAX_BATCH_PATHS {
                return; // потолок: остаток доберёт следующий батч
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn temp_root(tag: &str) -> PathBuf {
        let dir = std::env::temp_dir().join(format!(
            "swagcod-watch-{tag}-{}-{}",
            std::process::id(),
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        ));
        std::fs::create_dir_all(&dir).unwrap();
        dir
    }

    #[test]
    fn writes_arrive_as_one_debounced_batch() {
        let root = temp_root("batch");
        let (tx, rx) = channel();
        let _handle = watch(&root, tx).expect("watcher поднялся");
        // Серия быстрых записей: debounce обязан слить их в один батч.
        for i in 0..5 {
            std::fs::write(root.join(format!("f{i}.txt")), "x").unwrap();
        }
        let batch = rx
            .recv_timeout(Duration::from_secs(5))
            .expect("батч доехал");
        assert!(!batch.is_empty());
        assert!(
            batch.iter().any(|p| p.to_string_lossy().contains("f0.txt")),
            "в батче нет записанного файла: {batch:?}"
        );
        drop(_handle);
        let _ = std::fs::remove_dir_all(&root);
    }

    #[test]
    fn paths_are_relative_to_root() {
        let root = temp_root("rel");
        let (tx, rx) = channel();
        let _handle = watch(&root, tx).unwrap();
        std::fs::write(root.join("a.txt"), "1").unwrap();
        let batch = rx.recv_timeout(Duration::from_secs(5)).unwrap();
        assert!(
            batch.iter().all(|p| p.is_relative()),
            "пути должны быть относительными: {batch:?}"
        );
        let _ = std::fs::remove_dir_all(&root);
    }

    #[test]
    fn dropping_handle_stops_the_loop() {
        let root = temp_root("stop");
        let (tx, rx) = channel();
        let handle = watch(&root, tx).unwrap();
        drop(handle);
        // Канал закроется: recv_timeout вернёт Disconnected, не зависнет.
        let res = rx.recv_timeout(Duration::from_secs(2));
        assert!(res.is_err(), "поток не остановился после дропа ручки");
        let _ = std::fs::remove_dir_all(&root);
    }

    #[test]
    fn missing_root_is_error() {
        let bogus = std::env::temp_dir().join("swagcod-watch-nope-8f3a1c");
        let (tx, _rx) = channel();
        assert!(watch(&bogus, tx).is_err());
    }
}
