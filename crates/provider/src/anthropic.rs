/*! Нативный клиент Anthropic Messages API (`POST /v1/messages`, SSE).

Протокол сверен с https://docs.anthropic.com/ja/api/messages (октябрь 2026):
- заголовки `x-api-key` + `anthropic-version: 2023-06-01`, ключ обязателен;
- `max_tokens` обязателен в теле — берём из запроса, иначе 16384;
- системный промпт — отдельным полем `system`, а не ролью в messages;
- роли строго чередуются user/assistant: наши tool-результаты едут ролью
  user, поэтому соседей одной роли склеиваем, иначе API отвечает 400;
- стрим — SSE-кадры `event: <тип>` + `data: <json>`: text_delta → Content,
  thinking_delta → Reasoning, tool_use-блоки собираются из
  input_json_delta-кусков и отдаются ToolCallComplete на content_block_stop,
  usage складывается из message_start (input) + message_delta (output).

Наверх отдаём те же [`crate::sse::StreamEvent`], что и OpenAI-клиент:
драйвер хода разницы не замечает.
*/

use std::time::Duration;

use futures_util::StreamExt;
use thiserror::Error;
use tokio::sync::mpsc;

use crate::sse::{FinishReason, StreamEvent};
use crate::types::{ChatRequest, Role, ToolCall};

pub const ANTHROPIC_VERSION: &str = "2023-06-01";
/// Дефолт `max_tokens`, если запрос его не задал: агент без лимита
/// на ответ задыхается на обрезанных tool-вызовах.
pub const DEFAULT_MAX_TOKENS: u32 = 16384;

#[derive(Debug, Error)]
pub enum AnthropicError {
    #[error("http: {0}")]
    Http(String),
    #[error("anthropic returned {status}: {body}")]
    Status { status: u16, body: String },
    #[error("config: {0}")]
    Config(String),
}

impl From<AnthropicError> for crate::http::ProviderError {
    fn from(e: AnthropicError) -> Self {
        match e {
            AnthropicError::Http(m) => Self::Http(m),
            AnthropicError::Status { status, body } => Self::Status { status, body },
            AnthropicError::Config(m) => Self::Config(m),
        }
    }
}

/// Клиент Messages API. Дёшев в создании, клонируется свободно.
#[derive(Debug, Clone)]
pub struct AnthropicProvider {
    base_url: String,
    api_key: String,
    client: reqwest::Client,
}

impl AnthropicProvider {
    pub fn new(
        base_url: impl Into<String>,
        api_key: impl Into<String>,
    ) -> Result<Self, AnthropicError> {
        let base_url = base_url.into();
        let api_key = api_key.into();
        if base_url.trim().is_empty() {
            return Err(AnthropicError::Config("пустой base_url".into()));
        }
        if api_key.trim().is_empty() {
            return Err(AnthropicError::Config("пустой api_key".into()));
        }
        Self::build(base_url, api_key)
    }

    /// Совместимый `/v1/messages` без ключа (LM Studio и подобные).
    /// Заголовок `x-api-key` тогда не отправляется вовсе.
    pub fn new_open(base_url: impl Into<String>) -> Result<Self, AnthropicError> {
        let base_url = base_url.into();
        if base_url.trim().is_empty() {
            return Err(AnthropicError::Config("пустой base_url".into()));
        }
        Self::build(base_url, String::new())
    }

    fn build(base_url: String, api_key: String) -> Result<Self, AnthropicError> {
        let client = reqwest::Client::builder()
            .connect_timeout(Duration::from_secs(15))
            .tcp_nodelay(true)
            .build()
            .map_err(|e| AnthropicError::Http(e.to_string()))?;
        Ok(Self {
            base_url: base_url.trim_end_matches('/').to_string(),
            api_key,
            client,
        })
    }

    pub fn base_url(&self) -> &str {
        &self.base_url
    }

    fn headers(&self, rb: reqwest::RequestBuilder) -> reqwest::RequestBuilder {
        let rb = rb.header("anthropic-version", ANTHROPIC_VERSION);
        if self.api_key.is_empty() {
            rb
        } else {
            rb.header("x-api-key", &self.api_key)
        }
    }

