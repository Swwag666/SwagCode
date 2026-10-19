use serde::{Deserialize, Serialize};

/// Роль участника диалога в OpenAI-совместимом протоколе.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Role {
    System,
    User,
    Assistant,
    Tool,
}

/// Один вызов инструмента, собранный из инкрементальных дельт.
///
/// `arguments` — всегда уже распарсенный JSON-объект: сырые куски строки
/// буферизуются в [`crate::sse::SseParser`] и парсятся только после
/// `finish_reason`, потому что частичный JSON на лету невалиден.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct ToolCall {
    pub id: String,
    pub name: String,
    /// Уже валидный объект. Пустой объект, если модель не передала аргументы.
    pub arguments: serde_json::Map<String, serde_json::Value>,
}

/// Сообщение диалога. `reasoning` хранится отдельно от `content` (находка 1
/// в DECISIONS.md §5.6) — это два разных визуальных канала в UI.
///
/// Serialize написан вручную: наружу уходит wire-формат OpenAI, где
/// `tool_calls` — конверты `{id, type, function:{name, arguments}}`, а
/// `arguments` ОБЯЗАН быть строкой (JSON-текстом). Внутренний плоский
/// формат с распарсенным объектом удобен машине хода, но строгие
/// OpenAI-совместимые шлюзы отбивают его кодом 400.
#[derive(Debug, Clone, PartialEq, Deserialize)]
pub struct ChatMessage {
    pub role: Role,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub content: String,
    /// Поток рассуждений модели. Отдельное поле, не склеивается с `content`.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub reasoning: String,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub tool_calls: Vec<ToolCall>,
    /// Заполняется у роли `tool`: id вызова, на который отвечаем.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub tool_call_id: Option<String>,
}

impl ChatMessage {
    pub fn system(content: impl Into<String>) -> Self {
        Self::plain(Role::System, content)
    }

    pub fn user(content: impl Into<String>) -> Self {
        Self::plain(Role::User, content)
    }

    pub fn assistant(content: impl Into<String>) -> Self {
        Self::plain(Role::Assistant, content)
    }

    fn plain(role: Role, content: impl Into<String>) -> Self {
        Self {
            role,
            content: content.into(),
            reasoning: String::new(),
            tool_calls: Vec::new(),
            tool_call_id: None,
        }
    }
}

/// Конверт вызова в wire-формате OpenAI: шлюз требует `id`, `function.name`
/// и `arguments` строкой; пустые id/name подменяются заглушками, иначе
/// запрос упадёт 400 на валидации истории.
#[derive(Serialize)]
struct WireToolCall {
    id: String,
    #[serde(rename = "type")]
    kind: &'static str,
    function: WireFunction,
}

#[derive(Serialize)]
struct WireFunction {
    name: String,
    arguments: String,
}

impl serde::Serialize for ChatMessage {
    fn serialize<S: serde::Serializer>(&self, serializer: S) -> Result<S::Ok, S::Error> {
        use serde::ser::SerializeMap;
        /* content пишем ВСЕГДА и всегда строкой: строгие шлюзы требуют ключ
        content типа string|array даже у assistant с tool_calls, где текст
        пустой. Пропуск ключа даёт 400 «message content must be a string
        or an array». */
        let mut len = 2;
        if !self.reasoning.is_empty() {
            len += 1;
        }
        if !self.tool_calls.is_empty() {
            len += 1;
        }
        if self.tool_call_id.is_some() {
            len += 1;
        }
        let mut map = serializer.serialize_map(Some(len))?;
        map.serialize_entry("role", &self.role)?;
        map.serialize_entry("content", &self.content)?;
        if !self.reasoning.is_empty() {
            map.serialize_entry("reasoning", &self.reasoning)?;
        }
        if !self.tool_calls.is_empty() {
            let wire: Vec<WireToolCall> = self
                .tool_calls
                .iter()
                .enumerate()
                .map(|(i, tc)| WireToolCall {
                    id: if tc.id.is_empty() {
                        format!("call_{i}")
                    } else {
                        tc.id.clone()
                    },
                    kind: "function",
                    function: WireFunction {
                        name: if tc.name.is_empty() {
                            "unknown_tool".to_string()
                        } else {
                            tc.name.clone()
                        },
                        arguments: serde_json::to_string(&serde_json::Value::Object(
                            tc.arguments.clone(),
                        ))
                        .unwrap_or_else(|_| "{}".to_string()),
                    },
                })
                .collect();
            map.serialize_entry("tool_calls", &wire)?;
        }
        if let Some(id) = &self.tool_call_id {
            map.serialize_entry("tool_call_id", id)?;
        }
        map.end()
    }
}

