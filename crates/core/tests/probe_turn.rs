//! probe_turn — пробы «кривых рук» против машины хода (crates/core/src/turn.rs).
//!
//! Это НЕ регрессионные тесты продукта: это зондаж бета-тестера. Кейсы
//! сознательно кривые — дичь от модели и/или драйвера: не-строковые `path`,
//! странные пути, двойные подтверждения, колбэки не в том состоянии,
//! гигантские строки, лимиты на границе. Задача — паники, зависания,
//! нарушение инвариантов; чинить код здесь не нужно.
//!
//! Красные пробы (демонстрируют дефект) помечены `#[ignore]` + `// FINDING`
//! и оставлены как воспроизводитель: `cargo test -- --ignored`.

use swagcod_core::turn::{
    accumulate, describe_call, escapes_workspace, truncate_output, ApprovalDecision,
    ApprovalPolicy, StreamAccumulator, ToolOutcome, TurnConfig, TurnMachine, TurnOutcome, TurnStep,
    DEFAULT_MAX_ITERATIONS, MAX_TOOL_OUTPUT_BYTES,
};
use swagcod_provider::sse::{FinishReason, StreamEvent};
use swagcod_provider::types::{ChatMessage, Role, ToolCall};

#[cfg(windows)]
const ABS_OUT: &str = "C:\\Windows\\win.ini";
#[cfg(not(windows))]
const ABS_OUT: &str = "/etc/passwd";

fn call(id: &str, name: &str, args: serde_json::Value) -> ToolCall {
    ToolCall {
        id: id.into(),
        name: name.into(),
        arguments: args.as_object().cloned().unwrap_or_default(),
    }
}

fn call_path(id: &str, name: &str, path: serde_json::Value) -> ToolCall {
    call(id, name, serde_json::json!({ "path": path }))
}

fn acc_with_calls(calls: Vec<ToolCall>) -> StreamAccumulator {
    StreamAccumulator {
        tool_calls: calls,
        ..Default::default()
    }
}

fn acc_path(id: &str, name: &str, path: serde_json::Value) -> StreamAccumulator {
    acc_with_calls(vec![call_path(id, name, path)])
}

fn acc_text(text: &str) -> StreamAccumulator {
    StreamAccumulator {
        content: text.into(),
        ..Default::default()
    }
}

fn ok_out(text: &str) -> ToolOutcome {
    ToolOutcome {
        ok: true,
        output: text.into(),
    }
}

fn never_cfg() -> TurnConfig {
    TurnConfig {
        approval_policy: ApprovalPolicy::Never,
        ..Default::default()
    }
}

fn always_cfg() -> TurnConfig {
    TurnConfig {
        approval_policy: ApprovalPolicy::Always,
        ..Default::default()
    }
}

fn start(cfg: TurnConfig) -> (TurnMachine, TurnStep) {
    TurnMachine::new(cfg, vec![ChatMessage::user("go")])
}

fn is_finish(step: &TurnStep, outcome: TurnOutcome) -> bool {
    matches!(step, TurnStep::Finish { outcome: o } if *o == outcome)
}

/// Значение-матрёшка глубиной `depth` — для вложенных «path».
fn deep_value(depth: usize) -> serde_json::Value {
    let mut v = serde_json::Value::String("bottom".into());
    for _ in 0..depth {
        v = serde_json::json!({ "nested": v });
    }
    v
}

/// Инвариант из доки turn.rs: каждый tool_call ассистента закрыт
/// tool-сообщением с тем же id. Возвращает незакрытые id.
fn unclosed_tool_calls(history: &[ChatMessage]) -> Vec<String> {
    let mut open: Vec<String> = Vec::new();
    for msg in history {
        match msg.role {
            Role::Assistant => open.extend(msg.tool_calls.iter().map(|c| c.id.clone())),
            Role::Tool => {
                if let Some(id) = msg.tool_call_id.clone() {
                    if let Some(pos) = open.iter().position(|o| *o == id) {
                        open.remove(pos);
                    }
                }
            }
            _ => {}
        }
    }
    open
}

/// Обратное нарушение: tool-сообщение без открытого вызова (или без id).
fn orphan_tool_messages(history: &[ChatMessage]) -> Vec<String> {
    let mut open: Vec<String> = Vec::new();
    let mut orphans = Vec::new();
    for msg in history {
        match msg.role {
            Role::Assistant => open.extend(msg.tool_calls.iter().map(|c| c.id.clone())),
            Role::Tool => match msg.tool_call_id.clone() {
                Some(id) => match open.iter().position(|o| *o == id) {
                    Some(pos) => {
                        open.remove(pos);
                    }
                    None => orphans.push(id),
                },
                None => orphans.push("<без tool_call_id>".into()),
            },
            _ => {}
        }
    }
    orphans
}

