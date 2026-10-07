/*! Хранилище на better-sqlite3 через Node-sidecar (ревизия 25).

Пользователь просил именно better-sqlite3 — он и становится основным
хранилищем: Rust-ядро поднимает `sidecar/store-server.js` дочерним
процессом и говорит с ним линейным JSON-RPC поверх stdio. better-sqlite3
синхронный, как и наш контракт [`Store`]: запрос-ответ по одной строке,
без async-обвязки.

[`SqliteStore`](crate::store::SqliteStore) остаётся аварийным фолбэком:
если Node не найден или sidecar не поднялся, приложение работает на
встроенном rusqlite и пишет причину в stderr. Выбор: `SWAGCOD_STORE`
(`node` — только sidecar, `sqlite` — только встроенный, по умолчанию
auto). Путь к скрипту: `SWAGCOD_STORE_SIDECAR`, иначе `sidecar/`
рядом с exe, иначе от cwd (разработка из воркспейса).

Схема и миграции живут в двух местах намеренно (store.rs и
store-server.js) — оба файла обязаны меняться вместе; тест
`schema_matches_js_sidecar` сверяет наборы CREATE TABLE.
*/

use std::io::{BufRead, BufReader, Write};
use std::path::{Path, PathBuf};
use std::process::{Child, ChildStdin, ChildStdout, Command, Stdio};
use std::sync::Mutex;

use serde_json::{json, Value};

use crate::session::{Session, SessionId, SessionStatus, TurnRecord};
use crate::store::{
    policy_from_str_opt, policy_to_str, role_from_str, role_to_str, ApprovalEntry, Store,
    StoreError, StoreResult,
};
use swagcod_provider::types::{ChatMessage, ToolCall};

/// better-sqlite3-хранилище: дочерний Node-процесс + синхронный RPC.
pub struct NodeStore {
    io: Mutex<Io>,
    child: Mutex<Child>,
}

struct Io {
    stdin: ChildStdin,
    stdout: BufReader<ChildStdout>,
    next_id: u64,
}

impl NodeStore {
    /// Открыть (и создать) базу по пути через sidecar.
    pub fn open(path: &Path) -> StoreResult<Self> {
        Self::start(path)
    }

    /// База в памяти sidecar-процесса — для тестов.
    pub fn in_memory() -> StoreResult<Self> {
        Self::start(Path::new(":memory:"))
    }

    /// Целостность базы (PRAGMA integrity_check) — parity с SqliteStore.
    pub fn integrity_check(&self) -> StoreResult<String> {
        let resp = self.rpc(json!({"method": "integrity"}))?;
        Ok(resp["result"].as_str().unwrap_or("").to_string())
    }

    fn start(path: &Path) -> StoreResult<Self> {
        let script = sidecar_script()?;
        let node = node_binary();
        let mut child = Command::new(&node)
            .arg(&script)
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            // stderr sidecar'а — в наш stderr: логи сервера видно, протокол чист.
            .stderr(Stdio::inherit())
            .spawn()
            .map_err(|e| StoreError::Sidecar(format!("не поднял sidecar ({node}): {e}")))?;
        let stdin = child
            .stdin
            .take()
            .ok_or_else(|| StoreError::Sidecar("нет stdin у sidecar".into()))?;
        let stdout = child
            .stdout
            .take()
            .ok_or_else(|| StoreError::Sidecar("нет stdout у sidecar".into()))?;
        let store = Self {
            io: Mutex::new(Io {
                stdin,
                stdout: BufReader::new(stdout),
                next_id: 0,
            }),
            child: Mutex::new(child),
        };
        /* Запросы буферизуются пайпом: писать можно до полной готовности
           сервера — readline разберёт строки, когда поднимется. ping +
           open дают и readiness-гарантию, и инициализацию схемы. */
        store.rpc(json!({"method": "ping"}))?;
        store.rpc(json!({"method": "open", "path": path.to_string_lossy()}))?;
        Ok(store)
    }