/// Объявление инструмента, которое отправляем модели.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct ToolSpec {
    pub name: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub description: String,
    /// JSON Schema параметров.
    pub parameters: serde_json::Value,
}

/// Запрос к `chat/completions`.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ChatRequest {
    pub model: String,
    pub messages: Vec<ChatMessage>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub temperature: Option<f32>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub max_tokens: Option<u32>,
    /// Усилие рассуждения для reasoning-моделей (rustvy/openai-совместимые):
    /// включает поток мыслей (`reasoning_content`). Без него часть маршрутов
    /// (например `abyss/ahmia-v6-lite`) думает молча — «мыслей не видно».
    #[serde(
        default,
        skip_serializing_if = "Option::is_none",
        rename = "reasoning_effort"
    )]
    pub reasoning_effort: Option<String>,
    /// Всегда `true` для интерактивного агента: стрим — основа плавности.
    #[serde(default = "default_true")]
    pub stream: bool,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub tools: Vec<serde_json::Value>,
    /// Картинки текущего запроса (rev69). Прикладываются к ПОСЛЕДНЕМУ
    /// сообщению пользователя: история их не хранит, иначе база распухла бы.
    ///
    /// `serde(skip)` намеренно: в теле OpenAI-совместимого запроса
    /// верхнеуровневого `images` нет, и строгий провайдер ответил бы 400.
    /// Картинки подставляет сборщик тела по своему протоколу.
    #[serde(skip)]
    pub images: Vec<ImageAttachment>,
}

/// Тело OpenAI-совместимого запроса (rev71).
///
/// Обычно это просто сериализация [`ChatRequest`], но с картинками `content`
/// последнего сообщения пользователя обязан стать массивом частей: строку с
/// картинкой OpenAI не примет. Поле `images` в тело не попадает (`serde(skip)`),
/// поэтому картинки подставляются здесь, а не сериализатором.
pub fn openai_body(req: &ChatRequest) -> serde_json::Value {
    let mut v = serde_json::to_value(req).unwrap_or(serde_json::Value::Null);
    if req.images.is_empty() {
        return v;
    }
    let Some(msgs) = v.get_mut("messages").and_then(|m| m.as_array_mut()) else {
        return v;
    };
    let Some(idx) = msgs
        .iter()
        .rposition(|m| m.get("role").and_then(|r| r.as_str()) == Some("user"))
    else {
        return v;
    };
    let mut parts: Vec<serde_json::Value> = Vec::new();
    for img in &req.images {
        parts.push(serde_json::json!({
            "type": "image_url",
            "image_url": {
                "url": format!("data:{};base64,{}", img.media_type, img.data_base64),
            },
        }));
    }
    let text = msgs[idx]
        .get("content")
        .and_then(|c| c.as_str())
        .unwrap_or("")
        .to_string();
    if !text.is_empty() {
        parts.push(serde_json::json!({"type": "text", "text": text}));
    }
    msgs[idx]["content"] = serde_json::Value::Array(parts);
    v
}
fn default_true() -> bool {
    true
}

/// Картинка, приложенная к запросу.
///
/// Данные лежат в base64 БЕЗ префикса `data:`: оба протокола хотят именно так,
/// а префикс каждый добавляет по своим правилам.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct ImageAttachment {
    /// MIME-тип: `image/png`, `image/jpeg`, `image/webp`, `image/gif`.
    pub media_type: String,
    /// Сами байты в base64.
    pub data_base64: String,
}

impl ChatRequest {
    pub fn new(model: impl Into<String>, messages: Vec<ChatMessage>) -> Self {
        Self {
            model: model.into(),
            messages,
            temperature: None,
            max_tokens: None,
            reasoning_effort: None,
            stream: true,
            tools: Vec::new(),
            images: Vec::new(),
        }
    }

