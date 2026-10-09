//! Общий клиент JSON-RPC 2.0 поверх stdio (newline-delimited): процесс с
//! piped stdin/stdout, ответы маршрутизируются по id, запросы — с
//! таймаутом. Первый потребитель — MCP (E-3), второй — sidecar JS-плагинов
//! (E-4); транспорт один, протокольные рукопожатия делают потребители.
//!
//! Гарантии: смерть процесса (stdout закрыт) вычищает pending — ожидающие
//! получают ошибку канала, а не вечное ожидание; `kill_on_drop(true)` —
//! соединение, не дожившее до реестра, уносит дочерний процесс с собой.

use std::collections::HashMap;
use std::process::Stdio;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;
use std::time::Duration;

use serde_json::{json, Value};
use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
use tokio::process::{Child, ChildStdin, Command};
use tokio::sync::{oneshot, Mutex as AsyncMutex};

#[cfg(windows)]
const CREATE_NO_WINDOW: u32 = 0x0800_0000;

/// Живое соединение с дочерним процессом.
pub struct RpcConn {
    writer: AsyncMutex<ChildStdin>,
    pending: Arc<std::sync::Mutex<HashMap<u64, oneshot::Sender<Value>>>>,
    next_id: AtomicU64,
    child: AsyncMutex<Child>,
}

impl RpcConn {
    /// JSON-RPC запрос с таймаутом. Потеря канала (процесс умер) — ошибка,
    /// а не вечное ожидание.
    pub async fn request(
        &self,
        method: &str,
        params: Value,
        timeout: Duration,
    ) -> Result<Value, String> {
        let id = self.next_id.fetch_add(1, Ordering::Relaxed) + 1;
        let (tx, rx) = oneshot::channel();
        self.pending.lock().unwrap().insert(id, tx);
        let msg = json!({"jsonrpc": "2.0", "id": id, "method": method, "params": params});
        let mut line = serde_json::to_string(&msg).map_err(|e| format!("rpc: json: {e}"))?;
        line.push('\n');
        {
            let mut w = self.writer.lock().await;
            if let Err(e) = w.write_all(line.as_bytes()).await {
                self.pending.lock().unwrap().remove(&id);
                return Err(format!("rpc: stdin закрыт: {e}"));
            }
            let _ = w.flush().await;
        }
        match tokio::time::timeout(timeout, rx).await {
            Ok(Ok(v)) => Ok(v),
            Ok(Err(_)) => Err(format!("rpc: процесс завершил соединение ({method})")),
            Err(_) => {
                self.pending.lock().unwrap().remove(&id);
                Err(format!(
                    "rpc: таймаут {method} ({} мс)",
                    timeout.as_millis()
                ))
            }
        }
    }

    /// Уведомление без id (например, MCP notifications/initialized).
    pub async fn notify(&self, method: &str) {
        let msg = json!({"jsonrpc": "2.0", "method": method});
        if let Ok(mut line) = serde_json::to_string(&msg) {
            line.push('\n');
            let mut w = self.writer.lock().await;
            let _ = w.write_all(line.as_bytes()).await;
            let _ = w.flush().await;
        }
    }

    /// Убить дочерний процесс.
    pub async fn kill(&self) {
        let _ = self.child.lock().await.start_kill();
    }
}

/// Читатель stdout: ответы с id уходят ожидающим; уведомления и серверные
/// запросы игнорируются (клиент request-only). Конец stdout — процесс
/// умер: pending вычищается.
async fn reader_task(
    stdout: tokio::process::ChildStdout,
    pending: Arc<std::sync::Mutex<HashMap<u64, oneshot::Sender<Value>>>>,
    label: String,
) {
    let mut lines = BufReader::new(stdout).lines();
    loop {
        match lines.next_line().await {
            Ok(Some(line)) => {
                let v: Value = match serde_json::from_str(&line) {
                    Ok(v) => v,
                    Err(_) => continue, // мусор в stdout — не наш протокол
                };
                let Some(id) = v.get("id").and_then(|i| i.as_u64()) else {
                    continue;
                };
                if let Some(tx) = pending.lock().unwrap().remove(&id) {
                    let _ = tx.send(v);
                }
            }
            Ok(None) => break,
            Err(_) => break,
        }
    }
    eprintln!("rpc: {label} закрыл stdout");
    pending.lock().unwrap().clear();
}

/// Поднять процесс и завести канал без протокольного рукопожатия —
/// рукопожатие делает вызывающий (MCP initialize, загрузка плагинов...).
/// `label` уходит в тексты ошибок и лог.
pub fn spawn(
    command: &str,
    args: &[String],
    env: &HashMap<String, String>,
    label: &str,
) -> Result<Arc<RpcConn>, String> {
    let mut cmd = Command::new(command);
    cmd.args(args)
        .envs(env)
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::null())
        // Сироты недопустимы: соединение, которое не дожило до владельца,
        // обязано унести процесс с собой.
        .kill_on_drop(true);
    #[cfg(windows)]
    cmd.creation_flags(CREATE_NO_WINDOW);
    let mut child = cmd.spawn().map_err(|e| format!("{label}: запуск: {e}"))?;
    let stdin = child
        .stdin
        .take()
        .ok_or_else(|| format!("{label}: нет stdin"))?;
    let stdout = child
        .stdout
        .take()
        .ok_or_else(|| format!("{label}: нет stdout"))?;

    let pending: Arc<std::sync::Mutex<HashMap<u64, oneshot::Sender<Value>>>> =
        Arc::new(std::sync::Mutex::new(HashMap::new()));
    let conn = Arc::new(RpcConn {
        writer: AsyncMutex::new(stdin),
        pending: pending.clone(),
        next_id: AtomicU64::new(0),
        child: AsyncMutex::new(child),
    });
    tokio::spawn(reader_task(stdout, pending, label.to_string()));
    Ok(conn)
}

/// Общая разборка JSON-RPC ответа: error → Err(текст), иначе result.
pub fn unwrap_response(resp: &Value, what: &str) -> Result<Value, String> {
    if let Some(err) = resp.get("error") {
        let msg = err
            .get("message")
            .and_then(|m| m.as_str())
            .unwrap_or("неизвестная ошибка");
        return Err(format!("{what}: {msg}"));
    }
    Ok(resp.get("result").cloned().unwrap_or(Value::Null))
}
