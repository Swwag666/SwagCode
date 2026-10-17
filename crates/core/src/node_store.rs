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
    fts_match_expr, memory_check_scope, memory_check_text, policy_from_str_opt, policy_to_str,
    role_from_str, role_to_str, search_limit, ApprovalEntry, MemoryEntry, MessageHit, Store,
    StoreError, StoreResult, FTS_NEEDS_BACKFILL_SQL, FTS_REBUILD_SQL, SEARCH_MESSAGES_SQL,
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
        #[cfg(windows)]
        use std::os::windows::process::CommandExt;
        let script = sidecar_script()?;
        let node = node_binary();
        let mut cmd = Command::new(&node);
        // Без консольного окна: sidecar висит весь сеанс, мигать CMD
        // при каждом старте приложения незачем.
        #[cfg(windows)]
        cmd.creation_flags(0x0800_0000); // CREATE_NO_WINDOW
        let mut child = cmd
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
        /* F-10: схема создаёт индекс FTS5, но история в базе могла появиться
        раньше него — тогда индекс дозируется один раз. Тот же критерий, что
        в SqliteStore::fts_backfill: пусто в индексе при непустой messages. */
        store.fts_backfill()?;
        Ok(store)
    }

    /// F-10: разовая пересборка FTS-индекса из `messages`.
    ///
    /// Условие — [`FTS_NEEDS_BACKFILL_SQL`] (два LIMIT 1, а не COUNT(*)),
    /// поэтому повторный запуск на живой базе стоит двух коротких запросов и
    /// не ест бюджет старта (D-013).
    fn fts_backfill(&self) -> StoreResult<()> {
        let rows = self.query(FTS_NEEDS_BACKFILL_SQL, vec![])?;
        let needed = rows.first().map(|r| v_int(col(r, 0))).unwrap_or(0) != 0;
        if needed {
            self.exec(FTS_REBUILD_SQL, vec![])?;
        }
        Ok(())
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
            return Err(StoreError::Sidecar(
                "sidecar закрыл канал (процесс умер?)".into(),
            ));
        }
        let resp: Value = serde_json::from_str(line.trim())?;
        if resp["ok"] != json!(true) {
            return Err(StoreError::Sidecar(
                resp["error"]
                    .as_str()
                    .unwrap_or("неизвестная ошибка")
                    .to_string(),
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

/// E-4: каталоги sidecar-расклада в порядке предпочтения.
const SIDECAR_CANDIDATES: &[&str] = &["sidecar", "vendor/sidecar", "vendor\\sidecar"];

/// Все существующие кандидаты `dir/<расклад>/<name>`: рядом с exe (бандл),
/// затем от cwd и вверх по предкам (разработка, тесты cargo).
fn sidecar_candidates(name: &str) -> Vec<PathBuf> {
    let mut out = Vec::new();
    if let Ok(exe) = std::env::current_exe() {
        if let Some(dir) = exe.parent() {
            for rel in SIDECAR_CANDIDATES {
                let p = dir.join(rel).join(name);
                if p.is_file() {
                    out.push(p);
                }
            }
        }
    }
    if let Ok(cwd) = std::env::current_dir() {
        let mut dir = cwd.as_path();
        for _ in 0..6 {
            for rel in SIDECAR_CANDIDATES {
                let p = dir.join(rel).join(name);
                if p.is_file() {
                    out.push(p);
                }
            }
            match dir.parent() {
                Some(parent) => dir = parent,
                None => break,
            }
        }
    }
    out
}

/// Расклад способен поднять better-sqlite3: рядом со скриптом лежит
/// собранный нативный бинарник. `require()` в Node резолвится ОТ ПУТИ
/// СКРИПТА, а не от cwd процесса, поэтому «store-server.js найден» ещё не
/// значит «стор поднимется»: `pnpm install --ignore-scripts` (CI)
/// оставляет в `sidecar/node_modules` симлинк пакета БЕЗ бинарника, а
/// prebuilt качает только `tools/bundle-sidecar.ps1` и только в
/// `vendor/sidecar`.
fn layout_has_sqlite(script: &Path) -> bool {
    script
        .parent()
        .is_some_and(|dir| sqlite_binding(dir).is_file())
}

/// Расклад ещё и НЕ УСТАРЕЛ: его копия скрипта не старше исходника в
/// `sidecar/` того же дерева. `vendor/sidecar` собирает скрипт, и в рабочем
/// дереве разработчика он легко оказывается вчерашним; выбрать его значило
/// бы запустить старую схему стора (и сравнить в тесте схемы не то с не
/// тем). В бандле рядом с exe исходника нет вовсе — там сравнивать не с
/// чем, расклад пригоден по одному бинарнику.
fn layout_is_viable(script: &Path) -> bool {
    if !layout_has_sqlite(script) {
        return false;
    }
    let Some(source) = repo_sidecar_source(script) else {
        return true;
    };
    match (mtime(&source), mtime(script)) {
        (Some(src), Some(dst)) => dst >= src,
        // Время не читается — не повод браковать расклад: бинарник на
        // месте, а расхождение схемы поймает тест `schema_matches_js_sidecar`.
        _ => true,
    }
}

/// Исходник того же скрипта в `sidecar/` репозитория: от каталога расклада
/// вверх по предкам (`vendor/sidecar/x.js` → `vendor` → корень). Для копии
/// в бандле рядом с exe такого файла нет — None.
fn repo_sidecar_source(script: &Path) -> Option<PathBuf> {
    let name = script.file_name()?;
    let mut dir = script.parent()?;
    for _ in 0..6 {
        let candidate = dir.join("sidecar").join(name);
        if candidate != script && candidate.is_file() {
            return Some(candidate);
        }
        dir = dir.parent()?;
    }
    None
}

/// Время последней записи; None, если метаданные не читаются.
fn mtime(path: &Path) -> Option<std::time::SystemTime> {
    std::fs::metadata(path).and_then(|m| m.modified()).ok()
}

/// Путь нативного бинарника better-sqlite3 в раскладе `dir`.
fn sqlite_binding(dir: &Path) -> PathBuf {
    dir.join("node_modules")
        .join("better-sqlite3")
        .join("build")
        .join("Release")
        .join("better_sqlite3.node")
}

/// Выбор расклада из кандидатов: первый пригодный (бинарник на месте и
/// копия не старше исходника). Чистая функция — порядок и критерии
/// тестируются на списках путей.
fn pick_viable_layout(candidates: &[PathBuf]) -> Option<PathBuf> {
    candidates.iter().find(|p| layout_is_viable(p)).cloned()
}

/// E-4: найти любой файл sidecar-расклада (store-server.js,
/// plugin-server.js) по тем же кандидатам: рядом с exe (бандл) → vendor
/// рядом с exe → от cwd и вверх по предкам, включая vendor.
///
/// Непригодный расклад (нет нативного бинарника better-sqlite3 либо копия
/// скрипта устарела) отбрасывается, если есть пригодный: иначе скрипт из
/// репозитория затенял собранный `vendor/sidecar`, а `require` падал уже
/// внутри Node, где Rust видит только «закрыл stdout» — стор тихо падал в
/// rusqlite, а prefs-мост плагинов возвращал null.
pub fn sidecar_file(name: &str) -> Option<PathBuf> {
    let candidates = sidecar_candidates(name);
    pick_viable_layout(&candidates)
        .or_else(|| candidates.iter().find(|p| layout_has_sqlite(p)).cloned())
        .or_else(|| candidates.into_iter().next())
}

/// То же, но расклад ОБЯЗАН уметь поднять better-sqlite3. Для стора это
/// не «предпочтение», а условие работы: скрипт без бинарника отвечает
/// ошибкой `require`, а не данными. Свежесть копии здесь не проверяется:
/// устаревший, но рабочий бандл лучше честного отказа (схему сверяет тест).
pub fn sidecar_sqlite_file(name: &str) -> Option<PathBuf> {
    let candidates = sidecar_candidates(name);
    pick_viable_layout(&candidates)
        .or_else(|| candidates.into_iter().find(|p| layout_has_sqlite(p)))
}

/// Где искать скрипт sidecar'а: env → рядом с exe (бандл) → vendor рядом
/// с exe → от cwd и вверх по предкам, включая vendor (тесты cargo стартуют
/// из каталога крейта, разработка — из воркспейса).
fn sidecar_script() -> StoreResult<PathBuf> {
    if let Ok(p) = std::env::var("SWAGCOD_STORE_SIDECAR") {
        return Ok(PathBuf::from(p));
    }
    sidecar_sqlite_file("store-server.js").ok_or_else(|| match sidecar_file("store-server.js") {
        // Скрипт есть, а бинарника в его раскладе нет: stor поднялся бы и
        // умер на require внутри Node, где Rust видит только «закрыл
        // stdout». Называем настоящую причину и что с ней делать.
        Some(p) => StoreError::Sidecar(format!(
            "store-server.js найден ({}), но в его раскладе нет \
             node_modules/better-sqlite3/build/Release/better_sqlite3.node: соберите \
             vendor (tools/bundle-sidecar.ps1) или поставьте зависимости без --ignore-scripts",
            p.display()
        )),
        None => StoreError::Sidecar(
            "sidecar/store-server.js не найден (env SWAGCOD_STORE_SIDECAR, каталог exe, vendor или cwd)"
                .into(),
        ),
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

    fn save_turn(
        &self,
        session_id: &str,
        turn: &TurnRecord,
        history: &[ChatMessage],
    ) -> StoreResult<()> {
        let tools_json = serde_json::to_string(&turn.tool_calls)?;
        /* Ход и снимок истории — одна транзакция (batch на сервере):
        оборванный процесс не оставляет полупустой ход. */
        let mut steps = Vec::with_capacity(2 + history.len());
        steps.push(json!({
            "sql": "INSERT OR REPLACE INTO turns(id, session_id, started_ms, ended_ms, ok, failure, content, reasoning, tool_calls_json, est_in, est_out, parent_turn_id, checkpoint_sha)
                    VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12, ?13)",
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
                json!(turn.est_output_tokens as i64),
                turn.parent_turn_id.as_ref().map(|p| json!(p.as_str())).unwrap_or(Value::Null),
                turn.checkpoint_sha.as_deref().map(|s| json!(s)).unwrap_or(Value::Null)
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

    fn approval_log(
        &self,
        session_id: Option<&str>,
        limit: usize,
    ) -> StoreResult<Vec<ApprovalEntry>> {
        let limit = limit.clamp(1, 5000) as i64;
        let select = "SELECT session_id, turn_id, call_id, tool, summary, decision, actor, decided_ms FROM approvals";
        let rows = match session_id {
            Some(sid) => self.query(
                &format!(
                    "{select} WHERE session_id = ?1 ORDER BY decided_ms DESC, id DESC LIMIT ?2"
                ),
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

    fn memory_add(&self, scope: &str, session_id: Option<&str>, text: &str) -> StoreResult<i64> {
        let scope = memory_check_scope(scope)?;
        let text = memory_check_text(text)?;
        let now = crate::bus::now_ms() as i64;
        /* insertId отдаёт sidecar (better-sqlite3 lastInsertRowid): id нужен
        инструментам и UI, чтобы забыть конкретную строку. */
        let resp = self.exec(
            "INSERT INTO memory(scope, text, created_ms, session_id) VALUES (?1, ?2, ?3, ?4)",
            vec![
                json!(scope),
                json!(text),
                json!(now),
                session_id.map(|s| json!(s)).unwrap_or(Value::Null),
            ],
        )?;
        Ok(resp["insertId"].as_i64().unwrap_or(0))
    }

    fn memory_list(
        &self,
        scope: &str,
        session_id: Option<&str>,
        limit: usize,
    ) -> StoreResult<Vec<MemoryEntry>> {
        let scope = memory_check_scope(scope)?;
        let limit = limit.clamp(1, 1000) as i64;
        let select = "SELECT id, scope, text, created_ms, session_id FROM memory WHERE scope = ?1";
        let rows = match session_id {
            Some(sid) => self.query(
                &format!("{select} AND session_id = ?2 ORDER BY created_ms DESC, id DESC LIMIT ?3"),
                vec![json!(scope), json!(sid), json!(limit)],
            )?,
            None => self.query(
                &format!("{select} ORDER BY created_ms DESC, id DESC LIMIT ?2"),
                vec![json!(scope), json!(limit)],
            )?,
        };
        Ok(rows
            .iter()
            .map(|r| MemoryEntry {
                id: v_int(col(r, 0)),
                scope: v_text(col(r, 1)),
                text: v_text(col(r, 2)),
                created_ms: v_int(col(r, 3)) as u64,
                session_id: v_opt_text(col(r, 4)),
            })
            .collect())
    }

    fn memory_forget(&self, id: i64) -> StoreResult<bool> {
        let resp = self.exec("DELETE FROM memory WHERE id = ?1", vec![json!(id)])?;
        Ok(resp["changes"].as_i64().unwrap_or(0) > 0)
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
        let sql = format!(
            "SELECT {} FROM tasks WHERE id = ?1",
            crate::store::TASK_COLS
        );
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

    fn metrics_insert(&self, ts_ms: u64, name: &str, value: f64) -> StoreResult<()> {
        self.exec(
            "INSERT INTO metrics(ts_ms, name, value) VALUES(?1, ?2, ?3)",
            vec![
                json!(ts_ms.min(i64::MAX as u64) as i64),
                json!(name),
                json!(value),
            ],
        )?;
        Ok(())
    }

    fn metrics_query(
        &self,
        name: &str,
        since_ms: u64,
        limit: usize,
    ) -> StoreResult<Vec<(u64, f64)>> {
        let limit = limit.clamp(1, 10_000) as i64;
        let rows = self.query(
            "SELECT ts_ms, value FROM metrics WHERE name = ?1 AND ts_ms >= ?2 ORDER BY ts_ms LIMIT ?3",
            vec![json!(name), json!(since_ms.min(i64::MAX as u64) as i64), json!(limit)],
        )?;
        Ok(rows
            .iter()
            .map(|r| {
                (
                    v_int(col(r, 0)).max(0) as u64,
                    col(r, 1).as_f64().unwrap_or(0.0),
                )
            })
            .collect())
    }

    fn metrics_prune(&self, before_ms: u64) -> StoreResult<()> {
        let before = before_ms.min(i64::MAX as u64) as i64;
        self.exec("DELETE FROM metrics WHERE ts_ms < ?1", vec![json!(before)])?;
        Ok(())
    }

    fn tokens_by_day(&self, since_ms: u64) -> StoreResult<Vec<(u64, u64, u64)>> {
        let rows = self.query(
            "SELECT (started_ms / 86400000) * 86400000 AS day,
                    SUM(est_in + est_out) AS tokens, COUNT(*) AS turns
             FROM turns WHERE started_ms >= ?1 AND (est_in + est_out) > 0
             GROUP BY day ORDER BY day",
            vec![json!(since_ms.min(i64::MAX as u64) as i64)],
        )?;
        Ok(rows
            .iter()
            .map(|r| {
                (
                    v_int(col(r, 0)).max(0) as u64,
                    v_int(col(r, 1)).max(0) as u64,
                    v_int(col(r, 2)).max(0) as u64,
                )
            })
            .collect())
    }

    fn search_messages(&self, query: &str, limit: usize) -> StoreResult<Vec<MessageHit>> {
        // Тот же SQL и та же проверка запроса, что в SqliteStore: выборка не
        // имеет права разъезжаться между основным хранилищем и аварийным.
        let expr = fts_match_expr(query)?;
        let lim = search_limit(limit) as i64;
        let rows = self.query(SEARCH_MESSAGES_SQL, vec![json!(expr), json!(lim)])?;
        Ok(rows
            .iter()
            .map(|r| MessageHit {
                session_id: v_text(col(r, 0)),
                session_title: v_text(col(r, 1)),
                ord: v_int(col(r, 2)).max(0) as u32,
                role: v_text(col(r, 3)),
                snippet: v_text(col(r, 4)),
                rank: col(r, 5).as_f64().unwrap_or(0.0),
            })
            .collect())
    }

    fn shutdown(&mut self) {
        // То же, что Drop: вежливый rpc, затем kill страховки ради.
        // Вызывается перед установкой обновления: работающий node.exe
        // заблокирован на запись, NSIS не смог бы его заменить.
        let _ = self.rpc(json!({"method": "shutdown"}));
        if let Ok(mut child) = self.child.lock() {
            let _ = child.kill();
            let _ = child.wait();
        }
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
            "SELECT id, started_ms, ended_ms, ok, failure, content, reasoning, tool_calls_json, est_in, est_out, parent_turn_id, checkpoint_sha
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
                parent_turn_id: v_opt_text(col(r, 10)).map(crate::session::TurnId::new),
                checkpoint_sha: v_opt_text(col(r, 11)),
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
            parent_turn_id: None,
            checkpoint_sha: None,
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
        assert_eq!(
            loaded.approval_policy,
            Some(crate::turn::ApprovalPolicy::Never)
        );
        assert_eq!(loaded.turns.len(), 1);
        assert_eq!(loaded.turns[0].content, "ответ 1");
        assert!(loaded.turns[0].ok);
        assert_eq!(loaded.history.len(), ses.history.len());
        // Инструментальный хвост восстановился байт-в-байт.
        assert_eq!(loaded.history, ses.history);

        /* E-6: дочерний ход суб-агента — parent_turn_id переживает
        roundtrip через sidecar (паритет с rusqlite-бэкендом). */
        let mut child = sample_turn(2);
        child.id = crate::session::TurnId::new("t-1-sub-abc");
        child.parent_turn_id = Some(crate::session::TurnId::new("t-1"));
        store.save_turn("s-1", &child, &ses.history).unwrap();
        let with_child = store.load_session("s-1").unwrap().unwrap();
        assert_eq!(with_child.turns.len(), 2);
        let loaded_child = with_child
            .turns
            .iter()
            .find(|t| t.id.as_str() == "t-1-sub-abc")
            .expect("дочерний ход на месте");
        assert_eq!(
            loaded_child.parent_turn_id.as_ref().map(|p| p.as_str()),
            Some("t-1")
        );
        assert_eq!(with_child.turns[0].parent_turn_id, None);

        /* F-5: git-чекпоинт хода переживает roundtrip через sidecar —
        паритет с rusqlite-бэкендом (тот же тест в store.rs). Без него
        кнопка «Откатить» молча исчезла бы у всех, кто работает на NodeStore. */
        let sha = "b".repeat(40);
        let mut with_ckpt = sample_turn(3);
        with_ckpt.checkpoint_sha = Some(sha.clone());
        store.save_turn("s-1", &with_ckpt, &ses.history).unwrap();
        let loaded_ckpt = store.load_session("s-1").unwrap().unwrap();
        assert_eq!(
            loaded_ckpt
                .turns
                .iter()
                .find(|t| t.id.as_str() == "t-3")
                .and_then(|t| t.checkpoint_sha.clone())
                .as_deref(),
            Some(sha.as_str())
        );
        assert_eq!(
            loaded_ckpt.turns[0].checkpoint_sha, None,
            "ход без снимка — NULL, а не пустая строка"
        );

        // Политика снимается обратно в «как глобальная».
        store.set_session_policy("s-1", None).unwrap();
        let cleared = store.load_session("s-1").unwrap().unwrap();
        assert_eq!(cleared.approval_policy, None);

        assert!(store.load_session("нет-такой").unwrap().is_none());
    }

    #[test]
    fn node_metrics_and_tokens_parity() {
        // E-8: метрики и токены/день через sidecar — паритет с rusqlite.
        let Some(store) = try_memory() else { return };
        let ses = sample_session();
        store.create_session(&ses).unwrap();
        store
            .save_turn("s-1", &sample_turn(1), &ses.history)
            .unwrap();
        let mut t3 = sample_turn(3);
        t3.started_ms = 86_400_000 + 7000;
        store.save_turn("s-1", &t3, &ses.history).unwrap();
        let mut t4 = sample_turn(4);
        t4.started_ms = 86_400_000 + 8000;
        t4.est_input_tokens = 0;
        t4.est_output_tokens = 0;
        store.save_turn("s-1", &t4, &ses.history).unwrap();

        store.metrics_insert(1000, "bus_seq", 42.0).unwrap();
        store.metrics_insert(60_000, "bus_seq", 43.0).unwrap();
        store.metrics_insert(60_000, "sessions", 1.0).unwrap();
        let q = store.metrics_query("bus_seq", 0, 100).unwrap();
        assert_eq!(q, vec![(1000, 42.0), (60_000, 43.0)]);
        let q = store.metrics_query("bus_seq", 59_000, 1).unwrap();
        assert_eq!(q, vec![(60_000, 43.0)]);
        store.metrics_prune(59_000).unwrap();
        assert_eq!(
            store.metrics_query("bus_seq", 0, 100).unwrap(),
            vec![(60_000, 43.0)]
        );
        assert_eq!(
            store.metrics_query("sessions", 0, 100).unwrap(),
            vec![(60_000, 1.0)]
        );

        assert_eq!(
            store.tokens_by_day(0).unwrap(),
            vec![(0, 30, 1), (86_400_000, 30, 1)]
        );
        assert_eq!(
            store.tokens_by_day(86_400_000).unwrap(),
            vec![(86_400_000, 30, 1)]
        );
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

    /// F-7: память через sidecar ведёт себя как через rusqlite — области,
    /// порядок, forget, insertId и общая валидация.
    #[test]
    fn node_memory_roundtrip_matches_sqlite() {
        use crate::store::{MEMORY_SCOPE_GLOBAL, MEMORY_SCOPE_SESSION};
        let Some(store) = try_memory() else { return };
        let id = store
            .memory_add(MEMORY_SCOPE_GLOBAL, None, "  факт 1  ")
            .unwrap();
        assert!(id > 0, "sidecar обязан вернуть insertId");
        let id2 = store
            .memory_add(MEMORY_SCOPE_SESSION, Some("s-1"), "заметка")
            .unwrap();
        assert_ne!(id, id2);

        let globals = store.memory_list(MEMORY_SCOPE_GLOBAL, None, 50).unwrap();
        assert_eq!(globals.len(), 1, "сессионная строка не в глобальных");
        assert_eq!(globals[0].text, "факт 1", "текст пишется trimmed");
        assert_eq!(globals[0].id, id);
        assert!(globals[0].created_ms > 0);
        assert_eq!(globals[0].session_id, None);

        let sess = store
            .memory_list(MEMORY_SCOPE_SESSION, Some("s-1"), 50)
            .unwrap();
        assert_eq!(sess.len(), 1);
        assert_eq!(sess[0].session_id.as_deref(), Some("s-1"));
        assert!(store
            .memory_list(MEMORY_SCOPE_SESSION, Some("s-2"), 50)
            .unwrap()
            .is_empty());

        assert!(store.memory_forget(id2).unwrap());
        assert!(!store.memory_forget(id2).unwrap(), "повтор — false");
        // Валидация общая для обеих реализаций (memory_check_* в store.rs).
        assert!(store.memory_add(MEMORY_SCOPE_GLOBAL, None, "   ").is_err());
        assert!(store.memory_add("private", None, "x").is_err());
        assert!(store.memory_list("private", None, 10).is_err());
    }

    /* ── F-10: полнотекстовый поиск — тот же контракт, что в SqliteStore. ── */

    #[test]
    fn node_fts_search_finds_history_and_reasoning() {
        let Some(store) = try_memory() else { return };
        let ses = sample_session();
        store.create_session(&ses).unwrap();
        store
            .save_turn("s-1", &sample_turn(1), &ses.history)
            .unwrap();

        let hits = store.search_messages("привет", 10).unwrap();
        assert_eq!(hits.len(), 1, "найдено сообщение пользователя");
        assert_eq!(hits[0].session_title, "проба");
        assert_eq!(hits[0].role, "user");
        assert!(
            hits[0].snippet.contains('[') && hits[0].snippet.contains(']'),
            "сниппет подсвечивает совпадение: {}",
            hits[0].snippet
        );
        // Колонка reasoning индексируется и в sidecar-бэкенде.
        let think = store.search_messages("думаю", 10).unwrap();
        assert_eq!(think.len(), 1, "{think:?}");
        assert_eq!(think[0].role, "assistant");
        // Префикс и регистр — как в rusqlite.
        assert_eq!(store.search_messages("прив", 10).unwrap().len(), 1);
        assert_eq!(store.search_messages("ПРИВЕТ", 10).unwrap().len(), 1);
    }

    /// Пустой запрос — ошибка на обоих бэкендах. Без общего `fts_match_expr`
    /// better-sqlite3 бросил бы «fts5: syntax error», а rusqlite молча вернул
    /// ноль строк: одно и то же действие значило бы разное.
    #[test]
    fn node_fts_search_rejects_empty_and_wordless_queries() {
        let Some(store) = try_memory() else { return };
        let e = store.search_messages("", 10).unwrap_err();
        assert!(e.to_string().contains("пустой"), "{e}");
        let e = store.search_messages("*** ---", 10).unwrap_err();
        assert!(e.to_string().contains("ни одного слова"), "{e}");
    }

    #[test]
    fn node_fts_search_drops_hits_with_the_session() {
        let Some(store) = try_memory() else { return };
        let ses = sample_session();
        store.create_session(&ses).unwrap();
        store
            .save_turn("s-1", &sample_turn(1), &ses.history)
            .unwrap();
        assert_eq!(store.search_messages("привет", 10).unwrap().len(), 1);
        store.delete_session("s-1").unwrap();
        assert!(
            store.search_messages("привет", 10).unwrap().is_empty(),
            "триггер messages_fts_ad чистит индекс и в sidecar-бэкенде"
        );
    }

    /// Исходник sidecar-скрипта в репозитории: источник правды для сверки
    /// схемы. Читать для этого резолв рантайма нельзя — тот может вернуть
    /// копию из `vendor/sidecar`, а она генерируется скриптом упаковки и в
    /// рабочем дереве легко отстаёт от исходника на ревизию-другую.
    fn repo_script(name: &str) -> PathBuf {
        Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("../../sidecar")
            .join(name)
    }

    /// Схема не разъезжается с JS-sidecar: одинаковый набор таблиц.
    #[test]
    fn schema_matches_js_sidecar() {
        let source = repo_script("store-server.js");
        let raw = std::fs::read_to_string(&source)
            .unwrap_or_else(|e| panic!("{} не читается: {e}", source.display()));
        /* CRLF → LF: рабочая копия на Windows с core.autocrlf=true содержит
        \r\n, а сверяем мы DDL, а не стиль переносов в файле. Без этого
        многострочные утверждения (external-content FTS5) валятся локально и
        проходят в CI — худший вид расхождения. */
        let js = raw.replace("\r\n", "\n");
        for table in [
            "sessions",
            "turns",
            "messages",
            "prefs",
            "approvals",
            "tasks",
            "metrics",
            "memory",
        ] {
            assert!(
                js.contains(&format!("CREATE TABLE IF NOT EXISTS {table}(")),
                "в store-server.js нет таблицы {table}"
            );
        }
        /* Индекс — тоже часть схемы: без idx_memory_scope выборка памяти
        сканировала бы таблицу на каждом ходе. */
        assert!(
            js.contains("CREATE INDEX IF NOT EXISTS idx_memory_scope "),
            "в store-server.js нет индекса idx_memory_scope"
        );
        for col in ["summary", "approval_policy"] {
            assert!(
                js.contains(&format!("ALTER TABLE sessions ADD COLUMN {col}")),
                "в store-server.js нет миграции колонки {col}"
            );
        }
        for col in ["parent_turn_id", "checkpoint_sha"] {
            assert!(
                js.contains(&format!("ALTER TABLE turns ADD COLUMN {col}")),
                "в store-server.js нет миграции колонки turns.{col}"
            );
        }
        /* F-10: FTS5-индекс — тоже часть схемы, и его DDL обязан совпадать.
        Расхождение молчаливо: один бэкенд ищет по истории, второй возвращает
        пусто, а падает только поиск, который никто не запускает в тестах
        против обеих реализаций. Поэтому сверяем не наличие имени, а текст
        виртуальной таблицы, токенайзер и тела триггеров. */
        assert!(
            js.contains("CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5("),
            "в store-server.js нет виртуальной таблицы messages_fts"
        );
        assert!(
            js.contains("tokenize='unicode61 remove_diacritics 2'"),
            "в store-server.js другой токенайзер FTS5"
        );
        assert!(
            js.contains("content='messages',\n  content_rowid='id',")
                || js.contains("content='messages', content_rowid='id',"),
            "в store-server.js external-content FTS5 не над messages"
        );
        for trig in ["messages_fts_ai", "messages_fts_ad", "messages_fts_au"] {
            assert!(
                js.contains(&format!("CREATE TRIGGER IF NOT EXISTS {trig} ")),
                "в store-server.js нет триггера {trig}"
            );
        }
        for body in [
            "INSERT INTO messages_fts(rowid, content, reasoning) VALUES (new.id, new.content, new.reasoning);",
            "INSERT INTO messages_fts(messages_fts, rowid, content, reasoning) VALUES ('delete', old.id, old.content, old.reasoning);",
        ] {
            assert!(
                js.contains(body),
                "в store-server.js другое тело триггера FTS5: {body}"
            );
        }
        // Дозация индекса для баз, созданных до FTS5, и её критерий.
        assert!(
            js.contains("INSERT INTO messages_fts(messages_fts) VALUES('rebuild')"),
            "в store-server.js нет rebuild-дозации индекса"
        );
        assert!(
            js.contains("(SELECT COUNT(*) FROM (SELECT rowid FROM messages_fts_docsize LIMIT 1)) = 0"),
            "в store-server.js другой критерий дозации FTS5 (нужна теневая таблица messages_fts_docsize)"
        );
    }

    /// Расклад пригоден, только если рядом со скриптом есть нативный
    /// бинарник better-sqlite3, а копия скрипта не старше исходника в
    /// `sidecar/`. Регрессия CI (ревизия 49): `pnpm install
    /// --ignore-scripts` оставляет `sidecar/node_modules/better-sqlite3`
    /// без бинарника, prebuilt качается только в `vendor/sidecar`, а поиск
    /// брал первый найденный скрипт — репозиторный. `require` падал уже
    /// внутри Node, prefs-мост JS-плагинов молча возвращал null, и весь
    /// прогон красил ровно один тест без внятной причины.
    #[test]
    fn sidecar_layout_requires_sqlite_binding_and_freshness() {
        /* Уникальность корня обязательна: два параллельных прогона на одной
        машине не должны читать чужой расклад. */
        let uniq = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map(|d| d.subsec_nanos())
            .unwrap_or(0);
        let root =
            std::env::temp_dir().join(format!("swagcod-layout-{}-{}", std::process::id(), uniq));
        std::fs::create_dir_all(&root).unwrap();
        let repo = root.join("sidecar");
        let vendored = root.join("vendor").join("sidecar");
        std::fs::create_dir_all(&repo).unwrap();
        std::fs::create_dir_all(&vendored).unwrap();
        let repo_script = repo.join("store-server.js");
        let vendor_script = vendored.join("store-server.js");
        std::fs::write(&repo_script, "// source").unwrap();
        std::fs::write(&vendor_script, "// source").unwrap();

        assert!(
            !layout_has_sqlite(&repo_script),
            "скрипт без node_modules/better-sqlite3 не поднимет стор"
        );
        assert_eq!(
            pick_viable_layout(&[repo_script.clone(), vendor_script.clone()]),
            None,
            "нет бинарника ни в одном раскладе — честно None"
        );

        /* Бинарник только в vendor, а копия там СТАРЕЕ исходника: время
        записи ставим явно, потому что порядок `fs::write` на бегунке CI не
        даёт гарантий — два write подряд попадают в один тик системных часов
        (NTFS ленится обновлять mtime чаще ~15 мс), метки совпадают, а
        «dst >= src» читает равные метки как свежие. Прогон 39031688630
        красил на этом весь шаг `test (headless)`, тогда как локально тот же
        код проходил. */
        write_binding(&vendored);
        std::fs::write(&repo_script, "// source, исправленный после сборки vendor").unwrap();
        set_mtime(&vendor_script, 1_600_000_000);
        set_mtime(&repo_script, 1_700_000_000);
        assert!(layout_has_sqlite(&vendor_script), "бинарник на месте");
        assert!(
            !layout_is_viable(&vendor_script),
            "устаревшая копия не должна выигрывать у свежего исходника"
        );
        assert_eq!(
            pick_viable_layout(&[repo_script.clone(), vendor_script.clone()]),
            None,
            "единственный расклад с бинарником устарел — пригодного нет"
        );
        assert_eq!(
            repo_sidecar_source(&vendor_script),
            Some(repo_script.clone()),
            "исходник для копии в vendor/sidecar находится"
        );

        /* Vendor пересобран (bundle-sidecar.ps1 копирует скрипты и ставит
        им свежее время записи) — расклад снова пригоден и выигрывает у
        репозиторного, где бинарника нет. Метку копии снова задаём явно:
        «записали позже, значит время больше» на бегунке CI не гарантировано. */
        std::fs::write(
            &vendor_script,
            "// source, исправленный после сборки vendor",
        )
        .unwrap();
        set_mtime(&vendor_script, 1_800_000_000);
        assert!(layout_is_viable(&vendor_script));
        assert_eq!(
            pick_viable_layout(&[repo_script.clone(), vendor_script.clone()]),
            Some(vendor_script.clone()),
            "vendor с бинарником выигрывает у репозиторного расклада без него"
        );

        // Бинарник появился и в репозиторном раскладе — побеждает он:
        // это исходник, а не его копия.
        write_binding(&repo);
        assert_eq!(
            pick_viable_layout(&[repo_script.clone(), vendor_script.clone()]),
            Some(repo_script.clone()),
            "порядок кандидатов: пригодный исходник раньше пригодной копии"
        );
        assert_eq!(
            pick_viable_layout(&[vendor_script.clone(), repo_script.clone()]),
            Some(vendor_script),
            "порядок кандидатов не ломает выбор: пригоден любой из них"
        );

        /* Бандл рядом с exe: отдельное дерево, у которого в предках нет
        `sidecar/` (у установленного приложения исходника нет вовсе),
        поэтому решает один бинарник — сравнивать свежесть не с чем. */
        let ship = std::env::temp_dir().join(format!("swagcod-ship-{uniq}"));
        let bundled = ship.join("sidecar");
        std::fs::create_dir_all(&bundled).unwrap();
        let bundled_script = bundled.join("store-server.js");
        std::fs::write(&bundled_script, "// shipped").unwrap();
        write_binding(&bundled);
        assert_eq!(
            repo_sidecar_source(&bundled_script),
            None,
            "в установленном бандле нет исходника sidecar/ — сверять свежесть не с чем"
        );
        assert!(
            layout_is_viable(&bundled_script),
            "в бандле пригодность определяет бинарник"
        );

        let _ = std::fs::remove_dir_all(&root);
        let _ = std::fs::remove_dir_all(&ship);
    }

    /// Нативный бинарник в расклад `dir`. Содержимое не важно: резолв
    /// смотрит на факт наличия, а грузит его Node.
    fn write_binding(dir: &Path) {
        let binding = sqlite_binding(dir);
        std::fs::create_dir_all(binding.parent().unwrap()).unwrap();
        std::fs::write(&binding, b"stub").unwrap();
    }

    /// Время записи файла в секундах от эпохи — задаём явно, а не полагаемся
    /// на порядок `fs::write`: NTFS обновляет mtime лениво (раз в ~15 мс), и
    /// два файла, записанных подряд, получают ОДНУ метку. Тогда «dst >= src»
    /// читает равные метки как свежие, и тест свежести проходит или падает в
    /// зависимости от нагрузки на бегунок.
    fn set_mtime(path: &Path, secs: u64) {
        let when = std::time::UNIX_EPOCH + std::time::Duration::from_secs(secs);
        std::fs::File::options()
            .write(true)
            .open(path)
            .and_then(|f| f.set_modified(when))
            .unwrap_or_else(|e| panic!("не выставить mtime {}: {e}", path.display()));
    }

    /// В рабочем дереве репозитория живой расклад находится: иначе
    /// `schema_matches_js_sidecar` читал бы не тот скрипт, а стор в auto
    /// молча падал бы в rusqlite.
    #[test]
    fn sidecar_file_prefers_layout_that_can_load_sqlite() {
        if std::env::var_os("SWAGCOD_STORE_SIDECAR").is_some() {
            return; // явный путь владельца важнее резолва раскладов
        }
        let Some(p) = sidecar_file("store-server.js") else {
            return; // расклада нет вовсе (свежая машина) — тест не о этом
        };
        let viable = sidecar_sqlite_file("store-server.js");
        match viable {
            Some(v) => assert_eq!(
                v,
                p,
                "sidecar_file вернул {} , хотя есть живой расклад {}",
                p.display(),
                v.display()
            ),
            /* Живого нет нигде: sidecar_file обязан вернуть хоть что-то,
            а стор — внятную ошибку с инструкцией (sidecar_script). */
            None => {
                let err = sidecar_script().expect_err("без бинарника stor не поднимается");
                let msg = err.to_string();
                assert!(
                    msg.contains("better_sqlite3.node") && msg.contains("bundle-sidecar.ps1"),
                    "ошибка не называет причину и лечение: {msg}"
                );
            }
        }
    }
}
