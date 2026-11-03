/*! probe_flow_sessions: зонд живых пользовательских флоу через loopback HTTP API.

Слой — те же ядра, что у IPC-команд десктопа (`create_session_core`,
`start_turn_core`, `stop_turn_core`, `respond_approval_core`,
`transcript_events`, `turn_revert_core`, `turn_checkpoints_core`), подняты
через `serve_on` как в e2e_smoke. Отличие от e2e_smoke: не один счастливый
путь, а то, что делает живой пользователь — цепочки ходов, параллельные
сессии, откат, стоп по кнопке, двойной клик по «стоп», ход сразу за ходом
без паузы. Ввод везде валидный: мусорные тела — территория probe_http.

Темп задаёт echo-провайдер без сети (env-флаг): короткие сообщения —
мгновенные ходы для цепочек, длинное «лог»-сообщение (~500 КБ ASCII) —
стрим на десятки тысяч дельт, внутри которого живут сценарии «стоп
посреди стрима» и «второй ход, пока первый отвечает».

Контракт находки: паника, зависание (один await дольше 30 с), потеря или
дублирование истории, ход не завершается, параллельный второй ход в одной
сессии, откат ломает историю, cancel не останавливает. Кейс-находка
помечается `#[ignore = "FINDING: ..."]`: репро остаётся в дереве,
`cargo test` зелёный.

Честная граница зонда: флоу подтверждений тулзов (confirm → продолжение,
deny → честный финал) и ветвление сессии на этом слое недостижимы —
echo-провайдер не выдаёт tool-call вообще (стрим без вызовов инструментов,
см. crates/provider/src/echo.rs), а REST-поверхность не имеет маршрутов
`/v1/sessions/fork` и «поставить подтверждение на паузу»; IPC-команды
`session_fork`/`respond_approval` требуют живого Tauri-окна. Проверены
доступные края этих контрактов (честные 400 без висящего диалога). */

use std::sync::Arc;
use std::time::Duration;

use tokio::io::{AsyncBufReadExt, AsyncReadExt, AsyncWriteExt, BufReader};
use tokio::net::{TcpListener, TcpStream};
use tokio::sync::mpsc::{unbounded_channel, UnboundedReceiver};

use swagcod_app_lib::httpapi::serve_on;
use swagcod_app_lib::AppState;

const TOKEN: &str = "flow-token";

/// Дедлайн одного await в зонде. По контракту находки зависание — это
/// ожидание дольше 30 секунд; всё, что ждётся дольше, роняет тест.
const DEADLINE: Duration = Duration::from_secs(30);

// ---------- общий каркас (как e2e_smoke / probe_http, без своих идей) ----------

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

/// Один HTTP-запрос-ответ. В отличие от e2e_smoke, отсутствие EOF за дедлайн —
/// не тихий «прочитали сколько успели», а падение: зависший ответ и есть
/// находка по контракту.
async fn request(port: u16, raw: &str) -> String {
    let mut sock = TcpStream::connect(("127.0.0.1", port))
        .await
        .expect("соединение с loopback API");
    sock.write_all(raw.as_bytes())
        .await
        .expect("запрос отправлен");
    let mut out = Vec::new();
    match tokio::time::timeout(DEADLINE, sock.read_to_end(&mut out)).await {
        Err(_) => panic!("зависание: ответ не закрылся за {} с", DEADLINE.as_secs()),
        Ok(Err(e)) => panic!("сбой чтения ответа: {e}"),
        Ok(Ok(_)) => String::from_utf8_lossy(&out).to_string(),
    }
}

fn auth_head() -> String {
    format!("authorization: Bearer {TOKEN}\r\n")
}

async fn post(port: u16, path: &str, body: &str) -> String {
    let raw = format!(
        "POST {path} HTTP/1.1\r\nhost: x\r\n{}content-length: {}\r\nconnection: close\r\n\r\n{body}",
        auth_head(),
        body.len()
    );
    request(port, &raw).await
}

async fn get(port: u16, path: &str) -> String {
    let raw = format!("GET {path} HTTP/1.1\r\nhost: x\r\n{}\r\n", auth_head());
    request(port, &raw).await
}

fn status_of(resp: &str) -> u16 {
    resp.lines()
        .next()
        .unwrap_or("")
        .split(' ')
        .nth(1)
        .unwrap_or("")
        .parse()
        .unwrap_or(0)
}

fn body_of(resp: &str) -> serde_json::Value {
    let body = resp.split("\r\n\r\n").nth(1).unwrap_or("");
    serde_json::from_str(body).unwrap_or_else(|e| panic!("тело не JSON: {e}: {body}"))
}

/// Значение поля «kind» события (SSE или транскрипта).
fn kind_of(ev: &serde_json::Value) -> &str {
    ev.get("kind")
        .and_then(|k| k.get("kind"))
        .and_then(|v| v.as_str())
        .unwrap_or("")
}

/// Текстовое поле события (user/content/reasoning).
fn text_of(ev: &serde_json::Value) -> &str {
    ev.pointer("/kind/data/text")
        .and_then(|v| v.as_str())
        .unwrap_or("")
}