    /// `GET /models` (`{data: [{id, display_name}]}`). Сырой JSON, как и
    /// у OpenAI-клиента: нормализация — в слое команд (`fetch_provider_models`).
    pub async fn list_models(&self) -> Result<serde_json::Value, AnthropicError> {
        let resp = self
            .headers(self.client.get(format!("{}/models", self.base_url)))
            .send()
            .await
            .map_err(|e| AnthropicError::Http(e.to_string()))?;
        let status = resp.status();
        let text = resp
            .text()
            .await
            .map_err(|e| AnthropicError::Http(e.to_string()))?;
        if !status.is_success() {
            return Err(AnthropicError::Status {
                status: status.as_u16(),
                body: text.chars().take(500).collect(),
            });
        }
        serde_json::from_str(&text).map_err(|e| AnthropicError::Http(format!("models json: {e}")))
    }

    /// Стрим хода. Та же форма, что у [`crate::http::OpenAiProvider::stream`]:
    /// события в bounded-канал (backpressure), handle для отмены.
    pub fn stream(
        &self,
        req: ChatRequest,
    ) -> Result<(mpsc::Receiver<StreamEvent>, tokio::task::JoinHandle<()>), AnthropicError> {
        let url = format!("{}/messages", self.base_url);
        let client = self.client.clone();
        let key = self.api_key.clone();
        let (tx, rx) = mpsc::channel::<StreamEvent>(256);

        let handle = tokio::spawn(async move {
            let body = anthropic_body(&req);
            let mut rb = client
                .post(&url)
                .header("accept", "text/event-stream")
                .header("anthropic-version", ANTHROPIC_VERSION)
                .json(&body);
            if !key.is_empty() {
                rb = rb.header("x-api-key", &key);
            }
            let resp = match rb.send().await {
                Ok(r) => r,
                Err(e) => {
                    let _ = tx.send(StreamEvent::Error(format!("http: {e}"))).await;
                    return;
                }
            };
            let status = resp.status();
            if !status.is_success() {
                let text = resp.text().await.unwrap_or_default();
                let msg =
                    anthropic_error_text(&text).unwrap_or_else(|| text.chars().take(300).collect());
                let _ = tx
                    .send(StreamEvent::Error(format!("anthropic {status}: {msg}")))
                    .await;
                return;
            }

            let mut acc = Acc::default();
            let mut buf = String::new();
            let mut stream = resp.bytes_stream();
            while let Some(item) = stream.next().await {
                let bytes = match item {
                    Ok(b) => b,
                    Err(e) => {
                        for ev in acc.finish_turn() {
                            if tx.send(ev).await.is_err() {
                                return;
                            }
                        }
                        let _ = tx.send(StreamEvent::Error(format!("stream: {e}"))).await;
                        return;
                    }
                };
                buf.push_str(&String::from_utf8_lossy(&bytes));
                // Кадры разделены пустой строкой; хвост-огрызок ждёт следующий чанк.
                while let Some(pos) = buf.find("\n\n") {
                    let frame = buf[..pos].to_string();
                    buf = buf[pos + 2..].to_string();
                    for ev in acc.feed_frame(&frame) {
                        if tx.send(ev).await.is_err() {
                            return;
                        }
                    }
                    if acc.done {
                        // Хвост после message_stop не разбираем.
                        for ev in acc.finish_turn() {
                            let _ = tx.send(ev).await;
                        }
                        return;
                    }
                }
            }
            for ev in acc.finish_turn() {
                if tx.send(ev).await.is_err() {
                    return;
                }
            }
        });
        Ok((rx, handle))
    }
}

/// Текст ошибки из JSON `{error: {message}}`, иначе None (тогда покажем сырьё).
fn anthropic_error_text(text: &str) -> Option<String> {
    serde_json::from_str::<serde_json::Value>(text)
        .ok()?
        .get("error")?
        .get("message")?
        .as_str()
        .map(|s| s.to_string())
}

