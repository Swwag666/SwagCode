/*! probe_flow_phone: бета-прогон «телефонного» клиента по loopback HTTP API.

Симулирует НЕ кривого, а обычного юзера с телефона: Phone открывает сессию
в реальной временной папке, читает ленту, шлёт ходы, ловит SSE до события
конца хода, подтверждает тулзы, останавливает ход, роняет и переподнимает
SSE-соединение. Запросы честные: правильный Bearer-токен, настоящий
Content-Length, валидный JSON, connection: close как настоящий клиент,
параллельные запросы — живой темп.

Зона кривых рук (мусорные байты, битые CL, чужие методы) живёт в
probe_http.rs и здесь не дублируется.

Каркас как у e2e_smoke: echo-провайдер через env-флаг, эфемерный порт,
фиксированный токен, `serve_on` без DPAPI. Контракт ожиданий —
crates/app/src/httpapi.rs: 200 на живых маршрутах, JSON-тела, SSE-кадры
`data: {…}` до turn_ended.

Ограничение echo-провайдера, честно зафиксированное: эхо не вызывает
инструменты, поэтому сценарий «ход ждёт подтверждения тула» проверяется
по клиентской половине — событие approval_required по SSE и решение
через POST /v1/approvals до РЕАЛЬНО ожидающего oneshot-канала (его кладёт
в state.approvals сам тест тем же способом, что и драйвер хода).

Кейсы, которые падают (находки), помечаются #[ignore = "FINDING: …"]:
репро живёт в файле, cargo test остаётся зелёным. */

use std::sync::Arc;
use std::time::Duration;

use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::{TcpListener, TcpStream};

use swagcod_app_lib::httpapi::serve_on;
use swagcod_app_lib::AppState;

const TOKEN: &str = "phone-token";
/// Дедлайн клиентского ожидания: по контракту зонда зависание = тише 20 с.
const REQUEST_SECS: u64 = 15;
/// Дедлайн чтения SSE до события конца хода.
const SSE_SECS: u64 = 15;

// ---------- общий каркас (как e2e_smoke, без своих идей) ----------

fn enable_echo() {
    std::env::set_var(swagcod_provider::ECHO_ENV, "1");
}

fn state() -> Arc<AppState> {
    Arc::new(AppState {
        store: std::sync::Mutex::new(swagcod_core::store::open_memory()),
        ..Default::default()
    })
}

async fn serve() -> (u16, Arc<AppState>) {
    enable_echo();
    let st = state();
    let listener = TcpListener::bind("127.0.0.1:0")
        .await
        .expect("эфемерный порт");
    let port = listener.local_addr().expect("адрес").port();
    let spawned = st.clone();
    tokio::spawn(async move {
        serve_on(listener, spawned, TOKEN.to_string()).await;
    });
    for _ in 0..200 {
        let mut sock = TcpStream::connect(("127.0.0.1", port))
            .await
            .expect("connect");
        sock.write_all(b"GET /health HTTP/1.1\r\nhost: phone\r\n\r\n")
            .await
            .unwrap();
        let mut out = Vec::new();
        let _ = tokio::time::timeout(Duration::from_secs(2), sock.read_to_end(&mut out)).await;
        if String::from_utf8_lossy(&out).contains("\"ok\":true") {
            return (port, st);
        }
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
    panic!("loopback API не ответил на /health");
}

/// Ответ обычного запроса: статус, разобранное JSON-тело, сырой текст,
/// сбой канала (timeout/reset) — если был.
struct Resp {
    status: Option<u16>,
    body: serde_json::Value,
    raw: String,
    err: Option<String>,
}

async fn request(port: u16, raw: &str) -> Resp {
    let mut sock = match TcpStream::connect(("127.0.0.1", port)).await {
        Ok(s) => s,
        Err(e) => {
            return Resp {
                status: None,
                body: serde_json::Value::Null,
                raw: String::new(),
                err: Some(format!("connect: {e}")),
            }
        }
    };
    if let Err(e) = sock.write_all(raw.as_bytes()).await {
        return Resp {
            status: None,
            body: serde_json::Value::Null,
            raw: String::new(),
            err: Some(format!("write: {e}")),
        };
    }
    let mut out = Vec::new();
    let read = tokio::time::timeout(
        Duration::from_secs(REQUEST_SECS),
        sock.read_to_end(&mut out),
    )
    .await;
    let err = match read {
        Err(_) => Some(format!("timeout: EOF не наступил за {REQUEST_SECS} с")),
        Ok(Err(e)) => Some(format!("read: {e}")),
        Ok(Ok(_)) => None,
    };
    let raw = String::from_utf8_lossy(&out).to_string();
    let status = raw
        .lines()
        .next()
        .and_then(|l| l.split(' ').nth(1))
        .and_then(|s| s.parse::<u16>().ok());
    let body = raw
        .find("\r\n\r\n")
        .map(|at| serde_json::from_str(raw[at + 4..].trim()).unwrap_or(serde_json::Value::Null))
        .unwrap_or(serde_json::Value::Null);
    Resp {
        status,
        body,
        raw,
        err,
    }
}

/// POST с честным Content-Length и Bearer-токеном — как настоящий клиент.
async fn post(port: u16, path: &str, body: &str) -> Resp {
    let raw = format!(
        "POST {path} HTTP/1.1\r\nhost: phone\r\nauthorization: Bearer {TOKEN}\r\ncontent-type: application/json\r\ncontent-length: {}\r\nconnection: close\r\n\r\n{body}",
        body.len()
    );
    request(port, &raw).await
}

/// GET с токеном.
async fn get(port: u16, path: &str) -> Resp {
    let raw = format!(
        "GET {path} HTTP/1.1\r\nhost: phone\r\nauthorization: Bearer {TOKEN}\r\nconnection: close\r\n\r\n"
    );
    request(port, &raw).await
}

fn assert_200(r: &Resp, ctx: &str) -> serde_json::Value {
    assert!(r.err.is_none(), "{ctx}: сбой канала {:?}\n{}", r.err, r.raw);
    assert_eq!(
        r.status,
        Some(200),
        "{ctx}: статус {:?}\n{}",
        r.status,
        r.raw
    );
    r.body.clone()
}

fn assert_400(r: &Resp, ctx: &str) {
    assert!(r.err.is_none(), "{ctx}: сбой канала {:?}\n{}", r.err, r.raw);
    assert_eq!(
        r.status,
        Some(400),
        "{ctx}: статус {:?}\n{}",
        r.status,
        r.raw
    );
    assert!(
        r.body.get("error").is_some(),
        "{ctx}: 400 без error\n{}",
        r.raw
    );
}

/// Рабочая директория сессии — реальная, с уникальным именем под параллельные прогоны.
fn workdir(tag: &str) -> std::path::PathBuf {
    let ms = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_millis())
        .unwrap_or(0);
    let dir = std::env::temp_dir().join(format!("swagcod-phone-{tag}-{}-{ms}", std::process::id()));
    std::fs::create_dir_all(&dir).expect("временная директория создаётся");
    dir
}

