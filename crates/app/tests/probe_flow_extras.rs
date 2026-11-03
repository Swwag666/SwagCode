/*! probe_flow_extras: зонд-агент, симулирующий обычного юзера-энтузиаста.

Валидные конфигурационные флоу без кривизны: юзер подключает stdio
MCP-сервер (настоящий node-процесс, JSON-RPC по протоколу MCP), пишет
JS-плагин `plugins/hello.js`, поднимает терминал в PTY, читает телеметрию
и ждёт, что hot-reload подхватит переписанный плагин-файл. Всё на
реальных маленьких артефактах во временных каталогах.

Чем это отличается от существующих тестов:
- юнит-тесты mcp.rs гоняют один echo-сервер: env-конфиг, мусор в stdout,
  reconnect по тому же имени, initialize с JSON-RPC-ошибкой и вызов после
  смерти сервера здесь проверяются впервые;
- юнит-тесты jsplugins.rs покрывают sync-handler, дедлайн, prefs-мост и
  reload по команде: контракт «handler: async (args) => ({ok, output})»
  (так напишет любой энтузиаст из документации) здесь проверяется впервые;
- PTY-тест крейта ловит эхо, но не ассертит Exit-событие и resize;
- телеметрия в lib.rs проверяет один проход сэмплера, а не пару сэмплов
  с ростом счётчика (флоу «записал пару сэмплов → прочитал историю»);
- hot_reload.rs тестирует debounce функцией, но не живой watcher на
  реальной записи файла и перезагрузку каталога плагинов;

Фоновые задачи (E-5) отдельным тестом не покрыты здесь: run_due_tasks
приватный, полный прогон воркера с видами уже делает юнит-тест
`tasks_worker_runs_handlers_with_backoff` в lib.rs; повторять его
в integration-слое значило бы дублировать существующий флоу.

Кейсы, которые падают (находки), помечаются #[ignore] с FINDING: репро
живёт в файле, cargo test остаётся зелёным. */

use std::collections::HashMap;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::Duration;

use serde_json::json;

use swagcod_app_lib::hot_reload::HotReloadCoordinator;
use swagcod_app_lib::jsplugins::JsHost;
use swagcod_app_lib::mcp::{call_tool, connect, list_tools, McpRegistry, McpServerConfig};
use swagcod_app_lib::{run_metrics_sample, AppState};
use swagcod_pty::{default_shell, PtyManager, PtyMsg};

/* ---------- общие хелперы ---------- */

static SEQ: AtomicU64 = AtomicU64::new(0);

fn uid() -> u64 {
    SEQ.fetch_add(1, Ordering::Relaxed) ^ ((std::process::id() as u64) << 20)
}

/// Временный каталог с гарантированным созданием.
fn tempdir(tag: &str) -> PathBuf {
    let dir = std::env::temp_dir().join(format!("swagcod-probe-{tag}-{}", uid()));
    std::fs::create_dir_all(&dir).expect("временный каталог создаётся");
    dir
}

fn write_script(dir: &Path, name: &str, body: &str) -> PathBuf {
    let p = dir.join(name);
    std::fs::write(&p, body).expect("скрипт записывается");
    p
}

/// Где взять node для живых флоу: SWAGCOD_NODE → vendor репозитория → PATH.
/// Тот же порядок, что у node_store::node_binary, но без приватных деталей.
fn node_for_tests() -> Option<String> {
    if let Ok(n) = std::env::var("SWAGCOD_NODE") {
        if !n.is_empty() {
            return Some(n);
        }
    }
    let vendored = Path::new(env!("CARGO_MANIFEST_DIR")).join("../../vendor/node/node.exe");
    if vendored.is_file() {
        return Some(vendored.to_string_lossy().to_string());
    }
    Some("node".to_string())
}

fn mcp_cfg(name: &str, node: &str, script: &Path, env: HashMap<String, String>) -> McpServerConfig {
    McpServerConfig {
        name: name.to_string(),
        command: node.to_string(),
        args: vec![script.to_string_lossy().to_string()],
        env,
        enabled: true,
    }
}