/// Подписаться на SSE до старта ходов: собрать весь поток сессии.
async fn open_sse(port: u16) -> UnboundedReceiver<serde_json::Value> {
    let sock = TcpStream::connect(("127.0.0.1", port))
        .await
        .expect("SSE-соединение");
    let (tx, rx) = unbounded_channel();
    let (rd, mut wr) = sock.into_split();
    wr.write_all(format!("GET /v1/events HTTP/1.1\r\nhost: x\r\n{}\r\n", auth_head()).as_bytes())
        .await
        .expect("SSE-запрос");
    tokio::spawn(async move {
        let mut lines = BufReader::new(rd).lines();
        while let Ok(Some(line)) = lines.next_line().await {
            let Some(data) = line.strip_prefix("data: ") else {
                continue;
            };
            match serde_json::from_str::<serde_json::Value>(data) {
                Ok(ev) => {
                    if tx.send(ev).is_err() {
                        break;
                    }
                }
                Err(_) => continue,
            }
        }
    });
    rx
}

/// Рабочая директория сессии. Уникальна на вызов: тесты бинарника идут
/// параллельно, а откат проверяет содержимое файлов — общая папка на два
/// теста превращала бы проверку отката в гонку.
fn workdir(tag: &str) -> std::path::PathBuf {
    use std::sync::atomic::{AtomicU64, Ordering};
    static SEQ: AtomicU64 = AtomicU64::new(0);
    let n = SEQ.fetch_add(1, Ordering::SeqCst);
    let dir = std::env::temp_dir().join(format!("swagcod-flow-{tag}-{}-{n}", std::process::id()));
    std::fs::create_dir_all(&dir).expect("временная директория создаётся");
    dir
}

/// «Лог» пользователя: ~500 КБ ASCII. Echo режет ответ на дельты по 24
/// символа — это ~21 тысяча событий стрима, ход живёт дольше коротких
/// сообщений. Хвост не пробельный: журнал хранит реплику юзера с
/// обрезанными краями, и хвостовая инструкция исказила бы сравнение.
fn slow_message() -> String {
    format!("{}end", "data ".repeat(100_000))
}

/// Создать сессию в каталоге. 200 обязателен: это шаг каждого флоу.
async fn create_session(port: u16, cwd: &std::path::Path, model: Option<&str>) -> String {
    let mut body = serde_json::json!({ "cwd": cwd.to_string_lossy() });
    if let Some(m) = model {
        body["model"] = serde_json::json!(m);
    }
    let resp = post(port, "/v1/sessions", &body.to_string()).await;
    assert_eq!(status_of(&resp), 200, "сессия не создалась: {resp}");
    body_of(&resp)["id"]
        .as_str()
        .unwrap_or_else(|| panic!("id сессии в ответе: {resp}"))
        .to_string()
}

/// Отправить ход и убедиться, что он принят. Возвращает turn_id.
async fn start_turn(port: u16, sid: &str, message: &str) -> String {
    let body = serde_json::json!({ "session_id": sid, "message": message }).to_string();
    let resp = post(port, "/v1/turns", &body).await;
    assert_eq!(status_of(&resp), 200, "ход не принят: {resp}");
    body_of(&resp)["turn_id"]
        .as_str()
        .expect("turn_id в ответе")
        .to_string()
}

/// Отправить ход без проверки кода: сценарии «ожидаем отказ».
async fn try_start_turn(port: u16, sid: &str, message: &str) -> String {
    let body = serde_json::json!({ "session_id": sid, "message": message }).to_string();
    post(port, "/v1/turns", &body).await
}

/// Транскрипт сессии: массив событий в порядке журнала.
async fn transcript(port: u16, sid: &str) -> Vec<serde_json::Value> {
    let body = serde_json::json!({ "session_id": sid }).to_string();
    let resp = post(port, "/v1/sessions/transcript", &body).await;
    assert_eq!(status_of(&resp), 200, "транскрипт не читается: {resp}");
    body_of(&resp)
        .as_array()
        .cloned()
        .expect("транскрипт — массив")
}

/// Список сессий (GET /v1/sessions).
async fn sessions_list(port: u16) -> Vec<serde_json::Value> {
    let resp = get(port, "/v1/sessions").await;
    assert_eq!(status_of(&resp), 200, "список сессий не читается: {resp}");
    body_of(&resp).as_array().cloned().expect("список — массив")
}

/// Ходы сессии, которые можно откатить.
async fn checkpoints(port: u16, sid: &str) -> Vec<serde_json::Value> {
    let body = serde_json::json!({ "session_id": sid }).to_string();
    let resp = post(port, "/v1/checkpoints", &body).await;
    assert_eq!(status_of(&resp), 200, "чекпоинты не читаются: {resp}");
    body_of(&resp)
        .as_array()
        .cloned()
        .expect("чекпоинты — массив")
}

/// Откатить ход к его снимку. Возвращает сырой ответ.
async fn revert(port: u16, sid: &str, turn_id: &str) -> String {
    let body = serde_json::json!({ "session_id": sid, "turn_id": turn_id }).to_string();
    post(port, "/v1/turns/revert", &body).await
}

