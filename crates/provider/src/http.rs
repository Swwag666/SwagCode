/*! HTTP-транспорт к OpenAI-совместимому эндпоинту.

Намеренно отделён от [`crate::sse`]: парсер не знает про сеть и тестируется
headless, а этот модуль — единственное место, где живёт reqwest.
*/

use std::time::Duration;

use futures_util::StreamExt;
use thiserror::Error;
use tokio::sync::mpsc;

use crate::sse::{SseParser, StreamEvent};
use crate::types::ChatRequest;

#[derive(Debug, Error)]
pub enum ProviderError {
    #[error("http: {0}")]
    Http(String),
    #[error("provider returned {status}: {body}")]
    Status { status: u16, body: String },
    #[error("config: {0}")]
    Config(String),
}

/// Клиент OpenAI-совместимого эндпоинта (B-6: конкретная реализация
/// trait [`crate::Provider`]). Дёшев в создании, клонируется свободно.
///
/// Тот же протокол konuşат Ollama (`/v1`) и llama.cpp (`--api`): для них
/// base_url другой, а api_key может быть пустым — [`OpenAiProvider::new_open`].
#[derive(Debug, Clone)]
pub struct OpenAiProvider {
    base_url: String,
    api_key: String,
    client: reqwest::Client,
}

impl OpenAiProvider {
    /// `base_url` — например `https://rustvy.xyz/v1`. Ключ обязателен:
    /// облачный эндпоинт без ключа — почти всегда ошибка конфигурации (D-009).
    pub fn new(
        base_url: impl Into<String>,
        api_key: impl Into<String>,
    ) -> Result<Self, ProviderError> {
        let base_url = base_url.into();
        let api_key = api_key.into();
        if base_url.trim().is_empty() {
            return Err(ProviderError::Config("пустой base_url".into()));
        }
        if api_key.trim().is_empty() {
            return Err(ProviderError::Config(
                "пустой api_key (D-009: ключ читается из .env)".into(),
            ));
        }
        Self::build(base_url, api_key)
    }

    /// Локальный эндпоинт без ключа: Ollama, llama.cpp, vLLM без auth.
    /// Пустой ключ разрешён намеренно — Authorization не отправляется вовсе.
    pub fn new_open(base_url: impl Into<String>) -> Result<Self, ProviderError> {
        let base_url = base_url.into();
        if base_url.trim().is_empty() {
            return Err(ProviderError::Config("пустой base_url".into()));
        }
        Self::build(base_url, String::new())
    }

    fn build(base_url: String, api_key: String) -> Result<Self, ProviderError> {
        let client = reqwest::Client::builder()
            // Таймаут только на соединение: стрим может идти минуты, и обрывать
            // его по общему таймауту нельзя — потеряем ход.
            .connect_timeout(Duration::from_secs(15))
            .tcp_nodelay(true)
            .build()
            .map_err(|e| ProviderError::Http(e.to_string()))?;
        Ok(Self {
            base_url: base_url.trim_end_matches('/').to_string(),
            api_key,
            client,
        })
    }

    /// Из переменных окружения `.env` (см. `.env.example`).
    pub fn from_env() -> Result<Self, ProviderError> {
        let base =
            std::env::var("SWAGCOD_BASE_URL").unwrap_or_else(|_| "https://rustvy.xyz/v1".into());
        let key = std::env::var("SWAGCOD_API_KEY").unwrap_or_default();
        Self::new(base, key)
    }

    pub fn base_url(&self) -> &str {
        &self.base_url
    }

    /// Заголовок Authorization: пустой ключ = заголовка нет (локальные серверы).
    fn auth(&self, rb: reqwest::RequestBuilder) -> reqwest::RequestBuilder {
        if self.api_key.is_empty() {
            rb
        } else {
            rb.bearer_auth(&self.api_key)
        }
    }

    /// E-2: эмбеддинги — POST /embeddings, OpenAI-совместимый формат.
    /// Векторы приходят в `data[]` в произвольном порядке — [`parse_embeddings`]
    /// сортирует их по `index`.
    pub async fn embeddings(
        &self,
        model: &str,
        input: &[String],
    ) -> Result<Vec<Vec<f32>>, ProviderError> {
        let body = serde_json::json!({ "model": model, "input": input });
        let resp = self
            .auth(self.client.post(format!("{}/embeddings", self.base_url)).json(&body))
            .send()
            .await
            .map_err(|e| ProviderError::Http(e.to_string()))?;
        let status = resp.status();
        let text = resp
            .text()
            .await
            .map_err(|e| ProviderError::Http(e.to_string()))?;
        if !status.is_success() {
            return Err(ProviderError::Status {
                status: status.as_u16(),
                body: text.chars().take(500).collect(),
            });
        }
        let json: serde_json::Value = serde_json::from_str(&text)
            .map_err(|e| ProviderError::Http(format!("embeddings json: {e}")))?;
        parse_embeddings(&json)
    }

    /// Список моделей провайдера. Возвращает сырой JSON: формат `data[]`
    /// различается у агрегаторов, а нормализация нам пока не нужна.
    pub async fn list_models(&self) -> Result<serde_json::Value, ProviderError> {
        let resp = self
            .auth(self.client.get(format!("{}/models", self.base_url)))
            .send()
            .await
            .map_err(|e| ProviderError::Http(e.to_string()))?;
        let status = resp.status();
        let text = resp
            .text()
            .await
            .map_err(|e| ProviderError::Http(e.to_string()))?;
        if !status.is_success() {
            return Err(ProviderError::Status {
                status: status.as_u16(),
                body: text.chars().take(500).collect(),
            });
        }
        serde_json::from_str(&text).map_err(|e| ProviderError::Http(format!("models json: {e}")))
    }