/* ── escapes_workspace: кривые значения path ─────────────────────────────── */

#[test]
fn escapes_unit_weird_path_values_do_not_panic() {
    // path НЕ строка, а ядро ждёт строку: что будет с гейтом?
    for v in [
        serde_json::json!(12345),
        serde_json::json!(["C:\\x"]),
        serde_json::json!({ "вложенный": "C:\\x" }),
        serde_json::json!(true),
        serde_json::json!(null),
        deep_value(100),
    ] {
        let c = call_path("c1", "read", v.clone());
        // Не паникует и не считает выходом: нестроковый path просто мимо гейта.
        assert!(!escapes_workspace(&c), "path={v:?}");
    }
}

#[cfg(windows)]
#[test]
fn escapes_unit_windows_absolute_forms_are_caught() {
    for p in [
        r"\\server\share\x",
        r"\\?\C:\x",
        r"\\?\UNC\server\share\x",
        r"\\.\C:\x",
        "C:/forward/slash",
        "C:\\日本語\\x",
        r"C:\Windows\win.ini ",
        "  C:\\Windows",
    ] {
        let c = call_path("c1", "read", serde_json::json!(p));
        assert!(escapes_workspace(&c), "path={p:?} обязан ловиться");
    }
}

#[test]
fn escapes_unit_tool_name_case_and_kinds() {
    // Регистр имени тулза не должен выводить path-гейт из строя.
    for name in [
        "READ", "read", "Grep", "GREP", "List", "Patch", "Write", "GLOB",
    ] {
        let c = call_path("c1", name, serde_json::json!(ABS_OUT));
        assert!(escapes_workspace(&c), "{name} с абсолютным путём = выход");
    }
    // write с url вместо пути: путь не абсолютный, гейт не для этого.
    let c = call_path("c1", "write", serde_json::json!("https://x"));
    assert!(!escapes_workspace(&c));
    // Не-файловый тулз с абсолютным путём: путь — деталь аргументов, не цель.
    let c = call_path("c1", "fetch_url", serde_json::json!(ABS_OUT));
    assert!(!escapes_workspace(&c));
    // path из одного пробела и отсутствующий path — не выход.
    let c = call_path("c1", "read", serde_json::json!(" "));
    assert!(!escapes_workspace(&c));
    let c = call("c1", "grep", serde_json::json!({ "pattern": "x" }));
    assert!(!escapes_workspace(&c));
}

#[test]
fn escapes_unit_ten_thousand_keys_is_instant() {
    let mut map = serde_json::Map::new();
    for i in 0..10_000 {
        map.insert(format!("k{i}"), serde_json::json!(i));
    }
    map.insert("path".into(), serde_json::json!(ABS_OUT));
    let c = ToolCall {
        id: "c1".into(),
        name: "read".into(),
        arguments: map,
    };
    assert!(escapes_workspace(&c));
}

#[test]
fn machine_type_confused_path_executes_without_dialog() {
    // Машина, дефолт OnDangerous: read не «опасное имя», нестроковый path
    // мимо path-гейта → вызов уходит в исполнение без диалога.
    let (mut m, _) = start(TurnConfig::default());
    for v in [
        serde_json::json!(12345),
        serde_json::json!([1, 2]),
        serde_json::json!({ "a": "C:\\x" }),
        serde_json::json!(null),
        deep_value(50),
        serde_json::json!(" "),
    ] {
        let step = m.on_stream(acc_path("c1", "read", v));
        assert!(
            matches!(step, TurnStep::ExecuteTool { .. }),
            "кривой path уходит в исполнение без диалога: {step:?}"
        );
        let _ = m.on_tool_result(ok_out("err: bad path"));
    }
}

/* ── escapes_workspace: FINDING-кандидаты (Windows-специфика) ────────────── */

#[cfg(windows)]
// Ревизия 84 (бывшая находка 1): drive-relative/rooted/POSIX-пути ловятся
// гейтом — не только is_absolute().
#[test]
fn escapes_unit_windows_relativeish_paths_missed() {
    // Якорные формы Windows, которые std не считает абсолютными, всё
    // равно уводят за воркспейс: гейт обязан их видеть.
    for p in [
        "C:Windows\\System32",
        r"\Windows\System32",
        "/etc/passwd",
        r"\\server",
    ] {
        let c = call_path("c1", "read", serde_json::json!(p));
        assert!(
            escapes_workspace(&c),
            "путь {p:?} уводит файловый тулз за воркспейс, но гейт его не видит"
        );
    }
}

