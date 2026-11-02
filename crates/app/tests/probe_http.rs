/*! probe_http: бета-прогон кривыми руками по loopback HTTP API.

Поднимает сервер так же, как e2e_smoke (echo-провайдер через env-флаг,
эфемерный порт, фиксированный токен, `serve_on` без DPAPI), и бьёт его
сырыми байтами через tokio TcpStream — без reqwest, чтобы контролировать
каждый байт: мусорные JSON, битые Content-Length, oversized head/body,
чужие методы, кривые пути, обрывы SSE, гонки на /v1/turns.

Контракт ожиданий — crates/app/src/httpapi.rs: честные 400 с
{"error":...}, 401 без токена, 404 на неизвестный маршрут, 200 на живых,
SSE-ответ 200 с text/event-stream. Находка = паника, зависание, ответ
вне набора, тихий обрыв без ответа, потеря ответа при reset.

Кейсы, которые падают (находки), помечаются #[ignore] с комментарием
FINDING: репро живёт в файле, cargo test остаётся зелёным. */

use std::sync::Arc;
use std::time::Duration;

use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::{TcpListener, TcpStream};

use swagcod_app_lib::httpapi::serve_on;
use swagcod_app_lib::AppState;

const TOKEN: &str = "probe-token";

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
        sock.write_all(b"GET /health HTTP/1.1\r\nhost: x\r\n\r\n")
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

/// Один запрос сырыми байтами: пишет, читает до EOF с таймаутом.
/// Возврат: (прочитанные байты, сбой). Сбой = ошибка записи/чтения
/// (в т.ч. connection reset) или "timeout: ..." — EOF не наступил.
async fn request(port: u16, raw: &[u8]) -> (Vec<u8>, Option<String>) {
    let mut sock = match TcpStream::connect(("127.0.0.1", port)).await {
        Ok(s) => s,
        Err(e) => return (Vec::new(), Some(format!("connect: {e}"))),
    };
    if let Err(e) = sock.write_all(raw).await {
        // Ответ мог прийти до сбоя записи — пытаемся дочитать.
        let mut out = Vec::new();
        let _ = tokio::time::timeout(Duration::from_secs(2), sock.read_to_end(&mut out)).await;
        return (out, Some(format!("write: {e}")));
    }
    let mut out = Vec::new();
    match tokio::time::timeout(Duration::from_secs(10), sock.read_to_end(&mut out)).await {
        Err(_) => (out, Some("timeout: EOF не наступил за 10 с".into())),
        Ok(Err(e)) => (out, Some(format!("read: {e}"))),
        Ok(Ok(_)) => (out, None),
    }
}

fn status(bytes: &[u8]) -> Option<u16> {
    let s = String::from_utf8_lossy(bytes);
    let line = s.lines().next()?;
    line.split(' ').nth(1)?.parse().ok()
}

fn resp_text(bytes: &[u8]) -> String {
    String::from_utf8_lossy(bytes).to_string()
}

fn body_text(bytes: &[u8]) -> String {
    let s = String::from_utf8_lossy(bytes);
    match s.find("\r\n\r\n") {
        Some(at) => s[at + 4..].to_string(),
        None => s.to_string(),
    }
}

/// Строгая проверка: канал чист, статус из документированного набора.
fn assert_clean_in_set(ctx: &str, bytes: &[u8], err: &Option<String>) -> u16 {
    let text = resp_text(bytes);
    assert!(err.is_none(), "{ctx}: сбой канала {err:?}\n{text}");
    let st = status(bytes);
    assert!(
        matches!(st, Some(200 | 400 | 401 | 404)),
        "{ctx}: статус {st:?} вне набора\n{text}"
    );
    st.unwrap()
}

/// Мягкая проверка для oversized-кейсов: ответ обязан дойти до клиента
/// (пусть даже EOF после него грязный — reset). Потеря ответа = находка.
fn assert_delivered_in_set(ctx: &str, bytes: &[u8], err: &Option<String>) -> u16 {
    let st = status(bytes);
    assert!(
        matches!(st, Some(200 | 400 | 401 | 404)),
        "{ctx}: ответ не доставлен клиенту (сбой: {err:?})\n{}",
        resp_text(bytes)
    );
    st.unwrap()
}

fn post_raw(path: &str, body: &[u8]) -> Vec<u8> {
    let mut raw = format!(
        "POST {path} HTTP/1.1\r\nhost: x\r\nauthorization: Bearer {TOKEN}\r\ncontent-length: {}\r\n\r\n",
        body.len()
    )
    .into_bytes();
    raw.extend_from_slice(body);
    raw
}

fn post_json(path: &str, body: &str) -> Vec<u8> {
    post_raw(path, body.as_bytes())
}