/// Наш запрос → тело `/v1/messages`. Чистая функция — тестируется.
pub fn anthropic_body(req: &ChatRequest) -> serde_json::Value {
    let mut system_parts = Vec::new();
    // Роль → блоки контента; соседей одной роли склеим ниже.
    let mut seq: Vec<(&str, Vec<serde_json::Value>)> = Vec::new();
    for m in &req.messages {
        match m.role {
            Role::System => {
                if !m.content.is_empty() {
                    system_parts.push(m.content.clone());
                }
            }
            Role::User => {
                if !m.content.is_empty() {
                    seq.push((
                        "user",
                        vec![serde_json::json!({"type": "text", "text": m.content})],
                    ));
                }
            }
            Role::Tool => {
                let id = m.tool_call_id.clone().unwrap_or_default();
                if id.is_empty() || m.content.is_empty() {
                    continue;
                }
                seq.push((
                    "user",
                    vec![serde_json::json!({
                        "type": "tool_result",
                        "tool_use_id": id,
                        "content": m.content,
                    })],
                ));
            }
            Role::Assistant => {
                let mut blocks = Vec::new();
                if !m.content.is_empty() {
                    blocks.push(serde_json::json!({"type": "text", "text": m.content}));
                }
                for tc in &m.tool_calls {
                    blocks.push(serde_json::json!({
                        "type": "tool_use",
                        "id": tc.id,
                        "name": tc.name,
                        "input": serde_json::Value::Object(tc.arguments.clone()),
                    }));
                }
                // reasoning назад не отдаём: у Anthropic это thinking-блоки
                // с подписью, без неё API их отвергнет.
                if !blocks.is_empty() {
                    seq.push(("assistant", blocks));
                }
            }
        }
    }
    // Склейка соседей одной роли (иначе 400): блоки дописываются.
    let mut messages: Vec<serde_json::Value> = Vec::new();
    for (role, blocks) in seq {
        if let Some(last) = messages.last_mut() {
            if last.get("role").and_then(|r| r.as_str()) == Some(role) {
                if let Some(arr) = last.get_mut("content").and_then(|c| c.as_array_mut()) {
                    arr.extend(blocks);
                    continue;
                }
            }
        }
        messages.push(serde_json::json!({"role": role, "content": blocks}));
    }

    let mut body = serde_json::json!({
        "model": req.model,
        "max_tokens": req.max_tokens.unwrap_or(DEFAULT_MAX_TOKENS),
        "messages": messages,
        "stream": true,
    });
    if !system_parts.is_empty() {
        body["system"] = serde_json::Value::String(system_parts.join("\n\n"));
    }
    if let Some(t) = req.temperature {
        body["temperature"] = serde_json::json!(t);
    }
    let tools = openai_envelope_to_anthropic(&req.tools);
    if !tools.is_empty() {
        body["tools"] = serde_json::Value::Array(tools);
    }
    body
}

/// Наш OpenAI-конверт `{type: function, function: {...}}` → формат Anthropic
/// `{name, description, input_schema}`. Неизвестные формы отбрасываются:
/// кривой тулз хуже отсутствующего.
fn openai_envelope_to_anthropic(tools: &[serde_json::Value]) -> Vec<serde_json::Value> {
    let mut out = Vec::new();
    for t in tools {
        if let Some(f) = t.get("function") {
            let name = f.get("name").and_then(|n| n.as_str()).unwrap_or("");
            if name.is_empty() {
                continue;
            }
            out.push(serde_json::json!({
                "name": name,
                "description": f.get("description").and_then(|d| d.as_str()).unwrap_or(""),
                "input_schema": f.get("parameters").cloned().unwrap_or_else(|| serde_json::json!({"type": "object"})),
            }));
        } else if t.get("name").and_then(|n| n.as_str()).is_some() {
            out.push(t.clone()); // уже anthropic-форма
        }
    }
    out
}

#[derive(Debug, Default, PartialEq)]
enum SlotKind {
    #[default]
    Ignore,
    Text,
    Thinking,
    ToolUse,
}

#[derive(Debug, Default)]
struct Slot {
    kind: SlotKind,
    id: String,
    name: String,
    buf: String,
    started: bool,
}