/// Остановить живой ход. Возвращает сырой ответ.
async fn stop_turn(port: u16, sid: &str) -> String {
    let body = serde_json::json!({ "session_id": sid }).to_string();
    post(port, "/v1/turns/stop", &body).await
}

/// Дождаться, пока каждая сессия из `want` не получит указанное число СВЕЖИХ
/// финалов в этом вызове (счётчик локален: предыдущие коллекторы уже съели
/// свои события). Возвращает все события, увиденные до полного набора.
/// Порядок в шине проверяется попутно: seq обязан расти.
async fn await_turn_ends(
    rx: &mut UnboundedReceiver<serde_json::Value>,
    want: &[(&str, usize)],
    ctx: &str,
) -> Vec<serde_json::Value> {
    let deadline = tokio::time::Instant::now() + DEADLINE;
    let mut got: Vec<usize> = vec![0; want.len()];
    let mut out = Vec::new();
    let mut last_seq = 0u64;
    loop {
        let left = deadline.saturating_duration_since(tokio::time::Instant::now());
        assert!(
            !left.is_zero(),
            "ходы не завершились за 30 с ({ctx}): ждали {want:?}"
        );
        let ev = match tokio::time::timeout(left, rx.recv()).await {
            Err(_) => {
                let kinds: Vec<&str> = out.iter().map(kind_of).collect();
                panic!("ходы не завершились за 30 с ({ctx}): ждали {want:?}, увидели {} событий: {kinds:?}", out.len());
            }
            Ok(None) => panic!("SSE-канал закрыт ({ctx})"),
            Ok(Some(ev)) => ev,
        };
        let seq = ev.get("seq").and_then(|v| v.as_u64()).unwrap_or(0);
        if seq > 0 {
            assert!(
                seq > last_seq,
                "seq не монотонен ({ctx}): {seq} после {last_seq}"
            );
            last_seq = seq;
        }
        let ev_session = ev
            .pointer("/kind/data/session")
            .and_then(|v| v.as_str())
            .unwrap_or("");
        if kind_of(&ev) == "turn_ended" {
            if let Some(i) = want.iter().position(|(s, _)| *s == ev_session) {
                got[i] += 1;
                if got.iter().zip(want.iter()).all(|(g, (_, n))| g >= n) {
                    out.push(ev);
                    return out;
                }
            }
        }
        out.push(ev);
    }
}

/// Дождаться первого куска контента хода: доказательство, что стрим идёт.
async fn wait_first_content(rx: &mut UnboundedReceiver<serde_json::Value>, turn_id: &str) {
    let deadline = tokio::time::Instant::now() + DEADLINE;
    loop {
        let left = deadline.saturating_duration_since(tokio::time::Instant::now());
        assert!(!left.is_zero(), "стрим не начался за 30 с (ход {turn_id})");
        let ev = match tokio::time::timeout(left, rx.recv()).await {
            Err(_) => panic!("стрим не начался за 30 с (ход {turn_id})"),
            Ok(None) => panic!("SSE-канал закрыт до первого куска (ход {turn_id})"),
            Ok(Some(ev)) => ev,
        };
        let turn = ev
            .pointer("/kind/data/turn")
            .and_then(|v| v.as_str())
            .unwrap_or("");
        if kind_of(&ev) == "content" && turn == turn_id {
            return;
        }
    }
}

/// Собрать хвост событий без блокировки: проверка «после финала ничего
/// второго не приехало».
async fn drain_sse(
    rx: &mut UnboundedReceiver<serde_json::Value>,
    ms: u64,
) -> Vec<serde_json::Value> {
    let deadline = tokio::time::Instant::now() + Duration::from_millis(ms);
    let mut out = Vec::new();
    loop {
        let left = deadline.saturating_duration_since(tokio::time::Instant::now());
        if left.is_zero() {
            return out;
        }
        match tokio::time::timeout(left, rx.recv()).await {
            Ok(Some(ev)) => out.push(ev),
            Ok(None) => return out,
            Err(_) => return out,
        }
    }
}

fn user_texts(tr: &[serde_json::Value]) -> Vec<&str> {
    tr.iter()
        .filter(|e| kind_of(e) == "user")
        .map(text_of)
        .collect()
}

fn content_texts(tr: &[serde_json::Value]) -> Vec<&str> {
    tr.iter()
        .filter(|e| kind_of(e) == "content")
        .map(text_of)
        .collect()
}

fn turn_ids(tr: &[serde_json::Value]) -> Vec<&str> {
    tr.iter()
        .filter(|e| kind_of(e) == "turn_started")
        .filter_map(|e| e.pointer("/kind/data/turn").and_then(|v| v.as_str()))
        .collect()
}

/// Предыдущий транскрипт обязан быть префиксом нового: пользовательские
/// операции добавляют события в конец, а не переписывают и не дублируют.
fn assert_prefix(prev: &[serde_json::Value], next: &[serde_json::Value], ctx: &str) {
    assert!(
        prev.len() <= next.len(),
        "{ctx}: история скукожилась ({prev} → {next})",
        prev = prev.len(),
        next = next.len()
    );
    for (i, (a, b)) in prev.iter().zip(next.iter()).enumerate() {
        assert_eq!(a, b, "{ctx}: событие #{i} изменилось после новой операции");
    }
}