/// Создать сессию и вернуть (session_id, cwd).
async fn create_session(port: u16, cwd: &std::path::Path, model: &str) -> String {
    let body = serde_json::json!({ "cwd": cwd.to_string_lossy(), "model": model }).to_string();
    let r = post(port, "/v1/sessions", &body).await;
    let brief = assert_200(&r, "создание сессии");
    let sid = brief
        .get("id")
        .and_then(|v| v.as_str())
        .unwrap_or("")
        .to_string();
    assert!(!sid.is_empty(), "нет id в ответе: {}", r.raw);
    assert_eq!(
        brief.get("cwd").and_then(|v| v.as_str()),
        Some(cwd.to_string_lossy().as_ref()),
        "cwd в брифе: {}",
        r.raw
    );
    assert_eq!(
        brief.get("model").and_then(|v| v.as_str()),
        Some(model),
        "model в брифе: {}",
        r.raw
    );
    sid
}

/// Запустить ход и вернуть turn_id.
async fn start_turn(port: u16, session_id: &str, message: &str) -> String {
    let body = serde_json::json!({ "session_id": session_id, "message": message }).to_string();
    let r = post(port, "/v1/turns", &body).await;
    let v = assert_200(&r, "запуск хода");
    let tid = v
        .get("turn_id")
        .and_then(|x| x.as_str())
        .unwrap_or("")
        .to_string();
    assert!(!tid.is_empty(), "нет turn_id: {}", r.raw);
    tid
}

/// Лента транскрипта как массив событий.
async fn transcript(port: u16, session_id: &str) -> Vec<serde_json::Value> {
    let body = serde_json::json!({ "session_id": session_id }).to_string();
    let r = post(port, "/v1/sessions/transcript", &body).await;
    let v = assert_200(&r, "транскрипт");
    v.as_array().cloned().unwrap_or_default()
}

// ---------- SSE-клиент с таймаутами ----------

/// Живой SSE-ридер: сокет в руках теста, строки разбираются по мере
/// прибытия, у каждого ожидания — дедлайн. Дроп структуры = разрыв
/// соединения клиентом (как телефон в метро).
struct Sse {
    sock: TcpStream,
    buf: Vec<u8>,
    ready: std::collections::VecDeque<serde_json::Value>,
    /// Почему чтение остановилось: "eof", "err: …" или None (ещё читаем).
    stopped: Option<String>,
}

impl Sse {
    /// Открыть поток и дождаться рукопожатия 200 + text/event-stream.
    async fn open(port: u16) -> Sse {
        let mut sock = TcpStream::connect(("127.0.0.1", port))
            .await
            .expect("соединение SSE");
        sock.write_all(
            format!(
                "GET /v1/events HTTP/1.1\r\nhost: phone\r\nauthorization: Bearer {TOKEN}\r\naccept: text/event-stream\r\n\r\n"
            )
            .as_bytes(),
        )
        .await
        .expect("запрос SSE отправлен");
        let mut sse = Sse {
            sock,
            buf: Vec::new(),
            ready: std::collections::VecDeque::new(),
            stopped: None,
        };
        // Рукопожатие: читаем до \r\n\r\n.
        let deadline = tokio::time::Instant::now() + Duration::from_secs(5);
        loop {
            if sse.buf.windows(4).any(|w| w == b"\r\n\r\n") {
                break;
            }
            let left = deadline.saturating_duration_since(tokio::time::Instant::now());
            if left.is_zero() {
                panic!("SSE: рукопожатие не пришло за 5 с");
            }
            if !sse.read_more(left).await {
                panic!(
                    "SSE: соединение закрылось до рукопожатия ({})",
                    sse.stopped.clone().unwrap_or_default()
                );
            }
        }
        let head_end = sse
            .buf
            .windows(4)
            .position(|w| w == b"\r\n\r\n")
            .expect("терминатор заголовков");
        let head = String::from_utf8_lossy(&sse.buf[..head_end]).to_string();
        sse.buf.drain(..head_end + 4);
        assert!(
            head.starts_with("HTTP/1.1 200"),
            "SSE-рукопожатие не 200: {head}"
        );
        assert!(
            head.contains("text/event-stream"),
            "SSE-рукопожатие без event-stream: {head}"
        );
        sse.drain_events();
        sse
    }

    /// Дочитать кусок из сокета: false — EOF, ошибка или таймаут.
    async fn read_more(&mut self, left: Duration) -> bool {
        let mut chunk = [0u8; 16384];
        match tokio::time::timeout(left, self.sock.read(&mut chunk)).await {
            Ok(Ok(0)) => {
                self.stopped = Some("eof".into());
                false
            }
            Ok(Ok(n)) => {
                self.buf.extend_from_slice(&chunk[..n]);
                true
            }
            Ok(Err(e)) => {
                self.stopped = Some(format!("err: {e}"));
                false
            }
            Err(_) => false, // дедлайн этого куска — не останавливает поток
        }
    }

    /// Разобрать готовые строки буфера в события.
    fn drain_events(&mut self) {
        while let Some(pos) = self.buf.iter().position(|&b| b == b'\n') {
            let line: Vec<u8> = self.buf.drain(..=pos).collect();
            let line = String::from_utf8_lossy(&line[..line.len() - 1]);
            let line = line.trim_end_matches('\r');
            if let Some(payload) = line.strip_prefix("data: ") {
                if let Ok(ev) = serde_json::from_str::<serde_json::Value>(payload) {
                    self.ready.push_back(ev);
                }
            }
        }
    }

    /// Следующее событие или None по дедлайну/обрыву.
    async fn next_event(&mut self, secs: u64) -> Option<serde_json::Value> {
        let deadline = tokio::time::Instant::now() + Duration::from_secs(secs);
        loop {
            if let Some(ev) = self.ready.pop_front() {
                return Some(ev);
            }
            if self.stopped.is_some() {
                return None;
            }
            let left = deadline.saturating_duration_since(tokio::time::Instant::now());
            if left.is_zero() {
                return None;
            }
            if !self.read_more(left).await {
                return None;
            }
            self.drain_events();
        }
    }
}

fn kind_of(ev: &serde_json::Value) -> &str {
    ev.pointer("/kind/kind")
        .and_then(|v| v.as_str())
        .unwrap_or("")
}