#[cfg(windows)]
// Ревизия 84 (бывшая находка 2): drive-relative путь уходит на диалог.
#[test]
fn escapes_gate_drive_relative_path_bypasses_approval() {
    // OnDangerous (дефолт) + read + drive-relative путь — подтверждение
    // человека: путь резолвится от текущего каталога диска C:, т.е. читает
    // файловую систему владельца, а не проект.
    let (mut m, _) = start(TurnConfig::default());
    let step = m.on_stream(acc_path(
        "c1",
        "read",
        serde_json::json!("C:Windows\\System32\\config\\SAM"),
    ));
    assert!(
        matches!(step, TurnStep::AwaitApproval { .. }),
        "drive-relative путь обязан уходить на подтверждение: {step:?}"
    );
}

#[cfg(windows)]
// Ревизия 84 (бывшая находка 3): rooted-путь без префикса — на диалог.
#[test]
fn escapes_gate_rooted_backslash_path_bypasses_approval() {
    // Путь с корнем, но без префикса диска уводит за воркспейс — диалог.
    let (mut m, _) = start(TurnConfig::default());
    let step = m.on_stream(acc_path(
        "c1",
        "read",
        serde_json::json!(r"\Windows\System32\drivers\etc\hosts"),
    ));
    assert!(
        matches!(step, TurnStep::AwaitApproval { .. }),
        "rooted-путь без префикса обязан уходить на подтверждение: {step:?}"
    );
}

#[cfg(windows)]
#[test]
fn std_join_rooted_or_prefixed_path_replaces_workspace_base() {
    // Механика исполнителя: fsx резолвит путь как cwd.join(path). Пути с
    // корнем или префиксом диска заменяют базу целиком — поэтому пропуск
    // гейта (FINDING 1–3) означает реальное чтение вне воркспейса.
    let base = std::path::PathBuf::from(r"D:\workspace");
    let ws = std::path::Path::new(r"D:\workspace");
    for p in [r"\Windows\System32", "/etc/passwd", "C:evil"] {
        let joined = base.join(p);
        eprintln!("join({p:?}) = {joined:?}");
        assert!(
            !joined.starts_with(ws),
            "join с {p:?} уводит результат из воркспейса: {joined:?}"
        );
    }
}

/* ── машина хода: повадки и кривые колбэки ───────────────────────────────── */

#[test]
fn machine_on_tool_result_with_no_pending_returns_failed_and_keeps_history() {
    // on_tool_result БЕЗ активного вызова (ревизия 84): мусорный колбэк
    // гасится Noop — не Failed и не закрытие чужого вызова. История не
    // тронута.
    let (mut m, _) = start(never_cfg());
    let step = m.on_tool_result(ok_out("из ниоткуда"));
    assert!(matches!(step, TurnStep::Noop), "{step:?}");
    assert_eq!(m.history().len(), 1, "история не тронута");
}

#[test]
fn machine_on_tool_result_after_completed_finish() {
    // Поздний результат после завершённого хода (ревизия 84): ход
    // терминален, колбэк гасится Noop — Completed не флипается в Failed
    // мусорным событием, история не растёт.
    let (mut m, _) = start(never_cfg());
    let step = m.on_stream(acc_text("готово"));
    assert!(is_finish(&step, TurnOutcome::Completed));
    assert_eq!(m.history().len(), 2);
    let step = m.on_tool_result(ok_out("опоздал"));
    assert!(matches!(step, TurnStep::Noop), "{step:?}");
    assert_eq!(
        m.history().len(),
        2,
        "история не растёт от позднего результата"
    );
    assert!(unclosed_tool_calls(m.history()).is_empty());
    assert!(orphan_tool_messages(m.history()).is_empty());
}

#[test]
fn machine_on_approval_without_await_state_is_tolerated() {
    // on_approval без диалога (Never уже отдала вызов в исполнение,
    // ревизия 84): поздний Denied не отменяет уже исполняемый вызов и не
    // закрывает его — закрытие придёт штатным on_tool_result.
    let (mut m, _) = start(never_cfg());
    let step = m.on_stream(acc_with_calls(vec![call(
        "c1",
        "read",
        serde_json::json!({}),
    )]));
    assert!(matches!(step, TurnStep::ExecuteTool { .. }));
    let step = m.on_approval(ApprovalDecision::Denied);
    assert!(matches!(step, TurnStep::Noop), "{step:?}");
    // Вызов ещё в работе: закрывается результатом, не чужим колбэком.
    let step = m.on_tool_result(ok_out("r1"));
    assert!(matches!(step, TurnStep::RequestModel { .. }));
    assert!(unclosed_tool_calls(m.history()).is_empty());
    assert!(orphan_tool_messages(m.history()).is_empty());
}

