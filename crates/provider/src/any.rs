/*! Активный провайдер хода: OpenAI-цепочка или нативный Anthropic.

Драйвер (`send_message` в ядре) работает с событиями [`crate::sse::StreamEvent`]
и не знает, кто их произвёл: оба варианта отдают один и тот же поток.
Эмбеддинги здесь не живут — у Anthropic их нет в этом протоколе, семантика
продолжает ходить через env-Router (`/v1/embeddings`).
*/

use tokio::sync::mpsc;

use crate::anthropic::AnthropicProvider;
use crate::echo::{EchoProvider, ECHO_NAME};
use crate::http::ProviderError;
use crate::router::{Provider, Router};
use crate::sse::StreamEvent;
use crate::types::ChatRequest;

/// Любой настроенный чат-провайдер. `Box` не нужен: вариант выбирается
/// один раз при построении хода.
#[derive(Debug, Clone)]
pub enum AnyProvider {
    OpenAi(Router),
    Anthropic(AnthropicProvider),
    /// F-11: встроенное эхо для сквозных проверок. Включается env-флагом
    /// `SWAGCOD_ECHO_PROVIDER` в `build_chat_provider`; сети и ключей не
    /// требует, поэтому сквозной тест в CI проходит весь ход целиком.
    Echo(EchoProvider),
}

impl AnyProvider {
    pub fn name(&self) -> String {
        match self {
            Self::OpenAi(r) => r.name().to_string(),
            Self::Anthropic(p) => format!("anthropic({})", p.base_url()),
            Self::Echo(_) => ECHO_NAME.to_string(),
        }
    }

    #[allow(clippy::type_complexity)]
    pub fn stream(
        &self,
        req: ChatRequest,
    ) -> Result<(mpsc::Receiver<StreamEvent>, tokio::task::JoinHandle<()>), ProviderError> {
        match self {
            Self::OpenAi(r) => r.stream(req),
            Self::Anthropic(p) => p.stream(req).map_err(ProviderError::from),
            Self::Echo(p) => p.stream(req),
        }
    }

    pub async fn list_models(&self) -> Result<serde_json::Value, ProviderError> {
        match self {
            Self::OpenAi(r) => r.list_models().await,
            Self::Anthropic(p) => p.list_models().await.map_err(ProviderError::from),
            Self::Echo(p) => p.list_models().await,
        }
    }
}