fn post_head(path: &str, extra_head: &str, body: &[u8]) -> Vec<u8> {
    let mut raw =
        format!("POST {path} HTTP/1.1\r\nhost: x\r\nauthorization: Bearer {TOKEN}\r\n{extra_head}")
            .into_bytes();
    raw.extend_from_slice(b"\r\n");
    raw.extend_from_slice(body);
    raw
}

fn get_raw(path: &str) -> Vec<u8> {
    format!("GET {path} HTTP/1.1\r\nhost: x\r\nauthorization: Bearer {TOKEN}\r\n\r\n").into_bytes()
}

fn get_noauth(path: &str) -> Vec<u8> {
    format!("GET {path} HTTP/1.1\r\nhost: x\r\n\r\n").into_bytes()
}

fn contains(hay: &[u8], needle: &[u8]) -> bool {
    hay.windows(needle.len()).any(|w| w == needle)
}

/// Дочитать из сокета всё, что приедет за `secs` (только новые байты).
async fn read_more(sock: &mut TcpStream, secs: u64) -> Vec<u8> {
    let mut out = Vec::new();
    let mut chunk = [0u8; 4096];
    let deadline = tokio::time::Instant::now() + Duration::from_secs(secs);
    loop {
        let left = deadline.saturating_duration_since(tokio::time::Instant::now());
        if left.is_zero() {
            break;
        }
        match tokio::time::timeout(left, sock.read(&mut chunk)).await {
            Ok(Ok(0)) => break,
            Ok(Ok(n)) => {
                out.extend_from_slice(&chunk[..n]);
                if out.len() > 64 * 1024 {
                    break;
                }
            }
            _ => break,
        }
    }
    out
}

/// Прочитать заголовки ответа до \r\n\r\n (для SSE-рукопожатия).
async fn read_head(sock: &mut TcpStream) -> String {
    let mut acc = Vec::new();
    let mut chunk = [0u8; 4096];
    let deadline = tokio::time::Instant::now() + Duration::from_secs(5);
    loop {
        let left = deadline.saturating_duration_since(tokio::time::Instant::now());
        if left.is_zero() {
            break;
        }
        match tokio::time::timeout(left, sock.read(&mut chunk)).await {
            Ok(Ok(0)) => break,
            Ok(Ok(n)) => {
                acc.extend_from_slice(&chunk[..n]);
                if contains(&acc, b"\r\n\r\n") {
                    break;
                }
            }
            _ => break,
        }
    }
    String::from_utf8_lossy(&acc).to_string()
}

fn workdir(tag: &str) -> std::path::PathBuf {
    let dir = std::env::temp_dir().join(format!("swagcod-probe-{tag}"));
    std::fs::create_dir_all(&dir).expect("временная директория создаётся");
    dir
}