#[test]
fn machine_empty_id_and_name_flow_through() {
    // Пустые id/name от модели: не паникуем, пустое имя = неизвестный тулз
    // (диалог обязателен), пустой id протекает в tool_call_id (wire-слой
    // подменяет заглушкой — см. types.rs WireToolCall).
    let (mut m, _) = start(TurnConfig::default());
    let step = m.on_stream(acc_with_calls(vec![call("", "", serde_json::json!({}))]));
    assert!(
        matches!(step, TurnStep::AwaitApproval { .. }),
        "пустое имя = неизвестное → диалог: {step:?}"
    );
    let step = m.on_approval(ApprovalDecision::Approved);
    assert!(matches!(step, TurnStep::ExecuteTool { .. }));
    let _ = m.on_tool_result(ok_out("x"));
    let tools: Vec<_> = m
        .history()
        .iter()
        .filter(|msg| msg.role == Role::Tool)
        .collect();
    assert_eq!(tools.len(), 1);
    assert_eq!(tools[0].tool_call_id.as_deref(), Some(""));
}

#[test]
fn approval_policy_case_and_lookalike_names() {
    // ASCII-регистр не меняет ни builtin, ни dangerous.
    assert!(ApprovalPolicy::OnDangerous.requires_approval("BASH"));
    assert!(!ApprovalPolicy::OnDangerous.requires_approval("READ"));
    assert!(!ApprovalPolicy::OnDangerous.requires_approval("Memory_Add"));
    // Неточные копии имён — неизвестные: диалог обязателен.
    for name in ["ｒｅａｄ", "read ", " READ", "re\0ad", "Bash\t"] {
        assert!(
            ApprovalPolicy::OnDangerous.requires_approval(name),
            "{name:?} не должен считаться встроенным"
        );
    }
}

#[test]
fn machine_normal_flow_keeps_invariant() {
    // Санити зонд-валидатора: здоровая последовательность не даёт ни
    // незакрытых вызовов, ни tool-сообщений-сирот.
    let (mut m, _) = start(never_cfg());
    let step = m.on_stream(acc_with_calls(vec![
        call("c1", "read", serde_json::json!({})),
        call("c2", "bash", serde_json::json!({"command": "sleep 1"})),
    ]));
    assert!(matches!(step, TurnStep::ExecuteTool { call } if call.id == "c1"));
    let step = m.on_tool_result(ok_out("r1"));
    assert!(matches!(step, TurnStep::ExecuteTool { call } if call.id == "c2"));
    let step = m.on_tool_result(ok_out("r2"));
    assert!(matches!(step, TurnStep::RequestModel { .. }));
    let step = m.on_stream(acc_text("done"));
    assert!(is_finish(&step, TurnOutcome::Completed));
    assert!(unclosed_tool_calls(m.history()).is_empty());
    assert!(orphan_tool_messages(m.history()).is_empty());
}

/* ── машина хода: FINDING-кандидаты (кривые последовательности) ──────────── */

// Ревизия 84 (бывшая находка 4): двойной Approved гасится — вызов
// выдаётся на исполнение один раз.
#[test]
fn machine_double_approval_replays_same_call() {
    let (mut m, _) = start(always_cfg());
    let _ = m.on_stream(acc_with_calls(vec![
        call("c1", "read", serde_json::json!({})),
        call("c2", "read", serde_json::json!({})),
    ]));
    let first = m.on_approval(ApprovalDecision::Approved);
    let second = m.on_approval(ApprovalDecision::Approved);
    let replayed = [first, second]
        .iter()
        .filter(|s| matches!(s, TurnStep::ExecuteTool { call } if call.id == "c1"))
        .count();
    assert_eq!(
        replayed, 1,
        "двойной on_approval(Approved) обязан выдать c1 ровно один раз"
    );
}

// Ревизия 84 (бывшая находка 5): лишний on_tool_result гасится — c2
// не закрывается и не считается исполненным.
#[test]
fn machine_stray_tool_result_closes_next_unapproved_call() {
    let (mut m, _) = start(always_cfg());
    let _ = m.on_stream(acc_with_calls(vec![
        call("c1", "read", serde_json::json!({})),
        call("c2", "read", serde_json::json!({})),
    ]));
    assert!(matches!(
        m.on_approval(ApprovalDecision::Approved),
        TurnStep::ExecuteTool { .. }
    ));
    let _ = m.on_tool_result(ok_out("r1"));
    // Машина сейчас в AwaitApproval{c2}; кривой драйвер шлёт ещё результат.
    let step = m.on_tool_result(ok_out("r2"));
    let closed: Vec<&str> = m
        .history()
        .iter()
        .filter(|msg| msg.role == Role::Tool)
        .map(|msg| msg.tool_call_id.as_deref().unwrap_or(""))
        .collect();
    assert_eq!(
        closed,
        vec!["c1"],
        "лишний on_tool_result не имеет права закрывать c2: {closed:?}"
    );
    assert_eq!(
        m.report(TurnOutcome::Completed, 1).tool_calls,
        1,
        "c2 не посчитан исполненным"
    );
    assert!(
        matches!(step, TurnStep::Noop),
        "результат мимо диалога гасится: {step:?}"
    );
}