/// Обычный echo-ход — блок из пяти событий: старт, реплика юзера,
/// рассуждение, ответ, финал. Проверяется на каждом ходе цепочки.
fn assert_turn_blocks(tr: &[serde_json::Value], ctx: &str) {
    assert_eq!(
        tr.len() % 5,
        0,
        "{ctx}: длина транскрипта не кратна блоку хода: {}",
        tr.len()
    );
    for (i, block) in tr.chunks(5).enumerate() {
        let kinds: Vec<&str> = block.iter().map(kind_of).collect();
        assert_eq!(
            kinds,
            vec!["turn_started", "user", "reasoning", "content", "turn_ended"],
            "{ctx}: блок хода #{i} не полный: {kinds:?}"
        );
    }
}

// ---------- сцена 1: цепочка ходов, откат, стабильность истории ----------

/// Живой флоу: создать сессию → ход → история → второй ход → правки файлов →
/// откат последнего хода → история та же, файлы вернулись. Откаты ходов —
/// файловая операция (F-5/rev64), история обязана переживать их нетронутой,
/// события — не дублироваться.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn flow_two_turns_then_revert_keeps_history() {
    let (port, _st) = serve().await;
    let mut sse = open_sse(port).await;
    let cwd = workdir("revert");
    let note = cwd.join("note.txt");
    std::fs::write(&note, b"state-1").unwrap();
    let sid = create_session(port, &cwd, None).await;

    // До ходов откатывать нечего: пустой список — не ошибка.
    assert!(
        checkpoints(port, &sid).await.is_empty(),
        "чекпоинтов у новой сессии нет"
    );

    // Ход один.
    let t1 = start_turn(port, &sid, "ход один").await;
    await_turn_ends(&mut sse, &[(&sid, 1)], "ход один").await;
    let snap1 = transcript(port, &sid).await;
    assert_turn_blocks(&snap1, "после первого хода");
    assert_eq!(user_texts(&snap1), vec!["ход один"]);
    assert_eq!(content_texts(&snap1), vec!["эхо: ход один"]);

    // Перечитали историю без операций между: та же, без дублей.
    let snap1_again = transcript(port, &sid).await;
    assert_eq!(snap1, snap1_again, "повторное чтение изменило историю");

    // Юзер правит файл между ходами.
    std::fs::write(&note, b"state-2").unwrap();
    let t2 = start_turn(port, &sid, "ход два").await;
    await_turn_ends(&mut sse, &[(&sid, 1)], "ход два").await;
    let snap2 = transcript(port, &sid).await;
    assert_prefix(&snap1, &snap2, "после второго хода");
    assert_turn_blocks(&snap2, "после второго хода");
    assert_eq!(user_texts(&snap2), vec!["ход один", "ход два"]);
    assert_eq!(content_texts(&snap2), vec!["эхо: ход один", "эхо: ход два"]);

    // Снимки есть у обоих ходов: кнопка «Откатить» честная.
    let cps = checkpoints(port, &sid).await;
    let mut cp_turns: Vec<&str> = cps.iter().filter_map(|c| c["turn_id"].as_str()).collect();
    cp_turns.sort();
    let mut expected = vec![t1.as_str(), t2.as_str()];
    expected.sort_unstable();
    assert_eq!(cp_turns, expected, "чекпоинты у обоих ходов");
    assert!(
        cps.iter()
            .all(|c| c["sha"].as_str().is_some_and(|s| !s.is_empty())),
        "sha у каждого снимка"
    );

    // Снимок второго хода сделан ДО хода: файл тогда был state-2.
    std::fs::write(&note, b"state-3").unwrap();
    let resp = revert(port, &sid, &t2).await;
    assert_eq!(status_of(&resp), 200, "откат второго хода: {resp}");
    assert_eq!(
        std::fs::read(&note).unwrap(),
        b"state-2",
        "файл вернулся к снимку хода 2"
    );

    // История пережила откат: события не продублировались, порядок тот же.
    let snap3 = transcript(port, &sid).await;
    assert_eq!(snap2, snap3, "откат изменил историю сессии");

    // Откат первого хода возвращает state-1, история всё ещё цела.
    let resp = revert(port, &sid, &t1).await;
    assert_eq!(status_of(&resp), 200, "откат первого хода: {resp}");
    assert_eq!(
        std::fs::read(&note).unwrap(),
        b"state-1",
        "файл вернулся к снимку хода 1"
    );
    let snap4 = transcript(port, &sid).await;
    assert_eq!(snap2, snap4, "второй откат изменил историю");

    // Откат несуществующего хода — честный отказ, не паника.
    let resp = revert(port, &sid, "t-net-takogo").await;
    assert_eq!(
        status_of(&resp),
        400,
        "чужой turn_id должен получить 400: {resp}"
    );
    assert!(resp.contains("ход не найден"), "причина понятна: {resp}");

    // Сессия живёт после отката: следующий ход проходит и дописывает историю.
    start_turn(port, &sid, "ход три").await;
    await_turn_ends(&mut sse, &[(&sid, 1)], "ход три").await;
    let snap5 = transcript(port, &sid).await;
    assert_prefix(&snap4, &snap5, "после отката и нового хода");
    assert_eq!(content_texts(&snap5).last(), Some(&"эхо: ход три"));
}

