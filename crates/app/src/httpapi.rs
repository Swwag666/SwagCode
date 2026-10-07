/*! E-7: локальный HTTP API — loopback REST рядом с шиной.

Те же команды, что и IPC (`start_turn`, `respond_approval`,
`get_diagnostics`, `list_sessions`) плюс SSE-поток событий шины — для
скриптов и CI. Границы честные и узкие:

- Только `127.0.0.1`: сервер физически не слушает внешние интерфейсы.
- Bearer-токен: 32 случайных байта (BCryptGenRandom), хранится в prefs
  под DPAPI (механика B-7) — на диске только blob, plaintext живёт под
  учётной записью Windows. Сравнение токена — постоянное по времени.
- API по умолчанию ВЫКЛЮЧЕН: порт берётся из `SWAGCOD_HTTP_PORT` или
  prefs-ключа `http_api_port`; не задан — сервер не стартует.
- HTTP/1.1 без keep-alive (connection: close) и без chunked-encoding:
  Content-Length обязателен для POST. Это сознательная минимальность —
  hand-rolled парсер на tokio TcpListener, новых зависимостей нет.
- `/health` без токена (liveness), всё остальное — только с токеном.
 */

use std::sync::Arc;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::{TcpListener, TcpStream};

use crate::AppState;

/// Prefs-ключ DPAPI-blob'а токена HTTP API.
pub const TOKEN_PREF: &str = "http_api_token_blob";
/// Prefs-ключ порта (альтернатива env `SWAGCOD_HTTP_PORT`).
pub const PORT_PREF: &str = "http_api_port";

/// Пределы запроса: заголовки и тело. Превышение — честный 400, не OOM.
const HEAD_LIMIT: usize = 64 * 1024;
const BODY_LIMIT: usize = 1024 * 1024;

/// Порт API: env → prefs → None (выключено). 0 не считается портом.
pub fn configured_port(state: &AppState) -> Option<u16> {
    if let Ok(v) = std::env::var("SWAGCOD_HTTP_PORT") {
        let v = v.trim();
        if !v.is_empty() {
            return v.parse::<u16>().ok().filter(|p| *p > 0);
        }
    }
    state
        .store
        .lock()
        .ok()
        .and_then(|s| s.get_pref(PORT_PREF).ok().flatten())
        .and_then(|v| v.trim().parse::<u16>().ok())
        .filter(|p| *p > 0)
}

/// Токен API: расшифровать DPAPI-blob из prefs, иначе создать новый
/// (32 случайных байта) и сохранить зашифрованным. Вне Windows DPAPI нет
/// (модуль dpapi честно отказывает) — тогда и API недоступен.
pub fn api_token(state: &AppState) -> Option<String> {
    let store = state.store.lock().ok()?;
    let blob = store.get_pref(TOKEN_PREF).ok().flatten().unwrap_or_default();
    if !blob.trim().is_empty() {
        if let Ok(t) = crate::dpapi::unprotect_hex(blob.trim()) {
            return Some(t);
        }
        // Битый blob (не эта учётная запись?) — перевыпускаем токен.
    }
    let token = random_hex(32)?;
    let blob = crate::dpapi::protect_hex(&token).ok()?;
    let _ = store.set_pref(TOKEN_PREF, &blob);
    Some(token)
}

/// 32 байта от системного CSPRNG в hex. Только Windows (BCryptGenRandom);
/// на других платформах честный None — как у DPAPI.
#[cfg(windows)]
fn random_hex(n: usize) -> Option<String> {
    use windows_sys::Win32::Security::Cryptography::BCryptGenRandom;
    let mut buf = vec![0u8; n];
    // NULL-алгоритм + USE_SYSTEM_PREFERRED_RNG: системный провайдер без
    // явного открытия BCRYPT_ALG_HANDLE.
    let status =
        unsafe { BCryptGenRandom(std::ptr::null_mut(), buf.as_mut_ptr(), buf.len() as u32, 2) };
    if status < 0 {
        return None;
    }
    Some(crate::dpapi::to_hex(&buf))
}

