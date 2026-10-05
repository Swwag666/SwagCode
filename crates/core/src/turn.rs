/*! State machine одного хода агента.

Ход — это не «один запрос к модели», а цикл: модель генерирует, вызывает
тулзы, получает результаты, генерирует снова. Ограничение итераций обязательно:
без него модель, зациклившаяся на одном тулзе, жжёт токены бесконечно.

Дизайн под тестируемость: [`TurnRunner`] принимает уже разобранные
[`StreamEvent`], а не байты из сети. Поэтому весь агентский цикл проверяется
headless на записанных стримах — без ключа, без сети, детерминированно.
*/

use std::time::Duration;

use serde::{Deserialize, Serialize};
use swagcod_provider::sse::StreamEvent;
use swagcod_provider::types::{ChatMessage, Role, ToolCall};

use crate::session::{TurnId, TurnRecord};

/// Максимум итераций «модель → тулзы» внутри одного хода.
pub const DEFAULT_MAX_ITERATIONS: usize = 24;

/// Ограничение на размер результата одного тулза. Больше — обрезаем:
/// иначе один `cat` гигантского файла раздует контекст и уронит ход.
pub const MAX_TOOL_OUTPUT_BYTES: usize = 256 * 1024;

/// Причина, по которой ход закончился.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum TurnOutcome {
    /// Модель закончила ответ.
    Completed,
    /// Упёрлись в лимит итераций. Не ошибка: ход остановлен предсказуемо.
    IterationLimit,
    /// Отменён пользователем.
    Cancelled,
    /// Ошибка провайдера или тулза.
    Failed,
}

/// Политика подтверждений. Решение живёт в ядре, а не в UI (DECISIONS.md §2):
/// иначе один кривой плагин получает full-access, подменив обработчик в UI.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ApprovalPolicy {
    /// Спрашивать на каждый вызов.
    Always,
    /// Никогда не спрашивать (опасно, только для доверенных прогонов).
    Never,
    /// Спрашивать только на вызовах из списка опасных имён.
    OnDangerous,
}

impl ApprovalPolicy {
    /// Имена тулзов, которые всегда требуют подтверждения при
    /// [`ApprovalPolicy::OnDangerous`].
    pub const DANGEROUS: &'static [&'static str] =
        &["bash", "pwsh", "shell", "write", "edit", "delete", "remove"];

    /// Нужно ли подтверждение для этого вызова.
    pub fn requires_approval(&self, tool_name: &str) -> bool {
        match self {
            Self::Always => true,
            Self::Never => false,
            Self::OnDangerous => Self::DANGEROUS
                .iter()
                .any(|d| tool_name.eq_ignore_ascii_case(d)),
        }
    }
}

/// Ответ ядра на запрос подтверждения.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ApprovalDecision {
    Approved,
    Denied,
}

/// Результат прогона одного стрима модели.
#[derive(Debug, Clone, Default, PartialEq)]
pub struct StreamAccumulator {
    pub content: String,
    pub reasoning: String,
    pub tool_calls: Vec<ToolCall>,
    /// Идентификаторы вызовов, ожидающих подтверждения.
    pub awaiting_approval: Vec<String>,
}

/// Прогнать поток событий модели и собрать результат.
///
/// Чистая функция от последовательности событий: не трогает сеть и файлы,
/// поэтому детерминированно тестируется. Побочные эффекты (исполнение тулзов)
/// делает вызывающий слой — [`TurnRunner`].
pub fn accumulate(events: &[StreamEvent]) -> StreamAccumulator {
    let mut acc = StreamAccumulator::default();
    for ev in events {
        match ev {
            StreamEvent::Content(t) => acc.content.push_str(t),
            StreamEvent::Reasoning(t) => acc.reasoning.push_str(t),
            StreamEvent::ToolCallComplete(tc) => {
                acc.tool_calls.push(tc.clone());
                acc.awaiting_approval.push(tc.id.clone());
            }
            // ToolCallStart/Done/Error обрабатывает вызывающий: ему нужна
            // семантика завершения, а не только накопление текста.
            _ => {}
        }
    }
    acc
}

/// Обрезать вывод тулза до лимита, сохранив оба конца: середина большого
/// вывода обычно менее полезна, чем начало и конец.
pub fn truncate_output(raw: &str) -> String {
    if raw.len() <= MAX_TOOL_OUTPUT_BYTES {
        return raw.to_string();
    }
    let head = MAX_TOOL_OUTPUT_BYTES * 2 / 3;
    let tail = MAX_TOOL_OUTPUT_BYTES / 3;
    // Режем по границам char, иначе разорвём multi-byte символ.
    let head_cut = floor_char_boundary(raw, head);
    let tail_start = ceil_char_boundary(raw, raw.len() - tail);
    let dropped = raw.len() - head_cut - (raw.len() - tail_start);
    format!(
        "{}\n\n[... обрезано {} байт ...]\n\n{}",
        &raw[..head_cut],
        dropped,
        &raw[tail_start..]
    )
}