    /// Один запрос-ответ. Ответов ровно столько, сколько запросов, и они
    /// идут по порядку — read_line после write корректен без диспетчера.
    fn rpc(&self, mut req: Value) -> StoreResult<Value> {
        let mut io = self
            .io
            .lock()
            .map_err(|_| StoreError::Sidecar("канал sidecar отравлен".into()))?;
        io.next_id += 1;
        req["id"] = json!(io.next_id);
        writeln!(io.stdin, "{req}")?;
        io.stdin.flush()?;
        let mut line = String::new();
        let n = io.stdout.read_line(&mut line)?;
        if n == 0 {
            return Err(StoreError::Sidecar("sidecar закрыл канал (процесс умер?)".into()));
        }
        let resp: Value = serde_json::from_str(line.trim())?;
        if resp["ok"] != json!(true) {
            return Err(StoreError::Sidecar(
                resp["error"].as_str().unwrap_or("неизвестная ошибка").to_string(),
            ));
        }
        Ok(resp)
    }

    fn exec(&self, sql: &str, params: Vec<Value>) -> StoreResult<Value> {
        self.rpc(json!({"method": "exec", "sql": sql, "params": params}))
    }

    fn query(&self, sql: &str, params: Vec<Value>) -> StoreResult<Vec<Vec<Value>>> {
        let resp = self.rpc(json!({"method": "query", "sql": sql, "params": params}))?;
        let mut rows = Vec::new();
        if let Some(arr) = resp["rows"].as_array() {
            for r in arr {
                rows.push(r.as_array().cloned().unwrap_or_default());
            }
        }
        Ok(rows)
    }

    fn batch(&self, steps: Vec<Value>) -> StoreResult<()> {
        self.rpc(json!({"method": "batch", "steps": steps}))?;
        Ok(())
    }
}

impl Drop for NodeStore {
    fn drop(&mut self) {
        // Вежливое завершение; если sidecar уже мёртв, rpc вернёт ошибку
        // мгновенно (EOF), и kill добьёт процесс.
        let _ = self.rpc(json!({"method": "shutdown"}));
        if let Ok(mut child) = self.child.lock() {
            let _ = child.kill();
            let _ = child.wait();
        }
    }
}

/// E-1: каким node запускать sidecar. Порядок: явный `SWAGCOD_NODE` →
/// упакованный `node.exe` рядом с exe (бандл везёт свой рантайм — ABI
/// совпадает с бинарником better-sqlite3 гарантированно) → `vendor/node`
/// (локальный расклад инструментов упаковки) → node из PATH (разработка).
/// E-4: pub — app-слой переиспользует порядок для JS-плагинов (один
/// рантайм на все sidecar-процессы).
pub fn node_binary() -> String {
    if let Ok(p) = std::env::var("SWAGCOD_NODE") {
        if !p.is_empty() {
            return p;
        }
    }
    if let Ok(exe) = std::env::current_exe() {
        if let Some(dir) = exe.parent() {
            let bundled = dir.join("node.exe");
            if bundled.is_file() {
                return bundled.to_string_lossy().to_string();
            }
            let vendored = dir.join("vendor").join("node").join("node.exe");
            if vendored.is_file() {
                return vendored.to_string_lossy().to_string();
            }
        }
    }
    "node".to_string()
}

/// E-4: найти любой файл sidecar-расклада (store-server.js,
/// plugin-server.js) по тем же кандидатам: рядом с exe (бандл) → vendor
/// рядом с exe → от cwd и вверх по предкам, включая vendor.
pub fn sidecar_file(name: &str) -> Option<PathBuf> {
    const CANDIDATES: &[&str] = &["sidecar", "vendor/sidecar", "vendor\\sidecar"];
    if let Ok(exe) = std::env::current_exe() {
        if let Some(dir) = exe.parent() {
            for rel in CANDIDATES {
                let p = dir.join(rel).join(name);
                if p.is_file() {
                    return Some(p);
                }
            }
        }
    }
    if let Ok(cwd) = std::env::current_dir() {
        let mut dir = cwd.as_path();
        for _ in 0..6 {
            for rel in CANDIDATES {
                let p = dir.join(rel).join(name);
                if p.is_file() {
                    return Some(p);
                }
            }
            match dir.parent() {
                Some(parent) => dir = parent,
                None => break,
            }
        }
    }
    None
}