// ---------- сцена 2: параллельные сессии ----------

/// Две сессии одновременно: ход в A и ход в B идут параллельно, ответы не
/// пересекаются, история каждой сессии — только её собственная.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn flow_parallel_sessions_answers_do_not_cross() {
    let (port, _st) = serve().await;
    let mut sse = open_sse(port).await;
    let sid_a = create_session(port, &workdir("par-a"), None).await;
    let sid_b = create_session(port, &workdir("par-b"), None).await;

    // Оба хода ушли одновременно — как два окна приложения.
    let (ra, rb) = tokio::join!(
        try_start_turn(port, &sid_a, "вопрос альфа"),
        try_start_turn(port, &sid_b, "вопрос бета")
    );
    assert_eq!(status_of(&ra), 200, "ход A не принят: {ra}");
    assert_eq!(status_of(&rb), 200, "ход B не принят: {rb}");
    let (ta, tb) = (
        body_of(&ra)["turn_id"]
            .as_str()
            .expect("turn_id A")
            .to_string(),
        body_of(&rb)["turn_id"]
            .as_str()
            .expect("turn_id B")
            .to_string(),
    );
    assert_ne!(ta, tb, "у параллельных ходов разные turn_id");

    await_turn_ends(&mut sse, &[(&sid_a, 1), (&sid_b, 1)], "параллельные ходы").await;

    let (tr_a, tr_b) = (
        transcript(port, &sid_a).await,
        transcript(port, &sid_b).await,
    );
    assert_turn_blocks(&tr_a, "сессия A");
    assert_turn_blocks(&tr_b, "сессия B");
    assert_eq!(content_texts(&tr_a), vec!["эхо: вопрос альфа"], "ответ A");
    assert_eq!(content_texts(&tr_b), vec!["эхо: вопрос бета"], "ответ B");
    let (a_json, b_json) = (
        serde_json::to_string(&tr_a).unwrap(),
        serde_json::to_string(&tr_b).unwrap(),
    );
    assert!(!a_json.contains("бета"), "в историю A попал чужой ответ");
    assert!(!b_json.contains("альфа"), "в историю B попал чужой ответ");

    // Обе сессии видны списком, ходов — по одному.
    let list = sessions_list(port).await;
    for (sid, label) in [(&sid_a, "A"), (&sid_b, "B")] {
        let brief = list
            .iter()
            .find(|s| s["id"].as_str() == Some(sid.as_str()))
            .unwrap_or_else(|| panic!("сессия {label} пропала из списка"));
        assert_eq!(brief["turns"].as_i64(), Some(1), "у {label} один ход");
    }
}

// ---------- сцена 3: занятая сессия ----------

/// Ход ещё идёт — юзер успевает открыть чат, нажать «Откатить» и отправить
/// второй ход. Ожидание: транскрипт читается, откат честно отказывает,
/// второй ход отказывает «сессия занята», история остаётся с одним ходом,
/// сообщение не потерялось и не началось вторым параллельным ходом.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn flow_running_session_rejects_second_turn_without_parallelism() {
    let (port, _st) = serve().await;
    let mut sse = open_sse(port).await;
    let cwd = workdir("busy");
    std::fs::write(cwd.join("note.txt"), b"base").unwrap();
    let sid = create_session(port, &cwd, None).await;

    // Длинный ход уходит — и юзер сразу тыкает всё подряд, не дожидаясь.
    let t1 = start_turn(port, &sid, &slow_message()).await;

    // Чат открыт во время хода: транскрипт не виснет, ходов в нём ноль.
    let mid = transcript(port, &sid).await;
    assert!(
        mid.is_empty(),
        "незавершённый ход не показывается как готовый"
    );

    // Откат во время хода — честный отказ до всякого git (F-1).
    let resp = revert(port, &sid, &t1).await;
    assert_eq!(status_of(&resp), 400, "откат при живом ходе: {resp}");
    assert!(
        resp.contains("сначала остановите"),
        "причина отказа понятна: {resp}"
    );

    // Второй ход в ту же сессию — отказ, а не параллельный ход.
    let resp = try_start_turn(port, &sid, "второй ход не должен начаться").await;
    assert_eq!(
        status_of(&resp),
        400,
        "занятая сессия обязана отказать: {resp}"
    );
    assert!(resp.contains("занята"), "причина отказа понятна: {resp}");

    // Первый ход доигрывает до конца сам.
    await_turn_ends(&mut sse, &[(&sid, 1)], "длинный ход").await;

    // История: ровно один ход, отвергнутое сообщение в неё не попало.
    let snap = transcript(port, &sid).await;
    assert_turn_blocks(&snap, "после длинного хода");
    assert_eq!(
        snap.iter().filter(|e| kind_of(e) == "user").count(),
        1,
        "одно сообщение юзера"
    );
    let json = serde_json::to_string(&snap).unwrap();
    assert!(
        !json.contains("второй ход не должен начаться"),
        "отвергнутое сообщение протекло в историю"
    );

    // Отказ не сломал сессию: следующий ход проходит.
    start_turn(port, &sid, "теперь можно").await;
    await_turn_ends(&mut sse, &[(&sid, 1)], "ход после отказа").await;
    let snap2 = transcript(port, &sid).await;
    assert_prefix(&snap, &snap2, "после хода поверх отказа");
    assert_eq!(content_texts(&snap2).last(), Some(&"эхо: теперь можно"));
}