fn text_of(ev: &serde_json::Value) -> &str {
    ev.pointer("/kind/data/text")
        .and_then(|v| v.as_str())
        .unwrap_or("")
}

fn session_of(ev: &serde_json::Value) -> &str {
    ev.pointer("/kind/data/session")
        .and_then(|v| v.as_str())
        .unwrap_or("")
}

fn turn_of(ev: &serde_json::Value) -> &str {
    ev.pointer("/kind/data/turn")
        .and_then(|v| v.as_str())
        .unwrap_or("")
}

/// Собрать события до turn_ended нашей сессии (или паника по дедлайну).
async fn collect_turn(sse: &mut Sse, session_id: &str, ctx: &str) -> Vec<serde_json::Value> {
    let mut out = Vec::new();
    let deadline = tokio::time::Instant::now() + Duration::from_secs(SSE_SECS);
    loop {
        let left = deadline.saturating_duration_since(tokio::time::Instant::now());
        if left.is_zero() {
            panic!(
                "{ctx}: turn_ended не пришёл за {SSE_SECS} с, событий {}:\n{:?}",
                out.len(),
                out.iter().map(kind_of).collect::<Vec<_>>()
            );
        }
        let secs = left.as_secs().max(1);
        match sse.next_event(secs).await {
            Some(ev) => {
                let ends = kind_of(&ev) == "turn_ended" && session_of(&ev) == session_id;
                out.push(ev);
                if ends {
                    return out;
                }
            }
            None => panic!(
                "{ctx}: SSE замолчал ({}), событий {}:\n{:?}",
                sse.stopped.clone().unwrap_or_else(|| "таймаут".into()),
                out.len(),
                out.iter().map(kind_of).collect::<Vec<_>>()
            ),
        }
    }
}

/// То же, но ждём конец КОНКРЕТНОГО хода: ранние turn_ended чужих ходов
/// той же сессии пропускаются.
async fn collect_turn_of(
    sse: &mut Sse,
    session_id: &str,
    turn_id: &str,
    ctx: &str,
) -> Vec<serde_json::Value> {
    let mut out = Vec::new();
    let deadline = tokio::time::Instant::now() + Duration::from_secs(SSE_SECS);
    loop {
        let left = deadline.saturating_duration_since(tokio::time::Instant::now());
        if left.is_zero() {
            panic!(
                "{ctx}: turn_ended хода {turn_id} не пришёл за {SSE_SECS} с, событий {}:\n{:?}",
                out.len(),
                out.iter().map(kind_of).collect::<Vec<_>>()
            );
        }
        let secs = left.as_secs().max(1);
        match sse.next_event(secs).await {
            Some(ev) => {
                let ends = kind_of(&ev) == "turn_ended"
                    && session_of(&ev) == session_id
                    && turn_of(&ev) == turn_id;
                out.push(ev);
                if ends {
                    return out;
                }
            }
            None => panic!(
                "{ctx}: SSE замолчал ({}), событий {}:\n{:?}",
                sse.stopped.clone().unwrap_or_else(|| "таймаут".into()),
                out.len(),
                out.iter().map(kind_of).collect::<Vec<_>>()
            ),
        }
    }
}