#[cfg(not(windows))]
fn random_hex(_n: usize) -> Option<String> {
    None
}

/// Сравнение постоянное по времени: длина утекает и так (early return),
/// а вот побайтовое совпадение — нет.
pub fn ct_eq(a: &str, b: &str) -> bool {
    let (ab, bb) = (a.as_bytes(), b.as_bytes());
    if ab.len() != bb.len() {
        return false;
    }
    ab.iter().zip(bb).fold(0u8, |acc, (x, y)| acc | (x ^ y)) == 0
}

/// Найти первое вхождение `needle` в `hay`.
fn find_subslice(hay: &[u8], needle: &[u8]) -> Option<usize> {
    hay.windows(needle.len()).position(|w| w == needle)
}

/// Разобрать стартовую строку и заголовки (без тела). Чистая функция —
/// тестируется без сети. Возвращает (method, path, content_length,
/// authorization) или текст ошибки для 400.
pub fn parse_head(head: &str) -> Result<(String, String, Option<usize>, Option<String>), String> {
    let mut lines = head.lines();
    let mut req = lines.next().ok_or("пустой запрос")?.split_whitespace();
    let method = req.next().ok_or("нет метода")?.to_uppercase();
    let target = req.next().ok_or("нет пути")?.to_string();
    let path = target.split('?').next().unwrap_or("/").to_string();
    let mut content_length: Option<usize> = None;
    let mut auth: Option<String> = None;
    for line in lines {
        let Some((k, v)) = line.split_once(':') else { continue };
        match k.trim().to_ascii_lowercase().as_str() {
            "content-length" => {
                if content_length.is_some() {
                    // Двойной Content-Length — признак smuggling: отказ.
                    return Err("двойной Content-Length".into());
                }
                content_length =
                    Some(v.trim().parse::<usize>().map_err(|_| "Content-Length не число")?);
            }
            "authorization" => auth = Some(v.trim().to_string()),
            _ => {}
        }
    }
    Ok((method, path, content_length, auth))
}

/// Ответ одной строкой: HTTP/1.1, JSON, connection: close.
fn response(status: u16, reason: &str, body: &str) -> Vec<u8> {
    format!(
        "HTTP/1.1 {status} {reason}\r\ncontent-type: application/json; charset=utf-8\r\ncontent-length: {}\r\nconnection: close\r\n\r\n{body}",
        body.len()
    )
    .into_bytes()
}

fn json_error(msg: &str) -> String {
    serde_json::json!({ "error": msg }).to_string()
}

/// Проверка Bearer-токена.
fn authorized(auth: Option<&str>, token: &str) -> bool {
    let Some(v) = auth else { return false };
    let Some((scheme, tok)) = v.split_once(' ') else { return false };
    scheme.eq_ignore_ascii_case("bearer") && ct_eq(tok.trim(), token)
}

/// Запустить сервер на `127.0.0.1:port`. Ошибка bind — честная строка
/// (порт занят и т.п.), вызывающий её логирует.
pub async fn serve(state: Arc<AppState>, port: u16) -> Result<(), String> {
    // Только loopback: внешний интерфейс не слушается НИКОГДА.
    let addr = format!("127.0.0.1:{port}");
    let listener = TcpListener::bind(&addr)
        .await
        .map_err(|e| format!("http api: bind {addr}: {e}"))?;
    let token = api_token(&state).ok_or("http api: токен недоступен (DPAPI только Windows)")?;
    eprintln!("http api: слушаю {addr}");
    serve_on(listener, state, token).await;
    Ok(())
}