// ---------- сцена 4: ход за ходом без паузы ----------

/// Ход завершён — следующий уходит немедленно, без паузы между финалом и
/// отправкой. Три хода подряд, включая эмодзи и CJK в сообщении (реальный
/// юзер шлёт не только ASCII). Ожидание: все три принимаются и доводятся
/// до конца, история растёт монотонно.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn flow_back_to_back_turns_without_pause() {
    let (port, _st) = serve().await;
    let mut sse = open_sse(port).await;
    let sid = create_session(port, &workdir("chain"), None).await;

    let messages = ["ход раз", "ход 🐟 два", "ход три 日本語"];
    let mut prev: Vec<serde_json::Value> = Vec::new();
    for msg in messages {
        start_turn(port, &sid, msg).await;
        // Финал увидели — следующий ход уходит в тот же миг.
        await_turn_ends(&mut sse, &[(&sid, 1)], "цепочка ходов").await;
        let snap = transcript(port, &sid).await;
        assert_prefix(&prev, &snap, "цепочка");
        prev = snap;
    }

    assert_turn_blocks(&prev, "цепочка из трёх ходов");
    assert_eq!(
        user_texts(&prev),
        messages.to_vec(),
        "реплики юзера по порядку"
    );
    let contents = content_texts(&prev);
    let expected: Vec<String> = messages.iter().map(|m| format!("эхо: {m}")).collect();
    assert_eq!(
        contents,
        expected.iter().map(|s| s.as_str()).collect::<Vec<_>>(),
        "ответы по порядку"
    );

    let list = sessions_list(port).await;
    let brief = list
        .iter()
        .find(|s| s["id"].as_str() == Some(sid.as_str()))
        .expect("сессия в списке");
    assert_eq!(brief["turns"].as_i64(), Some(3), "в списке три хода");
}

// ---------- сцена 5: стоп посреди стрима ----------

/// Юзер жмёт «стоп», пока агент работает над длинным ответом: ход обязан
/// завершиться отменённым, ровно один раз, история — остаться консистентной
/// (реплика юзера на месте, частичный ответ не выдаётся за полный), кнопка
/// «стоп» на уже мёртвом ходе — честный отказ, следующий ход работает.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn flow_stop_mid_stream_then_session_recovers() {
    let (port, _st) = serve().await;
    let mut sse = open_sse(port).await;
    let sid = create_session(port, &workdir("stop"), None).await;

    // Длинный ход уходит — и юзер тут же передумал: «стоп» уходит в тот же
    // миг, пока ход жив (setup ходов занимает десятки миллисекунд — окно
    // больше кругового пути запроса).
    let t1 = start_turn(port, &sid, &slow_message()).await;

    let resp = stop_turn(port, &sid).await;
    assert_eq!(status_of(&resp), 200, "стоп по живому ходу: {resp}");

    // Финал приезжает и он один.
    let final_events = await_turn_ends(&mut sse, &[(&sid, 1)], "остановленный ход").await;
    let ended: Vec<&serde_json::Value> = final_events
        .iter()
        .filter(|e| {
            kind_of(e) == "turn_ended"
                && e.pointer("/kind/data/session").and_then(|v| v.as_str()) == Some(sid.as_str())
        })
        .collect();
    assert_eq!(ended.len(), 1, "ровно один turn_ended: {ended:?}");
    assert_eq!(
        ended[0].pointer("/kind/data/turn").and_then(|v| v.as_str()),
        Some(t1.as_str()),
        "финал того же хода"
    );
    assert_eq!(
        ended[0].pointer("/kind/data/ok").and_then(|v| v.as_bool()),
        Some(false),
        "ход отменён, не «успех»"
    );
    assert_eq!(
        ended[0]
            .pointer("/kind/data/reason")
            .and_then(|v| v.as_str()),
        Some("остановлено пользователем"),
        "причина отмены в финале"
    );

    // Хвост после финала пуст от вторых финалов (UI не дедуплицирует их).
    let tail = drain_sse(&mut sse, 300).await;
    let tail_ends = tail
        .iter()
        .filter(|e| {
            kind_of(e) == "turn_ended"
                && e.pointer("/kind/data/session").and_then(|v| v.as_str()) == Some(sid.as_str())
        })
        .count();
    assert_eq!(tail_ends, 0, "после финала приехал второй финал: {tail:?}");

    // История: реплика юзера на месте, отменённый ход не притворяется полным.
    // Допустимы две формы: стоп в тишине (контент не сохранился) и стоп
    // в момент конца стрима (контент успел доехать до машины) — контракт
    // один: ход один, он отменён, реплика юзера не потеряна.
    let snap = transcript(port, &sid).await;
    assert_eq!(
        user_texts(&snap),
        vec![slow_message()],
        "реплика юзера пережила отмену"
    );
    let kinds: Vec<&str> = snap.iter().map(kind_of).collect();
    assert!(
        kinds == vec!["turn_started", "user", "turn_ended"]
            || kinds == vec!["turn_started", "user", "reasoning", "content", "turn_ended"],
        "отменённый ход в истории неузнаваем: {kinds:?}"
    );

    // Двойной клик по «стоп»: живого хода нет — честный отказ.
    let resp = stop_turn(port, &sid).await;
    assert_eq!(status_of(&resp), 400, "повторный стоп: {resp}");
    assert!(
        resp.contains("нет живого хода"),
        "причина отказа понятна: {resp}"
    );

    // Состояние сессии видно списком: отменён, один ход, не «успех».
    let list = sessions_list(port).await;
    let brief = list
        .iter()
        .find(|s| s["id"].as_str() == Some(sid.as_str()))
        .expect("сессия в списке");
    assert_eq!(
        brief["status"].as_str(),
        Some("cancelled"),
        "статус после стопа: {brief}"
    );
    assert_eq!(brief["turns"].as_i64(), Some(1), "один ход в журнале");
    assert_eq!(
        brief["last_ok"].as_bool(),
        Some(false),
        "последний ход не успешен"
    );

    // Сессия жива: следующий ход проходит целиком.
    start_turn(port, &sid, "после остановки").await;
    await_turn_ends(&mut sse, &[(&sid, 1)], "ход после стопа").await;
    let snap2 = transcript(port, &sid).await;
    assert_prefix(&snap, &snap2, "после стопа и нового хода");
    assert_eq!(content_texts(&snap2).last(), Some(&"эхо: после остановки"));

    let list = sessions_list(port).await;
    let brief = list
        .iter()
        .find(|s| s["id"].as_str() == Some(sid.as_str()))
        .expect("сессия в списке");
    assert_eq!(
        brief["status"].as_str(),
        Some("idle"),
        "статус после нового хода: {brief}"
    );
    assert_eq!(brief["turns"].as_i64(), Some(2), "два хода в журнале");
}

