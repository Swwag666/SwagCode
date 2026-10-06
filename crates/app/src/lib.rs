/*! Тонкая Tauri-обёртка (D-011).

**Правило этого крейта: здесь не живёт бизнес-логика.** Всё содержательное —
в `swagcod-core`, `swagcod-provider`, `swagcod-pty`, `swagcod-fsx`. Этот файл
делает ровно три вещи: держит состояние, пробрасывает события шины в UI и
отдаёт команды. Причина — именно такой слой в Electron-версиях разрастается и
начинает тормозить, а тонкую обёртку легко переписать под другой шелл.
*/

use std::sync::Arc;

use serde::Serialize;
use swagcod_core::bus::{Bus, Event, EventKind};
use swagcod_core::session::{Session, SessionId};
use swagcod_core::turn::{
    builtin_tool_specs, describe_call, truncate_output, ApprovalDecision, ApprovalPolicy,
    ToolOutcome, TurnConfig, TurnMachine, TurnOutcome, TurnStep,
};
use swagcod_provider::types::ToolCall;
use swagcod_provider::{ChatRequest, Provider, StreamEvent};
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
}

impl Default for AppState {
    fn default() -> Self {
        Self {
            bus: Bus::new(),
            sessions: Mutex::new(Vec::new()),
            config: Mutex::new(TurnConfig::default()),
            turns: Mutex::new(std::collections::HashMap::new()),
            approvals: Mutex::new(std::collections::HashMap::new()),
        }
    }
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
fn spawn_event_pump(app: AppHandle, bus: Bus) -> tauri::async_runtime::JoinHandle<()> {
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
    let file_name = name
        .rsplit(['/', '\\'])
        .next()
        .unwrap_or_default()
        .trim();
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
#[tauri::command]
async fn session_transcript(
    state: State<'_, Arc<AppState>>,
    session_id: String,
) -> Result<Vec<serde_json::Value>, String> {
    use serde_json::{json, Value};
    use swagcod_provider::types::Role;

    let guard = state.sessions.lock().await;
    let session = guard
        .iter()
        .find(|s| s.id.as_str() == session_id)
        .ok_or_else(|| format!("сессия {session_id} не найдена"))?;

    let mut user_texts: std::collections::VecDeque<String> = std::collections::VecDeque::new();
    let mut tool_outputs: std::collections::HashMap<String, String> = std::collections::HashMap::new();
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
        out.push(json!({
            "seq": 0, "ts_ms": ts,
            "kind": { "kind": "turn_started", "data": { "turn": tid, "session": session_id } }
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
    let guard = state.sessions.lock().await;
    Ok(guard.iter().map(SessionBrief::from).collect())
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
) -> Result<String, String> {
    let message = message.trim().to_string();
    if message.is_empty() {
        return Err("сообщение не может быть пустым".into());
    }

    // C1-фикс: создаём провайдер ДО мутации статуса сессии.
    let provider = Provider::from_env().map_err(|e| format!("провайдер: {e}"))?;

    let (turn_id, history, session_model, cwd, cfg) = {
        let mut sessions = state.sessions.lock().await;
        let session = sessions
            .iter_mut()
            .find(|s| s.id.to_string() == session_id)
            .ok_or_else(|| format!("сессия не найдена: {session_id}"))?;

        if !session.status.can_start_turn() {
            return Err(format!("сессия занята: {:?}", session.status));
        }

        let ok = session.push_user_message(&message);
        if !ok {
            return Err("не удалось начать ход".into());
        }

        let turn_id = swagcod_core::TurnId::new(format!("t-{}", short_id()));
        session.current_turn = Some(turn_id.clone());
        session.status = swagcod_core::session::SessionStatus::Running;

        let cfg = state.config.lock().await.clone();
        (
            turn_id.to_string(),
            session.history.clone(),
            session.model.clone(),
            session.cwd.clone(),
            cfg,
        )
    };

    // W-6 фикс: используем переданную модель если есть, иначе из сессии.
    let effective_model = model.unwrap_or(session_model);
    // Тулзы уходят в каждом запросе: без определений модель не может их вызвать.
    let tool_wire: Vec<serde_json::Value> =
        ChatRequest::new(&effective_model, Vec::new()).with_tools(&builtin_tool_specs()).tools;
    let temperature = temperature.map(|t| t as f32);

    let bus = state.bus.clone();
    let turn = turn_id.clone();
    let sid = session_id.clone();
    let app_state = state.inner().clone();
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
        });
        let (mut machine, mut step) = TurnMachine::new(cfg, history);
        let mut fail_reason: Option<String> = None;

        let outcome = loop {
            if cancelled_in_task.load(std::sync::atomic::Ordering::SeqCst) {
                step = machine.cancel();
            }
            match step {
                TurnStep::RequestModel { history } => {
                    let request = ChatRequest {
                        model: effective_model.clone(),
                        messages: history,
                        stream: true,
                        temperature,
                        max_tokens: None,
                        tools: tool_wire.clone(),
                    };
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
                    let mut stream_error: Option<String> = None;
                    while let Some(event) = rx.recv().await {
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
                    let decision = rx.await.unwrap_or(ApprovalDecision::Denied);
                    app_state.approvals.lock().await.remove(&call_id);
                    step = machine.on_approval(decision);
                }
                TurnStep::ExecuteTool { call } => {
                    let started = std::time::Instant::now();
                    let tool_outcome = execute_tool(&call, &cwd, tool_timeout).await;
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
        let record = report.into_record(tid.clone(), calls, 0, 0);
        {
            let mut sessions = app_state.sessions.lock().await;
            if let Some(session) = sessions.iter_mut().find(|s| s.id.to_string() == sid) {
                // История сессии — это история машины: assistant с tool_calls
                // всегда закрыт ответами tool, следующий запрос валиден.
                session.history = machine.history().to_vec();
                session.turns.push(record);
                session.status = match outcome {
                    TurnOutcome::Cancelled => swagcod_core::session::SessionStatus::Cancelled,
                    TurnOutcome::Failed => swagcod_core::session::SessionStatus::Failed,
                    _ => swagcod_core::session::SessionStatus::Idle,
                };
                session.current_turn = None;
            }
        }

        // Ход закончился — убираем ручку, чтобы «стоп» не бил по прошлому.
        let mut turns = app_state.turns.lock().await;
        turns.remove(&turn);
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

/// Выполнить один вызов. Не паникует и не возвращает Err: любой отказ
/// становится результатом ok=false, и модель видит его как ответ тулза.
async fn execute_tool(
    call: &ToolCall,
    cwd: &std::path::Path,
    timeout: std::time::Duration,
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
                Ok(s) => ToolOutcome { ok: true, output: s },
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
        "bash" => {
            let command = arg_str(call, "command");
            if command.trim().is_empty() {
                return fail("пустая команда".into());
            }
            let child = tokio::process::Command::new("cmd")
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
                        let _ = tokio::process::Command::new("taskkill")
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
        other => fail(format!("неизвестный инструмент: {other}")),
    }
}

/// Решение человека по вызову: диалог подтверждения в UI стреляет этой командой.
#[tauri::command]
async fn respond_approval(
    state: State<'_, Arc<AppState>>,
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

/// Список моделей провайдера.
#[tauri::command]
async fn list_models() -> Result<serde_json::Value, String> {
    let provider = Provider::from_env().map_err(|e| format!("провайдер: {e}"))?;
    provider
        .list_models()
        .await
        .map_err(|e| format!("модели: {e}"))
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
    tokio::process::Command::new("explorer")
        .arg(format!("/select,{}", path))
        .spawn()
        .map_err(|e| format!("explorer: {e}"))?;
    Ok(())
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

pub fn run() {
    // Загружаем .env если есть (ключ провайдера, базовый URL, модель).
    // Не фатально если файла нет — переменные могут быть в окружении.
    let _ = dotenvy::dotenv();

    tauri::Builder::default()
        .plugin(tauri_plugin_dialog::init())
        .plugin(tauri_plugin_single_instance::init(|app, _argv, _cwd| {
            if let Some(w) = app.get_webview_window("main") {
                let _ = w.show();
                let _ = w.unminimize();
                let _ = w.set_focus();
            }
        }))
        .setup(|app| {
            let state = Arc::new(AppState::default());
            app.manage(state.clone());

            let handle = app.handle().clone();
            spawn_event_pump(handle, state.bus.clone());

            state.bus.publish(swagcod_core::bus::EventKind::Status {
                message: format!("SwagCod {} запущен", env!("CARGO_PKG_VERSION")),
            });
            Ok(())
        })
        .invoke_handler(tauri::generate_handler![
            build_info,
            initial_prefs,
            save_background,
            load_background,
            delete_background,
            session_transcript,
            save_session_log,
            list_sessions,
            create_session,
            set_approval_policy,
            bus_seq,
            start_turn,
            stop_turn,
            respond_approval,
            list_dir,
            read_file,
            list_models,
            open_in_explorer
        ])
        .run(tauri::generate_context!())
        .expect("не удалось запустить SwagCod");
}

#[cfg(test)]
mod tests {
    use super::*;

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
        )
        .await;
        assert!(!out.ok, "выход за cwd обязан отклоняться: {out:?}");

        let out = execute_tool(
            &call("read", serde_json::json!({"path": "inside.txt"})),
            &dir,
            timeout,
        )
        .await;
        assert!(out.ok && out.output == "ok", "относительный путь внутри cwd: {out:?}");

        let out = execute_tool(
            &call("write", serde_json::json!({"path": "../../evil.txt", "content": "x"})),
            &dir,
            timeout,
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
        )
        .await;
        assert!(!out.ok);
        assert!(
            out.output.contains("неизвестный инструмент"),
            "модель должна понять отказ: {}",
            out.output
        );
    }
}
