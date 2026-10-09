/*! OpenAI-совместимый провайдер (D-005) и маршрутизация (B-6).

Протокол: `POST /chat/completions` с `stream: true`, ответ — SSE.

Три особенности формата, подтверждённые живыми запросами к эндпоинту
(см. DECISIONS.md §5.6) и зафиксированные тестами в [`sse`]:

1. Каждый чанк несёт `delta.reasoning_content` параллельно с `delta.content`.
   Наивный парсер, читающий только `content`, показывает пустоту на несколько
   секунд в начале каждого хода.
2. `content` приходит пустой строкой даже когда есть reasoning. Пустая строка
   не является событием и не должна триггерить перерисовку.
3. `delta.tool_calls` приходит инкрементально: `id`/`name` в первом чанке,
   `arguments` — кусками JSON-строки в последующих. Аргументы буферизуются по
   индексу и парсятся только после `finish_reason`.

Также: `usage` в чанках живого эндпоинта приходит `null`, поэтому токены
считаются на стороне клиента (калибровка B-3); провайдеры, которые usage
присылают, отдают его событием [`StreamEvent::Usage`].

B-6: [`Provider`] — trait (stream, list_models), [`OpenAiProvider`] —
реализация для OpenAI-совместимых эндпоинтов (включая Ollama/llama.cpp с
пустым ключом), [`Router`] — fallback-цепочка с backoff и jitter.
*/

pub mod presets;
pub mod sse;
pub mod types;

#[cfg(feature = "http")]
pub mod anthropic;
#[cfg(feature = "http")]
pub mod any;
#[cfg(feature = "http")]
pub mod http;
#[cfg(feature = "http")]
pub mod router;

pub use presets::{find_preset, ProviderFlavor, ProviderPreset, PRESETS};
pub use sse::{SseParser, StreamEvent};
pub use types::{ChatMessage, ChatRequest, Role, ToolCall, ToolSpec};

#[cfg(feature = "http")]
pub use anthropic::{anthropic_body, AnthropicError, AnthropicProvider, ANTHROPIC_VERSION};
#[cfg(feature = "http")]
pub use any::AnyProvider;
#[cfg(feature = "http")]
pub use http::{parse_embeddings, OpenAiProvider, ProviderError};
#[cfg(feature = "http")]
pub use router::{
    backoff_delay, embeddings_model, parse_fallbacks, Provider, Router, RouterEndpoint,
};
