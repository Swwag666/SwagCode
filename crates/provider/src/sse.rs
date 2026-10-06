/*! Инкрементальный SSE-парсер для `chat/completions`.

Дизайн продиктован бюджетом производительности (DECISIONS.md §2): парсер
работает на произвольных кусках байтов, не требует полного кадра в памяти и
**не порождает событий на пустых дельтах** — иначе на длинном ходе получим
тысячи бесполезных апдейтов DOM.

Состояние тулзов аккумулируется здесь, а не в UI, потому что `arguments`
приходит кусками невалидного JSON и собрать их можно только по завершении.
*/

use serde::Deserialize;

use crate::types::ToolCall;

/// Событие, которое парсер отдаёт наверх. UI подписывается ровно на это.
#[derive(Debug, Clone, PartialEq)]
pub enum StreamEvent {
    /// Кусок рассуждений. Отдельный визуальный канал (находка 1).
    Reasoning(String),
    /// Кусок ответа. Пустые дельты сюда не попадают (находка 2).
    Content(String),
    /// Модель начала вызывать инструмент: известны id и имя.
    ToolCallStart {
        index: usize,
        id: String,
        name: String,
    },
    /// Аргументы вызова собраны и распарсены. Только после finish_reason.
    ToolCallComplete(ToolCall),
    /// Ход завершён.
    Done { finish: FinishReason },
    /// Провайдер сообщил об ошибке внутри HTTP 200-ответа.
    Error(String),
    /// B-3: провайдер отдал точный счёт токенов (`usage` в чанке).
    /// Наш живой эндпоинт шлёт `usage: null` — тогда события просто нет,
    /// и ядро считает токены калиброванной оценкой.
    Usage {
        input_tokens: u32,
        output_tokens: u32,
    },
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum FinishReason {
    Stop,
    ToolCalls,
    Length,
    ContentFilter,
    /// Провайдер прислал неизвестное значение — не роняемся, а фиксируем.
    Unknown,
}

impl FinishReason {
    fn parse(s: &str) -> Self {
        match s {
            "stop" => Self::Stop,
            "tool_calls" => Self::ToolCalls,
            "length" => Self::Length,
            "content_filter" => Self::ContentFilter,
            _ => Self::Unknown,
        }
    }
}

/// Wire-формат чанка. Все поля опциональны: провайдеры присылают разное,
/// и падать на неожиданном поле нельзя — это уронило бы весь ход.
#[derive(Debug, Deserialize)]
struct Chunk {
    #[serde(default)]
    choices: Vec<Choice>,
    #[serde(default)]
    error: Option<ErrorBody>,
    #[serde(default)]
    usage: Option<UsageBody>,
}

#[derive(Debug, Deserialize)]
struct UsageBody {
    #[serde(default)]
    prompt_tokens: Option<u64>,
    #[serde(default)]
    completion_tokens: Option<u64>,
    #[serde(default)]
    total_tokens: Option<u64>,
}

#[derive(Debug, Deserialize)]
struct Choice {
    #[serde(default)]
    delta: Option<Delta>,
    #[serde(default)]
    finish_reason: Option<String>,
}

#[derive(Debug, Deserialize)]
struct Delta {
    #[serde(default)]
    content: Option<String>,
    #[serde(default)]
    reasoning_content: Option<String>,
    #[serde(default)]
    tool_calls: Option<Vec<ToolDelta>>,
    /// Присутствует в первом чанке. Намеренно не используется: роль сообщения
    /// назначает ядро, а не парсер потока. Поле объявлено, чтобы serde не
    /// спотыкался и чтобы факт игнорирования был виден в коде.
    #[serde(default)]
    #[allow(dead_code)]
    role: Option<String>,
}

#[derive(Debug, Deserialize)]
struct ToolDelta {
    /// Индекс вызова внутри ответа: модель может вызвать несколько тулзов.
    #[serde(default)]
    index: usize,
    #[serde(default)]
    id: Option<String>,
    #[serde(default)]
    function: Option<FnDelta>,
}

#[derive(Debug, Deserialize)]
struct FnDelta {
    #[serde(default)]
    name: Option<String>,
    #[serde(default)]
    arguments: Option<String>,
}

#[derive(Debug, Deserialize)]
struct ErrorBody {
    #[serde(default)]
    message: Option<String>,
}

/// Превратить wire-usage в событие. Нули и null событием не становятся:
/// вызывающий отличает «нет данных» от «ноль токенов».
fn usage_events(u: Option<&UsageBody>) -> Vec<StreamEvent> {
    let Some(u) = u else { return Vec::new() };
    let input = u.prompt_tokens.unwrap_or(0);
    let output = u.completion_tokens.unwrap_or(u.total_tokens.unwrap_or(0));
    if input == 0 && output == 0 {
        return Vec::new();
    }
    vec![StreamEvent::Usage {
        input_tokens: input.min(u32::MAX as u64) as u32,
        output_tokens: output.min(u32::MAX as u64) as u32,
    }]
}

/// Черновик вызова инструмента, пока аргументы доходят кусками.
#[derive(Debug, Default)]
struct PendingTool {
    id: String,
    name: String,
    args: String,
}

/// Потоковый парсер SSE. Кормим [`SseParser::feed`] кусками байтов любой
/// granularity, получаем готовые события.
#[derive(Debug, Default)]
pub struct SseParser {
    /// Недочитанная строка: кадр может разорваться между чанками сети.
    buf: String,
    /// Неполный UTF-8 символ на конце последнего чанка. Держим байтами и
    /// доклеиваем к следующему чанку: выбросить их означало бы испортить
    /// текст на любом multi-byte символе, разорванном сетью.
    pending_bytes: Vec<u8>,
    pending_tools: Vec<(usize, PendingTool)>,
    finished: bool,
}

impl SseParser {
    pub fn new() -> Self {
        Self::default()
    }