/// Accept-цикл. Отдельная точка входа — тесты поднимают сервер на
/// эфемерном порту без env/DPAPI.
pub async fn serve_on(listener: TcpListener, state: Arc<AppState>, token: String) {
    loop {
        let Ok((sock, _peer)) = listener.accept().await else { break };
        let st = state.clone();
        let tk = token.clone();
        tokio::spawn(async move {
            let mut sock = sock;
            if let Some(resp) = serve_conn(&mut sock, &st, &tk).await {
                let _ = sock.write_all(&resp).await;
            }
            let _ = sock.shutdown().await;
        });
    }
}

/// Одно соединение: прочитать запрос, смаршрутизировать, ответить.
/// None — соединение закрыто клиентом или SSE уже отдал всё сам.
async fn serve_conn(sock: &mut TcpStream, state: &Arc<AppState>, token: &str) -> Option<Vec<u8>> {
    // 1. Заголовки — до \r\n\r\n, с пределом.
    let mut buf: Vec<u8> = Vec::new();
    let mut tmp = [0u8; 4096];
    let head_end = loop {
        if let Some(p) = find_subslice(&buf, b"\r\n\r\n") {
            break p;
        }
        if buf.len() > HEAD_LIMIT {
            return Some(response(400, "Bad Request", &json_error("заголовки слишком большие")));
        }
        let n = sock.read(&mut tmp).await.ok()?;
        if n == 0 {
            return None; // клиент ушёл без запроса
        }
        buf.extend_from_slice(&tmp[..n]);
    };
    let head = String::from_utf8_lossy(&buf[..head_end]).to_string();
    let (method, path, content_length, auth) = match parse_head(&head) {
        Ok(v) => v,
        Err(e) => return Some(response(400, "Bad Request", &json_error(&e))),
    };

    // 2. Тело по Content-Length (chunked не поддерживаем сознательно).
    let mut body = buf[head_end + 4..].to_vec();
    if let Some(cl) = content_length {
        if cl > BODY_LIMIT {
            return Some(response(400, "Bad Request", &json_error("тело больше 1 МБ")));
        }
        while body.len() < cl {
            let n = sock.read(&mut tmp).await.ok()?;
            if n == 0 {
                return Some(response(400, "Bad Request", &json_error("тело не дочитано")));
            }
            body.extend_from_slice(&tmp[..n]);
        }
        body.truncate(cl);
    }

    // 3. Маршруты. /health без токена — liveness для скриптов.
    if path == "/health" && method == "GET" {
        return Some(response(
            200,
            "OK",
            &serde_json::json!({ "ok": true, "service": "swagcod-http-api" }).to_string(),
        ));
    }
    if !authorized(auth.as_deref(), token) {
        return Some(response(401, "Unauthorized", &json_error("unauthorized")));
    }
    match (method.as_str(), path.as_str()) {
        ("GET", "/v1/diagnostics") => Some(response(
            200,
            "OK",
            &crate::get_diagnostics_core(state).to_string(),
        )),
        ("GET", "/v1/sessions") => Some(response(
            200,
            "OK",
            &serde_json::to_string(&crate::list_sessions_core(state).await)
                .unwrap_or_else(|_| "[]".into()),
        )),
        ("POST", "/v1/turns") => {
            let v: serde_json::Value = match serde_json::from_slice(&body) {
                Ok(v) => v,
                Err(e) => {
                    return Some(response(400, "Bad Request", &json_error(&format!("json: {e}"))))
                }
            };
            let session_id = v.get("session_id").and_then(|x| x.as_str()).unwrap_or("");
            let message = v.get("message").and_then(|x| x.as_str()).unwrap_or("");
            let model = v.get("model").and_then(|x| x.as_str()).map(String::from);
            let temperature = v.get("temperature").and_then(|x| x.as_f64());
            if session_id.trim().is_empty() {
                return Some(response(400, "Bad Request", &json_error("нужен session_id")));
            }
            match crate::start_turn_core(
                state.clone(),
                session_id.to_string(),
                message.to_string(),
                model,
                temperature,
                None,
            )
            .await
            {
                Ok(turn_id) => Some(response(
                    200,
                    "OK",
                    &serde_json::json!({ "turn_id": turn_id }).to_string(),
                )),
                Err(e) => Some(response(400, "Bad Request", &json_error(&e))),
            }
        }
        ("POST", "/v1/approvals") => {
            let v: serde_json::Value = match serde_json::from_slice(&body) {
                Ok(v) => v,
                Err(e) => {
                    return Some(response(400, "Bad Request", &json_error(&format!("json: {e}"))))
                }
            };
            let call_id = v.get("call_id").and_then(|x| x.as_str()).unwrap_or("");
            let decision = match v.get("decision").and_then(|x| x.as_str()).unwrap_or("") {
                d if d.eq_ignore_ascii_case("approved") => {
                    swagcod_core::turn::ApprovalDecision::Approved
                }
                d if d.eq_ignore_ascii_case("denied") => {
                    swagcod_core::turn::ApprovalDecision::Denied
                }
                _ => {
                    return Some(response(
                        400,
                        "Bad Request",
                        &json_error("decision должен быть approved или denied"),
                    ))
                }
            };
            if call_id.trim().is_empty() {
                return Some(response(400, "Bad Request", &json_error("нужен call_id")));
            }
            match crate::respond_approval_core(state, call_id.to_string(), decision).await {
                Ok(()) => Some(response(200, "OK", "{}")),
                Err(e) => Some(response(400, "Bad Request", &json_error(&e))),
            }
        }
        ("GET", "/v1/events") => {
            sse_loop(sock, state).await;
            None
        }
        _ => Some(response(404, "Not Found", &json_error("не найдено"))),
    }
}