const CALL_TIMEOUT: Duration = Duration::from_secs(10);

/* ================= MCP: живые stdio-серверы ================= */

/// Скелет MCP-сервера: initialize → tools/list → tools/call. `on_call` —
/// тело ответа tools/call, чтобы каждый тест наращивал своё поведение.
fn mcp_server_js(tools_decl: &str, on_call: &str) -> String {
    format!(
        r#"
const readline = require('readline');
const rl = readline.createInterface({{ input: process.stdin }});
function send(o) {{ process.stdout.write(JSON.stringify(o) + '\n'); }}
rl.on('line', (line) => {{
  let m; try {{ m = JSON.parse(line); }} catch {{ return; }}
  if (m.method === 'initialize') {{
    send({{jsonrpc:'2.0', id:m.id, result:{{protocolVersion:'2024-11-05',
      capabilities:{{tools:{{}}}}, serverInfo:{{name:'probe', version:'0.1'}}}}}});
  }} else if (m.method === 'tools/list') {{
    send({{jsonrpc:'2.0', id:m.id, result:{{tools:[{tools_decl}]}}}});
  }} else if (m.method === 'tools/call') {{
    const a = (m.params && m.params.arguments) || {{}};
    {on_call}
  }}
}});
"#
    )
}

const ECHO_TOOL: &str = r#"{name:'echo', description:'эхо', inputSchema:{type:'object',
  properties:{text:{type:'string'}}, required:['text']}}"#;

fn echo_on_call() -> &'static str {
    r#"send({jsonrpc:'2.0', id:m.id, result:{content:[{type:'text', text:'echo:' + (a.text || '')}]}});
"#
}

/// Юзер с секретом в env-конфиге сервера: переменная обязана доехать до
/// процесса. mcp.rs этого не проверяет (env там всегда пустой HashMap).
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn mcp_env_vars_reach_server_process() {
    let Some(node) = node_for_tests() else { return };
    let dir = tempdir("mcp-env");
    let body = mcp_server_js(
        r#"{name:'envvar', description:'отдаёт env', inputSchema:{type:'object',
            properties:{key:{type:'string'}}}}"#,
        r#"const k = (m.params && m.params.arguments && m.params.arguments.key) || '';
        send({jsonrpc:'2.0', id:m.id, result:{content:[{type:'text',
            text:'env:' + k + '=' + String(process.env[k] || '')}]}});
        "#,
    );
    let script = write_script(&dir, "env-mcp.js", &body);
    let cfg = mcp_cfg(
        "envsrv",
        &node,
        &script,
        HashMap::from([("SWAGCOD_PROBE_TOKEN".to_string(), "sekret42".to_string())]),
    );

    let conn = connect(&cfg).await.expect("рукопожатие с env-сервером");
    let tools = list_tools(&conn, "envsrv").await.expect("tools/list");
    assert_eq!(tools.len(), 1);
    let out = call_tool(
        &conn,
        "envvar",
        json!({"key": "SWAGCOD_PROBE_TOKEN"}),
        CALL_TIMEOUT,
    )
    .await
    .expect("tools/call");
    assert_eq!(out, "env:SWAGCOD_PROBE_TOKEN=sekret42");

    conn.kill().await;
    let _ = std::fs::remove_dir_all(&dir);
}

/// Reconnect по тому же имени (кнопка в настройках): старое соединение
/// умирает, тулзы не дублируются, вызов после переподключения живой.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn mcp_reconnect_replaces_connection_and_dedups_tools() {
    let Some(node) = node_for_tests() else { return };
    let dir = tempdir("mcp-reconnect");
    let script = write_script(
        &dir,
        "echo-mcp.js",
        &mcp_server_js(ECHO_TOOL, echo_on_call()),
    );
    let cfg = mcp_cfg("echo", &node, &script, HashMap::new());

    let mut reg = McpRegistry::default();
    assert_eq!(reg.connect_server(&cfg).await, 1, "первое подключение");
    assert_eq!(
        reg.connect_server(&cfg).await,
        1,
        "reconnect вернул те же 1 тулзу"
    );
    assert_eq!(reg.tools.len(), 1, "тулзы задублировались после Reconnect");
    assert_eq!(reg.status().len(), 1, "серверов в статусе два");

    let st = &reg.status()[0];
    assert!(st.state.contains("подключён"), "статус: {st:?}");

    let conn = reg.conn("echo").expect("живое соединение после Reconnect");
    let out = call_tool(&conn, "echo", json!({"text": "ping"}), CALL_TIMEOUT)
        .await
        .expect("вызов после переподключения");
    assert_eq!(out, "echo:ping");

    reg.shutdown().await;
    let _ = std::fs::remove_dir_all(&dir);
}

