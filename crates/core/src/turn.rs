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
use swagcod_provider::types::{ChatMessage, Role, ToolCall, ToolSpec};

use crate::bus::now_ms;
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
        &["bash", "pwsh", "shell", "write", "edit", "delete", "remove", "patch"];

    /// Встроенные имена тулзов (B-5). Всё, что не здесь, — плагин:
    /// внешняя команда не должна исполняться молча, поэтому при
    /// [`ApprovalPolicy::OnDangerous`] неизвестное имя считается опасным.
    /// E-2: `semantic_search` — встроенный и read-only, в опасные не входит.
    /// E-6: `subagent` — встроенный: внутри суб-агента только read-only
    /// инструменты, опасных операций ветка не выполняет.
    pub const BUILTIN: &'static [&'static str] = &[
        "read",
        "list",
        "write",
        "bash",
        "memory_append",
        "grep",
        "glob",
        "patch",
        "fetch_url",
        "semantic_search",
        "subagent",
    ];

    /// Нужно ли подтверждение для этого вызова.
    pub fn requires_approval(&self, tool_name: &str) -> bool {
        match self {
            Self::Always => true,
            Self::Never => false,
            Self::OnDangerous => {
                Self::DANGEROUS
                    .iter()
                    .any(|d| tool_name.eq_ignore_ascii_case(d))
                    || !Self::BUILTIN
                        .iter()
                        .any(|b| tool_name.eq_ignore_ascii_case(b))
            }
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
            // E-6: машина ведёт ходы верхнего уровня; дочерние записи
            // суб-агентов драйвер строит сам с parent_turn_id.
            parent_turn_id: None,
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

/* ── Цикл хода: чистая state machine ───────────────────────────────────────
 * [`TurnMachine`] — тот самый [`TurnRunner`]-цикл из шапки модуля, но без
 * сети и файлов: она принимает накопленный стрим и результаты тулзов и
 * говорит драйверу (crates/app), какое действие выполнить следующим.
 * Драйвер поэтому тонкий, а весь цикл прогоняется headless на записанных
 * стримах: лимит итераций, approval и история проверяются без ключа. */

/// Результат выполнения одного вызова инструмента.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ToolOutcome {
    pub ok: bool,
    pub output: String,
}

/// Следующее действие для драйвера цикла.
#[derive(Debug, Clone, PartialEq)]
pub enum TurnStep {
    /// Отправить `history` модели и стримить ответ до конца.
    RequestModel { history: Vec<ChatMessage> },
    /// Вызов требует решения человека: драйвер показывает диалог и вернёт
    /// ответ через [`TurnMachine::on_approval`].
    AwaitApproval { call: ToolCall },
    /// Выполнить вызов и вернуть результат через [`TurnMachine::on_tool_result`].
    ExecuteTool { call: ToolCall },
    /// Ход закончен; отчёт берётся [`TurnMachine::report`].
    Finish { outcome: TurnOutcome },
}

/// State machine одного хода: история + очередь вызовов текущего ответа.
///
/// Инвариант: каждое сообщение ассистента с `tool_calls` в истории всегда
/// закрыто сообщениями `tool` по каждому вызову — иначе OpenAI-совместимый
/// сервер отклонит следующий запрос. Отклонённый вызов тоже закрывается
/// сообщением `tool` с текстом отказа.
#[derive(Debug)]
pub struct TurnMachine {
    config: TurnConfig,
    history: Vec<ChatMessage>,
    iterations: usize,
    executed_calls: usize,
    /// Вызовы последнего ответа модели, которые ещё не обработаны.
    pending: Vec<ToolCall>,
    content: String,
    reasoning: String,
    started_ms: u64,
}

impl TurnMachine {
    /// Начать ход. Первый шаг всегда — запрос модели.
    pub fn new(config: TurnConfig, history: Vec<ChatMessage>) -> (Self, TurnStep) {
        let machine = Self {
            config,
            history,
            iterations: 0,
            executed_calls: 0,
            pending: Vec::new(),
            content: String::new(),
            reasoning: String::new(),
            started_ms: now_ms(),
        };
        let step = machine.request_step();
        (machine, step)
    }

    fn request_step(&self) -> TurnStep {
        TurnStep::RequestModel {
            history: self.history.clone(),
        }
    }

    /// Стрим текущего запроса закончился, `acc` — накопленный ответ.
    pub fn on_stream(&mut self, acc: StreamAccumulator) -> TurnStep {
        self.iterations += 1;
        push_joined(&mut self.content, &acc.content);
        push_joined(&mut self.reasoning, &acc.reasoning);
        self.history.push(build_assistant_message(&acc));

        // Модель закончила сама: вызовов нет — ход завершён.
        if acc.tool_calls.is_empty() {
            self.pending.clear();
            return TurnStep::Finish {
                outcome: TurnOutcome::Completed,
            };
        }
        self.pending = acc.tool_calls;
        self.next_call_step()
    }

    /// Решение человека по вызову, стоящему первым в очереди.
    pub fn on_approval(&mut self, decision: ApprovalDecision) -> TurnStep {
        let Some(call) = self.pending.first().cloned() else {
            return TurnStep::Finish {
                outcome: TurnOutcome::Failed,
            };
        };
        match decision {
            ApprovalDecision::Approved => TurnStep::ExecuteTool { call },
            ApprovalDecision::Denied => {
                self.close_call(
                    &call,
                    ToolOutcome {
                        ok: false,
                        output: "вызов отклонён пользователем".into(),
                    },
                );
                self.after_queue()
            }
        }
    }

    /// Результат выполнения вызова, стоявшего первым в очереди.
    pub fn on_tool_result(&mut self, outcome: ToolOutcome) -> TurnStep {
        let Some(call) = self.pending.first().cloned() else {
            return TurnStep::Finish {
                outcome: TurnOutcome::Failed,
            };
        };
        self.executed_calls += 1;
        self.close_call(&call, outcome);
        self.after_queue()
    }

    /// Прервать ход снаружи (кнопка «стоп»): драйвер больше не спрашивает шагов.
    pub fn cancel(&self) -> TurnStep {
        TurnStep::Finish {
            outcome: TurnOutcome::Cancelled,
        }
    }

    /// Отчёт о ходе для журнала сессии.
    pub fn report(&self, outcome: TurnOutcome, ended_ms: u64) -> TurnReport {
        TurnReport {
            outcome,
            content: self.content.clone(),
            reasoning: self.reasoning.clone(),
            iterations: self.iterations,
            tool_calls: self.executed_calls,
            started_ms: self.started_ms,
            ended_ms,
        }
    }

    /// История на текущий момент: драйвер сохраняет её в сессию после хода.
    pub fn history(&self) -> &[ChatMessage] {
        &self.history
    }

    fn close_call(&mut self, call: &ToolCall, outcome: ToolOutcome) {
        self.pending.remove(0);
        self.history.push(ChatMessage {
            role: Role::Tool,
            // Большой вывод режем здесь, а не в исполнителе: лимит — свойство
            // контекста, а не конкретного тулза.
            content: truncate_output(&outcome.output),
            reasoning: String::new(),
            tool_calls: Vec::new(),
            tool_call_id: Some(call.id.clone()),
        });
    }

    /// Очередь опустела: либо следующий вызов, либо новый запрос модели.
    fn after_queue(&mut self) -> TurnStep {
        if !self.pending.is_empty() {
            return self.next_call_step();
        }
        if self.iterations >= self.config.max_iterations {
            // Модель зациклилась: останавливаем предсказуемо, не ошибкой.
            return TurnStep::Finish {
                outcome: TurnOutcome::IterationLimit,
            };
        }
        self.request_step()
    }

    fn next_call_step(&self) -> TurnStep {
        let Some(call) = self.pending.first().cloned() else {
            // Очередь пуста: решение примет after_queue/on_stream.
            return TurnStep::Finish {
                outcome: TurnOutcome::Completed,
            };
        };
        if self.config.approval_policy.requires_approval(&call.name) {
            TurnStep::AwaitApproval { call }
        } else {
            TurnStep::ExecuteTool { call }
        }
    }
}

fn push_joined(dst: &mut String, part: &str) {
    if part.is_empty() {
        return;
    }
    if !dst.is_empty() {
        dst.push_str("\n\n");
    }
    dst.push_str(part);
}

/// Встроенные инструменты агента: имена совпадают со списком
/// [`ApprovalPolicy::DANGEROUS`] там, где вызов меняет систему.
pub fn builtin_tool_specs() -> Vec<ToolSpec> {
    let path_only = serde_json::json!({
        "type": "object",
        "properties": { "path": { "type": "string", "description": "Path inside the session working directory" } },
        "required": ["path"]
    });
    vec![
        ToolSpec {
            name: "read".into(),
            description: "Read a text file inside the session working directory. Returns its content.".into(),
            parameters: path_only.clone(),
        },
        ToolSpec {
            name: "list".into(),
            description: "List directory entries inside the session working directory.".into(),
            parameters: path_only,
        },
        ToolSpec {
            name: "write".into(),
            description: "Create or overwrite a text file inside the session working directory.".into(),
            parameters: serde_json::json!({
                "type": "object",
                "properties": {
                    "path": { "type": "string" },
                    "content": { "type": "string" }
                },
                "required": ["path", "content"]
            }),
        },
        ToolSpec {
            name: "bash".into(),
            description: "Run a shell command with the session working directory as cwd. Returns stdout, stderr and exit code.".into(),
            parameters: serde_json::json!({
                "type": "object",
                "properties": { "command": { "type": "string" } },
                "required": ["command"]
            }),
        },
        ToolSpec {
            name: "memory_append".into(),
            description: "Append a durable note to the project memory file .swagcod/MEMORY.md inside the session working directory. Use it for facts that must survive context compaction: project conventions, decided approaches, important file locations.".into(),
            parameters: serde_json::json!({
                "type": "object",
                "properties": { "text": { "type": "string", "description": "Note text, markdown" } },
                "required": ["text"]
            }),
        },
        /* B-5: grep/glob/patch/fetch_url. */
        ToolSpec {
            name: "grep".into(),
            description: "Search file contents in the session working directory with a regex (Rust syntax). Returns matching lines with numbers and surrounding context. Respects .gitignore.".into(),
            parameters: serde_json::json!({
                "type": "object",
                "properties": {
                    "pattern": { "type": "string", "description": "Regular expression" },
                    "path": { "type": "string", "description": "Optional subdirectory to search in" },
                    "context": { "type": "integer", "description": "Context lines around a match, 0..10, default 2" }
                },
                "required": ["pattern"]
            }),
        },
        ToolSpec {
            name: "glob".into(),
            description: "Find files in the session working directory by glob pattern (e.g. \"src/**/*.rs\", \"*.toml\"). A pattern without \"/\" matches basenames at any depth. Respects .gitignore.".into(),
            parameters: serde_json::json!({
                "type": "object",
                "properties": {
                    "pattern": { "type": "string", "description": "Glob pattern" },
                    "path": { "type": "string", "description": "Optional subdirectory to search in" }
                },
                "required": ["pattern"]
            }),
        },
        ToolSpec {
            name: "patch".into(),
            description: "Replace an exact literal block in a text file inside the session working directory. old_string must appear exactly once unless replace_all is true. Whitespace-only drift in old_string is tolerated.".into(),
            parameters: serde_json::json!({
                "type": "object",
                "properties": {
                    "path": { "type": "string" },
                    "old_string": { "type": "string", "description": "Literal text to replace" },
                    "new_string": { "type": "string", "description": "Replacement text" },
                    "replace_all": { "type": "boolean", "description": "Replace every occurrence, default false" }
                },
                "required": ["path", "old_string", "new_string"]
            }),
        },
        ToolSpec {
            name: "fetch_url".into(),
            description: "Fetch an http(s) URL and return its content as text. HTML is reduced to readable text (scripts and styles removed). Response body capped at 2 MB.".into(),
            parameters: serde_json::json!({
                "type": "object",
                "properties": { "url": { "type": "string", "description": "http(s) URL" } },
                "required": ["url"]
            }),
        },
        /* E-2: семантический поиск по кодовой базе. Read-only, поэтому вне
           списка опасных; индекс строится фоном при старте хода. */
        ToolSpec {
            name: "semantic_search".into(),
            description: "Semantic search over the workspace code: finds meaning, not exact strings (e.g. \"where payments are processed\"). Returns top matching fragments with paths, line ranges and similarity scores. The embedding index builds in the background; if it is still empty the tool says so — retry a bit later.".into(),
            parameters: serde_json::json!({
                "type": "object",
                "properties": {
                    "query": { "type": "string", "description": "Natural-language query" },
                    "top_k": { "type": "integer", "description": "Max fragments to return, 1..50, default 8" }
                },
                "required": ["query"]
            }),
        },
        /* E-6: суб-агент — изолированная ветка с собственным контекстом,
           read-only инструментами и бюджетом токенов. Полный прогон
           записывается дочерним ходом (parent_turn_id) и виден в
           траектории сессии. Не рекурсивен: суб-агент не плодит своих. */
        ToolSpec {
            name: "subagent".into(),
            description: "Run an isolated sub-agent for a self-contained research or analysis task. The sub-agent gets a fresh context (optionally seeded with the session summary), read-only tools only (read, list, grep, glob, fetch_url, semantic_search), its own token budget and round cap. Its full run is recorded as a child turn of the current turn, and its final report is returned here. Use for focused independent investigation that should not pollute the main context. It cannot modify files or spawn sub-agents of its own. Pass a cheaper model (e.g. a flash/mini one from the active provider) for bulk research; omit model to reuse the parent turn's model.".into(),
            parameters: serde_json::json!({
                "type": "object",
                "properties": {
                    "prompt": { "type": "string", "description": "Self-contained task for the sub-agent (it sees no other context by default)" },
                    "context": { "type": "boolean", "description": "Seed the sub-agent with the session summary (B-3), default false" },
                    "max_rounds": { "type": "integer", "description": "Tool-round cap 1..12, default 6" },
                    "model": { "type": "string", "description": "Model for the sub-agent run (must exist on the active provider); empty or omitted = parent turn's model" }
                },
                "required": ["prompt"]
            }),
        },
    ]
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

    /// E-2: semantic_search — встроенный read-only инструмент: при
    /// OnDangerous подтверждения не требует и в спеках модели присутствует.
    #[test]
    fn semantic_search_is_builtin_and_safe() {
        assert!(!ApprovalPolicy::OnDangerous.requires_approval("semantic_search"));
        assert!(builtin_tool_specs().iter().any(|s| s.name == "semantic_search"));
        let spec = builtin_tool_specs()
            .into_iter()
            .find(|s| s.name == "semantic_search")
            .unwrap();
        assert_eq!(spec.parameters["required"][0], "query");
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

    // ---- turn machine: весь цикл хода headless, без сети ----

    fn acc_text(text: &str) -> StreamAccumulator {
        StreamAccumulator {
            content: text.into(),
            ..Default::default()
        }
    }

    fn acc_tool(id: &str, name: &str) -> StreamAccumulator {
        StreamAccumulator {
            tool_calls: vec![ToolCall {
                id: id.into(),
                name: name.into(),
                arguments: Default::default(),
            }],
            ..Default::default()
        }
    }

    fn never_cfg() -> TurnConfig {
        TurnConfig {
            approval_policy: ApprovalPolicy::Never,
            ..Default::default()
        }
    }

    #[test]
    fn machine_finishes_completed_when_model_stops() {
        let (mut m, step) = TurnMachine::new(TurnConfig::default(), vec![ChatMessage::user("hi")]);
        assert!(matches!(step, TurnStep::RequestModel { ref history } if history.len() == 1));
        let step = m.on_stream(acc_text("привет"));
        assert_eq!(step, TurnStep::Finish { outcome: TurnOutcome::Completed });
        let rep = m.report(TurnOutcome::Completed, 1);
        assert_eq!(rep.content, "привет");
        assert_eq!(rep.iterations, 1);
        // user + assistant
        assert_eq!(m.history().len(), 2);
    }

    #[test]
    fn machine_closes_tool_call_with_tool_message_and_loops() {
        let (mut m, _) = TurnMachine::new(never_cfg(), vec![ChatMessage::user("go")]);
        let call = ToolCall {
            id: "c1".into(),
            name: "read".into(),
            arguments: Default::default(),
        };
        let step = m.on_stream(acc_tool("c1", "read"));
        assert_eq!(step, TurnStep::ExecuteTool { call });
        let step = m.on_tool_result(ToolOutcome {
            ok: true,
            output: "file body".into(),
        });
        match step {
            TurnStep::RequestModel { history } => {
                assert_eq!(history.len(), 3);
                assert_eq!(history[2].role, Role::Tool);
                assert_eq!(history[2].tool_call_id.as_deref(), Some("c1"));
                assert_eq!(history[2].content, "file body");
            }
            other => panic!("ожидали RequestModel, получили {other:?}"),
        }
        let step = m.on_stream(acc_text("done"));
        assert_eq!(step, TurnStep::Finish { outcome: TurnOutcome::Completed });
        let rep = m.report(TurnOutcome::Completed, 2);
        assert_eq!(rep.iterations, 2);
        assert_eq!(rep.tool_calls, 1);
    }

    #[test]
    fn machine_gates_dangerous_call_behind_approval() {
        let cfg = TurnConfig {
            approval_policy: ApprovalPolicy::OnDangerous,
            ..Default::default()
        };
        let (mut m, _) = TurnMachine::new(cfg, vec![ChatMessage::user("go")]);
        let step = m.on_stream(acc_tool("c1", "bash"));
        assert!(
            matches!(step, TurnStep::AwaitApproval { .. }),
            "bash обязан уйти на подтверждение: {step:?}"
        );
        let step = m.on_approval(ApprovalDecision::Denied);
        match step {
            TurnStep::RequestModel { history } => {
                assert_eq!(history[2].role, Role::Tool);
                assert!(
                    history[2].content.contains("отклонён"),
                    "модель должна увидеть отказ: {}",
                    history[2].content
                );
            }
            other => panic!("после отказа ждём новый запрос модели: {other:?}"),
        }
    }

    #[test]
    fn machine_approved_call_executes() {
        let cfg = TurnConfig {
            approval_policy: ApprovalPolicy::Always,
            ..Default::default()
        };
        let (mut m, _) = TurnMachine::new(cfg, vec![ChatMessage::user("go")]);
        let _ = m.on_stream(acc_tool("c1", "read"));
        let step = m.on_approval(ApprovalDecision::Approved);
        assert!(matches!(step, TurnStep::ExecuteTool { .. }));
    }

    #[test]
    fn machine_stops_looping_at_iteration_limit() {
        let cfg = TurnConfig {
            max_iterations: 2,
            ..never_cfg()
        };
        let (mut m, _) = TurnMachine::new(cfg, vec![ChatMessage::user("go")]);
        let step = m.on_stream(acc_tool("c1", "read"));
        assert!(matches!(step, TurnStep::ExecuteTool { .. }));
        let step = m.on_tool_result(ToolOutcome {
            ok: true,
            output: "x".into(),
        });
        assert!(
            matches!(step, TurnStep::RequestModel { .. }),
            "лимит ещё не исчерпан: {step:?}"
        );
        let step = m.on_stream(acc_tool("c2", "read"));
        assert!(matches!(step, TurnStep::ExecuteTool { .. }));
        let step = m.on_tool_result(ToolOutcome {
            ok: true,
            output: "x".into(),
        });
        assert_eq!(step, TurnStep::Finish { outcome: TurnOutcome::IterationLimit });
    }

    #[test]
    fn machine_truncates_huge_tool_output_before_history() {
        let (mut m, _) = TurnMachine::new(never_cfg(), vec![ChatMessage::user("go")]);
        let _ = m.on_stream(acc_tool("c1", "read"));
        let big = "z".repeat(MAX_TOOL_OUTPUT_BYTES * 3);
        let _ = m.on_tool_result(ToolOutcome { ok: true, output: big });
        let tool_msg = m.history().iter().find(|m| m.role == Role::Tool).unwrap();
        assert!(
            tool_msg.content.len() <= MAX_TOOL_OUTPUT_BYTES + 64,
            "вывод тулза не должен раздувать контекст"
        );
    }

    #[test]
    fn builtin_specs_match_approval_dangerous_list() {
        let p = ApprovalPolicy::OnDangerous;
        for spec in builtin_tool_specs() {
            let expected = matches!(spec.name.as_str(), "write" | "bash" | "patch");
            assert_eq!(
                p.requires_approval(&spec.name),
                expected,
                "тулз {} должен соответствовать списку опасных",
                spec.name
            );
            assert!(spec.parameters.get("properties").is_some());
        }
    }

    #[test]
    fn unknown_tool_is_dangerous_by_default() {
        // B-5: плагин — внешняя команда, молча исполняться не должен.
        let p = ApprovalPolicy::OnDangerous;
        assert!(p.requires_approval("my_plugin_tool"));
        assert!(!p.requires_approval("fetch_url"));
        assert!(!p.requires_approval("MEMORY_APPEND"));
    }

    #[test]
    fn subagent_is_builtin_and_safe() {
        /* E-6: внутри ветки только read-only инструменты, поэтому сам
           `subagent` не опасен и подтверждений не требует. */
        assert!(!ApprovalPolicy::OnDangerous.requires_approval("subagent"));
        let spec = builtin_tool_specs()
            .into_iter()
            .find(|s| s.name == "subagent")
            .expect("subagent есть в спеках");
        assert!(spec.parameters.get("properties").is_some());
        let required = spec.parameters["required"].as_array().unwrap();
        assert!(required.iter().any(|v| v == "prompt"));
        // Модель ветки — опциональный аргумент (по умолчанию модель родителя).
        let props = spec.parameters["properties"].as_object().unwrap();
        assert!(props.contains_key("model"));
        assert!(!required.iter().any(|v| v == "model"));
    }
}