    /// Завершён ли поток (получен `finish_reason` или `[DONE]`).
    pub fn is_finished(&self) -> bool {
        self.finished
    }

    /// Обработать кусок байтов. Возвращает события в порядке появления.
    ///
    /// Никогда не паникует на битых данных: невалидный кадр пропускается,
    /// потому что один кривой чанк не должен ронять ход длиной в минуту.
    pub fn feed(&mut self, bytes: &[u8]) -> Vec<StreamEvent> {
        // Дописываем хвост прошлого чанка, если символ был разорван.
        let owned;
        let slice: &[u8] = if self.pending_bytes.is_empty() {
            bytes
        } else {
            self.pending_bytes.extend_from_slice(bytes);
            owned = std::mem::take(&mut self.pending_bytes);
            &owned
        };

        match std::str::from_utf8(slice) {
            Ok(text) => self.buf.push_str(text),
            Err(e) => {
                // Всё до границы символа — валидно, остаток ждёт следующий чанк.
                let up_to = e.valid_up_to();
                if up_to > 0 {
                    if let Ok(text) = std::str::from_utf8(&slice[..up_to]) {
                        self.buf.push_str(text);
                    }
                }
                self.pending_bytes.extend_from_slice(&slice[up_to..]);
            }
        }

        let mut out = Vec::new();
        // Обрабатываем только полные строки; неполный хвост остаётся в buf.
        while let Some(nl) = self.buf.find('\n') {
            let line: String = self.buf.drain(..=nl).collect();
            out.extend(self.handle_line(line.trim_end_matches(['\n', '\r'])));
        }
        out
    }

    /// Дожать поток, если он оборвался без `[DONE]`.
    pub fn finish(&mut self) -> Vec<StreamEvent> {
        let mut out = Vec::new();
        if !self.buf.trim().is_empty() {
            let line = std::mem::take(&mut self.buf);
            out.extend(self.handle_line(line.trim_end_matches(['\n', '\r'])));
        }
        if !self.finished {
            // Поток оборвался: отдаём накопленные тулзы, иначе вызов потеряется.
            out.extend(self.flush_tools());
            self.finished = true;
        }
        out
    }