/// SSE-поток событий шины: subscribe → `data: {json}` на каждое событие.
/// Лаг broadcast — честный комментарий `: lagged N`, не разрыв.
async fn sse_loop(sock: &mut TcpStream, state: &Arc<AppState>) {
    use tokio::sync::broadcast::error::RecvError;
    let mut rx = state.bus.subscribe();
    let head = "HTTP/1.1 200 OK\r\ncontent-type: text/event-stream\r\ncache-control: no-store\r\nconnection: close\r\n\r\n";
    if sock.write_all(head.as_bytes()).await.is_err() {
        return;
    }
    let _ = sock.flush().await;
    loop {
        let frame = match rx.recv().await {
            Ok(ev) => format!(
                "data: {}\n\n",
                serde_json::to_string(&ev).unwrap_or_default()
            ),
            Err(RecvError::Lagged(n)) => format!(": lagged {n}\n\n"),
            Err(RecvError::Closed) => break,
        };
        if sock.write_all(frame.as_bytes()).await.is_err() {
            break; // клиент отключился
        }
        if sock.flush().await.is_err() {
            break;
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parse_head_get_post_and_errors() {
        let (m, p, cl, auth) =
            parse_head("GET /v1/sessions?x=1 HTTP/1.1\r\nHost: a\r\nAuthorization: Bearer t").unwrap();
        assert_eq!(m, "GET");
        assert_eq!(p, "/v1/sessions");
        assert_eq!(cl, None);
        assert_eq!(auth.as_deref(), Some("Bearer t"));

        let (m, _, cl, _) =
            parse_head("post /v1/turns HTTP/1.1\r\ncontent-length: 17\r\n").unwrap();
        assert_eq!(m, "POST");
        assert_eq!(cl, Some(17));

        assert!(parse_head("").is_err());
        assert!(parse_head("GET").is_err());
        // Двойной Content-Length — smuggling-признак, отказ.
        assert!(parse_head("POST /x HTTP/1.1\r\nContent-Length: 1\r\ncontent-length: 2\r\n").is_err());
        assert!(parse_head("POST /x HTTP/1.1\r\nContent-Length: abc\r\n").is_err());
    }

    #[test]
    fn ct_eq_is_honest() {
        assert!(ct_eq("abc", "abc"));
        assert!(!ct_eq("abc", "abd"));
        assert!(!ct_eq("abc", "ab"));
        assert!(ct_eq("", ""));
    }

    #[test]
    fn authorized_requires_bearer_and_token() {
        assert!(authorized(Some("Bearer tok"), "tok"));
        assert!(authorized(Some("bearer tok"), "tok"));
        assert!(!authorized(Some("Bearer tok"), "other"));
        assert!(!authorized(Some("Basic tok"), "tok"));
        assert!(!authorized(None, "tok"));
        assert!(!authorized(Some("Bearer"), "tok"));
    }

    #[test]
    fn configured_port_pref_and_disabled_by_default() {
        let state = AppState {
            store: std::sync::Mutex::new(swagcod_core::store::open_memory()),
            ..Default::default()
        };
        std::env::remove_var("SWAGCOD_HTTP_PORT");
        assert_eq!(configured_port(&state), None, "по умолчанию API выключен");
        {
            let g = state.store.lock().unwrap();
            g.set_pref(PORT_PREF, "47479").unwrap();
        }
        assert_eq!(configured_port(&state), Some(47479));
        {
            let g = state.store.lock().unwrap();
            g.set_pref(PORT_PREF, "0").unwrap();
        }
        assert_eq!(configured_port(&state), None, "0 — не порт");
        std::env::set_var("SWAGCOD_HTTP_PORT", "1234");
        assert_eq!(configured_port(&state), Some(1234), "env сильнее prefs");
        std::env::remove_var("SWAGCOD_HTTP_PORT");
    }

    #[cfg(windows)]
    #[test]
    fn token_is_random_persisted_and_stable() {
        let state = AppState {
            store: std::sync::Mutex::new(swagcod_core::store::open_memory()),
            ..Default::default()
        };
        let t1 = api_token(&state).expect("DPAPI на Windows доступен");
        assert_eq!(t1.len(), 64, "32 байта в hex");
        assert!(t1.chars().all(|c| c.is_ascii_hexdigit()));
        // Повторный вызов — тот же токен (из blob), а не новый.
        assert_eq!(api_token(&state).as_deref(), Some(t1.as_str()));
        // В базе лежит blob, не plaintext.
        let blob = {
            let g = state.store.lock().unwrap();
            g.get_pref(TOKEN_PREF).unwrap().unwrap_or_default()
        };
        assert!(!blob.is_empty());
        assert!(!blob.contains(&t1));
        // Два AppState с разными базами — разные токены (случайность жива).
        let other = AppState {
            store: std::sync::Mutex::new(swagcod_core::store::open_memory()),
            ..Default::default()
        };
        assert_ne!(api_token(&other).as_deref(), Some(t1.as_str()));
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn http_api_endpoints_auth_and_stream() {
        let state = Arc::new(AppState {
            store: std::sync::Mutex::new(swagcod_core::store::open_memory()),
            ..Default::default()
        });
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let port = listener.local_addr().unwrap().port();
        let st = state.clone();
        tokio::spawn(async move { serve_on(listener, st, "test-token".into()).await });

        async fn req(port: u16, raw: &str) -> String {
            let mut sock = TcpStream::connect(("127.0.0.1", port)).await.unwrap();
            sock.write_all(raw.as_bytes()).await.unwrap();
            let mut out = Vec::new();
            let _ = tokio::time::timeout(std::time::Duration::from_secs(5), sock.read_to_end(&mut out)).await;
            String::from_utf8_lossy(&out).to_string()
        }
        let auth = "authorization: Bearer test-token\r\n";

        // /health без токена — liveness.
        let r = req(port, "GET /health HTTP/1.1\r\nhost: x\r\n\r\n").await;
        assert!(r.starts_with("HTTP/1.1 200"), "{r}");
        assert!(r.contains("\"ok\":true"), "{r}");

        // Диагностика: 401 без токена и с чужим, 200 с нашим.
        let r = req(port, "GET /v1/diagnostics HTTP/1.1\r\nhost: x\r\n\r\n").await;
        assert!(r.starts_with("HTTP/1.1 401"), "{r}");
        let r = req(
            port,
            "GET /v1/diagnostics HTTP/1.1\r\nhost: x\r\nauthorization: Bearer wrong\r\n\r\n",
        )
        .await;
        assert!(r.starts_with("HTTP/1.1 401"), "{r}");
        let r = req(
            port,
            &format!("GET /v1/diagnostics HTTP/1.1\r\nhost: x\r\n{auth}\r\n"),
        )
        .await;
        assert!(r.starts_with("HTTP/1.1 200"), "{r}");
        assert!(r.contains("\"version\""), "{r}");

        // Сессии: пустой список, но 200.
        let r = req(port, &format!("GET /v1/sessions HTTP/1.1\r\nhost: x\r\n{auth}\r\n")).await;
        assert!(r.starts_with("HTTP/1.1 200"), "{r}");
        assert!(r.contains("[]"), "{r}");

        // Ход с пустым сообщением — честный 400 из ядра start_turn.
        let body = r#"{"session_id":"s-1","message":"  "}"#;
        let r = req(
            port,
            &format!(
                "POST /v1/turns HTTP/1.1\r\nhost: x\r\n{auth}content-length: {}\r\n\r\n{body}",
                body.len()
            ),
        )
        .await;
        assert!(r.starts_with("HTTP/1.1 400"), "{r}");
        assert!(r.contains("пустым"), "{r}");

        // Подтверждение несуществующего вызова — честный 400.
        let body = r#"{"call_id":"nope","decision":"approved"}"#;
        let r = req(
            port,
            &format!(
                "POST /v1/approvals HTTP/1.1\r\nhost: x\r\n{auth}content-length: {}\r\n\r\n{body}",
                body.len()
            ),
        )
        .await;
        assert!(r.starts_with("HTTP/1.1 400"), "{r}");
        assert!(r.contains("нет ожидающего"), "{r}");

        // Неверный decision — 400 до похода в approvals.
        let body = r#"{"call_id":"x","decision":"maybe"}"#;
        let r = req(
            port,
            &format!(
                "POST /v1/approvals HTTP/1.1\r\nhost: x\r\n{auth}content-length: {}\r\n\r\n{body}",
                body.len()
            ),
        )
        .await;
        assert!(r.starts_with("HTTP/1.1 400"), "{r}");

        // Неизвестный путь — 404 (с токеном!).
        let r = req(port, &format!("GET /nope HTTP/1.1\r\nhost: x\r\n{auth}\r\n")).await;
        assert!(r.starts_with("HTTP/1.1 404"), "{r}");

        // SSE: событие шины доезжает до подписчика data-кадром.
        let mut sse = TcpStream::connect(("127.0.0.1", port)).await.unwrap();
        sse.write_all(format!("GET /v1/events HTTP/1.1\r\nhost: x\r\n{auth}\r\n").as_bytes())
            .await
            .unwrap();
        // Дать серверу дойти до subscribe.
        tokio::time::sleep(std::time::Duration::from_millis(150)).await;
        state.bus.publish(swagcod_core::bus::EventKind::Status {
            message: "проба SSE".into(),
        });
        let mut chunk = [0u8; 4096];
        let got = tokio::time::timeout(std::time::Duration::from_secs(5), async {
            let mut acc = Vec::new();
            loop {
                let n = sse.read(&mut chunk).await.unwrap();
                if n == 0 {
                    break;
                }
                acc.extend_from_slice(&chunk[..n]);
                // Ждём именно data-кадр: заголовки сами по себе тоже
                // заканчиваются пустой строкой.
                if find_subslice(&acc, b"data: ").is_some() {
                    break;
                }
            }
            String::from_utf8_lossy(&acc).to_string()
        })
        .await
        .expect("SSE-кадр пришёл");
        assert!(got.contains("text/event-stream"), "{got}");
        assert!(got.contains("data: "), "{got}");
        assert!(got.contains("проба SSE"), "{got}");
    }
}