// Ревизия 84 (бывшая находка 6): on_tool_result в состоянии AwaitApproval
// не обходит подтверждение — вызов остаётся в диалоге.
#[test]
fn machine_tool_result_in_await_state_bypasses_approval() {
    let (mut m, _) = start(always_cfg());
    let step = m.on_stream(acc_with_calls(vec![call(
        "c1",
        "read",
        serde_json::json!({}),
    )]));
    assert!(matches!(step, TurnStep::AwaitApproval { .. }));
    // Кривой плагин/драйвер шлёт результат вместо решения человека.
    let step = m.on_tool_result(ok_out("прорвался"));
    assert!(
        matches!(step, TurnStep::Noop),
        "on_tool_result в AwaitApproval гасится, вызов ждёт решения: {step:?}"
    );
    assert_eq!(
        m.report(TurnOutcome::Failed, 1).tool_calls,
        0,
        "вызов не посчитан исполненным без подтверждения"
    );
    // Штатное закрытие после решения человека всё ещё работает.
    assert!(matches!(
        m.on_approval(ApprovalDecision::Approved),
        TurnStep::ExecuteTool { .. }
    ));
}

// Ревизия 84 (бывшая находка 7): ход терминален — после Finish история
// не растёт, тулзы не исполняются.
#[test]
fn machine_work_and_history_continue_after_finish() {
    let (mut m, _) = start(never_cfg());
    let step = m.on_stream(acc_text("готово"));
    assert!(is_finish(&step, TurnOutcome::Completed));
    assert_eq!(m.history().len(), 2);
    let step = m.on_stream(acc_text("ещё"));
    assert!(
        matches!(step, TurnStep::Noop),
        "после Finish ход терминален: {step:?}"
    );
    assert_eq!(
        m.history().len(),
        2,
        "история не растёт после Finish: {} сообщений",
        m.history().len()
    );
    // И исполнение тоже: тулз после завершённого хода.
    let (mut m2, _) = start(never_cfg());
    let _ = m2.on_stream(acc_text("done"));
    let step = m2.on_stream(acc_with_calls(vec![call(
        "c9",
        "read",
        serde_json::json!({}),
    )]));
    assert!(
        !matches!(step, TurnStep::ExecuteTool { .. }),
        "тулз не исполняется после Finish: {step:?}"
    );
}

// Ревизия 84 (бывшая находка 8): повторный Denied по пустому слоту —
// тишина, живой ход не флипается в Failed.
#[test]
fn machine_second_deny_flips_live_turn_to_failed() {
    let (mut m, _) = start(always_cfg());
    let _ = m.on_stream(acc_with_calls(vec![call(
        "c1",
        "read",
        serde_json::json!({}),
    )]));
    let step = m.on_approval(ApprovalDecision::Denied);
    assert!(matches!(step, TurnStep::RequestModel { .. }));
    let step = m.on_approval(ApprovalDecision::Denied);
    assert!(
        matches!(step, TurnStep::Noop),
        "второй Denied гасится, не убивая ход: {step:?}"
    );
}

// Ревизия 84 (бывшая находка 9): cancel фиксируется — после отмены
// on_approval(Approved) не исполняет вызов.
#[test]
fn machine_cancel_does_not_latch() {
    let (mut m, _) = start(always_cfg());
    let _ = m.on_stream(acc_with_calls(vec![call(
        "c1",
        "read",
        serde_json::json!({}),
    )]));
    let step = m.cancel();
    assert!(is_finish(&step, TurnOutcome::Cancelled));
    let step = m.on_approval(ApprovalDecision::Approved);
    assert!(
        matches!(step, TurnStep::Noop),
        "после cancel() вызовы не выдаются: {step:?}"
    );
}

