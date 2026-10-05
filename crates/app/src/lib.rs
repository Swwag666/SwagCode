/*! Тонкая Tauri-обёртка (D-011).

**Правило этого крейта: здесь не живёт бизнес-логика.** Всё содержательное —
в `swagcod-core`, `swagcod-provider`, `swagcod-pty`, `swagcod-fsx`. Этот файл
делает ровно три вещи: держит состояние, пробрасывает события шины в UI и
отдаёт команды. Причина — именно такой слой в Electron-версиях разрастается и
начинает тормозить, а тонкую обёртку легко переписать под другой шелл.
*/

use std::sync::Arc;

use serde::Serialize;
use swagcod_core::bus::{Bus, Event};
use swagcod_core::session::{Session, SessionId};
use swagcod_core::turn::{ApprovalPolicy, TurnConfig};
use swagcod_provider::{ChatRequest, Provider, StreamEvent};
use tauri::{AppHandle, Emitter, Manager, State};
use tokio::sync::Mutex;

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
}

impl Default for AppState {
    fn default() -> Self {
        Self {
            bus: Bus::new(),
            sessions: Mutex::new(Vec::new()),
            config: Mutex::new(TurnConfig::default()),
            turns: Mutex::new(std::collections::HashMap::new()),
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
    if cwd.is_empty() {
        return Err("cwd не может быть пустым".into());
    }
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

/// Запустить ход: сообщение пользователя → провайдер → стрим в шину.
///
/// Это живой стрим с реального эндпоинта (Этап 1). События публикуются
/// в шину по мере поступления, UI батчит их по кадрам.
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
    // Если ключ не задан — сессия остаётся Idle, пользователь видит ошибку.
    let provider = Provider::from_env().map_err(|e| format!("провайдер: {e}"))?;

    // Находим сессию
    let (turn_id, history, session_model) = {
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

        // Устанавливаем текущий ход и статус
        let turn_id = swagcod_core::TurnId::new(format!("t-{}", short_id()));
        session.current_turn = Some(turn_id.clone());
        session.status = swagcod_core::session::SessionStatus::Running;

        // История уже в wire-формате провайдера
        let history = session.history.clone();
        let session_model = session.model.clone();
        let turn_id = turn_id.to_string();
        (turn_id, history, session_model)
    };

    // W-6 фикс: используем переданную модель если есть, иначе из сессии
    let effective_model = model.unwrap_or(session_model);
    let request = ChatRequest {
        model: effective_model,
        messages: history,
        stream: true,
        temperature: temperature.map(|t| t as f32),
        max_tokens: None,
        tools: Vec::new(),
    };

    let bus = state.bus.clone();
    let turn = turn_id.clone();
    let sid = session_id.clone();
    let app_state = state.inner().clone();
    let cancelled = Arc::new(std::sync::atomic::AtomicBool::new(false));
    let cancelled_in_task = cancelled.clone();
    let started_ms = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_millis() as u64)
        .unwrap_or(0);

    // Запускаем стрим в фоне
    let task = tauri::async_runtime::spawn(async move {
        // C2-фикс: накапливаем ответ модели для сохранения в историю
        let mut content_buf = String::new();
        let mut reasoning_buf = String::new();
        let mut turn_ok = false;
        let mut stopped = false;

        match provider.stream(request) {
            Ok((mut rx, handle)) => {
                while let Some(event) = rx.recv().await {
                    // Кнопка «стоп» в UI: выходим из цикла и гасим HTTP-задачу.
                    if cancelled_in_task.load(std::sync::atomic::Ordering::SeqCst) {
                        stopped = true;
                        handle.abort();
                        break;
                    }
                    let kind = match &event {
                        StreamEvent::Reasoning(text) => {
                            reasoning_buf.push_str(text);
                            swagcod_core::bus::EventKind::Reasoning {
                                turn: swagcod_core::TurnId::new(&turn),
                                text: text.clone(),
                            }
                        }
                        StreamEvent::Content(text) => {
                            content_buf.push_str(text);
                            swagcod_core::bus::EventKind::Content {
                                turn: swagcod_core::TurnId::new(&turn),
                                text: text.clone(),
                            }
                        }
                        StreamEvent::ToolCallStart { index, id, name } => {
                            swagcod_core::bus::EventKind::ToolCall {
                                turn: swagcod_core::TurnId::new(&turn),
                                call_id: id.clone(),
                                name: name.clone(),
                                arguments: serde_json::json!({ "index": index }),
                            }
                        }
                        StreamEvent::ToolCallComplete(tc) => {
                            swagcod_core::bus::EventKind::ToolCall {
                                turn: swagcod_core::TurnId::new(&turn),
                                call_id: tc.id.clone(),
                                name: tc.name.clone(),
                                arguments: serde_json::Value::Object(tc.arguments.clone()),
                            }
                        }
                        StreamEvent::Done { finish } => {
                            turn_ok = true;
                            swagcod_core::bus::EventKind::TurnEnded {
                                turn: swagcod_core::TurnId::new(&turn),
                                session: swagcod_core::SessionId::new(&sid),
                                ok: true,
                                reason: Some(format!("{finish:?}")),
                            }
                        }
                        StreamEvent::Error(msg) => swagcod_core::bus::EventKind::TurnEnded {
                            turn: swagcod_core::TurnId::new(&turn),
                            session: swagcod_core::SessionId::new(&sid),
                            ok: false,
                            reason: Some(msg.clone()),
                        },
                    };
                    bus.publish(kind);
                }
            }
            Err(e) => {
                bus.publish(swagcod_core::bus::EventKind::TurnEnded {
                    turn: swagcod_core::TurnId::new(&turn),
                    session: swagcod_core::SessionId::new(&sid),
                    ok: false,
                    reason: Some(format!("ошибка стрима: {e}")),
                });
            }
        }

        // Остановка пользователем — это отдельный исход хода, не ошибка сети.
        if stopped {
            bus.publish(swagcod_core::bus::EventKind::TurnEnded {
                turn: swagcod_core::TurnId::new(&turn),
                session: swagcod_core::SessionId::new(&sid),
                ok: false,
                reason: Some("остановлено пользователем".to_string()),
            });
        }

        // C2-фикс: сохраняем ответ модели в историю сессии
        if (turn_ok || stopped) && !content_buf.is_empty() {
            let mut sessions = app_state.sessions.lock().await;
            if let Some(session) = sessions.iter_mut().find(|s| s.id.to_string() == sid) {
                let msg = swagcod_provider::ChatMessage {
                    role: swagcod_provider::Role::Assistant,
                    content: content_buf.clone(),
                    reasoning: reasoning_buf.clone(),
                    tool_calls: Vec::new(),
                    tool_call_id: None,
                };
                session.push_assistant_message(msg);
            }
        }

        // Журнал хода: список сессий показывает счётчик ходов и исход
        // последнего («успех/промах») именно из этого вектора.
        let ended_ms = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map(|d| d.as_millis() as u64)
            .unwrap_or(0);
        {
            let mut sessions = app_state.sessions.lock().await;
            if let Some(session) = sessions.iter_mut().find(|s| s.id.to_string() == sid) {
                session.turns.push(swagcod_core::session::TurnRecord {
                    id: swagcod_core::TurnId::new(&turn),
                    started_ms,
                    ended_ms: Some(ended_ms),
                    content: content_buf.clone(),
                    reasoning: reasoning_buf.clone(),
                    tool_calls: Vec::new(),
                    est_input_tokens: 0,
                    est_output_tokens: 0,
                    ok: turn_ok,
                    failure: if stopped {
                        Some("остановлено пользователем".to_string())
                    } else if turn_ok {
                        None
                    } else {
                        Some("ход не завершился успешно".to_string())
                    },
                });
            }
        }

        // Сбрасываем статус сессии после завершения
        let mut sessions = app_state.sessions.lock().await;
        if let Some(session) = sessions.iter_mut().find(|s| s.id.to_string() == sid) {
            session.status = if stopped {
                swagcod_core::session::SessionStatus::Cancelled
            } else {
                swagcod_core::session::SessionStatus::Idle
            };
            session.current_turn = None;
        }
        drop(sessions);

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

#[tauri::command]
async fn list_dir(path: String) -> Result<Vec<FileEntry>, String> {
    // C4-фикс: песочница — только внутри cwd сессии
    let dir = std::path::Path::new(&path);
    if !dir.is_dir() {
        return Err(format!("не директория: {path}"));
    }

    // Проверяем что путь внутри разрешённой зоны (D:\SwagCod)
    let root = std::path::Path::new("D:\\SwagCod");
    if !swagcod_fsx::is_within(root, dir) {
        return Err("доступ запрещён: путь вне рабочей директории".into());
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

/// Запуск команды в рабочей директории (для терминала UI).
#[derive(Debug, Clone, Serialize)]
pub struct CommandResult {
    pub stdout: String,
    pub stderr: String,
    pub code: i32,
}

#[tauri::command]
async fn run_command(command: String, cwd: String) -> Result<CommandResult, String> {
    let command = command.trim().to_string();
    if command.is_empty() {
        return Err("пустая команда".into());
    }

    // Безопасность: запускаем через cmd /c на Windows
    let output = tokio::process::Command::new("cmd")
        .arg("/C")
        .arg(&command)
        .current_dir(&cwd)
        .output()
        .await
        .map_err(|e| format!("запуск: {e}"))?;

    Ok(CommandResult {
        stdout: String::from_utf8_lossy(&output.stdout).to_string(),
        stderr: String::from_utf8_lossy(&output.stderr).to_string(),
        code: output.status.code().unwrap_or(-1),
    })
}

/// Чтение файла (для просмотра содержимого и diff).
#[tauri::command]
async fn read_file(path: String) -> Result<String, String> {
    let path = std::path::Path::new(&path);
    if !path.is_file() {
        return Err(format!("не файл: {path:?}"));
    }

    // C4-фикс: песочница
    let root = std::path::Path::new("D:\\SwagCod");
    if !swagcod_fsx::is_within(root, path) {
        return Err("доступ запрещён: путь вне рабочей директории".into());
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
async fn open_in_explorer(path: String) -> Result<(), String> {
    // C-1 фикс: песочница + путь как аргумент, не склейка
    let p = std::path::Path::new(&path);
    let root = std::path::Path::new("D:\\SwagCod");
    if !swagcod_fsx::is_within(root, p) {
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
            list_sessions,
            create_session,
            set_approval_policy,
            bus_seq,
            start_turn,
            stop_turn,
            list_dir,
            run_command,
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
}
