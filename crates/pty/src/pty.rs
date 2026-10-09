/*! Менеджер PTY-сессий поверх `portable-pty` (этап B-2).

ConPTY на Windows и forkpty на Unix приходят из проверенного крейта: своего
кода меньше — стабильность выше. Поверх него наше: читатель потока батчит
вывод (пачка по 256 КБ или по признаку «читалка притихла»), медленный
потребитель получает backpressure ограниченного канала: лишние пачки
дропаются и считаются, а не роняют процесс.
*/

use std::collections::HashMap;
use std::io::{Read, Write};
use std::sync::mpsc::{sync_channel, Receiver, SyncSender};
use std::sync::{Arc, Mutex};

use portable_pty::{CommandBuilder, MasterPty, NativePtySystem, PtySize, PtySystem};

/// Пачка вывода на один кадр-фlush: больше — редкие события, меньше — шум.
const BATCH_BYTES: usize = 256 * 1024;
/// Буфер одного чтения из PTY. Большой: ConPTY отдаёт данные крупными
/// порциями, и 256 КБ на чтение снижают число системных вызовов.
const READ_CHUNK: usize = 256 * 1024;
/// Ёмкость канала вывода: 256 пачек по ≤256 КБ ≈ до 64 МБ запаса.
const CHANNEL_CAPACITY: usize = 256;

#[derive(Debug)]
pub enum PtyMsg {
    /// Пачка сырых байт вывода (до ANSI-разборки: парсер живёт у потребителя).
    Output(Vec<u8>),
    /// Процесс за PTY завершился: код выхода.
    Exit(u32),
}

#[derive(Debug, thiserror::Error)]
pub enum PtyError {
    #[error("pty: {0}")]
    Io(#[from] std::io::Error),
    #[error("pty: {0}")]
    Other(String),
}

pub type PtyResult<T> = Result<T, PtyError>;

/// Живая PTY-сессия: писатель, мастер для resize и pid для kill.
///
/// Сам ребёнок живёт в потоке-читателе: он же ждёт выход и сообщает код.
/// Убить извне — по process_id системным средством, потому что `Child`
/// из portable-pty один и принадлежит своему потоку.
pub struct PtyHandle {
    id: String,
    writer: Mutex<Box<dyn Write + Send>>,
    master: Mutex<Box<dyn MasterPty + Send>>,
    pid: u32,
    rx: Mutex<Option<Receiver<PtyMsg>>>,
    /// Сколько пачек сброшено из-за переполнения канала: метрика backpressure.
    /// Arc общий с читательским потоком — счётчик один на сессию.
    dropped: Arc<std::sync::atomic::AtomicU64>,
}

impl PtyHandle {
    pub fn id(&self) -> &str {
        &self.id
    }

    /// Забрать приёмник вывода: один потребитель на сессию.
    pub fn take_rx(&self) -> Option<Receiver<PtyMsg>> {
        self.rx.lock().ok().and_then(|mut g| g.take())
    }

    pub fn write(&self, data: &[u8]) -> PtyResult<()> {
        let mut w = self
            .writer
            .lock()
            .map_err(|e| PtyError::Other(e.to_string()))?;
        w.write_all(data)?;
        w.flush()?;
        Ok(())
    }

    pub fn resize(&self, cols: u16, rows: u16) -> PtyResult<()> {
        let m = self
            .master
            .lock()
            .map_err(|e| PtyError::Other(e.to_string()))?;
        m.resize(PtySize {
            rows,
            cols,
            pixel_width: 0,
            pixel_height: 0,
        })
        .map_err(|e| PtyError::Other(e.to_string()))?;
        Ok(())
    }