/// initialize отвечает JSON-RPC-ошибкой (кривой/чужой сервер): connect
/// обязан вернуть честный Err и не оставить висящий процесс-соединение.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn mcp_initialize_jsonrpc_error_is_honest() {
    let Some(node) = node_for_tests() else { return };
    let dir = tempdir("mcp-init-err");
    let body = r#"
const readline = require('readline');
const rl = readline.createInterface({ input: process.stdin });
rl.on('line', (line) => {
  let m; try { m = JSON.parse(line); } catch { return; }
  if (m.method === 'initialize') {
    process.stdout.write(JSON.stringify({jsonrpc:'2.0', id:m.id,
      error:{code:-32000, message:'fake-err: инициализация не удалась'}}) + '\n');
  }
});
"#;
    let script = write_script(&dir, "err-mcp.js", body);
    let cfg = mcp_cfg("errsrv", &node, &script, HashMap::new());

    let err_text = match connect(&cfg).await {
        Ok(_conn) => panic!("initialize с error обязан отказать, получено соединение"),
        Err(e) => e,
    };
    assert!(
        err_text.contains("fake-err"),
        "текст ошибки сервера потерялся: {err_text}"
    );

    let _ = std::fs::remove_dir_all(&dir);
}

/// Сервер, который логирует в stdout не-JSON строками (типичная привычка
/// сторонних MCP-серверов): транспорт обязан мусор игнорировать, а
/// протокол — работать.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn mcp_server_stdout_garbage_lines_are_ignored() {
    let Some(node) = node_for_tests() else { return };
    let dir = tempdir("mcp-noisy");
    let body = r#"
const readline = require('readline');
const rl = readline.createInterface({ input: process.stdin });
function send(o) { process.stdout.write(JSON.stringify(o) + '\n'); }
rl.on('line', (line) => {
  let m; try { m = JSON.parse(line); } catch { return; }
  // юзерский лог в протокольный канал: мусор не по протоколу
  process.stdout.write('log: something happened\n');
  if (m.method === 'initialize') {
    send({jsonrpc:'2.0', id:m.id, result:{protocolVersion:'2024-11-05',
      capabilities:{tools:{}}, serverInfo:{name:'noisy', version:'0.1'}}});
  } else if (m.method === 'tools/list') {
    send({jsonrpc:'2.0', id:m.id, result:{tools:[{name:'echo',
      description:'эхо', inputSchema:{type:'object',
      properties:{text:{type:'string'}}}}]}});
  } else if (m.method === 'tools/call') {
    const a = (m.params && m.params.arguments) || {};
    send({jsonrpc:'2.0', id:m.id, result:{content:[{type:'text', text:'echo:' + (a.text || '')}]}});
  }
});
"#;
    let script = write_script(&dir, "noisy-mcp.js", body);
    let cfg = mcp_cfg("noisy", &node, &script, HashMap::new());

    let conn = connect(&cfg)
        .await
        .expect("рукопожатие сквозь мусор в stdout");
    let tools = list_tools(&conn, "noisy")
        .await
        .expect("tools/list сквозь мусор");
    assert_eq!(tools.len(), 1);
    let out = call_tool(&conn, "echo", json!({"text": "через шум"}), CALL_TIMEOUT)
        .await
        .expect("tools/call сквозь мусор");
    assert_eq!(out, "echo:через шум");

    conn.kill().await;
    let _ = std::fs::remove_dir_all(&dir);
}