/// Где искать скрипт sidecar'а: env → рядом с exe (бандл) → vendor рядом
/// с exe → от cwd и вверх по предкам, включая vendor (тесты cargo стартуют
/// из каталога крейта, разработка — из воркспейса).
fn sidecar_script() -> StoreResult<PathBuf> {
    if let Ok(p) = std::env::var("SWAGCOD_STORE_SIDECAR") {
        return Ok(PathBuf::from(p));
    }
    sidecar_file("store-server.js").ok_or_else(|| {
        StoreError::Sidecar(
            "sidecar/store-server.js не найден (env SWAGCOD_STORE_SIDECAR, каталог exe, vendor или cwd)".into(),
        )
    })
}

/* ── Декодирование строк: JSON-значения колонок в доменные типы. ── */

fn v_text(v: &Value) -> String {
    v.as_str().unwrap_or_default().to_string()
}

fn v_opt_text(v: &Value) -> Option<String> {
    v.as_str().map(|s| s.to_string())
}

fn v_int(v: &Value) -> i64 {
    v.as_i64().unwrap_or(0)
}

fn v_bool(v: &Value) -> bool {
    match v {
        Value::Bool(b) => *b,
        other => other.as_i64().unwrap_or(0) != 0,
    }
}

fn col(row: &[Value], i: usize) -> &Value {
    static NULL: Value = Value::Null;
    row.get(i).unwrap_or(&NULL)
}

impl Store for NodeStore {
    fn create_session(&self, s: &Session) -> StoreResult<()> {
        self.exec(
            "INSERT OR IGNORE INTO sessions(id, cwd, model, title, approval_policy, created_ms) VALUES (?1, ?2, ?3, ?4, ?5, ?6)",
            vec![
                json!(s.id.as_str()),
                json!(s.cwd),
                json!(s.model),
                json!(s.title),
                s.approval_policy.map(policy_to_str).map(|p| json!(p)).unwrap_or(Value::Null),
                json!(s.created_ms as i64),
            ],
        )?;
        Ok(())
    }

    fn set_session_title(&self, id: &str, title: &str) -> StoreResult<()> {
        self.exec(
            "UPDATE sessions SET title = ?2 WHERE id = ?1",
            vec![json!(id), json!(title)],
        )?;
        Ok(())
    }

    fn set_session_summary(&self, id: &str, summary: &str) -> StoreResult<()> {
        self.exec(
            "UPDATE sessions SET summary = ?2 WHERE id = ?1",
            vec![json!(id), json!(summary)],
        )?;
        Ok(())
    }

    fn delete_session(&self, id: &str) -> StoreResult<()> {
        self.exec("DELETE FROM sessions WHERE id = ?1", vec![json!(id)])?;
        Ok(())
    }

    fn load_all(&self) -> StoreResult<Vec<Session>> {
        let rows = self.query(
            "SELECT id, cwd, model, title, summary, approval_policy, created_ms FROM sessions ORDER BY created_ms, id",
            vec![],
        )?;
        let mut out = Vec::with_capacity(rows.len());
        for row in rows {
            let mut ses = Session::new(
                SessionId::new(v_text(col(&row, 0))),
                v_text(col(&row, 1)),
                v_text(col(&row, 2)),
            );
            ses.title = v_text(col(&row, 3));
            ses.summary = v_text(col(&row, 4));
            ses.approval_policy = policy_from_str_opt(v_opt_text(col(&row, 5)));
            ses.created_ms = v_int(col(&row, 6)) as u64;
            ses.status = SessionStatus::Idle;
            self.fill_session(&mut ses)?;
            out.push(ses);
        }
        Ok(out)
    }

    fn load_session(&self, id: &str) -> StoreResult<Option<Session>> {
        let rows = self.query(
            "SELECT cwd, model, title, summary, approval_policy, created_ms FROM sessions WHERE id = ?1",
            vec![json!(id)],
        )?;
        let Some(row) = rows.into_iter().next() else {
            return Ok(None);
        };
        let mut ses = Session::new(
            SessionId::new(id),
            v_text(col(&row, 0)),
            v_text(col(&row, 1)),
        );
        ses.title = v_text(col(&row, 2));
        ses.summary = v_text(col(&row, 3));
        ses.approval_policy = policy_from_str_opt(v_opt_text(col(&row, 4)));
        ses.created_ms = v_int(col(&row, 5)) as u64;
        ses.status = SessionStatus::Idle;
        self.fill_session(&mut ses)?;
        Ok(Some(ses))
    }