// ---------- сцена 6: длинная сессия ----------

/// 22 хода подряд: история цела, блоки ходов полные, turn_id уникальны,
/// реплики и ответы по порядку, транскрипт стабилен при перечитывании.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn flow_long_session_22_turns_intact() {
    let (port, _st) = serve().await;
    let mut sse = open_sse(port).await;
    let sid = create_session(port, &workdir("long"), None).await;

    const TURNS: usize = 22;
    let mut prev: Vec<serde_json::Value> = Vec::new();
    for i in 1..=TURNS {
        let msg = format!("ход {i}");
        start_turn(port, &sid, &msg).await;
        await_turn_ends(&mut sse, &[(&sid, 1)], format!("ход {i}").as_str()).await;
        let snap = transcript(port, &sid).await;
        assert_prefix(&prev, &snap, "длинная сессия");
        prev = snap;
    }

    assert_turn_blocks(&prev, "длинная сессия");
    let ids = turn_ids(&prev);
    let unique: std::collections::HashSet<&&str> = ids.iter().collect();
    assert_eq!(unique.len(), ids.len(), "turn_id уникальны");
    let users = user_texts(&prev);
    let expected_users: Vec<String> = (1..=TURNS).map(|i| format!("ход {i}")).collect();
    assert_eq!(
        users,
        expected_users
            .iter()
            .map(|s| s.as_str())
            .collect::<Vec<_>>(),
        "реплики по порядку"
    );
    let contents = content_texts(&prev);
    let expected_contents: Vec<String> = (1..=TURNS).map(|i| format!("эхо: ход {i}")).collect();
    assert_eq!(
        contents,
        expected_contents
            .iter()
            .map(|s| s.as_str())
            .collect::<Vec<_>>(),
        "ответы по порядку без потерь"
    );

    // Перечитали — то же самое.
    let again = transcript(port, &sid).await;
    assert_eq!(prev, again, "повторное чтение длинной истории меняет её");

    let list = sessions_list(port).await;
    let brief = list
        .iter()
        .find(|s| s["id"].as_str() == Some(sid.as_str()))
        .expect("сессия в списке");
    assert_eq!(
        brief["turns"].as_i64(),
        Some(TURNS as i64),
        "в списке все ходы"
    );
}

// ---------- сцена 7: подтверждения — доступные края ----------