/// «Длинный ход» для телефона: эхо-стрим мчится миллисекунды, теневая копия
/// маленькой папки — тоже. Честная человеческая длительность ходу даётся
/// настоящей механикой приложения: pre_turn-хук владельца, спящий ~3 с
/// (ping с N эхо-запросами по секунде). Хук живёт в prefs store — как
/// настроил бы владелец, никакой подмены сервера.
fn slow_pre_turn_hook(st: &Arc<AppState>, pings: u32) {
    let raw = format!(r#"{{"pre_turn":["ping -n {pings} 127.0.0.1 > nul"]}}"#);
    st.store
        .lock()
        .expect("store")
        .set_pref("hooks", &raw)
        .expect("хук записан");
}

/// Снять замедляющий хук — дальнейшие ходы бегут без паузы.
fn clear_hooks(st: &Arc<AppState>) {
    st.store
        .lock()
        .expect("store")
        .set_pref("hooks", "{}")
        .expect("хуки сняты");
}

// ---------- сценарии ----------

/// Главный флоу телефона: открыть сессию (реальный tempdir) → лента пуста →
/// ход «привет, что умеешь?» → SSE до конца хода → транскрипт показывает
/// реплику юзера и ответ модели → список сессий отражает созданную с cwd.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn phone_open_session_turn_sse_and_transcript() {
    let (port, _st) = serve().await;
    let mut sse = Sse::open(port).await;

    let cwd = workdir("flow");
    let sid = create_session(port, &cwd, "echo").await;

    // Лента новой сессии пуста — но это 200 и массив.
    let items = transcript(port, &sid).await;
    assert!(items.is_empty(), "новая сессия не пуста: {items:?}");

    // Каталог моделей: телефон выбирает модель перед первым ходом.
    let r = get(port, "/v1/models").await;
    let models = assert_200(&r, "каталог моделей");
    let first_id = models
        .pointer("/data/0/id")
        .and_then(|v| v.as_str())
        .unwrap_or("");
    assert_eq!(first_id, "echo", "каталог моделей: {}", r.raw);

    // Ход.
    let message = "привет, что умеешь?";
    let tid = start_turn(port, &sid, message).await;

    // SSE до конца хода.
    let events = collect_turn(&mut sse, &sid, "ход").await;
    let kinds: Vec<&str> = events.iter().map(kind_of).collect();
    assert!(
        kinds.contains(&"turn_started"),
        "нет turn_started: {kinds:?}"
    );
    assert!(kinds.contains(&"reasoning"), "нет reasoning: {kinds:?}");
    assert!(kinds.contains(&"content"), "нет content: {kinds:?}");
    assert!(kinds.contains(&"turn_ended"), "нет turn_ended: {kinds:?}");
    let at = |k: &str| kinds.iter().position(|x| *x == k).unwrap_or(usize::MAX);
    assert!(
        at("turn_started") < at("reasoning")
            && at("reasoning") < at("content")
            && at("content") < at("turn_ended"),
        "порядок событий хода нарушен: {kinds:?}"
    );
    assert_eq!(
        events
            .iter()
            .filter(|e| kind_of(e) == "turn_started")
            .map(turn_of)
            .collect::<Vec<_>>(),
        vec![tid.as_str()],
        "turn_started не нашего хода: {kinds:?}"
    );
    let ended = events.iter().find(|e| kind_of(e) == "turn_ended").unwrap();
    assert_eq!(
        ended.pointer("/kind/data/ok").and_then(|v| v.as_bool()),
        Some(true),
        "ход не успешен: {ended}"
    );

    // Ответ модели по SSE складывается в эхо.
    let content: String = events
        .iter()
        .filter(|e| kind_of(e) == "content")
        .map(text_of)
        .collect();
    assert_eq!(
        content,
        format!("эхо: {message}"),
        "ответ эха по SSE: {content}"
    );

    // Транскрипт: реплика юзера и ответ модели на месте, порядок стабилен.
    let items = transcript(port, &sid).await;
    let tkinds: Vec<&str> = items.iter().map(kind_of).collect();
    assert!(
        tkinds.contains(&"user"),
        "в ленте нет реплики юзера: {tkinds:?}"
    );
    assert!(
        tkinds.contains(&"reasoning"),
        "в ленте нет reasoning: {tkinds:?}"
    );
    assert!(
        tkinds.contains(&"content"),
        "в ленте нет ответа: {tkinds:?}"
    );
    assert!(
        tkinds.contains(&"turn_ended"),
        "в ленте нет конца хода: {tkinds:?}"
    );
    let tat = |k: &str| tkinds.iter().position(|x| *x == k).unwrap_or(usize::MAX);
    assert!(
        tat("turn_started") < tat("user")
            && tat("user") < tat("reasoning")
            && tat("reasoning") < tat("content")
            && tat("content") < tat("turn_ended"),
        "порядок ленты нарушен: {tkinds:?}"
    );
    assert_eq!(
        tkinds.iter().filter(|k| **k == "turn_ended").count(),
        1,
        "дубль turn_ended: {tkinds:?}"
    );
    assert_eq!(
        tkinds.iter().filter(|k| **k == "user").count(),
        1,
        "дубль user: {tkinds:?}"
    );
    let user_text = items
        .iter()
        .find(|e| kind_of(e) == "user")
        .map(text_of)
        .unwrap_or("");
    assert_eq!(user_text, message, "реплика юзера в ленте: {user_text}");
    let logged = items
        .iter()
        .find(|e| kind_of(e) == "content")
        .map(text_of)
        .unwrap_or("");
    assert_eq!(logged, format!("эхо: {message}"), "ответ в ленте: {logged}");

    // Повторное чтение ленты — тот же результат.
    let again = transcript(port, &sid).await;
    assert_eq!(
        serde_json::to_string(&items).unwrap(),
        serde_json::to_string(&again).unwrap(),
        "повторное чтение ленты отличается"
    );

    // Точки отката хода: вне git это теневой снимок, телефон видит его в
    // списке checkpoint'ов сессии.
    let body = serde_json::json!({ "session_id": sid }).to_string();
    let r = post(port, "/v1/checkpoints", &body).await;
    let cps = assert_200(&r, "точки отката");
    let rows = cps.as_array().cloned().unwrap_or_default();
    assert_eq!(rows.len(), 1, "точки отката: {}", r.raw);
    assert_eq!(
        rows[0].get("turn_id").and_then(|v| v.as_str()),
        Some(tid.as_str()),
        "точка отката не нашего хода: {}",
        r.raw
    );
    let sha = rows[0].get("sha").and_then(|v| v.as_str()).unwrap_or("");
    assert!(
        sha.starts_with("shadow:"),
        "вне git снимок обязан быть теневым: {sha}"
    );

    // Список сессий отражает созданную с правильным cwd.
    let r = get(port, "/v1/sessions").await;
    let list = assert_200(&r, "список сессий");
    let arr = list.as_array().cloned().unwrap_or_default();
    let mine = arr
        .iter()
        .find(|s| s.get("id").and_then(|v| v.as_str()) == Some(sid.as_str()));
    let mine = mine.cloned().unwrap_or(serde_json::Value::Null);
    assert!(!mine.is_null(), "сессии нет в списке: {}", r.raw);
    assert_eq!(
        mine.get("cwd").and_then(|v| v.as_str()),
        Some(cwd.to_string_lossy().as_ref()),
        "cwd в списке: {mine}"
    );
    assert_eq!(
        mine.get("turns").and_then(|v| v.as_u64()),
        Some(1),
        "счётчик ходов в списке: {mine}"
    );
}

/// Два хода подряд в одну сессию: второй после завершения первого работает,
/// SSE живёт через границы ходов, лента хранит оба в стабильном порядке.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn phone_two_turns_in_a_row_same_session() {
    let (port, _st) = serve().await;
    let mut sse = Sse::open(port).await;

    let cwd = workdir("two-turns");
    let sid = create_session(port, &cwd, "echo").await;

    // Первый ход.
    let t1 = start_turn(port, &sid, "первый ход").await;
    let e1 = collect_turn(&mut sse, &sid, "ход-1").await;
    assert_eq!(
        e1.iter()
            .find(|e| kind_of(e) == "turn_ended")
            .and_then(|e| e.pointer("/kind/data/ok"))
            .and_then(|v| v.as_bool()),
        Some(true),
        "ход-1 не успешен"
    );

    // Второй ход по ТОМУ ЖЕ SSE-соединению — поток не умирает с концом хода.
    let t2 = start_turn(port, &sid, "второй ход").await;
    let e2 = collect_turn(&mut sse, &sid, "ход-2").await;
    assert_eq!(
        e2.iter()
            .find(|e| kind_of(e) == "turn_ended")
            .and_then(|e| e.pointer("/kind/data/ok"))
            .and_then(|v| v.as_bool()),
        Some(true),
        "ход-2 не успешен"
    );
    assert_ne!(t1, t2, "ходы получили одинаковый turn_id");
    let c2: String = e2
        .iter()
        .filter(|e| kind_of(e) == "content")
        .map(text_of)
        .collect();
    assert_eq!(c2, "эхо: второй ход", "ответ хода-2: {c2}");

    // Лента: оба хода целиком, в порядке следования, без перемешивания.
    let items = transcript(port, &sid).await;
    let by_turn = |tid: &str| -> Vec<&str> {
        items
            .iter()
            .filter(|e| turn_of(e) == tid)
            .map(kind_of)
            .collect()
    };
    assert_eq!(
        by_turn(&t1),
        vec!["turn_started", "user", "reasoning", "content", "turn_ended"],
        "лента хода-1: {:?}",
        by_turn(&t1)
    );
    assert_eq!(
        by_turn(&t2),
        vec!["turn_started", "user", "reasoning", "content", "turn_ended"],
        "лента хода-2: {:?}",
        by_turn(&t2)
    );
    let pos = |tid: &str, k: &str| {
        items
            .iter()
            .position(|e| turn_of(e) == tid && kind_of(e) == k)
            .unwrap()
    };
    assert!(
        pos(&t1, "turn_ended") < pos(&t2, "turn_started"),
        "ходы перемешаны в ленте"
    );
    let users: Vec<&str> = items
        .iter()
        .filter(|e| kind_of(e) == "user")
        .map(text_of)
        .collect();
    assert_eq!(
        users,
        vec!["первый ход", "второй ход"],
        "реплики юзера в ленте: {users:?}"
    );
    let contents: Vec<&str> = items
        .iter()
        .filter(|e| kind_of(e) == "content")
        .map(text_of)
        .collect();
    assert_eq!(
        contents,
        vec!["эхо: первый ход", "эхо: второй ход"],
        "ответы в ленте: {contents:?}"
    );
}