    fn save_turn(&self, session_id: &str, turn: &TurnRecord, history: &[ChatMessage]) -> StoreResult<()> {
        let tools_json = serde_json::to_string(&turn.tool_calls)?;
        /* Ход и снимок истории — одна транзакция (batch на сервере):
           оборванный процесс не оставляет полупустой ход. */
        let mut steps = Vec::with_capacity(2 + history.len());
        steps.push(json!({
            "sql": "INSERT OR REPLACE INTO turns(id, session_id, started_ms, ended_ms, ok, failure, content, reasoning, tool_calls_json, est_in, est_out)
                    VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11)",
            "params": [
                json!(turn.id.as_str()),
                json!(session_id),
                json!(turn.started_ms as i64),
                turn.ended_ms.map(|v| json!(v as i64)).unwrap_or(Value::Null),
                json!(if turn.ok { 1 } else { 0 }),
                turn.failure.as_deref().map(|f| json!(f)).unwrap_or(Value::Null),
                json!(turn.content),
                json!(turn.reasoning),
                json!(tools_json),
                json!(turn.est_input_tokens as i64),
                json!(turn.est_output_tokens as i64)
            ]
        }));
        steps.push(json!({
            "sql": "DELETE FROM messages WHERE session_id = ?1",
            "params": [json!(session_id)]
        }));
        for (ord, m) in history.iter().enumerate() {
            let tools = serde_json::to_string(&m.tool_calls)?;
            steps.push(json!({
                "sql": "INSERT INTO messages(session_id, ord, role, content, reasoning, tool_call_id, tool_calls_json)
                        VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7)",
                "params": [
                    json!(session_id),
                    json!(ord as i64),
                    json!(role_to_str(m.role)),
                    json!(m.content),
                    json!(m.reasoning),
                    m.tool_call_id.as_deref().map(|c| json!(c)).unwrap_or(Value::Null),
                    json!(tools)
                ]
            }));
        }
        self.batch(steps)
    }

    fn get_pref(&self, key: &str) -> StoreResult<Option<String>> {
        let rows = self.query("SELECT value FROM prefs WHERE key = ?1", vec![json!(key)])?;
        Ok(rows.into_iter().next().map(|r| v_text(col(&r, 0))))
    }

    fn set_pref(&self, key: &str, value: &str) -> StoreResult<()> {
        self.exec(
            "INSERT INTO prefs(key, value) VALUES (?1, ?2) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
            vec![json!(key), json!(value)],
        )?;
        Ok(())
    }

    fn set_session_policy(&self, id: &str, policy: Option<&str>) -> StoreResult<()> {
        self.exec(
            "UPDATE sessions SET approval_policy = ?2 WHERE id = ?1",
            vec![json!(id), policy.map(|p| json!(p)).unwrap_or(Value::Null)],
        )?;
        Ok(())
    }

    fn log_approval(&self, e: &ApprovalEntry) -> StoreResult<()> {
        self.exec(
            "INSERT INTO approvals(session_id, turn_id, call_id, tool, summary, decision, actor, decided_ms)
             VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8)",
            vec![
                json!(e.session_id),
                json!(e.turn_id),
                json!(e.call_id),
                json!(e.tool),
                json!(e.summary),
                json!(e.decision),
                json!(e.actor),
                json!(e.decided_ms as i64),
            ],
        )?;
        Ok(())
    }

    fn approval_log(&self, session_id: Option<&str>, limit: usize) -> StoreResult<Vec<ApprovalEntry>> {
        let limit = limit.clamp(1, 5000) as i64;
        let select = "SELECT session_id, turn_id, call_id, tool, summary, decision, actor, decided_ms FROM approvals";
        let rows = match session_id {
            Some(sid) => self.query(
                &format!("{select} WHERE session_id = ?1 ORDER BY decided_ms DESC, id DESC LIMIT ?2"),
                vec![json!(sid), json!(limit)],
            )?,
            None => self.query(
                &format!("{select} ORDER BY decided_ms DESC, id DESC LIMIT ?1"),
                vec![json!(limit)],
            )?,
        };
        Ok(rows
            .iter()
            .map(|r| ApprovalEntry {
                session_id: v_text(col(r, 0)),
                turn_id: v_text(col(r, 1)),
                call_id: v_text(col(r, 2)),
                tool: v_text(col(r, 3)),
                summary: v_text(col(r, 4)),
                decision: v_text(col(r, 5)),
                actor: v_text(col(r, 6)),
                decided_ms: v_int(col(r, 7)) as u64,
            })
            .collect())
    }

    fn task_enqueue(&self, t: &crate::tasks::Task) -> StoreResult<()> {
        self.exec(
            "INSERT OR IGNORE INTO tasks(id, kind, payload, state, attempts, next_try_ms, last_error, every_ms, created_ms, updated_ms)
             VALUES (?1, ?2, ?3, 'queued', 0, ?4, '', ?5, ?6, ?6)",
            vec![
                json!(t.id),
                json!(t.kind),
                json!(t.payload.to_string()),
                json!(t.next_try_ms as i64),
                t.every_ms.map(|v| json!(v as i64)).unwrap_or(Value::Null),
                json!(t.created_ms as i64),
            ],
        )?;
        Ok(())
    }

    fn task_claim_due(&self, now_ms: u64) -> StoreResult<Vec<crate::tasks::Task>> {
        let rows = self.query(
            "SELECT id FROM tasks WHERE state = 'queued' AND next_try_ms <= ?1 ORDER BY next_try_ms, id LIMIT 8",
            vec![json!(now_ms as i64)],
        )?;
        let ids: Vec<String> = rows.iter().map(|r| v_text(col(r, 0))).collect();
        let mut out = Vec::new();
        for id in ids {
            // Guard state='queued': защита от двойного claim.
            self.exec(
                "UPDATE tasks SET state = 'running', attempts = attempts + 1,
                 updated_ms = CAST(strftime('%s','now') AS INTEGER) * 1000
                 WHERE id = ?1 AND state = 'queued'",
                vec![json!(id)],
            )?;
            if let Some(t) = self.task_get(&id)? {
                if t.state == crate::tasks::TASK_RUNNING {
                    out.push(t);
                }
            }
        }
        Ok(out)
    }

    fn task_get(&self, id: &str) -> StoreResult<Option<crate::tasks::Task>> {
        let sql = format!("SELECT {} FROM tasks WHERE id = ?1", crate::store::TASK_COLS);
        let rows = self.query(&sql, vec![json!(id)])?;
        Ok(rows.into_iter().next().map(|r| task_from_values(&r)))
    }

    fn task_update(
        &self,
        id: &str,
        state: &str,
        message: &str,
        attempts: u32,
        next_try_ms: u64,
    ) -> StoreResult<()> {
        self.exec(
            "UPDATE tasks SET state = ?2, last_error = ?3, attempts = ?4, next_try_ms = ?5,
             updated_ms = CAST(strftime('%s','now') AS INTEGER) * 1000
             WHERE id = ?1",
            vec![
                json!(id),
                json!(state),
                json!(message),
                json!(attempts as i64),
                json!(next_try_ms as i64),
            ],
        )?;
        Ok(())
    }

    fn tasks_list(&self, limit: usize) -> StoreResult<Vec<crate::tasks::Task>> {
        let limit = limit.clamp(1, 1000) as i64;
        let sql = format!(
            "SELECT {} FROM tasks ORDER BY created_ms DESC, id DESC LIMIT ?1",
            crate::store::TASK_COLS
        );
        let rows = self.query(&sql, vec![json!(limit)])?;
        Ok(rows.iter().map(|r| task_from_values(r)).collect())
    }

    fn tasks_recover_running(&self) -> StoreResult<()> {
        self.exec(
            "UPDATE tasks SET state = 'queued', next_try_ms = 0,
             updated_ms = CAST(strftime('%s','now') AS INTEGER) * 1000
             WHERE state = 'running'",
            vec![],
        )?;
        Ok(())
    }

    fn tasks_prune(&self, before_ms: u64) -> StoreResult<()> {
        // u64::MAX as i64 дал бы -1: зажимаем до i64::MAX («удалить всё»).
        let before = before_ms.min(i64::MAX as u64) as i64;
        self.exec(
            "DELETE FROM tasks WHERE state = 'done' AND updated_ms < ?1",
            vec![json!(before)],
        )?;
        Ok(())
    }
}