/// Полный флоу confirm/deny недостижим на эхе (нет tool-call), но края
/// контракта проверяемы: решение по несуществующему диалогу — честный
/// отказ; кривое решение — честный отказ; пустой запрос — честный отказ.
/// Ни один из них не должен ни виснуть, ни молча «проходить».
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn flow_approval_edges_are_honest() {
    let (port, _st) = serve().await;
    let sid = create_session(port, &workdir("approve"), None).await;

    for decision in ["approved", "denied"] {
        let body = serde_json::json!({ "call_id": "call-net-takogo", "decision": decision, "session_id": sid }).to_string();
        let resp = post(port, "/v1/approvals", &body).await;
        assert_eq!(
            status_of(&resp),
            400,
            "ответ по чужому call_id ({decision}): {resp}"
        );
        assert!(
            resp.contains("нет ожидающего"),
            "причина отказа понятна: {resp}"
        );
    }

    let body =
        serde_json::json!({ "call_id": "c1", "decision": "maybe", "session_id": sid }).to_string();
    let resp = post(port, "/v1/approvals", &body).await;
    assert_eq!(status_of(&resp), 400, "кривое решение: {resp}");

    let resp = post(port, "/v1/approvals", "{}").await;
    assert_eq!(status_of(&resp), 400, "пустой запрос: {resp}");

    // Живая сессия не пострадала от тыканья в чужие подтверждения.
    // Подписка открыта ДО хода: broadcast не хранит историю.
    let mut sse = open_sse(port).await;
    start_turn(port, &sid, "после тыканья в подтверждения").await;
    await_turn_ends(&mut sse, &[(&sid, 1)], "ход после approval-краёв").await;
    assert_eq!(
        content_texts(&transcript(port, &sid).await),
        vec!["эхо: после тыканья в подтверждения"]
    );
}

// ---------- сцена 8: мгновенный следующий ход после финала ----------

/// Контракт: событие turn_ended означает «ход завершён» — UI разблокирует
/// ввод по нему. Отправка следующего хода в ответ на turn_ended обязана
/// приниматься, а не получать «сессия занята».
///
/// Окно гонки: драйвер публикует turn_ended ДО сброса статуса сессии —
/// между публикацией и сбросом стоят запись метрик (мьютекс store, sqlite) и
/// запись журнала. В покое окно микросекундное, но тот же мьютекс делят все
/// операции приложения: параллельная сессия, пишущая prefs/метрики, держит
/// его миллисекунды. Репро удерживает store-мьютекс из отдельного потока в
/// момент между первым куском стрима и финалом — это моделирует ровно ту
/// конкуренцию, которую создаёт второе окно приложения.
///
/// Сессия создана с моделью из таблицы цен: иначе метрика стоимости не
/// пишется и окно сужается до сброса статуса.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn finding_immediate_turn_after_final_is_rejected() {
    let (port, st) = serve().await;
    let mut sse = open_sse(port).await;
    let cwd = workdir("race");
    std::fs::write(cwd.join("note.txt"), b"base").unwrap();
    let sid = create_session(port, &cwd, Some("deepseek-chat")).await;

    let t1 = start_turn(port, &sid, &slow_message()).await;
    wait_first_content(&mut sse, &t1).await;

    // Держатель store-мьютекса: отдельный поток, чтобы не держать std-лок
    // через await. Задача хода встанет на нём уже ПОСЛЕ публикации финала.
    use std::sync::atomic::{AtomicBool, Ordering};
    let acquired = Arc::new(AtomicBool::new(false));
    let release = Arc::new(AtomicBool::new(false));
    let st_thread = st.clone();
    let (acq_flag, rel_flag) = (acquired.clone(), release.clone());
    let locker = std::thread::spawn(move || {
        let _guard = st_thread.store.lock().expect("store не отравлен");
        acq_flag.store(true, Ordering::SeqCst);
        while !rel_flag.load(Ordering::SeqCst) {
            std::thread::sleep(Duration::from_millis(5));
        }
    });
    for _ in 0..2_000 {
        if acquired.load(Ordering::SeqCst) {
            break;
        }
        tokio::time::sleep(Duration::from_millis(5)).await;
    }
    assert!(
        acquired.load(Ordering::SeqCst),
        "мьютекс store не взялся за 10 с"
    );

    // Финал опубликован, задача ещё не сбросила статус (стоит на метриках).
    await_turn_ends(&mut sse, &[(&sid, 1)], "финал под залоченным store").await;

    // Реакция клиента на turn_ended: следующий ход немедленно.
    let resp = try_start_turn(port, &sid, "сразу после финала").await;
    assert_eq!(
        status_of(&resp),
        200,
        "FINDING: ход после turn_ended отклонён: {resp}"
    );

    // Отпустить store, дать задаче доиграть финализацию.
    release.store(true, Ordering::SeqCst);
    let _ = locker.join();

    // Статус обязан вернуться к покою.
    let deadline = tokio::time::Instant::now() + Duration::from_secs(10);
    loop {
        let list = sessions_list(port).await;
        let brief = list
            .iter()
            .find(|s| s["id"].as_str() == Some(sid.as_str()))
            .expect("сессия");
        if brief["status"].as_str() != Some("running") {
            break;
        }
        assert!(
            tokio::time::Instant::now() < deadline,
            "статус не сбросился за 10 с после освобождения store"
        );
        tokio::time::sleep(Duration::from_millis(20)).await;
    }

    // Ход проходит и завершается — сессия здорова.
    start_turn(port, &sid, "второй после гонки").await;
    await_turn_ends(&mut sse, &[(&sid, 1)], "ход после гонки").await;
    let snap = transcript(port, &sid).await;
    assert_turn_blocks(&snap, "после гонки");
    assert_eq!(
        content_texts(&snap).last().copied().unwrap_or_default(),
        "эхо: сразу после финала"
    );
}