// ---------- мусорные JSON-тела ----------

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn garbage_json_bodies_get_honest_400() {
    let (port, _st) = serve().await;
    let cases: Vec<(&str, Vec<u8>)> = vec![
        ("обрубленный json", b"{\"session_id\":".to_vec()),
        ("массив", b"[]".to_vec()),
        ("null", b"null".to_vec()),
        ("пустой объект", b"{}".to_vec()),
        ("число", b"5".to_vec()),
        ("строка", b"\"x\"".to_vec()),
        ("true", b"true".to_vec()),
        ("session_id число", b"{\"session_id\": 123}".to_vec()),
        ("session_id массив", b"{\"session_id\": [\"a\"]}".to_vec()),
        ("BOM перед json", {
            let mut v = vec![0xEF, 0xBB, 0xBF];
            v.extend_from_slice(b"{\"session_id\":\"s\"}");
            v
        }),
        ("не-UTF8 байты", vec![0xFF, 0xFE, 0x00, 0xC3]),
        ("вложенность 200", vec![b'['; 200]),
    ];
    for (name, body) in &cases {
        let (out, err) = request(port, &post_raw("/v1/turns", body)).await;
        let st = assert_clean_in_set(name, &out, &err);
        assert_eq!(st, 400, "{name}: ждём 400\n{}", body_text(&out));
        assert!(
            body_text(&out).contains("\"error\""),
            "{name}: нет error\n{}",
            body_text(&out)
        );
    }

    for (name, body) in [
        ("approvals массив", "[]"),
        ("approvals call_id объект", r#"{"call_id": {"x": 1}}"#),
    ] {
        let (out, err) = request(port, &post_json("/v1/approvals", body)).await;
        let st = assert_clean_in_set(name, &out, &err);
        assert_eq!(st, 400, "{name}\n{}", body_text(&out));
    }
}

/// FINDING: мусорный JSON на POST /v1/sessions создаёт сессию.
/// `[]`, `null`, `5`, `{"cwd": 5}` — str_field молча даёт пустой cwd,
/// а create_session_core трактует пустой cwd как USERPROFILE и возвращает
/// 200 с настоящей сессией в домашнем каталоге. По контракту httpapi.rs
/// («телефон получает ту же валидацию cwd», честный 400 на мусор у
/// остальных POST-маршрутов) здесь ожидается 400.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn garbage_json_on_v1_sessions_should_be_rejected() {
    let (port, _st) = serve().await;
    for (name, body) in [
        ("sessions массив", "[]"),
        ("sessions null", "null"),
        ("sessions число", "5"),
        ("sessions cwd не строка", r#"{"cwd": 5}"#),
    ] {
        let (out, err) = request(port, &post_json("/v1/sessions", body)).await;
        let st = assert_clean_in_set(name, &out, &err);
        assert_eq!(st, 400, "{name}\n{}", body_text(&out));
    }
}

// ---------- Content-Length ----------

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn content_length_variants_get_honest_400() {
    let (port, _st) = serve().await;
    // (имя, строки заголовков, тело)
    let cases: Vec<(&str, &str, Vec<u8>)> = vec![
        ("CL 0", "content-length: 0\r\n", Vec::new()),
        ("CL отрицательный", "content-length: -1\r\n", b"{}".to_vec()),
        ("CL не число", "content-length: abc\r\n", b"{}".to_vec()),
        ("CL дробный", "content-length: 1.5\r\n", b"{}".to_vec()),
        (
            "CL переполняет usize",
            "content-length: 99999999999999999999999999\r\n",
            b"{}".to_vec(),
        ),
        (
            "CL usize::MAX",
            "content-length: 18446744073709551615\r\n",
            b"{}".to_vec(),
        ),
        (
            "два разных CL",
            "content-length: 5\r\nContent-Length: 6\r\n",
            b"{}".to_vec(),
        ),
        (
            "два одинаковых CL",
            "content-length: 5\r\ncontent-length: 5\r\n",
            b"{}".to_vec(),
        ),
        // RFC 7230 требует только цифры; "+5" — языковая вольность
        // usize::from_str. Ожидаем, что парсер её допускает, но
        // маршрут всё равно честно 400 на битом JSON.
        ("CL +5", "content-length: +5\r\n", b"{\"a\":".to_vec()),
        (
            "CL с пробелами",
            "content-length:   7  \r\n",
            b"{\"a\":1}".to_vec(),
        ),
        ("нет CL вовсе", "", b"{}".to_vec()),
    ];
    for (name, extra_head, body) in &cases {
        let (out, err) = request(port, &post_head("/v1/turns", extra_head, body)).await;
        let st = assert_clean_in_set(name, &out, &err);
        assert_eq!(st, 400, "{name}\n{}", body_text(&out));
        assert!(
            body_text(&out).contains("\"error\""),
            "{name}: нет error\n{}",
            body_text(&out)
        );
    }

    // Лимит тела применяется до маршрутизации — даже к GET /health.
    let raw = "GET /health HTTP/1.1\r\nhost: x\r\ncontent-length: 99999999999\r\n\r\n";
    let (out, err) = request(port, raw.as_bytes()).await;
    let st = assert_clean_in_set("GET с гигантским CL", &out, &err);
    assert_eq!(st, 400, "\n{}", body_text(&out));
}

// ---------- границы лимита тела ----------

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn body_limit_boundary_is_honest() {
    let (port, _st) = serve().await;

    // Ровно на лимите: 1 МиБ мусора — 400 от JSON-парсера.
    let body = vec![b'A'; 1024 * 1024];
    let (out, err) = request(port, &post_raw("/v1/turns", &body)).await;
    let st = assert_clean_in_set("тело ровно 1 МиБ", &out, &err);
    assert_eq!(st, 400, "\n{}", body_text(&out));

    // Чуть больше лимита, тело короткое: немедленный 400, тело не ждём.
    let raw = post_head("/v1/turns", "content-length: 1048577\r\n", b"AAAAAAAAAA");
    let (out, err) = request(port, &raw).await;
    let st = assert_delivered_in_set("тело 1 МиБ + 1", &out, &err);
    assert_eq!(st, 400, "\n{}", body_text(&out));
    assert!(
        body_text(&out).contains("1 МБ"),
        "причина про лимит: {}",
        body_text(&out)
    );

    // CL объявлен, тело оборвано (FIN посреди): честный 400 «не дочитано».
    let mut sock = TcpStream::connect(("127.0.0.1", port)).await.unwrap();
    sock.write_all(&post_head(
        "/v1/turns",
        "content-length: 100000\r\n",
        b"AAAAA",
    ))
    .await
    .unwrap();
    sock.shutdown().await.unwrap();
    let mut out = Vec::new();
    let _ = tokio::time::timeout(Duration::from_secs(5), sock.read_to_end(&mut out)).await;
    let st = assert_delivered_in_set("тело оборвано FIN'ом", &out, &None);
    assert_eq!(st, 400, "\n{}", body_text(&out));
}

// ---------- чужие методы ----------

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn foreign_methods_fall_to_documented_answers() {
    let (port, _st) = serve().await;
    for m in ["PUT", "DELETE", "OPTIONS", "TRACE", "PATCH", "GARBAGE"] {
        // С токеном: маршрут не существует для этого метода — 404 из
        // документированного набора (405 в роутере не предусмотрено).
        let raw = format!(
            "{m} /v1/sessions HTTP/1.1\r\nhost: x\r\nauthorization: Bearer {TOKEN}\r\n\r\n"
        );
        let (out, err) = request(port, raw.as_bytes()).await;
        let st = assert_clean_in_set(m, &out, &err);
        assert_eq!(st, 404, "{m}\n{}", body_text(&out));
        // Без токена — сначала ворота авторизации: 401.
        let raw = format!("{m} /v1/sessions HTTP/1.1\r\nhost: x\r\n\r\n");
        let (out, err) = request(port, raw.as_bytes()).await;
        let st = assert_clean_in_set(&format!("{m} без токена"), &out, &err);
        assert_eq!(st, 401, "{m}\n{}", body_text(&out));
    }

    // GET с телом: тело читается и игнорируется, маршрут живой — 200.
    let mut raw = format!(
        "GET /v1/sessions HTTP/1.1\r\nhost: x\r\nauthorization: Bearer {TOKEN}\r\ncontent-length: 5\r\n\r\n"
    )
    .into_bytes();
    raw.extend_from_slice(b"hello");
    let (out, err) = request(port, &raw).await;
    let st = assert_clean_in_set("GET с телом", &out, &err);
    assert_eq!(st, 200, "\n{}", body_text(&out));
}

// ---------- кривые пути ----------

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn weird_paths_get_documented_answers() {
    let (port, _st) = serve().await;
    // (путь, ожидание)
    for (path, want) in [
        ("/v1/sessions/", 404),          // хвостовой слэш
        ("//v1//sessions", 404),         // двойные слэши
        ("/v1/SESSIONS", 404),           // регистр
        ("/v1/сессии", 404),             // юникод
        ("/v1/sessions/%20", 404),       // процент-кодирование
        ("/v1/sessions?x=1", 200),       // query отбрасывается
        ("/v1/sessions?x=1&y=%zz", 200), // мусорный query
    ] {
        let (out, err) = request(port, &get_raw(path)).await;
        let st = assert_clean_in_set(path, &out, &err);
        assert_eq!(st, want, "{path}\n{}", body_text(&out));
    }
}

// ---------- авторизация ----------

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn auth_variants_keep_the_gate() {
    let (port, _st) = serve().await;
    // (строка Authorization, ожидание)
    let cases: Vec<(&str, &str, u16)> = vec![
        ("нет заголовка", "", 401),
        ("голый токен без схемы", TOKEN, 401),
        ("чужой токен", "Bearer wrong", 401),
        ("токен другой длины", "Bearer probe-toke", 401),
        ("лишнее слово после токена", "Bearer probe-token extra", 401),
        ("Basic со своим токеном", "Basic probe-token", 401),
        ("Bearer без значения", "Bearer", 401),
        ("маленькая схема", "bearer probe-token", 200),
        ("ВЕРХНИЙ регистр схемы", "BEARER probe-token", 200),
        ("двойной пробел", "Bearer   probe-token", 200),
        ("хвостовой пробел", "Bearer probe-token ", 200),
    ];
    for (name, auth_value, want) in &cases {
        let head = if auth_value.is_empty() {
            "GET /v1/sessions HTTP/1.1\r\nhost: x\r\n\r\n".to_string()
        } else {
            format!("GET /v1/sessions HTTP/1.1\r\nhost: x\r\nauthorization: {auth_value}\r\n\r\n")
        };
        let (out, err) = request(port, head.as_bytes()).await;
        let st = assert_clean_in_set(name, &out, &err);
        assert_eq!(st, *want, "{name}\n{}", body_text(&out));
    }
}

// ---------- мусор в стартовой строке и заголовках ----------

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn head_garbage_gets_documented_answers() {
    let (port, _st) = serve().await;

    // Только пустая строка запроса.
    let (out, err) = request(port, b"\r\n\r\n").await;
    let st = assert_clean_in_set("пустой запрос", &out, &err);
    assert_eq!(st, 400, "\n{}", body_text(&out));

    // Метод без пути.
    let (out, err) = request(port, b"GET\r\n\r\n").await;
    let st = assert_clean_in_set("нет пути", &out, &err);
    assert_eq!(st, 400, "\n{}", body_text(&out));

    // Ведущий CRLF перед стартовой строкой (RFC: SHOULD игнорировать).
    let (out, err) = request(port, b"\r\nGET /health HTTP/1.1\r\nhost: x\r\n\r\n").await;
    let st = assert_clean_in_set("ведущий CRLF", &out, &err);
    assert_eq!(st, 400, "\n{}", body_text(&out));

    // Бинарный мусор в стартовой строке.
    let (out, err) = request(port, b"\xFF\xFE GET /health HTTP/1.1\r\nhost: x\r\n\r\n").await;
    let st = assert_clean_in_set("битые байты в методе", &out, &err);
    assert!(matches!(st, 400 | 401), "\n{}", body_text(&out));

    // Заголовок без имени и без двоеточия — игнорируются, запрос жив.
    let raw = b"GET /health HTTP/1.1\r\nhost: x\r\njust garbage no colon\r\n: empty name\r\n\r\n";
    let (out, err) = request(port, raw).await;
    let st = assert_clean_in_set("заголовок без двоеточия", &out, &err);
    assert_eq!(st, 200, "\n{}", body_text(&out));

    // Вообще без заголовков.
    let (out, err) = request(port, b"GET /health HTTP/1.1\r\n\r\n").await;
    let st = assert_clean_in_set("без заголовков", &out, &err);
    assert_eq!(st, 200, "\n{}", body_text(&out));
}

// ---------- pipelining ----------

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn pipelined_second_request_is_not_served() {
    // Документированное поведение: connection: close, без keep-alive —
    // второй запрос в том же соединении не обслуживается, ответ один.
    let (port, _st) = serve().await;
    let raw = b"GET /health HTTP/1.1\r\nhost: x\r\n\r\nGET /health HTTP/1.1\r\nhost: x\r\n\r\n";
    let (out, err) = request(port, raw).await;
    let st = assert_clean_in_set("pipelining", &out, &err);
    assert_eq!(st, 200, "\n{}", body_text(&out));
    let text = resp_text(&out);
    assert_eq!(
        text.matches("HTTP/1.1").count(),
        1,
        "обслужен только первый запрос\n{text}"
    );
}

// ---------- SSE ----------

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn sse_abrupt_disconnect_leaves_server_alive() {
    let (port, st) = serve().await;
    let mut sock = TcpStream::connect(("127.0.0.1", port)).await.unwrap();
    sock.write_all(
        format!("GET /v1/events HTTP/1.1\r\nhost: x\r\nauthorization: Bearer {TOKEN}\r\n\r\n")
            .as_bytes(),
    )
    .await
    .unwrap();
    let head = read_head(&mut sock).await;
    assert!(head.starts_with("HTTP/1.1 200"), "{head}");
    assert!(head.contains("text/event-stream"), "{head}");

    st.bus.publish(swagcod_core::bus::EventKind::Status {
        message: "sse-проба-обрыв".into(),
    });
    let ev = read_more(&mut sock, 3).await;
    assert!(
        contains(&ev, b"data: ") && contains(&ev, "sse-проба-обрыв".as_bytes()),
        "событие не доехало: {}",
        String::from_utf8_lossy(&ev)
    );

    // Резкий обрыв посреди потока.
    drop(sock);
    // Событие в разорванный сокет не должно уронить сервер.
    st.bus.publish(swagcod_core::bus::EventKind::Status {
        message: "после-обрыва".into(),
    });
    tokio::time::sleep(Duration::from_millis(200)).await;
    let (out, err) = request(port, &get_noauth("/health")).await;
    let st = assert_clean_in_set("жив после обрыва SSE", &out, &err);
    assert_eq!(st, 200, "\n{}", body_text(&out));
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn sse_second_request_in_same_connection_is_ignored() {
    // SSE-соединение занято потоком: второй запрос в нём не обслуживается
    // (нет keep-alive), сервер продолжает жить.
    let (port, _st) = serve().await;
    let mut sock = TcpStream::connect(("127.0.0.1", port)).await.unwrap();
    sock.write_all(
        format!("GET /v1/events HTTP/1.1\r\nhost: x\r\nauthorization: Bearer {TOKEN}\r\n\r\n")
            .as_bytes(),
    )
    .await
    .unwrap();
    let head = read_head(&mut sock).await;
    assert!(head.contains("text/event-stream"), "{head}");
    tokio::time::sleep(Duration::from_millis(100)).await;

    sock.write_all(b"GET /health HTTP/1.1\r\nhost: x\r\n\r\n")
        .await
        .unwrap();
    let extra = read_more(&mut sock, 2).await;
    assert!(
        extra.is_empty(),
        "второй запрос в SSE-соединении получил ответ: {}",
        String::from_utf8_lossy(&extra)
    );
    drop(sock);
    let (out, err) = request(port, &get_noauth("/health")).await;
    let st = assert_clean_in_set("жив после второго запроса в SSE", &out, &err);
    assert_eq!(st, 200, "\n{}", body_text(&out));
}

// ---------- гонки ----------

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn concurrent_turns_on_one_session_no_crash() {
    let (port, _st) = serve().await;
    let cwd = workdir("race");
    let body = serde_json::json!({ "cwd": cwd.to_string_lossy() }).to_string();
    let (out, err) = request(port, &post_json("/v1/sessions", &body)).await;
    let st = assert_clean_in_set("сессия для гонки", &out, &err);
    assert_eq!(st, 200, "\n{}", body_text(&out));
    let sid = serde_json::from_str::<serde_json::Value>(&body_text(&out))
        .expect("JSON-тело")
        .get("id")
        .and_then(|v| v.as_str())
        .unwrap_or("")
        .to_string();
    assert!(!sid.is_empty(), "нет id: {}", body_text(&out));

    let msg = serde_json::json!({ "session_id": sid, "message": "го" }).to_string();
    let raw1 = post_json("/v1/turns", &msg);
    let raw2 = post_json("/v1/turns", &msg);
    let ((o1, e1), (o2, e2)) = tokio::join!(request(port, &raw1), request(port, &raw2));
    println!(
        "гонка: ход-1 = {:?} ({}) ход-2 = {:?} ({})",
        status(&o1),
        body_text(&o1),
        status(&o2),
        body_text(&o2)
    );
    for (name, o, e) in [("ход-1", &o1, &e1), ("ход-2", &o2, &e2)] {
        let st = assert_clean_in_set(name, o, e);
        assert!(matches!(st, 200 | 400), "{name}: {st}\n{}", body_text(o));
        if st == 400 {
            assert!(
                body_text(o).contains("\"error\""),
                "{name}: 400 без error\n{}",
                body_text(o)
            );
        }
    }
    assert!(
        status(&o1) == Some(200) || status(&o2) == Some(200),
        "хотя бы один ход запустился"
    );

    // Дать эху завершиться и убедиться, что сессия не сломалась.
    tokio::time::sleep(Duration::from_millis(800)).await;
    let tr = post_json(
        "/v1/sessions/transcript",
        &serde_json::json!({ "session_id": sid }).to_string(),
    );
    let (out, err) = request(port, &tr).await;
    let st = assert_clean_in_set("транскрипт после гонки", &out, &err);
    assert_eq!(st, 200, "\n{}", body_text(&out));
    let (out, err) = request(port, &get_noauth("/health")).await;
    assert_eq!(
        assert_clean_in_set("жив после гонки", &out, &err),
        200,
        "\n{}",
        body_text(&out)
    );
}

// ---------- стоп и approvals на пустом месте ----------

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn stop_and_approvals_edges_are_honest() {
    let (port, _st) = serve().await;

    // Стоп несуществующей сессии.
    let (out, err) = request(
        port,
        &post_json("/v1/turns/stop", r#"{"session_id":"nope"}"#),
    )
    .await;
    let st = assert_clean_in_set("stop чужой сессии", &out, &err);
    assert_eq!(st, 400, "\n{}", body_text(&out));

    // Стоп с session_id в 10000 символов.
    let long_sid = format!("{{\"session_id\":\"{}\"}}", "a".repeat(10000));
    let (out, err) = request(port, &post_json("/v1/turns/stop", &long_sid)).await;
    let st = assert_clean_in_set("stop 10000-символьный id", &out, &err);
    assert_eq!(st, 400, "\n{}", body_text(&out));

    // Стоп без session_id.
    let (out, err) = request(port, &post_json("/v1/turns/stop", "{}")).await;
    let st = assert_clean_in_set("stop без id", &out, &err);
    assert_eq!(st, 400, "\n{}", body_text(&out));

    // Approvals на чужой call_id — дважды, оба раза честный 400.
    for i in 1..=2 {
        let (out, err) = request(
            port,
            &post_json(
                "/v1/approvals",
                r#"{"call_id":"nope","decision":"approved"}"#,
            ),
        )
        .await;
        let st = assert_clean_in_set(&format!("approvals чужой call_id #{i}"), &out, &err);
        assert_eq!(st, 400, "\n{}", body_text(&out));
    }

    // Approvals без решения и с пустым call_id.
    let (out, err) = request(port, &post_json("/v1/approvals", r#"{"call_id":"x"}"#)).await;
    let st = assert_clean_in_set("approvals без decision", &out, &err);
    assert_eq!(st, 400, "\n{}", body_text(&out));
    let (out, err) = request(
        port,
        &post_json("/v1/approvals", r#"{"call_id":"","decision":"approved"}"#),
    )
    .await;
    let st = assert_clean_in_set("approvals пустой call_id", &out, &err);
    assert_eq!(st, 400, "\n{}", body_text(&out));
}

// ---------- патологические session_id ----------

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn pathological_session_ids_are_rejected_not_crashed() {
    let (port, _st) = serve().await;
    let ids: Vec<(&str, String)> = vec![
        ("10000 символов", "a".repeat(10000)),
        ("SQL-инъекция", "s' OR '1'='1 --".into()),
        ("путь наверх", "../../x".into()),
        ("null-байт", "s\u{0000}null".into()),
        ("эмодзи", "сессия-\u{1F420}".into()),
        ("только пробелы", "   ".into()),
    ];
    for (name, sid) in &ids {
        let body = serde_json::json!({ "session_id": sid }).to_string();
        let (out, err) = request(port, &post_json("/v1/sessions/transcript", &body)).await;
        let st = assert_clean_in_set(name, &out, &err);
        assert_eq!(st, 400, "{name}\n{}", body_text(&out));
        assert!(
            body_text(&out).contains("\"error\""),
            "{name}: нет error\n{}",
            body_text(&out)
        );
    }
}

// ---------- oversized: ответ обязан доходить до клиента ----------

// FINDING: ответ на перелимит отправляется до дочитки тела; сокет
// закрывается с ~2 МиБ недочитанных данных → Windows шлёт RST,
// клиент ловит сбой записи/чтения и 400 «тело больше 1 МБ» не видит.
// Факт прогона: err=read os error 10053, прочитано 0 байт.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn over_limit_body_response_reaches_client() {
    let (port, _st) = serve().await;
    let mut raw = format!(
        "POST /v1/turns HTTP/1.1\r\nhost: x\r\nauthorization: Bearer {TOKEN}\r\ncontent-length: 2097152\r\n\r\n"
    )
    .into_bytes();
    raw.extend_from_slice(&vec![b'A'; 2 * 1024 * 1024]);
    let (out, err) = request(port, &raw).await;
    println!("over-limit 2 МиБ: err={err:?} bytes={}", out.len());
    let st = assert_delivered_in_set("тело 2 МиБ при лимите 1 МиБ", &out, &err);
    assert_eq!(st, 400, "\n{}", body_text(&out));
    assert!(body_text(&out).contains("1 МБ"), "\n{}", body_text(&out));
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn giant_header_response_reaches_client() {
    // Прошёл честно: 400 «заголовки слишком большие» дошёл (174 байта,
    // чистый EOF) при ~36 КиБ недочитанных данных. Держим как регрессию
    // на границе с FINDING про потерю ответа при больших объёмах.
    let (port, _st) = serve().await;
    let mut raw = b"GET /health HTTP/1.1\r\nhost: x\r\nx-junk: ".to_vec();
    raw.extend_from_slice(&vec![b'A'; 100_000]);
    raw.extend_from_slice(b"\r\n\r\n");
    let (out, err) = request(port, &raw).await;
    println!("гигантский header: err={err:?} bytes={}", out.len());
    let st = assert_delivered_in_set("заголовок 100 КиБ", &out, &err);
    assert_eq!(st, 400, "\n{}", body_text(&out));
    assert!(body_text(&out).contains("слишком"), "\n{}", body_text(&out));
}

// FINDING: сервер читает CL байт, отвечает и закрывает сокет, не дочитав
// остаток. При 64 КиБ сверх CL ответ доходит (165 байт), при 1 МиБ —
// abort (os error 10053), 0 байт: ответ теряется. Факты прогона в println.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn extra_body_beyond_content_length_response_reaches_client() {
    let (port, _st) = serve().await;
    // Градация: 64 КиБ сверх CL — меньше порога буферов?
    let mut raw = format!(
        "POST /v1/turns HTTP/1.1\r\nhost: x\r\nauthorization: Bearer {TOKEN}\r\ncontent-length: 5\r\n\r\n"
    )
    .into_bytes();
    raw.extend_from_slice(&vec![b'A'; 64 * 1024]);
    let (out, err) = request(port, &raw).await;
    println!("лишнее тело 64 КиБ: err={err:?} bytes={}", out.len());
    let st = assert_delivered_in_set("64 КиБ сверх CL: 5", &out, &err);
    assert_eq!(st, 400, "\n{}", body_text(&out));

    let mut raw = format!(
        "POST /v1/turns HTTP/1.1\r\nhost: x\r\nauthorization: Bearer {TOKEN}\r\ncontent-length: 5\r\n\r\n"
    )
    .into_bytes();
    raw.extend_from_slice(&vec![b'A'; 1024 * 1024]);
    let (out, err) = request(port, &raw).await;
    println!("лишнее тело 1 МиБ: err={err:?} bytes={}", out.len());
    let st = assert_delivered_in_set("1 МиБ сверх CL: 5", &out, &err);
    assert_eq!(st, 400, "\n{}", body_text(&out));
}

// ---------- обрыв запроса без ответа ----------

// FINDING: обрезанный head + FIN — сервер молча закрывает соединение
// (n == 0 в цикле чтения head → None), без честного 400. У тела
// «не дочитано» есть, у head — нет. Факт прогона: Ok(Ok(0)), 0 байт.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn truncated_head_gets_a_response() {
    let (port, _st) = serve().await;
    let mut sock = TcpStream::connect(("127.0.0.1", port)).await.unwrap();
    sock.write_all(b"GET /health HTTP/1.1\r\nhost: x\r\n")
        .await
        .unwrap();
    sock.shutdown().await.unwrap();
    let mut out = Vec::new();
    let read = tokio::time::timeout(Duration::from_secs(5), sock.read_to_end(&mut out)).await;
    println!("обрезанный head: read={read:?} bytes={}", out.len());
    assert!(
        status(&out).is_some(),
        "сервер закрыл соединение без ответа (байт прочитано: {})",
        out.len()
    );
}

// FINDING: запрос с \n вместо \r\n никогда не находит терминатор
// \r\n\r\n; на FIN сервер уходит молча, без ответа. RFC-лояльные
// серверы либо терпят LF, либо отвечают 400. Факт: Ok(Ok(0)), 0 байт.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn bare_lf_request_gets_a_response() {
    let (port, _st) = serve().await;
    let mut sock = TcpStream::connect(("127.0.0.1", port)).await.unwrap();
    sock.write_all(b"GET /health HTTP/1.1\nhost: x\n\n")
        .await
        .unwrap();
    sock.shutdown().await.unwrap();
    let mut out = Vec::new();
    let read = tokio::time::timeout(Duration::from_secs(5), sock.read_to_end(&mut out)).await;
    println!("bare LF: read={read:?} bytes={}", out.len());
    assert!(
        status(&out).is_some(),
        "сервер закрыл соединение без ответа (байт прочитано: {})",
        out.len()
    );
}

// ---------- медленный клиент: дедлайна чтения нет ----------

// FINDING: незавершённый head (нет \r\n\r\n) с открытым соединением —
// серверная задача висит в read() бесконечно; дедлайна чтения нет.
// Факт прогона: молчание 10 с (Err(Elapsed)), в httpapi.rs таймаутов
// чтения нет вовсе.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn slow_head_gets_deadline() {
    let (port, _st) = serve().await;
    let mut sock = TcpStream::connect(("127.0.0.1", port)).await.unwrap();
    sock.write_all(b"GET /health HTTP/1.1\r\nhost: x\r\n")
        .await
        .unwrap();
    let mut chunk = [0u8; 4096];
    let got = tokio::time::timeout(Duration::from_secs(10), sock.read(&mut chunk)).await;
    println!("slow head: read за 10 с = {got:?}");
    // Пока висит незакрытое соединение, сервер обязан отвечать другим.
    let (h, herr) = request(port, &get_noauth("/health")).await;
    println!(
        "slow head: /health параллельно = {:?} err={herr:?}",
        status(&h)
    );
    assert!(
        matches!(&got, Ok(Ok(n)) if *n > 0),
        "сервер молчит 10 с на незавершённом head — задача висит без дедлайна"
    );
}

// FINDING: CL: 100000, прислано 5 байт, соединение открыто —
// задача висит в read() бесконечно. Факт: молчание 10 с.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn slow_body_gets_deadline() {
    let (port, _st) = serve().await;
    let mut sock = TcpStream::connect(("127.0.0.1", port)).await.unwrap();
    sock.write_all(&post_head(
        "/v1/turns",
        "content-length: 100000\r\n",
        b"AAAAA",
    ))
    .await
    .unwrap();
    let mut chunk = [0u8; 4096];
    let got = tokio::time::timeout(Duration::from_secs(10), sock.read(&mut chunk)).await;
    println!("slow body: read за 10 с = {got:?}");
    // Параллельное соединение — слушатель жив, висит только задача.
    let (h, herr) = request(port, &get_noauth("/health")).await;
    println!(
        "slow body: /health параллельно = {:?} err={herr:?}",
        status(&h)
    );
    assert!(
        matches!(&got, Ok(Ok(n)) if *n > 0),
        "сервер молчит 10 с на недотянутом теле — задача висит без дедлайна"
    );
}