/// Две сессии через один сервер: ходы идут параллельно, ленты не
/// пересекаются, параллельные GET/POST не роняют сервер. Второе
/// SSE-соединение (планшет рядом с телефоном) видит те же события.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn phone_two_sessions_parallel_turns_isolated() {
    let (port, _st) = serve().await;
    let mut sse = Sse::open(port).await;
    let mut tablet = Sse::open(port).await;

    let cwd_a = workdir("sess-a");
    let cwd_b = workdir("sess-b");
    let sid_a = create_session(port, &cwd_a, "echo").await;
    let sid_b = create_session(port, &cwd_b, "echo").await;

    // Параллельные ходы в A и B — как два чата на одном телефоне.
    let (ra, rb) = tokio::join!(
        start_turn(port, &sid_a, "сообщение для A"),
        start_turn(port, &sid_b, "сообщение для B")
    );
    let (ta, tb) = (ra, rb);

    // Параллельный GET /v1/sessions и /health во время ходов — живой темп.
    let (l1, l2) = tokio::join!(get(port, "/v1/sessions"), get(port, "/health"));
    for (ctx, r) in [
        ("сессии в момент ходов", &l1),
        ("health в момент ходов", &l2),
    ] {
        assert!(r.err.is_none(), "{ctx}: сбой канала {:?}", r.err);
        assert_eq!(r.status, Some(200), "{ctx}: {}", r.raw);
    }

    // Оба хода завершаются.
    let mut ended = std::collections::HashSet::new();
    let deadline = tokio::time::Instant::now() + Duration::from_secs(SSE_SECS);
    let mut all = Vec::new();
    while ended.len() < 2 {
        let left = deadline.saturating_duration_since(tokio::time::Instant::now());
        if left.is_zero() {
            panic!("не оба хода завершились: {:?}", ended);
        }
        match sse.next_event(left.as_secs().max(1)).await {
            Some(ev) => {
                if kind_of(&ev) == "turn_ended"
                    && (session_of(&ev) == sid_a || session_of(&ev) == sid_b)
                {
                    ended.insert(session_of(&ev).to_string());
                }
                all.push(ev);
            }
            None => panic!(
                "SSE замолчал до конца ходов ({}), завершены {:?}",
                sse.stopped.clone().unwrap_or_else(|| "таймаут".into()),
                ended
            ),
        }
    }
    for sid in [&sid_a, &sid_b] {
        let ok = all
            .iter()
            .find(|e| kind_of(e) == "turn_ended" && session_of(e) == *sid)
            .and_then(|e| e.pointer("/kind/data/ok"))
            .and_then(|v| v.as_bool());
        assert_eq!(ok, Some(true), "ход сессии {sid} не успешен");
    }

    // Планшет видел те же события: вторая подписка не мешает первой и
    // не теряет ходы.
    let mut tablet_ended = std::collections::HashSet::new();
    let deadline = tokio::time::Instant::now() + Duration::from_secs(SSE_SECS);
    while tablet_ended.len() < 2 {
        let left = deadline.saturating_duration_since(tokio::time::Instant::now());
        if left.is_zero() {
            panic!("планшет не увидел оба конца ходов: {:?}", tablet_ended);
        }
        match tablet.next_event(left.as_secs().max(1)).await {
            Some(ev) => {
                if kind_of(&ev) == "turn_ended"
                    && (session_of(&ev) == sid_a || session_of(&ev) == sid_b)
                {
                    assert_eq!(
                        ev.pointer("/kind/data/ok").and_then(|v| v.as_bool()),
                        Some(true),
                        "планшет: ход завершился неуспешно: {ev}"
                    );
                    tablet_ended.insert(session_of(&ev).to_string());
                }
            }
            None => panic!(
                "SSE планшета замолчал ({}), завершены {:?}",
                tablet.stopped.clone().unwrap_or_else(|| "таймаут".into()),
                tablet_ended
            ),
        }
    }
    drop(tablet);

    // Ленты не пересекаются.
    let items_a = transcript(port, &sid_a).await;
    let items_b = transcript(port, &sid_b).await;
    let texts_a: Vec<&str> = items_a
        .iter()
        .filter(|e| kind_of(e) == "user")
        .map(text_of)
        .collect();
    let texts_b: Vec<&str> = items_b
        .iter()
        .filter(|e| kind_of(e) == "user")
        .map(text_of)
        .collect();
    assert_eq!(texts_a, vec!["сообщение для A"], "лента A: {texts_a:?}");
    assert_eq!(texts_b, vec!["сообщение для B"], "лента B: {texts_b:?}");
    let ca: String = items_a
        .iter()
        .filter(|e| kind_of(e) == "content")
        .map(text_of)
        .collect();
    let cb: String = items_b
        .iter()
        .filter(|e| kind_of(e) == "content")
        .map(text_of)
        .collect();
    assert_eq!(ca, "эхо: сообщение для A", "ответ A: {ca}");
    assert_eq!(cb, "эхо: сообщение для B", "ответ B: {cb}");
    let turns_a: Vec<&str> = items_a.iter().map(turn_of).collect();
    assert!(!turns_a.is_empty(), "лента A пуста");
    assert!(
        turns_a.iter().all(|t| *t == ta),
        "в ленте A чужой ход: {turns_a:?}"
    );
    let turns_b: Vec<&str> = items_b.iter().map(turn_of).collect();
    assert!(!turns_b.is_empty(), "лента B пуста");
    assert!(
        turns_b.iter().all(|t| *t == tb),
        "в ленте B чужой ход: {turns_b:?}"
    );

    // Сервер жив, обе сессии в списке.
    let r = get(port, "/v1/sessions").await;
    let list = assert_200(&r, "список после ходов");
    for sid in [&sid_a, &sid_b] {
        assert!(
            list.to_string().contains(sid.as_str()),
            "сессии {sid} нет в списке: {}",
            r.raw
        );
    }
}