/// Сервер умирает после первого вызова: второй tools/call обязан вернуть
/// ошибку (EOF/таймаут), а не зависнуть ход навсегда.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn mcp_call_after_server_exit_returns_error_not_hang() {
    let Some(node) = node_for_tests() else { return };
    let dir = tempdir("mcp-dies");
    let body = r#"
const readline = require('readline');
const rl = readline.createInterface({ input: process.stdin });
function send(o) { process.stdout.write(JSON.stringify(o) + '\n'); }
let answered = false;
rl.on('line', (line) => {
  let m; try { m = JSON.parse(line); } catch { return; }
  if (m.method === 'initialize') {
    send({jsonrpc:'2.0', id:m.id, result:{protocolVersion:'2024-11-05',
      capabilities:{tools:{}}, serverInfo:{name:'dies', version:'0.1'}}});
  } else if (m.method === 'tools/list') {
    send({jsonrpc:'2.0', id:m.id, result:{tools:[{name:'echo',
      description:'эхо один раз', inputSchema:{type:'object',
      properties:{text:{type:'string'}}}}]}});
  } else if (m.method === 'tools/call') {
    const a = (m.params && m.params.arguments) || {};
    send({jsonrpc:'2.0', id:m.id, result:{content:[{type:'text', text:'echo:' + (a.text || '')}]}});
    if (!answered) {
      answered = true;
      setTimeout(() => process.exit(0), 100);
    }
  }
});
"#;
    let script = write_script(&dir, "die-mcp.js", body);
    let cfg = mcp_cfg("dies", &node, &script, HashMap::new());

    let conn = connect(&cfg).await.expect("рукопожатие");
    let out = call_tool(&conn, "echo", json!({"text": "once"}), CALL_TIMEOUT)
        .await
        .expect("первый вызов до смерти сервера");
    assert_eq!(out, "echo:once");

    // Даём процессу выйти: reader замечает EOF и вычищает pending.
    tokio::time::sleep(Duration::from_millis(400)).await;

    let second = tokio::time::timeout(
        Duration::from_secs(15),
        call_tool(&conn, "echo", json!({"text": "twice"}), CALL_TIMEOUT),
    )
    .await;
    match second {
        Err(_) => panic!("вызов после смерти сервера завис: хода нет, ошибки нет"),
        Ok(Err(e)) => {
            assert!(
                e.contains("соединение") || e.contains("таймаут"),
                "ожидалась ошибка канала, получено: {e}"
            );
        }
        Ok(Ok(v)) => panic!("мёртвый сервер ответил: {v}"),
    }
    conn.kill().await;
    let _ = std::fs::remove_dir_all(&dir);
}

/* ================= JS-плагины: sidecar plugin-server ================= */

const HELLO_SYNC_JS: &str = r#"
swagcod.define({
  name: 'hello',
  description: 'приветствие по имени',
  parameters: { type: 'object', properties: { name: { type: 'string' } }, required: ['name'] },
  handler: (args) => ({ ok: true, output: 'hi ' + String(args.name || '') }),
});
"#;

/// Ровно флоу из документации для энтузиаста: валидный plugins/hello.js,
/// подхватился, вызов вернул результат. Синхронный handler — эталон.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn js_sync_hello_plugin_flows_end_to_end() {
    let Some(_node) = node_for_tests() else {
        return;
    };
    if swagcod_core::node_store::sidecar_file("plugin-server.js").is_none() {
        return; // расклад sidecar не собран — честный пропуск
    }
    let dir = tempdir("js-sync");
    std::fs::write(dir.join("hello.js"), HELLO_SYNC_JS).unwrap();

    let host = JsHost::start(&dir, None).await.expect("sidecar поднялся");
    assert!(host.errors.is_empty(), "ошибки загрузки: {:?}", host.errors);
    assert_eq!(host.tools.len(), 1);
    assert_eq!(host.tools[0].name, "js:hello");
    assert_eq!(host.tools[0].short, "hello");

    let (ok, out) = host
        .call_plugin("hello", json!({"name": "мир"}))
        .await
        .expect("вызов плагина");
    assert!(ok, "{out}");
    assert_eq!(out, "hi мир");

    host.kill().await;
    let _ = std::fs::remove_dir_all(&dir);
}

