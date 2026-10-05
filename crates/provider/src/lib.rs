/*! OpenAI-совместимый провайдер (D-005).

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

Также: `usage` в чанках приходит `null`, поэтому токены и стоимость считаются
на стороне клиента, а не берутся у провайдера.
*/

pub mod sse;
pub mod types;

#[cfg(feature = "http")]
pub mod http;

pub use sse::{SseParser, StreamEvent};
pub use types::{ChatMessage, ChatRequest, Role, ToolCall, ToolSpec};

#[cfg(feature = "http")]
pub use http::Provider;