/// Остановка хода посреди живого хода: кнопка «стоп» — ход завершается
/// честно (turn_ended ok=false с причиной), сервер жив, следующий ход работает.
///
/// «Длинный ход» — pre_turn-хук, спящий ~3 с (эхо-стрим сам по себе короче
/// латентности HTTP, гоняться с ним бессмысленно). Гейт «ход жив» — честный
/// двойной тап: второй POST /v1/turns обязан получить 400 «сессия занята»,
/// пока ход не закончился.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn phone_stop_button_ends_running_turn_honestly() {
    let (port, st) = serve().await;
    let mut sse = Sse::open(port).await;

    let cwd = workdir("stop");
    let sid = create_session(port, &cwd, "echo").await;
    slow_pre_turn_hook(&st, 4);

    // Длинный ход.
    let tid = start_turn(port, &sid, "останови меня").await;
    let started = std::time::Instant::now();
    let mut victim = tid.clone();

    // Двойной тап как гейт: 400 «сессия занята» = ход точно жив.
    // 200 значило бы, что ход уже закончился и тап запустил новый —
    // тогда жертвой становится новый ход (и гейт повторяется).
    let mut busy = false;
    for _ in 0..10 {
        let body = serde_json::json!({ "session_id": sid, "message": "двойной тап" }).to_string();
        let r = post(port, "/v1/turns", &body).await;
        assert!(
            r.err.is_none(),
            "двойной тап: сбой канала {:?}\n{}",
            r.err,
            r.raw
        );
        match r.status {
            Some(400) => {
                busy = true;
                break;
            }
            Some(200) => {
                println!("stop: ход закончился до двойного тапа, тап запустил новый");
                victim = r
                    .body
                    .get("turn_id")
                    .and_then(|v| v.as_str())
                    .unwrap_or("")
                    .to_string();
                assert!(!victim.is_empty(), "тап без turn_id: {}", r.raw);
                tokio::time::sleep(Duration::from_millis(25)).await;
            }
            other => panic!("двойной тап: статус {other:?}\n{}", r.raw),
        }
    }
    assert!(busy, "ход ни разу не был занят — гейт не сработал");
    let to_busy = started.elapsed();

    // Стоп по живому ходу.
    let body = serde_json::json!({ "session_id": sid }).to_string();
    let stop_started = std::time::Instant::now();
    let r = post(port, "/v1/turns/stop", &body).await;
    let stop_latency = stop_started.elapsed();
    assert!(r.err.is_none(), "стоп: сбой канала {:?}\n{}", r.err, r.raw);
    assert_eq!(r.status, Some(200), "стоп не прошёл: {}", r.raw);
    println!(
        "stop: гейт занятости за {:?}, стоп ответил за {:?}",
        to_busy, stop_latency
    );

    // Ход завершён честно: turn_ended ok=false с причиной.
    let events = collect_turn_of(&mut sse, &sid, &victim, "ход после стопа").await;
    let ended = events
        .iter()
        .find(|e| kind_of(e) == "turn_ended" && turn_of(e) == victim)
        .cloned()
        .unwrap_or(serde_json::Value::Null);
    assert!(
        !ended.is_null(),
        "нет turn_ended нашего хода: {:?}",
        events.iter().map(kind_of).collect::<Vec<_>>()
    );
    assert_eq!(
        ended.pointer("/kind/data/ok").and_then(|v| v.as_bool()),
        Some(false),
        "остановленный ход должен завершиться ok=false: {ended}"
    );
    let reason = ended
        .pointer("/kind/data/reason")
        .and_then(|v| v.as_str())
        .unwrap_or("");
    assert!(
        reason.contains("остановлено"),
        "причина остановки: {reason}"
    );

    // Лента хранит остановленный ход с честным финалом.
    let items = transcript(port, &sid).await;
    let te = items
        .iter()
        .find(|e| kind_of(e) == "turn_ended" && turn_of(e) == victim)
        .cloned()
        .unwrap_or(serde_json::Value::Null);
    assert!(
        !te.is_null(),
        "остановленного хода нет в ленте: {:?}",
        items.iter().map(kind_of).collect::<Vec<_>>()
    );
    assert_eq!(
        te.pointer("/kind/data/ok").and_then(|v| v.as_bool()),
        Some(false),
        "лента: остановленный ход ок: {te}"
    );
    // Реплика юзера пережила стоп: сообщение не должно исчезать из ленты.
    let victim_user: Vec<&str> = items
        .iter()
        .filter(|e| kind_of(e) == "user" && turn_of(e) == victim)
        .map(text_of)
        .collect();
    let expected_msg = if victim == tid {
        "останови меня"
    } else {
        "двойной тап"
    };
    assert_eq!(
        victim_user,
        vec![expected_msg],
        "реплика юзера остановленного хода: {victim_user:?}"
    );

    // Сервер жив, сессия не застряла: следующий ход проходит целиком.
    // Замедляющий хук снят — ход бежит без искусственной паузы.
    clear_hooks(&st);
    let r = get(port, "/health").await;
    assert_eq!(r.status, Some(200), "health после стопа: {}", r.raw);
    let t2 = start_turn(port, &sid, "ход после стопа").await;
    let e2 = collect_turn_of(&mut sse, &sid, &t2, "ход после стопа").await;
    let ok2 = e2
        .iter()
        .find(|e| kind_of(e) == "turn_ended" && turn_of(e) == t2)
        .and_then(|e| e.pointer("/kind/data/ok"))
        .and_then(|v| v.as_bool());
    assert_eq!(ok2, Some(true), "ход после стопа не успешен");
}