    /// Запустить стрим. События уходят в канал; функция возвращает handle,
    /// чтобы вызывающий мог читать канал и при желании отменить ход.
    ///
    /// Канал **bounded** намеренно: это backpressure. Если UI не успевает
    /// разбирать события, провайдер притормозит, а память не вырастет до
    /// бесконечности (требование DECISIONS.md §2).
    pub fn stream(
        &self,
        req: ChatRequest,
    ) -> Result<(mpsc::Receiver<StreamEvent>, tokio::task::JoinHandle<()>), ProviderError> {
        let url = format!("{}/chat/completions", self.base_url);
        let key = self.api_key.clone();
        let client = self.client.clone();
        let (tx, rx) = mpsc::channel::<StreamEvent>(256);

        let handle = tokio::spawn(async move {
            let body = match serde_json::to_string(&req) {
                Ok(b) => b,
                Err(e) => {
                    let _ = tx
                        .send(StreamEvent::Error(format!("serialize request: {e}")))
                        .await;
                    return;
                }
            };
            let mut rb = client
                .post(&url)
                .header("accept", "text/event-stream")
                .json(&serde_json::from_str::<serde_json::Value>(&body).unwrap_or_default());
            if !key.is_empty() {
                rb = rb.bearer_auth(&key);
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
                let _ = tx
                    .send(StreamEvent::Error(format!(
                        "provider {status}: {}",
                        text.chars().take(300).collect::<String>()
                    )))
                    .await;
                return;
            }

            let mut stream = resp.bytes_stream();
            let mut parser = SseParser::new();
            while let Some(item) = stream.next().await {
                let bytes = match item {
                    Ok(b) => b,
                    Err(e) => {
                        // Обрыв соединения: дожимаем накопленные тулзы.
                        for ev in parser.finish() {
                            if tx.send(ev).await.is_err() {
                                return;
                            }
                        }
                        let _ = tx.send(StreamEvent::Error(format!("stream: {e}"))).await;
                        return;
                    }
                };
                for ev in parser.feed(&bytes) {
                    // send().is_err() = получатель выпал (UI закрыт) — выходим.
                    if tx.send(ev).await.is_err() {
                        return;
                    }
                }
                if parser.is_finished() {
                    break;
                }
            }
            for ev in parser.finish() {
                if tx.send(ev).await.is_err() {
                    return;
                }
            }
        });
        Ok((rx, handle))
    }
}

/// Чистый парсер ответа `/embeddings`: `data[]` сортируется по `index`
/// (провайдеры отдают элементы в произвольном порядке), векторы — f32.
/// Функция не знает про сеть и тестируется headless.
pub fn parse_embeddings(v: &serde_json::Value) -> Result<Vec<Vec<f32>>, ProviderError> {
    let data = v
        .get("data")
        .and_then(|d| d.as_array())
        .ok_or_else(|| ProviderError::Http("embeddings: в ответе нет data[]".into()))?;
    if data.is_empty() {
        return Err(ProviderError::Http("embeddings: пустой data[]".into()));
    }
    let mut items: Vec<(i64, Vec<f32>)> = Vec::with_capacity(data.len());
    for (i, item) in data.iter().enumerate() {
        let idx = item.get("index").and_then(|x| x.as_i64()).unwrap_or(i as i64);
        let emb = item
            .get("embedding")
            .and_then(|e| e.as_array())
            .ok_or_else(|| ProviderError::Http(format!("embeddings: у data[{i}] нет embedding[]")))?;
        let vec: Vec<f32> = emb
            .iter()
            .filter_map(|x| x.as_f64().map(|f| f as f32))
            .collect();
        if vec.is_empty() {
            return Err(ProviderError::Http(format!("embeddings: data[{i}] — пустой вектор")));
        }
        items.push((idx, vec));
    }
    items.sort_by_key(|(idx, _)| *idx);
    Ok(items.into_iter().map(|(_, v)| v).collect())
}

#[cfg(test)]
mod embeddings_tests {
    use super::parse_embeddings;
    use serde_json::json;

    #[test]
    fn parses_and_sorts_by_index() {
        let v = json!({"data": [
            {"index": 1, "embedding": [3.0, 4.0]},
            {"index": 0, "embedding": [1.0, 2.0]}
        ]});
        let out = parse_embeddings(&v).unwrap();
        assert_eq!(out.len(), 2);
        assert_eq!(out[0], vec![1.0, 2.0]);
        assert_eq!(out[1], vec![3.0, 4.0]);
    }

    #[test]
    fn missing_data_is_error() {
        assert!(parse_embeddings(&json!({})).is_err());
        assert!(parse_embeddings(&json!({"data": []})).is_err());
    }

    #[test]
    fn empty_vector_is_error() {
        let v = json!({"data": [{"index": 0, "embedding": []}]});
        assert!(parse_embeddings(&v).is_err());
    }

    #[test]
    fn item_without_embedding_is_error() {
        let v = json!({"data": [{"index": 0}]});
        assert!(parse_embeddings(&v).is_err());
    }
}