    fn handle_line(&mut self, line: &str) -> Vec<StreamEvent> {
        // Поток завершён: контент после [DONE]/finish не принимаем — без этой
        // проверки поздние кадры (дубли, мусор от прокси) породили бы призрачные
        // события и второй ответ в уже закрытом ходе. Исключение — счёт токенов:
        // провайдеры с include_usage шлют usage последним кадром, и терять
        // точные числа из-за порядка кадров обидно (B-3).
        if self.finished {
            let data = match line.strip_prefix("data:") {
                Some(d) => d.trim(),
                None => return Vec::new(),
            };
            if data.is_empty() || data == "[DONE]" {
                return Vec::new();
            }
            return match serde_json::from_str::<Chunk>(data) {
                Ok(chunk) => usage_events(chunk.usage.as_ref()),
                Err(_) => Vec::new(),
            };
        }
        // SSE-комментарии и пустые строки-разделители — не данные.
        let data = match line.strip_prefix("data:") {
            Some(d) => d.trim(),
            None => return Vec::new(),
        };
        if data.is_empty() {
            return Vec::new();
        }
        if data == "[DONE]" {
            self.finished = true;
            // Аргументы могли не дождаться finish_reason — дожимаем.
            return self.flush_tools();
        }

        let chunk: Chunk = match serde_json::from_str(data) {
            Ok(c) => c,
            Err(_) => return Vec::new(), // битый кадр пропускаем, не роняем ход
        };

        if let Some(err) = chunk.error {
            self.finished = true;
            return vec![StreamEvent::Error(
                err.message.unwrap_or_else(|| "provider error".into()),
            )];
        }

        let mut out = Vec::new();
        for choice in chunk.choices {
            if let Some(delta) = choice.delta {
                out.extend(self.handle_delta(delta));
            }
            if let Some(fr) = choice.finish_reason {
                // Аргументы тулзов парсим только здесь (находка 3).
                out.extend(self.flush_tools());
                out.push(StreamEvent::Done {
                    finish: FinishReason::parse(&fr),
                });
                self.finished = true;
            }
        }
        // B-3: точный счёт токенов, если провайдер его прислал. Нули и null
        // событием не становятся: вызывающий отличает «нет данных» от «ноль».
        out.extend(usage_events(chunk.usage.as_ref()));
        out
    }

    fn handle_delta(&mut self, d: Delta) -> Vec<StreamEvent> {
        let mut out = Vec::new();

        // Находка 2: пустой content приходит в каждом чанке. Не событие.
        if let Some(c) = d.content {
            if !c.is_empty() {
                out.push(StreamEvent::Content(c));
            }
        }
        if let Some(r) = d.reasoning_content {
            if !r.is_empty() {
                out.push(StreamEvent::Reasoning(r));
            }
        }

        if let Some(calls) = d.tool_calls {
            for tc in calls {
                let idx = tc.index;
                let slot = match self.pending_tools.iter_mut().find(|(i, _)| *i == idx) {
                    Some((_, p)) => p,
                    None => {
                        self.pending_tools.push((idx, PendingTool::default()));
                        &mut self.pending_tools.last_mut().unwrap().1
                    }
                };
                if let Some(id) = tc.id {
                    if !id.is_empty() {
                        // Первый чанк вызова: id и имя известны.
                        slot.id = id.clone();
                        let name = tc
                            .function
                            .as_ref()
                            .and_then(|f| f.name.clone())
                            .unwrap_or_default();
                        slot.name = name.clone();
                        out.push(StreamEvent::ToolCallStart {
                            index: idx,
                            id,
                            name,
                        });
                    }
                }
                if let Some(name) = tc.function.as_ref().and_then(|f| f.name.clone()) {
                    if !name.is_empty() && slot.name.is_empty() {
                        slot.name = name.clone();
                    }
                }
                // Куски аргументов буферизуем как есть: JSON пока невалиден.
                if let Some(args) = tc.function.as_ref().and_then(|f| f.arguments.clone()) {
                    slot.args.push_str(&args);
                }
            }
        }
        // `role` в первом чанке намеренно игнорируем: роль сообщения
        // назначает ядро (crate::turn), а не парсер потока.
        out
    }