/// SSE-разрыв посреди хода: клиент закрыл соединение на середине живого
/// хода — сервер пережил, ход дошёл до конца, переподключение ловит
/// новые события.
///
/// Ход удлинён pre_turn-хуком (~3 с): обрыв делаем после turn_started —
/// ход гарантированно ещё жив.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn phone_sse_disconnect_mid_turn_server_survives() {
    let (port, st) = serve().await;
    let mut sse = Sse::open(port).await;

    let cwd = workdir("sse-drop");
    let sid = create_session(port, &cwd, "echo").await;
    slow_pre_turn_hook(&st, 4);

    let tid = start_turn(port, &sid, "проверка обрыва").await;

    // Ждём turn_started по SSE: ход жив, хук ещё спит.
    let deadline = tokio::time::Instant::now() + Duration::from_secs(SSE_SECS);
    let mut saw_start = false;
    while !saw_start {
        let left = deadline.saturating_duration_since(tokio::time::Instant::now());
        if left.is_zero() {
            panic!("turn_started не пришёл за {SSE_SECS} с");
        }
        match sse.next_event(left.as_secs().max(1)).await {
            Some(ev) => {
                if kind_of(&ev) == "turn_started" && turn_of(&ev) == tid {
                    saw_start = true;
                }
            }
            None => panic!(
                "SSE замолчал до turn_started ({})",
                sse.stopped.clone().unwrap_or_else(|| "таймаут".into())
            ),
        }
    }

    // Обрыв: телефон ушёл в метро посреди живого хода.
    drop(sse);
    tokio::time::sleep(Duration::from_millis(50)).await;

    // Сервер пережил: /health отвечает, ход не убит — дошёл до конца.
    let r = get(port, "/health").await;
    assert!(r.err.is_none(), "health после обрыва SSE: {:?}", r.err);
    assert_eq!(r.status, Some(200), "health после обрыва SSE: {}", r.raw);

    let full = "эхо: проверка обрыва".to_string();
    let deadline = tokio::time::Instant::now() + Duration::from_secs(SSE_SECS);
    let turn_ended = loop {
        let items = transcript(port, &sid).await;
        if let Some(e) = items
            .iter()
            .find(|e| kind_of(e) == "turn_ended" && turn_of(e) == tid)
        {
            break e.clone();
        }
        if deadline <= tokio::time::Instant::now() {
            panic!("ход не завершился после обрыва SSE за {SSE_SECS} с");
        }
        tokio::time::sleep(Duration::from_millis(100)).await;
    };
    assert_eq!(
        turn_ended
            .pointer("/kind/data/ok")
            .and_then(|v| v.as_bool()),
        Some(true),
        "ход убит обрывом SSE: {turn_ended}"
    );
    let items = transcript(port, &sid).await;
    let logged = items
        .iter()
        .find(|e| kind_of(e) == "content" && turn_of(e) == tid)
        .map(text_of)
        .unwrap_or("");
    assert_eq!(logged, full, "ход не донёс ответ после обрыва SSE");
    // Ленту читали ПОХОДЯ (поллингом во время хода) — событие ответа
    // обязано остаться ровно одним, без дублей от повторных чтений.
    assert_eq!(
        items
            .iter()
            .filter(|e| kind_of(e) == "content" && turn_of(e) == tid)
            .count(),
        1,
        "дубль content после поллинга ленты во время хода: {:?}",
        items.iter().map(kind_of).collect::<Vec<_>>()
    );

    // Переподключение: новый SSE ловит события нового хода.
    // Замедляющий хук снят — ход бежит без искусственной паузы.
    clear_hooks(&st);
    let mut sse2 = Sse::open(port).await;
    let t2 = start_turn(port, &sid, "после переподключения").await;
    let events = collect_turn_of(&mut sse2, &sid, &t2, "ход после переподключения").await;
    let ok2 = events
        .iter()
        .find(|e| kind_of(e) == "turn_ended" && turn_of(e) == t2)
        .and_then(|e| e.pointer("/kind/data/ok"))
        .and_then(|v| v.as_bool());
    assert_eq!(ok2, Some(true), "ход после переподключения не успешен");
    let c2: String = events
        .iter()
        .filter(|e| kind_of(e) == "content")
        .map(text_of)
        .collect();
    assert_eq!(
        c2, "эхо: после переподключения",
        "ответ после переподключения: {c2}"
    );
}

/// Подтверждение тула с телефона: SSE доносит approval_required, POST
/// /v1/approvals доводит решение до РЕАЛЬНО ожидающего канала, повторный
/// approve того же вызова — честный 400.
///
/// Echo-провайдер не вызывает инструменты, поэтому ожидающее подтверждение
/// создаётся тестом тем же способом, что и драйвером хода: oneshot-канал в
/// state.approvals + событие ApprovalRequired в шину. Клиентская половина
/// флоу (SSE + POST) проверяется настоящая.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn phone_approval_event_and_decision_delivery() {
    use swagcod_core::turn::ApprovalDecision;
    use tokio::sync::oneshot;

    let (port, st) = serve().await;
    let mut sse = Sse::open(port).await;
    let cwd = workdir("approval");
    let sid = create_session(port, &cwd, "echo").await;
    let tid = start_turn(port, &sid, "прогрев соединения").await;
    collect_turn(&mut sse, &sid, "прогрев").await;
    let _ = tid;

    // Два ожидающих подтверждения, как два тула в одном ходе.
    let (tx1, mut rx1) = oneshot::channel::<ApprovalDecision>();
    let (tx2, mut rx2) = oneshot::channel::<ApprovalDecision>();
    {
        let mut approvals = st.approvals.lock().await;
        approvals.insert("call-phone-1".into(), tx1);
        approvals.insert("call-phone-2".into(), tx2);
    }
    st.bus
        .publish(swagcod_core::bus::EventKind::ApprovalRequired {
            turn: swagcod_core::TurnId::new("t-phone-approve"),
            call_id: "call-phone-1".into(),
            tool: "bash".into(),
            summary: "rm -rf /tmp/probe".into(),
            preview: None,
        });
    st.bus
        .publish(swagcod_core::bus::EventKind::ApprovalRequired {
            turn: swagcod_core::TurnId::new("t-phone-approve"),
            call_id: "call-phone-2".into(),
            tool: "write".into(),
            summary: "записать файл".into(),
            preview: Some("@@ -1,1 +1,1 @@\n-a\n+b\n".into()),
        });

    // SSE доносит оба approval-события.
    let mut got = Vec::new();
    let deadline = tokio::time::Instant::now() + Duration::from_secs(SSE_SECS);
    while got.len() < 2 {
        let left = deadline.saturating_duration_since(tokio::time::Instant::now());
        if left.is_zero() {
            panic!("approval-события не дошли: {:?}", got);
        }
        match sse.next_event(left.as_secs().max(1)).await {
            Some(ev) => {
                if kind_of(&ev) == "approval_required" {
                    got.push(ev);
                }
            }
            None => panic!(
                "SSE замолчал до approval-событий ({})",
                sse.stopped.clone().unwrap_or_else(|| "таймаут".into())
            ),
        }
    }
    got.sort_by_key(|e| {
        e.pointer("/kind/data/call_id")
            .and_then(|v| v.as_str())
            .unwrap_or("")
            .to_string()
    });
    let ids: Vec<&str> = got
        .iter()
        .map(|e| {
            e.pointer("/kind/data/call_id")
                .and_then(|v| v.as_str())
                .unwrap_or("")
        })
        .collect();
    assert_eq!(
        ids,
        vec!["call-phone-1", "call-phone-2"],
        "approval по SSE: {ids:?}"
    );
    assert_eq!(
        got[0].pointer("/kind/data/tool").and_then(|v| v.as_str()),
        Some("bash"),
        "тул в approval: {}",
        got[0]
    );
    assert_eq!(
        got[0]
            .pointer("/kind/data/summary")
            .and_then(|v| v.as_str()),
        Some("rm -rf /tmp/probe"),
        "сводка в approval: {}",
        got[0]
    );
    assert_eq!(
        got[1]
            .pointer("/kind/data/preview")
            .and_then(|v| v.as_str()),
        Some("@@ -1,1 +1,1 @@\n-a\n+b\n"),
        "diff-preview в approval: {}",
        got[1]
    );

    // Одобряем первый, отказываем второй.
    let r = post(
        port,
        "/v1/approvals",
        r#"{"call_id":"call-phone-1","decision":"approved"}"#,
    )
    .await;
    assert_eq!(r.status, Some(200), "approve: {}", r.raw);
    let r = post(
        port,
        "/v1/approvals",
        r#"{"call_id":"call-phone-2","decision":"denied"}"#,
    )
    .await;
    assert_eq!(r.status, Some(200), "deny: {}", r.raw);

    // Решения доехали до ожидающих каналов (approve доводит ход).
    let d1 = tokio::time::timeout(Duration::from_secs(5), &mut rx1).await;
    assert!(
        matches!(d1, Ok(Ok(ApprovalDecision::Approved))),
        "первый канал не получил approved: {d1:?}"
    );
    let d2 = tokio::time::timeout(Duration::from_secs(5), &mut rx2).await;
    assert!(
        matches!(d2, Ok(Ok(ApprovalDecision::Denied))),
        "второй канал не получил denied: {d2:?}"
    );

    // Повторный approve снятого вызова — честный 400, а не молчаливый 200.
    let r = post(
        port,
        "/v1/approvals",
        r#"{"call_id":"call-phone-1","decision":"approved"}"#,
    )
    .await;
    assert_400(&r, "повторный approve снятого вызова");

    // Сервер жив.
    let r = get(port, "/health").await;
    assert_eq!(r.status, Some(200), "health после approvals: {}", r.raw);
}