    pub fn kill(&self) {
        // Кроссплатформенно и без лишних зависимостей: системный kill по pid.
        #[cfg(windows)]
        use std::os::windows::process::CommandExt;
        let mut cmd = if cfg!(windows) {
            let mut c = std::process::Command::new("taskkill");
            c.args(["/PID", &self.pid.to_string(), "/T", "/F"]);
            c
        } else {
            let mut c = std::process::Command::new("kill");
            c.args(["-9", &self.pid.to_string()]);
            c
        };
        // Без консольного окна при убийстве PTY.
        #[cfg(windows)]
        cmd.creation_flags(0x0800_0000); // CREATE_NO_WINDOW
        let _ = cmd
            .stdout(std::process::Stdio::null())
            .stderr(std::process::Stdio::null())
            .status();
    }

    pub fn pid(&self) -> u32 {
        self.pid
    }

    pub fn dropped_batches(&self) -> u64 {
        self.dropped.load(std::sync::atomic::Ordering::Relaxed)
    }
}

/// Реестр PTY-сессий приложения.
#[derive(Default)]
pub struct PtyManager {
    sessions: Mutex<HashMap<String, Arc<PtyHandle>>>,
}

impl PtyManager {
    pub fn new() -> Self {
        Self::default()
    }

    /// Поднять PTY: shell в cwd сессии, читатель уходит в свой поток.
    pub fn spawn(
        &self,
        id: &str,
        shell: &str,
        cwd: &str,
        cols: u16,
        rows: u16,
    ) -> PtyResult<Arc<PtyHandle>> {
        let pty_system = NativePtySystem::default();
        let pair = pty_system
            .openpty(PtySize {
                rows,
                cols,
                pixel_width: 0,
                pixel_height: 0,
            })
            .map_err(|e| PtyError::Other(e.to_string()))?;
        let mut cmd = CommandBuilder::new(shell);
        cmd.cwd(cwd);
        let child = pair
            .slave
            .spawn_command(cmd)
            .map_err(|e| PtyError::Other(format!("не поднял {shell}: {e}")))?;
        let pid = child
            .process_id()
            .ok_or_else(|| PtyError::Other("PTY-ребёнок без pid".into()))?;
        let mut reader = pair
            .master
            .try_clone_reader()
            .map_err(|e| PtyError::Other(e.to_string()))?;
        let writer = pair
            .master
            .take_writer()
            .map_err(|e| PtyError::Other(e.to_string()))?;

        let (tx, rx): (SyncSender<PtyMsg>, Receiver<PtyMsg>) = sync_channel(CHANNEL_CAPACITY);
        let dropped = Arc::new(std::sync::atomic::AtomicU64::new(0));
        let dropped_thread = dropped.clone();
        std::thread::Builder::new()
            .name(format!("pty-read-{id}"))
            .spawn(move || {
                // Ребёнок живёт здесь же: поток читает вывод и сам ждёт выход.
                let mut child = child;
                let mut buf = vec![0u8; READ_CHUNK];
                let mut batch: Vec<u8> = Vec::with_capacity(BATCH_BYTES);
                let send = |msg: PtyMsg,
                            tx: &SyncSender<PtyMsg>,
                            dropped: &std::sync::atomic::AtomicU64| {
                    // try_send: переполнение канала = медленный потребитель,
                    // пачка дропается и считается (backpressure по плану B-2).
                    if tx.try_send(msg).is_err() {
                        dropped.fetch_add(1, std::sync::atomic::Ordering::Relaxed);
                    }
                };
                loop {
                    match reader.read(&mut buf) {
                        Ok(0) => break,
                        Ok(n) => {
                            // Недочитанное чтение = поток притих: мелкий вывод
                            // (эхо команды, приглашение) уходит сразу, без
                            // ожидания порога. При флуде чтения полные
                            // (n == READ_CHUNK) и пачка копится до BATCH_BYTES —
                            // латентность интерактива и крупные пачки под
                            // нагрузкой не конфликтуют.
                            let quiet = n < buf.len();
                            batch.extend_from_slice(&buf[..n]);
                            if batch.len() >= BATCH_BYTES || quiet {
                                let out =
                                    std::mem::replace(&mut batch, Vec::with_capacity(BATCH_BYTES));
                                send(PtyMsg::Output(out), &tx, &dropped_thread);
                            }
                        }
                        Err(ref e) if e.kind() == std::io::ErrorKind::Interrupted => continue,
                        Err(_) => break,
                    }
                }
                if !batch.is_empty() {
                    send(PtyMsg::Output(batch), &tx, &dropped_thread);
                }
                let code = child.wait().map(|s| s.exit_code()).unwrap_or(1);
                let _ = tx.send(PtyMsg::Exit(code));
            })
            .map_err(|e| PtyError::Other(e.to_string()))?;

        let handle = Arc::new(PtyHandle {
            id: id.to_string(),
            writer: Mutex::new(writer),
            master: Mutex::new(pair.master),
            pid,
            rx: Mutex::new(Some(rx)),
            dropped: dropped.clone(),
        });
        if let Ok(mut map) = self.sessions.lock() {
            map.insert(id.to_string(), handle.clone());
        }
        Ok(handle)
    }