// Ревизия 84 (бывшая находка 10): вторая волна стрима до закрытия первой
// гасится — инвариант «каждый tool_call закрыт» не рвётся.
#[test]
fn machine_new_stream_while_pending_leaves_call_unclosed() {
    let (mut m, _) = start(always_cfg());
    let _ = m.on_stream(acc_with_calls(vec![call(
        "c1",
        "read",
        serde_json::json!({}),
    )]));
    // Кривой ретрай: новый стрим до решения по c1.
    let step = m.on_stream(acc_with_calls(vec![call(
        "c2",
        "read",
        serde_json::json!({}),
    )]));
    assert!(
        matches!(step, TurnStep::Noop),
        "вторая волна до закрытия первой гасится: {step:?}"
    );
    // Штатный флоу для первой волны: c1 закрывается решением и результатом.
    let _ = m.on_approval(ApprovalDecision::Approved);
    let _ = m.on_tool_result(ok_out("r1"));
    let step = m.on_stream(acc_text("done"));
    assert!(is_finish(&step, TurnOutcome::Completed));
    let unclosed = unclosed_tool_calls(m.history());
    assert!(
        unclosed.is_empty(),
        "вызов c1 обязан быть закрыт tool-сообщением: {unclosed:?}"
    );
}

// Ревизия 84 (бывшая находка 11): max_iterations=0 не исполняет ни одного
// вызова; вызовы закрываются сообщениями, история валидна.
#[test]
fn machine_zero_iteration_limit_still_executes_batch() {
    let cfg = TurnConfig {
        max_iterations: 0,
        ..never_cfg()
    };
    let (mut m, _) = start(cfg);
    let step = m.on_stream(acc_with_calls(vec![call(
        "c1",
        "read",
        serde_json::json!({}),
    )]));
    assert!(
        is_finish(&step, TurnOutcome::IterationLimit),
        "max_iterations=0 = ни одного исполнения: {step:?}"
    );
    assert!(
        unclosed_tool_calls(m.history()).is_empty(),
        "вызовы лимита закрыты сообщениями"
    );
}

/* ── лимиты и объём ──────────────────────────────────────────────────────── */

#[test]
fn machine_iteration_limit_at_exactly_default_and_one_more() {
    // Ровно DEFAULT_MAX_ITERATIONS партий «стрим+результат»: после 24-й —
    // IterationLimit; с max=+1 — 25-я партия исполняется и лимит на 25-й.
    let cfg = TurnConfig {
        max_iterations: DEFAULT_MAX_ITERATIONS,
        ..never_cfg()
    };
    let (mut m, _) = start(cfg);
    for i in 0..DEFAULT_MAX_ITERATIONS {
        let step = m.on_stream(acc_with_calls(vec![call(
            &format!("c{i}"),
            "read",
            serde_json::json!({}),
        )]));
        assert!(
            matches!(step, TurnStep::ExecuteTool { .. }),
            "партия {}: {step:?}",
            i + 1
        );
        let step = m.on_tool_result(ok_out("x"));
        if i + 1 < DEFAULT_MAX_ITERATIONS {
            assert!(matches!(step, TurnStep::RequestModel { .. }), "{step:?}");
        } else {
            assert!(is_finish(&step, TurnOutcome::IterationLimit), "{step:?}");
        }
    }
    let cfg = TurnConfig {
        max_iterations: DEFAULT_MAX_ITERATIONS + 1,
        ..never_cfg()
    };
    let (mut m, _) = start(cfg);
    for i in 0..DEFAULT_MAX_ITERATIONS + 1 {
        let _ = m.on_stream(acc_with_calls(vec![call(
            &format!("c{i}"),
            "read",
            serde_json::json!({}),
        )]));
        let step = m.on_tool_result(ok_out("x"));
        if i + 1 < DEFAULT_MAX_ITERATIONS + 1 {
            assert!(matches!(step, TurnStep::RequestModel { .. }), "{step:?}");
        } else {
            assert!(is_finish(&step, TurnOutcome::IterationLimit), "{step:?}");
        }
    }
}

#[test]
fn machine_100_tool_calls_in_single_response_all_execute() {
    // 100 tool_calls за один ответ: все исполняются (одна «итерация»),
    // очередь доходит до конца, история валидна.
    let (mut m, _) = start(never_cfg());
    let calls: Vec<ToolCall> = (0..100)
        .map(|i| call(&format!("c{i}"), "read", serde_json::json!({})))
        .collect();
    let step = m.on_stream(acc_with_calls(calls));
    assert!(matches!(step, TurnStep::ExecuteTool { call } if call.id == "c0"));
    for i in 0..100 {
        let step = m.on_tool_result(ok_out("x"));
        if i < 99 {
            assert!(
                matches!(step, TurnStep::ExecuteTool { ref call } if call.id == format!("c{}", i + 1)),
                "шаг {i}: {step:?}"
            );
        }
    }
    let step = m.on_tool_result(ToolOutcome {
        ok: false,
        output: "хвостовой двойной результат".into(),
    });
    // 100-й результат уже закрыл очередь; лишний (ревизия 84) гасится
    // Noop: не Failed, не мутация.
    assert!(matches!(step, TurnStep::Noop), "{step:?}");
    let rep = m.report(TurnOutcome::IterationLimit, 1);
    assert_eq!(rep.tool_calls, 100);
    assert_eq!(m.history().len(), 102);
    assert!(unclosed_tool_calls(m.history()).is_empty());
    assert!(orphan_tool_messages(m.history()).is_empty());
}