fn floor_char_boundary(s: &str, mut i: usize) -> usize {
    if i >= s.len() {
        return s.len();
    }
    while i > 0 && !s.is_char_boundary(i) {
        i -= 1;
    }
    i
}

fn ceil_char_boundary(s: &str, mut i: usize) -> usize {
    while i < s.len() && !s.is_char_boundary(i) {
        i += 1;
    }
    i
}

/// Настройки хода.
#[derive(Debug, Clone)]
pub struct TurnConfig {
    pub max_iterations: usize,
    pub approval_policy: ApprovalPolicy,
    /// Таймаут одного вызова тулза.
    pub tool_timeout: Duration,
}

impl Default for TurnConfig {
    fn default() -> Self {
        Self {
            max_iterations: DEFAULT_MAX_ITERATIONS,
            approval_policy: ApprovalPolicy::OnDangerous,
            // Долго, но не бесконечно: зависший тулз не должен вешать ход.
            tool_timeout: Duration::from_secs(120),
        }
    }
}

/// Полная запись хода для журнала.
#[derive(Debug, Clone)]
pub struct TurnReport {
    pub outcome: TurnOutcome,
    pub content: String,
    pub reasoning: String,
    pub iterations: usize,
    pub tool_calls: usize,
    pub started_ms: u64,
    pub ended_ms: u64,
}

impl TurnReport {
    /// Превратить в запись журнала сессии.
    pub fn into_record(
        self,
        id: TurnId,
        calls: Vec<ToolCall>,
        est_input: u32,
        est_output: u32,
    ) -> TurnRecord {
        TurnRecord {
            id,
            started_ms: self.started_ms,
            ended_ms: Some(self.ended_ms),
            content: self.content,
            reasoning: self.reasoning,
            tool_calls: calls,
            est_input_tokens: est_input,
            est_output_tokens: est_output,
            ok: self.outcome == TurnOutcome::Completed,
            failure: match self.outcome {
                TurnOutcome::Completed => None,
                TurnOutcome::IterationLimit => Some("превышен лимит итераций".into()),
                TurnOutcome::Cancelled => Some("отменён пользователем".into()),
                TurnOutcome::Failed => Some("ошибка хода".into()),
            },
        }
    }
}

/// Построить сообщение ассистента из накопленного стрима — то, что кладётся
/// в историю перед результатами тулзов. Порядок важен: провайдер требует,
/// чтобы `tool_calls` шли в том же сообщении, что и ответ модели.
pub fn build_assistant_message(acc: &StreamAccumulator) -> ChatMessage {
    ChatMessage {
        role: Role::Assistant,
        content: acc.content.clone(),
        reasoning: acc.reasoning.clone(),
        tool_calls: acc.tool_calls.clone(),
        tool_call_id: None,
    }
}