/// Аккумулятор SSE-кадров Anthropic → наши StreamEvent.
/// `feed_frame` принимает сырой кадр (`event:` + `data:` строки).
/// Чистая логика без сети — тестируется скриптованными кадрами.
#[derive(Debug, Default)]
pub struct Acc {
    slots: Vec<Slot>,
    input_tokens: u32,
    finish: FinishReason,
    done: bool,
}

fn parse_frame(frame: &str) -> (String, String) {
    let mut event = String::new();
    let mut data_lines = Vec::new();
    for line in frame.lines() {
        if let Some(e) = line.strip_prefix("event:") {
            event = e.trim().to_string();
        } else if let Some(d) = line.strip_prefix("data:") {
            data_lines.push(d.trim_start().to_string());
        }
    }
    (event, data_lines.join("\n"))
}

impl Acc {
    pub fn feed_frame(&mut self, frame: &str) -> Vec<StreamEvent> {
        let (event, data) = parse_frame(frame);
        if event.is_empty() || data.is_empty() {
            return Vec::new();
        }
        // Пинг и мусорные кадры — мимо.
        if event == "ping" {
            return Vec::new();
        }
        let v: serde_json::Value = match serde_json::from_str(&data) {
            Ok(v) => v,
            Err(_) => return Vec::new(),
        };
        match event.as_str() {
            "message_start" => {
                self.input_tokens = v
                    .get("message")
                    .and_then(|m| m.get("usage"))
                    .and_then(|u| u.get("input_tokens"))
                    .and_then(|n| n.as_u64())
                    .unwrap_or(0) as u32;
                Vec::new()
            }
            "content_block_start" => {
                let index = v.get("index").and_then(|i| i.as_u64()).unwrap_or(0) as usize;
                let block = v.get("content_block");
                let btype = block
                    .and_then(|b| b.get("type"))
                    .and_then(|t| t.as_str())
                    .unwrap_or("");
                let kind = match btype {
                    "text" => SlotKind::Text,
                    "thinking" => SlotKind::Thinking,
                    "tool_use" => SlotKind::ToolUse,
                    _ => SlotKind::Ignore,
                };
                let id = block
                    .and_then(|b| b.get("id"))
                    .and_then(|i| i.as_str())
                    .unwrap_or("")
                    .to_string();
                let name = block
                    .and_then(|b| b.get("name"))
                    .and_then(|n| n.as_str())
                    .unwrap_or("")
                    .to_string();
                while self.slots.len() <= index {
                    self.slots.push(Slot::default());
                }
                self.slots[index] = Slot {
                    kind: SlotKind::Ignore,
                    id: String::new(),
                    name: String::new(),
                    buf: String::new(),
                    started: false,
                };
                let slot = &mut self.slots[index];
                slot.kind = kind;
                slot.id = id;
                slot.name = name;
                if slot.kind == SlotKind::ToolUse {
                    if slot.id.is_empty() {
                        slot.id = format!("anthropic-{index}");
                    }
                    slot.started = true;
                    vec![StreamEvent::ToolCallStart {
                        index,
                        id: slot.id.clone(),
                        name: slot.name.clone(),
                    }]
                } else {
                    Vec::new()
                }
            }
            "content_block_delta" => {
                let index = v.get("index").and_then(|i| i.as_u64()).unwrap_or(0) as usize;
                if index >= self.slots.len() {
                    return Vec::new();
                }
                let delta = v.get("delta").cloned().unwrap_or(serde_json::Value::Null);
                let dtype = delta.get("type").and_then(|t| t.as_str()).unwrap_or("");
                match dtype {
                    "text_delta" => {
                        let text = delta.get("text").and_then(|t| t.as_str()).unwrap_or("");
                        if text.is_empty() || self.slots[index].kind != SlotKind::Text {
                            Vec::new()
                        } else {
                            vec![StreamEvent::Content(text.to_string())]
                        }
                    }
                    "thinking_delta" => {
                        let text = delta.get("thinking").and_then(|t| t.as_str()).unwrap_or("");
                        if text.is_empty() {
                            Vec::new()
                        } else {
                            vec![StreamEvent::Reasoning(text.to_string())]
                        }
                    }
                    "input_json_delta" => {
                        let part = delta
                            .get("partial_json")
                            .and_then(|p| p.as_str())
                            .unwrap_or("");
                        self.slots[index].buf.push_str(part);
                        Vec::new()
                    }
                    // signature_delta и прочее — служебное, молча.
                    _ => Vec::new(),
                }
            }
            "content_block_stop" => {
                let index = v.get("index").and_then(|i| i.as_u64()).unwrap_or(0) as usize;
                if index >= self.slots.len() {
                    return Vec::new();
                }
                let slot = &mut self.slots[index];
                if slot.kind != SlotKind::ToolUse || !slot.started {
                    return Vec::new();
                }
                slot.started = false;
                let arguments = serde_json::from_str::<serde_json::Value>(&slot.buf)
                    .ok()
                    .and_then(|a| a.as_object().cloned())
                    .unwrap_or_default();
                vec![StreamEvent::ToolCallComplete(ToolCall {
                    id: slot.id.clone(),
                    name: slot.name.clone(),
                    arguments,
                })]
            }
            "message_delta" => {
                let stop = v
                    .get("delta")
                    .and_then(|d| d.get("stop_reason"))
                    .and_then(|s| s.as_str())
                    .unwrap_or("");
                self.finish = match stop {
                    "end_turn" | "stop_sequence" => FinishReason::Stop,
                    "max_tokens" => FinishReason::Length,
                    "tool_use" => FinishReason::ToolCalls,
                    "refusal" => FinishReason::ContentFilter,
                    _ => FinishReason::Unknown,
                };
                let output = v
                    .get("usage")
                    .and_then(|u| u.get("output_tokens"))
                    .and_then(|n| n.as_u64())
                    .unwrap_or(0) as u32;
                vec![StreamEvent::Usage {
                    input_tokens: self.input_tokens,
                    output_tokens: output,
                }]
            }
            "message_stop" => {
                self.done = true;
                vec![StreamEvent::Done {
                    finish: self.finish,
                }]
            }
            "error" => {
                let msg = v
                    .get("error")
                    .and_then(|e| e.get("message"))
                    .and_then(|m| m.as_str())
                    .unwrap_or("anthropic stream error");
                vec![StreamEvent::Error(msg.to_string())]
            }
            _ => Vec::new(),
        }
    }