#[test]
fn machine_duplicate_ids_close_in_order() {
    // Дубли id в одном ответе: машина закрывает их по порядку, по одному
    // tool-сообщению на вызов (счёт по количеству, не по уникальности id).
    let (mut m, _) = start(never_cfg());
    let calls = vec![
        call("dup", "read", serde_json::json!({})),
        call("dup", "read", serde_json::json!({})),
        call("dup", "read", serde_json::json!({})),
    ];
    let _ = m.on_stream(acc_with_calls(calls));
    for i in 0..3 {
        let step = m.on_tool_result(ok_out("r"));
        if i < 2 {
            assert!(
                matches!(step, TurnStep::ExecuteTool { .. }),
                "закрытие {i}: {step:?}"
            );
        } else {
            assert!(
                matches!(step, TurnStep::RequestModel { .. }),
                "очередь дубликатов исчерпана: {step:?}"
            );
        }
    }
    // Лишний результат по исчерпанной очереди (ревизия 84): Noop, история
    // не тронута.
    let step = m.on_tool_result(ok_out("r"));
    assert!(matches!(step, TurnStep::Noop), "{step:?}");
    let tools: Vec<&str> = m
        .history()
        .iter()
        .filter(|msg| msg.role == Role::Tool)
        .map(|msg| msg.tool_call_id.as_deref().unwrap_or(""))
        .collect();
    assert_eq!(tools, vec!["dup", "dup", "dup"]);
    assert!(unclosed_tool_calls(m.history()).is_empty());
    assert_eq!(m.report(TurnOutcome::Completed, 1).tool_calls, 3);
}

/* ── огромные строки и граница обрезки ───────────────────────────────────── */

#[test]
fn truncate_exact_limit_untouched_and_plus_one_cuts() {
    let exact = "a".repeat(MAX_TOOL_OUTPUT_BYTES);
    assert_eq!(truncate_output(&exact).len(), MAX_TOOL_OUTPUT_BYTES);
    let over = "a".repeat(MAX_TOOL_OUTPUT_BYTES + 1);
    let out = truncate_output(&over);
    assert!(out.contains("обрезано"), "маркер: {out}");
    assert!(out.len() <= MAX_TOOL_OUTPUT_BYTES + 64);
    assert!(out.starts_with('a') && out.ends_with('a'));
}

#[test]
fn truncate_multibyte_boundaries_no_panic_no_garbage() {
    // Граница реза внутри 2-байтового символа (я) и внутри 4-байтового (😀):
    // либо символ сохраняется целиком, либо теряется целиком — мусора в
    // байтах быть не должно (и паники на срезе — тоже).
    let two = format!("H{}T", "я".repeat(MAX_TOOL_OUTPUT_BYTES / 2 + 1));
    let out = truncate_output(&two);
    assert!(out.contains("обрезано"));
    assert!(out.starts_with('H'));
    assert!(out.ends_with('T'));
    let (head, rest) = out.split_once("\n\n[... обрезано").unwrap();
    let (_, tail) = rest.split_once(" байт ...]\n\n").unwrap();
    assert!(
        head.chars().all(|c| c == 'я' || c == 'H'),
        "голова без разорванных символов"
    );
    assert!(
        tail.chars().all(|c| c == 'я' || c == 'T'),
        "хвост без разорванных символов"
    );

    let four = format!("H{}T", "😀".repeat(MAX_TOOL_OUTPUT_BYTES / 4 + 1));
    let out = truncate_output(&four);
    assert!(out.contains("обрезано"));
    assert!(out.starts_with('H'));
    assert!(out.ends_with('T'));
    let (head, rest) = out.split_once("\n\n[... обрезано").unwrap();
    let (_, tail) = rest.split_once(" байт ...]\n\n").unwrap();
    assert!(
        head.chars().all(|c| c == '😀' || c == 'H'),
        "голова без разорванных символов"
    );
    assert!(
        tail.chars().all(|c| c == '😀' || c == 'T'),
        "хвост без разорванных символов"
    );
}

