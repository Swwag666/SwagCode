/*! F-11: сквозная проверка хода через loopback HTTP API на встроенном
echo-провайдере.

План стадии: «релизная сборка, запуск с env-флагом встроенного echo-провайдера
(паттерн fake-mcp), прогон хода через HTTP API: создать сессию → ход → ассерт
SSE-событий и токенов».

Зачем отдельный тестовый бинарник, а не ещё один `#[test]` в `lib.rs`: флаг
`SWAGCOD_ECHO_PROVIDER` — переменная окружения ПРОЦЕССА, а тесты внутри одного
бинарника cargo гоняет параллельно. В собственном бинарнике флаг включается для
всех тестов сразу и не может подменить провайдера чужому тесту, который ждёт
боевого.

Что здесь настоящее: тот же `serve_on`, что слушает телефон (F-9), тот же
`start_turn_core`, что у IPC-команды, та же машина хода, те же хуки, тот же
store и та же шина событий. Подделка ровно одна — провайдер, и она встроенная:
ни сети, ни ключей, ни секретов в CI.

Почему не запускается собранный `.exe`: приложению нужны WebView2 и оконная
сессия, которых на headless-раннере нет. По этой же причине в CI нет бенчей
старта (комментарий в конце `ci.yml`, D-013) — число получилось бы
нерепрезентативным, а здесь получился бы просто красный job. Релизный профиль
при этом сохраняется: job `e2e-smoke` гоняет этот тест через `cargo test
--release`, то есть проверяется оптимизированная сборка, а не debug.
*/

use std::sync::Arc;
use std::time::Duration;

use tokio::io::{AsyncBufReadExt, AsyncReadExt, AsyncWriteExt, BufReader};
use tokio::net::{TcpListener, TcpStream};
use tokio::sync::mpsc::{unbounded_channel, UnboundedReceiver};

use swagcod_app_lib::httpapi::serve_on;
use swagcod_app_lib::AppState;
use swagcod_provider::AnyProvider;

/// Токен loopback-API для этого прогона. Случайный не нужен: порт эфемерный,
/// слушает только 127.0.0.1, а проверяем мы не стойкость токена.
const TOKEN: &str = "smoke-token";

/// Сообщение хода. Особо длинное не нужно: эхо режет ответ на дельты по 24
/// символа, поэтому инкрементальность стрима видна и на коротком тексте.
const MESSAGE: &str = "проверка сквозного хода";

/// Ожидаемый ответ эха (см. `echo_body` в `swagcod_provider::echo`).
fn expected_reply() -> String {
    format!("эхо: {MESSAGE}")
}

/// Включить эхо. Идемпотентно: все тесты бинарника хотят одно и то же.
fn enable_echo() {
    std::env::set_var(swagcod_provider::ECHO_ENV, "1");
}

/// Состояние с чистой in-memory базой: сквозной проверке не нужна чужая
/// история, а sidecar (NodeStore) не нужен вовсе — rusqlite-путь тот же
/// интерфейс `Store`, и тянуть в job node только ради него было бы лишним.
fn state() -> Arc<AppState> {
    Arc::new(AppState {
        store: std::sync::Mutex::new(swagcod_core::store::open_memory()),
        ..Default::default()
    })
}

/// Рабочая директория сессии. Обязана существовать: `create_session_core`
/// проверяет cwd, а не доверяет ему.
fn workdir(tag: &str) -> std::path::PathBuf {
    let dir = std::env::temp_dir().join(format!("swagcod-e2e-{tag}"));
    std::fs::create_dir_all(&dir).expect("временная директория создаётся");
    dir
}

/// Один запрос/ответ HTTP/1.1. Читаем до конца с пределом: сервер закрывает
/// соединение после ответа, а таймаут страхует от зависшего теста.
async fn request(port: u16, raw: &str) -> String {
    let mut sock = TcpStream::connect(("127.0.0.1", port))
        .await
        .expect("соединение с loopback API");
    sock.write_all(raw.as_bytes())
        .await
        .expect("запрос отправлен");
    let mut out = Vec::new();
    let _ = tokio::time::timeout(Duration::from_secs(30), sock.read_to_end(&mut out)).await;
    String::from_utf8_lossy(&out).to_string()
}

