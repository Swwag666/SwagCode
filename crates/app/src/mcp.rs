//! E-3: MCP-клиент (Model Context Protocol) — внешние серверы инструментов.
//!
//! Транспорт — stdio: процесс сервера запускается приложением, обмен
//! newline-delimited JSON-RPC 2.0 (одно сообщение на строку, без
//! вложенных переводов). Рукопожатие: `initialize` → уведомление
//! `notifications/initialized` → discover `tools/list`; вызовы —
//! `tools/call`. Ресурсы и промпты MCP — вторая очередь (план E-3),
//! здесь только tools.
//!
//! Имя инструмента в модели — `mcp:<server>:<tool>`: имя не входит в
//! ApprovalPolicy::BUILTIN, поэтому при политике OnDangerous каждый вызов
//! уходит на подтверждение человеком, а журнал B-7 получает tool с
//! префиксом `mcp:` без отдельной ветки кода.
//!
//! Таймауты обязательны: initialize/list — MCP_INIT_TIMEOUT_MS, вызов —
//! MCP_CALL_TIMEOUT_MS (переопределяется SWAGCOD_MCP_TIMEOUT_MS). Смерть
//! сервера — не падение приложения: pending-запросы получают ошибку,
//! статус честно виден в реестре.

use std::collections::HashMap;
use std::process::Stdio;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;
use std::time::Duration;

use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
use tokio::process::{Child, ChildStdin, Command};
use tokio::sync::{oneshot, Mutex as AsyncMutex};

/// Версия протокола, которую предлагает клиент. Сервер отвечает своей —
/// работаем с той, что вернул initialize.
pub const MCP_PROTOCOL_VERSION: &str = "2024-11-05";
pub const MCP_INIT_TIMEOUT_MS: u64 = 15_000;
pub const MCP_CALL_TIMEOUT_MS: u64 = 60_000;
/// Префикс имён MCP-инструментов в модели и журнале.
pub const MCP_PREFIX: &str = "mcp:";

#[cfg(windows)]
const CREATE_NO_WINDOW: u32 = 0x0800_0000;

/// Конфигурация сервера. Хранится JSON-массивом в prefs под ключом
/// `mcp_servers` — реестр переживает перезапуск.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct McpServerConfig {
    pub name: String,
    pub command: String,
    #[serde(default)]
    pub args: Vec<String>,
    #[serde(default)]
    pub env: HashMap<String, String>,
    #[serde(default = "default_enabled")]
    pub enabled: bool,
}

fn default_enabled() -> bool {
    true
}

/// Инструмент подключённого сервера: полное имя для модели + inputSchema.
#[derive(Debug, Clone, Serialize)]
pub struct McpTool {
    pub name: String,
    pub server: String,
    pub tool: String,
    pub description: String,
    pub parameters: Value,
}

/// Живое соединение: писатель в stdin, карта ожидающих ответов, ребёнок.
pub struct Conn {
    writer: AsyncMutex<ChildStdin>,
    pending: Arc<std::sync::Mutex<HashMap<u64, oneshot::Sender<Value>>>>,
    next_id: AtomicU64,
    child: AsyncMutex<Child>,
}

impl Conn {
    /// JSON-RPC запрос с таймаутом. Потеря канала (сервер умер) — ошибка,
    /// а не вечное ожидание.
    pub async fn request(&self, method: &str, params: Value, timeout: Duration) -> Result<Value, String> {
        let id = self.next_id.fetch_add(1, Ordering::Relaxed) + 1;
        let (tx, rx) = oneshot::channel();
        self.pending.lock().unwrap().insert(id, tx);
        let msg = json!({"jsonrpc": "2.0", "id": id, "method": method, "params": params});
        let mut line = serde_json::to_string(&msg).map_err(|e| format!("mcp: json: {e}"))?;
        line.push('\n');
        {
            let mut w = self.writer.lock().await;
            if let Err(e) = w.write_all(line.as_bytes()).await {
                self.pending.lock().unwrap().remove(&id);
                return Err(format!("mcp: stdin закрыт: {e}"));
            }
            let _ = w.flush().await;
        }
        match tokio::time::timeout(timeout, rx).await {
            Ok(Ok(v)) => Ok(v),
            Ok(Err(_)) => Err(format!("mcp: сервер завершил соединение ({method})")),
            Err(_) => {
                self.pending.lock().unwrap().remove(&id);
                Err(format!("mcp: таймаут {method} ({} мс)", timeout.as_millis()))
            }
        }
    }