/// E-5: строка tasks из sidecar-ответа — порядок колонок TASK_COLS.
fn task_from_values(r: &[Value]) -> crate::tasks::Task {
    crate::tasks::Task {
        id: v_text(col(r, 0)),
        kind: v_text(col(r, 1)),
        payload: serde_json::from_str(&v_text(col(r, 2))).unwrap_or_else(|_| json!({})),
        state: v_text(col(r, 3)),
        attempts: v_int(col(r, 4)).max(0) as u32,
        next_try_ms: v_int(col(r, 5)).max(0) as u64,
        last_error: v_text(col(r, 6)),
        every_ms: col(r, 7).as_i64().map(|v| v.max(0) as u64),
        created_ms: v_int(col(r, 8)).max(0) as u64,
        updated_ms: v_int(col(r, 9)).max(0) as u64,
    }
}

impl NodeStore {
    /// Ходы и сообщения сессии — аналог fill_session для rusqlite.
    fn fill_session(&self, ses: &mut Session) -> StoreResult<()> {
        let trows = self.query(
            "SELECT id, started_ms, ended_ms, ok, failure, content, reasoning, tool_calls_json, est_in, est_out
             FROM turns WHERE session_id = ?1 ORDER BY started_ms, id",
            vec![json!(ses.id.as_str())],
        )?;
        ses.turns = trows
            .iter()
            .map(|r| TurnRecord {
                id: crate::session::TurnId::new(v_text(col(r, 0))),
                started_ms: v_int(col(r, 1)) as u64,
                ended_ms: col(r, 2).as_i64().map(|v| v as u64),
                ok: v_bool(col(r, 3)),
                failure: v_opt_text(col(r, 4)),
                content: v_text(col(r, 5)),
                reasoning: v_text(col(r, 6)),
                tool_calls: serde_json::from_str(&v_text(col(r, 7))).unwrap_or_default(),
                est_input_tokens: v_int(col(r, 8)) as u32,
                est_output_tokens: v_int(col(r, 9)) as u32,
            })
            .collect();

        let mrows = self.query(
            "SELECT role, content, reasoning, tool_call_id, tool_calls_json
             FROM messages WHERE session_id = ?1 ORDER BY ord, id",
            vec![json!(ses.id.as_str())],
        )?;
        let mut history = Vec::with_capacity(mrows.len());
        for r in mrows {
            let tool_calls: Vec<ToolCall> = serde_json::from_str(&v_text(col(&r, 4)))?;
            history.push(ChatMessage {
                role: role_from_str(&v_text(col(&r, 0))),
                content: v_text(col(&r, 1)),
                reasoning: v_text(col(&r, 2)),
                tool_calls,
                tool_call_id: v_opt_text(col(&r, 3)),
            });
        }
        ses.history = history;
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::session::TurnId;

    /// Нет Node или sidecar не собрался — тесты честно пропускаются:
    /// встроенный rusqlite покрывает тот же контракт своим набором.
    fn try_memory() -> Option<NodeStore> {
        match NodeStore::in_memory() {
            Ok(s) => Some(s),
            Err(e) => {
                eprintln!("node_store: пропускаю, sidecar недоступен: {e}");
                None
            }
        }
    }

    fn sample_session() -> Session {
        let mut ses = Session::new(SessionId::new("s-1"), "D:/proj", "test-model");
        ses.title = "проба".to_string();
        ses.push_user_message("привет");
        ses.push_assistant_message(ChatMessage {
            role: swagcod_provider::types::Role::Assistant,
            content: "здравствуй".to_string(),
            reasoning: "думаю".to_string(),
            tool_calls: vec![ToolCall {
                id: "c-1".to_string(),
                name: "read".to_string(),
                arguments: serde_json::Map::new(),
            }],
            tool_call_id: None,
        });
        ses.push_tool_result("c-1", "содержимое файла");
        ses
    }

    fn sample_turn(n: u64) -> TurnRecord {
        TurnRecord {
            id: TurnId::new(format!("t-{n}")),
            started_ms: 1000 + n,
            ended_ms: Some(2000 + n),
            content: format!("ответ {n}"),
            reasoning: format!("мысли {n}"),
            tool_calls: vec![],
            est_input_tokens: 10,
            est_output_tokens: 20,
            ok: true,
            failure: None,
        }
    }

    #[test]
    fn node_session_roundtrip_with_history_and_policy() {
        let Some(store) = try_memory() else { return };
        let mut ses = sample_session();
        ses.approval_policy = Some(crate::turn::ApprovalPolicy::Never);
        store.create_session(&ses).unwrap();
        store
            .save_turn("s-1", &sample_turn(1), &ses.history)
            .unwrap();

        let loaded = store.load_session("s-1").unwrap().expect("сессия есть");
        assert_eq!(loaded.title, "проба");
        assert_eq!(loaded.approval_policy, Some(crate::turn::ApprovalPolicy::Never));
        assert_eq!(loaded.turns.len(), 1);
        assert_eq!(loaded.turns[0].content, "ответ 1");
        assert!(loaded.turns[0].ok);
        assert_eq!(loaded.history.len(), ses.history.len());
        // Инструментальный хвост восстановился байт-в-байт.
        assert_eq!(loaded.history, ses.history);

        // Политика снимается обратно в «как глобальная».
        store.set_session_policy("s-1", None).unwrap();
        let cleared = store.load_session("s-1").unwrap().unwrap();
        assert_eq!(cleared.approval_policy, None);

        assert!(store.load_session("нет-такой").unwrap().is_none());
    }

    #[test]
    fn node_prefs_and_journal_survive_session_delete() {
        let Some(store) = try_memory() else { return };
        let ses = sample_session();
        store.create_session(&ses).unwrap();

        store.set_pref("k1", "v1").unwrap();
        assert_eq!(store.get_pref("k1").unwrap().as_deref(), Some("v1"));
        store.set_pref("k1", "v2").unwrap();
        assert_eq!(store.get_pref("k1").unwrap().as_deref(), Some("v2"));
        assert_eq!(store.get_pref("missing").unwrap(), None);

        store
            .log_approval(&ApprovalEntry {
                session_id: "s-1".into(),
                turn_id: "t-1".into(),
                call_id: "c-1".into(),
                tool: "bash".into(),
                summary: "rm -rf".into(),
                decision: "denied".into(),
                actor: "user".into(),
                decided_ms: 42,
            })
            .unwrap();
        store.delete_session("s-1").unwrap();

        // Аудит важнее каскада: журнал переживает удаление сессии.
        let log = store.approval_log(None, 10).unwrap();
        assert_eq!(log.len(), 1);
        assert_eq!(log[0].decision, "denied");
        assert_eq!(log[0].decided_ms, 42);
        assert!(store.load_session("s-1").unwrap().is_none());
    }

    #[test]
    fn node_load_all_orders_by_created() {
        let Some(store) = try_memory() else { return };
        let mut a = Session::new(SessionId::new("s-b"), "D:/b", "m");
        a.created_ms = 200;
        let mut b = Session::new(SessionId::new("s-a"), "D:/a", "m");
        b.created_ms = 100;
        store.create_session(&a).unwrap();
        store.create_session(&b).unwrap();
        let all = store.load_all().unwrap();
        let ids: Vec<_> = all.iter().map(|s| s.id.as_str().to_string()).collect();
        assert_eq!(ids, vec!["s-a", "s-b"]);
    }

    #[test]
    fn node_integrity_ok() {
        let Some(store) = try_memory() else { return };
        assert_eq!(store.integrity_check().unwrap(), "ok");
    }

    /// E-1: тестовый exe живёт в target/*/deps — там нет ни node.exe, ни
    /// vendor, поэтому резолв обязан упасть в PATH-`node` (если только
    /// SWAGCOD_NODE не переопределил выбор явно).
    #[test]
    fn node_binary_falls_back_to_path_node() {
        if std::env::var_os("SWAGCOD_NODE").is_some() {
            return;
        }
        assert_eq!(node_binary(), "node");
    }

    /// Схема не разъезжается с JS-sidecar: одинаковый набор таблиц.
    #[test]
    fn schema_matches_js_sidecar() {
        let js = std::fs::read_to_string(sidecar_script().expect("скрипт sidecar найдён от cwd"))
            .expect("скрипт читается");
        for table in ["sessions", "turns", "messages", "prefs", "approvals"] {
            assert!(
                js.contains(&format!("CREATE TABLE IF NOT EXISTS {table}(")),
                "в store-server.js нет таблицы {table}"
            );
        }
        for col in ["summary", "approval_policy"] {
            assert!(
                js.contains(&format!("ALTER TABLE sessions ADD COLUMN {col}")),
                "в store-server.js нет миграции колонки {col}"
            );
        }
    }
}