#[test]
fn machine_giant_content_reasoning_and_tool_output_absorbed() {
    // 10 МБ content + 5 МБ reasoning в стриме; 10 МБ вывод тулза обрезается.
    let (mut m, _) = start(never_cfg());
    let acc = StreamAccumulator {
        content: "x".repeat(10 * 1024 * 1024),
        reasoning: "r".repeat(5 * 1024 * 1024),
        ..Default::default()
    };
    let step = m.on_stream(acc);
    assert!(is_finish(&step, TurnOutcome::Completed));
    let rep = m.report(TurnOutcome::Completed, 1);
    assert_eq!(rep.content.len(), 10 * 1024 * 1024);
    assert_eq!(rep.reasoning.len(), 5 * 1024 * 1024);

    let (mut m2, _) = start(never_cfg());
    let _ = m2.on_stream(acc_with_calls(vec![call(
        "c1",
        "read",
        serde_json::json!({}),
    )]));
    let step = m2.on_tool_result(ok_out(&"y".repeat(10 * 1024 * 1024)));
    assert!(matches!(step, TurnStep::RequestModel { .. }));
    let tool_msg = m2
        .history()
        .iter()
        .find(|msg| msg.role == Role::Tool)
        .unwrap();
    assert!(tool_msg.content.len() <= MAX_TOOL_OUTPUT_BYTES + 64);
}

/* ── accumulate: кривые события ──────────────────────────────────────────── */

#[test]
fn accumulate_weird_events_are_ignored_cleanly() {
    let evs = vec![
        StreamEvent::Content("".into()),
        StreamEvent::Reasoning("".into()),
        StreamEvent::Error("".into()),
        StreamEvent::Error("провал".into()),
        StreamEvent::Usage {
            input_tokens: 0,
            output_tokens: 0,
        },
        StreamEvent::Usage {
            input_tokens: u32::MAX,
            output_tokens: u32::MAX,
        },
        StreamEvent::ToolCallStart {
            index: 0,
            id: "".into(),
            name: "".into(),
        },
        StreamEvent::Done {
            finish: FinishReason::Unknown,
        },
    ];
    assert_eq!(accumulate(&evs), StreamAccumulator::default());
}

#[test]
fn accumulate_tool_call_with_weird_argument_values() {
    // ToolCallComplete с arguments, где path — число: как есть в accumulator.
    let evs = vec![StreamEvent::ToolCallComplete(call_path(
        "c1",
        "read",
        serde_json::json!(123),
    ))];
    let acc = accumulate(&evs);
    assert_eq!(acc.tool_calls.len(), 1);
    assert_eq!(
        acc.tool_calls[0].arguments.get("path"),
        Some(&serde_json::json!(123))
    );
    assert_eq!(acc.awaiting_approval, vec!["c1".to_string()]);
}

/* ── describe_call: кривые аргументы ─────────────────────────────────────── */

#[test]
fn describe_call_control_characters_are_escaped() {
    let c = call(
        "c1",
        "bash",
        serde_json::json!({"command": "a\u{0}b\u{1b}c"}),
    );
    let d = describe_call(&c);
    assert!(d.starts_with("bash"), "{d:?}");
    assert!(d.contains("a\\u0000"), "NUL экранируется: {d:?}");
    assert!(d.chars().count() < 400);
}

#[test]
fn describe_call_unicode_cap_is_by_chars() {
    // 600+ юникод-символов: обрезка по chars (300), не по байтам —
    // разрыва суррогатных пар/многобайтных символов быть не может.
    let long = "é😀あ".repeat(200);
    let c = call("c1", "write", serde_json::json!({ "content": long }));
    let d = describe_call(&c);
    let body = d.strip_prefix("write ").unwrap_or(&d);
    assert!(
        body.chars().count() <= 300,
        "обрезка по chars: {}",
        body.chars().count()
    );
    assert!(!body.chars().any(|ch| ch == char::REPLACEMENT_CHARACTER));
}

#[test]
fn describe_call_huge_arguments_do_not_hang_or_overflow() {
    let mut args = serde_json::Map::new();
    for i in 0..10_000 {
        args.insert(format!("k{i}"), serde_json::json!(i));
    }
    let c = ToolCall {
        id: "c1".into(),
        name: "write".into(),
        arguments: args,
    };
    let d = describe_call(&c);
    assert!(d.chars().count() < 400, "{}", d.chars().count());

    let c2 = call(
        "c2",
        "write",
        serde_json::json!({ "content": deep_value(100) }),
    );
    let d2 = describe_call(&c2);
    assert!(d2.chars().count() < 400);
}

#[test]
fn describe_call_empty_arguments_and_empty_name() {
    let c = call("c1", "", serde_json::json!({}));
    let d = describe_call(&c);
    assert_eq!(d, " {}", "имя-пустышка: {d:?}");
}