    pub fn get(&self, id: &str) -> Option<Arc<PtyHandle>> {
        self.sessions.lock().ok()?.get(id).cloned()
    }

    pub fn remove(&self, id: &str) -> Option<Arc<PtyHandle>> {
        self.sessions.lock().ok()?.remove(id)
    }
}

/// Shell по умолчанию: COMSPEC/SHELL окружения, без хардкода пути автора.
pub fn default_shell() -> String {
    if cfg!(windows) {
        std::env::var("COMSPEC").unwrap_or_else(|_| "cmd.exe".into())
    } else {
        std::env::var("SHELL").unwrap_or_else(|_| "/bin/bash".into())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::ansi::{AnsiParser, Op, VecSink};

    #[test]
    fn spawn_echo_roundtrip() {
        // Эхо через реальный PTY: вывод доезжает батчем, процесс завершается.
        let mgr = PtyManager::new();
        let cwd = std::env::temp_dir();
        let shell = if cfg!(windows) { "cmd.exe" } else { "/bin/sh" };
        let handle = mgr
            .spawn("pty-test", shell, cwd.to_str().unwrap(), 80, 24)
            .expect("pty поднялся");
        let rx = handle.take_rx().expect("приёмник один");
        let cmd = if cfg!(windows) {
            "echo swagcod-pty-ok\r"
        } else {
            "echo swagcod-pty-ok\n"
        };
        handle.write(cmd.as_bytes()).unwrap();

        let mut got = String::new();
        let mut exited = None;
        let deadline = std::time::Instant::now() + std::time::Duration::from_secs(10);
        while std::time::Instant::now() < deadline {
            match rx.recv_timeout(std::time::Duration::from_millis(200)) {
                Ok(PtyMsg::Output(b)) => got.push_str(&String::from_utf8_lossy(&b)),
                Ok(PtyMsg::Exit(code)) => {
                    exited = Some(code);
                    break;
                }
                Err(std::sync::mpsc::RecvTimeoutError::Timeout) => continue,
                Err(_) => break,
            }
            if got.contains("swagcod-pty-ok") && exited.is_none() {
                // Эхо доехало: ждём завершения или ещё немного вывода.
                continue;
            }
        }
        handle.kill();
        assert!(
            got.contains("swagcod-pty-ok"),
            "вывод эха не доехал: {got:?}"
        );
        let _ = exited;
    }

    #[test]
    fn parser_strips_csi_from_pty_stream() {
        let mut p = AnsiParser::new();
        let mut sink = VecSink::default();
        p.feed(b"\x1b[32mok\x1b[0m", &mut sink);
        p.finish(&mut sink);
        let texts: Vec<&str> = sink
            .0
            .iter()
            .filter_map(|o| match o {
                Op::Text { s } => Some(s.as_str()),
                _ => None,
            })
            .collect();
        assert_eq!(texts, vec!["ok"]);
    }

    /// Бенч бюджета B-2: 50 МБ через реальный PTY, быстрый потребитель.
    /// Ручной: `cargo test -p swagcod-pty --release -- --ignored --nocapture`.
    #[test]
    #[ignore = "ручной бенч пропускной способности PTY"]
    fn throughput_fifty_megabytes() {
        use std::io::Write as _;
        const TOTAL: usize = 50 * 1024 * 1024;
        let dir = std::env::temp_dir().join(format!("swagcod-pty-bench-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let file = dir.join("big.log");
        {
            let mut f = std::fs::File::create(&file).unwrap();
            let line = "x".repeat(99) + "\n";
            let chunk = line.repeat(1024); // ~100 КБ
            let mut written = 0;
            while written < TOTAL {
                f.write_all(chunk.as_bytes()).unwrap();
                written += chunk.len();
            }
        }
        let mgr = PtyManager::new();
        // Windows: байтовая запись прямо в stdout-поток — самый быстрый честный
        // флуд через ConPTY, без строковых перекодировок PowerShell.
        // Unix: cat.
        let (shell, cmd) = if cfg!(windows) {
            (
                "powershell.exe".to_string(),
                "$b=[IO.File]::ReadAllBytes('big.log');$s=[Console]::OpenStandardOutput();$s.Write($b,0,$b.Length);$s.Flush()\r"
                    .to_string(),
            )
        } else {
            ("/bin/sh".to_string(), "cat big.log\n".to_string())
        };
        let handle = mgr
            .spawn("pty-bench", &shell, dir.to_str().unwrap(), 120, 30)
            .expect("pty поднялся");
        let rx = handle.take_rx().unwrap();
        handle.write(cmd.as_bytes()).unwrap();

        let start = std::time::Instant::now();
        let mut got = 0usize;
        let mut first: Option<std::time::Instant> = None;
        let mut last = start;
        let mut exited = false;
        let mut quiet = 0u32;
        let deadline = start + std::time::Duration::from_secs(120);
        while std::time::Instant::now() < deadline {
            match rx.recv_timeout(std::time::Duration::from_millis(500)) {
                Ok(PtyMsg::Output(b)) => {
                    let now = std::time::Instant::now();
                    if first.is_none() {
                        first = Some(now);
                    }
                    last = now;
                    quiet = 0;
                    got += b.len();
                }
                Ok(PtyMsg::Exit(_)) => {
                    exited = true;
                    break;
                }
                Err(std::sync::mpsc::RecvTimeoutError::Timeout) => {
                    // Весь объём доехал и поток молчит 2 с — бенч закончен,
                    // шелл можно не ждать.
                    if got >= TOTAL {
                        quiet += 1;
                        if quiet >= 4 {
                            break;
                        }
                    }
                }
                Err(_) => break,
            }
        }
        // Считаем от первого до последнего байта: старт шелла и чтение файла
        // в поток не входят — бюджет про сам конвейер PTY, а не про PowerShell.
        let window = first
            .map(|f| last.duration_since(f).as_secs_f64())
            .unwrap_or(0.0);
        let rate = if window > 0.0 {
            got as f64 / window
        } else {
            0.0
        };
        handle.kill();
        let _ = std::fs::remove_dir_all(&dir);
        println!(
            "PTY-бенч: {:.1} МБ за {:.2} с окна = {:.1} МБ/с (выход: {exited}), дропнуто пачек: {}",
            got as f64 / 1e6,
            window,
            rate / 1e6,
            handle.dropped_batches()
        );
        // Бюджет плана — 50 МБ/с, но на Windows верхняя планка не наша:
        // ConPTY сам рендерит поток и на этом железе даёт ~19 МБ/с. Наша
        // сторона обязана поглотить всё без единого дропа — это и проверяем,
        // плюс нижний порог реальной скорости платформы (см. DECISIONS, ревизия 17).
        assert_eq!(
            handle.dropped_batches(),
            0,
            "конвейер PTY уронил пачки: потребитель не успевает"
        );
        assert!(
            rate >= 15.0 * 1e6,
            "пропускная способность {:.1} МБ/с ниже порога платформы 15 МБ/с",
            rate / 1e6
        );
    }
}