    /// Собрать накопленные вызовы в готовые [`ToolCall`].
    fn flush_tools(&mut self) -> Vec<StreamEvent> {
        if self.pending_tools.is_empty() {
            return Vec::new();
        }
        let mut taken = std::mem::take(&mut self.pending_tools);
        // Порядок определяется индексом, а не порядком прихода чанков.
        taken.sort_by_key(|(i, _)| *i);
        taken
            .into_iter()
            .map(|(_, p)| {
                // Пустая строка аргументов = модель не передала параметров.
                let raw = if p.args.trim().is_empty() {
                    "{}"
                } else {
                    &p.args
                };
                let arguments = serde_json::from_str::<serde_json::Value>(raw)
                    .ok()
                    .and_then(|v| v.as_object().cloned())
                    // Невалидный JSON от модели не роняет ход: отдаём пустой
                    // объект, а ошибку видно по отсутствию ожидаемых полей.
                    .unwrap_or_default();
                StreamEvent::ToolCallComplete(ToolCall {
                    id: p.id,
                    name: p.name,
                    arguments,
                })
            })
            .collect()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Склеить события из произвольных кусков, имитируя сеть.
    fn parse_chunks(chunks: &[&str]) -> Vec<StreamEvent> {
        let mut p = SseParser::new();
        let mut out = Vec::new();
        for c in chunks {
            out.extend(p.feed(c.as_bytes()));
        }
        out.extend(p.finish());
        out
    }

    fn texts(evs: &[StreamEvent], want_reasoning: bool) -> Vec<&str> {
        evs.iter()
            .filter_map(|e| match e {
                StreamEvent::Content(s) if !want_reasoning => Some(s.as_str()),
                StreamEvent::Reasoning(s) if want_reasoning => Some(s.as_str()),
                _ => None,
            })
            .collect()
    }

    // ---- находка 1: reasoning_content идёт отдельным каналом ----

    #[test]
    fn reasoning_arrives_as_separate_events() {
        let evs = parse_chunks(&[
            "data: {\"choices\":[{\"delta\":{\"content\":\"\",\"reasoning_content\":\"The\"},\"finish_reason\":null}]}\n\n",
            "data: {\"choices\":[{\"delta\":{\"content\":\"\",\"reasoning_content\":\" user\"},\"finish_reason\":null}]}\n\n",
            "data: {\"choices\":[{\"delta\":{\"content\":\"pong\"},\"finish_reason\":\"stop\"}]}\n\n",
            "data: [DONE]\n\n",
        ]);
        assert_eq!(texts(&evs, true), vec!["The", " user"]);
        assert_eq!(texts(&evs, false), vec!["pong"]);
    }

    // ---- находка 2: пустой content не порождает событий ----

    #[test]
    fn empty_content_deltas_produce_no_events() {
        let evs = parse_chunks(&[
            "data: {\"choices\":[{\"delta\":{\"content\":\"\",\"role\":\"assistant\",\"reasoning_content\":\"\"},\"finish_reason\":null}]}\n\n",
            "data: {\"choices\":[{\"delta\":{\"content\":\"\",\"reasoning_content\":\"\"},\"finish_reason\":null}]}\n\n",
            "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\n",
            "data: [DONE]\n\n",
        ]);
        let contents = evs
            .iter()
            .filter(|e| matches!(e, StreamEvent::Content(_)))
            .count();
        assert_eq!(
            contents, 1,
            "пустые дельты не должны доходить до UI: {evs:?}"
        );
    }

    #[test]
    fn empty_reasoning_deltas_produce_no_events() {
        let evs = parse_chunks(&[
            "data: {\"choices\":[{\"delta\":{\"content\":\"\",\"reasoning_content\":\"\"},\"finish_reason\":\"stop\"}]}\n\n",
            "data: [DONE]\n\n",
        ]);
        assert!(
            !evs.iter().any(|e| matches!(e, StreamEvent::Reasoning(_))),
            "{evs:?}"
        );
    }

    // ---- находка 3: аргументы тулзов кусками, парсим после finish ----

    #[test]
    fn tool_call_arguments_are_buffered_across_chunks() {
        let evs = parse_chunks(&[
            "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"{\"}}],\"content\":\"\"},\"finish_reason\":null}]}\n\n",
            "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"type\":\"function\",\"index\":0,\"function\":{\"arguments\":\"\\\"city\\\": \"}}]}}]}\n\n",
            "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"type\":\"function\",\"index\":0,\"function\":{\"arguments\":\"\\\"Tokyo\"}}]}}]}\n\n",
            "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"type\":\"function\",\"index\":0,\"function\":{\"arguments\":\"\\\"}\"}}]}}]}\n\n",
            "data: {\"choices\":[{\"delta\":{\"content\":\"\",\"reasoning_content\":\"\"},\"finish_reason\":\"tool_calls\"}]}\n\n",
            "data: [DONE]\n\n",
        ]);

        let starts: Vec<_> = evs
            .iter()
            .filter_map(|e| match e {
                StreamEvent::ToolCallStart { id, name, .. } => Some((id.as_str(), name.as_str())),
                _ => None,
            })
            .collect();
        assert_eq!(starts, vec![("call_1", "get_weather")]);

        let complete: Vec<_> = evs
            .iter()
            .filter_map(|e| match e {
                StreamEvent::ToolCallComplete(t) => Some(t),
                _ => None,
            })
            .collect();
        assert_eq!(complete.len(), 1);
        assert_eq!(complete[0].id, "call_1");
        assert_eq!(complete[0].name, "get_weather");
        assert_eq!(
            complete[0].arguments["city"],
            serde_json::json!("Tokyo"),
            "аргументы должны собраться из кусков в валидный JSON"
        );
        assert!(evs.iter().any(|e| matches!(
            e,
            StreamEvent::Done {
                finish: FinishReason::ToolCalls
            }
        )));
    }

    #[test]
    fn partial_json_is_never_emitted_before_finish() {
        // До finish_reason аргументы — невалидный JSON; наружу ничего не уходит.
        // Куски как на живом эндпоинте: скобка закрывается в последнем чанке.
        let mut p = SseParser::new();
        let mut evs = p.feed(
            "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c\",\"function\":{\"name\":\"f\",\"arguments\":\"{\\\"a\\\":\"}}]}}]}\n\n"
                .as_bytes(),
        );
        evs.extend(p.feed(
            "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"1\"}}]}}]}\n\n"
                .as_bytes(),
        ));
        assert!(
            !evs.iter()
                .any(|e| matches!(e, StreamEvent::ToolCallComplete(_))),
            "нельзя отдавать частичный JSON: {evs:?}"
        );
        evs.extend(p.feed("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"}\"}}]}}]}\n\n".as_bytes()));
        evs.extend(p.feed(
            "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n".as_bytes(),
        ));
        let done: Vec<_> = evs
            .iter()
            .filter_map(|e| match e {
                StreamEvent::ToolCallComplete(t) => Some(t),
                _ => None,
            })
            .collect();
        assert_eq!(done.len(), 1);
        assert_eq!(done[0].arguments["a"], serde_json::json!(1));
    }

    #[test]
    fn multiple_parallel_tool_calls_keep_index_order() {
        // Модель вызывает два тулза; чанки приходят вперемешку.
        let evs = parse_chunks(&[
            "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":1,\"id\":\"b\",\"function\":{\"name\":\"second\",\"arguments\":\"{}\"}},{\"index\":0,\"id\":\"a\",\"function\":{\"name\":\"first\",\"arguments\":\"{}\"}}]}}]}\n\n",
            "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n",
            "data: [DONE]\n\n",
        ]);
        let names: Vec<_> = evs
            .iter()
            .filter_map(|e| match e {
                StreamEvent::ToolCallComplete(t) => Some(t.name.as_str()),
                _ => None,
            })
            .collect();
        assert_eq!(names, vec!["first", "second"], "сортировка по index");
    }

    #[test]
    fn tool_without_arguments_yields_empty_object() {
        let evs = parse_chunks(&[
            "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"a\",\"function\":{\"name\":\"noop\"}}]}}]}\n\n",
            "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n",
            "data: [DONE]\n\n",
        ]);
        let t = evs
            .iter()
            .find_map(|e| match e {
                StreamEvent::ToolCallComplete(t) => Some(t),
                _ => None,
            })
            .expect("tool call");
        assert!(t.arguments.is_empty());
    }

    #[test]
    fn invalid_tool_arguments_do_not_panic() {
        let evs = parse_chunks(&[
            "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"a\",\"function\":{\"name\":\"f\",\"arguments\":\"{broken\"}}]}}]}\n\n",
            "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n",
            "data: [DONE]\n\n",
        ]);
        let t = evs
            .iter()
            .find_map(|e| match e {
                StreamEvent::ToolCallComplete(t) => Some(t),
                _ => None,
            })
            .expect("ход не должен падать на битом JSON");
        assert!(t.arguments.is_empty());
    }

    // ---- устойчивость к сети ----

    #[test]
    fn frame_split_across_network_chunks() {
        // Один data-кадр разорван посреди JSON.
        let evs = parse_chunks(&[
            "data: {\"choices\":[{\"delta\":{\"con",
            "tent\":\"hello\"},\"finish_reason\":\"stop\"}]}\n",
            "\ndata: [DONE]\n\n",
        ]);
        assert_eq!(texts(&evs, false), vec!["hello"]);
    }

    #[test]
    fn multibyte_char_split_across_chunks_is_not_corrupted() {
        // 'я' = 2 байта. Режем кадр между ними: наивная реализация потеряла бы
        // символ или выдала бы мусор. Это частый случай на CJK/emoji-стриме.
        let frame = "data: {\"choices\":[{\"delta\":{\"content\":\"привет я\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n";
        let bytes = frame.as_bytes();
        // Найдём позицию внутри многобайтового символа 'я'.
        let ya_pos = bytes
            .windows(2)
            .position(|w| w == [0xD1, 0x8F])
            .expect("символ 'я' должен быть в кадре");
        let split = ya_pos + 1; // разрыв ровно посреди символа

        let mut p = SseParser::new();
        let mut evs = p.feed(&bytes[..split]);
        evs.extend(p.feed(&bytes[split..]));
        evs.extend(p.finish());

        assert_eq!(
            texts(&evs, false),
            vec!["привет я"],
            "текст должен собраться без потерь: {evs:?}"
        );
    }

    #[test]
    fn emoji_split_across_chunks_is_not_corrupted() {
        // 4-байтовый символ, разорванный на три части.
        let frame = "data: {\"choices\":[{\"delta\":{\"content\":\"ok 🚀\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n";
        let bytes = frame.as_bytes();
        let rocket = bytes
            .windows(4)
            .position(|w| w == [0xF0, 0x9F, 0x9A, 0x80])
            .expect("emoji должен быть в кадре");

        let mut p = SseParser::new();
        let mut evs = p.feed(&bytes[..rocket + 1]);
        evs.extend(p.feed(&bytes[rocket + 1..rocket + 3]));
        evs.extend(p.feed(&bytes[rocket + 3..]));
        evs.extend(p.finish());

        assert_eq!(texts(&evs, false), vec!["ok 🚀"], "{evs:?}");
    }

    #[test]
    fn crlf_line_endings_are_handled() {
        let evs = parse_chunks(&[
            "data: {\"choices\":[{\"delta\":{\"content\":\"x\"},\"finish_reason\":\"stop\"}]}\r\n\r\n",
            "data: [DONE]\r\n\r\n",
        ]);
        assert_eq!(texts(&evs, false), vec!["x"]);
    }

    #[test]
    fn malformed_frames_are_skipped_not_fatal() {
        let evs = parse_chunks(&[
            "data: not-json-at-all\n\n",
            ":keepalive comment\n\n",
            "\n",
            "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n",
            "data: [DONE]\n\n",
        ]);
        assert_eq!(texts(&evs, false), vec!["ok"]);
    }

    #[test]
    fn truncated_stream_still_flushes_pending_tools() {
        // Оборвалось без finish_reason и без [DONE].
        let mut p = SseParser::new();
        p.feed("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"a\",\"function\":{\"name\":\"f\",\"arguments\":\"{\\\"k\\\":1}\"}}]}}]}\n\n".as_bytes());
        let evs = p.finish();
        assert!(
            evs.iter()
                .any(|e| matches!(e, StreamEvent::ToolCallComplete(_))),
            "вызов не должен теряться при обрыве: {evs:?}"
        );
        assert!(p.is_finished());
    }

    // ---- ошибки провайдера ----

    #[test]
    fn provider_error_body_becomes_error_event() {
        let evs = parse_chunks(&[
            "data: {\"error\":{\"message\":\"Request body must be valid JSON\",\"type\":\"invalid_request_error\"}}\n\n",
        ]);
        assert!(matches!(
            evs.first(),
            Some(StreamEvent::Error(m)) if m.contains("Request body")
        ));
    }

    // ---- usage ----

    #[test]
    fn null_usage_does_not_break_parsing() {
        // Подтверждено на живом эндпоинте: usage приходит null в чанках.
        let evs = parse_chunks(&[
            "data: {\"id\":\"x\",\"usage\":null,\"choices\":[{\"delta\":{\"content\":\"a\"},\"finish_reason\":\"stop\"}]}\n\n",
            "data: [DONE]\n\n",
        ]);
        assert_eq!(texts(&evs, false), vec!["a"]);
        assert!(!evs.iter().any(|e| matches!(e, StreamEvent::Usage { .. })));
    }

    #[test]
    fn real_usage_becomes_event() {
        // B-3: провайдер, который честно шлёт usage, даёт ядру точный счёт.
        let evs = parse_chunks(&[
            "data: {\"choices\":[{\"delta\":{\"content\":\"a\"},\"finish_reason\":\"stop\"}]}\n\n",
            "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":120,\"completion_tokens\":34,\"total_tokens\":154}}\n\n",
            "data: [DONE]\n\n",
        ]);
        assert!(evs.contains(&StreamEvent::Usage {
            input_tokens: 120,
            output_tokens: 34,
        }));
    }

    #[test]
    fn total_tokens_used_when_completion_missing() {
        let evs = parse_chunks(&[
            "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"total_tokens\":17}}\n\n",
        ]);
        assert!(evs.contains(&StreamEvent::Usage {
            input_tokens: 10,
            output_tokens: 17,
        }));
    }

    #[test]
    fn unknown_finish_reason_maps_to_unknown() {
        let evs = parse_chunks(&[
            "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"weird_new_value\"}]}\n\n",
            "data: [DONE]\n\n",
        ]);
        assert!(matches!(
            evs.iter().find(|e| matches!(e, StreamEvent::Done { .. })),
            Some(StreamEvent::Done {
                finish: FinishReason::Unknown
            })
        ));
    }

    #[test]
    fn events_after_done_are_ignored() {
        let mut p = SseParser::new();
        p.feed(
            "data: {\"choices\":[{\"delta\":{\"content\":\"a\"},\"finish_reason\":\"stop\"}]}\n\n"
                .as_bytes(),
        );
        p.feed("data: [DONE]\n\n".as_bytes());
        assert!(p.is_finished());
        let late =
            p.feed("data: {\"choices\":[{\"delta\":{\"content\":\"ghost\"}}]}\n\n".as_bytes());
        assert!(
            !late.iter().any(|e| matches!(e, StreamEvent::Content(_))),
            "после [DONE] ничего не принимаем: {late:?}"
        );
    }

    // ---- B-7: бедняцкий fuzz на stable ----

    /// Псевдослучайные байты из LCG с фиксированным сидом: парсер обязан
    /// пережить любой мусор и любое дробление на чанки без паники. Сид
    /// фиксирован — падение воспроизводимо. Настоящий fuzz живёт в
    /// `fuzz/fuzz_targets/sse_parser.rs` (cargo fuzz, nightly), а этот тест
    /// гоняет ту же идею на stable в каждом CI-прогоне.
    #[test]
    fn random_bytes_never_panic_seeded() {
        struct Lcg(u64);
        impl Lcg {
            fn next(&mut self) -> u8 {
                self.0 = self
                    .0
                    .wrapping_mul(6364136223846793005)
                    .wrapping_add(1442695040888963407);
                (self.0 >> 33) as u8
            }
        }
        let mut rng = Lcg(0x005E_EDB7_A5E1);
        for _ in 0..3000 {
            let len = (rng.next() as usize % 220) + 1;
            let bytes: Vec<u8> = (0..len).map(|_| rng.next()).collect();

            // Целым куском.
            let mut p = SseParser::new();
            p.feed(&bytes);
            p.finish();

            // Дроблением: разрывы посреди UTF-8, "data:" и JSON.
            let mut p2 = SseParser::new();
            let mut i = 0;
            while i < bytes.len() {
                let step = (rng.next() as usize % 9).max(1);
                let end = (i + step).min(bytes.len());
                p2.feed(&bytes[i..end]);
                i = end;
            }
            p2.finish();
        }

        // Валидные кадры вперемешку с мусором: структура кадра выживает.
        let mut rng2 = Lcg(0xC0FF_EE11);
        for _ in 0..500 {
            let mut bytes: Vec<u8> = Vec::new();
            bytes.extend_from_slice(b"data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n");
            for _ in 0..(rng2.next() % 24) {
                bytes.push(rng2.next());
            }
            bytes.extend_from_slice(b"\ndata: [DONE]\n\n");
            let mut p = SseParser::new();
            let evs = p.feed(&bytes);
            let _ = p.finish();
            assert!(
                evs.iter()
                    .any(|e| matches!(e, StreamEvent::Content(t) if t == "ok")),
                "валидный кадр обязан распознаваться даже рядом с мусором"
            );
        }
    }
}
