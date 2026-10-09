/*! Тонкая Tauri-обёртка (D-011).

**Правило этого крейта: здесь не живёт бизнес-логика.** Всё содержательное —
в `swagcod-core`, `swagcod-provider`, `swagcod-pty`, `swagcod-fsx`. Этот файл
делает ровно три вещи: держит состояние, пробрасывает события шины в UI и
отдаёт команды. Причина — именно такой слой в Electron-версиях разрастается и
начинает тормозить, а тонкую обёртку легко переписать под другой шелл.
*/

use std::sync::Arc;

pub mod crashlog;
pub mod dpapi;
pub mod dsh_import;
pub mod httpapi;
pub mod jsplugins;
pub mod mcp;
pub mod providers;
pub mod stdio_rpc;

use serde::Serialize;
use swagcod_core::bus::{Bus, Event, EventKind};
use swagcod_core::session::{Session, SessionId, TurnRecord};
use swagcod_core::tasks::{
    task_backoff_ms, Task, TASK_CANCELLED, TASK_DONE, TASK_FAILED, TASK_QUEUED,
};
use swagcod_core::turn::{
    builtin_tool_specs, describe_call, truncate_output, ApprovalDecision, ApprovalPolicy,
    ToolOutcome, TurnConfig, TurnMachine, TurnOutcome, TurnStep,
};
use swagcod_provider::types::{ChatMessage, ToolCall, ToolSpec};
use swagcod_provider::{AnyProvider, ChatRequest, Router, StreamEvent};
use tauri::{AppHandle, Emitter, Manager, State};
use tokio::sync::{oneshot, Mutex};

/// Ручка живого хода: нужна кнопке «стоп» в UI.
pub struct TurnHandle {
    pub session: String,
    pub cancelled: Arc<std::sync::atomic::AtomicBool>,
    pub task: tauri::async_runtime::JoinHandle<()>,
}

/// Состояние приложения, разделяемое между командами.
pub struct AppState {
    pub bus: Bus,
    pub sessions: Mutex<Vec<Session>>,
    pub config: Mutex<TurnConfig>,
    /// Живые ходы по turn_id: остановка ставит флаг и абортит задачу стрима.
    pub turns: Mutex<std::collections::HashMap<String, TurnHandle>>,
    /// Каналы подтверждений по call_id: драйвер хода ждёт receiver,
    /// команда `respond_approval` стреляет в sender. Решение человека
    /// приходит в ядро, а не исполняется в UI (DECISIONS.md §2).
    pub approvals: Mutex<std::collections::HashMap<String, oneshot::Sender<ApprovalDecision>>>,
    /// B-1: персистентность сессий и настроек. Синхронный SQLite за
    /// std-мьютексом: операции миллисекундные, async над ними дал бы
    /// только сложность (философия better-sqlite из плана B-1).
    pub store: std::sync::Mutex<Box<dyn swagcod_core::store::Store>>,
    /// B-2: реестр PTY-сессий. Arc: сливной поток вывода живёт дольше
    /// команды, которая его подняла.
    pub ptys: std::sync::Arc<swagcod_pty::PtyManager>,
    /// B-4: наблюдатели рабочих директорий. Дроп ручки останавливает watcher.
    pub watches: Mutex<std::collections::HashMap<String, swagcod_fsx::WatchHandle>>,
    /// B-4: кэш индексов файлов с TTL — поиск не должен обходить репозиторий
    /// на каждое нажатие клавиши.
    pub indexes: std::sync::Mutex<
        std::collections::HashMap<
            String,
            (std::time::Instant, std::sync::Arc<swagcod_fsx::FileIndex>),
        >,
    >,
    /// B-5: зарегистрированные внешние инструменты (плагины). Модель видит
    /// их в одном списке со встроенными; исполнение — через shell команду,
    /// аргументы JSON-ом в stdin. Не-встроенное имя при OnDangerous всегда
    /// уходит на подтверждение (ApprovalPolicy::BUILTIN).
    pub plugins: std::sync::Mutex<Vec<PluginTool>>,
    /// E-2: кэш эмбеддингов (отдельная база embeddings.db — кэш, не домен).
    /// Ленивое открытие при первом обращении; поиск синхронный и быстрый,
    /// поэтому std-мьютекс, как у store.
    pub semantic: std::sync::Mutex<Option<swagcod_core::semantic::SemanticIndex>>,
    /// E-2: индексация в ходе — две задачи не должны толкаться в одной базе.
    pub semantic_busy: std::sync::atomic::AtomicBool,
    /// E-2: когда последний раз запускалась индексация (мс epoch) — частые
    /// ходы не должны гонять полный обход репозитория каждый раз.
    pub semantic_last_ms: std::sync::atomic::AtomicU64,
    /// B-8: сколько раз UI-насос отставал и сколько событий при этом
    /// потеряно. Диагностический экспорт без этих чисел слеп: «лагает»
    /// без счётчика — это анекдот, а не наблюдение.
    pub lagging_events: Arc<std::sync::atomic::AtomicU64>,
    pub lagging_dropped: Arc<std::sync::atomic::AtomicU64>,
    /// B-8: живые ходы для watchdog — возраст и время последнего события.
    /// std-мьютекс: обновление на каждом токене стрима должно быть дешевле
    /// самого токена.
    pub turn_activity: Arc<std::sync::Mutex<std::collections::HashMap<String, TurnActivity>>>,
    /// D-121: сводные цифры сессий для сайдбара. Сессии живут в памяти
    /// «лёгкими» (история и журнал ходов — в базе, грузятся по требованию),
    /// поэтому list_sessions берёт счётчики отсюда, а не из пустого журнала.
    pub session_meta: std::sync::Mutex<std::collections::HashMap<String, SessionMeta>>,
    /// E-3: MCP-серверы — живые stdio-соединения, инструменты для модели и
    /// честные статусы. tokio-мьютекс: подключение — async (рукопожатие).
    pub mcp: Mutex<mcp::McpRegistry>,
    /// E-4: sidecar JS-плагинов — живое соединение, загруженные инструменты
    /// и честные ошибки загрузки каталога.
    pub js_host: std::sync::Mutex<Option<Arc<jsplugins::JsHost>>>,
    pub js_tools: std::sync::Mutex<Vec<jsplugins::JsPluginTool>>,
    pub js_errors: std::sync::Mutex<Vec<String>>,
}

/// D-121: лёгкий снимок сессии — то, что нужно сайдбару, без мегабайтов
/// истории в RAM.
#[derive(Debug, Clone, Copy)]
pub struct SessionMeta {
    pub turns: usize,
    pub est_context_tokens: u32,
    pub last_ok: Option<bool>,
    pub last_activity_ms: u64,
}

fn meta_of(s: &Session) -> SessionMeta {
    SessionMeta {
        turns: s.turns.len(),
        est_context_tokens: s.estimate_context_tokens(),
        last_ok: s.turns.last().map(|t| t.ok),
        last_activity_ms: s
            .turns
            .last()
            .map(|t| t.ended_ms.unwrap_or(t.started_ms))
            .unwrap_or(s.created_ms),
    }
}

/// D-121: сделать список сессий «лёгким» для RAM: тяжёлые история и журнал
/// вычищаются (они уже в базе), сводные цифры уходят в meta-карту.
fn light_sessions(
    loaded: Vec<Session>,
) -> (Vec<Session>, std::collections::HashMap<String, SessionMeta>) {
    let mut meta = std::collections::HashMap::new();
    let mut light: Vec<Session> = Vec::with_capacity(loaded.len());
    for mut s in loaded {
        meta.insert(s.id.to_string(), meta_of(&s));
        s.history.clear();
        s.turns.clear();
        light.push(s);
    }
    (light, meta)
}

/// D-121: подгрузить историю и журнал сессии из store, если в памяти их
/// нет. Вызывается перед началом хода и при показе транскрипта — двум
/// командам нужна полная сессия, остальным достаточно лёгкой.
fn rehydrate_session(state: &AppState, session: &mut Session) {
    if !session.history.is_empty() || !session.turns.is_empty() {
        return;
    }
    if let Ok(store) = state.store.lock() {
        match store.load_session(session.id.as_str()) {
            Ok(Some(full)) => {
                session.history = full.history;
                session.turns = full.turns;
                if session.summary.is_empty() {
                    session.summary = full.summary;
                }
            }
            Ok(None) => {}
            Err(e) => eprintln!("store: история сессии не подгружена: {e}"),
        }
    }
}

/// B-8: запись watchdog о живом ходе.
#[derive(Debug, Clone)]
pub struct TurnActivity {
    pub session: String,
    pub started_ms: u64,
    pub last_ms: u64,
    /// Ход ждёт человека (диалог подтверждения): тишина здесь — не зависание,
    /// watchdog молчит.
    pub waiting_human: bool,
    /// Момент последнего предупреждения: не долбить каждые 30 секунд.
    pub warned_ms: Option<u64>,
}

/// Внешний инструмент: spec для модели + команда для исполнения.
#[derive(Debug, Clone, Serialize)]
pub struct PluginTool {
    pub name: String,
    pub description: String,
    /// JSON-Schema параметров (объект).
    pub parameters: serde_json::Value,
    /// Командная строка: исполняется через cmd /C (windows) или sh -c,
    /// cwd — рабочая директория сессии, аргументы приходят JSON-ом в stdin.
    pub command: String,
}

impl Default for AppState {
    fn default() -> Self {
        Self {
            bus: Bus::new(),
            sessions: Mutex::new(Vec::new()),
            config: Mutex::new(TurnConfig::default()),
            turns: Mutex::new(std::collections::HashMap::new()),
            approvals: Mutex::new(std::collections::HashMap::new()),
            store: std::sync::Mutex::new(swagcod_core::store::open_memory()),
            ptys: std::sync::Arc::new(swagcod_pty::PtyManager::new()),
            watches: Mutex::new(std::collections::HashMap::new()),
            indexes: std::sync::Mutex::new(std::collections::HashMap::new()),
            plugins: std::sync::Mutex::new(Vec::new()),
            semantic: std::sync::Mutex::new(None),
            semantic_busy: std::sync::atomic::AtomicBool::new(false),
            semantic_last_ms: std::sync::atomic::AtomicU64::new(0),
            lagging_events: Arc::new(std::sync::atomic::AtomicU64::new(0)),
            lagging_dropped: Arc::new(std::sync::atomic::AtomicU64::new(0)),
            turn_activity: Arc::new(std::sync::Mutex::new(std::collections::HashMap::new())),
            session_meta: std::sync::Mutex::new(std::collections::HashMap::new()),
            mcp: Mutex::new(mcp::McpRegistry::default()),
            js_host: std::sync::Mutex::new(None),
            js_tools: std::sync::Mutex::new(Vec::new()),
            js_errors: std::sync::Mutex::new(Vec::new()),
        }
    }
}

/// B-8: момент запуска процесса для uptime в диагностике.
static START_MS: std::sync::OnceLock<u64> = std::sync::OnceLock::new();

fn start_ms() -> u64 {
    *START_MS.get_or_init(swagcod_core::bus::now_ms)
}

#[derive(Debug, Clone, Serialize)]
pub struct SessionBrief {
    pub id: String,
    pub title: String,
    pub cwd: String,
    pub model: String,
    pub status: swagcod_core::session::SessionStatus,
    pub turns: usize,
    pub est_context_tokens: u32,
    /// Результат последнего хода: UI рисует честный индикатор
    /// (ошибка / работа / готово), а не «всегда зелёную точку».
    pub last_ok: Option<bool>,
    /// Когда сессия последний раз шевелилась: конец последнего хода или
    /// создание. Сайдбар сортирует группы «по обновлению» как DeepSeek.
    pub last_activity_ms: u64,
    /// B-7: политика подтверждений сессии (None = глобальная). UI настроек
    /// инициализирует селектор текущим значением, а не гадает.
    pub approval_policy: Option<ApprovalPolicy>,
}

impl From<&Session> for SessionBrief {
    fn from(s: &Session) -> Self {
        Self {
            id: s.id.to_string(),
            title: if s.title.is_empty() {
                s.cwd.clone()
            } else {
                s.title.clone()
            },
            cwd: s.cwd.clone(),
            model: s.model.clone(),
            status: s.status,
            turns: s.turns.len(),
            est_context_tokens: s.estimate_context_tokens(),
            last_ok: s.turns.last().map(|t| t.ok),
            last_activity_ms: s
                .turns
                .last()
                .map(|t| t.ended_ms.unwrap_or(t.started_ms))
                .unwrap_or(s.created_ms),
            approval_policy: s.approval_policy,
        }
    }
}

#[derive(Debug, Clone, Serialize)]
pub struct BuildInfo {
    pub version: String,
    pub rustc: String,
    pub target: String,
    pub debug: bool,
}

/// Диапазон шины для подписки: UI батчит по кадрам, и ему нужен `seq`.
#[derive(Debug, Clone, Serialize)]
pub struct WireEvent {
    pub seq: u64,
    pub ts_ms: u64,
    pub kind: swagcod_core::bus::EventKind,
}

impl From<Event> for WireEvent {
    fn from(e: Event) -> Self {
        Self {
            seq: e.seq,
            ts_ms: e.ts_ms,
            kind: e.kind,
        }
    }
}

/// Запустить насос событий: шина ядра → канал Tauri → UI.
///
/// Подписка идёт на bounded-канал шины, поэтому медленный WebView не съест
/// память, а отставание будет видно как `Lagged` в логе, а не как зависание.
/// B-8: отставания считаются — диагностика отдаёт и число лагов, и объём
/// потерянных событий.
fn spawn_event_pump(
    app: AppHandle,
    bus: Bus,
    lagging_events: Arc<std::sync::atomic::AtomicU64>,
    lagging_dropped: Arc<std::sync::atomic::AtomicU64>,
) -> tauri::async_runtime::JoinHandle<()> {
    tauri::async_runtime::spawn(async move {
        let mut rx = bus.subscribe();
        loop {
            match rx.recv().await {
                Ok(ev) => {
                    let wire = WireEvent::from(ev);
                    if app.emit("swagcod://event", wire).is_err() {
                        // UI закрыт — насос больше не нужен.
                        break;
                    }
                }
                Err(tokio::sync::broadcast::error::RecvError::Lagged(n)) => {
                    lagging_events.fetch_add(1, std::sync::atomic::Ordering::Relaxed);
                    lagging_dropped.fetch_add(n, std::sync::atomic::Ordering::Relaxed);
                    // Не фатально: сообщаем UI, что нужно перечитать снапшот.
                    let _ = app.emit(
                        "swagcod://event",
                        WireEvent {
                            seq: 0,
                            ts_ms: swagcod_core::bus::now_ms(),
                            kind: swagcod_core::bus::EventKind::Status {
                                message: format!("UI отстал на {n} событий, перечитайте снапшот"),
                            },
                        },
                    );
                }
                Err(tokio::sync::broadcast::error::RecvError::Closed) => break,
            }
        }
    })
}

#[tauri::command]
fn build_info() -> BuildInfo {
    BuildInfo {
        version: env!("CARGO_PKG_VERSION").to_string(),
        rustc: option_env!("RUSTC_VERSION")
            .unwrap_or("unknown")
            .to_string(),
        target: std::env::consts::ARCH.to_string() + "-" + std::env::consts::OS,
        debug: cfg!(debug_assertions),
    }
}

/// Стартовые предпочтения из окружения: SWAGCOD_APPEARANCE, SWAGCOD_BG_MODE.
///
/// Нужно для киосков/скриншотов/CI: тему и режим фона можно задать снаружи,
/// не трогая localStorage. Пустые/неизвестные значения игнорируются.
#[tauri::command]
fn initial_prefs() -> std::collections::BTreeMap<String, String> {
    let mut out = std::collections::BTreeMap::new();
    for (key, env_name) in [
        ("appearance", "SWAGCOD_APPEARANCE"),
        ("bg_mode", "SWAGCOD_BG_MODE"),
    ] {
        if let Ok(v) = std::env::var(env_name) {
            let v = v.trim().to_string();
            if !v.is_empty() {
                out.insert(key.to_string(), v);
            }
        }
    }
    out
}

/* ── Пользовательский фон (gif / mp4 / webm / картинки) ───────────────────
 * Файлы живут в собственной папке приложения, поэтому команды чтения/записи
 * не дают доступ к произвольным путям: имя санитизируется, расширение
 * проверяется по белому списку, всё остальное отклоняется. */

/// Каталог фонов: %LOCALAPPDATA%\SwagCod\backgrounds.
fn backgrounds_dir() -> Result<std::path::PathBuf, String> {
    let base = std::env::var("LOCALAPPDATA")
        .map(std::path::PathBuf::from)
        .unwrap_or_else(|_| std::env::temp_dir());
    Ok(base.join("SwagCod").join("backgrounds"))
}

const BG_MAX_BYTES: usize = 48 * 1024 * 1024;
const BG_EXTENSIONS: [&str; 8] = ["gif", "mp4", "webm", "png", "jpg", "jpeg", "webp", "apng"];

/// Минимальный base64 без зависимостей: IPC гоняет JSON, поэтому бинарные
/// файлы фона передаются строкой, а не массивом из миллионов чисел.
fn base64_encode(data: &[u8]) -> String {
    const TABLE: &[u8; 64] = b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
    let mut out = String::with_capacity(data.len().div_ceil(3) * 4);
    for chunk in data.chunks(3) {
        let b = [
            chunk[0],
            *chunk.get(1).unwrap_or(&0),
            *chunk.get(2).unwrap_or(&0),
        ];
        out.push(TABLE[(b[0] >> 2) as usize] as char);
        out.push(TABLE[(((b[0] & 3) << 4) | (b[1] >> 4)) as usize] as char);
        out.push(if chunk.len() > 1 {
            TABLE[(((b[1] & 15) << 2) | (b[2] >> 6)) as usize] as char
        } else {
            '='
        });
        out.push(if chunk.len() > 2 {
            TABLE[(b[2] & 63) as usize] as char
        } else {
            '='
        });
    }
    out
}

fn base64_decode(input: &str) -> Result<Vec<u8>, String> {
    let mut acc: u32 = 0;
    let mut bits = 0;
    let mut out = Vec::with_capacity(input.len() / 4 * 3);
    for c in input.chars() {
        if c == '=' || c.is_whitespace() {
            continue;
        }
        let v = match c {
            'A'..='Z' => c as u32 - 'A' as u32,
            'a'..='z' => c as u32 - 'a' as u32 + 26,
            '0'..='9' => c as u32 - '0' as u32 + 52,
            '+' => 62,
            '/' => 63,
            _ => return Err("base64 содержит недопустимый символ".into()),
        };
        acc = (acc << 6) | v;
        bits += 6;
        if bits >= 8 {
            bits -= 8;
            out.push((acc >> bits) as u8);
        }
    }
    Ok(out)
}

/// Санитизация имени файла фона: только безопасные символы, без путей.
fn sanitize_bg_name(name: &str) -> Result<String, String> {
    let file_name = name.rsplit(['/', '\\']).next().unwrap_or_default().trim();
    let clean: String = file_name
        .chars()
        .filter(|c| c.is_ascii_alphanumeric() || matches!(c, '.' | '-' | '_'))
        .collect();
    if clean.is_empty() || clean.len() > 80 {
        return Err("недопустимое имя файла фона".into());
    }
    let ext = clean
        .rsplit('.')
        .next()
        .unwrap_or_default()
        .to_ascii_lowercase();
    if !BG_EXTENSIONS.contains(&ext.as_str()) {
        return Err(format!("расширение .{ext} не подходит для фона"));
    }
    Ok(clean)
}

#[tauri::command]
async fn save_background(name: String, data_b64: String) -> Result<String, String> {
    let data = base64_decode(&data_b64)?;
    if data.is_empty() {
        return Err("пустой файл фона".into());
    }
    if data.len() > BG_MAX_BYTES {
        return Err("файл фона больше 48 МБ".into());
    }
    let clean = sanitize_bg_name(&name)?;
    let dir = backgrounds_dir()?;
    std::fs::create_dir_all(&dir).map_err(|e| format!("не создал каталог фонов: {e}"))?;
    let path = dir.join(&clean);
    std::fs::write(&path, &data).map_err(|e| format!("не записал фон: {e}"))?;
    Ok(clean)
}

#[tauri::command]
async fn load_background(name: String) -> Result<String, String> {
    let clean = sanitize_bg_name(&name)?;
    let path = backgrounds_dir()?.join(&clean);
    let data = std::fs::read(&path).map_err(|e| format!("не прочитал фон: {e}"))?;
    Ok(base64_encode(&data))
}

#[tauri::command]
async fn delete_background(name: String) -> Result<(), String> {
    let clean = sanitize_bg_name(&name)?;
    let path = backgrounds_dir()?.join(&clean);
    if path.exists() {
        std::fs::remove_file(&path).map_err(|e| format!("не удалил фон: {e}"))?;
    }
    Ok(())
}

/// История сессии в wire-формате шины событий.
///
/// Фронтенд прогоняет её через тот же `applyBatch`, что и живой стрим,
/// поэтому восстановленный после переключения чат выглядит идентично:
/// reasoning и content остаются отдельными строками (находка 1, §5.6).
/// Пользовательские реплики и выводы тулзов берём из wire-истории сессии —
/// в журнале ходов их нет.
/// События сессии в wire-формате B-8 (D-121: ленивая история — перед
/// экспортом сессия догружается). Потребители: команда session_transcript
/// и задача E-5 journal_export.
async fn transcript_events(
    state: &AppState,
    session_id: &str,
) -> Result<Vec<serde_json::Value>, String> {
    use serde_json::{json, Value};
    use swagcod_provider::types::Role;

    let mut guard = state.sessions.lock().await;
    let session = guard
        .iter_mut()
        .find(|s| s.id.as_str() == session_id)
        .ok_or_else(|| format!("сессия {session_id} не найдена"))?;
    // D-121: транскрипт — один из двух потребителей полной сессии.
    rehydrate_session(state, session);

    let mut user_texts: std::collections::VecDeque<String> = std::collections::VecDeque::new();
    let mut tool_outputs: std::collections::HashMap<String, String> =
        std::collections::HashMap::new();
    for m in &session.history {
        match m.role {
            Role::User => user_texts.push_back(m.content.clone()),
            Role::Tool => {
                if let Some(id) = &m.tool_call_id {
                    tool_outputs.insert(id.clone(), m.content.clone());
                }
            }
            _ => {}
        }
    }

    let mut out: Vec<Value> = Vec::new();
    for turn in &session.turns {
        let tid = turn.id.as_str().to_string();
        let ts = turn.started_ms;
        /* E-6: parent в turn_started — ветки суб-агентов видны и в истории,
        а не только в живом стриме (wire-формат тот же, поле опционально). */
        out.push(json!({
            "seq": 0, "ts_ms": ts,
            "kind": { "kind": "turn_started", "data": {
                "turn": tid, "session": session_id,
                "parent": turn.parent_turn_id.as_ref().map(|p| p.as_str())
            } }
        }));
        if let Some(text) = user_texts.pop_front() {
            out.push(json!({
                "seq": 0, "ts_ms": ts,
                "kind": { "kind": "user", "data": { "turn": tid, "text": text } }
            }));
        }
        if !turn.reasoning.is_empty() {
            out.push(json!({
                "seq": 0, "ts_ms": ts,
                "kind": { "kind": "reasoning", "data": { "turn": tid, "text": turn.reasoning } }
            }));
        }
        for tc in &turn.tool_calls {
            out.push(json!({
                "seq": 0, "ts_ms": ts,
                "kind": { "kind": "tool_call", "data": {
                    "turn": tid, "call_id": tc.id, "name": tc.name, "arguments": tc.arguments
                } }
            }));
            let output = tool_outputs.get(&tc.id).cloned().unwrap_or_default();
            out.push(json!({
                "seq": 0, "ts_ms": ts,
                "kind": { "kind": "tool_result", "data": {
                    "turn": tid, "call_id": tc.id, "ok": true, "output": output, "elapsed_ms": 0
                } }
            }));
        }
        if !turn.content.is_empty() {
            out.push(json!({
                "seq": 0, "ts_ms": ts,
                "kind": { "kind": "content", "data": { "turn": tid, "text": turn.content } }
            }));
        }
        out.push(json!({
            "seq": 0, "ts_ms": turn.ended_ms.unwrap_or(ts),
            "kind": { "kind": "turn_ended", "data": {
                "turn": tid, "session": session_id, "ok": turn.ok, "reason": turn.failure
            } }
        }));
    }
    Ok(out)
}

/// Журнал сессии в формате B-8.
#[tauri::command]
async fn session_transcript(
    state: State<'_, Arc<AppState>>,
    session_id: String,
) -> Result<Vec<serde_json::Value>, String> {
    transcript_events(&state, &session_id).await
}