    /// Уведомление без id (notifications/initialized).
    async fn notify(&self, method: &str) {
        let msg = json!({"jsonrpc": "2.0", "method": method});
        if let Ok(mut line) = serde_json::to_string(&msg) {
            line.push('\n');
            let mut w = self.writer.lock().await;
            let _ = w.write_all(line.as_bytes()).await;
            let _ = w.flush().await;
        }
    }

    /// Убить процесс сервера (disconnect / удаление / выход).
    pub async fn kill(&self) {
        let _ = self.child.lock().await.start_kill();
    }
}

/// Читатель stdout: ответы с id уходят ожидающим; серверные уведомления и
/// запросы игнорируются (tools-only клиент). Конец stdout — сервер умер:
/// pending вычищается, ожидающие получают ошибку канала.
async fn reader_task(
    stdout: tokio::process::ChildStdout,
    pending: Arc<std::sync::Mutex<HashMap<u64, oneshot::Sender<Value>>>>,
    server: String,
) {
    let mut lines = BufReader::new(stdout).lines();
    loop {
        match lines.next_line().await {
            Ok(Some(line)) => {
                let v: Value = match serde_json::from_str(&line) {
                    Ok(v) => v,
                    Err(_) => continue, // мусор в stdout сервера — не наш протокол
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
    eprintln!("mcp: сервер {server} закрыл stdout");
    pending.lock().unwrap().clear();
}

/// Поднять сервер и выполнить рукопожатие MCP. Ошибка — строка для статуса
/// реестра: подключение не должно ронять приложение.
pub async fn connect_with_timeout(
    cfg: &McpServerConfig,
    init_timeout: Duration,
) -> Result<Arc<Conn>, String> {
    let mut cmd = Command::new(&cfg.command);
    cmd.args(&cfg.args)
        .envs(&cfg.env)
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::null())
        // Сироты недопустимы: соединение, которое не дожило до реестра
        // (таймаут рукопожатия, ошибка), обязано унести процесс с собой.
        .kill_on_drop(true);
    #[cfg(windows)]
    cmd.creation_flags(CREATE_NO_WINDOW);
    let mut child = cmd
        .spawn()
        .map_err(|e| format!("mcp {}: запуск: {e}", cfg.name))?;
    let stdin = child.stdin.take().ok_or_else(|| "mcp: нет stdin".to_string())?;
    let stdout = child.stdout.take().ok_or_else(|| "mcp: нет stdout".to_string())?;

    let pending: Arc<std::sync::Mutex<HashMap<u64, oneshot::Sender<Value>>>> =
        Arc::new(std::sync::Mutex::new(HashMap::new()));
    let conn = Arc::new(Conn {
        writer: AsyncMutex::new(stdin),
        pending: pending.clone(),
        next_id: AtomicU64::new(0),
        child: AsyncMutex::new(child),
    });
    tokio::spawn(reader_task(stdout, pending, cfg.name.clone()));

    let init = conn
        .request(
            "initialize",
            json!({
                "protocolVersion": MCP_PROTOCOL_VERSION,
                "capabilities": {},
                "clientInfo": {"name": "swagcod", "version": env!("CARGO_PKG_VERSION")}
            }),
            init_timeout,
        )
        .await?;
    if let Some(err) = init.get("error") {
        conn.kill().await;
        return Err(format!("mcp {}: initialize: {}", cfg.name, err));
    }
    conn.notify("notifications/initialized").await;
    Ok(conn)
}

pub async fn connect(cfg: &McpServerConfig) -> Result<Arc<Conn>, String> {
    connect_with_timeout(cfg, Duration::from_millis(MCP_INIT_TIMEOUT_MS)).await
}

/// Чистый разбор ответа tools/list — тестируется без живого сервера.
pub fn tools_from_response(resp: &Value, server: &str) -> Result<Vec<McpTool>, String> {
    if let Some(err) = resp.get("error") {
        return Err(format!(
            "mcp {server}: {}",
            err.get("message").and_then(|m| m.as_str()).unwrap_or("tools/list error")
        ));
    }
    let Some(arr) = resp.pointer("/result/tools").and_then(|v| v.as_array()) else {
        return Err(format!("mcp {server}: в ответе нет result.tools"));
    };
    Ok(arr
        .iter()
        .filter_map(|t| {
            let tool = t.get("name")?.as_str()?.to_string();
            Some(McpTool {
                name: format!("{MCP_PREFIX}{server}:{tool}"),
                server: server.to_string(),
                tool,
                description: t
                    .get("description")
                    .and_then(|d| d.as_str())
                    .unwrap_or_default()
                    .to_string(),
                parameters: t
                    .get("inputSchema")
                    .cloned()
                    .unwrap_or_else(|| json!({"type": "object"})),
            })
        })
        .collect())
}

pub async fn list_tools(conn: &Conn, server: &str) -> Result<Vec<McpTool>, String> {
    let resp = conn
        .request("tools/list", json!({}), Duration::from_millis(MCP_INIT_TIMEOUT_MS))
        .await?;
    tools_from_response(&resp, server)
}

/// Чистый разбор результата tools/call: (is_error, текст). Из content
/// берутся только text-части: изображения/ресурсы — вторая очередь.
pub fn text_from_call_result(result: &Value) -> (bool, String) {
    let is_err = result.get("isError").and_then(|v| v.as_bool()).unwrap_or(false);
    let text = result
        .get("content")
        .and_then(|c| c.as_array())
        .map(|parts| {
            parts
                .iter()
                .filter_map(|p| match p.get("type").and_then(|t| t.as_str()) {
                    Some("text") => p.get("text").and_then(|t| t.as_str()).map(String::from),
                    _ => None,
                })
                .collect::<Vec<_>>()
                .join("\n")
        })
        .unwrap_or_default();
    (is_err, text)
}

pub async fn call_tool(
    conn: &Conn,
    tool: &str,
    args: Value,
    timeout: Duration,
) -> Result<String, String> {
    let resp = conn
        .request("tools/call", json!({"name": tool, "arguments": args}), timeout)
        .await?;
    if let Some(err) = resp.get("error") {
        return Err(format!(
            "mcp: {}",
            err.get("message").and_then(|m| m.as_str()).unwrap_or("tools/call error")
        ));
    }
    let result = resp.get("result").cloned().unwrap_or(Value::Null);
    let (is_err, text) = text_from_call_result(&result);
    if is_err {
        Err(if text.is_empty() {
            format!("mcp: инструмент {tool} вернул ошибку")
        } else {
            text
        })
    } else {
        Ok(text)
    }
}

/// Таймаут вызова инструмента: env SWAGCOD_MCP_TIMEOUT_MS или 60 с.
pub fn call_timeout() -> Duration {
    let ms = std::env::var("SWAGCOD_MCP_TIMEOUT_MS")
        .ok()
        .and_then(|v| v.parse::<u64>().ok())
        .filter(|v| *v > 0)
        .unwrap_or(MCP_CALL_TIMEOUT_MS);
    Duration::from_millis(ms)
}

/// Реестр подключений: соединения, инструменты для модели, честные статусы
/// для UI. Живёт в AppState за tokio-мьютексом (connect — async).
#[derive(Default)]
pub struct McpRegistry {
    conns: HashMap<String, Arc<Conn>>,
    pub tools: Vec<McpTool>,
    states: HashMap<String, String>,
}

/// Строка статуса сервера для UI.
#[derive(Debug, Clone, Serialize)]
pub struct McpStatus {
    pub name: String,
    pub state: String,
    pub tools: usize,
}

impl McpRegistry {
    /// Подключить сервер по конфигу: старое соединение убивается, статус и
    /// инструменты обновляются. Возвращает число инструментов (0 при ошибке —
    /// ошибка видна в status()).
    pub async fn connect_server(&mut self, cfg: &McpServerConfig) -> usize {
        if !cfg.enabled {
            self.disconnect(&cfg.name).await;
            self.states.insert(cfg.name.clone(), "отключён".into());
            return 0;
        }
        self.disconnect(&cfg.name).await;
        match connect(cfg).await {
            Ok(conn) => match list_tools(&conn, &cfg.name).await {
                Ok(tools) => {
                    let n = tools.len();
                    self.tools.retain(|t| t.server != cfg.name);
                    self.tools.extend(tools);
                    self.states
                        .insert(cfg.name.clone(), format!("подключён: {n} инстр."));
                    self.conns.insert(cfg.name.clone(), conn);
                    n
                }
                Err(e) => {
                    conn.kill().await;
                    self.states.insert(cfg.name.clone(), e);
                    0
                }
            },
            Err(e) => {
                self.states.insert(cfg.name.clone(), e);
                0
            }
        }
    }

    pub async fn disconnect(&mut self, name: &str) {
        if let Some(c) = self.conns.remove(name) {
            c.kill().await;
        }
        self.tools.retain(|t| t.server != name);
        self.states.remove(name);
    }

    pub fn conn(&self, server: &str) -> Option<Arc<Conn>> {
        self.conns.get(server).cloned()
    }

    pub fn status(&self) -> Vec<McpStatus> {
        let mut out: Vec<McpStatus> = self
            .states
            .iter()
            .map(|(name, state)| McpStatus {
                name: name.clone(),
                state: state.clone(),
                tools: self.tools.iter().filter(|t| t.server == *name).count(),
            })
            .collect();
        out.sort_by(|a, b| a.name.cmp(&b.name));
        out
    }

    /// Убить все серверы (выход приложения).
    pub async fn shutdown(&self) {
        for c in self.conns.values() {
            c.kill().await;
        }
    }
}

/// Разбор `mcp:<server>:<tool>` — чистая функция, тестируется.
pub fn split_tool_name(name: &str) -> Option<(&str, &str)> {
    let rest = name.strip_prefix(MCP_PREFIX)?;
    let (server, tool) = rest.split_once(':')?;
    if server.is_empty() || tool.is_empty() {
        return None;
    }
    Some((server, tool))
}

/// Валидация имени сервера для реестра: без `:` (разделитель имён),
/// без пробелов по краям, непустое.
pub fn valid_server_name(name: &str) -> bool {
    !name.trim().is_empty() && !name.contains(':') && name.len() <= 64
}

/// Где взять node для живых протокольных тестов: SWAGCOD_NODE → vendor
/// репозитория → PATH. Общий для mcp.rs и lib.rs.
#[cfg(test)]
pub fn tests_node() -> Option<String> {
    if let Ok(n) = std::env::var("SWAGCOD_NODE") {
        if !n.is_empty() {
            return Some(n);
        }
    }
    let vendored =
        std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("../../vendor/node/node.exe");
    if vendored.exists() {
        return Some(vendored.to_string_lossy().to_string());
    }
    Some("node".to_string())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn config_roundtrip_and_defaults() {
        let raw = r#"{"name":"echo","command":"node","args":["s.js"],"env":{"K":"V"}}"#;
        let cfg: McpServerConfig = serde_json::from_str(raw).unwrap();
        assert!(cfg.enabled, "enabled по умолчанию true");
        assert_eq!(cfg.args, ["s.js"]);
        let back = serde_json::to_string(&cfg).unwrap();
        let cfg2: McpServerConfig = serde_json::from_str(&back).unwrap();
        assert_eq!(cfg2.name, "echo");
        // Массив конфигов — формат prefs.
        let list: Vec<McpServerConfig> =
            serde_json::from_str(&format!("[{raw}]")).unwrap();
        assert_eq!(list.len(), 1);
    }

    #[test]
    fn tools_from_response_maps_names_and_schema() {
        let resp = json!({
            "jsonrpc": "2.0", "id": 1,
            "result": {"tools": [
                {"name": "echo", "description": "эхо",
                 "inputSchema": {"type": "object", "properties": {"text": {"type": "string"}}}},
                {"name": "bare"}
            ]}
        });
        let tools = tools_from_response(&resp, "srv").unwrap();
        assert_eq!(tools.len(), 2);
        assert_eq!(tools[0].name, "mcp:srv:echo");
        assert_eq!(tools[0].description, "эхо");
        assert_eq!(tools[0].parameters["type"], "object");
        // без inputSchema — заглушка object, а не паника
        assert_eq!(tools[1].parameters, json!({"type": "object"}));
    }

    #[test]
    fn tools_from_response_propagates_error() {
        let resp = json!({"jsonrpc": "2.0", "id": 1, "error": {"code": -32601, "message": "нет такого"}});
        let e = tools_from_response(&resp, "srv").unwrap_err();
        assert!(e.contains("нет такого"), "{e}");
        let no_result = json!({"jsonrpc": "2.0", "id": 1, "result": {}});
        assert!(tools_from_response(&no_result, "srv").is_err());
    }

    #[test]
    fn call_result_text_and_error_flag() {
        let ok = json!({"content": [{"type": "text", "text": "раз"}, {"type": "image"}, {"type": "text", "text": "два"}]});
        let (is_err, text) = text_from_call_result(&ok);
        assert!(!is_err);
        assert_eq!(text, "раз\nдва", "не-текстовые части пропускаются");

        let bad = json!({"content": [{"type": "text", "text": "сломалось"}], "isError": true});
        let (is_err, text) = text_from_call_result(&bad);
        assert!(is_err);
        assert_eq!(text, "сломалось");
    }

    #[test]
    fn split_tool_name_parses_and_rejects() {
        assert_eq!(split_tool_name("mcp:srv:echo"), Some(("srv", "echo")));
        assert_eq!(split_tool_name("mcp:a:b:c"), Some(("a", "b:c")));
        assert_eq!(split_tool_name("read"), None);
        assert_eq!(split_tool_name("mcp::echo"), None);
        assert_eq!(split_tool_name("mcp:srv:"), None);
        assert_eq!(split_tool_name("mcp:srv"), None);
    }

    #[test]
    fn server_name_validation() {
        assert!(valid_server_name("echo"));
        assert!(!valid_server_name(""));
        assert!(!valid_server_name("  "));
        assert!(!valid_server_name("a:b"), "двоеточие — разделитель имён");
        assert!(!valid_server_name(&"x".repeat(65)));
    }

    /// Где взять node для живого протокольного теста: SWAGCOD_NODE →
    /// vendor репозитория → PATH. Нет нигде — тест честно пропускается.
    fn node_for_tests() -> Option<String> {
        crate::mcp::tests_node()
    }

    /// Фейковый MCP-сервер на node: полное рукопожатие + echo-инструмент.
    /// Это живой процесс и настоящий stdio JSON-RPC — протокол проверяется
    /// не моком, а реальным обменом строками.
    const FAKE_SERVER_JS: &str = r#"
const readline = require('readline');
const rl = readline.createInterface({ input: process.stdin });
function send(o) { process.stdout.write(JSON.stringify(o) + '\n'); }
rl.on('line', (line) => {
  let m; try { m = JSON.parse(line); } catch { return; }
  if (m.method === 'initialize') {
    send({jsonrpc:'2.0', id:m.id, result:{protocolVersion:'2024-11-05',
      capabilities:{tools:{}}, serverInfo:{name:'fake', version:'0.1'}}});
  } else if (m.method === 'tools/list') {
    send({jsonrpc:'2.0', id:m.id, result:{tools:[{name:'echo',
      description:'возвращает текст', inputSchema:{type:'object',
      properties:{text:{type:'string'}}, required:['text']}}]}});
  } else if (m.method === 'tools/call') {
    const a = (m.params && m.params.arguments) || {};
    if (m.params.name === 'boom') {
      send({jsonrpc:'2.0', id:m.id, result:{content:[{type:'text', text:'взрыв'}], isError:true}});
    } else {
      send({jsonrpc:'2.0', id:m.id, result:{content:[{type:'text', text:'echo:' + (a.text || '')}], isError:false}});
    }
  }
});
"#;

    fn write_fake_server(dir: &std::path::Path, body: &str) -> std::path::PathBuf {
        std::fs::create_dir_all(dir).unwrap();
        let p = dir.join("fake-mcp.js");
        std::fs::write(&p, body).unwrap();
        p
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn live_handshake_list_and_call() {
        let Some(node) = node_for_tests() else { return };
        let dir = std::env::temp_dir().join(format!("swagcod-mcp-{}", crate::short_id()));
        let script = write_fake_server(&dir, FAKE_SERVER_JS);
        let cfg = McpServerConfig {
            name: "fake".into(),
            command: node,
            args: vec![script.to_string_lossy().to_string()],
            env: HashMap::new(),
            enabled: true,
        };

        let conn = connect(&cfg).await.expect("рукопожатие с живым сервером");
        let tools = list_tools(&conn, "fake").await.expect("tools/list");
        assert_eq!(tools.len(), 1);
        assert_eq!(tools[0].name, "mcp:fake:echo");

        let out = call_tool(&conn, "echo", json!({"text": "привет"}), Duration::from_secs(10))
            .await
            .expect("tools/call");
        assert_eq!(out, "echo:привет");

        // isError:true — честная ошибка с текстом сервера.
        let e = call_tool(&conn, "boom", json!({}), Duration::from_secs(10))
            .await
            .unwrap_err();
        assert_eq!(e, "взрыв");

        // Реестр: статус и disconnect убивают процесс.
        let mut reg = McpRegistry::default();
        let n = reg.connect_server(&cfg).await;
        assert_eq!(n, 1);
        assert_eq!(reg.status()[0].state, "подключён: 1 инстр.");
        assert!(reg.conn("fake").is_some());
        reg.disconnect("fake").await;
        assert!(reg.conn("fake").is_none());
        assert!(reg.tools.is_empty());

        conn.kill().await;
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn silent_server_times_out() {
        let Some(node) = node_for_tests() else { return };
        // Сервер, который читает stdin и молчит: initialize обязан упасть
        // по таймауту, а не ждать вечно.
        let cfg = McpServerConfig {
            name: "silent".into(),
            command: node,
            args: vec!["-e".into(), "process.stdin.resume()".into()],
            env: HashMap::new(),
            enabled: true,
        };
        let r = connect_with_timeout(&cfg, Duration::from_millis(400)).await;
        let e = match r {
            Ok(_) => panic!("молчащий сервер обязан упасть по таймауту"),
            Err(e) => e,
        };
        assert!(e.contains("таймаут"), "{e}");
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn dead_command_is_status_not_panic() {
        let cfg = McpServerConfig {
            name: "ghost".into(),
            command: "swagcod-no-such-mcp-server-xyz".into(),
            args: vec![],
            env: HashMap::new(),
            enabled: true,
        };
        let mut reg = McpRegistry::default();
        assert_eq!(reg.connect_server(&cfg).await, 0);
        let st = reg.status();
        assert_eq!(st.len(), 1);
        assert!(st[0].state.contains("запуск"), "{:?}", st[0].state);

        // disabled — статус «отключён», соединения нет
        let mut off = cfg.clone();
        off.name = "off".into();
        off.command = "node".into();
        off.enabled = false;
        assert_eq!(reg.connect_server(&off).await, 0);
        assert!(reg.status().iter().any(|s| s.name == "off" && s.state == "отключён"));
    }
}