/// Ревизия 85: закрыто — probe-дренаж 50 мс вместо слепых 500. Прежде каждый
/// ~500 мс ПОСЛЕ отсылки тела. `drain_after_response` (httpapi.rs:254)
/// после write_all читает сокет до FIN клиента или DRAIN_TIMEOUT=500 мс —
/// обычный клиент, отправивший запрос целиком и ждущий ответа, FIN не
/// шлёт, поэтому сервер молотит пустой read все полсекунды и только потом
/// shutdown. Клиент, честно дочитывающий соединение до конца (а
/// `connection: close` его к этому прямо приглашает), получает +500 мс
/// к КАЖДОМУ запросу: 6-8 запросов на отрисовку чата = ~3 с чистого
/// ожидания EOF. Наблюдение: GET /health — тело по Content-Length
/// приходит мгновенно, EOF отстаёт на ~500 мс. Ожидание: EOF вслед за
/// телом (тело уже получено по Content-Length — держать сокет не для
/// чего). Клиент, живущий по Content-Length, налога не платит, но
/// серверная задача и сокет живут лишние полсекунды на каждый запрос.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn phone_rest_body_arrives_but_eof_lingers_half_second() {
    use tokio::io::AsyncReadExt;

    let (port, _st) = serve().await;
    let mut sock = tokio::net::TcpStream::connect(("127.0.0.1", port))
        .await
        .expect("connect");
    sock.write_all(b"GET /health HTTP/1.1\r\nhost: phone\r\n\r\n")
        .await
        .expect("запрос");

    // Тело читаем строго по Content-Length — как клиент, знающий HTTP.
    let started = std::time::Instant::now();
    let mut buf: Vec<u8> = Vec::new();
    let mut chunk = [0u8; 8192];
    let _cl = loop {
        let n = sock.read(&mut chunk).await.expect("чтение ответа");
        assert!(
            n > 0,
            "EOF до полного ответа: {:?}",
            String::from_utf8_lossy(&buf)
        );
        buf.extend_from_slice(&chunk[..n]);
        let text = String::from_utf8_lossy(&buf);
        if let Some(at) = text.find("\r\n\r\n") {
            let head = &text[..at];
            let cl = head
                .lines()
                .find_map(|l| {
                    let (k, v) = l.split_once(':')?;
                    if k.trim().eq_ignore_ascii_case("content-length") {
                        v.trim().parse::<usize>().ok()
                    } else {
                        None
                    }
                })
                .unwrap_or(usize::MAX);
            if cl != usize::MAX && buf.len() >= at + 4 + cl {
                break cl;
            }
        }
    };
    let body_at = started.elapsed();

    // Тело целиком в руках — connection: close закрывает сразу после.
    let mut rest = Vec::new();
    let _ = tokio::time::timeout(Duration::from_secs(5), sock.read_to_end(&mut rest)).await;
    let eof_at = started.elapsed();
    println!(
        "phone: тело /health за {body_at:?}, EOF за {eof_at:?} (задержка {:?})",
        eof_at - body_at
    );
    assert!(
        eof_at - body_at < Duration::from_millis(200),
        "EOF отстал от тела на {:?}: drain_after_response держит соединение ~500 мс после ответа; тело уже доставлено по Content-Length, держать не для чего",
        eof_at - body_at
    );
}

/// Стоп по чужой сессии: кнопка «стоп» в чате B (живого хода нет) не
/// трогает идущий ход в чате A — честный 400, ход A доезжает до конца.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn phone_stop_of_other_session_does_not_kill_live_turn() {
    let (port, st) = serve().await;
    let mut sse = Sse::open(port).await;

    let sid_a = create_session(port, &workdir("stop-a"), "echo").await;
    let sid_b = create_session(port, &workdir("stop-b"), "echo").await;
    slow_pre_turn_hook(&st, 4);

    let tid = start_turn(port, &sid_a, "ход в A").await;

    // Стоп по сессии B: хода там нет — честный 400.
    let body = serde_json::json!({ "session_id": sid_b }).to_string();
    let r = post(port, "/v1/turns/stop", &body).await;
    assert_400(&r, "стоп по сессии без хода");

    // Ход A не задет: доезжает до конца успешно.
    let events = collect_turn_of(&mut sse, &sid_a, &tid, "ход A при чужом стопе").await;
    let ok = events
        .iter()
        .find(|e| kind_of(e) == "turn_ended" && turn_of(e) == tid)
        .and_then(|e| e.pointer("/kind/data/ok"))
        .and_then(|v| v.as_bool());
    assert_eq!(ok, Some(true), "чужой стоп убил ход A");
    let content: String = events
        .iter()
        .filter(|e| kind_of(e) == "content")
        .map(text_of)
        .collect();
    assert_eq!(content, "эхо: ход в A", "ответ A: {content}");
}