/// Журнал сессии в файл, выбранный в нативном диалоге: JSONL wire-событий
/// в том же формате, что шина. Белый список расширений не даёт случайно
/// затереть журналом исходники или exe.
#[tauri::command]
async fn save_session_log(
    state: State<'_, Arc<AppState>>,
    session_id: String,
    path: String,
) -> Result<String, String> {
    let target = std::path::PathBuf::from(&path);
    match target.extension().and_then(|e| e.to_str()) {
        Some("jsonl") | Some("json") | Some("txt") | Some("log") => {}
        _ => return Err("журнал сохраняется только в .jsonl / .json / .txt / .log".into()),
    }
    let events = session_transcript(state, session_id).await?;
    let mut body = String::new();
    for e in &events {
        body.push_str(&serde_json::to_string(e).map_err(|e| format!("json: {e}"))?);
        body.push('\n');
    }
    if let Some(dir) = target.parent() {
        if !dir.as_os_str().is_empty() {
            std::fs::create_dir_all(dir).map_err(|e| format!("папка: {e}"))?;
        }
    }
    std::fs::write(&target, body).map_err(|e| format!("не записал журнал: {e}"))?;
    Ok(target.display().to_string())
}

#[tauri::command]
async fn list_sessions(state: State<'_, Arc<AppState>>) -> Result<Vec<SessionBrief>, String> {
    Ok(list_sessions_core(&state).await)
}

/* E-7: ядро списка сессий — общее для IPC и loopback REST. */
pub(crate) async fn list_sessions_core(state: &AppState) -> Vec<SessionBrief> {
    let guard = state.sessions.lock().await;
    let meta = state.session_meta.lock().ok();
    guard
        .iter()
        .map(|s| {
            let mut b = SessionBrief::from(s);
            /* D-121: «лёгкая» сессия имеет пустой журнал — цифры сайдбара
            берутся из meta-снимка. У живой (регидратированной) сессии
            журнал свежее снимка — тогда верим памяти. */
            if b.turns == 0 {
                if let Some(m) = meta.as_ref().and_then(|m| m.get(&b.id)) {
                    b.turns = m.turns;
                    b.est_context_tokens = m.est_context_tokens;
                    b.last_ok = m.last_ok;
                    b.last_activity_ms = m.last_activity_ms;
                }
            }
            b
        })
        .collect()
}

#[tauri::command]
async fn create_session(
    state: State<'_, Arc<AppState>>,
    cwd: String,
    model: Option<String>,
) -> Result<SessionBrief, String> {
    let cwd = cwd.trim().to_string();
    // Пустой cwd — домашний каталог: фронтенд больше не хардкодит путь
    // разработки, а пользователь может открыть сессию где угодно.
    let cwd = if cwd.is_empty() {
        std::env::var("USERPROFILE")
            .or_else(|_| std::env::var("HOME"))
            .map_err(|_| "не удалось определить домашний каталог".to_string())?
    } else {
        cwd
    };
    let path = std::path::Path::new(&cwd);
    if !path.is_absolute() {
        return Err(format!("cwd должен быть абсолютным: {cwd}"));
    }
    if !path.is_dir() {
        return Err(format!("директория не существует: {cwd}"));
    }
    let model = model
        .filter(|m| !m.trim().is_empty())
        .or_else(|| std::env::var("SWAGCOD_MODEL").ok())
        .unwrap_or_else(|| "fable-ultra-promax".into());

    let id = SessionId::new(format!("s-{}", short_id()));
    let session = Session::new(id.clone(), cwd, model);
    let brief = SessionBrief::from(&session);
    // D-121: новая сессия сразу получает meta-запись (нулевую).
    if let Ok(mut m) = state.session_meta.lock() {
        m.insert(brief.id.clone(), meta_of(&session));
    }
    /* B-1: сессия сразу уходит в store — перезапуск не потеряет даже
    сессию без единого хода. */
    if let Ok(store) = state.store.lock() {
        if let Err(e) = store.create_session(&session) {
            eprintln!("store: сессия не записана: {e}");
        }
    }
    state.sessions.lock().await.push(session);
    state.bus.publish(swagcod_core::bus::EventKind::Status {
        message: format!("сессия {id} создана"),
    });
    Ok(brief)
}

#[tauri::command]
async fn set_approval_policy(
    state: State<'_, Arc<AppState>>,
    policy: ApprovalPolicy,
) -> Result<String, String> {
    state.config.lock().await.approval_policy = policy;
    let s = serde_json::to_string(&policy).map_err(|e| e.to_string())?;
    state.bus.publish(swagcod_core::bus::EventKind::Status {
        message: format!("политика подтверждений: {s}"),
    });
    Ok(s)
}

#[tauri::command]
async fn bus_seq(state: State<'_, Arc<AppState>>) -> Result<u64, String> {
    Ok(state.bus.seq())
}

/// B-1: файл базы в локальном профиле: %LOCALAPPDATA%\SwagCod\swagcod.db.
fn db_path() -> Result<std::path::PathBuf, String> {
    let base = std::env::var("LOCALAPPDATA")
        .or_else(|_| std::env::var("HOME"))
        .map_err(|_| "не удалось определить локальный профиль".to_string())?;
    Ok(std::path::PathBuf::from(base)
        .join("SwagCod")
        .join("swagcod.db"))
}

/// B-1: удаление сессии из памяти и из store — закрытие давнего stub в UI.
/// Занятую сессию не удаляем: ход пишет историю прямо сейчас.
#[tauri::command]
async fn delete_session(state: State<'_, Arc<AppState>>, session_id: String) -> Result<(), String> {
    {
        let sessions = state.sessions.lock().await;
        match sessions.iter().find(|s| s.id.to_string() == session_id) {
            Some(s) if s.status.is_busy() => return Err("нельзя удалить сессию: идёт ход".into()),
            None => return Err(format!("сессия {session_id} не найдена")),
            Some(_) => {}
        }
    }
    {
        let mut sessions = state.sessions.lock().await;
        sessions.retain(|s| s.id.to_string() != session_id);
    }
    // B-4: watcher и кэш индекса умирают вместе с сессией.
    state.watches.lock().await.remove(&session_id);
    if let Ok(mut m) = state.indexes.lock() {
        m.remove(&session_id);
    }
    // D-121: meta-снимок — тоже.
    if let Ok(mut m) = state.session_meta.lock() {
        m.remove(&session_id);
    }
    let store = state.store.lock().map_err(|e| e.to_string())?;
    store.delete_session(&session_id).map_err(|e| e.to_string())
}

/// B-1: настройки ключ-значение в store. Фронтенд пишет с debounce 500 мс,
/// чтобы не дёргать диск на каждый чих студии тем.
#[tauri::command]
fn get_pref(state: State<'_, Arc<AppState>>, key: String) -> Result<Option<String>, String> {
    let store = state.store.lock().map_err(|e| e.to_string())?;
    store.get_pref(&key).map_err(|e| e.to_string())
}

#[tauri::command]
fn set_pref(state: State<'_, Arc<AppState>>, key: String, value: String) -> Result<(), String> {
    let store = state.store.lock().map_err(|e| e.to_string())?;
    store.set_pref(&key, &value).map_err(|e| e.to_string())
}

/* B-2: терминал на настоящем PTY (plan B-2). Команды тонкие: spawn, write,
resize, kill. Вывод уходит не через них, а через шину событиями
pty_output с уже разобранными ANSI-операциями: разборка живёт в Rust,
WebView только рисует. */

/// Поднять PTY в cwd сессии. Shell — по желанию, иначе COMSPEC/SHELL.
#[tauri::command]
async fn pty_spawn(
    state: State<'_, Arc<AppState>>,
    session_id: String,
    shell: Option<String>,
) -> Result<String, String> {
    let cwd = {
        let sessions = state.sessions.lock().await;
        let ses = sessions
            .iter()
            .find(|s| s.id.to_string() == session_id)
            .ok_or_else(|| format!("сессия {session_id} не найдена"))?;
        ses.cwd.clone()
    };
    let shell = shell
        .filter(|s| !s.trim().is_empty())
        .unwrap_or_else(swagcod_pty::default_shell);
    let id = format!("pty-{}", short_id());
    let handle = state
        .ptys
        .spawn(&id, &shell, &cwd, 120, 30)
        .map_err(|e| e.to_string())?;
    let rx = handle.take_rx().ok_or("приёмник вывода уже занят")?;

    // Слив вывода — отдельный поток, не tokio-воркер: recv блокирующий,
    // парсинг и батчинг синхронные, а publish дешёвый.
    let bus = state.bus.clone();
    let did = id.clone();
    std::thread::Builder::new()
        .name(format!("pty-drain-{id}"))
        .spawn(move || {
            let mut parser = swagcod_pty::AnsiParser::new();
            let mut sink = swagcod_pty::VecSink::default();
            let flush = |parser: &mut swagcod_pty::AnsiParser,
                         sink: &mut swagcod_pty::VecSink,
                         bus: &Bus,
                         did: &str| {
                let ops: Vec<serde_json::Value> = sink
                    .0
                    .iter()
                    .filter_map(|o| serde_json::to_value(o).ok())
                    .collect();
                if !ops.is_empty() {
                    bus.publish(EventKind::PtyOutput {
                        pty: did.to_string(),
                        ops,
                    });
                }
                sink.0.clear();
                let _ = parser;
            };
            while let Ok(msg) = rx.recv() {
                match msg {
                    swagcod_pty::PtyMsg::Output(bytes) => {
                        parser.feed(&bytes, &mut sink);
                        flush(&mut parser, &mut sink, &bus, &did);
                    }
                    swagcod_pty::PtyMsg::Exit(code) => {
                        parser.finish(&mut sink);
                        flush(&mut parser, &mut sink, &bus, &did);
                        bus.publish(EventKind::PtyExit {
                            pty: did.clone(),
                            code,
                        });
                        break;
                    }
                }
            }
        })
        .map_err(|e| e.to_string())?;
    Ok(id)
}

#[tauri::command]
fn pty_write(state: State<'_, Arc<AppState>>, pty_id: String, text: String) -> Result<(), String> {
    let h = state
        .ptys
        .get(&pty_id)
        .ok_or_else(|| format!("pty {pty_id} не найден"))?;
    h.write(text.as_bytes()).map_err(|e| e.to_string())
}

#[tauri::command]
fn pty_resize(
    state: State<'_, Arc<AppState>>,
    pty_id: String,
    cols: u16,
    rows: u16,
) -> Result<(), String> {
    let h = state
        .ptys
        .get(&pty_id)
        .ok_or_else(|| format!("pty {pty_id} не найден"))?;
    h.resize(cols.max(8), rows.max(4))
        .map_err(|e| e.to_string())
}

#[tauri::command]
fn pty_kill(state: State<'_, Arc<AppState>>, pty_id: String) -> Result<(), String> {
    if let Some(h) = state.ptys.remove(&pty_id) {
        h.kill();
    }
    Ok(())
}

/// Вложения пользователя (скрепка в UI): текстовые файлы подшиваются к
/// сообщению блоками с содержимым. Провайдер текстовый — картинки и
/// бинарщина честно помечаются неприкреплёнными, а не молча теряются.
/// Лимит 200 КБ на файл бережёт контекст (B-3) от одного жирного лога.
const ATTACH_MAX_BYTES: u64 = 200 * 1024;

fn render_attachments(paths: &[String]) -> String {
    let mut out = String::new();
    for p in paths {
        let name = std::path::Path::new(p)
            .file_name()
            .map(|n| n.to_string_lossy().to_string())
            .unwrap_or_else(|| p.clone());
        let ext = std::path::Path::new(p)
            .extension()
            .map(|e| e.to_string_lossy().to_lowercase())
            .unwrap_or_default();
        if matches!(
            ext.as_str(),
            "png" | "jpg" | "jpeg" | "gif" | "webp" | "bmp" | "ico"
        ) {
            out.push_str(&format!(
                "\n\n---\nВложение {name}: не прикреплено — картинки пока не поддерживаются (текстовый провайдер)."
            ));
            continue;
        }
        match std::fs::metadata(p) {
            Ok(md) if md.len() > ATTACH_MAX_BYTES => {
                out.push_str(&format!(
                    "\n\n---\nВложение {name}: не прикреплено — файл больше {} КБ.",
                    ATTACH_MAX_BYTES / 1024
                ));
            }
            Ok(_) => match std::fs::read_to_string(p) {
                Ok(text) => {
                    out.push_str(&format!(
                        "\n\n---\nВложение: {name} ({p})\n```\n{text}\n```"
                    ));
                }
                Err(_) => {
                    out.push_str(&format!(
                        "\n\n---\nВложение {name}: не прикреплено — бинарный файл."
                    ));
                }
            },
            Err(e) => {
                out.push_str(&format!("\n\n---\nВложение {name}: не прикреплено — {e}."));
            }
        }
    }
    out
}

/// Запустить ход: сообщение пользователя → агентский цикл → журнал сессии.
///
/// Цикл ведёт [`TurnMachine`] из swagcod-core: эта задача лишь выполняет
/// шаги, которые машина просит (стрим модели, подтверждение человеком,
/// тулз), и публикует всё в шину. Поэтому лимит итераций и approval
/// невозможно обойти из UI: решений в фронтенде нет.
#[tauri::command]
async fn start_turn(
    state: State<'_, Arc<AppState>>,
    session_id: String,
    message: String,
    model: Option<String>,
    temperature: Option<f64>,
    attachments: Option<Vec<String>>,
) -> Result<String, String> {
    start_turn_core(
        state.inner().clone(),
        session_id,
        message,
        model,
        temperature,
        attachments,
    )
    .await
}

/* E-7: ядро старта хода без Tauri-State — один вход для IPC-команды и
loopback REST API. Поведение идентично: та же машина хода, та же шина. */
pub(crate) async fn start_turn_core(
    state: Arc<AppState>,
    session_id: String,
    message: String,
    model: Option<String>,
    temperature: Option<f64>,
    attachments: Option<Vec<String>>,
) -> Result<String, String> {
    /* Скрепка: вложения подшиваются к тексту одним сообщением — история
    и контекст видят их как часть реплики пользователя. */
    let mut message = message.trim().to_string();
    if let Some(paths) = attachments.as_ref().filter(|a| !a.is_empty()) {
        message.push_str(&render_attachments(paths));
        message = message.trim().to_string();
    }
    if message.is_empty() {
        return Err("сообщение не может быть пустым".into());
    }

    // C1-фикс: создаём провайдер ДО мутации статуса сессии.
    // Активный провайдер из настроек (default = .env/DPAPI как раньше).
    let provider = providers::build_chat_provider(&state).map_err(|e| format!("провайдер: {e}"))?;

    let (turn_id, history, session_summary, session_model, cwd, cfg) = {
        let mut sessions = state.sessions.lock().await;
        let session = sessions
            .iter_mut()
            .find(|s| s.id.to_string() == session_id)
            .ok_or_else(|| format!("сессия не найдена: {session_id}"))?;

        if !session.status.can_start_turn() {
            return Err(format!("сессия занята: {:?}", session.status));
        }

        /* D-121: второй потребитель полной сессии — ход. История грузится
        из базы лениво: в покое RAM не держит чужие транскрипты. */
        rehydrate_session(&state, session);

        let ok = session.push_user_message(&message);
        if !ok {
            return Err("не удалось начать ход".into());
        }

        let turn_id = swagcod_core::TurnId::new(format!("t-{}", short_id()));
        session.current_turn = Some(turn_id.clone());
        session.status = swagcod_core::session::SessionStatus::Running;

        let mut cfg = state.config.lock().await.clone();
        // B-7: per-session политика подтверждений перекрывает глобальную.
        if let Some(p) = session.approval_policy {
            cfg.approval_policy = p;
        }
        (
            turn_id.to_string(),
            session.history.clone(),
            session.summary.clone(),
            session.model.clone(),
            session.cwd.clone(),
            cfg,
        )
    };

    // W-6 фикс: используем переданную модель если есть, иначе из сессии.
    let effective_model = model.unwrap_or(session_model);
    // Тулзы уходят в каждом запросе: без определений модель не может их вызвать.
    // B-5: встроенные плюс зарегистрированные плагины — модель видит один список.
    let plugins: Vec<PluginTool> = state.plugins.lock().map(|p| p.clone()).unwrap_or_default();
    let mut specs = builtin_tool_specs();
    specs.extend(plugins.iter().map(|p| ToolSpec {
        name: p.name.clone(),
        description: p.description.clone(),
        parameters: p.parameters.clone(),
    }));
    /* E-3: инструменты MCP-серверов видны модели одним списком с
    встроенными и плагинами B-5. */
    {
        let reg = state.mcp.lock().await;
        specs.extend(reg.tools.iter().map(|t| ToolSpec {
            name: t.name.clone(),
            description: t.description.clone(),
            parameters: t.parameters.clone(),
        }));
    }
    /* E-4: JS-плагины sidecar — тот же единый список (имена js:*). */
    if let Ok(tools) = state.js_tools.lock() {
        specs.extend(tools.iter().map(|t| ToolSpec {
            name: t.name.clone(),
            description: t.description.clone(),
            parameters: t.parameters.clone(),
        }));
    }
    let tool_wire: Vec<serde_json::Value> = ChatRequest::new(&effective_model, Vec::new())
        .with_tools(&specs)
        .tools;
    let temperature = temperature.map(|t| t as f32);

    let bus = state.bus.clone();
    let turn = turn_id.clone();
    let sid = session_id.clone();
    let app_state = state.clone();
    let cancelled = Arc::new(std::sync::atomic::AtomicBool::new(false));
    let cancelled_in_task = cancelled.clone();

    let task = tauri::async_runtime::spawn(async move {
        let tid = swagcod_core::TurnId::new(&turn);
        let cwd = std::path::PathBuf::from(&cwd);
        let tool_timeout = cfg.tool_timeout;
        /* TurnStarted обязан уходить в шину ПЕРВЫМ: фронт строит по нему
        карту turn→session, и без неё стрим либо терялся, либо (раньше)
        сваливался в активный чат — отсюда росли «слияния» чатов. */
        bus.publish(EventKind::TurnStarted {
            turn: tid.clone(),
            session: swagcod_core::SessionId::new(&sid),
            parent: None,
        });
        /* E-2: в начале каждого хода фоном освежаем семантический индекс.
        Кулдаун 5 минут + busy-флаг: частые ходы не гоняют обход
        репозитория и не толкаются в одной базе. */
        spawn_semantic_index(app_state.clone(), cwd.clone(), false);
        /* B-8: watchdog получает запись о живом ходе. Время — now_ms из
        шины: диагностика и журнал живут на одних часах. */
        if let Ok(mut activity) = app_state.turn_activity.lock() {
            let now = swagcod_core::bus::now_ms();
            activity.insert(
                turn.clone(),
                TurnActivity {
                    session: sid.clone(),
                    started_ms: now,
                    last_ms: now,
                    waiting_human: false,
                    warned_ms: None,
                },
            );
        }

        /* B-3: контекст — калиброванный счёт, компакция сайд-запросом и
        память проекта. Всё до старта машины: машина получает уже
        подготовленную историю. F-2: компакция вынесена в run_compaction —
        её же дёргает /compact из палитры команд. */
        use swagcod_core::context;
        let calib = app_state
            .store
            .lock()
            .ok()
            .and_then(|s| s.get_pref("token_calibration").ok().flatten());
        let cpt = context::chars_per_token(&effective_model, calib.as_deref());
        let (summary, history, _) = run_compaction(
            &app_state,
            &provider,
            &sid,
            &effective_model,
            false,
            session_summary,
            history,
        )
        .await;
        // Память проекта инжектится в system-промпт каждого запроса хода.
        let memory_md =
            std::fs::read_to_string(cwd.join(".swagcod").join("MEMORY.md")).unwrap_or_default();
        let system_msg = context::system_prompt(&summary, &memory_md);

        let (mut machine, mut step) = TurnMachine::new(cfg, history);
        let mut fail_reason: Option<String> = None;
        // B-3: точный счёт от провайдера, если он отдаёт usage в стриме.
        let mut last_usage: Option<(u32, u32)> = None;

        let outcome = loop {
            // B-8: каждый шаг машины — признак жизни для watchdog.
            touch_turn(&app_state, &turn, None);
            if cancelled_in_task.load(std::sync::atomic::Ordering::SeqCst) {
                step = machine.cancel();
            }
            match step {
                TurnStep::RequestModel { history } => {
                    // System не хранится в истории сессии: подставляется
                    // свежим в каждый запрос, иначе сообщения множились бы.
                    let messages = {
                        let mut v = Vec::with_capacity(history.len() + 1);
                        v.push(system_msg.clone());
                        v.extend(history);
                        v
                    };
                    let request = ChatRequest {
                        model: effective_model.clone(),
                        messages,
                        stream: true,
                        temperature,
                        max_tokens: None,
                        tools: tool_wire.clone(),
                    };
                    let stream_started = std::time::Instant::now();
                    let (mut rx, handle) = match provider.stream(request) {
                        Ok(pair) => pair,
                        Err(e) => {
                            fail_reason = Some(format!("ошибка стрима: {e}"));
                            bus.publish(EventKind::Error {
                                turn: Some(tid.clone()),
                                message: format!("ошибка стрима: {e}"),
                            });
                            step = TurnStep::Finish {
                                outcome: TurnOutcome::Failed,
                            };
                            continue;
                        }
                    };
                    let mut acc = swagcod_core::turn::StreamAccumulator::default();
                    let mut stopped = false;
                    let mut latency_recorded = false;
                    let mut stream_error: Option<String> = None;
                    while let Some(event) = rx.recv().await {
                        // B-8: токен — событие. Долгая генерация reasoning не
                        // должна выглядеть для watchdog как зависание.
                        touch_turn(&app_state, &turn, None);
                        /* E-8: латентность провайдера — время до первого
                        события стрима. Одна точка на запрос, fire-and-
                        forget: телеметрия не должна ломать ход. */
                        if !latency_recorded {
                            latency_recorded = true;
                            let ms = stream_started.elapsed().as_secs_f64() * 1000.0;
                            if let Ok(g) = app_state.store.lock() {
                                let _ = g.metrics_insert(
                                    swagcod_core::bus::now_ms(),
                                    "provider_latency_ms",
                                    ms,
                                );
                            }
                        }
                        if cancelled_in_task.load(std::sync::atomic::Ordering::SeqCst) {
                            stopped = true;
                            handle.abort();
                            break;
                        }
                        match &event {
                            StreamEvent::Reasoning(text) => {
                                acc.reasoning.push_str(text);
                                bus.publish(EventKind::Reasoning {
                                    turn: tid.clone(),
                                    text: text.clone(),
                                });
                            }
                            StreamEvent::Content(text) => {
                                acc.content.push_str(text);
                                bus.publish(EventKind::Content {
                                    turn: tid.clone(),
                                    text: text.clone(),
                                });
                            }
                            StreamEvent::ToolCallStart { id, name, .. } => {
                                bus.publish(EventKind::ToolCallStart {
                                    turn: tid.clone(),
                                    call_id: id.clone(),
                                    name: name.clone(),
                                });
                            }
                            StreamEvent::ToolCallComplete(tc) => {
                                acc.tool_calls.push(tc.clone());
                                bus.publish(EventKind::ToolCall {
                                    turn: tid.clone(),
                                    call_id: tc.id.clone(),
                                    name: tc.name.clone(),
                                    arguments: serde_json::Value::Object(tc.arguments.clone()),
                                });
                            }
                            StreamEvent::Done { .. } => {}
                            StreamEvent::Error(msg) => stream_error = Some(msg.clone()),
                            StreamEvent::Usage {
                                input_tokens,
                                output_tokens,
                            } => {
                                last_usage = Some((*input_tokens, *output_tokens));
                            }
                        }
                    }
                    if stopped {
                        step = machine.cancel();
                    } else if let Some(msg) = stream_error {
                        fail_reason = Some(msg.clone());
                        bus.publish(EventKind::Error {
                            turn: Some(tid.clone()),
                            message: msg,
                        });
                        step = TurnStep::Finish {
                            outcome: TurnOutcome::Failed,
                        };
                    } else {
                        step = machine.on_stream(acc);
                    }
                }
                TurnStep::AwaitApproval { call } => {
                    let call_id = call.id.clone();
                    bus.publish(EventKind::ApprovalRequired {
                        turn: tid.clone(),
                        call_id: call_id.clone(),
                        tool: call.name.clone(),
                        summary: describe_call(&call),
                    });
                    let (tx, rx) = oneshot::channel();
                    {
                        let mut approvals = app_state.approvals.lock().await;
                        // Каналы завершившихся ходов выметаем, чтобы карта не росла.
                        approvals.retain(|_, sender| !sender.is_closed());
                        approvals.insert(call_id.clone(), tx);
                    }
                    // Потеря канала (ход остановлен) считаем отказом: молчаливое
                    // «одобрено» было бы дырой в политике подтверждений.
                    // B-8: на время ожидания человека watchdog замолкает —
                    // диалог подтверждения не зависание.
                    touch_turn(&app_state, &turn, Some(true));
                    let res = rx.await;
                    touch_turn(&app_state, &turn, Some(false));
                    let actor = if res.is_ok() { "user" } else { "system" };
                    let decision = res.unwrap_or(ApprovalDecision::Denied);
                    /* B-7: журнал подтверждений — кто, что, когда. actor=system
                    означает решение без человека (канал потерян). */
                    if let Ok(store) = app_state.store.lock() {
                        let _ = store.log_approval(&swagcod_core::store::ApprovalEntry {
                            session_id: sid.clone(),
                            turn_id: tid.to_string(),
                            call_id: call_id.clone(),
                            tool: call.name.clone(),
                            summary: describe_call(&call),
                            decision: match decision {
                                ApprovalDecision::Approved => "approved",
                                ApprovalDecision::Denied => "denied",
                            }
                            .into(),
                            actor: actor.into(),
                            decided_ms: swagcod_core::bus::now_ms(),
                        });
                    }
                    app_state.approvals.lock().await.remove(&call_id);
                    step = machine.on_approval(decision);
                }
                TurnStep::ExecuteTool { call } => {
                    let started = std::time::Instant::now();
                    /* E-2: semantic_search исполняется здесь, а не в
                    execute_tool — ему нужны AppState (индекс, DPAPI-ключ)
                    и сеть, а execute_tool остаётся чистой и тестируемой.
                    E-3: то же для mcp:* — вызов идёт в живой
                    stdio-процесс сервера из реестра AppState. */
                    let tool_outcome = if call.name == "semantic_search" {
                        run_semantic_search(&app_state, &call, &cwd).await
                    } else if call.name.starts_with(mcp::MCP_PREFIX) {
                        run_mcp_call(&app_state, &call).await
                    } else if call.name.starts_with(jsplugins::JS_PREFIX) {
                        run_js_call(&app_state, &call).await
                    } else if call.name == "subagent" {
                        /* E-6: ветка суб-агента — собственный стрим и история;
                        снимок истории сессии нужен её save_turn (messages
                        заменяются целиком). */
                        run_subagent(
                            &app_state,
                            &provider,
                            &call,
                            &sid,
                            &turn,
                            &effective_model,
                            temperature,
                            &cwd,
                            &summary,
                            tool_timeout,
                            machine.history(),
                        )
                        .await
                    } else {
                        execute_tool(&call, &cwd, tool_timeout, &plugins).await
                    };
                    let elapsed_ms = started.elapsed().as_millis() as u64;
                    bus.publish(EventKind::ToolResult {
                        turn: tid.clone(),
                        call_id: call.id.clone(),
                        ok: tool_outcome.ok,
                        output: truncate_output(&tool_outcome.output),
                        elapsed_ms,
                    });
                    step = machine.on_tool_result(tool_outcome);
                }
                TurnStep::Finish { outcome: o } => break o,
            }
        };

        // TurnEnded публикуется ровно один раз и только из исхода цикла:
        // UI закрывает строки хода по нему и не дедуплирует несколько финалов.
        let reason = match outcome {
            TurnOutcome::Completed => None,
            TurnOutcome::IterationLimit => Some("превышен лимит итераций".to_string()),
            TurnOutcome::Cancelled => Some(
                fail_reason
                    .clone()
                    .unwrap_or_else(|| "остановлено пользователем".to_string()),
            ),
            TurnOutcome::Failed => Some(
                fail_reason
                    .clone()
                    .unwrap_or_else(|| "ошибка хода".to_string()),
            ),
        };
        bus.publish(EventKind::TurnEnded {
            turn: tid.clone(),
            session: swagcod_core::SessionId::new(&sid),
            ok: outcome == TurnOutcome::Completed,
            reason,
        });

        let ended_ms = swagcod_core::bus::now_ms();
        let report = machine.report(outcome, ended_ms);
        let calls: Vec<ToolCall> = machine
            .history()
            .iter()
            .flat_map(|m| m.tool_calls.clone())
            .collect();
        /* B-3: счёт токенов — точный usage от провайдера, когда он его
        отдаёт, иначе калиброванная оценка. Честный ноль в журнале больше
        не живёт. */
        let (est_in, est_out) = match last_usage {
            Some((i, o)) => (i, o),
            None => (
                swagcod_core::context::estimate_history_tokens(machine.history(), cpt),
                swagcod_core::context::estimate_tokens(&report.content, cpt)
                    + swagcod_core::context::estimate_tokens(&report.reasoning, cpt),
            ),
        };
        let record = report.into_record(tid.clone(), calls, est_in, est_out);
        {
            let mut sessions = app_state.sessions.lock().await;
            if let Some(session) = sessions.iter_mut().find(|s| s.id.to_string() == sid) {
                // История сессии — это история машины: assistant с tool_calls
                // всегда закрыт ответами tool, следующий запрос валиден.
                session.history = machine.history().to_vec();
                session.turns.push(record.clone());
                session.status = match outcome {
                    TurnOutcome::Cancelled => swagcod_core::session::SessionStatus::Cancelled,
                    TurnOutcome::Failed => swagcod_core::session::SessionStatus::Failed,
                    _ => swagcod_core::session::SessionStatus::Idle,
                };
                session.current_turn = None;
                /* B-1: ход и снимок истории уходят в store одной транзакцией
                прямо здесь, в конце хода. */
                if let Ok(store) = app_state.store.lock() {
                    if let Err(e) = store.save_turn(&sid, &record, &session.history) {
                        eprintln!("store: ход не записан: {e}");
                    }
                }
                // D-121: сайдбар видит свежие счётчики без полной сессии.
                if let Ok(mut m) = app_state.session_meta.lock() {
                    m.insert(sid.clone(), meta_of(session));
                }
            }
        }

        // Ход закончился — убираем ручку, чтобы «стоп» не бил по прошлому.
        let mut turns = app_state.turns.lock().await;
        turns.remove(&turn);
        // B-8: watchdog больше не следит за мёртвым ходом.
        if let Ok(mut activity) = app_state.turn_activity.lock() {
            activity.remove(&turn);
        }
    });

    // Ручка хода доступна команде stop_turn сразу после старта.
    {
        let mut turns = state.turns.lock().await;
        turns.insert(
            turn_id.clone(),
            TurnHandle {
                session: session_id.clone(),
                cancelled,
                task,
            },
        );
    }

    Ok(turn_id)
}