/// Контракт документации: `handler: async (args) => ({ok, output})`.
/// Так напишет почти любой юзер (в JS async — дефолт): обещание —
/// результат вызова виден плагину, а не сериализованный Promise.
/// Ревизия 85: закрыто — doCall дожидается thenable с дедлайном.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn js_async_handler_returns_result_not_serialized_promise() {
    let Some(_node) = node_for_tests() else {
        return;
    };
    if swagcod_core::node_store::sidecar_file("plugin-server.js").is_none() {
        return;
    }
    let dir = tempdir("js-async");
    let body = r#"
swagcod.define({
  name: 'hello',
  description: 'асинхронный привет (контракт документации)',
  parameters: { type: 'object', properties: { name: { type: 'string' } }, required: ['name'] },
  handler: async (args) => ({ ok: true, output: 'hi ' + String(args.name || '') }),
});
"#;
    std::fs::write(dir.join("hello.js"), body).unwrap();

    let host = JsHost::start(&dir, None).await.expect("sidecar поднялся");
    assert!(host.errors.is_empty(), "ошибки загрузки: {:?}", host.errors);

    let (ok, out) = host
        .call_plugin("hello", json!({"name": "мир"}))
        .await
        .expect("вызов плагина");
    assert!(ok, "вызов обязан завершаться успешно: {out}");
    assert_eq!(
        out, "hi мир",
        "async handler молча теряет результат (сериализованный Promise?): {out:?}"
    );

    host.kill().await;
    let _ = std::fs::remove_dir_all(&dir);
}

/// handler возвращает {ok:false, output}: вызов обязан быть ошибкой с
/// текстом плагина, а не успехом с пустотой.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn js_handler_object_result_ok_false_is_honest_error() {
    let Some(_node) = node_for_tests() else {
        return;
    };
    if swagcod_core::node_store::sidecar_file("plugin-server.js").is_none() {
        return;
    }
    let dir = tempdir("js-fail");
    let body = r#"
swagcod.define({
  name: 'fail',
  description: 'всегда отказ',
  parameters: { type: 'object', properties: { reason: { type: 'string' } } },
  handler: (args) => ({ ok: false, output: 'отказ: ' + String(args.reason || 'без причины') }),
});
"#;
    std::fs::write(dir.join("fail.js"), body).unwrap();

    let host = JsHost::start(&dir, None).await.expect("sidecar поднялся");
    let (ok, out) = host
        .call_plugin("fail", json!({"reason": "нет ключа"}))
        .await
        .expect("вызов плагина");
    assert!(!ok, "ok:false обработан как успех: {out}");
    assert_eq!(out, "отказ: нет ключа");

    host.kill().await;
    let _ = std::fs::remove_dir_all(&dir);
}

/* ================= PTY: терминал юзера ================= */

/// Собрать вывод PTY до маркера (или таймаута). Возврат — накопленный текст.
fn pty_collect_until(
    rx: &std::sync::mpsc::Receiver<PtyMsg>,
    marker: &str,
    timeout: Duration,
) -> (String, Option<u32>) {
    let deadline = std::time::Instant::now() + timeout;
    let mut got = String::new();
    while std::time::Instant::now() < deadline {
        match rx.recv_timeout(Duration::from_millis(150)) {
            Ok(PtyMsg::Output(b)) => got.push_str(&String::from_utf8_lossy(&b)),
            Ok(PtyMsg::Exit(code)) => return (got, Some(code)),
            Err(std::sync::mpsc::RecvTimeoutError::Timeout) => {}
            Err(_) => break,
        }
        if got.contains(marker) {
            return (got, None);
        }
    }
    (got, None)
}