    /// Конец HTTP-тела без message_stop: дожимаем собранные тулзы и
    /// закрываем ход, чтобы драйвер не висел.
    pub fn finish_turn(&mut self) -> Vec<StreamEvent> {
        let mut out = Vec::new();
        for (index, slot) in self.slots.iter_mut().enumerate() {
            if slot.kind == SlotKind::ToolUse && slot.started {
                slot.started = false;
                let arguments = serde_json::from_str::<serde_json::Value>(&slot.buf)
                    .ok()
                    .and_then(|a| a.as_object().cloned())
                    .unwrap_or_default();
                // Старта не было только если start-кадр потерялся: драйверу
                // нужен Start раньше Complete.
                out.push(StreamEvent::ToolCallStart {
                    index,
                    id: slot.id.clone(),
                    name: slot.name.clone(),
                });
                out.push(StreamEvent::ToolCallComplete(ToolCall {
                    id: slot.id.clone(),
                    name: slot.name.clone(),
                    arguments,
                }));
            }
        }
        if !self.done {
            self.done = true;
            out.push(StreamEvent::Done {
                finish: self.finish,
            });
        }
        out
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::types::{ChatMessage, ChatRequest, ToolSpec};

    fn req_with_tools() -> ChatRequest {
        let spec = ToolSpec {
            name: "read".into(),
            description: "Читать файл".into(),
            parameters: serde_json::json!({"type": "object", "properties": {"path": {"type": "string"}}}),
        };
        ChatRequest::new(
            "claude-opus-4-5",
            vec![ChatMessage::system("ты кодер"), ChatMessage::user("привет")],
        )
        .with_tools(&[spec])
    }

    #[test]
    fn body_maps_system_tools_and_tokens() {
        let body = anthropic_body(&req_with_tools());
        assert_eq!(body["model"], serde_json::json!("claude-opus-4-5"));
        assert_eq!(body["system"], serde_json::json!("ты кодер"));
        assert_eq!(body["max_tokens"], serde_json::json!(DEFAULT_MAX_TOKENS));
        assert_eq!(body["stream"], serde_json::json!(true));
        assert_eq!(body["messages"][0]["role"], serde_json::json!("user"));
        let tools = body["tools"].as_array().unwrap();
        assert_eq!(tools.len(), 1);
        assert_eq!(tools[0]["name"], serde_json::json!("read"));
        assert_eq!(
            tools[0]["input_schema"]["properties"]["path"]["type"],
            serde_json::json!("string")
        );
        assert!(tools[0].get("function").is_none(), "конверт снят");
    }

    #[test]
    fn body_merges_same_role_neighbours() {
        let tool_msg = ChatMessage {
            role: Role::Tool,
            content: "ok".into(),
            reasoning: String::new(),
            tool_calls: vec![],
            tool_call_id: Some("call_1".into()),
        };
        let req = ChatRequest::new(
            "m",
            vec![
                ChatMessage::user("сделай"),
                tool_msg,
                ChatMessage::user("ещё"),
            ],
        );
        let body = anthropic_body(&req);
        let msgs = body["messages"].as_array().unwrap();
        // Все три user-роли склеены в одно сообщение из трёх блоков.
        assert_eq!(msgs.len(), 1);
        assert_eq!(msgs[0]["content"].as_array().unwrap().len(), 3);
        assert_eq!(
            msgs[0]["content"][1]["type"],
            serde_json::json!("tool_result")
        );
    }

    #[test]
    fn body_respects_explicit_max_tokens() {
        let mut req = ChatRequest::new("m", vec![ChatMessage::user("hi")]);
        req.max_tokens = Some(4096);
        assert_eq!(anthropic_body(&req)["max_tokens"], serde_json::json!(4096));
    }

    fn frames_script() -> Vec<&'static str> {
        vec![
            "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"usage\":{\"input_tokens\":12,\"output_tokens\":0}}}",
            "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}",
            "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Привет\"}}",
            "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}",
            "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"read\"}}",
            "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"path\\\": \"}}",
            "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"\\\"a.txt\\\"}\"}}",
            "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}",
            "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":30}}",
            "event: message_stop\ndata: {\"type\":\"message_stop\"}",
        ]
    }

    #[test]
    fn sse_script_yields_driver_events() {
        let mut acc = Acc::default();
        let mut evs = Vec::new();
        for f in frames_script() {
            evs.extend(acc.feed_frame(f));
        }
        assert_eq!(evs[0], StreamEvent::Content("Привет".into()));
        assert_eq!(
            evs[1],
            StreamEvent::ToolCallStart {
                index: 1,
                id: "toolu_1".into(),
                name: "read".into()
            }
        );
        match &evs[2] {
            StreamEvent::ToolCallComplete(tc) => {
                assert_eq!(tc.id, "toolu_1");
                assert_eq!(tc.name, "read");
                assert_eq!(tc.arguments["path"], serde_json::json!("a.txt"));
            }
            other => panic!("жду Complete, получил {other:?}"),
        }
        assert_eq!(
            evs[3],
            StreamEvent::Usage {
                input_tokens: 12,
                output_tokens: 30
            }
        );
        assert_eq!(
            evs[4],
            StreamEvent::Done {
                finish: FinishReason::ToolCalls
            }
        );
    }

    #[test]
    fn error_frame_and_garbage_are_safe() {
        let mut acc = Acc::default();
        assert!(acc.feed_frame("event: ping\ndata: {}").is_empty());
        assert!(acc.feed_frame("мусор без формата").is_empty());
        assert!(acc
            .feed_frame("event: message_delta\ndata: не json")
            .is_empty());
        let evs = acc.feed_frame(
            "event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"overloaded\"}}",
        );
        assert_eq!(evs, vec![StreamEvent::Error("overloaded".into())]);
    }
}