/// Остановка живого хода: флаг в задачу стрима + аборт HTTP-задачи.
///
/// UI после этого показывает ход как завершённый («остановлено пользователем»)
/// и разблокирует ввод; очередь сообщений продолжает работать.
#[tauri::command]
async fn stop_turn(state: State<'_, Arc<AppState>>, session_id: String) -> Result<(), String> {
    let handle = {
        let mut turns = state.turns.lock().await;
        let victim = turns
            .iter()
            .find(|(_, h)| h.session == session_id)
            .map(|(tid, _)| tid.clone());
        victim.and_then(|tid| turns.remove(&tid))
    };
    let Some(handle) = handle else {
        return Err("нет живого хода у сессии".into());
    };
    handle
        .cancelled
        .store(true, std::sync::atomic::Ordering::SeqCst);
    handle.task.abort();
    Ok(())
}

/// Список файлов и директорий (для файлового дерева UI).
#[derive(Debug, Clone, Serialize)]
pub struct FileEntry {
    pub name: String,
    pub path: String,
    pub is_dir: bool,
}

/// cwd сессии — корень песочницы файловых команд.
///
/// Хардкода пути разработки здесь больше нет: сессия может быть открыта в
/// любом каталоге, и граница песочницы ездит вместе с ней.
async fn session_cwd(
    state: &State<'_, Arc<AppState>>,
    session_id: &str,
) -> Result<std::path::PathBuf, String> {
    let sessions = state.sessions.lock().await;
    sessions
        .iter()
        .find(|s| s.id.to_string() == session_id)
        .map(|s| std::path::PathBuf::from(&s.cwd))
        .ok_or_else(|| format!("сессия не найдена: {session_id}"))
}

#[tauri::command]
async fn list_dir(
    state: State<'_, Arc<AppState>>,
    session_id: String,
    path: String,
) -> Result<Vec<FileEntry>, String> {
    // C4-фикс: песочница — только внутри cwd сессии
    let root = session_cwd(&state, &session_id).await?;
    let dir = std::path::Path::new(&path);
    if !dir.is_dir() {
        return Err(format!("не директория: {path}"));
    }
    if !swagcod_fsx::is_within(&root, dir) {
        return Err("доступ запрещён: путь вне рабочей директории сессии".into());
    }

    let mut entries: Vec<FileEntry> = Vec::new();
    let read_dir = std::fs::read_dir(dir).map_err(|e| format!("read_dir: {e}"))?;

    for entry in read_dir.flatten() {
        let name = entry.file_name().to_string_lossy().to_string();
        // Скрываем служебные файлы (W7: .env содержит ключ — не показываем)
        if name.starts_with('.') {
            continue;
        }
        if name == "target" || name == "node_modules" || name == ".git" {
            continue;
        }
        let is_dir = entry.file_type().map(|t| t.is_dir()).unwrap_or(false);
        entries.push(FileEntry {
            name,
            path: entry.path().to_string_lossy().to_string(),
            is_dir,
        });
    }

    // Сортировка: директории первыми, потом по имени
    entries.sort_by(|a, b| match (a.is_dir, b.is_dir) {
        (true, false) => std::cmp::Ordering::Less,
        (false, true) => std::cmp::Ordering::Greater,
        _ => a.name.to_lowercase().cmp(&b.name.to_lowercase()),
    });

    Ok(entries)
}

/* ── B-4: живая файловая система ──────────────────────────────────────────
 * Watch с debounce 200 мс шлёт FileChanged в шину, fuzzy-поиск по индексу
 * с учётом .gitignore, diff рабочего файла против git HEAD через diff_text. */

#[tauri::command]
async fn start_watch(state: State<'_, Arc<AppState>>, session_id: String) -> Result<(), String> {
    if state.watches.lock().await.contains_key(&session_id) {
        return Ok(());
    }
    let cwd = {
        let sessions = state.sessions.lock().await;
        sessions
            .iter()
            .find(|s| s.id.to_string() == session_id)
            .ok_or_else(|| format!("сессия не найдена: {session_id}"))?
            .cwd
            .clone()
    };
    let (tx, rx) = std::sync::mpsc::channel();
    let handle = swagcod_fsx::watch(std::path::Path::new(&cwd), tx).map_err(|e| e.to_string())?;
    let bus = state.bus.clone();
    let sid = session_id.clone();
    std::thread::Builder::new()
        .name(format!("fsx-events-{session_id}"))
        .spawn(move || {
            while let Ok(paths) = rx.recv() {
                let paths: Vec<String> = paths
                    .iter()
                    .take(50)
                    .map(|p| p.to_string_lossy().replace('\\', "/"))
                    .collect();
                bus.publish(EventKind::FileChanged {
                    session: swagcod_core::SessionId::new(&sid),
                    paths,
                });
            }
        })
        .map_err(|e| e.to_string())?;
    state.watches.lock().await.insert(session_id, handle);
    Ok(())
}

#[tauri::command]
async fn stop_watch(state: State<'_, Arc<AppState>>, session_id: String) -> Result<(), String> {
    state.watches.lock().await.remove(&session_id);
    Ok(())
}

#[tauri::command]
async fn search_files(
    state: State<'_, Arc<AppState>>,
    session_id: String,
    query: String,
    limit: Option<usize>,
) -> Result<Vec<String>, String> {
    let cwd = {
        let sessions = state.sessions.lock().await;
        sessions
            .iter()
            .find(|s| s.id.to_string() == session_id)
            .ok_or_else(|| format!("сессия не найдена: {session_id}"))?
            .cwd
            .clone()
    };
    let limit = limit.unwrap_or(50);
    // Кэш 5 с: debounce ввода на фронте плюс свежий индекс после FileChanged.
    let cached = state.indexes.lock().ok().and_then(|m| {
        m.get(&session_id)
            .filter(|(t, _)| t.elapsed().as_secs() < 5)
            .map(|(_, i)| i.clone())
    });
    let idx = match cached {
        Some(i) => i,
        None => {
            let built = std::sync::Arc::new(
                swagcod_fsx::FileIndex::build(std::path::Path::new(&cwd))
                    .map_err(|e| e.to_string())?,
            );
            if let Ok(mut m) = state.indexes.lock() {
                m.insert(
                    session_id.clone(),
                    (std::time::Instant::now(), built.clone()),
                );
            }
            built
        }
    };
    Ok(idx.search(&query, limit))
}

/// Ответ diff против git HEAD: сам diff плюс факт существования файла в HEAD.
#[derive(Serialize)]
struct DiffView {
    head_exists: bool,
    added: u32,
    removed: u32,
    lines: Vec<swagcod_fsx::DiffLine>,
}

#[tauri::command]
async fn diff_against_head(
    state: State<'_, Arc<AppState>>,
    session_id: String,
    path: String,
) -> Result<DiffView, String> {
    let cwd = {
        let sessions = state.sessions.lock().await;
        sessions
            .iter()
            .find(|s| s.id.to_string() == session_id)
            .ok_or_else(|| format!("сессия не найдена: {session_id}"))?
            .cwd
            .clone()
    };
    let cwd_path = std::path::PathBuf::from(&cwd);
    let abs = sandbox_path(&cwd_path, &path)?;
    let work = std::fs::read_to_string(&abs).map_err(|e| format!("рабочий файл: {e}"))?;
    let rel = abs
        .strip_prefix(&cwd_path)
        .map_err(|e| e.to_string())?
        .to_string_lossy()
        .replace('\\', "/");
    // git show HEAD:<rel>: не-репозиторий или новый файл — не ошибка,
    // а пустая левая сторона diff.
    let head_out = tokio::time::timeout(
        std::time::Duration::from_secs(10),
        silent_cmd("git")
            .args(["show", &format!("HEAD:{rel}")])
            .current_dir(&cwd_path)
            .output(),
    )
    .await;
    let (head_exists, head) = match head_out {
        Ok(Ok(o)) if o.status.success() => (true, String::from_utf8_lossy(&o.stdout).into_owned()),
        _ => (false, String::new()),
    };
    let d = swagcod_fsx::diff_text(&head, &work);
    Ok(DiffView {
        head_exists,
        added: d.added,
        removed: d.removed,
        lines: d.lines,
    })
}

/* B-5: слой плагинов. Регистрация внешнего инструмента: spec для модели
плюс команда для исполнения. Имя не должно пересекаться со встроенными:
иначе плагин смог бы затенить read/write и обойти их семантику. */

#[tauri::command]
fn register_plugin_tool(
    state: State<'_, Arc<AppState>>,
    name: String,
    description: String,
    parameters_json: String,
    command: String,
) -> Result<(), String> {
    let name = name.trim().to_string();
    if name.is_empty() || command.trim().is_empty() {
        return Err("имя и команда обязательны".into());
    }
    if ApprovalPolicy::BUILTIN
        .iter()
        .any(|b| name.eq_ignore_ascii_case(b))
    {
        return Err(format!("имя {name} занято встроенным инструментом"));
    }
    let parameters: serde_json::Value = if parameters_json.trim().is_empty() {
        serde_json::json!({ "type": "object", "properties": {} })
    } else {
        serde_json::from_str(&parameters_json).map_err(|e| format!("parameters: {e}"))?
    };
    if !parameters.is_object() {
        return Err("parameters должны быть JSON-объектом".into());
    }
    let tool = PluginTool {
        name: name.clone(),
        description,
        parameters,
        command,
    };
    let mut plugins = state.plugins.lock().map_err(|e| e.to_string())?;
    match plugins.iter_mut().find(|p| p.name == name) {
        Some(slot) => *slot = tool,
        None => plugins.push(tool),
    }
    Ok(())
}

#[tauri::command]
fn list_plugin_tools(state: State<'_, Arc<AppState>>) -> Result<Vec<PluginTool>, String> {
    Ok(state.plugins.lock().map_err(|e| e.to_string())?.clone())
}

/* ── Исполнитель тулзов агента ────────────────────────────────────────────
 * Песочница — cwd сессии: модель не читает и не пишет вне рабочей
 * директории, даже если человек подтвердил вызов. Оболочка достигается
 * только тулзом `bash`, который при on_dangerous всегда уходит на
 * подтверждение (имя в списке ApprovalPolicy::DANGEROUS).
 * Команды `run_command` в IPC больше нет: шелл из WebView был дырой,
 * а агентскому циклу хватает этого исполнителя. */

/// Резолвить путь внутри cwd сессии; всё снаружи отклоняется.
fn sandbox_path(cwd: &std::path::Path, raw: &str) -> Result<std::path::PathBuf, String> {
    let p = std::path::Path::new(raw);
    let abs = if p.is_absolute() {
        p.to_path_buf()
    } else {
        cwd.join(p)
    };
    if !swagcod_fsx::is_within(cwd, &abs) {
        return Err(format!("путь вне рабочей директории сессии: {raw}"));
    }
    Ok(abs)
}

fn arg_str(call: &ToolCall, key: &str) -> String {
    call.arguments
        .get(key)
        .and_then(|v| v.as_str())
        .unwrap_or_default()
        .to_string()
}

/* ===================== E-2: семантический поиск по кодовой базе ===================== */

/// Кэш эмбеддингов живёт рядом с основной базой: embeddings.db.
/// Это перестраиваемый кэш, а не доменные данные (D-113) — отдельный
/// файл на rusqlite, без stdio-налога на десятки тысяч векторов.
/// `SWAGCOD_SEMANTIC_DB` переопределяет путь (тесты, переносимость).
fn semantic_path() -> Result<std::path::PathBuf, String> {
    if let Ok(p) = std::env::var("SWAGCOD_SEMANTIC_DB") {
        if !p.trim().is_empty() {
            return Ok(std::path::PathBuf::from(p));
        }
    }
    let base = std::env::var("LOCALAPPDATA")
        .or_else(|_| std::env::var("HOME"))
        .map_err(|_| "не удалось определить локальный профиль".to_string())?;
    Ok(std::path::PathBuf::from(base)
        .join("SwagCod")
        .join("embeddings.db"))
}

/// Лениво открыть индекс и применить синхронную операцию.
/// Guard не должен жить через await: поиск/запись миллисекундные.
fn with_semantic<R>(
    state: &AppState,
    f: impl FnOnce(&swagcod_core::semantic::SemanticIndex) -> R,
) -> Result<R, String> {
    let mut guard = state
        .semantic
        .lock()
        .map_err(|e| format!("semantic: {e}"))?;
    if guard.is_none() {
        let path = semantic_path()?;
        *guard = Some(
            swagcod_core::semantic::SemanticIndex::open(&path)
                .map_err(|e| format!("semantic index: {e}"))?,
        );
    }
    Ok(f(guard.as_ref().expect("индекс только что установлен")))
}

/// Текстовые расширения, которые стоит эмбеддить. Всё остальное
/// (бинарщина, lock-файлы, картинки) индексатор пропускает.
const SEMANTIC_EXTENSIONS: &[&str] = &[
    "rs", "ts", "tsx", "js", "jsx", "svelte", "json", "md", "toml", "yaml", "yml", "html", "css",
    "scss", "py", "go", "c", "h", "cpp", "hpp", "cs", "java", "kt", "rb", "php", "sql", "sh",
    "ps1", "bat", "txt", "xml", "ini", "conf", "env",
];

/// Потолок размера файла для эмбеддингов: 512 КБ текста — это ~120 чанков,
/// дальше стоимость не окупает ценность.
const MAX_SEMANTIC_FILE_BYTES: u64 = 512 * 1024;
/// Потолок длины чанка в символах для запроса эмбеддингов.
const MAX_SEMANTIC_CHUNK_CHARS: usize = 4000;
/// Файлов на один запрос `/v1/embeddings`.
const SEMANTIC_EMBED_BATCH: usize = 16;
/// Кулдаун повторной индексации: ходы частые, обход репозитория недешёв.
const SEMANTIC_REINDEX_COOLDOWN_MS: u64 = 5 * 60 * 1000;

fn is_semantic_candidate(rel: &str) -> bool {
    match rel.rsplit_once('.') {
        Some((_, ext)) => {
            let e = ext.to_ascii_lowercase();
            SEMANTIC_EXTENSIONS.iter().any(|x| *x == e)
        }
        None => false,
    }
}

/// Запустить фоновую индексацию, если она не идёт и кулдаун истёк
/// (или `force`). Возвращает true, когда задача действительно поднята.
fn spawn_semantic_index(state: Arc<AppState>, cwd: std::path::PathBuf, force: bool) -> bool {
    use std::sync::atomic::Ordering;
    let now = swagcod_core::bus::now_ms();
    let last = state.semantic_last_ms.load(Ordering::Relaxed);
    if !force && now.saturating_sub(last) < SEMANTIC_REINDEX_COOLDOWN_MS {
        return false;
    }
    if state.semantic_busy.swap(true, Ordering::SeqCst) {
        return false;
    }
    state.semantic_last_ms.store(now, Ordering::Relaxed);
    tauri::async_runtime::spawn(async move {
        match index_workspace(&state, &cwd).await {
            Ok(n) if n > 0 => eprintln!("semantic: проиндексировано файлов: {n} ({cwd:?})"),
            Ok(_) => {}
            Err(e) => eprintln!("semantic: индексация {cwd:?}: {e}"),
        }
        state.semantic_busy.store(false, Ordering::SeqCst);
    });
    true
}

/// Обход рабочей директории (FileIndex уважает .gitignore) → текстовые
/// файлы ≤ 512 КБ → чанки по 40 строк → эмбеддинги → upsert в индекс.
/// Вкус эмбеддингов (E-2): `cloud:<model>` через Router `/v1/embeddings`
/// или `local:v1` — локальный лексический feature-hashing, когда у
/// провайдера эмбеддингов нет (живой rustvy.xyz: 13 моделей, все чатовые).
/// Режим — `SWAGCOD_EMBEDDINGS_MODE`: auto (default) | cloud | local.
/// Auto-фолбэк в локальный — громкий (eprintln), а смена маркера
/// пространства стирает старый кэш: вектора разных эмбеддеров несравнимы.
async fn index_workspace(state: &Arc<AppState>, cwd: &std::path::Path) -> Result<usize, String> {
    use swagcod_core::semantic::{chunk_text, content_hash, local_embedding, Chunk};

    let mode = std::env::var("SWAGCOD_EMBEDDINGS_MODE")
        .unwrap_or_else(|_| "auto".into())
        .to_ascii_lowercase();
    let mut router: Option<Router> = None;
    let flavor = match mode.as_str() {
        "local" => "local:v1".to_string(),
        "cloud" => {
            let r = Router::from_env_with_key(dpapi_key(state))
                .map_err(|e| format!("эмбеддинги (cloud): {e}"))?;
            // Проба обязательна: cloud-режим без эмбеддингов — громкая
            // ошибка конфигурации, никакого тихого местного суррогата.
            r.embeddings(&["swagcod probe".to_string()])
                .await
                .map_err(|e| format!("эмбеддинги (cloud): {e}"))?;
            router = Some(r);
            format!("cloud:{}", swagcod_provider::embeddings_model())
        }
        _ => match Router::from_env_with_key(dpapi_key(state)) {
            Ok(r) => match r.embeddings(&["swagcod probe".to_string()]).await {
                Ok(_) => {
                    router = Some(r);
                    format!("cloud:{}", swagcod_provider::embeddings_model())
                }
                Err(e) => {
                    eprintln!("semantic: /v1/embeddings недоступен ({e}) — локальный лексический эмбеддер");
                    "local:v1".to_string()
                }
            },
            Err(e) => {
                eprintln!("semantic: роутер эмбеддингов не собрать ({e}) — локальный лексический эмбеддер");
                "local:v1".to_string()
            }
        },
    };

    // Смена пространства — полная перестройка; иначе берём snapshot хэшей.
    let stored = with_semantic(state, |ix| -> Result<_, String> {
        let prev = ix.embedder().map_err(|e| e.to_string())?;
        if prev.as_deref() != Some(flavor.as_str()) {
            ix.clear_all().map_err(|e| e.to_string())?;
            ix.set_embedder(&flavor).map_err(|e| e.to_string())?;
        }
        ix.file_hashes().map_err(|e| e.to_string())
    })??;

    let idx = swagcod_fsx::FileIndex::build(cwd).map_err(|e| format!("индекс файлов: {e}"))?;
    let mut present: std::collections::HashSet<String> = std::collections::HashSet::new();
    let mut todo: Vec<(String, String, String)> = Vec::new(); // (rel, hash, text)
    for rel in &idx.files {
        if !is_semantic_candidate(rel) {
            continue;
        }
        present.insert(rel.clone());
        let abs = cwd.join(rel);
        match std::fs::metadata(&abs) {
            Ok(m) if m.len() <= MAX_SEMANTIC_FILE_BYTES => {}
            _ => continue,
        }
        let text = match std::fs::read_to_string(&abs) {
            Ok(t) => t,
            Err(_) => continue, // бинарщина под текстовым расширением — пропускаем
        };
        let hash = content_hash(&text);
        if stored.get(rel).map(|s| s.as_str()) == Some(hash.as_str()) {
            continue;
        }
        todo.push((rel.clone(), hash, text));
    }

    let mut indexed = 0usize;

    // Локальный вкус: сети нет, всё считается на месте.
    if router.is_none() {
        for (rel, hash, text) in &todo {
            let items: Vec<(Chunk, Vec<f32>)> = chunk_text(text)
                .into_iter()
                .map(|c| {
                    let t: String = c.text.chars().take(MAX_SEMANTIC_CHUNK_CHARS).collect();
                    (c, local_embedding(&t))
                })
                .collect();
            with_semantic(state, |ix| {
                ix.upsert_file(rel, hash, &items).map_err(|e| e.to_string())
            })??;
            indexed += 1;
            if indexed % 64 == 0 {
                // Индексация фоновая, но event loop не должен голодать.
                tokio::task::yield_now().await;
            }
        }
        return finish_indexing(state, &present, indexed);
    }

    // Облачный вкус: батчи через Router (фолбэк-цепочка B-6 работает).
    let router = router.expect("проверено выше");
    for batch in todo.chunks(SEMANTIC_EMBED_BATCH) {
        let mut per_file: Vec<(&String, &String, Vec<Chunk>)> = Vec::new();
        let mut texts: Vec<String> = Vec::new();
        let mut counts: Vec<usize> = Vec::new();
        for (rel, hash, text) in batch {
            let chunks = chunk_text(text);
            if chunks.is_empty() {
                // Пустой файл: хэш всё равно запоминаем, чтобы не перечитывать.
                let _ = with_semantic(state, |ix| {
                    ix.upsert_file(rel, hash, &[]).map_err(|e| e.to_string())
                });
                indexed += 1;
                continue;
            }
            counts.push(chunks.len());
            for c in &chunks {
                texts.push(c.text.chars().take(MAX_SEMANTIC_CHUNK_CHARS).collect());
            }
            per_file.push((rel, hash, chunks));
        }
        if texts.is_empty() {
            continue;
        }
        let vecs = router
            .embeddings(&texts)
            .await
            .map_err(|e| format!("эмбеддинги: {e}"))?;
        if vecs.len() != texts.len() {
            return Err(format!(
                "эмбеддинги: вернули {} векторов на {} запросов",
                vecs.len(),
                texts.len()
            ));
        }
        let mut offset = 0usize;
        for ((rel, hash, chunks), count) in per_file.iter().zip(counts.iter()) {
            let items: Vec<(Chunk, Vec<f32>)> = chunks
                .iter()
                .cloned()
                .zip(vecs[offset..offset + count].to_vec())
                .collect();
            offset += count;
            with_semantic(state, |ix| {
                ix.upsert_file(rel, hash, &items).map_err(|e| e.to_string())
            })??;
            indexed += 1;
        }
        tokio::task::yield_now().await;
    }
    finish_indexing(state, &present, indexed)
}