    /// Упаковывает наши [`ToolSpec`] в wire-формат провайдера.
    pub fn with_tools(mut self, specs: &[ToolSpec]) -> Self {
        self.tools = specs
            .iter()
            .map(|t| {
                serde_json::json!({
                    "type": "function",
                    "function": {
                        "name": t.name,
                        "description": t.description,
                        "parameters": t.parameters,
                    }
                })
            })
            .collect();
        self
    }

    /// Приложить картинки к запросу (rev69).
    pub fn with_images(mut self, images: Vec<ImageAttachment>) -> Self {
        self.images = images;
        self
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn roles_serialize_lowercase() {
        assert_eq!(
            serde_json::to_string(&Role::Assistant).unwrap(),
            "\"assistant\""
        );
    }

    #[test]
    fn request_serializes_stream_true_by_default() {
        let r = ChatRequest::new("m", vec![ChatMessage::user("hi")]);
        let v: serde_json::Value = serde_json::to_value(&r).unwrap();
        assert_eq!(v["stream"], serde_json::json!(true));
        assert_eq!(v["model"], serde_json::json!("m"));
    }

    #[test]
    fn empty_optional_fields_are_omitted() {
        // Провоз пустых полей ломает строгие OpenAI-совместимые серверы.
        let r = ChatRequest::new("m", vec![ChatMessage::user("hi")]);
        let s = serde_json::to_string(&r).unwrap();
        assert!(!s.contains("temperature"), "{s}");
        assert!(!s.contains("tools"), "{s}");
    }

    #[test]
    fn tool_specs_wrap_into_function_envelope() {
        let spec = ToolSpec {
            name: "get_weather".into(),
            description: "Погода города".into(),
            parameters: serde_json::json!({
                "type": "object",
                "properties": { "city": { "type": "string" } },
                "required": ["city"]
            }),
        };
        let r = ChatRequest::new("m", vec![]).with_tools(&[spec]);
        assert_eq!(r.tools[0]["type"], serde_json::json!("function"));
        assert_eq!(
            r.tools[0]["function"]["name"],
            serde_json::json!("get_weather")
        );
        assert_eq!(
            r.tools[0]["function"]["parameters"]["required"][0],
            serde_json::json!("city")
        );
    }

    #[test]
    fn reasoning_is_a_separate_field_not_merged_into_content() {
        let m = ChatMessage {
            role: Role::Assistant,
            content: "answer".into(),
            reasoning: "thinking".into(),
            tool_calls: vec![],
            tool_call_id: None,
        };
        assert_eq!(m.content, "answer");
        assert_eq!(m.reasoning, "thinking");
        assert_ne!(m.content, m.reasoning);
    }

    #[test]
    fn tool_calls_serialize_as_openai_wire_envelope() {
        let m = ChatMessage {
            role: Role::Assistant,
            content: String::new(),
            reasoning: String::new(),
            tool_calls: vec![ToolCall {
                id: "c1".into(),
                name: "read".into(),
                arguments: serde_json::json!({ "path": "a.txt" })
                    .as_object()
                    .cloned()
                    .unwrap(),
            }],
            tool_call_id: None,
        };
        let v: serde_json::Value = serde_json::to_value(&m).unwrap();
        let tc = &v["tool_calls"][0];
        assert_eq!(tc["id"], serde_json::json!("c1"));
        assert_eq!(tc["type"], serde_json::json!("function"));
        assert_eq!(tc["function"]["name"], serde_json::json!("read"));
        // arguments — строка (JSON-текст), а не объект: иначе шлюз даёт 400.
        assert!(tc["function"]["arguments"].is_string());
        let parsed: serde_json::Value =
            serde_json::from_str(tc["function"]["arguments"].as_str().unwrap()).unwrap();
        assert_eq!(parsed["path"], serde_json::json!("a.txt"));
        // content присутствует всегда и строкой: шлюз требует ключ даже у
        // assistant без текста (только tool_calls).
        assert_eq!(v["content"], serde_json::json!(""));
    }

    #[test]
    fn content_key_always_present_as_string() {
        let m = ChatMessage {
            role: Role::Tool,
            content: String::new(),
            reasoning: String::new(),
            tool_calls: vec![],
            tool_call_id: Some("c9".into()),
        };
        let v: serde_json::Value = serde_json::to_value(&m).unwrap();
        assert_eq!(v["content"], serde_json::json!(""));
        assert_eq!(v["tool_call_id"], serde_json::json!("c9"));
    }
}