/// Юзер запустил эхо и вышел: вывод не теряется, событие Exit приходит
/// с честным кодом 0. Существующий тест крейта Exit не ассертит.
#[test]
fn pty_echo_exit_event_and_output_not_lost() {
    let dir = tempdir("pty-exit");
    let mgr = PtyManager::new();
    let shell = default_shell();
    let handle = mgr
        .spawn(
            "probe-pty-exit",
            &shell,
            dir.to_string_lossy().to_string().as_str(),
            80,
            24,
        )
        .expect("PTY поднялся");
    let rx = handle.take_rx().expect("приёмник вывода");

    let cmd = if cfg!(windows) {
        "echo swagcod-probe-pty\r"
    } else {
        "echo swagcod-probe-pty\n"
    };
    handle.write(cmd.as_bytes()).expect("команда записана");
    let (mut got, early_exit) =
        pty_collect_until(&rx, "swagcod-probe-pty", Duration::from_secs(15));
    assert!(got.contains("swagcod-probe-pty"), "эхо не доехало: {got:?}");
    assert!(early_exit.is_none(), "шелл умер до эха: {got:?}");

    let exit_cmd = if cfg!(windows) { "exit\r" } else { "exit\n" };
    handle.write(exit_cmd.as_bytes()).expect("exit записан");

    // Exit(0) обязан прийти: канал не умирает молча.
    let deadline = std::time::Instant::now() + Duration::from_secs(15);
    let mut exited = None;
    while std::time::Instant::now() < deadline {
        match rx.recv_timeout(Duration::from_millis(150)) {
            Ok(PtyMsg::Output(b)) => got.push_str(&String::from_utf8_lossy(&b)),
            Ok(PtyMsg::Exit(code)) => {
                exited = Some(code);
                break;
            }
            Err(std::sync::mpsc::RecvTimeoutError::Timeout) => {}
            Err(_) => break,
        }
    }
    handle.kill();
    assert_eq!(
        exited,
        Some(0),
        "код выхода не 0 или Exit не пришёл: {got:?}"
    );
    let _ = std::fs::remove_dir_all(&dir);
}

/// Юзер потянул окно терминала: resize не должен рвать сессию — следующая
/// команда продолжает стримить вывод.
#[test]
fn pty_resize_keeps_stream_alive() {
    let dir = tempdir("pty-resize");
    let mgr = PtyManager::new();
    let shell = default_shell();
    let handle = mgr
        .spawn(
            "probe-pty-resize",
            &shell,
            dir.to_string_lossy().to_string().as_str(),
            80,
            24,
        )
        .expect("PTY поднялся");
    let rx = handle.take_rx().expect("приёмник вывода");

    let first = if cfg!(windows) {
        "echo probe-before-resize\r"
    } else {
        "echo probe-before-resize\n"
    };
    handle.write(first.as_bytes()).unwrap();
    let (got, _) = pty_collect_until(&rx, "probe-before-resize", Duration::from_secs(15));
    assert!(got.contains("probe-before-resize"), "до resize: {got:?}");

    handle.resize(110, 40).expect("resize не должен рвать PTY");

    let second = if cfg!(windows) {
        "echo probe-after-resize\r"
    } else {
        "echo probe-after-resize\n"
    };
    handle.write(second.as_bytes()).unwrap();
    let (got2, _) = pty_collect_until(&rx, "probe-after-resize", Duration::from_secs(15));
    assert!(
        got2.contains("probe-after-resize"),
        "вывод после resize потерян: {got2:?}"
    );
    handle.kill();
    let _ = std::fs::remove_dir_all(&dir);
}

/* ================= Телеметрия: пара сэмплов → история ================= */