/// Хвост индексации: вычистить пропавшие файлы, отрапортовать.
fn finish_indexing(
    state: &Arc<AppState>,
    present: &std::collections::HashSet<String>,
    indexed: usize,
) -> Result<usize, String> {
    let removed = with_semantic(state, |ix| {
        ix.prune_missing(present).map_err(|e| e.to_string())
    })
    .unwrap_or(Ok(0))
    .unwrap_or(0);
    if removed > 0 {
        eprintln!("semantic: вычищено устаревших файлов: {removed}");
    }
    Ok(indexed)
}

/// Инструмент агента `semantic_search`: запрос → эмбеддинг → косинус-поиск
/// по индексу → топ-k фрагментов с путями, строками и оценкой. Обновление
/// индекса запускается фоном и ответ не блокирует: поиск идёт по тому,
/// что уже есть. Пустой индекс — честный ответ «повторите позже».
async fn run_semantic_search(
    state: &Arc<AppState>,
    call: &ToolCall,
    cwd: &std::path::Path,
) -> ToolOutcome {
    let fail = |output: String| ToolOutcome { ok: false, output };
    let query = arg_str(call, "query");
    if query.trim().is_empty() {
        return fail("semantic_search: пустой запрос".into());
    }
    let top_k = call
        .arguments
        .get("top_k")
        .and_then(|v| v.as_u64())
        .unwrap_or(8)
        .clamp(1, 50) as usize;

    let stats = match with_semantic(state, |ix| ix.stats().map_err(|e| e.to_string())) {
        Ok(Ok(s)) => s,
        Ok(Err(e)) | Err(e) => return fail(format!("semantic_search: {e}")),
    };
    spawn_semantic_index(state.clone(), cwd.to_path_buf(), false);
    if stats.1 == 0 {
        return fail(
            "семантический индекс пуст — запущена фоновая индексация; повторите вызов через минуту-другую".into(),
        );
    }

    // E-2: запрос эмбеддится в ТОМ ЖЕ пространстве, что и индекс.
    // Маркер хранится в базе: cloud:<model> → Router /v1/embeddings,
    // local:v1 → локальный лексический эмбеддер. Смешивать нельзя.
    let kind = match with_semantic(state, |ix| ix.embedder().map_err(|e| e.to_string())) {
        Ok(Ok(Some(k))) => k,
        Ok(Ok(None)) => {
            return fail("семантический индекс без маркера пространства — индексация пересоберёт его; повторите позже".into())
        }
        Ok(Err(e)) | Err(e) => return fail(format!("semantic_search: {e}")),
    };
    let qv = if kind.starts_with("cloud:") {
        let router = match Router::from_env_with_key(dpapi_key(state)) {
            Ok(r) => r,
            Err(e) => return fail(format!("semantic_search: {e}")),
        };
        match router.embeddings(std::slice::from_ref(&query)).await {
            Ok(mut v) if !v.is_empty() => v.remove(0),
            Ok(_) => return fail("semantic_search: эмбеддинги вернули пустоту".into()),
            Err(e) => return fail(format!("semantic_search: эмбеддинги: {e}")),
        }
    } else {
        swagcod_core::semantic::local_embedding(&query)
    };
    let hits = match with_semantic(state, |ix| ix.search(&qv, top_k).map_err(|e| e.to_string())) {
        Ok(Ok(h)) => h,
        Ok(Err(e)) | Err(e) => return fail(format!("semantic_search: {e}")),
    };
    if hits.is_empty() {
        return ToolOutcome {
            ok: true,
            output: format!(
                "по запросу «{query}» ничего не найдено (индекс: {} файлов, {} чанков{})",
                stats.0,
                stats.1,
                if kind.starts_with("local:") {
                    ", лексический"
                } else {
                    ""
                }
            ),
        };
    }

    let mut out = String::new();
    if kind.starts_with("local:") {
        out.push_str(
            "# индекс лексический (локальный эмбеддер): ищет совпадение терминов, не смысл; \
             для настоящей семантики нужен эндпоинт /v1/embeddings (SWAGCOD_EMBEDDINGS_MODE/MODEL)\n",
        );
    }
    for h in &hits {
        out.push_str(&format!(
            "{}:{}-{} (близость {:.2})\n",
            h.path, h.start, h.end, h.score
        ));
        if let Ok(text) = std::fs::read_to_string(cwd.join(&h.path)) {
            out.push_str("```\n");
            for line in text
                .lines()
                .skip((h.start as usize).saturating_sub(1))
                .take((h.end - h.start + 1) as usize)
                .take(12)
            {
                let trimmed: String = line.chars().take(160).collect();
                out.push_str(&trimmed);
                out.push('\n');
            }
            out.push_str("```\n");
        }
    }
    ToolOutcome {
        ok: true,
        output: out,
    }
}

/* ── E-6: суб-агент — изолированная ветка дерева ходов ─────────────────────
 * Модель вызывает `subagent` как обычный инструмент, но за ним —
 * миниатюрная машина хода: собственный стрим провайдера, собственная
 * история (НЕ история сессии), только read-only инструменты, бюджет
 * оценочных токенов и предел раундов. Прогон записывается дочерним ходом
 * с `parent_turn_id` — ветки видны в траектории, чате и транскрипте.
 * Суб-агент не рекурсивен: в его списке спеков нет `subagent`, а вызовы
 * исполняются напрямую через execute_tool без диспетчера вложенности. */

/// Инструменты внутри суб-агента: только read-only. write/bash/patch не
/// входят, поэтому ветка физически не способна на опасные операции —
/// подтверждений из суб-агента не бывает ни при какой политике.
pub const SUBAGENT_SAFE_TOOLS: &[&str] = &[
    "read",
    "list",
    "grep",
    "glob",
    "fetch_url",
    "semantic_search",
];

/// Предел раундов (запросов модели) ветки по умолчанию.
pub const SUBAGENT_MAX_ROUNDS: u32 = 6;

/// Бюджет оценочных токенов ветки (вход+выход всех раундов) по умолчанию.
pub const SUBAGENT_BUDGET_TOKENS: u64 = 100_000;

/// Лимиты ветки: аргументы вызова (max_rounds/budget_tokens), затем env,
/// затем дефолты. Клампы защищают и от нуля, и от абсурдных значений.
pub fn subagent_limits(args: &serde_json::Map<String, serde_json::Value>) -> (u32, u64) {
    let env_rounds = std::env::var("SWAGCOD_SUBAGENT_MAX_ROUNDS")
        .ok()
        .and_then(|v| v.parse::<u32>().ok());
    let env_budget = std::env::var("SWAGCOD_SUBAGENT_BUDGET_TOKENS")
        .ok()
        .and_then(|v| v.parse::<u64>().ok());
    let rounds = args
        .get("max_rounds")
        .and_then(|v| v.as_u64())
        .map(|v| v as u32)
        .or(env_rounds)
        .unwrap_or(SUBAGENT_MAX_ROUNDS)
        .clamp(1, 12);
    let budget = args
        .get("budget_tokens")
        .and_then(|v| v.as_u64())
        .or(env_budget)
        .unwrap_or(SUBAGENT_BUDGET_TOKENS)
        .clamp(1_000, 10_000_000);
    (rounds, budget)
}

/// Модель ветки: аргумент `model`, иначе модель родительского хода.
/// Пустая/пробельная строка — тоже родитель (модель могла прийти из
/// шаблона с незаполненным плейсхолдером). Несуществующую модель честно
/// отвергнет сам провайдер — выдумывать проверку по списку здесь нечем.
pub fn subagent_model(args: &serde_json::Map<String, serde_json::Value>, parent: &str) -> String {
    args.get("model")
        .and_then(|v| v.as_str())
        .map(str::trim)
        .filter(|m| !m.is_empty())
        .unwrap_or(parent)
        .to_string()
}

/// Прогнать ветку суб-агента: изолированный диалог с моделью, read-only
/// инструменты, дочерняя запись хода. `session_history` — снимок истории
/// СЕССИИ для save_turn: messages заменяются целиком, поэтому изолированную
/// историю ветки туда писать нельзя (затрёт wire-историю родителя).
#[allow(clippy::too_many_arguments)]
async fn run_subagent(
    state: &Arc<AppState>,
    provider: &AnyProvider,
    call: &ToolCall,
    sid: &str,
    parent_tid: &str,
    model: &str,
    temperature: Option<f32>,
    cwd: &std::path::Path,
    summary: &str,
    tool_timeout: std::time::Duration,
    session_history: &[ChatMessage],
) -> ToolOutcome {
    use swagcod_core::turn::{build_assistant_message, StreamAccumulator};
    use swagcod_provider::types::Role;

    let prompt = call
        .arguments
        .get("prompt")
        .and_then(|v| v.as_str())
        .unwrap_or("")
        .trim()
        .to_string();
    if prompt.is_empty() {
        return ToolOutcome {
            ok: false,
            output: "subagent: пустой prompt в аргументах".into(),
        };
    }
    let with_context = call
        .arguments
        .get("context")
        .and_then(|v| v.as_bool())
        .unwrap_or(false);
    let (max_rounds, budget) = subagent_limits(&call.arguments);
    // Ветка может ехать на своей модели (дешёвый флеш для bulk-поиска),
    // по умолчанию — модель родительского хода.
    let model = subagent_model(&call.arguments, model);
    // turns.id — глобальный ключ: id ветки = id родителя + уникальный хвост.
    let child_id = format!("{parent_tid}-sub-{}", short_id());
    let child_tid = swagcod_core::TurnId::new(&child_id);
    let parent_turn = swagcod_core::TurnId::new(parent_tid);
    let started = swagcod_core::bus::now_ms();
    let bus = state.bus.clone();

    /* TurnStarted ветки уходит в шину первым — фронт строит карту
    turn→session и для дочерних ходов, иначе стрим ветки потерялся бы. */
    bus.publish(EventKind::TurnStarted {
        turn: child_tid.clone(),
        session: swagcod_core::SessionId::new(sid),
        parent: Some(parent_turn.clone()),
    });

    // Изолированная история: свежий контекст, сводка B-3 — по желанию.
    let mut history: Vec<ChatMessage> = Vec::new();
    if with_context && !summary.trim().is_empty() {
        history.push(ChatMessage::user(format!(
            "Контекст сессии (сводка):\n{}\n\nЗадача: {prompt}",
            summary.trim()
        )));
    } else {
        history.push(ChatMessage::user(prompt));
    }
    let system = ChatMessage {
        role: Role::System,
        content: "Ты — суб-агент внутри SwagCod: решаешь одну самодостаточную исследовательскую задачу. \
                  У тебя только read-only инструменты (read, list, grep, glob, fetch_url, semantic_search). \
                  Ты не можешь изменять файлы, выполнять команды и плодить собственных суб-агентов. \
                  Когда задача решена — ответь итоговым отчётом обычным текстом, без вызовов инструментов."
            .into(),
        reasoning: String::new(),
        tool_calls: Vec::new(),
        tool_call_id: None,
    };
    let specs: Vec<ToolSpec> = builtin_tool_specs()
        .into_iter()
        .filter(|s| SUBAGENT_SAFE_TOOLS.contains(&s.name.as_str()))
        .collect();
    let tool_wire: Vec<serde_json::Value> = ChatRequest::new(&model, Vec::new())
        .with_tools(&specs)
        .tools;
    let cpt = swagcod_core::context::chars_per_token(&model, None);

    let mut est_in: u64 = 0;
    let mut est_out: u64 = 0;
    let mut final_content = String::new();
    let mut all_reasoning = String::new();
    let mut all_tools: Vec<ToolCall> = Vec::new();
    let mut ok = true;
    let mut failure: Option<String> = None;
    let mut clean_finish = false;
    let mut rounds_used: u32 = 0;

    for round in 0..max_rounds {
        rounds_used = round + 1;
        // B-8: ветка живёт внутри родительского хода — кормим watchdog.
        touch_turn(state, parent_tid, None);
        let mut messages = Vec::with_capacity(history.len() + 1);
        messages.push(system.clone());
        messages.extend(history.iter().cloned());
        est_in += swagcod_core::context::estimate_history_tokens(&messages, cpt) as u64;
        let request = ChatRequest {
            model: model.to_string(),
            messages,
            stream: true,
            temperature,
            max_tokens: None,
            tools: tool_wire.clone(),
        };
        let (mut rx, handle) = match provider.stream(request) {
            Ok(pair) => pair,
            Err(e) => {
                ok = false;
                failure = Some(format!("subagent: ошибка стрима: {e}"));
                break;
            }
        };
        let mut acc = StreamAccumulator::default();
        let mut stream_error: Option<String> = None;
        while let Some(event) = rx.recv().await {
            touch_turn(state, parent_tid, None);
            match &event {
                StreamEvent::Reasoning(t) => {
                    acc.reasoning.push_str(t);
                    bus.publish(EventKind::Reasoning {
                        turn: child_tid.clone(),
                        text: t.clone(),
                    });
                }
                StreamEvent::Content(t) => {
                    acc.content.push_str(t);
                    bus.publish(EventKind::Content {
                        turn: child_tid.clone(),
                        text: t.clone(),
                    });
                }
                StreamEvent::ToolCallComplete(tc) => {
                    acc.tool_calls.push(tc.clone());
                    bus.publish(EventKind::ToolCall {
                        turn: child_tid.clone(),
                        call_id: tc.id.clone(),
                        name: tc.name.clone(),
                        arguments: serde_json::Value::Object(tc.arguments.clone()),
                    });
                }
                StreamEvent::Error(m) => stream_error = Some(m.clone()),
                _ => {}
            }
        }
        handle.abort();
        if let Some(m) = stream_error {
            ok = false;
            failure = Some(format!("subagent: {m}"));
            final_content = acc.content;
            break;
        }
        est_out += swagcod_core::context::estimate_tokens(&acc.content, cpt) as u64
            + swagcod_core::context::estimate_tokens(&acc.reasoning, cpt) as u64;
        all_reasoning.push_str(&acc.reasoning);
        history.push(build_assistant_message(&acc));
        if acc.tool_calls.is_empty() {
            final_content = acc.content;
            clean_finish = true;
            break;
        }
        final_content = acc.content.clone();
        for tc in &acc.tool_calls {
            all_tools.push(tc.clone());
            /* Защита в глубину: даже если модель «выдумала» опасный инструмент,
            ветка его не исполнит — честный отказ вместо исполнения. */
            let out = if tc.name == "semantic_search" {
                run_semantic_search(state, tc, cwd).await
            } else if SUBAGENT_SAFE_TOOLS.contains(&tc.name.as_str()) {
                execute_tool(tc, cwd, tool_timeout, &[]).await
            } else {
                ToolOutcome {
                    ok: false,
                    output: format!("subagent: инструмент недоступен в суб-агенте: {}", tc.name),
                }
            };
            bus.publish(EventKind::ToolResult {
                turn: child_tid.clone(),
                call_id: tc.id.clone(),
                ok: out.ok,
                output: truncate_output(&out.output),
                elapsed_ms: 0,
            });
            history.push(ChatMessage {
                role: Role::Tool,
                content: truncate_output(&out.output),
                reasoning: String::new(),
                tool_calls: Vec::new(),
                tool_call_id: Some(tc.id.clone()),
            });
        }
        if est_in + est_out >= budget {
            ok = false;
            failure = Some(format!(
                "subagent: бюджет токенов ветки исчерпан ({} ≥ {})",
                est_in + est_out,
                budget
            ));
            break;
        }
        bus.publish(EventKind::Status {
            message: format!(
                "суб-агент {child_id}: раунд {rounds_used}/{max_rounds}, инструментов {}",
                all_tools.len()
            ),
        });
    }
    if !clean_finish && failure.is_none() {
        ok = false;
        failure = Some(format!("subagent: достигнут предел раундов ({max_rounds})"));
    }

    let ended = swagcod_core::bus::now_ms();
    let child = TurnRecord {
        id: child_tid.clone(),
        started_ms: started,
        ended_ms: Some(ended),
        content: final_content.clone(),
        reasoning: all_reasoning,
        tool_calls: all_tools.clone(),
        est_input_tokens: est_in.min(u32::MAX as u64) as u32,
        est_output_tokens: est_out.min(u32::MAX as u64) as u32,
        ok,
        failure: failure.clone(),
        parent_turn_id: Some(parent_turn),
    };
    /* Дочерний ход — в стор и в живую сессию: ветка видна в траектории
    сразу, не дожидаясь конца родительского хода. */
    if let Ok(store) = state.store.lock() {
        let _ = store.save_turn(sid, &child, session_history);
    }
    {
        let mut sessions = state.sessions.lock().await;
        if let Some(s) = sessions.iter_mut().find(|s| s.id.as_str() == sid) {
            s.turns.push(child);
        }
    }
    bus.publish(EventKind::TurnEnded {
        turn: child_tid,
        session: swagcod_core::SessionId::new(sid),
        ok,
        reason: failure.clone(),
    });

    // Отчёт родительскому ходу: текст ветки + честная статистика.
    let mut output = format!(
        "[суб-агент {child_id} · {model}] раундов: {rounds_used}, инструментов: {}, токенов (оценка): {est_in} in / {est_out} out\n",
        all_tools.len()
    );
    if final_content.trim().is_empty() {
        output.push_str("(пустой ответ)");
    } else {
        output.push_str(final_content.trim());
    }
    if let Some(f) = &failure {
        output.push_str(&format!("\n[ветка не завершена: {f}]"));
    }
    ToolOutcome { ok, output }
}

/// Выполнить один вызов. Не паникует и не возвращает Err: любой отказ
/// становится результатом ok=false, и модель видит его как ответ тулза.
/// `plugins` — зарегистрированные внешние инструменты (B-5): неизвестное
/// встроенное имя ищется там и исполняется отдельной командой.
/// E-2: `semantic_search` исполняется НЕ здесь, а в `run_semantic_search`
/// (нужны AppState и сеть); диспетчер хода ветвит по имени до вызова.
async fn execute_tool(
    call: &ToolCall,
    cwd: &std::path::Path,
    timeout: std::time::Duration,
    plugins: &[PluginTool],
) -> ToolOutcome {
    let fail = |output: String| ToolOutcome { ok: false, output };
    match call.name.as_str() {
        "read" => {
            let path = match sandbox_path(cwd, &arg_str(call, "path")) {
                Ok(p) => p,
                Err(e) => return fail(e),
            };
            let meta = match std::fs::metadata(&path) {
                Ok(m) => m,
                Err(e) => return fail(format!("read: {e}")),
            };
            if meta.len() > 1_048_576 {
                return fail("файл больше 1 МБ, читайте частями".into());
            }
            match std::fs::read_to_string(&path) {
                Ok(s) => ToolOutcome {
                    ok: true,
                    output: s,
                },
                Err(e) => fail(format!("read: {e}")),
            }
        }
        "list" => {
            let path = match sandbox_path(cwd, &arg_str(call, "path")) {
                Ok(p) => p,
                Err(e) => return fail(e),
            };
            let rd = match std::fs::read_dir(&path) {
                Ok(rd) => rd,
                Err(e) => return fail(format!("list: {e}")),
            };
            let mut lines: Vec<String> = Vec::new();
            for entry in rd.flatten() {
                let is_dir = entry.file_type().map(|t| t.is_dir()).unwrap_or(false);
                lines.push(format!(
                    "{}{}",
                    if is_dir { "d " } else { "  " },
                    entry.file_name().to_string_lossy()
                ));
            }
            lines.sort();
            ToolOutcome {
                ok: true,
                output: lines.join("\n"),
            }
        }
        "write" => {
            let path = match sandbox_path(cwd, &arg_str(call, "path")) {
                Ok(p) => p,
                Err(e) => return fail(e),
            };
            let content = arg_str(call, "content");
            if let Some(parent) = path.parent() {
                if let Err(e) = std::fs::create_dir_all(parent) {
                    return fail(format!("mkdir: {e}"));
                }
            }
            match std::fs::write(&path, content.as_bytes()) {
                Ok(()) => ToolOutcome {
                    ok: true,
                    output: format!("записано {} байт", content.len()),
                },
                Err(e) => fail(format!("write: {e}")),
            }
        }
        /* B-3: память проекта. Заметка дописывается в .swagcod/MEMORY.md
        внутри cwd сессии — песочница та же, подтверждение не требуется:
        это блокнот агента, а не изменение кода пользователя. */
        "memory_append" => {
            let text = arg_str(call, "text");
            if text.trim().is_empty() {
                return fail("пустая заметка".into());
            }
            let dir = cwd.join(".swagcod");
            if let Err(e) = std::fs::create_dir_all(&dir) {
                return fail(format!("memory_append: mkdir: {e}"));
            }
            let path = dir.join("MEMORY.md");
            use std::io::Write as _;
            let entry = format!("\n## {}\n{}\n", swagcod_core::bus::now_ms(), text.trim());
            match std::fs::OpenOptions::new()
                .create(true)
                .append(true)
                .open(&path)
            {
                Ok(mut f) => match f.write_all(entry.as_bytes()) {
                    Ok(()) => ToolOutcome {
                        ok: true,
                        output: "заметка добавлена в .swagcod/MEMORY.md".into(),
                    },
                    Err(e) => fail(format!("memory_append: {e}")),
                },
                Err(e) => fail(format!("memory_append: {e}")),
            }
        }
        "bash" => {
            let command = arg_str(call, "command");
            if command.trim().is_empty() {
                return fail("пустая команда".into());
            }
            let child = silent_cmd("cmd")
                .arg("/C")
                .arg(&command)
                .current_dir(cwd)
                .spawn();
            let child = match child {
                Ok(c) => c,
                Err(e) => return fail(format!("запуск: {e}")),
            };
            // Таймаут обязателен: зависшая команда не должна вешать ход.
            // wait_with_output уносит child в future, поэтому pid берём заранее
            // и по таймауту убиваем дерево процессов через taskkill /T.
            let pid = child.id();
            match tokio::time::timeout(timeout, child.wait_with_output()).await {
                Ok(Ok(output)) => ToolOutcome {
                    ok: output.status.success(),
                    output: format!(
                        "stdout:\n{}\nstderr:\n{}\nexit: {}",
                        String::from_utf8_lossy(&output.stdout),
                        String::from_utf8_lossy(&output.stderr),
                        output.status.code().unwrap_or(-1)
                    ),
                },
                Ok(Err(e)) => fail(format!("запуск: {e}")),
                Err(_) => {
                    if let Some(pid) = pid {
                        let _ = silent_cmd("taskkill")
                            .args(["/PID", &pid.to_string(), "/T", "/F"])
                            .output()
                            .await;
                    }
                    fail(format!(
                        "команда не завершилась за {} с и остановлена",
                        timeout.as_secs()
                    ))
                }
            }
        }
        /* B-5: grep/glob/patch/fetch_url. Чистая логика живёт в fsx::tools,
        здесь только песочница cwd и оформление результата. */
        "grep" => {
            let pattern = arg_str(call, "pattern");
            if pattern.is_empty() {
                return fail("пустой pattern".into());
            }
            let sub = arg_str(call, "path");
            let context = call
                .arguments
                .get("context")
                .and_then(|v| v.as_u64())
                .unwrap_or(2) as usize;
            match swagcod_fsx::tools::grep_files(
                cwd,
                if sub.is_empty() {
                    None
                } else {
                    Some(sub.as_str())
                },
                &pattern,
                context,
            ) {
                Ok(hits) if hits.is_empty() => ToolOutcome {
                    ok: true,
                    output: "совпадений нет".into(),
                },
                Ok(hits) => ToolOutcome {
                    ok: true,
                    output: swagcod_fsx::tools::format_hits(&hits),
                },
                Err(e) => fail(format!("grep: {e}")),
            }
        }
        "glob" => {
            let pattern = arg_str(call, "pattern");
            if pattern.is_empty() {
                return fail("пустой pattern".into());
            }
            let sub = arg_str(call, "path");
            match swagcod_fsx::tools::glob_files(
                cwd,
                if sub.is_empty() {
                    None
                } else {
                    Some(sub.as_str())
                },
                &pattern,
            ) {
                Ok(files) if files.is_empty() => ToolOutcome {
                    ok: true,
                    output: "совпадений нет".into(),
                },
                Ok(files) => ToolOutcome {
                    ok: true,
                    output: files.join("\n"),
                },
                Err(e) => fail(format!("glob: {e}")),
            }
        }
        "patch" => {
            let path = match sandbox_path(cwd, &arg_str(call, "path")) {
                Ok(p) => p,
                Err(e) => return fail(e),
            };
            let old = arg_str(call, "old_string");
            let new = arg_str(call, "new_string");
            let replace_all = call
                .arguments
                .get("replace_all")
                .and_then(|v| v.as_bool())
                .unwrap_or(false);
            let content = match std::fs::read_to_string(&path) {
                Ok(c) => c,
                Err(e) => return fail(format!("patch: {e}")),
            };
            match swagcod_fsx::tools::patch_text(&content, &old, &new, replace_all) {
                Ok((patched, n)) => match std::fs::write(&path, patched.as_bytes()) {
                    Ok(()) => ToolOutcome {
                        ok: true,
                        output: format!("замен: {n}"),
                    },
                    Err(e) => fail(format!("patch write: {e}")),
                },
                Err(e) => fail(format!("patch: {e}")),
            }
        }
        "fetch_url" => {
            let url = arg_str(call, "url");
            if url.trim().is_empty() {
                return fail("пустой url".into());
            }
            let fetch_timeout = timeout.min(std::time::Duration::from_secs(30));
            match swagcod_fsx::tools::fetch_url(&url, fetch_timeout).await {
                Ok(text) => ToolOutcome {
                    ok: true,
                    output: text,
                },
                Err(e) => fail(format!("fetch_url: {e}")),
            }
        }
        other => {
            /* B-5: слой плагинов. Не-встроенное имя ищется в реестре;
            подтверждение уже пройдено (политика считает неизвестные
            имена опасными), здесь только исполнение. */
            match plugins.iter().find(|p| p.name == other) {
                None => fail(format!("неизвестный инструмент: {other}")),
                Some(plugin) => run_plugin(plugin, call, cwd, timeout).await,
            }
        }
    }
}

