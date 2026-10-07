/*! Персистентность сессий и настроек (этап B-1).

Синхронный SQLite, вмурованный в бинарник (`rusqlite` + `bundled`): философия
better-sqlite — sync API без async-обвязки, потому что SQLite быстр, а async
над ним дал бы только сложность. Режим WAL: запись в конце хода не блокирует
читателей во время стрима.

Схема минимальна и соответствует домену: сессия, ходы, сообщения истории и
ключ-значение настроек. История хода пишется пачкой в одной транзакции в
конце хода — оборванный процесс не оставляет полупустой ход (WAL + транзакция).
*/

use std::path::Path;

use rusqlite::{params, Connection};

use crate::session::{Session, SessionId, SessionStatus, TurnRecord};
use swagcod_provider::types::{ChatMessage, Role, ToolCall};

#[derive(Debug, thiserror::Error)]
pub enum StoreError {
    #[error("база: {0}")]
    Db(#[from] rusqlite::Error),
    #[error("файл: {0}")]
    Io(#[from] std::io::Error),
    #[error("json: {0}")]
    Json(#[from] serde_json::Error),
    /// Ревизия 25: ошибки Node-sidecar (better-sqlite3) — процесс, канал, SQL.
    #[error("sidecar: {0}")]
    Sidecar(String),
}

pub type StoreResult<T> = Result<T, StoreError>;

/// B-7: запись журнала подтверждений.
#[derive(Debug, Clone, PartialEq, serde::Serialize)]
pub struct ApprovalEntry {
    pub session_id: String,
    pub turn_id: String,
    pub call_id: String,
    pub tool: String,
    pub summary: String,
    /// "approved" | "denied".
    pub decision: String,
    /// "user" — человек в диалоге; "system" — канал потерян (ход остановлен),
    /// молчаливое одобрение исключено политикой.
    pub actor: String,
    pub decided_ms: u64,
}

/// Схема и режимы. WAL и NORMAL выставляются сразу: после перезапуска
/// приложения база обязана продолжать писать без настройки извне.
const SCHEMA: &str = r#"
PRAGMA journal_mode = WAL;
PRAGMA synchronous = NORMAL;
PRAGMA foreign_keys = ON;
CREATE TABLE IF NOT EXISTS sessions(
  id TEXT PRIMARY KEY,
  cwd TEXT NOT NULL,
  model TEXT NOT NULL,
  title TEXT NOT NULL DEFAULT '',
  summary TEXT NOT NULL DEFAULT '',
  approval_policy TEXT,
  created_ms INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS turns(
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  started_ms INTEGER NOT NULL,
  ended_ms INTEGER,
  ok INTEGER NOT NULL,
  failure TEXT,
  content TEXT NOT NULL,
  reasoning TEXT NOT NULL,
  tool_calls_json TEXT NOT NULL DEFAULT '[]',
  est_in INTEGER NOT NULL DEFAULT 0,
  est_out INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS messages(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  ord INTEGER NOT NULL,
  role TEXT NOT NULL,
  content TEXT NOT NULL,
  reasoning TEXT NOT NULL DEFAULT '',
  tool_call_id TEXT,
  tool_calls_json TEXT NOT NULL DEFAULT '[]'
);
CREATE TABLE IF NOT EXISTS prefs(
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
/* B-7: журнал подтверждений — кто, что, когда одобрил или отклонил.
   Автономная таблица (не FK на turns): запись обязана пережить любой
   обрыв хода, а аудит важнее нормализации. */
CREATE TABLE IF NOT EXISTS approvals(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id TEXT NOT NULL,
  turn_id TEXT NOT NULL,
  call_id TEXT NOT NULL,
  tool TEXT NOT NULL,
  summary TEXT NOT NULL DEFAULT '',
  decision TEXT NOT NULL,
  actor TEXT NOT NULL DEFAULT 'user',
  decided_ms INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS tasks(
  id TEXT PRIMARY KEY,
  kind TEXT NOT NULL,
  payload TEXT NOT NULL DEFAULT '{}',
  state TEXT NOT NULL DEFAULT 'queued',
  attempts INTEGER NOT NULL DEFAULT 0,
  next_try_ms INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  every_ms INTEGER,
  created_ms INTEGER NOT NULL,
  updated_ms INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_turns_session ON turns(session_id, started_ms);
CREATE INDEX IF NOT EXISTS idx_messages_session ON messages(session_id, ord);
CREATE INDEX IF NOT EXISTS idx_approvals_session ON approvals(session_id, decided_ms);
CREATE INDEX IF NOT EXISTS idx_tasks_due ON tasks(state, next_try_ms);
"#;

/// Хранилище: синхронный контракт домена над персистентностью.
///
/// Ревизия 25: реализаций две — основной [`crate::node_store::NodeStore`]
/// (better-sqlite3 в Node-sidecar, как просил пользователь) и аварийный
/// [`SqliteStore`] (встроенный rusqlite, если Node нет). Выбор — через
/// фабрики [`open`]/[`open_memory`]. Send обязателен: состояние приложения
/// носит трейт-объект за Mutex между потоками.
pub trait Store: Send {
    fn create_session(&self, s: &Session) -> StoreResult<()>;
    fn set_session_title(&self, id: &str, title: &str) -> StoreResult<()>;
    /// B-3: свёртка старых ходов живёт рядом с сессией.
    fn set_session_summary(&self, id: &str, summary: &str) -> StoreResult<()>;
    fn delete_session(&self, id: &str) -> StoreResult<()>;
    /// Все сессии с историей и ходами, в порядке создания.
    fn load_all(&self) -> StoreResult<Vec<Session>>;
    fn load_session(&self, id: &str) -> StoreResult<Option<Session>>;
    /// Ход плюс снимок истории пишутся одной транзакцией.
    fn save_turn(&self, session_id: &str, turn: &TurnRecord, history: &[ChatMessage]) -> StoreResult<()>;
    fn get_pref(&self, key: &str) -> StoreResult<Option<String>>;
    fn set_pref(&self, key: &str, value: &str) -> StoreResult<()>;
    /// B-7: per-session политика подтверждений (None = глобальная).
    fn set_session_policy(&self, id: &str, policy: Option<&str>) -> StoreResult<()>;
    /// B-7: журнал подтверждений.
    fn log_approval(&self, e: &ApprovalEntry) -> StoreResult<()>;
    fn approval_log(&self, session_id: Option<&str>, limit: usize) -> StoreResult<Vec<ApprovalEntry>>;
    /// E-5: очередь фоновых задач. id — ключ дедупликации (INSERT OR
    /// IGNORE): сид периодической задачи идемпотентен, отмена не
    /// воскрешается повторным сидом.
    fn task_enqueue(&self, t: &crate::tasks::Task) -> StoreResult<()>;
    /// E-5: забрать задачи с наступившим сроком (queued, next_try_ms <=
    /// now): перевод в running со счётчиком попыток и возврат списка.
    fn task_claim_due(&self, now_ms: u64) -> StoreResult<Vec<crate::tasks::Task>>;
    fn task_get(&self, id: &str) -> StoreResult<Option<crate::tasks::Task>>;
    /// E-5: исход задачи — состояние, сообщение журнала (last_error),
    /// попытки и следующий срок. updated_ms ставит база (strftime).
    fn task_update(
        &self,
        id: &str,
        state: &str,
        message: &str,
        attempts: u32,
        next_try_ms: u64,
    ) -> StoreResult<()>;
    fn tasks_list(&self, limit: usize) -> StoreResult<Vec<crate::tasks::Task>>;
    /// E-5: сироты после краша — вернуть running в очередь.
    fn tasks_recover_running(&self) -> StoreResult<()>;
    /// E-5: гигиена — удалить старые done.
    fn tasks_prune(&self, before_ms: u64) -> StoreResult<()>;
}

/// SQLite-реализация контракта [`Store`].
pub struct SqliteStore {
    conn: Connection,
}

impl SqliteStore {
    /// Открыть (и создать) базу по пути, подняв схему и режимы.
    pub fn open(path: &Path) -> StoreResult<Self> {
        if let Some(dir) = path.parent() {
            if !dir.as_os_str().is_empty() {
                std::fs::create_dir_all(dir)?;
            }
        }
        let conn = Connection::open(path)?;
        conn.execute_batch(SCHEMA)?;
        // Миграция B-3: колонка свёртки для баз, созданных до ревизии 18.
        // «duplicate column» — нормальный повторный запуск, игнорируем.
        let _ = conn.execute(
            "ALTER TABLE sessions ADD COLUMN summary TEXT NOT NULL DEFAULT ''",
            [],
        );
        // Миграция B-7: per-session политика подтверждений.
        let _ = conn.execute("ALTER TABLE sessions ADD COLUMN approval_policy TEXT", []);
        Ok(Self { conn })
    }

    /// In-memory база для тестов: та же схема, те же режимы.
    pub fn in_memory() -> StoreResult<Self> {
        let conn = Connection::open_in_memory()?;
        conn.execute_batch(SCHEMA)?;
        Ok(Self { conn })
    }

    /// Целостность базы: используется краш-тестами после обрыва процесса.
    pub fn integrity_check(&self) -> StoreResult<String> {
        let ok: String = self.conn.query_row("PRAGMA integrity_check", [], |r| r.get(0))?;
        Ok(ok)
    }
}

pub(crate) fn role_to_str(role: Role) -> &'static str {
    match role {
        Role::System => "system",
        Role::User => "user",
        Role::Assistant => "assistant",
        Role::Tool => "tool",
    }
}

pub(crate) fn role_from_str(s: &str) -> Role {
    match s {
        "system" => Role::System,
        "user" => Role::User,
        "assistant" => Role::Assistant,
        _ => Role::Tool,
    }
}

/* B-7: политика сессии хранится строкой — теми же значениями, что serde
   пишет в wire-формат (snake_case), чтобы журнал и конфиг читались одинаково. */
pub(crate) fn policy_to_str(p: crate::turn::ApprovalPolicy) -> &'static str {
    match p {
        crate::turn::ApprovalPolicy::Always => "always",
        crate::turn::ApprovalPolicy::Never => "never",
        crate::turn::ApprovalPolicy::OnDangerous => "on_dangerous",
    }
}

pub(crate) fn policy_from_str_opt(s: Option<String>) -> Option<crate::turn::ApprovalPolicy> {
    match s.as_deref() {
        Some("always") => Some(crate::turn::ApprovalPolicy::Always),
        Some("never") => Some(crate::turn::ApprovalPolicy::Never),
        Some("on_dangerous") => Some(crate::turn::ApprovalPolicy::OnDangerous),
        _ => None,
    }
}

/* Ревизия 25: выбор хранилища. better-sqlite3 через Node-sidecar —
   основное хранилище (прямое требование пользователя); встроенный
   rusqlite — аварийный фолбэк для окружений без Node. SWAGCOD_STORE
   форсирует выбор: `node` — только sidecar (падение громкое, без
   тихой подмены), `sqlite` — только встроенный, иначе auto. */

/// Открыть файловую базу: sidecar, при неудаче (в auto) — rusqlite.
pub fn open(path: &Path) -> StoreResult<Box<dyn Store>> {
    let mode = std::env::var("SWAGCOD_STORE").unwrap_or_else(|_| "auto".to_string());
    if mode != "sqlite" {
        match crate::node_store::NodeStore::open(path) {
            Ok(s) => return Ok(Box::new(s)),
            Err(e) => {
                if mode == "node" {
                    return Err(e);
                }
                eprintln!("store: better-sqlite3 sidecar недоступен, фолбэк на встроенный SQLite: {e}");
            }
        }
    }
    Ok(Box::new(SqliteStore::open(path)?))
}

/// In-memory база: sidecar для тестов, rusqlite если Node не завёлся.
pub fn open_memory() -> Box<dyn Store> {
    let mode = std::env::var("SWAGCOD_STORE").unwrap_or_else(|_| "auto".to_string());
    if mode != "sqlite" {
        match crate::node_store::NodeStore::in_memory() {
            Ok(s) => return Box::new(s),
            Err(e) => {
                if mode == "node" {
                    panic!("SWAGCOD_STORE=node, но sidecar не поднялся: {e}");
                }
                eprintln!("store: sidecar недоступен, in-memory на встроенном SQLite: {e}");
            }
        }
    }
    Box::new(SqliteStore::in_memory().expect("in-memory SQLite открывается"))
}

impl Store for SqliteStore {
    fn create_session(&self, s: &Session) -> StoreResult<()> {
        self.conn.execute(
            "INSERT OR IGNORE INTO sessions(id, cwd, model, title, approval_policy, created_ms) VALUES (?1, ?2, ?3, ?4, ?5, ?6)",
            params![
                s.id.as_str(),
                s.cwd,
                s.model,
                s.title,
                s.approval_policy.map(policy_to_str),
                s.created_ms as i64
            ],
        )?;
        Ok(())
    }

    fn set_session_title(&self, id: &str, title: &str) -> StoreResult<()> {
        self.conn
            .execute("UPDATE sessions SET title = ?2 WHERE id = ?1", params![id, title])?;
        Ok(())
    }

    fn set_session_summary(&self, id: &str, summary: &str) -> StoreResult<()> {
        self.conn.execute(
            "UPDATE sessions SET summary = ?2 WHERE id = ?1",
            params![id, summary],
        )?;
        Ok(())
    }

    fn delete_session(&self, id: &str) -> StoreResult<()> {
        // Каскад удаляет ходы и сообщения следом за сессией.
        self.conn
            .execute("DELETE FROM sessions WHERE id = ?1", params![id])?;
        Ok(())
    }

    fn load_all(&self) -> StoreResult<Vec<Session>> {
        /// Строка выборки сессий: (id, cwd, model, title, summary, policy, created_ms).
        type SessionRow = (String, String, String, String, String, Option<String>, u64);
        let mut stmt = self.conn.prepare(
            "SELECT id, cwd, model, title, summary, approval_policy, created_ms FROM sessions ORDER BY created_ms, id",
        )?;
        let ids: Vec<SessionRow> = stmt
            .query_map([], |r| {
                Ok((
                    r.get(0)?,
                    r.get(1)?,
                    r.get(2)?,
                    r.get(3)?,
                    r.get(4)?,
                    r.get(5)?,
                    r.get::<_, i64>(6)? as u64,
                ))
            })?
            .collect::<Result<_, _>>()?;
        let mut out = Vec::with_capacity(ids.len());
        for (id, cwd, model, title, summary, policy, created_ms) in ids {
            let mut ses = Session::new(SessionId::new(&id), cwd, model);
            ses.title = title;
            ses.summary = summary;
            ses.approval_policy = policy_from_str_opt(policy);
            ses.created_ms = created_ms;
            ses.status = SessionStatus::Idle;
            fill_session(&self.conn, &mut ses)?;
            out.push(ses);
        }
        Ok(out)
    }

    fn load_session(&self, id: &str) -> StoreResult<Option<Session>> {
        let row = self.conn.query_row(
            "SELECT cwd, model, title, summary, approval_policy, created_ms FROM sessions WHERE id = ?1",
            params![id],
            |r| {
                Ok((
                    r.get::<_, String>(0)?,
                    r.get::<_, String>(1)?,
                    r.get::<_, String>(2)?,
                    r.get::<_, String>(3)?,
                    r.get::<_, Option<String>>(4)?,
                    r.get::<_, i64>(5)? as u64,
                ))
            },
        );
        let (cwd, model, title, summary, policy, created_ms) = match row {
            Ok(v) => v,
            Err(rusqlite::Error::QueryReturnedNoRows) => return Ok(None),
            Err(e) => return Err(e.into()),
        };
        let mut ses = Session::new(SessionId::new(id), cwd, model);
        ses.title = title;
        ses.summary = summary;
        ses.approval_policy = policy_from_str_opt(policy);
        ses.created_ms = created_ms;
        ses.status = SessionStatus::Idle;
        fill_session(&self.conn, &mut ses)?;
        Ok(Some(ses))
    }

    fn save_turn(&self, session_id: &str, turn: &TurnRecord, history: &[ChatMessage]) -> StoreResult<()> {
        let tx = self.conn.unchecked_transaction()?;
        let tools_json = serde_json::to_string(&turn.tool_calls)?;
        tx.execute(
            "INSERT OR REPLACE INTO turns(id, session_id, started_ms, ended_ms, ok, failure, content, reasoning, tool_calls_json, est_in, est_out)
             VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11)",
            params![
                turn.id.as_str(),
                session_id,
                turn.started_ms as i64,
                turn.ended_ms.map(|v| v as i64),
                turn.ok,
                turn.failure,
                turn.content,
                turn.reasoning,
                tools_json,
                turn.est_input_tokens as i64,
                turn.est_output_tokens as i64,
            ],
        )?;
        // Снимок истории целиком: порядок и содержимое восстанавливаются
        // байт-в-байт, без учёта инкрементальных хвостов.
        tx.execute("DELETE FROM messages WHERE session_id = ?1", params![session_id])?;
        {
            let mut stmt = tx.prepare(
                "INSERT INTO messages(session_id, ord, role, content, reasoning, tool_call_id, tool_calls_json)
                 VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7)",
            )?;
            for (ord, m) in history.iter().enumerate() {
                let tools = serde_json::to_string(&m.tool_calls)?;
                stmt.execute(params![
                    session_id,
                    ord as i64,
                    role_to_str(m.role),
                    m.content,
                    m.reasoning,
                    m.tool_call_id,
                    tools,
                ])?;
            }
        }
        tx.commit()?;
        Ok(())
    }

    fn get_pref(&self, key: &str) -> StoreResult<Option<String>> {
        let row = self
            .conn
            .query_row("SELECT value FROM prefs WHERE key = ?1", params![key], |r| {
                r.get::<_, String>(0)
            });
        match row {
            Ok(v) => Ok(Some(v)),
            Err(rusqlite::Error::QueryReturnedNoRows) => Ok(None),
            Err(e) => Err(e.into()),
        }
    }

    fn set_pref(&self, key: &str, value: &str) -> StoreResult<()> {
        self.conn.execute(
            "INSERT INTO prefs(key, value) VALUES (?1, ?2) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
            params![key, value],
        )?;
        Ok(())
    }

    fn set_session_policy(&self, id: &str, policy: Option<&str>) -> StoreResult<()> {
        self.conn.execute(
            "UPDATE sessions SET approval_policy = ?2 WHERE id = ?1",
            params![id, policy],
        )?;
        Ok(())
    }

    fn log_approval(&self, e: &ApprovalEntry) -> StoreResult<()> {
        self.conn.execute(
            "INSERT INTO approvals(session_id, turn_id, call_id, tool, summary, decision, actor, decided_ms)
             VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8)",
            params![
                e.session_id,
                e.turn_id,
                e.call_id,
                e.tool,
                e.summary,
                e.decision,
                e.actor,
                e.decided_ms as i64
            ],
        )?;
        Ok(())
    }

    fn approval_log(&self, session_id: Option<&str>, limit: usize) -> StoreResult<Vec<ApprovalEntry>> {
        let limit = limit.clamp(1, 5000) as i64;
        // Два запроса намеренно: у варианта без фильтра LIMIT — ?1,
        // иначе rusqlite считает дырку в нумерации недостающим параметром.
        let map_row = |r: &rusqlite::Row<'_>| -> rusqlite::Result<ApprovalEntry> {
            Ok(ApprovalEntry {
                session_id: r.get(0)?,
                turn_id: r.get(1)?,
                call_id: r.get(2)?,
                tool: r.get(3)?,
                summary: r.get(4)?,
                decision: r.get(5)?,
                actor: r.get(6)?,
                decided_ms: r.get::<_, i64>(7)? as u64,
            })
        };
        let select = "SELECT session_id, turn_id, call_id, tool, summary, decision, actor, decided_ms FROM approvals";
        let rows = match session_id {
            Some(sid) => {
                let sql = format!(
                    "{select} WHERE session_id = ?1 ORDER BY decided_ms DESC, id DESC LIMIT ?2"
                );
                let mut stmt = self.conn.prepare(&sql)?;
                let mapped = stmt.query_map(params![sid, limit], map_row)?;
                mapped.collect::<Result<Vec<_>, _>>()?
            }
            None => {
                let sql = format!("{select} ORDER BY decided_ms DESC, id DESC LIMIT ?1");
                let mut stmt = self.conn.prepare(&sql)?;
                let mapped = stmt.query_map(params![limit], map_row)?;
                mapped.collect::<Result<Vec<_>, _>>()?
            }
        };
        Ok(rows)
    }

    fn task_enqueue(&self, t: &crate::tasks::Task) -> StoreResult<()> {
        self.conn.execute(
            "INSERT OR IGNORE INTO tasks(id, kind, payload, state, attempts, next_try_ms, last_error, every_ms, created_ms, updated_ms)
             VALUES (?1, ?2, ?3, 'queued', 0, ?4, '', ?5, ?6, ?6)",
            params![
                t.id,
                t.kind,
                t.payload.to_string(),
                t.next_try_ms as i64,
                t.every_ms.map(|v| v as i64),
                t.created_ms as i64
            ],
        )?;
        Ok(())
    }

    fn task_claim_due(&self, now_ms: u64) -> StoreResult<Vec<crate::tasks::Task>> {
        let ids: Vec<String> = {
            let mut stmt = self.conn.prepare(
                "SELECT id FROM tasks WHERE state = 'queued' AND next_try_ms <= ?1 ORDER BY next_try_ms, id LIMIT 8",
            )?;
            let mapped = stmt.query_map(params![now_ms as i64], |r| r.get::<_, String>(0))?;
            mapped.collect::<Result<Vec<_>, _>>()?
        };
        let mut out = Vec::new();
        for id in ids {
            // Guard state='queued': единственный воркер, но защита от
            // двойного claim дешевле расследования.
            self.conn.execute(
                "UPDATE tasks SET state = 'running', attempts = attempts + 1,
                 updated_ms = CAST(strftime('%s','now') AS INTEGER) * 1000
                 WHERE id = ?1 AND state = 'queued'",
                params![id],
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
        let sql = format!("SELECT {TASK_COLS} FROM tasks WHERE id = ?1");
        let mut stmt = self.conn.prepare(&sql)?;
        let mut mapped = stmt.query_map(params![id], task_from_row)?;
        Ok(mapped.next().transpose()?)
    }

    fn task_update(
        &self,
        id: &str,
        state: &str,
        message: &str,
        attempts: u32,
        next_try_ms: u64,
    ) -> StoreResult<()> {
        self.conn.execute(
            "UPDATE tasks SET state = ?2, last_error = ?3, attempts = ?4, next_try_ms = ?5,
             updated_ms = CAST(strftime('%s','now') AS INTEGER) * 1000
             WHERE id = ?1",
            params![id, state, message, attempts as i64, next_try_ms as i64],
        )?;
        Ok(())
    }

    fn tasks_list(&self, limit: usize) -> StoreResult<Vec<crate::tasks::Task>> {
        let limit = limit.clamp(1, 1000) as i64;
        let sql = format!("SELECT {TASK_COLS} FROM tasks ORDER BY created_ms DESC, id DESC LIMIT ?1");
        let mut stmt = self.conn.prepare(&sql)?;
        let mapped = stmt.query_map(params![limit], task_from_row)?;
        Ok(mapped.collect::<Result<Vec<_>, _>>()?)
    }

    fn tasks_recover_running(&self) -> StoreResult<()> {
        self.conn.execute(
            "UPDATE tasks SET state = 'queued', next_try_ms = 0,
             updated_ms = CAST(strftime('%s','now') AS INTEGER) * 1000
             WHERE state = 'running'",
            [],
        )?;
        Ok(())
    }

    fn tasks_prune(&self, before_ms: u64) -> StoreResult<()> {
        // u64::MAX as i64 дал бы -1: зажимаем до i64::MAX («удалить всё»).
        let before = before_ms.min(i64::MAX as u64) as i64;
        self.conn.execute(
            "DELETE FROM tasks WHERE state = 'done' AND updated_ms < ?1",
            params![before],
        )?;
        Ok(())
    }
}

/// E-5: колонки tasks — порядок общий для rusqlite и NodeStore.
pub(crate) const TASK_COLS: &str =
    "id, kind, payload, state, attempts, next_try_ms, last_error, every_ms, created_ms, updated_ms";

fn task_from_row(r: &rusqlite::Row<'_>) -> rusqlite::Result<crate::tasks::Task> {
    let payload: String = r.get(2)?;
    let every: Option<i64> = r.get(7)?;
    Ok(crate::tasks::Task {
        id: r.get(0)?,
        kind: r.get(1)?,
        payload: serde_json::from_str(&payload).unwrap_or_else(|_| serde_json::json!({})),
        state: r.get(3)?,
        attempts: r.get::<_, i64>(4)?.max(0) as u32,
        next_try_ms: r.get::<_, i64>(5)?.max(0) as u64,
        last_error: r.get(6)?,
        every_ms: every.map(|v| v.max(0) as u64),
        created_ms: r.get::<_, i64>(8)?.max(0) as u64,
        updated_ms: r.get::<_, i64>(9)?.max(0) as u64,
    })
}

/// Ходы и сообщения сессии из базы в живую структуру.
fn fill_session(conn: &Connection, ses: &mut Session) -> StoreResult<()> {
    let mut tstmt = conn.prepare(
        "SELECT id, started_ms, ended_ms, ok, failure, content, reasoning, tool_calls_json, est_in, est_out
         FROM turns WHERE session_id = ?1 ORDER BY started_ms, id",
    )?;
    let turns = tstmt
        .query_map(params![ses.id.as_str()], |r| {
            let tools_json: String = r.get(7)?;
            Ok(TurnRecord {
                id: crate::session::TurnId::new(r.get::<_, String>(0)?),
                started_ms: r.get::<_, i64>(1)? as u64,
                ended_ms: r.get::<_, Option<i64>>(2)?.map(|v| v as u64),
                ok: r.get(3)?,
                failure: r.get(4)?,
                content: r.get(5)?,
                reasoning: r.get(6)?,
                tool_calls: serde_json::from_str(&tools_json).unwrap_or_default(),
                est_input_tokens: r.get::<_, i64>(8)? as u32,
                est_output_tokens: r.get::<_, i64>(9)? as u32,
            })
        })?
        .collect::<Result<Vec<_>, _>>()?;
    ses.turns = turns;

    let mut mstmt = conn.prepare(
        "SELECT role, content, reasoning, tool_call_id, tool_calls_json
         FROM messages WHERE session_id = ?1 ORDER BY ord, id",
    )?;
    let history = mstmt
        .query_map(params![ses.id.as_str()], |r| {
            Ok((
                r.get::<_, String>(0)?,
                r.get::<_, String>(1)?,
                r.get::<_, String>(2)?,
                r.get::<_, Option<String>>(3)?,
                r.get::<_, String>(4)?,
            ))
        })?
        .map(|row| {
            let (role, content, reasoning, tool_call_id, tools_json) = row?;
            let tool_calls: Vec<ToolCall> = serde_json::from_str(&tools_json)?;
            Ok::<_, StoreError>(ChatMessage {
                role: role_from_str(&role),
                content,
                reasoning,
                tool_calls,
                tool_call_id,
            })
        })
        .collect::<Result<Vec<_>, _>>()?;
    ses.history = history;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::session::TurnId;

    fn sample_session() -> Session {
        let mut ses = Session::new(SessionId::new("s-1"), "D:/proj", "test-model");
        ses.title = "проба".to_string();
        ses.push_user_message("привет");
        ses.push_assistant_message(ChatMessage {
            role: Role::Assistant,
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
    fn roundtrip_history_is_byte_equal() {
        let store = SqliteStore::in_memory().unwrap();
        let ses = sample_session();
        store.create_session(&ses).unwrap();
        let turn = sample_turn(1);
        store.save_turn(ses.id.as_str(), &turn, &ses.history).unwrap();

        let loaded = store.load_session("s-1").unwrap().expect("сессия есть");
        assert_eq!(serde_json::to_string(&loaded.history).unwrap(), serde_json::to_string(&ses.history).unwrap());
        assert_eq!(loaded.turns.len(), 1);
        assert_eq!(loaded.turns[0], turn);
        assert_eq!(loaded.title, "проба");
        assert_eq!(loaded.cwd, "D:/proj");
    }

    #[test]
    fn load_all_keeps_creation_order_and_delete_cascades() {
        let store = SqliteStore::in_memory().unwrap();
        let a = sample_session();
        let mut b = Session::new(SessionId::new("s-2"), "D:/other", "test-model");
        b.created_ms = a.created_ms + 5;
        store.create_session(&a).unwrap();
        store.create_session(&b).unwrap();
        store.save_turn(a.id.as_str(), &sample_turn(1), &a.history).unwrap();

        let all = store.load_all().unwrap();
        assert_eq!(all.len(), 2);
        assert_eq!(all[0].id.as_str(), "s-1");
        assert_eq!(all[1].id.as_str(), "s-2");

        store.delete_session("s-1").unwrap();
        let all = store.load_all().unwrap();
        assert_eq!(all.len(), 1);
        assert_eq!(all[0].id.as_str(), "s-2");
        // Каскад: ходов и сообщений удалённой сессии в базе не осталось.
        let turns: i64 = store
            .conn
            .query_row("SELECT COUNT(*) FROM turns WHERE session_id = 's-1'", [], |r| r.get(0))
            .unwrap();
        let msgs: i64 = store
            .conn
            .query_row("SELECT COUNT(*) FROM messages WHERE session_id = 's-1'", [], |r| r.get(0))
            .unwrap();
        assert_eq!(turns, 0);
        assert_eq!(msgs, 0);
    }

    #[test]
    fn prefs_set_get_overwrite() {
        let store = SqliteStore::in_memory().unwrap();
        assert_eq!(store.get_pref("theme").unwrap(), None);
        store.set_pref("theme", "dark").unwrap();
        store.set_pref("theme", "matrix").unwrap();
        assert_eq!(store.get_pref("theme").unwrap().as_deref(), Some("matrix"));
    }

    #[test]
    fn crash_between_writes_keeps_db_intact() {
        // База в файле: первый владелец роняет соединение без закрытия
        // (имитация обрыва процесса), WAL обязана пережить это.
        let dir = std::env::temp_dir().join(format!("swagcod-store-test-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("crash.db");
        let _ = std::fs::remove_file(&path);
        {
            let store = SqliteStore::open(&path).unwrap();
            let ses = sample_session();
            store.create_session(&ses).unwrap();
            store.save_turn(ses.id.as_str(), &sample_turn(1), &ses.history).unwrap();
            // Намеренно не закрываем: соединение уходит без drop-семантики
            // закрытия, как при kill процесса.
            std::mem::forget(store);
        }
        let store = SqliteStore::open(&path).unwrap();
        assert_eq!(store.integrity_check().unwrap(), "ok");
        let loaded = store.load_session("s-1").unwrap().expect("сессия пережила обрыв");
        assert_eq!(loaded.turns.len(), 1);
        assert_eq!(loaded.history.len(), 3);
        drop(store);
        let _ = std::fs::remove_dir_all(&dir);
    }

    // ---- B-7: журнал подтверждений и per-session политика ----

    fn entry(call: &str, decision: &str, actor: &str, ms: u64) -> ApprovalEntry {
        ApprovalEntry {
            session_id: "s-1".into(),
            turn_id: "t-1".into(),
            call_id: call.into(),
            tool: "bash".into(),
            summary: "rm -rf build".into(),
            decision: decision.into(),
            actor: actor.into(),
            decided_ms: ms,
        }
    }

    #[test]
    fn approval_journal_roundtrip() {
        let store = SqliteStore::in_memory().unwrap();
        let ses = sample_session();
        store.create_session(&ses).unwrap();
        store.log_approval(&entry("c1", "approved", "user", 100)).unwrap();
        store.log_approval(&entry("c2", "denied", "user", 200)).unwrap();
        store.log_approval(&entry("c3", "denied", "system", 300)).unwrap();

        let all = store.approval_log(None, 10).unwrap();
        assert_eq!(all.len(), 3);
        // Свежайшие первыми: журнал читается как лента.
        assert_eq!(all[0].call_id, "c3");
        assert_eq!(all[0].actor, "system");
        assert_eq!(all[2].call_id, "c1");
        assert_eq!(all[2].decision, "approved");

        let limited = store.approval_log(Some("s-1"), 2).unwrap();
        assert_eq!(limited.len(), 2);
        assert!(store.approval_log(Some("s-2"), 10).unwrap().is_empty());
    }

    #[test]
    fn approval_journal_survives_session_delete_in_file_db() {
        // Журнал автономный (без FK на turns) намеренно: аудит переживает
        // удаление сессии из списка — но по session_id остаётся фильтруемым.
        let dir = std::env::temp_dir().join(format!("swagcod-approvals-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("a.db");
        let _ = std::fs::remove_file(&path);
        {
            let store = SqliteStore::open(&path).unwrap();
            let ses = sample_session();
            store.create_session(&ses).unwrap();
            store.log_approval(&entry("c1", "approved", "user", 100)).unwrap();
            store.delete_session("s-1").unwrap();
            let all = store.approval_log(None, 10).unwrap();
            assert_eq!(all.len(), 1, "аудит не исчезает вместе с сессией");
        }
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn session_policy_persists_and_clears() {
        use crate::turn::ApprovalPolicy;
        let store = SqliteStore::in_memory().unwrap();
        let ses = sample_session();
        store.create_session(&ses).unwrap();
        // По умолчанию политика не задана: наследуется глобальная.
        let loaded = store.load_session("s-1").unwrap().unwrap();
        assert_eq!(loaded.approval_policy, None);

        store.set_session_policy("s-1", Some("always")).unwrap();
        let loaded = store.load_session("s-1").unwrap().unwrap();
        assert_eq!(loaded.approval_policy, Some(ApprovalPolicy::Always));

        // load_all видит то же, что load_session.
        let all = store.load_all().unwrap();
        assert_eq!(all[0].approval_policy, Some(ApprovalPolicy::Always));

        store.set_session_policy("s-1", None).unwrap();
        let loaded = store.load_session("s-1").unwrap().unwrap();
        assert_eq!(loaded.approval_policy, None);

        // Мусорная строка не становится политикой.
        store.set_session_policy("s-1", Some("yolo")).unwrap();
        let loaded = store.load_session("s-1").unwrap().unwrap();
        assert_eq!(loaded.approval_policy, None);
    }

    #[test]
    fn create_session_persists_policy() {
        use crate::turn::ApprovalPolicy;
        let store = SqliteStore::in_memory().unwrap();
        let mut ses = sample_session();
        ses.approval_policy = Some(ApprovalPolicy::OnDangerous);
        store.create_session(&ses).unwrap();
        let loaded = store.load_session("s-1").unwrap().unwrap();
        assert_eq!(loaded.approval_policy, Some(ApprovalPolicy::OnDangerous));
    }
}