/// Юзер смотрит дашборд: сэмплер отработал дважды, между сэмплами счётчик
/// шины вырос — в истории две точки на метрику, значения растут.
/// (lib-тест проверяет один проход; флоу «пара сэмплов» — здесь.)
#[test]
fn metrics_sampler_two_passes_write_growing_history() {
    let state = AppState {
        store: std::sync::Mutex::new(swagcod_core::store::open_memory()),
        ..Default::default()
    };
    // Жизнь между сэмплами: события гоняют шину, seq детерминированно растёт.
    let seq1 = state
        .bus
        .publish(swagcod_core::bus::EventKind::Status {
            message: "тик телеметрии 1".into(),
        })
        .max(1);
    run_metrics_sample(&state);
    let seq2 = state
        .bus
        .publish(swagcod_core::bus::EventKind::Status {
            message: "тик телеметрии 2".into(),
        })
        .max(seq1 + 1);
    run_metrics_sample(&state);

    let g = state.store.lock().unwrap();
    let bus_rows = g.metrics_query("bus_seq", 0, 10).unwrap();
    assert_eq!(
        bus_rows.len(),
        2,
        "две точки на метрику bus_seq: {bus_rows:?}"
    );
    assert_eq!(
        bus_rows[0].1, seq1 as f64,
        "первая точка = seq на момент сэмпла"
    );
    assert_eq!(
        bus_rows[1].1, seq2 as f64,
        "вторая точка = выросший seq: {bus_rows:?}"
    );
    assert!(bus_rows[1].1 > bus_rows[0].1, "история растёт, а не стоит");

    let sessions_rows = g.metrics_query("sessions", 0, 10).unwrap();
    assert_eq!(
        sessions_rows.len(),
        2,
        "пара сэмплов пишет все метрики: {sessions_rows:?}"
    );
    assert_eq!(sessions_rows[0].1, 0.0);
    assert_eq!(sessions_rows[1].1, 0.0);
}

/* ================= Hot-reload: переписанный плагин-файл ================= */

const GREET_JS: &str = r#"
swagcod.define({
  name: 'greet',
  description: 'второй плагин, дописан пользователем',
  parameters: { type: 'object', properties: { who: { type: 'string' } } },
  handler: (args) => ({ ok: true, output: 'greet ' + String(args.who || '') }),
});
"#;

/// Флою ТЗ: юзер дописал плагин-файл в каталог plugins/ — watcher увидел,
/// sidecar перезапустился, новый инструмент появился в списке. Ожидание
/// ограничено 5 с (дебаунс 300 мс + рестарт node).
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn hot_reload_picks_up_new_plugin_file() {
    let Some(node) = node_for_tests() else { return };
    if swagcod_core::node_store::sidecar_file("plugin-server.js").is_none() {
        return;
    }
    let dir = tempdir("hot-reload");
    std::fs::write(dir.join("hello.js"), HELLO_SYNC_JS).unwrap();

    // Каталог плагинов юзера — наш tempdir (тот же env, что читает reload).
    std::env::set_var("SWAGCOD_NODE", &node);
    std::env::set_var("SWAGCOD_PLUGINS_DIR", dir.to_string_lossy().to_string());

    let state = std::sync::Arc::new(AppState {
        store: std::sync::Mutex::new(swagcod_core::store::open_memory()),
        ..Default::default()
    });

    // Приложение уже подняло sidecar (как при старте): тулзы видны.
    let host = JsHost::start(&dir, None)
        .await
        .expect("sidecar до hot-reload");
    assert_eq!(host.tools.len(), 1, "{:?}", host.errors);
    *state.js_tools.lock().unwrap() = host.tools.clone();
    *state.js_host.lock().unwrap() = Some(std::sync::Arc::new(host));

    // Координатор следит за каталогом плагинов.
    let coord = HotReloadCoordinator::new(vec![dir.clone()]).expect("watcher поднялся");
    let st = state.clone();
    tokio::spawn(async move { coord.run(st).await });

    // Юзер дописывает плагин — без перезапуска приложения.
    std::fs::write(dir.join("greet.js"), GREET_JS).unwrap();

    let deadline = tokio::time::Instant::now() + Duration::from_secs(5);
    let mut seen = false;
    while tokio::time::Instant::now() < deadline {
        {
            let tools = state.js_tools.lock().unwrap();
            if tools.iter().any(|t| t.short == "greet") {
                seen = true;
                break;
            }
        }
        tokio::time::sleep(Duration::from_millis(100)).await;
    }
    std::env::remove_var("SWAGCOD_PLUGINS_DIR");
    std::env::remove_var("SWAGCOD_NODE");

    assert!(
        seen,
        "hot-reload не подхватил greet.js за 5 с; tools: {:?}",
        state
            .js_tools
            .lock()
            .unwrap()
            .iter()
            .map(|t| t.short.clone())
            .collect::<Vec<_>>()
    );
    let _ = std::fs::remove_dir_all(&dir);
}