/// Исполнить внешний инструмент: команда через shell, аргументы JSON-ом
/// в stdin, cwd — рабочая директория сессии, таймаут как у bash.
async fn run_plugin(
    plugin: &PluginTool,
    call: &ToolCall,
    cwd: &std::path::Path,
    timeout: std::time::Duration,
) -> ToolOutcome {
    use tokio::io::AsyncWriteExt;
    let fail = |output: String| ToolOutcome { ok: false, output };
    let args = serde_json::to_string(&call.arguments).unwrap_or_else(|_| "{}".into());
    let (shell, flag) = if cfg!(windows) {
        ("cmd", "/C")
    } else {
        ("sh", "-c")
    };
    let mut child = match silent_cmd(shell)
        .arg(flag)
        .arg(&plugin.command)
        .current_dir(cwd)
        .stdin(std::process::Stdio::piped())
        .stdout(std::process::Stdio::piped())
        .stderr(std::process::Stdio::piped())
        .spawn()
    {
        Ok(c) => c,
        Err(e) => return fail(format!("плагин {}: запуск: {e}", plugin.name)),
    };
    if let Some(mut stdin) = child.stdin.take() {
        // Ошибка записи не фатальна: плагин мог не читать stdin.
        let _ = stdin.write_all(args.as_bytes()).await;
    }
    let pid = child.id();
    match tokio::time::timeout(timeout, child.wait_with_output()).await {
        Ok(Ok(output)) => ToolOutcome {
            ok: output.status.success(),
            output: format!(
                "stdout:\n{}\nstderr:\n{}\nexit: {}",
                String::from_utf8_lossy(&output.stdout),
                String::from_utf8_lossy(&output.stderr),
                output.status.code().unwrap_or(-1)
            ),
        },
        Ok(Err(e)) => fail(format!("плагин {}: {e}", plugin.name)),
        Err(_) => {
            if let Some(pid) = pid {
                if cfg!(windows) {
                    let _ = silent_cmd("taskkill")
                        .args(["/PID", &pid.to_string(), "/T", "/F"])
                        .output()
                        .await;
                } else {
                    let _ = silent_cmd("kill")
                        .args(["-9", &pid.to_string()])
                        .output()
                        .await;
                }
            }
            fail(format!(
                "плагин {} не завершился за {} с и остановлен",
                plugin.name,
                timeout.as_secs()
            ))
        }
    }
}

/// Решение человека по вызову: диалог подтверждения в UI стреляет этой командой.
#[tauri::command]
async fn respond_approval(
    state: State<'_, Arc<AppState>>,
    call_id: String,
    decision: ApprovalDecision,
) -> Result<(), String> {
    respond_approval_core(&state, call_id, decision).await
}

/* E-7: ядро ответа на подтверждение — общее для IPC и REST. */
pub(crate) async fn respond_approval_core(
    state: &AppState,
    call_id: String,
    decision: ApprovalDecision,
) -> Result<(), String> {
    let tx = state
        .approvals
        .lock()
        .await
        .remove(&call_id)
        .ok_or_else(|| format!("нет ожидающего подтверждения: {call_id}"))?;
    tx.send(decision)
        .map_err(|_| "ход уже завершился".to_string())
}

/* ── B-7: укрепление безопасности ─────────────────────────────────────────
 * Per-session политика подтверждений, журнал подтверждений из store и
 * DPAPI-хранилище ключа провайдера (опция: .env продолжает работать). */

/// Ключ провайдера из DPAPI-blob в store. Env имеет приоритет: если
/// SWAGCOD_API_KEY задан, blob даже не расшифровывается.
fn dpapi_key(state: &AppState) -> Option<String> {
    if !std::env::var("SWAGCOD_API_KEY")
        .unwrap_or_default()
        .trim()
        .is_empty()
    {
        return None;
    }
    let blob = state
        .store
        .lock()
        .ok()?
        .get_pref("api_key_dpapi")
        .ok()
        .flatten()?;
    if blob.trim().is_empty() {
        return None;
    }
    dpapi::unprotect_hex(&blob).ok()
}

/// Политика подтверждений конкретной сессии. None/пустая строка — сброс
/// к глобальной политике приложения.
#[tauri::command]
async fn set_session_approval_policy(
    state: State<'_, Arc<AppState>>,
    session_id: String,
    policy: Option<String>,
) -> Result<(), String> {
    let parsed: Option<ApprovalPolicy> = match policy.as_deref().map(str::trim) {
        None | Some("") | Some("global") => None,
        Some(s) => Some(
            serde_json::from_value::<ApprovalPolicy>(serde_json::Value::String(s.to_lowercase()))
                .map_err(|_| format!("неизвестная политика: {s} (always|never|on_dangerous)"))?,
        ),
    };
    {
        let mut sessions = state.sessions.lock().await;
        let session = sessions
            .iter_mut()
            .find(|s| s.id.to_string() == session_id)
            .ok_or_else(|| format!("сессия не найдена: {session_id}"))?;
        session.approval_policy = parsed;
    }
    let store = state.store.lock().map_err(|e| e.to_string())?;
    store
        .set_session_policy(
            &session_id,
            parsed.map(|p| match p {
                ApprovalPolicy::Always => "always",
                ApprovalPolicy::Never => "never",
                ApprovalPolicy::OnDangerous => "on_dangerous",
            }),
        )
        .map_err(|e| e.to_string())
}

/// Журнал подтверждений: кто, что, когда одобрил. Свежайшие первыми.
#[tauri::command]
fn approval_log(
    state: State<'_, Arc<AppState>>,
    session_id: Option<String>,
    limit: Option<usize>,
) -> Result<Vec<swagcod_core::store::ApprovalEntry>, String> {
    let store = state.store.lock().map_err(|e| e.to_string())?;
    store
        .approval_log(session_id.as_deref(), limit.unwrap_or(200))
        .map_err(|e| e.to_string())
}

/// Положить ключ провайдера под DPAPI: в store уходит только шифроблоб.
#[tauri::command]
fn save_protected_key(state: State<'_, Arc<AppState>>, key: String) -> Result<(), String> {
    if key.trim().is_empty() {
        return Err("пустой ключ".into());
    }
    let blob = dpapi::protect_hex(key.trim())?;
    let store = state.store.lock().map_err(|e| e.to_string())?;
    store
        .set_pref("api_key_dpapi", &blob)
        .map_err(|e| e.to_string())
}

/// Забыть DPAPI-ключ (env-ключ продолжает работать).
#[tauri::command]
fn clear_protected_key(state: State<'_, Arc<AppState>>) -> Result<(), String> {
    let store = state.store.lock().map_err(|e| e.to_string())?;
    store
        .set_pref("api_key_dpapi", "")
        .map_err(|e| e.to_string())
}

/* ── B-8: стабильность под наблюдением ──────────────────────────────────────
 * Watchdog живых ходов, счётчики Lagging, диагностика. Всё локально,
 * никакой внешней телеметрии (план B-8). */

/// Ход без событий дольше порога считается подозрительно тихим.
pub const QUIET_TURN_MS: u64 = 5 * 60 * 1000;
/// Повторное предупреждение — не чаще этого интервала.
pub const QUIET_REWARN_MS: u64 = 5 * 60 * 1000;
/// Период обхода watchdog.
pub const WATCHDOG_TICK_MS: u64 = 30 * 1000;

/// Отметить признак жизни хода. `waiting_human` — Some при входе/выходе
/// из ожидания человека (диалог подтверждения).
fn touch_turn(state: &AppState, turn: &str, waiting_human: Option<bool>) {
    let Ok(mut activity) = state.turn_activity.lock() else {
        return;
    };
    if let Some(a) = activity.get_mut(turn) {
        a.last_ms = swagcod_core::bus::now_ms();
        if let Some(w) = waiting_human {
            a.waiting_human = w;
        }
    }
}

/// Сканирование watchdog — чистая функция: какие ходы тихие и пора
/// предупредить. Вход: (turn_id, last_ms, waiting_human, warned_ms).
/// Ждущий человека ход не предупреждается: тишина в диалоге подтверждения
/// — это не зависание.
pub fn quiet_turns_to_warn(
    entries: &[(String, u64, bool, Option<u64>)],
    now_ms: u64,
    quiet_ms: u64,
    rewarn_ms: u64,
) -> Vec<(String, u64)> {
    entries
        .iter()
        .filter(|(_, last, waiting, warned)| {
            !waiting
                && now_ms.saturating_sub(*last) >= quiet_ms
                && warned.map_or(true, |w| now_ms.saturating_sub(w) >= rewarn_ms)
        })
        .map(|(t, last, ..)| (t.clone(), now_ms.saturating_sub(*last)))
        .collect()
}

/// Поток watchdog: раз в 30 с проверяет живые ходы и публикует Status
/// для подозрительно тихих — UI показывает это как строку статуса вместо
/// вечного «думает».
fn spawn_watchdog(state: Arc<AppState>) {
    std::thread::spawn(move || loop {
        std::thread::sleep(std::time::Duration::from_millis(WATCHDOG_TICK_MS));
        let now = swagcod_core::bus::now_ms();
        let snapshot: Vec<(String, u64, bool, Option<u64>)> = {
            let Ok(activity) = state.turn_activity.lock() else {
                continue;
            };
            activity
                .iter()
                .map(|(k, a)| (k.clone(), a.last_ms, a.waiting_human, a.warned_ms))
                .collect()
        };
        for (turn, idle_ms) in quiet_turns_to_warn(&snapshot, now, QUIET_TURN_MS, QUIET_REWARN_MS) {
            state.bus.publish(EventKind::Status {
                message: format!(
                    "ход {turn} молчит уже {} мин — подозрительно тихо, возможно завис (стоп в любой момент)",
                    idle_ms / 60_000
                ),
            });
            if let Ok(mut activity) = state.turn_activity.lock() {
                if let Some(a) = activity.get_mut(&turn) {
                    a.warned_ms = Some(now);
                }
            }
        }
    });
}

/// Диагностический снимок бекенда для экспорта (план B-8): счётчики
/// Lagging, живые ходы с возрастом и тишиной, шина, uptime.
#[tauri::command]
fn get_diagnostics(state: State<'_, Arc<AppState>>) -> serde_json::Value {
    get_diagnostics_core(&state)
}

/// E-7: статус loopback REST API — порт и токен для экрана настроек.
/// Токен показывается только локальному UI (та же модель доверия, что
/// у DPAPI-ключа во вкладке «Безопасность»): API слушает лишь 127.0.0.1.
#[tauri::command]
async fn http_api_status(state: State<'_, Arc<AppState>>) -> Result<serde_json::Value, String> {
    let port = httpapi::configured_port(&state);
    let token = if port.is_some() {
        httpapi::api_token(&state)
    } else {
        None
    };
    Ok(serde_json::json!({
        "enabled": port.is_some(),
        "port": port,
        "token": token,
    }))
}

/// E-8: история телеметрии для графиков — выборки по именам с since_ms,
/// одним списком {name, ts_ms, value}. Предел 5000 точек на имя.
#[tauri::command]
async fn metrics_history(
    state: State<'_, Arc<AppState>>,
    names: Vec<String>,
    since_ms: u64,
) -> Result<Vec<serde_json::Value>, String> {
    let mut out = Vec::new();
    let Ok(g) = state.store.lock() else {
        return Err("store недоступен".into());
    };
    for name in names.iter().take(10) {
        if let Ok(rows) = g.metrics_query(name, since_ms, 5_000) {
            for (ts_ms, value) in rows {
                out.push(serde_json::json!({ "name": name, "ts_ms": ts_ms, "value": value }));
            }
        }
    }
    Ok(out)
}

/// E-8: токены/день (UTC) — агрегация оценочных токенов ходов, которые
/// машина хода и так пишет в turns (est_in/est_out).
#[tauri::command]
async fn tokens_by_day(
    state: State<'_, Arc<AppState>>,
    since_ms: u64,
) -> Result<Vec<serde_json::Value>, String> {
    let g = state.store.lock().map_err(|e| e.to_string())?;
    let rows = g.tokens_by_day(since_ms).map_err(|e| e.to_string())?;
    Ok(rows
        .iter()
        .map(|(day_ms, tokens, turns)| {
            serde_json::json!({ "day_ms": day_ms, "tokens": tokens, "turns": turns })
        })
        .collect())
}

/* E-7: ядро диагностики — общее для IPC и loopback REST. */
pub(crate) fn get_diagnostics_core(state: &AppState) -> serde_json::Value {
    let now = swagcod_core::bus::now_ms();
    let turns: Vec<serde_json::Value> = state
        .turn_activity
        .lock()
        .map(|activity| {
            activity
                .iter()
                .map(|(k, a)| {
                    serde_json::json!({
                        "turn": k,
                        "session": a.session,
                        "started_ms": a.started_ms,
                        "age_ms": now.saturating_sub(a.started_ms),
                        "idle_ms": now.saturating_sub(a.last_ms),
                        "waiting_human": a.waiting_human,
                        "quiet": !a.waiting_human
                            && now.saturating_sub(a.last_ms) >= QUIET_TURN_MS,
                    })
                })
                .collect()
        })
        .unwrap_or_default();
    serde_json::json!({
        "generated_ms": now,
        "uptime_ms": now.saturating_sub(start_ms()),
        "version": env!("CARGO_PKG_VERSION"),
        "bus": {
            "seq": state.bus.seq(),
            "subscribers": state.bus.subscriber_count(),
        },
        "lagging": {
            "events": state.lagging_events.load(std::sync::atomic::Ordering::Relaxed),
            "dropped": state.lagging_dropped.load(std::sync::atomic::Ordering::Relaxed),
        },
        "active_turns": turns,
        "sessions": state.sessions.try_lock().map(|s| s.len()).unwrap_or(0),
    })
}

/// Чтение файла (для просмотра содержимого и diff).
#[tauri::command]
async fn read_file(
    state: State<'_, Arc<AppState>>,
    session_id: String,
    path: String,
) -> Result<String, String> {
    let root = session_cwd(&state, &session_id).await?;
    let path = std::path::Path::new(&path);
    if !path.is_file() {
        return Err(format!("не файл: {path:?}"));
    }

    // C4-фикс: песочница
    if !swagcod_fsx::is_within(&root, path) {
        return Err("доступ запрещён: путь вне рабочей директории сессии".into());
    }

    // Ограничение: не читаем файлы больше 1 МБ
    let meta = std::fs::metadata(path).map_err(|e| format!("metadata: {e}"))?;
    if meta.len() > 1_048_576 {
        return Err("файл слишком большой (> 1 МБ)".into());
    }
    std::fs::read_to_string(path).map_err(|e| format!("read: {e}"))
}

/// Список моделей АКТИВНОГО провайдера (вкладка провайдеров).
#[tauri::command]
async fn list_models(state: State<'_, Arc<AppState>>) -> Result<serde_json::Value, String> {
    let provider = providers::build_chat_provider(&state).map_err(|e| format!("провайдер: {e}"))?;
    provider
        .list_models()
        .await
        .map_err(|e| format!("модели: {e}"))
}

/* ── Обновления (rev37): GitHub Releases + tauri-plugin-updater. ────────

Схема та же, что в DSH Phone: манифест latest.json на
releases/latest/download, артефакты подписаны minisign (приватный
ключ вне репозитория — профиль пользователя ~/.swagcod-updater и
секреты CI). update_check не валит команду при сетевой ошибке —
ошибка приходит полем, UI показывает её честно. Перед установкой
гасим свои node-процессы (sidecar хранилища, хост плагинов, MCP):
работающий node.exe заблокирован на запись, и NSIS не смог бы его
перезаписать — урок dsh-phone (os error 5). */

/// Результат проверки обновлений. `current` заполнен всегда — UI
/// показывает «у вас x.y.z», даже когда проверка ничего не нашла.
#[derive(Debug, Clone, serde::Serialize)]
pub struct UpdateInfo {
    pub available: bool,
    /// Версия из релиза (если обновление есть).
    pub version: Option<String>,
    /// Текущая версия приложения.
    pub current: String,
    pub notes: Option<String>,
    /// Причина, по которой проверить не удалось. Ошибка сети — не повод
    /// валить команду: UI показывает её текстом вместо тихого «обновлений нет».
    pub error: Option<String>,
}

/// Проверить GitHub Releases на новую версию (манифест latest.json).
#[tauri::command]
async fn update_check(app: tauri::AppHandle) -> Result<UpdateInfo, String> {
    use tauri_plugin_updater::UpdaterExt;
    let current = app.package_info().version.to_string();
    let updater = app
        .updater_builder()
        .build()
        .map_err(|e| format!("updater не собрался: {e}"))?;
    match updater.check().await {
        Ok(Some(u)) => Ok(UpdateInfo {
            available: true,
            version: Some(u.version.clone()),
            current,
            notes: u.body.clone(),
            error: None,
        }),
        Ok(None) => Ok(UpdateInfo {
            available: false,
            version: None,
            current,
            notes: None,
            error: None,
        }),
        Err(e) => Ok(UpdateInfo {
            available: false,
            version: None,
            current,
            notes: None,
            error: Some(e.to_string()),
        }),
    }
}

/// Скачать и поставить обновление, шля прогресс событием `update-progress`.
/// Живые ходы — честный отказ: рвать траекторию посреди стрима нельзя.
#[tauri::command]
async fn update_install(app: tauri::AppHandle) -> Result<String, String> {
    use tauri_plugin_updater::UpdaterExt;
    let state = app.state::<Arc<AppState>>();

    if !state.turns.lock().await.is_empty() {
        return Err("обновление: есть активные ходы — дождитесь завершения и повторите".into());
    }

    let updater = app
        .updater_builder()
        .build()
        .map_err(|e| format!("updater не собрался: {e}"))?;
    let update = updater
        .check()
        .await
        .map_err(|e| format!("проверка обновлений: {e}"))?
        .ok_or_else(|| "нет доступного обновления".to_string())?;
    let version = update.version.clone();

    // Гасим node-процессы до установки: MCP-соединения, хост плагинов,
    // sidecar хранилища. Их exe заблокированы на запись для NSIS.
    state.mcp.lock().await.shutdown().await;
    if let Some(host) = state.js_host.lock().ok().and_then(|g| g.clone()) {
        host.kill().await;
    }
    if let Ok(mut store) = state.store.lock() {
        store.shutdown();
    }

    let h = app.clone();
    let mut downloaded: u64 = 0;
    let mut total: Option<u64> = None;
    update
        .download_and_install(
            move |chunk, len| {
                downloaded += chunk as u64;
                if let Some(t) = len {
                    total = Some(t);
                }
                let percent = total
                    .filter(|t| *t > 0)
                    .map(|t| ((downloaded * 100) / t).min(100));
                let _ = h.emit(
                    "update-progress",
                    serde_json::json!({
                        "downloaded": downloaded,
                        "total": total,
                        "percent": percent,
                    }),
                );
            },
            || {},
        )
        .await
        .map_err(|e| format!("установка обновления: {e}"))?;

    Ok(version)
}

/// Перезапуск после установки. request_restart, а не restart: первый идёт
/// через RunEvent::Exit и потому срабатывает надёжно из любого потока.
#[tauri::command]
fn restart_app(app: tauri::AppHandle) {
    app.request_restart();
}

/// Открыть файл в проводнике (безопасно: путь отдельным аргументом).
#[tauri::command]
async fn open_in_explorer(
    state: State<'_, Arc<AppState>>,
    session_id: String,
    path: String,
) -> Result<(), String> {
    // C-1 фикс: песочница + путь как аргумент, не склейка
    let root = session_cwd(&state, &session_id).await?;
    let p = std::path::Path::new(&path);
    if !swagcod_fsx::is_within(&root, p) {
        return Err("доступ запрещён".into());
    }
    silent_cmd("explorer")
        .arg(format!("/select,{}", path))
        .spawn()
        .map_err(|e| format!("explorer: {e}"))?;
    Ok(())
}

/// Команда без всплывающего консольного окна на Windows
/// (CREATE_NO_WINDOW). Иначе каждый git/cmd/taskkill моргает
/// консолью поверх окна приложения.
fn silent_cmd(program: &str) -> tokio::process::Command {
    let mut cmd = tokio::process::Command::new(program);
    #[cfg(windows)]
    cmd.creation_flags(0x0800_0000);
    cmd
}

/// Короткий идентификатор без зависимостей: наносекунды + монотонный счётчик.
///
/// Счётчик обязателен: два вызова в одну наносекунду иначе дали бы
/// одинаковый id, а коллизия id сессий — это перемешанные истории.
fn short_id() -> String {
    use std::sync::atomic::{AtomicU32, Ordering};
    use std::time::{SystemTime, UNIX_EPOCH};

    static SEQ: AtomicU32 = AtomicU32::new(0);
    let nanos = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.subsec_nanos())
        .unwrap_or(0);
    let n = SEQ.fetch_add(1, Ordering::Relaxed);
    format!("{nanos:08x}{n:08x}")
}

/* ===================== E-9: импорт сессий из DSH ===================== */