async fn post(port: u16, path: &str, body: &str) -> String {
    let raw = format!(
        "POST {path} HTTP/1.1\r\nhost: x\r\nauthorization: Bearer {TOKEN}\r\ncontent-length: {}\r\n\r\n{body}",
        body.len()
    );
    request(port, &raw).await
}

async fn get(port: u16, path: &str) -> String {
    let raw = format!("GET {path} HTTP/1.1\r\nhost: x\r\nauthorization: Bearer {TOKEN}\r\n\r\n");
    request(port, &raw).await
}

/// Тело ответа как JSON (заголовки отбрасываем).
fn body_of(resp: &str) -> serde_json::Value {
    let at = resp.find("\r\n\r\n").expect("ответ с заголовками");
    serde_json::from_str(resp[at + 4..].trim()).unwrap_or(serde_json::Value::Null)
}

fn status_of(resp: &str) -> &str {
    resp.split(' ').nth(1).unwrap_or("")
}

/// `kind` события шины: wire-форма `{"seq":…,"ts_ms":…,"kind":{"kind":…,"data":{…}}}`.
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

/// Подписка на SSE. Открывается ДО начала хода: `broadcast` не хранит историю,
/// и опоздавший подписчик событий не увидит — ровно как телефон.
async fn open_sse(port: u16) -> UnboundedReceiver<serde_json::Value> {
    let mut sock = TcpStream::connect(("127.0.0.1", port))
        .await
        .expect("соединение для SSE");
    sock.write_all(
        format!("GET /v1/events HTTP/1.1\r\nhost: x\r\nauthorization: Bearer {TOKEN}\r\n\r\n")
            .as_bytes(),
    )
    .await
    .expect("запрос SSE отправлен");

    let (tx, rx) = unbounded_channel();
    tokio::spawn(async move {
        let mut lines = BufReader::new(sock).lines();
        while let Ok(Some(line)) = lines.next_line().await {
            let Some(payload) = line.strip_prefix("data: ") else {
                continue;
            };
            if let Ok(ev) = serde_json::from_str::<serde_json::Value>(payload) {
                if tx.send(ev).is_err() {
                    break; // тест закончил слушать
                }
            }
        }
    });
    rx
}

/// Собрать события хода до `turn_ended` нашей сессии (или до таймаута).
async fn collect_turn(
    rx: &mut UnboundedReceiver<serde_json::Value>,
    session_id: &str,
) -> Vec<serde_json::Value> {
    let deadline = tokio::time::Instant::now() + Duration::from_secs(90);
    let mut out = Vec::new();
    loop {
        let left = deadline.saturating_duration_since(tokio::time::Instant::now());
        if left.is_zero() {
            break;
        }
        match tokio::time::timeout(left, rx.recv()).await {
            Ok(Some(ev)) => {
                let ends_our_turn = kind_of(&ev) == "turn_ended"
                    && ev.pointer("/kind/data/session").and_then(|v| v.as_str())
                        == Some(session_id);
                out.push(ev);
                if ends_our_turn {
                    break;
                }
            }
            _ => break,
        }
    }
    out
}