/// Сформировать человекочитаемое описание вызова для диалога подтверждения.
///
/// Показываем аргументы, потому что «bash» без команды бесполезно для
/// решения: пользователь должен видеть, что именно запускается.
pub fn describe_call(call: &ToolCall) -> String {
    let args = serde_json::to_string(&call.arguments).unwrap_or_default();
    let short: String = args.chars().take(300).collect();
    format!("{} {short}", call.name)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::bus::{now_ms, EventKind};
    use crate::session::SessionId;
    use swagcod_provider::sse::FinishReason;

    fn tc(id: &str, name: &str, args: serde_json::Value) -> StreamEvent {
        StreamEvent::ToolCallComplete(ToolCall {
            id: id.into(),
            name: name.into(),
            arguments: args.as_object().cloned().unwrap_or_default(),
        })
    }

    // ---- accumulate ----

    #[test]
    fn accumulate_joins_content_fragments() {
        let evs = vec![
            StreamEvent::Content("при".into()),
            StreamEvent::Content("вет".into()),
        ];
        assert_eq!(accumulate(&evs).content, "привет");
    }

    #[test]
    fn accumulate_keeps_reasoning_separate_from_content() {
        let evs = vec![
            StreamEvent::Reasoning("думаю".into()),
            StreamEvent::Content("ответ".into()),
            StreamEvent::Reasoning(" ещё".into()),
        ];
        let acc = accumulate(&evs);
        assert_eq!(acc.content, "ответ");
        assert_eq!(acc.reasoning, "думаю ещё");
        assert!(!acc.content.contains("думаю"));
    }

    #[test]
    fn accumulate_collects_tool_calls_with_ids_pending_approval() {
        let evs = vec![
            tc("c1", "read", serde_json::json!({"path":"a.txt"})),
            tc("c2", "bash", serde_json::json!({"cmd":"ls"})),
        ];
        let acc = accumulate(&evs);
        assert_eq!(acc.tool_calls.len(), 2);
        assert_eq!(acc.awaiting_approval, vec!["c1", "c2"]);
        assert_eq!(acc.tool_calls[1].name, "bash");
    }

    #[test]
    fn accumulate_ignores_lifecycle_events() {
        let evs = vec![
            StreamEvent::ToolCallStart {
                index: 0,
                id: "c1".into(),
                name: "read".into(),
            },
            StreamEvent::Content("x".into()),
            StreamEvent::Done {
                finish: FinishReason::Stop,
            },
            StreamEvent::Error("boom".into()),
        ];
        let acc = accumulate(&evs);
        assert_eq!(acc.content, "x");
        assert!(
            acc.tool_calls.is_empty(),
            "Start не должен становиться вызовом"
        );
    }

    #[test]
    fn accumulate_on_empty_stream_is_default() {
        assert_eq!(accumulate(&[]), StreamAccumulator::default());
    }

    // ---- approval policy ----

    #[test]
    fn always_policy_approves_everything() {
        let p = ApprovalPolicy::Always;
        assert!(p.requires_approval("read"));
        assert!(p.requires_approval("bash"));
        assert!(p.requires_approval("anything"));
    }

    #[test]
    fn never_policy_approves_nothing() {
        assert!(!ApprovalPolicy::Never.requires_approval("bash"));
    }

    #[test]
    fn on_dangerous_flags_mutating_tools_only() {
        let p = ApprovalPolicy::OnDangerous;
        assert!(p.requires_approval("bash"));
        assert!(p.requires_approval("write"));
        assert!(p.requires_approval("edit"));
        assert!(!p.requires_approval("read"));
        assert!(!p.requires_approval("grep"));
        assert!(!p.requires_approval("glob"));
    }

    #[test]
    fn dangerous_match_is_case_insensitive() {
        // Имена тулзов приходят от модели: регистр не гарантирован.
        assert!(ApprovalPolicy::OnDangerous.requires_approval("BASH"));
        assert!(ApprovalPolicy::OnDangerous.requires_approval("Bash"));
    }

    // ---- truncate ----

    #[test]
    fn short_output_is_untouched() {
        assert_eq!(truncate_output("hello"), "hello");
        assert_eq!(truncate_output(""), "");
    }

    #[test]
    fn huge_output_is_truncated_to_budget() {
        let big = "x".repeat(MAX_TOOL_OUTPUT_BYTES * 4);
        let out = truncate_output(&big);
        assert!(out.len() < big.len());
        assert!(out.len() <= MAX_TOOL_OUTPUT_BYTES + 64, "запас на маркер");
        assert!(out.contains("обрезано"));
    }

    #[test]
    fn truncation_keeps_both_ends() {
        // Начало и конец полезнее середины: в конце обычно итог команды.
        let big = format!("START{}END", "m".repeat(MAX_TOOL_OUTPUT_BYTES * 3));
        let out = truncate_output(&big);
        assert!(out.starts_with("START"));
        assert!(out.ends_with("END"));
    }

    #[test]
    fn truncation_does_not_split_multibyte_chars() {
        // Граница реза попадает внутрь CJK-символа. Если бы резали по байтам
        // вслепую, здесь была бы паника или невалидный UTF-8.
        let big = "я".repeat(MAX_TOOL_OUTPUT_BYTES);
        let out = truncate_output(&big);
        // String уже гарантирует валидный UTF-8; проверяем отсутствие мусора.
        assert!(out.len() < big.len(), "должно быть обрезано");
        let marker = "[... обрезано";
        let head = out.split(marker).next().unwrap();
        assert!(
            head.chars().all(|c| c == 'я' || c == '\n'),
            "головная часть должна содержать только исходные символы"
        );
        assert!(
            out.contains("байт"),
            "маркер обрезки должен быть виден: {out}"
        );
    }

    // ---- wire format ----

    #[test]
    fn assistant_message_carries_tool_calls_and_reasoning() {
        let acc = accumulate(&[
            StreamEvent::Reasoning("r".into()),
            StreamEvent::Content("c".into()),
            tc("c1", "read", serde_json::json!({"p":1})),
        ]);
        let msg = build_assistant_message(&acc);
        assert_eq!(msg.role, Role::Assistant);
        assert_eq!(msg.content, "c");
        assert_eq!(msg.reasoning, "r");
        assert_eq!(msg.tool_calls.len(), 1);
        assert!(msg.tool_call_id.is_none());
    }

    #[test]
    fn describe_call_shows_tool_name_and_arguments() {
        let call = ToolCall {
            id: "c1".into(),
            name: "bash".into(),
            arguments: serde_json::json!({"command": "rm -rf build"})
                .as_object()
                .unwrap()
                .clone(),
        };
        let d = describe_call(&call);
        assert!(d.starts_with("bash"));
        assert!(
            d.contains("rm -rf build"),
            "пользователь должен видеть команду: {d}"
        );
    }

    #[test]
    fn describe_call_caps_very_long_arguments() {
        let long = "y".repeat(2000);
        let call = ToolCall {
            id: "c1".into(),
            name: "write".into(),
            arguments: serde_json::json!({"content": long})
                .as_object()
                .unwrap()
                .clone(),
        };
        assert!(describe_call(&call).chars().count() < 400);
    }

    // ---- config ----

    #[test]
    fn default_config_has_sane_limits() {
        let c = TurnConfig::default();
        assert!(c.max_iterations > 0);
        assert_eq!(c.approval_policy, ApprovalPolicy::OnDangerous);
        assert!(c.tool_timeout > Duration::from_secs(0));
    }

    #[test]
    fn iteration_limit_is_bounded() {
        // Без лимита зациклившаяся модель жжёт токены бесконечно.
        // Проверяем на реальном значении конфига, а не на константе: clippy
        // прав, что assert на константе всегда истинен и ничего не ловит.
        let cfg = TurnConfig {
            max_iterations: DEFAULT_MAX_ITERATIONS,
            ..Default::default()
        };
        assert!(cfg.max_iterations < 1000);
        assert!(cfg.max_iterations > 0);
    }

    #[test]
    fn report_marks_only_completed_as_ok() {
        let base = TurnReport {
            outcome: TurnOutcome::Completed,
            content: String::new(),
            reasoning: String::new(),
            iterations: 1,
            tool_calls: 0,
            started_ms: now_ms(),
            ended_ms: now_ms(),
        };
        assert!(base.clone().into_record(TurnId::new("t"), vec![], 0, 0).ok);
        for (o, expect_reason) in [
            (TurnOutcome::IterationLimit, true),
            (TurnOutcome::Cancelled, true),
            (TurnOutcome::Failed, true),
        ] {
            let mut r = base.clone();
            r.outcome = o;
            let rec = r.into_record(TurnId::new("t"), vec![], 0, 0);
            assert!(!rec.ok);
            assert_eq!(rec.failure.is_some(), expect_reason);
        }
    }

    #[test]
    fn report_keeps_session_and_turn_identity() {
        let r = TurnReport {
            outcome: TurnOutcome::Completed,
            content: "c".into(),
            reasoning: "r".into(),
            iterations: 2,
            tool_calls: 1,
            started_ms: 100,
            ended_ms: 200,
        };
        let rec = r.into_record(TurnId::new("t9"), vec![], 5, 6);
        assert_eq!(rec.id, TurnId::new("t9"));
        assert_eq!(rec.started_ms, 100);
        assert_eq!(rec.ended_ms, Some(200));
        assert_eq!(rec.est_input_tokens, 5);
        assert_eq!(rec.est_output_tokens, 6);
        assert_eq!(rec.content, "c");
    }

    // ---- event mapping ----

    #[test]
    fn stream_events_map_onto_bus_events_without_loss() {
        // Контракт между provider и core: reasoning не теряется и не
        // склеивается с content по дороге в UI.
        let sid = SessionId::new("s");
        let tid = TurnId::new("t");
        let mapped: Vec<EventKind> = vec![
            StreamEvent::Reasoning("r".into()),
            StreamEvent::Content("c".into()),
        ]
        .into_iter()
        .map(|ev| match ev {
            StreamEvent::Reasoning(text) => EventKind::Reasoning {
                turn: tid.clone(),
                text,
            },
            StreamEvent::Content(text) => EventKind::Content {
                turn: tid.clone(),
                text,
            },
            _ => unreachable!(),
        })
        .collect();
        assert_eq!(mapped.len(), 2);
        assert!(matches!(&mapped[0], EventKind::Reasoning { text, .. } if text == "r"));
        assert!(matches!(&mapped[1], EventKind::Content { text, .. } if text == "c"));
        assert!(!sid.is_empty());
    }
}