/// Прогнать импорт и поставить флаг готовности. Отсутствие DSH на машине —
/// тихий пропуск (флаг всё равно ставится). После успешного импорта
/// перечитываем сессии в память: UI видит перенесённое без перезапуска.
fn run_dsh_import(state: &Arc<AppState>, auto: bool) -> dsh_import::ImportReport {
    let mut report = dsh_import::ImportReport::default();
    let root = match dsh_import::dsh_sessions_root() {
        Some(r) => r,
        None => {
            if let Ok(store) = state.store.lock() {
                let _ = store.set_pref("dsh_import_done", "no-dsh");
            }
            return report;
        }
    };
    // DSH-история бывает огромной (сотни файлов, сотни МБ zstd) — импорт
    // идёт через ОТДЕЛЬНОЕ соединение с базой, а не через state.store.
    // Иначе Mutex занят весь прогон, весь IPC голодает на нём и окно
    // висит "не отвечает", хотя импорт по задумке фоновый. Флаг
    // готовности по-прежнему ставится через общее соединение в конце.
    let path = match db_path() {
        Ok(p) => p,
        Err(e) => {
            report.errors.push(format!("store: нет пути базы: {e}"));
            return report;
        }
    };
    match swagcod_core::store::open(&path) {
        Ok(import_store) => {
            report = dsh_import::import_dsh_sessions(import_store.as_ref(), &root);
            // E-9b: конфиги MCP/плагинов — тем же отдельным соединением.
            let cfg = dsh_import::import_dsh_configs(import_store.as_ref());
            report.mcp_imported = cfg.mcp_imported;
            report.mcp_skipped = cfg.mcp_skipped;
            report.plugin_notes = cfg.plugin_notes;
        }
        Err(e) => {
            report
                .errors
                .push(format!("store: не открыл базу для импорта: {e}"));
            return report;
        }
    }
    if let Ok(store) = state.store.lock() {
        let _ = store.set_pref(
            "dsh_import_done",
            &format!("sessions={},turns={}", report.sessions, report.turns),
        );
    }
    if report.sessions > 0 || report.resumed > 0 {
        if let Ok(store) = state.store.lock() {
            if let Ok(loaded) = store.load_all() {
                /* D-121: импортированная история не остаётся в RAM — список
                переустанавливается «лёгким», цифры уходят в meta. */
                let (light, meta) = light_sessions(loaded);
                if let Ok(mut m) = state.session_meta.lock() {
                    *m = meta;
                }
                if let Ok(mut sessions) = state.sessions.try_lock() {
                    *sessions = light;
                }
            }
        }
    }
    eprintln!(
        "dsh-import{}: сессий {}, ходов {}, пропущено {}, догружено {}, ошибок {}, mcp {}, mcp-пропусков {}, плагинов-заметок {}",
        if auto { " (авто)" } else { "" },
        report.sessions,
        report.turns,
        report.skipped,
        report.resumed,
        report.errors.len(),
        report.mcp_imported,
        report.mcp_skipped.len(),
        report.plugin_notes.len()
    );
    report
}

/// E-9: ручной импорт — кнопка в настройках. Тяжёлая работа (zstd, обход,
/// запись в базу) уходит в blocking-пул.
#[tauri::command]
async fn import_dsh_sessions(state: State<'_, Arc<AppState>>) -> Result<serde_json::Value, String> {
    let st = state.inner().clone();
    let report = tokio::task::spawn_blocking(move || run_dsh_import(&st, false))
        .await
        .map_err(|e| format!("импорт DSH: {e}"))?;
    Ok(serde_json::json!({
        "sessions": report.sessions,
        "turns": report.turns,
        "messages": report.messages,
        "skipped": report.skipped,
        "resumed": report.resumed,
        "errors": report.errors,
        "mcp_imported": report.mcp_imported,
        "mcp_skipped": report.mcp_skipped,
        "plugin_notes": report.plugin_notes,
    }))
}

/* ===================== E-3: MCP-клиент ===================== */

/// Реестр MCP-серверов живёт в prefs (`mcp_servers`, JSON-массив) —
/// конфигурация переживает перезапуск, живые соединения не переживают.
fn mcp_configs(state: &AppState) -> Vec<mcp::McpServerConfig> {
    state
        .store
        .lock()
        .ok()
        .and_then(|s| s.get_pref("mcp_servers").ok())
        .flatten()
        .and_then(|v| serde_json::from_str(&v).ok())
        .unwrap_or_default()
}

fn save_mcp_configs(state: &AppState, cfgs: &[mcp::McpServerConfig]) -> Result<(), String> {
    let v = serde_json::to_string(cfgs).map_err(|e| format!("mcp: json: {e}"))?;
    let store = state.store.lock().map_err(|e| e.to_string())?;
    store.set_pref("mcp_servers", &v).map_err(|e| e.to_string())
}

/// E-3: вызов MCP-инструмента из хода. Имя `mcp:<server>:<tool>` не входит
/// в BUILTIN, поэтому подтверждение при OnDangerous уже спросила машина
/// хода, а журнал B-7 получил tool с префиксом `mcp:` — здесь только
/// исполнение с таймаутом.
async fn run_mcp_call(state: &AppState, call: &ToolCall) -> ToolOutcome {
    let fail = |output: String| ToolOutcome { ok: false, output };
    let Some((server, tool)) = mcp::split_tool_name(&call.name) else {
        return fail(format!("mcp: неверное имя инструмента: {}", call.name));
    };
    let conn = state.mcp.lock().await.conn(server);
    let Some(conn) = conn else {
        return fail(format!(
            "mcp: сервер не подключён: {server} (проверьте статус в настройках MCP)"
        ));
    };
    let args = serde_json::Value::Object(call.arguments.clone());
    match mcp::call_tool(&conn, tool, args, mcp::call_timeout()).await {
        Ok(text) => ToolOutcome {
            ok: true,
            output: if text.trim().is_empty() {
                "(пустой ответ)".to_string()
            } else {
                text
            },
        },
        Err(e) => fail(e),
    }
}

/// E-3: реестр для UI — конфигурация (prefs) + живые статусы подключений.
#[tauri::command]
async fn mcp_list(state: State<'_, Arc<AppState>>) -> Result<serde_json::Value, String> {
    let cfgs = mcp_configs(&state);
    let reg = state.mcp.lock().await;
    let statuses = reg.status();
    let servers: Vec<serde_json::Value> = cfgs
        .iter()
        .map(|c| {
            let st = statuses.iter().find(|s| s.name == c.name);
            serde_json::json!({
                "name": c.name,
                "command": c.command,
                "args": c.args,
                "enabled": c.enabled,
                "state": st.map(|s| s.state.clone()).unwrap_or_else(|| "не запущен".to_string()),
                "tools": st.map(|s| s.tools).unwrap_or(0),
            })
        })
        .collect();
    let tools: Vec<String> = reg.tools.iter().map(|t| t.name.clone()).collect();
    Ok(serde_json::json!({ "servers": servers, "tools": tools }))
}

/// E-3: добавить (или заменить) сервер и сразу подключить. Ошибка
/// подключения — не ошибка команды: сервер записан в реестр, статус
/// честный, повторить можно из UI.
#[tauri::command]
async fn mcp_add(
    state: State<'_, Arc<AppState>>,
    name: String,
    command: String,
    args: Option<Vec<String>>,
) -> Result<serde_json::Value, String> {
    let name = name.trim().to_string();
    if !mcp::valid_server_name(&name) {
        return Err("имя: непустое, без двоеточий, до 64 символов".into());
    }
    let command = command.trim().to_string();
    if command.is_empty() {
        return Err("команда не может быть пустой".into());
    }
    let cfg = mcp::McpServerConfig {
        name: name.clone(),
        command,
        args: args.unwrap_or_default(),
        env: std::collections::HashMap::new(),
        enabled: true,
    };
    let mut cfgs = mcp_configs(&state);
    cfgs.retain(|c| c.name != name);
    cfgs.push(cfg.clone());
    save_mcp_configs(&state, &cfgs)?;
    let mut reg = state.mcp.lock().await;
    let tools = reg.connect_server(&cfg).await;
    let status = reg
        .status()
        .into_iter()
        .find(|s| s.name == name)
        .map(|s| s.state)
        .unwrap_or_default();
    Ok(serde_json::json!({ "name": name, "tools": tools, "state": status }))
}

/// E-3: убрать сервер из реестра и убить его процесс.
#[tauri::command]
async fn mcp_remove(state: State<'_, Arc<AppState>>, name: String) -> Result<(), String> {
    let cfgs: Vec<mcp::McpServerConfig> = mcp_configs(&state)
        .into_iter()
        .filter(|c| c.name != name)
        .collect();
    save_mcp_configs(&state, &cfgs)?;
    state.mcp.lock().await.disconnect(&name).await;
    Ok(())
}

/* ===================== E-4: JS-плагины в sidecar ===================== */

/// Поднять (или переподнять) sidecar JS-плагинов и установить инструменты.
/// Ошибка — не приговор: статус виден в настройках, reload повторит.
async fn start_js_plugins(state: &AppState) {
    let dir = jsplugins::plugins_dir();
    let db = db_path().ok();
    match jsplugins::JsHost::start(&dir, db.as_deref()).await {
        Ok(host) => {
            eprintln!(
                "js-плагины: {} инструментов из {} (ошибок: {})",
                host.tools.len(),
                host.dir,
                host.errors.len()
            );
            if let Ok(mut t) = state.js_tools.lock() {
                *t = host.tools.clone();
            }
            if let Ok(mut e) = state.js_errors.lock() {
                *e = host.errors.clone();
            }
            if let Ok(mut h) = state.js_host.lock() {
                *h = Some(Arc::new(host));
            }
        }
        Err(e) => {
            eprintln!("js-плагины: {e}");
            if let Ok(mut errs) = state.js_errors.lock() {
                *errs = vec![e];
            }
        }
    }
}

/// E-4: вызов JS-плагина из хода. Имя `js:<name>` не входит в BUILTIN —
/// подтверждение при OnDangerous спрашивает машина хода, журнал B-7 пишет
/// tool с префиксом `js:`. Дедлайн гарантирует vm на стороне sidecar.
async fn run_js_call(state: &AppState, call: &ToolCall) -> ToolOutcome {
    let fail = |output: String| ToolOutcome { ok: false, output };
    let Some(short) = call.name.strip_prefix(jsplugins::JS_PREFIX) else {
        return fail(format!("js: неверное имя инструмента: {}", call.name));
    };
    if short.is_empty() || short.contains(':') {
        return fail(format!("js: неверное имя инструмента: {}", call.name));
    }
    let host = state.js_host.lock().ok().and_then(|h| h.clone());
    let Some(host) = host else {
        return fail("js: sidecar плагинов не запущен (перезагрузите в настройках Plugins)".into());
    };
    let args = serde_json::Value::Object(call.arguments.clone());
    match host.call_plugin(short, args).await {
        Ok((ok, output)) => ToolOutcome {
            ok,
            output: if output.trim().is_empty() {
                "(пустой ответ)".to_string()
            } else {
                output
            },
        },
        Err(e) => fail(e),
    }
}

/// E-4: список для UI — состояние sidecar, инструменты, ошибки загрузки.
#[tauri::command]
async fn js_plugins_list(state: State<'_, Arc<AppState>>) -> Result<serde_json::Value, String> {
    let host = state.js_host.lock().ok().and_then(|h| h.clone());
    let tools: Vec<serde_json::Value> = state
        .js_tools
        .lock()
        .map(|ts| {
            ts.iter()
                .map(|t| {
                    serde_json::json!({
                        "name": t.short,
                        "description": t.description,
                        "file": t.file,
                    })
                })
                .collect()
        })
        .unwrap_or_default();
    let errors = state
        .js_errors
        .lock()
        .map(|e| e.clone())
        .unwrap_or_default();
    let dir = host
        .as_ref()
        .map(|h| h.dir.clone())
        .unwrap_or_else(|| jsplugins::plugins_dir().to_string_lossy().to_string());
    Ok(serde_json::json!({
        "running": host.is_some(),
        "dir": dir,
        "tools": tools,
        "errors": errors,
    }))
}

/// E-4: горячая перезагрузка — полный рестарт sidecar (свежие vm-контексты,
/// никакого устаревшего состояния). Старый процесс убиваем явно.
#[tauri::command]
async fn js_plugins_reload(state: State<'_, Arc<AppState>>) -> Result<serde_json::Value, String> {
    // Guard std-мьютекса не переживает await: сначала забираем хост.
    let old = state.js_host.lock().ok().and_then(|mut h| h.take());
    if let Some(old) = old {
        old.kill().await;
    }
    start_js_plugins(&state).await;
    js_plugins_list(state).await
}

/* ===================== E-5: tasks + воркер ===================== */

/// Период опроса воркера (override SWAGCOD_TASKS_POLL_MS, 1..3600 с).
pub const TASKS_POLL_MS: u64 = 15_000;
/// Попыток до терминального failed (backoff B-6: 30 с → 30 мин).
pub const TASKS_MAX_ATTEMPTS: u32 = 5;
/// Таймаут сетевой операции git fetch.
const GIT_FETCH_TIMEOUT_SECS: u64 = 120;

fn tasks_poll_ms() -> u64 {
    std::env::var("SWAGCOD_TASKS_POLL_MS")
        .ok()
        .and_then(|v| v.parse::<u64>().ok())
        .map(|v| v.clamp(1_000, 3_600_000))
        .unwrap_or(TASKS_POLL_MS)
}

/// E-5: воркер фоновых задач — живёт как насос watchdog: тикает, забирает
/// задачи с наступившим сроком, фиксирует исход с backoff.
async fn tasks_worker(state: Arc<AppState>) {
    let mut ticker = tokio::time::interval(std::time::Duration::from_millis(tasks_poll_ms()));
    loop {
        ticker.tick().await;
        run_due_tasks(&state).await;
    }
}

/// Один проход воркера: claim + обработка. Отдельно от цикла — для тестов.
async fn run_due_tasks(state: &Arc<AppState>) {
    let claimed = match state.store.lock() {
        Ok(g) => g.task_claim_due(swagcod_core::bus::now_ms()),
        Err(e) => {
            eprintln!("tasks: store: {e}");
            return;
        }
    };
    let due = match claimed {
        Ok(d) => d,
        Err(e) => {
            eprintln!("tasks: claim: {e}");
            return;
        }
    };
    for task in due {
        let outcome = run_task_handler(state, &task).await;
        let (next_state, msg, attempts, next_try) = match outcome {
            Ok(m) => match task.every_ms {
                // Периодическая задача перевооружается, разовая — done.
                Some(every) => (TASK_QUEUED, m, 0, swagcod_core::bus::now_ms() + every),
                None => (TASK_DONE, m, 0, 0),
            },
            Err(e) => {
                // attempts уже посчитаны claim'ом: backoff по текущему числу.
                if task.attempts >= TASKS_MAX_ATTEMPTS {
                    (TASK_FAILED, e, task.attempts, 0)
                } else {
                    let delay = task_backoff_ms(task.attempts);
                    (
                        TASK_QUEUED,
                        e,
                        task.attempts,
                        swagcod_core::bus::now_ms() + delay,
                    )
                }
            }
        };
        let res = match state.store.lock() {
            Ok(g) => g.task_update(&task.id, next_state, &msg, attempts, next_try),
            Err(e) => {
                eprintln!("tasks: store: {e}");
                continue;
            }
        };
        if let Err(e) = res {
            eprintln!("tasks: finish {}: {e}", task.id);
        }
    }
}

/* E-8: сэмплер телеметрии — точка раз в минуту, хранение 30 дней.
Стоимость прохода: несколько атомарных чтений и по одному INSERT на
метрику; prune — индексированный DELETE, обычно по нулю строк. */
pub const METRICS_SAMPLE_MS: u64 = 60_000;
pub const METRICS_RETENTION_MS: u64 = 30 * 24 * 3_600_000;

/// Срез текущих счётчиков — чистая функция, тестируется без таймеров.
pub fn sample_metrics(state: &AppState) -> Vec<(&'static str, f64)> {
    let mut rows: Vec<(&'static str, f64)> = vec![
        (
            "lagging_events",
            state
                .lagging_events
                .load(std::sync::atomic::Ordering::Relaxed) as f64,
        ),
        (
            "lagging_dropped",
            state
                .lagging_dropped
                .load(std::sync::atomic::Ordering::Relaxed) as f64,
        ),
        ("bus_seq", state.bus.seq() as f64),
        ("bus_subscribers", state.bus.subscriber_count() as f64),
        (
            "sessions",
            state.sessions.try_lock().map(|s| s.len()).unwrap_or(0) as f64,
        ),
    ];
    if let Ok(activity) = state.turn_activity.lock() {
        rows.push(("active_turns", activity.len() as f64));
        rows.push((
            "waiting_human",
            activity.values().filter(|a| a.waiting_human).count() as f64,
        ));
    }
    rows
}

/// Один проход сэмплера: срез → база + гигиена. Отдельно от цикла —
/// для тестов (как run_due_tasks у задач E-5).
pub fn run_metrics_sample(state: &AppState) {
    let now = swagcod_core::bus::now_ms();
    let rows = sample_metrics(state);
    let Ok(g) = state.store.lock() else { return };
    for (name, value) in rows {
        let _ = g.metrics_insert(now, name, value);
    }
    let _ = g.metrics_prune(now.saturating_sub(METRICS_RETENTION_MS));
}

async fn metrics_worker(state: Arc<AppState>) {
    let mut ticker = tokio::time::interval(std::time::Duration::from_millis(METRICS_SAMPLE_MS));
    loop {
        ticker.tick().await;
        run_metrics_sample(&state);
    }
}

/// Обработчики задач по kind. Ok — сообщение журнала исполнения,
/// Err — причина для ретрая с backoff.
async fn run_task_handler(state: &Arc<AppState>, task: &Task) -> Result<String, String> {
    match task.kind.as_str() {
        "semantic_reindex" => {
            // Без cwd в payload цель динамическая: воркспейс свежайшей
            // сессии (периодическая задача не прибивается к устаревшему пути).
            let cwd = task
                .payload
                .get("cwd")
                .and_then(|v| v.as_str())
                .map(str::to_string);
            let cwd = match cwd {
                Some(c) if !c.trim().is_empty() => c,
                _ => {
                    let sessions = state.sessions.lock().await;
                    sessions
                        .iter()
                        .max_by_key(|s| s.created_ms)
                        .map(|s| s.cwd.clone())
                        .unwrap_or_default()
                }
            };
            if cwd.trim().is_empty() {
                return Ok("нет воркспейса — пропуск".into());
            }
            if spawn_semantic_index(state.clone(), std::path::PathBuf::from(&cwd), true) {
                Ok(format!("переиндексация запущена: {cwd}"))
            } else {
                Ok("индексация уже идёт — пропуск".into())
            }
        }
        "git_fetch" => {
            let cwd = task
                .payload
                .get("cwd")
                .and_then(|v| v.as_str())
                .unwrap_or_default();
            if cwd.trim().is_empty() {
                return Err("git_fetch: в payload нет cwd".into());
            }
            let mut cmd = tokio::process::Command::new("git");
            cmd.arg("fetch").arg("--quiet").current_dir(cwd);
            #[cfg(windows)]
            cmd.creation_flags(0x0800_0000);
            let out = tokio::time::timeout(
                std::time::Duration::from_secs(GIT_FETCH_TIMEOUT_SECS),
                cmd.output(),
            )
            .await
            .map_err(|_| format!("git_fetch: таймаут {GIT_FETCH_TIMEOUT_SECS} с"))?
            .map_err(|e| format!("git_fetch: {e}"))?;
            if out.status.success() {
                Ok(format!("git fetch: ok ({cwd})"))
            } else {
                let stderr = String::from_utf8_lossy(&out.stderr);
                Err(format!("git fetch: {}", stderr.trim()))
            }
        }
        "journal_export" => {
            let sid = task
                .payload
                .get("session_id")
                .and_then(|v| v.as_str())
                .unwrap_or_default();
            let path = task
                .payload
                .get("path")
                .and_then(|v| v.as_str())
                .unwrap_or_default();
            if sid.is_empty() || path.trim().is_empty() {
                return Err("journal_export: в payload нужны session_id и path".into());
            }
            // Тот же whitelist расширений, что у ручного экспорта:
            // задача — не обход защиты.
            let lower = path.to_ascii_lowercase();
            if !(lower.ends_with(".jsonl") || lower.ends_with(".json") || lower.ends_with(".txt")) {
                return Err("journal_export: разрешены только .jsonl/.json/.txt".into());
            }
            let events = transcript_events(state, sid).await?;
            let body: String = events
                .iter()
                .map(|e| e.to_string())
                .collect::<Vec<_>>()
                .join("\n")
                + "\n";
            tokio::fs::write(path, body)
                .await
                .map_err(|e| format!("journal_export: {e}"))?;
            Ok(format!("journal_export: {} событий → {path}", events.len()))
        }
        other => Err(format!("неизвестный вид задачи: {other}")),
    }
}

/// B-3/F-2: компакция истории сайд-запросом саммаризатору. Общая для
/// turn-драйвера (по порогу should_compact) и команды session_compact
/// (/compact из палитры, force = всегда). Возвращает (сводка, история,
/// число свёрнутых старых сообщений). Ошибка саммаризатора не роняет
/// вызывающего: остаёмся на полном контексте, как в B-3.
async fn run_compaction(
    app_state: &Arc<AppState>,
    provider: &AnyProvider,
    sid: &str,
    model: &str,
    force: bool,
    summary: String,
    history: Vec<ChatMessage>,
) -> (String, Vec<ChatMessage>, usize) {
    use swagcod_core::context;
    let calib = app_state
        .store
        .lock()
        .ok()
        .and_then(|s| s.get_pref("token_calibration").ok().flatten());
    let cpt = context::chars_per_token(model, calib.as_deref());
    let ctx_window = context::model_context_tokens(model);
    let est =
        context::estimate_history_tokens(&history, cpt) + context::estimate_tokens(&summary, cpt);
    if !force && !context::should_compact(est, ctx_window) {
        return (summary, history, 0);
    }
    let (old, recent) = context::split_history(&history, context::KEEP_RECENT_TURNS);
    if old.is_empty() {
        return (summary, history, 0);
    }
    let folded = old.len();
    let bus = app_state.bus.clone();
    bus.publish(EventKind::Status {
        message: "сжимаю контекст: сворачиваю старые ходы".into(),
    });
    let sum_model = app_state
        .store
        .lock()
        .ok()
        .and_then(|s| s.get_pref("summarizer_model").ok().flatten())
        .filter(|m| !m.trim().is_empty())
        .unwrap_or_else(|| model.to_string());
    let prompt = context::summarizer_prompt(&summary, &old);
    let sum_req = ChatRequest::new(&sum_model, vec![ChatMessage::user(prompt)]);
    match provider.stream(sum_req) {
        Ok((mut srx, shandle)) => {
            let mut text = String::new();
            let mut sum_err: Option<String> = None;
            while let Some(ev) = srx.recv().await {
                match ev {
                    StreamEvent::Content(t) => text.push_str(&t),
                    StreamEvent::Error(m) => sum_err = Some(m),
                    _ => {}
                }
            }
            shandle.abort();
            if sum_err.is_none() && !text.trim().is_empty() {
                let summary = text.trim().to_string();
                {
                    let mut sessions = app_state.sessions.lock().await;
                    if let Some(s) = sessions.iter_mut().find(|s| s.id.to_string() == sid) {
                        s.summary = summary.clone();
                        s.history = recent.clone();
                        // D-121: после сжатия контекст стал легче —
                        // снимок для сайдбара обновляется сразу.
                        if let Ok(mut m) = app_state.session_meta.lock() {
                            m.insert(sid.to_string(), meta_of(s));
                        }
                    }
                }
                if let Ok(store) = app_state.store.lock() {
                    let _ = store.set_session_summary(sid, &summary);
                }
                bus.publish(EventKind::Status {
                    message: format!("контекст сжат: {folded} старых сообщений свёрнуто в сводку"),
                });
                (summary, recent, folded)
            } else {
                bus.publish(EventKind::Status {
                    message: "сжатие не удалось, продолжаю с полным контекстом".into(),
                });
                (summary, history, 0)
            }
        }
        Err(e) => {
            bus.publish(EventKind::Status {
                message: format!("сжатие не удалось: {e}"),
            });
            (summary, history, 0)
        }
    }
}

/// F-2: /compact — ручная компакция истории сессии (B-3 по требованию,
/// без порога). Возвращает число свёрнутых старых сообщений; 0 — сжимать
/// нечего (история мала) или саммаризатор не смог: UI скажет честно.
#[tauri::command]
async fn session_compact(
    state: State<'_, Arc<AppState>>,
    session_id: String,
) -> Result<usize, String> {
    let provider = providers::build_chat_provider(&state).map_err(|e| format!("провайдер: {e}"))?;
    let (history, summary, model) = {
        let sessions = state.sessions.lock().await;
        let s = sessions
            .iter()
            .find(|s| s.id.to_string() == session_id)
            .ok_or_else(|| format!("сессия не найдена: {session_id}"))?;
        (s.history.clone(), s.summary.clone(), s.model.clone())
    };
    let (_, _, folded) = run_compaction(
        &state,
        &provider,
        &session_id,
        &model,
        true,
        summary,
        history,
    )
    .await;
    Ok(folded)
}

/// E-5: список задач для UI/диагностики (журнал исполнения — last_error).
#[tauri::command]
async fn tasks_list(state: State<'_, Arc<AppState>>) -> Result<Vec<Task>, String> {
    let guard = state.store.lock().map_err(|e| e.to_string())?;
    guard.tasks_list(200).map_err(|e| e.to_string())
}