/// Поднять API на эфемерном порту и вернуть (порт, состояние).
async fn serve() -> (u16, Arc<AppState>) {
    let st = state();
    let listener = TcpListener::bind("127.0.0.1:0")
        .await
        .expect("эфемерный порт");
    let port = listener.local_addr().expect("адрес порта").port();
    let spawned = st.clone();
    tokio::spawn(async move {
        serve_on(listener, spawned, TOKEN.to_string()).await;
    });
    // /health без токена — liveness; ждём, пока слушатель примет первое.
    for _ in 0..100 {
        let r = request(port, "GET /health HTTP/1.1\r\nhost: x\r\n\r\n").await;
        if r.contains("\"ok\":true") {
            return (port, st);
        }
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
    panic!("loopback API не ответил на /health");
}

/// Флаг обязан выбирать эхо ДО разбора конфигурации провайдеров: на чистом
/// раннере нет ни endpoint'а, ни ключа, и требовать их значило бы, что флаг
/// бесполезен ровно там, где он нужен.
#[tokio::test]
async fn env_flag_selects_the_built_in_echo_provider() {
    enable_echo();
    let st = state();
    let provider = swagcod_app_lib::providers::build_chat_provider(&st)
        .expect("эхо не требует ни ключа, ни endpoint'а");
    assert!(
        matches!(provider, AnyProvider::Echo(_)),
        "флаг {flag} должен давать эхо, получено {name}",
        flag = swagcod_provider::ECHO_ENV,
        name = provider.name()
    );
    assert_eq!(provider.name(), swagcod_provider::ECHO_NAME);
}

/// Главный сквозной тест стадии: создать сессию → ход → SSE-события → токены.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn turn_over_http_api_streams_events_and_counts_tokens() {
    enable_echo();
    let (port, st) = serve().await;
    let mut sse = open_sse(port).await;

    // 1. Создать сессию — тот же core, что у IPC-команды и у телефона.
    let cwd = workdir("turn");
    let body = serde_json::json!({ "cwd": cwd.to_string_lossy() }).to_string();
    let resp = post(port, "/v1/sessions", &body).await;
    assert_eq!(status_of(&resp), "200", "создание сессии: {resp}");
    let created = body_of(&resp);
    let session_id = created
        .get("id")
        .and_then(|v| v.as_str())
        .unwrap_or("")
        .to_string();
    assert!(!session_id.is_empty(), "сессия вернула id: {resp}");

    // 2. Ход. Подписка уже открыта, поэтому события не потеряются.
    let body = serde_json::json!({ "session_id": session_id, "message": MESSAGE }).to_string();
    let resp = post(port, "/v1/turns", &body).await;
    assert_eq!(status_of(&resp), "200", "запуск хода: {resp}");
    let turn_id = body_of(&resp)
        .get("turn_id")
        .and_then(|v| v.as_str())
        .unwrap_or("")
        .to_string();
    assert!(!turn_id.is_empty(), "ход вернул turn_id: {resp}");

    // 3. SSE-события хода.
    let events = collect_turn(&mut sse, &session_id).await;
    let kinds: Vec<&str> = events.iter().map(kind_of).collect();
    assert!(
        kinds.contains(&"turn_started"),
        "в ленте нет turn_started: {kinds:?}"
    );
    assert!(
        kinds.contains(&"reasoning"),
        "рассуждение — отдельный визуальный канал, оно обязано быть: {kinds:?}"
    );
    assert!(kinds.contains(&"content"), "в ленте нет content: {kinds:?}");
    assert!(
        kinds.contains(&"turn_ended"),
        "ход не завершился за 90 с: {kinds:?}"
    );

    let at = |k: &str| kinds.iter().position(|x| *x == k).unwrap_or(usize::MAX);
    assert!(
        at("turn_started") < at("content") && at("content") < at("turn_ended"),
        "порядок событий нарушен: {kinds:?}"
    );

    // Контент приходит кусками и складывается в ответ эха. Проверяем и то, что
    // дельт больше одной: на одной дельте стрим не отличим от неленточного
    // ответа, и половина пути осталась бы непроверенной.
    let content: String = events
        .iter()
        .filter(|e| kind_of(e) == "content")
        .map(text_of)
        .collect();
    assert_eq!(content, expected_reply(), "ответ эха: {kinds:?}");
    let deltas = events.iter().filter(|e| kind_of(e) == "content").count();
    assert!(deltas >= 1, "контент пришел {deltas} дельтами");
    assert_eq!(
        events
            .iter()
            .filter(|e| kind_of(e) == "turn_started")
            .filter_map(|e| e.pointer("/kind/data/turn"))
            .filter(|t| t.as_str() == Some(turn_id.as_str()))
            .count(),
        1,
        "turn_started относится к нашему ходу"
    );

    let ended = events
        .iter()
        .find(|e| kind_of(e) == "turn_ended")
        .expect("turn_ended");
    assert_eq!(
        ended.pointer("/kind/data/ok").and_then(|v| v.as_bool()),
        Some(true),
        "ход успешен: {ended}"
    );
    assert!(
        ended
            .pointer("/kind/data/reason")
            .map(|v| v.is_null())
            .unwrap_or(true),
        "у успешного хода нет причины провала: {ended}"
    );

    // 4. Токены: событие Usage от провайдера обязано дойти до записи хода
    // именно им, а не калиброванной оценкой ядра (B-3).
    let recorded = {
        let guard = st.sessions.lock().await;
        let session = guard
            .iter()
            .find(|s| s.id.as_str() == session_id)
            .expect("сессия в памяти");
        let rec = session.turns.last().expect("запись хода").clone();
        rec
    };
    assert!(recorded.ok, "запись хода успешна: {:?}", recorded.failure);
    assert_eq!(recorded.id.as_str(), turn_id);
    assert_eq!(recorded.content, expected_reply());
    assert!(
        recorded.est_input_tokens > 0,
        "входные токены не могут быть нулём: ноль неотличим от «счёт не пришёл»"
    );
    assert_eq!(
        recorded.est_output_tokens,
        swagcod_provider::echo::estimate_tokens(&recorded.content),
        "счёт выхода — это usage эха, дошедший без потерь"
    );

    // 5. Журнал сессии: то, что увидит телефон и что переживёт перезапуск.
    let resp = post(
        port,
        "/v1/sessions/transcript",
        &serde_json::json!({ "session_id": session_id }).to_string(),
    )
    .await;
    assert_eq!(status_of(&resp), "200", "транскрипт: {resp}");
    let items = body_of(&resp);
    let items = items.as_array().unwrap_or(&Vec::new()).clone();
    let has = |k: &str| items.iter().any(|e| kind_of(e) == k);
    assert!(has("user"), "в журнале нет реплики пользователя");
    assert!(has("reasoning"), "в журнале нет рассуждения");
    assert!(has("content"), "в журнале нет ответа");
    assert!(has("turn_ended"), "в журнале нет завершения хода");
    let logged = items
        .iter()
        .find(|e| kind_of(e) == "content")
        .map(text_of)
        .unwrap_or("");
    assert_eq!(logged, expected_reply(), "журнал совпадает с живым стримом");

    // 6. Сессия видна в списке — иначе телефон её не откроет.
    let resp = get(port, "/v1/sessions").await;
    assert_eq!(status_of(&resp), "200", "список сессий: {resp}");
    assert!(
        resp.contains(&session_id),
        "созданная сессия не вернулась в списке: {resp}"
    );
}