/// E-5: отмена задачи, которая ещё не стартовала. Выполняющуюся отменить
/// нельзя: обработчик владеет процессом/IO — честное прерывание не имитируем.
#[tauri::command]
async fn task_cancel(state: State<'_, Arc<AppState>>, id: String) -> Result<bool, String> {
    let guard = state.store.lock().map_err(|e| e.to_string())?;
    let t = guard
        .task_get(&id)
        .map_err(|e| e.to_string())?
        .ok_or_else(|| "нет такой задачи".to_string())?;
    if t.state != TASK_QUEUED {
        return Err(format!("нельзя отменить: задача в состоянии {}", t.state));
    }
    guard
        .task_update(&id, TASK_CANCELLED, "отменена пользователем", t.attempts, 0)
        .map_err(|e| e.to_string())?;
    Ok(true)
}

/// E-5: ручная постановка задачи. Виды — whitelist: неизвестный вид
/// отклоняется сразу, а не после круга воркера.
#[tauri::command]
async fn task_add(
    state: State<'_, Arc<AppState>>,
    kind: String,
    payload: serde_json::Value,
    every_ms: Option<u64>,
) -> Result<Task, String> {
    const ALLOWED: &[&str] = &["semantic_reindex", "git_fetch", "journal_export"];
    if !ALLOWED.contains(&kind.as_str()) {
        return Err(format!("неизвестный вид задачи: {kind}"));
    }
    let now = swagcod_core::bus::now_ms();
    let id = format!("{kind}-{}", short_id());
    let t = match every_ms.filter(|v| *v >= 60_000) {
        Some(every) => Task::periodic(id, kind.as_str(), payload, every, now),
        None => Task::new(id, kind.as_str(), payload, now),
    };
    let guard = state.store.lock().map_err(|e| e.to_string())?;
    guard.task_enqueue(&t).map_err(|e| e.to_string())?;
    Ok(t)
}