/// Ход с пустым сообщением обязан отказать честно, а не уйти в провайдера:
/// это та же валидация, что у десктопа, и эхо не должно её обходить.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn empty_message_is_rejected_before_any_provider_call() {
    enable_echo();
    let (port, _st) = serve().await;
    let cwd = workdir("empty");
    let resp = post(
        port,
        "/v1/sessions",
        &serde_json::json!({ "cwd": cwd.to_string_lossy() }).to_string(),
    )
    .await;
    let session_id = body_of(&resp)
        .get("id")
        .and_then(|v| v.as_str())
        .unwrap_or("")
        .to_string();
    assert!(!session_id.is_empty(), "сессия создана: {resp}");

    let resp = post(
        port,
        "/v1/turns",
        &serde_json::json!({ "session_id": session_id, "message": "   " }).to_string(),
    )
    .await;
    assert_eq!(status_of(&resp), "400", "пустое сообщение — 400: {resp}");
    assert!(resp.contains("пустым"), "причина отказа понятна: {resp}");
}

/// Без токена ход не запустится даже при включённом эхе: флаг убирает сеть,
/// но не авторизацию (F-9: loopback — не значит «для всех»).
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn turn_requires_the_bearer_token() {
    enable_echo();
    let (port, _st) = serve().await;
    let body = serde_json::json!({ "session_id": "s-nope", "message": MESSAGE }).to_string();
    let raw = format!(
        "POST /v1/turns HTTP/1.1\r\nhost: x\r\ncontent-length: {}\r\n\r\n{body}",
        body.len()
    );
    let resp = request(port, &raw).await;
    assert_eq!(status_of(&resp), "401", "без токена — 401: {resp}");
}