pub fn run() {
    /* B-8: panic-hook ставится ДО всего остального, чтобы поймать даже
    панику инициализации. Лог — на диске, телеметрии нет. */
    crashlog::install_hook();
    let _ = start_ms();

    // Загружаем .env если есть (ключ провайдера, базовый URL, модель).
    // Не фатально если файла нет — переменные могут быть в окружении.
    let _ = dotenvy::dotenv();

    tauri::Builder::default()
        .plugin(tauri_plugin_dialog::init())
        .plugin(tauri_plugin_updater::Builder::new().build())
        .plugin(tauri_plugin_process::init())
        .plugin(tauri_plugin_single_instance::init(|app, _argv, _cwd| {
            if let Some(w) = app.get_webview_window("main") {
                let _ = w.show();
                let _ = w.unminimize();
                let _ = w.set_focus();
            }
        }))
        .setup(|app| {
            let mut state = AppState::default();
            /* B-1: файловая база в локальном профиле и сидирование сессий
            из неё — история переживает перезапуск приложения. */
            match db_path() {
                Ok(path) => match swagcod_core::store::open(&path) {
                    Ok(store) => state.store = std::sync::Mutex::new(store),
                    Err(e) => eprintln!("store: не открыл базу {path:?}: {e}"),
                },
                Err(e) => eprintln!("store: нет пути базы: {e}"),
            }
            let state = Arc::new(state);
            if let Ok(store) = state.store.lock() {
                match store.load_all() {
                    Ok(loaded) => {
                        /* D-121: бюджет памяти — в RAM живёт «лёгкий» список
                        (история и журнал остаются в базе), цифры сайдбара
                        уезжают в session_meta. Старт с 69 импортированными
                        сессиями без этого стоил 68 МБ приватной памяти. */
                        let (light, meta) = light_sessions(loaded);
                        if let Ok(mut m) = state.session_meta.lock() {
                            *m = meta;
                        }
                        let mut sessions = state.sessions.blocking_lock();
                        *sessions = light;
                    }
                    Err(e) => eprintln!("store: не прочитал сессии: {e}"),
                }
            }
            /* E-9: автоимпорт DSH при старте ВЫРЕЗАН намеренно: сотни файлов
            и сотни МБ zstd держали Mutex базы весь прогон, весь IPC
            голодал и окно висело «не отвечает». Ручной триггер —
            команда import_dsh_sessions (кнопка в настройках). */
            {
                // Флаг-пустышка, чтобы UI не предлагал автоимпорт заново.
                if let Ok(store) = state.store.lock() {
                    let has = store.get_pref("dsh_import_done").ok().flatten().is_some();
                    if !has {
                        let _ = store.set_pref("dsh_import_done", "manual");
                    }
                }
            }
            /* E-3: MCP-серверы из prefs поднимаются фоном — старт не ждёт
            рукопожатий внешних процессов. Статусы видны в настройках. */
            {
                let st = state.clone();
                tauri::async_runtime::spawn(async move {
                    for cfg in mcp_configs(&st) {
                        let n = st.mcp.lock().await.connect_server(&cfg).await;
                        eprintln!("mcp: {} — {n} инструментов", cfg.name);
                    }
                });
            }
            /* E-4: sidecar JS-плагинов тоже стартует фоном — бюджет старта
            не ждёт node-процесс и чтение каталога. */
            {
                let st = state.clone();
                tauri::async_runtime::spawn(async move {
                    start_js_plugins(&st).await;
                });
            }
            /* E-5: tasks — восстановить сирот после краша (running →
            queued), вычистить старые done, сидировать периодическую
            переиндексацию (первый старт через сутки: стартовую
            индексацию уже делает триггер E-2). Сид идемпотентен:
            INSERT OR IGNORE, отменённая задача не воскрешается. */
            {
                if let Ok(g) = state.store.lock() {
                    let _ = g.tasks_recover_running();
                    let _ = g.tasks_prune(
                        swagcod_core::bus::now_ms().saturating_sub(7 * 24 * 3_600_000),
                    );
                    let _ = g.task_enqueue(&Task::periodic(
                        "semantic-reindex",
                        "semantic_reindex",
                        serde_json::json!({}),
                        24 * 3_600_000,
                        swagcod_core::bus::now_ms(),
                    ));
                }
            }
            {
                let st = state.clone();
                tauri::async_runtime::spawn(tasks_worker(st));
            }
            /* E-8: сэмплер телеметрии — точка раз в минуту с первого тика
            (interval стартует сразу), хранение 30 дней. */
            {
                let st = state.clone();
                tauri::async_runtime::spawn(metrics_worker(st));
            }
            /* E-7: loopback REST API стартует, только если порт явно задан
            (env SWAGCOD_HTTP_PORT или prefs http_api_port). По
            умолчанию API выключен — поверхности нет. */
            if let Some(port) = httpapi::configured_port(&state) {
                let st = state.clone();
                tauri::async_runtime::spawn(async move {
                    if let Err(e) = httpapi::serve(st.clone(), port).await {
                        st.bus.publish(EventKind::Status { message: e });
                    }
                });
            }
            app.manage(state.clone());

            let handle = app.handle().clone();
            spawn_event_pump(
                handle,
                state.bus.clone(),
                state.lagging_events.clone(),
                state.lagging_dropped.clone(),
            );
            // B-8: watchdog живых ходов.
            spawn_watchdog(state.clone());

            state.bus.publish(swagcod_core::bus::EventKind::Status {
                message: format!("SwagCod {} запущен", env!("CARGO_PKG_VERSION")),
            });
            Ok(())
        })
        .invoke_handler(tauri::generate_handler![
            build_info,
            import_dsh_sessions,
            mcp_list,
            mcp_add,
            mcp_remove,
            js_plugins_list,
            js_plugins_reload,
            tasks_list,
            session_compact,
            task_cancel,
            task_add,
            initial_prefs,
            save_background,
            load_background,
            delete_background,
            session_transcript,
            save_session_log,
            list_sessions,
            create_session,
            delete_session,
            get_pref,
            set_pref,
            pty_spawn,
            pty_write,
            pty_resize,
            pty_kill,
            start_watch,
            stop_watch,
            search_files,
            diff_against_head,
            register_plugin_tool,
            list_plugin_tools,
            set_approval_policy,
            set_session_approval_policy,
            approval_log,
            save_protected_key,
            clear_protected_key,
            get_diagnostics,
            http_api_status,
            metrics_history,
            tokens_by_day,
            bus_seq,
            start_turn,
            stop_turn,
            respond_approval,
            list_dir,
            read_file,
            list_models,
            providers::list_provider_presets,
            providers::providers_state,
            providers::fetch_provider_models,
            providers::save_provider,
            providers::delete_provider,
            providers::set_active_provider,
            open_in_explorer,
            update_check,
            update_install,
            restart_app
        ])
        .run(tauri::generate_context!())
        .expect("не удалось запустить SwagCod");
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn subagent_model_override_and_fallback() {
        use serde_json::json;
        let parent = "pro-model";
        // Явная модель ветки — едет она.
        let args = json!({"prompt": "x", "model": "cheap-flash"})
            .as_object()
            .cloned()
            .unwrap();
        assert_eq!(subagent_model(&args, parent), "cheap-flash");
        // Пробелы по краям режутся.
        let args = json!({"prompt": "x", "model": "  cheap-flash  "})
            .as_object()
            .cloned()
            .unwrap();
        assert_eq!(subagent_model(&args, parent), "cheap-flash");
        // Нет/пусто/пробелы — модель родителя.
        let args = json!({"prompt": "x"}).as_object().cloned().unwrap();
        assert_eq!(subagent_model(&args, parent), parent);
        let args = json!({"prompt": "x", "model": ""})
            .as_object()
            .cloned()
            .unwrap();
        assert_eq!(subagent_model(&args, parent), parent);
        let args = json!({"prompt": "x", "model": "   "})
            .as_object()
            .cloned()
            .unwrap();
        assert_eq!(subagent_model(&args, parent), parent);
        // Не строка — тоже родитель, а не паника.
        let args = json!({"prompt": "x", "model": 42})
            .as_object()
            .cloned()
            .unwrap();
        assert_eq!(subagent_model(&args, parent), parent);
    }

    #[test]
    fn updater_config_is_wired() {
        // Контракт автообновления (rev37): endpoint смотрит в наш репозиторий,
        // публичный ключ minisign вшит, артефакты апдейтера включены. Тест
        // ловит потерю pubkey/endpoint ДО публикации релиза — урок
        // dsh-phone (tests/updater_release.rs).
        let conf: serde_json::Value = serde_json::from_str(include_str!("../tauri.conf.json"))
            .expect("tauri.conf.json — валидный JSON");
        let up = &conf["plugins"]["updater"];
        let eps = up["endpoints"]
            .as_array()
            .expect("updater endpoints — массив");
        assert!(
            eps.iter().any(|e| e.as_str()
                == Some(
                    "https://github.com/Swwag666/SwagCode/releases/latest/download/latest.json"
                )),
            "endpoint автообновления смотрит в releases Swwag666/SwagCode"
        );
        let pubkey = up["pubkey"].as_str().unwrap_or("");
        assert!(pubkey.len() > 50, "публичный ключ подписи вшит в конфиг");
        assert_eq!(
            conf["bundle"]["createUpdaterArtifacts"],
            serde_json::json!(true),
            "бандлер обязан подписывать артефакты апдейтера"
        );
        assert_eq!(conf["bundle"]["targets"][0], serde_json::json!("nsis"));
        // Ресурсы бандла: node.exe и sidecar едут в установщик (E-1).
        assert!(conf["bundle"]["resources"]["../../vendor/node/node.exe"].is_string());
        assert!(conf["bundle"]["resources"]["../../vendor/sidecar"].is_string());
    }

    #[tokio::test]
    async fn compact_force_on_tiny_history_folds_nothing() {
        // F-2: /compact на пустой или крошечной истории не должен трогать
        // саммаризатор: ноль свёрнутых, сводка и история неизменны
        // (ранний выход до сайд-запроса — сеть в тесте не нужна).
        let provider = AnyProvider::OpenAi(
            Router::from_env_with_key(Some("unit-test-key".into())).expect("роутер с явным ключом"),
        );
        let state = Arc::new(AppState::default());
        let (summary, history, folded) = run_compaction(
            &state,
            &provider,
            "s-none",
            "gpt-4o-mini",
            true,
            "old summary".into(),
            vec![],
        )
        .await;
        assert_eq!(folded, 0);
        assert_eq!(summary, "old summary");
        assert!(history.is_empty());

        // Крошечная история: split_history оставляет всё в recent —
        // сворачивать нечего даже с force.
        let tiny = vec![
            ChatMessage::user("привет"),
            ChatMessage::assistant("привет!"),
        ];
        let (_, hist2, folded2) = run_compaction(
            &state,
            &provider,
            "s-none",
            "gpt-4o-mini",
            true,
            String::new(),
            tiny.clone(),
        )
        .await;
        assert_eq!(folded2, 0);
        assert_eq!(hist2.len(), 2);

        // Без force и ниже порога B-3 — тоже ноль, провайдер не дёргается.
        let (_, hist3, folded3) = run_compaction(
            &state,
            &provider,
            "s-none",
            "gpt-4o-mini",
            false,
            String::new(),
            tiny,
        )
        .await;
        assert_eq!(folded3, 0);
        assert_eq!(hist3.len(), 2);
    }

    #[test]
    fn attachments_text_goes_in_binary_and_big_get_honest_note() {
        let dir = std::env::temp_dir().join(format!("swagcod-attach-{}", short_id()));
        std::fs::create_dir_all(&dir).unwrap();
        let txt = dir.join("note.txt");
        std::fs::write(&txt, "содержимое").unwrap();
        let bin = dir.join("data.bin");
        std::fs::write(&bin, [0xFF_u8, 0xFE, 0x00, 0x01]).unwrap();
        let big = dir.join("big.log");
        std::fs::write(&big, vec![b'x'; ATTACH_MAX_BYTES as usize + 1]).unwrap();
        let gone = dir.join("missing.txt");

        let out = render_attachments(&[
            txt.to_string_lossy().to_string(),
            bin.to_string_lossy().to_string(),
            big.to_string_lossy().to_string(),
            gone.to_string_lossy().to_string(),
        ]);

        // Текстовый файл — содержимым в блок кода.
        assert!(out.contains("Вложение: note.txt"), "{out}");
        assert!(out.contains("```\nсодержимое\n```"), "{out}");
        // Бинарный, слишком большой и отсутствующий — честные пометки, не тишина.
        assert!(
            out.contains("data.bin: не прикреплено — бинарный файл"),
            "{out}"
        );
        assert!(
            out.contains("big.log: не прикреплено — файл больше"),
            "{out}"
        );
        assert!(out.contains("missing.txt: не прикреплено —"), "{out}");

        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn attachments_images_are_marked_unsupported() {
        let out = render_attachments(&["C:/pics/cat.PNG".to_string()]);
        assert!(out.contains("cat.PNG"), "{out}");
        assert!(out.contains("картинки пока не поддерживаются"), "{out}");
    }

    /// E-2: сквозной тест локального вкуса — index_workspace собирает кэш
    /// из временного воркспейса, semantic_search находит нужный файл по
    /// лексическому запросу. Сеть не трогается: mode=local явно.
    #[tokio::test]
    async fn semantic_local_flavor_end_to_end() {
        let dir = std::env::temp_dir().join(format!("swagcod-semantic-{}", short_id()));
        std::fs::create_dir_all(dir.join("src")).unwrap();
        std::fs::write(
            dir.join("src/payments.rs"),
            "pub fn process_payment(order: &Order) -> Receipt {\n    approve_user_charge(order)\n}\n",
        )
        .unwrap();
        std::fs::write(
            dir.join("src/db.rs"),
            "pub fn open_store(path: &Path) -> Store {\n    /* sqlite */\n    todo!()\n}\n",
        )
        .unwrap();
        std::fs::write(dir.join("logo.png"), [0x89_u8, b'P', b'N', b'G']).unwrap();

        std::env::set_var("SWAGCOD_EMBEDDINGS_MODE", "local");
        std::env::set_var("SWAGCOD_SEMANTIC_DB", dir.join("embeddings.db"));
        let state = Arc::new(AppState::default());

        let indexed = index_workspace(&state, &dir).await.unwrap();
        assert_eq!(
            indexed, 2,
            "png не кандидат, два .rs обязаны проиндексироваться"
        );

        // Кулдаун свежего старта гасит фоновый respawn внутри run_semantic_search.
        state.semantic_last_ms.store(
            swagcod_core::bus::now_ms(),
            std::sync::atomic::Ordering::Relaxed,
        );

        let call = ToolCall {
            id: "call-sem-1".into(),
            name: "semantic_search".into(),
            arguments: serde_json::json!({"query": "process payment approve charge"})
                .as_object()
                .unwrap()
                .clone(),
        };
        let out = run_semantic_search(&state, &call, &dir).await;
        assert!(out.ok, "{}", out.output);
        assert!(
            out.output.contains("payments.rs"),
            "поиск не попал в payments.rs:\n{}",
            out.output
        );
        assert!(
            out.output.contains("лексический"),
            "локальный индекс обязан маркироваться:\n{}",
            out.output
        );

        // Повторный прогон ничего не переиндексирует: хэши совпали.
        let again = index_workspace(&state, &dir).await.unwrap();
        assert_eq!(again, 0, "неизменённые файлы не должны переустанавливаться");

        let empty = ToolCall {
            id: "call-sem-2".into(),
            name: "semantic_search".into(),
            arguments: serde_json::json!({"query": "   "})
                .as_object()
                .unwrap()
                .clone(),
        };
        let out = run_semantic_search(&state, &empty, &dir).await;
        assert!(
            !out.ok && out.output.contains("пустой запрос"),
            "{}",
            out.output
        );

        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn session_brief_falls_back_to_cwd_as_title() {
        let s = Session::new(SessionId::new("s1"), "D:/proj", "m");
        let b = SessionBrief::from(&s);
        assert_eq!(b.title, "D:/proj");
        assert_eq!(b.id, "s1");
        assert_eq!(b.turns, 0);
    }

    #[test]
    fn session_brief_uses_explicit_title() {
        let mut s = Session::new(SessionId::new("s1"), "D:/proj", "m");
        s.title = "Мой проект".into();
        assert_eq!(SessionBrief::from(&s).title, "Мой проект");
    }

    #[test]
    fn wire_event_preserves_seq_and_kind() {
        let ev = Event {
            seq: 42,
            ts_ms: 7,
            kind: swagcod_core::bus::EventKind::Content {
                turn: swagcod_core::TurnId::new("t"),
                text: "hi".into(),
            },
        };
        let w = WireEvent::from(ev.clone());
        assert_eq!(w.seq, 42);
        assert_eq!(w.ts_ms, 7);
        assert_eq!(w.kind, ev.kind);
    }

    #[test]
    fn wire_event_serializes_with_kind_tag() {
        let w = WireEvent {
            seq: 1,
            ts_ms: 2,
            kind: swagcod_core::bus::EventKind::Status {
                message: "ok".into(),
            },
        };
        let v = serde_json::to_value(&w).unwrap();
        assert_eq!(v["seq"], 1);
        assert_eq!(v["kind"]["kind"], "status");
    }

    #[test]
    fn build_info_reports_version_and_target() {
        let b = build_info();
        assert_eq!(b.version, env!("CARGO_PKG_VERSION"));
        assert!(b.target.contains('-'));
    }

    #[test]
    fn background_names_are_sanitized() {
        assert_eq!(sanitize_bg_name("clip.mp4").unwrap(), "clip.mp4");
        // Путь схлопывается до имени файла, но расширение чужое — отказ.
        assert!(sanitize_bg_name("../../etc/passwd").is_err());
        // Исполняемые файлы фоном не становятся.
        assert!(sanitize_bg_name("setup.exe").is_err());
        // Кириллица и пробелы вычищаются; без расширения — отказ.
        assert!(sanitize_bg_name("мой фон").is_err());
        assert_eq!(sanitize_bg_name("a/b/loop.GIF").unwrap(), "loop.GIF");
    }

    #[test]
    fn base64_roundtrip() {
        for len in [0usize, 1, 2, 3, 4, 5, 64, 1000] {
            let data: Vec<u8> = (0..len).map(|i| (i % 256) as u8).collect();
            let encoded = base64_encode(&data);
            let decoded = base64_decode(&encoded).expect("decode");
            assert_eq!(decoded, data, "roundtrip len={len}");
        }
        assert_eq!(base64_encode(b"SwagCod"), "U3dhZ0NvZA==");
    }

    #[tokio::test]
    async fn background_save_load_delete_roundtrip() {
        // Путь на диск обязан работать: раньше ошибка сохранения глоталась,
        // и медиафон «не ставился» без видимой причины.
        let name = format!("roundtrip-{}.gif", short_id());
        let data: Vec<u8> = (0..512).map(|i| (i % 256) as u8).collect();
        let b64 = base64_encode(&data);
        let saved = save_background(name.clone(), b64).await.expect("save");
        assert_eq!(saved, name);
        let loaded = load_background(name.clone()).await.expect("load");
        assert_eq!(base64_decode(&loaded).expect("decode"), data);
        delete_background(name.clone()).await.expect("delete");
        assert!(
            load_background(name).await.is_err(),
            "после удаления файл не должен читаться"
        );
    }

    #[test]
    fn short_ids_are_unique() {
        let ids: Vec<String> = (0..1000).map(|_| short_id()).collect();
        let unique: std::collections::HashSet<_> = ids.iter().collect();
        assert_eq!(unique.len(), ids.len(), "id должны быть уникальными");
    }

    #[tokio::test]
    async fn app_state_starts_with_empty_sessions() {
        let st = Arc::new(AppState::default());
        assert!(st.sessions.lock().await.is_empty());
        assert_eq!(st.bus.seq(), 0);
    }

    #[tokio::test]
    async fn publishing_to_state_bus_reaches_subscriber() {
        let st = Arc::new(AppState::default());
        let mut rx = st.bus.subscribe();
        st.bus.publish(swagcod_core::bus::EventKind::Status {
            message: "alive".into(),
        });
        let ev = rx.recv().await.unwrap();
        assert!(matches!(
            ev.kind,
            swagcod_core::bus::EventKind::Status { .. }
        ));
    }

    #[tokio::test]
    async fn approval_policy_defaults_to_on_dangerous() {
        let st = AppState::default();
        assert_eq!(
            st.config.lock().await.approval_policy,
            ApprovalPolicy::OnDangerous
        );
    }

    // ---- исполнитель тулзов: песочница по cwd сессии ----

    fn call(name: &str, args: serde_json::Value) -> ToolCall {
        ToolCall {
            id: "c1".into(),
            name: name.into(),
            arguments: args.as_object().cloned().unwrap_or_default(),
        }
    }

    #[tokio::test]
    async fn execute_tool_rejects_paths_outside_session_cwd() {
        let dir = std::env::temp_dir().join(format!("swagcod-sandbox-{}", short_id()));
        std::fs::create_dir_all(&dir).unwrap();
        std::fs::write(dir.join("inside.txt"), "ok").unwrap();
        let timeout = std::time::Duration::from_secs(5);

        let out = execute_tool(
            &call("read", serde_json::json!({"path": "../outside.txt"})),
            &dir,
            timeout,
            &[],
        )
        .await;
        assert!(!out.ok, "выход за cwd обязан отклоняться: {out:?}");

        let out = execute_tool(
            &call("read", serde_json::json!({"path": "inside.txt"})),
            &dir,
            timeout,
            &[],
        )
        .await;
        assert!(
            out.ok && out.output == "ok",
            "относительный путь внутри cwd: {out:?}"
        );

        let out = execute_tool(
            &call(
                "write",
                serde_json::json!({"path": "../../evil.txt", "content": "x"}),
            ),
            &dir,
            timeout,
            &[],
        )
        .await;
        assert!(!out.ok, "запись вне cwd обязана отклоняться: {out:?}");

        std::fs::remove_dir_all(&dir).ok();
    }

    #[tokio::test]
    async fn execute_tool_unknown_tool_is_a_result_not_a_panic() {
        let out = execute_tool(
            &call("fly", serde_json::json!({})),
            std::path::Path::new("."),
            std::time::Duration::from_secs(1),
            &[],
        )
        .await;
        assert!(!out.ok);
        assert!(
            out.output.contains("неизвестный инструмент"),
            "модель должна понять отказ: {}",
            out.output
        );
    }

    // ---- B-5: grep/glob/patch через исполнитель ----

    #[tokio::test]
    async fn execute_tool_grep_glob_patch_roundtrip() {
        let dir = std::env::temp_dir().join(format!("swagcod-b5-{}", short_id()));
        std::fs::create_dir_all(dir.join("src")).unwrap();
        std::fs::write(
            dir.join("src").join("a.rs"),
            "fn main() {\n    let x = 1;\n}\n",
        )
        .unwrap();
        std::fs::write(dir.join("b.txt"), "needle here\n").unwrap();
        let timeout = std::time::Duration::from_secs(5);

        let out = execute_tool(
            &call("grep", serde_json::json!({"pattern": "needle"})),
            &dir,
            timeout,
            &[],
        )
        .await;
        assert!(
            out.ok && out.output.contains("b.txt:1") || out.output.contains("b.txt"),
            "{out:?}"
        );
        assert!(out.output.contains("needle here"), "{out:?}");

        let out = execute_tool(
            &call("glob", serde_json::json!({"pattern": "*.rs"})),
            &dir,
            timeout,
            &[],
        )
        .await;
        assert!(out.ok && out.output.contains("src/a.rs"), "{out:?}");

        let out = execute_tool(
            &call(
                "patch",
                serde_json::json!({"path": "src/a.rs", "old_string": "let x = 1;", "new_string": "let x = 2;"}),
            ),
            &dir,
            timeout,
            &[],
        )
        .await;
        assert!(out.ok && out.output.contains("замен: 1"), "{out:?}");
        let patched = std::fs::read_to_string(dir.join("src").join("a.rs")).unwrap();
        assert!(patched.contains("let x = 2;"));

        // patch вне cwd отклоняется песочницей.
        let out = execute_tool(
            &call(
                "patch",
                serde_json::json!({"path": "../evil.rs", "old_string": "a", "new_string": "b"}),
            ),
            &dir,
            timeout,
            &[],
        )
        .await;
        assert!(!out.ok);

        std::fs::remove_dir_all(&dir).ok();
    }

    #[tokio::test]
    async fn execute_tool_fetch_url_rejects_non_http() {
        let out = execute_tool(
            &call(
                "fetch_url",
                serde_json::json!({"url": "file:///etc/passwd"}),
            ),
            std::path::Path::new("."),
            std::time::Duration::from_secs(5),
            &[],
        )
        .await;
        assert!(!out.ok);
        assert!(out.output.contains("http"), "{out:?}");
    }

    #[tokio::test]
    #[cfg(windows)]
    async fn plugin_tool_executes_registered_command() {
        let dir = std::env::temp_dir().join(format!("swagcod-plugin-{}", short_id()));
        std::fs::create_dir_all(&dir).unwrap();
        let plugin = PluginTool {
            name: "echo_args".into(),
            description: "test plugin".into(),
            parameters: serde_json::json!({"type": "object", "properties": {}}),
            // findstr читает stdin; просто отдадим текст через cmd /C echo.
            command: "echo plugin-out".into(),
        };
        let out = execute_tool(
            &call("echo_args", serde_json::json!({"a": 1})),
            &dir,
            std::time::Duration::from_secs(10),
            std::slice::from_ref(&plugin),
        )
        .await;
        assert!(out.ok, "{out:?}");
        assert!(out.output.contains("plugin-out"), "{out:?}");
        std::fs::remove_dir_all(&dir).ok();
    }

    // ---- B-8: watchdog и активность ходов ----

    #[test]
    fn watchdog_warns_only_quiet_turns() {
        let now = 1_000_000u64;
        let entries = vec![
            // Тихий 6 минут — предупреждать.
            ("t-quiet".to_string(), now - 6 * 60_000, false, None),
            // Активный 10 секунд назад — молчать.
            ("t-active".to_string(), now - 10_000, false, None),
            // Ждёт человека 10 минут — молчать: диалог не зависание.
            ("t-waiting".to_string(), now - 10 * 60_000, true, None),
            // Тихий, но предупреждён минуту назад — молчать до rewarn.
            (
                "t-warned".to_string(),
                now - 10 * 60_000,
                false,
                Some(now - 60_000),
            ),
            // Тихий и предупреждён 6 минут назад — снова предупреждать.
            (
                "t-rewarn".to_string(),
                now - 12 * 60_000,
                false,
                Some(now - 6 * 60_000),
            ),
        ];
        let mut warned = quiet_turns_to_warn(&entries, now, QUIET_TURN_MS, QUIET_REWARN_MS);
        warned.sort();
        let ids: Vec<&str> = warned.iter().map(|(t, _)| t.as_str()).collect();
        assert_eq!(ids, vec!["t-quiet", "t-rewarn"]);
        // idle_ms честный: для t-quiet это 6 минут.
        let quiet = warned.iter().find(|(t, _)| t == "t-quiet").unwrap();
        assert_eq!(quiet.1, 6 * 60_000);
    }

    #[test]
    fn watchdog_threshold_boundary_is_inclusive() {
        let now = 500_000u64;
        let exactly = vec![("t".to_string(), now - QUIET_TURN_MS, false, None)];
        assert_eq!(
            quiet_turns_to_warn(&exactly, now, QUIET_TURN_MS, QUIET_REWARN_MS).len(),
            1
        );
        let just_under = vec![("t".to_string(), now - QUIET_TURN_MS + 1, false, None)];
        assert!(quiet_turns_to_warn(&just_under, now, QUIET_TURN_MS, QUIET_REWARN_MS).is_empty());
    }

    // ---- E-8: сэмплер телеметрии ----

    #[test]
    fn metrics_sample_snapshots_counters_into_store() {
        let state = AppState {
            store: std::sync::Mutex::new(swagcod_core::store::open_memory()),
            ..Default::default()
        };
        state
            .lagging_events
            .store(7, std::sync::atomic::Ordering::Relaxed);
        state
            .lagging_dropped
            .store(2, std::sync::atomic::Ordering::Relaxed);
        state.turn_activity.lock().unwrap().insert(
            "t-wait".into(),
            TurnActivity {
                session: "s1".into(),
                started_ms: 0,
                last_ms: 0,
                waiting_human: true,
                warned_ms: None,
            },
        );
        let rows = sample_metrics(&state);
        let get = |n: &str| rows.iter().find(|(k, _)| *k == n).map(|(_, v)| *v);
        assert_eq!(get("lagging_events"), Some(7.0));
        assert_eq!(get("lagging_dropped"), Some(2.0));
        assert_eq!(get("active_turns"), Some(1.0));
        assert_eq!(get("waiting_human"), Some(1.0));
        assert_eq!(get("sessions"), Some(0.0));
        assert!(get("bus_seq").is_some());
        assert!(get("bus_subscribers").is_some());

        // Проход сэмплера кладёт точки в базу тем же значением.
        run_metrics_sample(&state);
        let g = state.store.lock().unwrap();
        let q = g.metrics_query("lagging_events", 0, 10).unwrap();
        assert_eq!(q.len(), 1);
        assert_eq!(q[0].1, 7.0);
        assert_eq!(g.metrics_query("waiting_human", 0, 10).unwrap().len(), 1);
        assert_eq!(g.metrics_query("bus_seq", 0, 10).unwrap().len(), 1);
    }

    #[test]
    fn touch_turn_updates_activity_and_human_flag() {
        let state = AppState::default();
        let now = swagcod_core::bus::now_ms();
        state.turn_activity.lock().unwrap().insert(
            "t1".into(),
            TurnActivity {
                session: "s1".into(),
                started_ms: now - 60_000,
                last_ms: now - 60_000,
                waiting_human: false,
                warned_ms: None,
            },
        );
        touch_turn(&state, "t1", Some(true));
        {
            let a = state.turn_activity.lock().unwrap();
            let e = a.get("t1").unwrap();
            assert!(e.waiting_human, "флаг ожидания человека установлен");
            assert!(e.last_ms >= now, "время жизни обновлено");
        }
        touch_turn(&state, "t1", Some(false));
        assert!(!state.turn_activity.lock().unwrap()["t1"].waiting_human);
        // Чужой ход не создаёт запись из воздуха.
        touch_turn(&state, "t-missing", None);
        assert!(!state
            .turn_activity
            .lock()
            .unwrap()
            .contains_key("t-missing"));
    }

    #[test]
    fn quiet_turns_pure_function_has_no_clock() {
        // Часы — параметр: функция детерминирована и не спит в тестах.
        let entries: Vec<(String, u64, bool, Option<u64>)> = vec![];
        assert!(quiet_turns_to_warn(&entries, 0, QUIET_TURN_MS, QUIET_REWARN_MS).is_empty());
    }

    #[test]
    fn lazy_sessions_roundtrip_d121() {
        use swagcod_core::session::{TurnId, TurnRecord};
        use swagcod_provider::types::ChatMessage;

        let state = Arc::new(AppState::default());
        let sid = "s-lazy-1";
        {
            let store = state.store.lock().unwrap();
            let mut s = Session::new(SessionId::new(sid), "C:\\tmp".to_string(), "m".to_string());
            s.created_ms = 1000;
            store.create_session(&s).unwrap();
            let rec = TurnRecord {
                id: TurnId::new("t-1"),
                started_ms: 1,
                ended_ms: Some(2),
                content: "привет".into(),
                reasoning: String::new(),
                tool_calls: vec![],
                est_input_tokens: 0,
                est_output_tokens: 0,
                ok: true,
                failure: None,
                parent_turn_id: None,
            };
            store
                .save_turn(
                    sid,
                    &rec,
                    &[ChatMessage::user("ау"), ChatMessage::assistant("привет")],
                )
                .unwrap();
        }

        // Полный список → «лёгкий»: история и журнал вычищены, цифры — в meta.
        let loaded = state.store.lock().unwrap().load_all().unwrap();
        assert_eq!(loaded.len(), 1);
        assert_eq!(loaded[0].turns.len(), 1);
        let (light, meta) = light_sessions(loaded);
        assert!(light[0].turns.is_empty() && light[0].history.is_empty());
        let m = meta.get(sid).unwrap();
        assert_eq!(m.turns, 1);
        assert_eq!(m.last_ok, Some(true));
        assert!(m.est_context_tokens > 0, "снимок помнит оценку контекста");
        *state.session_meta.lock().unwrap() = meta;
        *state.sessions.try_lock().unwrap() = light;

        // Регидратация по требованию возвращает всё из базы.
        {
            let mut sessions = state.sessions.try_lock().unwrap();
            let s = sessions.iter_mut().find(|s| s.id.as_str() == sid).unwrap();
            rehydrate_session(&state, s);
            assert_eq!(s.turns.len(), 1);
            assert_eq!(s.history.len(), 2);
            assert_eq!(s.history[0].role, swagcod_provider::types::Role::User);
        }
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn run_mcp_call_routes_to_live_server() {
        /* E-3: диспетчерный уровень — ToolCall с именем mcp:<server>:<tool>
        уходит в живой stdio-процесс и возвращается ToolOutcome. */
        let Some(node) = mcp::tests_node() else {
            return;
        };
        let dir = std::env::temp_dir().join(format!("swagcod-mcp-rt-{}", short_id()));
        std::fs::create_dir_all(&dir).unwrap();
        let script = dir.join("srv.js");
        std::fs::write(
            &script,
            r#"
const readline = require('readline');
const rl = readline.createInterface({ input: process.stdin });
const send = (o) => process.stdout.write(JSON.stringify(o) + '\n');
rl.on('line', (l) => {
  let m; try { m = JSON.parse(l); } catch { return; }
  if (m.method === 'initialize') send({jsonrpc:'2.0', id:m.id, result:{protocolVersion:'2024-11-05', capabilities:{}, serverInfo:{name:'rt', version:'0'}}});
  else if (m.method === 'tools/list') send({jsonrpc:'2.0', id:m.id, result:{tools:[{name:'echo', description:'эхо', inputSchema:{type:'object'}}]}});
  else if (m.method === 'tools/call') send({jsonrpc:'2.0', id:m.id, result:{content:[{type:'text', text:'rt:' + ((m.params.arguments||{}).text||'')}], isError:false}});
});
"#,
        )
        .unwrap();

        let state = Arc::new(AppState::default());
        let cfg = mcp::McpServerConfig {
            name: "rt".into(),
            command: node,
            args: vec![script.to_string_lossy().to_string()],
            env: std::collections::HashMap::new(),
            enabled: true,
        };
        {
            let mut reg = state.mcp.lock().await;
            assert_eq!(reg.connect_server(&cfg).await, 1);
        }

        let call = ToolCall {
            id: "c-1".into(),
            name: "mcp:rt:echo".into(),
            arguments: serde_json::json!({"text": "данные"})
                .as_object()
                .unwrap()
                .clone(),
        };
        let out = run_mcp_call(&state, &call).await;
        assert!(out.ok, "{}", out.output);
        assert_eq!(out.output, "rt:данные");

        // Незнакомый сервер — честная ошибка, а не паника.
        let ghost = ToolCall {
            id: "c-2".into(),
            name: "mcp:ghost:echo".into(),
            arguments: Default::default(),
        };
        let out = run_mcp_call(&state, &ghost).await;
        assert!(!out.ok);
        assert!(out.output.contains("не подключён"), "{}", out.output);

        state.mcp.lock().await.shutdown().await;
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn tasks_worker_runs_handlers_with_backoff() {
        use swagcod_core::session::{TurnId, TurnRecord};

        // open_memory: sidecar при доступном node (SQL-паритет обоих
        // бэкендов проверяется одним тестом), иначе rusqlite.
        let state = Arc::new(AppState {
            store: std::sync::Mutex::new(swagcod_core::store::open_memory()),
            ..Default::default()
        });

        let dir = std::env::temp_dir().join(format!("swagcod-tasks-{}", short_id()));
        std::fs::create_dir_all(&dir).unwrap();

        /* Живая сессия для journal_export: задача покрывает rehydrate-путь
        и запись wire-журнала B-8. */
        let sid = "task-sess";
        let mut s = Session::new(
            SessionId::new(sid),
            dir.to_string_lossy().to_string(),
            "test-model".to_string(),
        );
        s.turns.push(TurnRecord {
            id: TurnId::new("t-1"),
            started_ms: 100,
            ended_ms: Some(200),
            content: "готово".into(),
            reasoning: String::new(),
            tool_calls: vec![],
            est_input_tokens: 1,
            est_output_tokens: 2,
            ok: true,
            failure: None,
            parent_turn_id: None,
        });
        let out_path = dir.join("journal.jsonl");
        {
            let g = state.store.lock().unwrap();
            g.create_session(&s).unwrap();
            g.save_turn(sid, &s.turns[0], &s.history).unwrap();
            g.task_enqueue(&Task::new(
                "t-unknown",
                "no_such_kind",
                serde_json::json!({}),
                0,
            ))
            .unwrap();
            g.task_enqueue(&Task::new(
                "t-export",
                "journal_export",
                serde_json::json!({"session_id": sid, "path": out_path.to_string_lossy()}),
                0,
            ))
            .unwrap();
            g.task_enqueue(&Task::new(
                "t-git",
                "git_fetch",
                serde_json::json!({"cwd": dir.join("no-git").to_string_lossy()}),
                0,
            ))
            .unwrap();
        }
        state.sessions.lock().await.push(s);

        run_due_tasks(&state).await;

        {
            let g = state.store.lock().unwrap();

            // Успех: журнал записан, задача done, сообщение в last_error.
            let export = g.task_get("t-export").unwrap().unwrap();
            assert_eq!(export.state, TASK_DONE, "{export:?}");
            assert!(
                export.last_error.contains("событий"),
                "{:?}",
                export.last_error
            );
            let body = std::fs::read_to_string(&out_path).unwrap();
            assert!(
                body.contains(sid) && body.contains("turn_started"),
                "журнал записан"
            );

            // Неизвестный вид: первая ошибка — backoff-ретрай, не терминал.
            let unknown = g.task_get("t-unknown").unwrap().unwrap();
            assert_eq!(unknown.state, TASK_QUEUED);
            assert_eq!(unknown.attempts, 1);
            assert!(
                unknown.last_error.contains("неизвестный вид"),
                "{unknown:?}"
            );
            assert!(unknown.next_try_ms > 0, "следующая попытка назначена");

            // git_fetch на каталоге без репозитория — честная ошибка.
            let git = g.task_get("t-git").unwrap().unwrap();
            assert_eq!(git.state, TASK_QUEUED);
            assert_eq!(git.attempts, 1);
            assert!(git.last_error.contains("git_fetch"), "{git:?}");
        }

        // Исчерпание попыток → терминальный failed.
        for _ in 1..TASKS_MAX_ATTEMPTS {
            {
                let g = state.store.lock().unwrap();
                let t = g.task_get("t-unknown").unwrap().unwrap();
                g.task_update("t-unknown", TASK_QUEUED, &t.last_error, t.attempts, 0)
                    .unwrap();
            }
            run_due_tasks(&state).await;
        }
        {
            let g = state.store.lock().unwrap();
            let t = g.task_get("t-unknown").unwrap().unwrap();
            assert_eq!(
                t.state, TASK_FAILED,
                "терминал после {TASKS_MAX_ATTEMPTS} попыток: {t:?}"
            );
            assert!(t.attempts >= TASKS_MAX_ATTEMPTS);
        }

        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn subagent_limits_args_env_defaults() {
        // E-6: приоритет — аргументы вызова, затем env, затем дефолты;
        // клампы не пускают ни нули, ни абсурд.
        std::env::remove_var("SWAGCOD_SUBAGENT_MAX_ROUNDS");
        std::env::remove_var("SWAGCOD_SUBAGENT_BUDGET_TOKENS");
        let empty = serde_json::Map::new();
        assert_eq!(
            subagent_limits(&empty),
            (SUBAGENT_MAX_ROUNDS, SUBAGENT_BUDGET_TOKENS)
        );
        let wild = serde_json::json!({"max_rounds": 99, "budget_tokens": 1})
            .as_object()
            .unwrap()
            .clone();
        assert_eq!(subagent_limits(&wild), (12, 1_000));
        std::env::set_var("SWAGCOD_SUBAGENT_MAX_ROUNDS", "4");
        assert_eq!(subagent_limits(&empty).0, 4);
        // Аргументы сильнее env.
        assert_eq!(subagent_limits(&wild).0, 12);
        std::env::remove_var("SWAGCOD_SUBAGENT_MAX_ROUNDS");
    }

    #[test]
    fn subagent_safe_tools_exclude_dangerous() {
        // E-6: whitelist ветки не пересекается с опасными инструментами —
        // подтверждений из суб-агента не бывает ни при какой политике.
        for name in SUBAGENT_SAFE_TOOLS {
            assert!(
                !swagcod_core::turn::ApprovalPolicy::OnDangerous.requires_approval(name),
                "{name} не должен требовать подтверждения"
            );
        }
        for bad in ["write", "bash", "patch", "edit"] {
            assert!(
                !SUBAGENT_SAFE_TOOLS.contains(&bad),
                "{bad} просочился в whitelist"
            );
        }
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn subagent_runs_readonly_branch_and_records_child_turn() {
        use std::sync::atomic::{AtomicUsize, Ordering};
        use tokio::io::{AsyncReadExt, AsyncWriteExt};
        use tokio::net::TcpListener;

        /* Фейк OpenAI-совместимого SSE-провайдера. Ответы по номеру
        запроса: 1 → tool_call read, 2 → финальный текст,
        3 → tool_call read (фаза предела раундов), 4 → tool_call bash
        (фаза отказа опасного инструмента), 5 → финальный текст. */
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let port = listener.local_addr().unwrap().port();
        let hits = Arc::new(AtomicUsize::new(0));
        let srv_hits = hits.clone();
        tokio::spawn(async move {
            loop {
                let Ok((mut sock, _)) = listener.accept().await else {
                    break;
                };
                let n = srv_hits.fetch_add(1, Ordering::SeqCst) + 1;
                tokio::spawn(async move {
                    let mut buf = [0u8; 16384];
                    let _ = sock.read(&mut buf).await;
                    let body = match n {
                        2 => "data: {\"choices\":[{\"delta\":{\"content\":\"ОТЧЁТ ВЕТКИ\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n".to_string(),
                        5 => "data: {\"choices\":[{\"delta\":{\"content\":\"ГОТОВО\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n".to_string(),
                        4 => "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"cb\",\"type\":\"function\",\"function\":{\"name\":\"bash\",\"arguments\":\"{\\\"command\\\": \\\"pwnt\\\"}\"}}]},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n".to_string(),
                        _ => "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"type\":\"function\",\"function\":{\"name\":\"read\",\"arguments\":\"{\\\"path\\\": \\\"probe.txt\\\"}\"}}]},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n".to_string(),
                    };
                    let resp = format!(
                        "HTTP/1.1 200 OK\r\ncontent-type: text/event-stream\r\ncontent-length: {}\r\n\r\n{}",
                        body.len(),
                        body
                    );
                    let _ = sock.write_all(resp.as_bytes()).await;
                    let _ = sock.shutdown().await;
                });
            }
        });

        std::env::set_var("SWAGCOD_BASE_URL", format!("http://127.0.0.1:{port}/v1"));
        std::env::set_var("SWAGCOD_API_KEY", "test-key");
        let provider = AnyProvider::OpenAi(Router::from_env_with_key(None).expect("роутер из env"));

        let state = Arc::new(AppState {
            store: std::sync::Mutex::new(swagcod_core::store::open_memory()),
            ..Default::default()
        });
        let dir = std::env::temp_dir().join(format!("swagcod-sub-{}", short_id()));
        std::fs::create_dir_all(&dir).unwrap();
        std::fs::write(dir.join("probe.txt"), "СЕКРЕТ ВЕТКИ 42").unwrap();
        let sid = "sub-sess";
        let s = Session::new(
            SessionId::new(sid),
            dir.to_string_lossy().to_string(),
            "test-model".to_string(),
        );
        {
            let g = state.store.lock().unwrap();
            g.create_session(&s).unwrap();
        }
        state.sessions.lock().await.push(s);

        // Фаза A: чистый финиш — tool-раунд (read реально исполняется) + отчёт.
        let call = ToolCall {
            id: "call-a".into(),
            name: "subagent".into(),
            arguments: serde_json::json!({
                "prompt": "прочитай probe.txt и отчитайся",
                "max_rounds": 3
            })
            .as_object()
            .unwrap()
            .clone(),
        };
        let out = run_subagent(
            &state,
            &provider,
            &call,
            sid,
            "parent-1",
            "test-model",
            None,
            &dir,
            "",
            std::time::Duration::from_secs(5),
            &[],
        )
        .await;
        assert!(out.ok, "{}", out.output);
        assert!(out.output.contains("ОТЧЁТ ВЕТКИ"), "{}", out.output);
        assert!(
            out.output.contains("[суб-агент parent-1-sub-"),
            "{}",
            out.output
        );

        {
            let g = state.store.lock().unwrap();
            let loaded = g.load_session(sid).unwrap().expect("сессия есть");
            assert_eq!(loaded.turns.len(), 1);
            let child = &loaded.turns[0];
            assert!(child.id.as_str().starts_with("parent-1-sub-"));
            assert_eq!(
                child.parent_turn_id.as_ref().map(|p| p.as_str()),
                Some("parent-1")
            );
            assert!(child.ok);
            assert_eq!(child.content, "ОТЧЁТ ВЕТКИ");
            assert_eq!(child.tool_calls.len(), 1);
            assert_eq!(child.tool_calls[0].name, "read");
            assert!(child.est_input_tokens > 0, "честный счёт токенов");
        }
        {
            let live = state.sessions.lock().await;
            let ls = live.iter().find(|s| s.id.as_str() == sid).unwrap();
            assert_eq!(ls.turns.len(), 1, "ветка видна в живой сессии сразу");
        }

        // Фаза B: предел раундов — честный провал с частичным отчётом.
        let call_b = ToolCall {
            id: "call-b".into(),
            name: "subagent".into(),
            arguments: serde_json::json!({"prompt": "читай вечно", "max_rounds": 1})
                .as_object()
                .unwrap()
                .clone(),
        };
        let out_b = run_subagent(
            &state,
            &provider,
            &call_b,
            sid,
            "parent-2",
            "test-model",
            None,
            &dir,
            "",
            std::time::Duration::from_secs(5),
            &[],
        )
        .await;
        assert!(!out_b.ok);
        assert!(out_b.output.contains("предел раундов"), "{}", out_b.output);

        // Фаза C: опасный инструмент модель «выдумала» — ветка отказывает,
        // а не исполняет, и дочитывает до чистого финиша.
        let call_c = ToolCall {
            id: "call-c".into(),
            name: "subagent".into(),
            arguments: serde_json::json!({"prompt": "сломай всё", "max_rounds": 3})
                .as_object()
                .unwrap()
                .clone(),
        };
        let out_c = run_subagent(
            &state,
            &provider,
            &call_c,
            sid,
            "parent-3",
            "test-model",
            None,
            &dir,
            "",
            std::time::Duration::from_secs(5),
            &[],
        )
        .await;
        assert!(out_c.ok, "{}", out_c.output);
        assert!(out_c.output.contains("ГОТОВО"), "{}", out_c.output);

        {
            let g = state.store.lock().unwrap();
            let loaded = g.load_session(sid).unwrap().unwrap();
            assert_eq!(loaded.turns.len(), 3);
            let c = loaded
                .turns
                .iter()
                .find(|t| t.id.as_str().starts_with("parent-3-sub-"))
                .unwrap();
            assert_eq!(c.tool_calls.len(), 1);
            assert_eq!(c.tool_calls[0].name, "bash");
            assert_eq!(c.content, "ГОТОВО");
            let b = loaded
                .turns
                .iter()
                .find(|t| t.id.as_str().starts_with("parent-2-sub-"))
                .unwrap();
            assert!(!b.ok);
            assert!(b.failure.as_deref().unwrap().contains("предел раундов"));
        }
        assert_eq!(
            hits.load(Ordering::SeqCst),
            5,
            "ровно пять запросов к провайдеру"
        );

        std::env::remove_var("SWAGCOD_BASE_URL");
        std::env::remove_var("SWAGCOD_API_KEY");
        let _ = std::fs::remove_dir_all(&dir);
    }
}
