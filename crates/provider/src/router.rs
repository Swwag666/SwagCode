/*! Маршрутизация провайдеров: trait и fallback-цепочка (этап B-6).

[`Router`] оборачивает список OpenAI-совместимых эндпоинтов и переключается
на следующий при ошибке соединения, 429 или 5xx — с экспоненциальным
backoff и jitter, чтобы не добивать лежащий сервер синхронными ретраями.

Граница переключения — ПЕРВОЕ событие стрима: пока контент не потёк,
смена эндпоинта бесплатна для контекста; после первого токена ошибки
форвардятся как есть (середина хода не мигрирует: целостность ответа
важнее живучести).
*/

use std::time::Duration;

use tokio::sync::mpsc;

use crate::http::{OpenAiProvider, ProviderError};
use crate::sse::StreamEvent;
use crate::types::ChatRequest;

/// Единый интерфейс провайдера (план B-6): stream, list_models.
/// Точный счёт токенов (usage) приходит событием [`StreamEvent::Usage`]
/// внутри стрима — отдельным методом он не выразим.
pub trait Provider: Send + Sync {
    /// Человекочитаемое имя для логов и UI.
    fn name(&self) -> &str;

    fn stream(
        &self,
        req: ChatRequest,
    ) -> Result<(mpsc::Receiver<StreamEvent>, tokio::task::JoinHandle<()>), ProviderError>;

    fn list_models(
        &self,
    ) -> impl std::future::Future<Output = Result<serde_json::Value, ProviderError>> + Send;
}

impl Provider for OpenAiProvider {
    fn name(&self) -> &str {
        self.base_url()
    }

    fn stream(
        &self,
        req: ChatRequest,
    ) -> Result<(mpsc::Receiver<StreamEvent>, tokio::task::JoinHandle<()>), ProviderError> {
        OpenAiProvider::stream(self, req)
    }

    async fn list_models(&self) -> Result<serde_json::Value, ProviderError> {
        OpenAiProvider::list_models(self).await
    }
}

/// Одна точка fallback-цепочки: эндпоинт + опциональная подмена модели.
#[derive(Debug, Clone)]
pub struct RouterEndpoint {
    pub provider: OpenAiProvider,
    /// Если задана, запрос уходит с этой моделью (локальная Ollama живёт
    /// со своим именем модели, не как облако).
    pub model: Option<String>,
}

/// Fallback-цепочка эндпоинтов.
#[derive(Debug, Clone)]
pub struct Router {
    endpoints: Vec<RouterEndpoint>,
    label: String,
}

/// Сколько ждём первое событие, прежде чем счесть эндпоинт лежащим.
/// Секунды, не миллисекунды: reasoning-модели думают до первого чанка.
pub const FIRST_EVENT_TIMEOUT: Duration = Duration::from_secs(60);

impl Router {
    pub fn new(endpoints: Vec<RouterEndpoint>) -> Result<Self, ProviderError> {
        if endpoints.is_empty() {
            return Err(ProviderError::Config("пустая цепочка эндпоинтов".into()));
        }
        let label = format!("router({})", endpoints.len());
        Ok(Self { endpoints, label })
    }

    /// Цепочка из окружения: основной эндпоинт `SWAGCOD_BASE_URL`/`SWAGCOD_API_KEY`
    /// плюс `SWAGCOD_FALLBACKS` = `url|key|model;url|key` (key и model необязательны,
    /// пустой key разрешён — локальные Ollama/llama.cpp).
    pub fn from_env() -> Result<Self, ProviderError> {
        Self::from_env_with_key(None)
    }

    /// То же, что [`Router::from_env`], но ключ основного эндпоинта можно
    /// передать извне (B-7: расшифрованный DPAPI-ключ из защищённого хранилища
    /// приоритетнее голого env). Пустой ключ основного эндпоинта допустим
    /// только для локальных баз — тогда создается open-провайдер.
    pub fn from_env_with_key(key_override: Option<String>) -> Result<Self, ProviderError> {
        let base =
            std::env::var("SWAGCOD_BASE_URL").unwrap_or_else(|_| "https://rustvy.xyz/v1".into());
        let env_key = std::env::var("SWAGCOD_API_KEY").unwrap_or_default();
        let key = key_override
            .filter(|k| !k.trim().is_empty())
            .unwrap_or(env_key);
        let primary = if key.trim().is_empty() {
            let lower = base.to_lowercase();
            if lower.contains("localhost") || lower.contains("127.0.0.1") {
                OpenAiProvider::new_open(&base)?
            } else {
                // Облако без ключа — ошибка конфигурации (D-009).
                return Err(ProviderError::Config(
                    "пустой api_key (D-009: ключ читается из .env или DPAPI-хранилища)".into(),
                ));
            }
        } else {
            OpenAiProvider::new(&base, &key)?
        };
        let mut endpoints = vec![RouterEndpoint { provider: primary, model: None }];
        if let Ok(spec) = std::env::var("SWAGCOD_FALLBACKS") {
            for (base, key, model) in parse_fallbacks(&spec) {
                let provider = if key.is_empty() {
                    match OpenAiProvider::new_open(&base) {
                        Ok(p) => p,
                        Err(_) => continue,
                    }
                } else {
                    match OpenAiProvider::new(&base, &key) {
                        Ok(p) => p,
                        Err(_) => continue,
                    }
                };
                endpoints.push(RouterEndpoint { provider, model });
            }
        }
        Self::new(endpoints)
    }

    pub fn len(&self) -> usize {
        self.endpoints.len()
    }

    pub fn is_empty(&self) -> bool {
        self.endpoints.is_empty()
    }

    /// E-2: эмбеддинги через всю fallback-цепочку. Модель общая для всех
    /// точек: chat-подмены моделей из `SWAGCOD_FALLBACKS` не применяются —
    /// эмбеддинги своя семья моделей (см. [`embeddings_model`]). Между
    /// попытками — тот же backoff с jitter, что и у чата.
    pub async fn embeddings(&self, input: &[String]) -> Result<Vec<Vec<f32>>, ProviderError> {
        if input.is_empty() {
            return Ok(Vec::new());
        }
        let model = embeddings_model();
        let mut last: Option<ProviderError> = None;
        for (attempt, ep) in self.endpoints.iter().enumerate() {
            if attempt > 0 {
                tokio::time::sleep(backoff_delay(attempt as u32, attempt as u64)).await;
            }
            match ep.provider.embeddings(&model, input).await {
                Ok(v) => return Ok(v),
                Err(e) => {
                    eprintln!("embeddings: эндпоинт {} отпал: {e}", ep.provider.base_url());
                    last = Some(e);
                }
            }
        }
        Err(last.unwrap_or_else(|| ProviderError::Config("пустая цепочка эндпоинтов".into())))
    }
}

/// E-2: модель эмбеддингов — своя env-переменная; дефолт соответствует
/// публичному `/v1/embeddings`-семейству. Локальные серверы (Ollama,
/// llama.cpp с флагом embedding) переопределяют её своей моделью.
pub fn embeddings_model() -> String {
    std::env::var("SWAGCOD_EMBEDDINGS_MODEL").unwrap_or_else(|_| "text-embedding-3-small".into())
}

/// Разбор `SWAGCOD_FALLBACKS`: точки через ';', поля через '|'.
/// Мусорные записи (без url) отбрасываются молча: одна кривая строка
/// не должна ронать всю маршрутизацию.
pub fn parse_fallbacks(spec: &str) -> Vec<(String, String, Option<String>)> {
    let mut out = Vec::new();
    for part in spec.split(';') {
        let part = part.trim();
        if part.is_empty() {
            continue;
        }
        let fields: Vec<&str> = part.split('|').map(|f| f.trim()).collect();
        let url = fields[0].to_string();
        if url.is_empty() {
            continue;
        }
        let key = fields.get(1).unwrap_or(&"").to_string();
        let model = fields
            .get(2)
            .filter(|m| !m.is_empty())
            .map(|m| m.to_string());
        out.push((url, key, model));
    }
    out
}

/// Экспоненциальный backoff с jitter: 500 мс * 2^attempt, ±30%, потолок 8 с.
/// Чистая функция от attempt и источника энтропии — тестируется без сна.
pub fn backoff_delay(attempt: u32, jitter_source: u64) -> Duration {
    const BASE_MS: u64 = 500;
    const CAP_MS: u64 = 8_000;
    let exp = BASE_MS.saturating_mul(1u64 << attempt.min(6));
    let capped = exp.min(CAP_MS);
    // jitter ±30%: детерминированно от jitter_source (наносекунды в бою).
    let spread = (capped as f64 * 0.3) as u64;
    let jitter = if spread == 0 {
        0
    } else {
        jitter_source % (spread * 2 + 1)
    };
    Duration::from_millis(capped + jitter - spread)
}

impl Provider for Router {
    fn name(&self) -> &str {
        &self.label
    }

    fn stream(
        &self,
        req: ChatRequest,
    ) -> Result<(mpsc::Receiver<StreamEvent>, tokio::task::JoinHandle<()>), ProviderError> {
        let endpoints = self.endpoints.clone();
        let (tx, rx) = mpsc::channel::<StreamEvent>(256);
        let handle = tokio::spawn(async move {
            let total = endpoints.len();
            let mut last_error = String::from("цепочка пуста");
            for (i, ep) in endpoints.iter().enumerate() {
                let mut req = req.clone();
                if let Some(m) = &ep.model {
                    req.model = m.clone();
                }
                match ep.provider.stream(req) {
                    Ok((mut erx, ehandle)) => {
                        // Ждём первое событие: Error до контента = эндпоинт
                        // лежит/отказал, можно переключаться без потерь.
                        let first =
                            tokio::time::timeout(FIRST_EVENT_TIMEOUT, erx.recv()).await;
                        match first {
                            Ok(Some(StreamEvent::Error(m))) => {
                                last_error = m;
                                ehandle.abort();
                            }
                            Ok(Some(ev)) => {
                                // Эндпоинт жив: форвардим первое и остальные.
                                if tx.send(ev).await.is_err() {
                                    ehandle.abort();
                                    return;
                                }
                                while let Some(ev) = erx.recv().await {
                                    if tx.send(ev).await.is_err() {
                                        break;
                                    }
                                }
                                return;
                            }
                            Ok(None) => {
                                last_error = "стрим закрылся без событий".into();
                                ehandle.abort();
                            }
                            Err(_) => {
                                last_error = format!(
                                    "нет первого события за {} с",
                                    FIRST_EVENT_TIMEOUT.as_secs()
                                );
                                ehandle.abort();
                            }
                        }
                    }
                    Err(e) => {
                        last_error = e.to_string();
                    }
                }
                if i + 1 < total {
                    let jitter = std::time::SystemTime::now()
                        .duration_since(std::time::UNIX_EPOCH)
                        .map(|d| d.subsec_nanos() as u64)
                        .unwrap_or(0);
                    tokio::time::sleep(backoff_delay(i as u32, jitter)).await;
                }
            }
            let _ = tx.send(StreamEvent::Error(last_error)).await;
        });
        Ok((rx, handle))
    }

    async fn list_models(&self) -> Result<serde_json::Value, ProviderError> {
        let mut last = Err(ProviderError::Config("цепочка пуста".into()));
        for ep in &self.endpoints {
            match ep.provider.list_models().await {
                Ok(v) => return Ok(v),
                Err(e) => last = Err(e),
            }
        }
        last
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use tokio::io::{AsyncReadExt, AsyncWriteExt};
    use tokio::net::TcpListener;

    #[test]
    fn parse_fallbacks_spec() {
        let spec = "https://a.xyz/v1|key-a|model-a; http://localhost:11434/v1 ; |bad;https://b.xyz/v1|key-b";
        let parsed = parse_fallbacks(spec);
        assert_eq!(parsed.len(), 3);
        assert_eq!(
            parsed[0],
            (
                "https://a.xyz/v1".into(),
                "key-a".into(),
                Some("model-a".into())
            )
        );
        assert_eq!(parsed[1].0, "http://localhost:11434/v1");
        assert_eq!(parsed[1].1, "");
        assert_eq!(parsed[1].2, None);
        assert_eq!(parsed[2], ("https://b.xyz/v1".into(), "key-b".into(), None));
        assert!(parse_fallbacks("").is_empty());
        assert!(parse_fallbacks(";;;").is_empty());
    }

    #[test]
    fn backoff_grows_with_cap_and_jitter_bounds() {
        for jitter_source in [0u64, 42, 123_456_789] {
            let d0 = backoff_delay(0, jitter_source);
            assert!((350..=650).contains(&d0.as_millis()), "attempt 0: {d0:?}");
            let d1 = backoff_delay(1, jitter_source);
            assert!((700..=1300).contains(&d1.as_millis()), "attempt 1: {d1:?}");
            let d10 = backoff_delay(10, jitter_source);
            assert!((5600..=10400).contains(&d10.as_millis()), "cap: {d10:?}");
        }
        // Детерминизм: один вход — один выход.
        assert_eq!(backoff_delay(2, 7), backoff_delay(2, 7));
        assert_ne!(backoff_delay(2, 7), backoff_delay(2, 8));
    }

    #[test]
    fn router_requires_at_least_one_endpoint() {
        assert!(Router::new(vec![]).is_err());
    }

    /// Минимальный SSE-сервер на один запрос: отдаёт «привет» и закрывается.
    async fn one_shot_sse_server() -> String {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        tokio::spawn(async move {
            if let Ok((mut sock, _)) = listener.accept().await {
                let mut buf = [0u8; 4096];
                let _ = tokio::time::timeout(Duration::from_secs(5), sock.read(&mut buf)).await;
                let body = "data: {\"choices\":[{\"delta\":{\"content\":\"привет\"},\"finish_reason\":\"stop\"}]}\n\n\
                            data: [DONE]\n\n";
                let resp = format!(
                    "HTTP/1.1 200 OK\r\ncontent-type: text/event-stream\r\ncontent-length: {}\r\n\r\n{}",
                    body.len(),
                    body
                );
                let _ = sock.write_all(resp.as_bytes()).await;
                let _ = sock.shutdown().await;
            }
        });
        format!("http://{addr}/v1")
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn router_falls_back_from_dead_endpoint_to_live_one() {
        let good_base = one_shot_sse_server().await;
        // Мёртвый порт: соединение отклоняется немедленно.
        let dead = OpenAiProvider::new_open("http://127.0.0.1:9/v1").unwrap();
        let good = OpenAiProvider::new_open(&good_base).unwrap();
        let router = Router::new(vec![
            RouterEndpoint { provider: dead, model: None },
            RouterEndpoint { provider: good, model: Some("local-model".into()) },
        ])
        .unwrap();

        let req = ChatRequest::new("cloud-model", vec![crate::types::ChatMessage::user("hi")]);
        let (mut rx, _handle) = router.stream(req).unwrap();
        let mut got_content = None;
        let mut got_error = None;
        while let Some(ev) = tokio::time::timeout(Duration::from_secs(20), rx.recv())
            .await
            .expect("роутер завис")
        {
            match ev {
                StreamEvent::Content(t) => got_content = Some(t),
                StreamEvent::Error(m) => got_error = Some(m),
                _ => {}
            }
        }
        assert!(got_error.is_none(), "живой эндпоинт не должен дать ошибку: {got_error:?}");
        assert_eq!(got_content.as_deref(), Some("привет"));
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn router_reports_error_when_all_endpoints_dead() {
        let dead1 = OpenAiProvider::new_open("http://127.0.0.1:9/v1").unwrap();
        let dead2 = OpenAiProvider::new_open("http://127.0.0.1:9/v2").unwrap();
        let router = Router::new(vec![
            RouterEndpoint { provider: dead1, model: None },
            RouterEndpoint { provider: dead2, model: None },
        ])
        .unwrap();
        let req = ChatRequest::new("m", vec![crate::types::ChatMessage::user("hi")]);
        let (mut rx, _handle) = router.stream(req).unwrap();
        let mut errors = 0;
        while let Some(ev) = tokio::time::timeout(Duration::from_secs(20), rx.recv())
            .await
            .expect("роутер завис")
        {
            if matches!(ev, StreamEvent::Error(_)) {
                errors += 1;
            }
        }
        // Ровно одна итоговая ошибка: промежуточные не утекают (иначе
        // драйвер счёл бы ход проваленным до успешного ретрая).
        assert_eq!(errors, 1);
    }

    /// Эндпоинт, отвечающий 500 на любой запрос.
    async fn error_server(status: u16) -> String {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        tokio::spawn(async move {
            while let Ok((mut sock, _)) = listener.accept().await {
                let mut buf = [0u8; 4096];
                let _ = tokio::time::timeout(Duration::from_millis(500), sock.read(&mut buf))
                    .await;
                let resp = format!("HTTP/1.1 {status} ERR\r\ncontent-length: 4\r\n\r\noops");
                let _ = sock.write_all(resp.as_bytes()).await;
                let _ = sock.shutdown().await;
            }
        });
        format!("http://{addr}/v1")
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn http_500_is_retryable_and_reaches_next_endpoint() {
        let bad_base = error_server(500).await;
        let good_base = one_shot_sse_server().await;
        let router = Router::new(vec![
            RouterEndpoint {
                provider: OpenAiProvider::new_open(&bad_base).unwrap(),
                model: None,
            },
            RouterEndpoint {
                provider: OpenAiProvider::new_open(&good_base).unwrap(),
                model: None,
            },
        ])
        .unwrap();
        let req = ChatRequest::new("m", vec![crate::types::ChatMessage::user("hi")]);
        let (mut rx, _handle) = router.stream(req).unwrap();
        let mut content = None;
        while let Some(ev) = tokio::time::timeout(Duration::from_secs(20), rx.recv())
            .await
            .expect("роутер завис")
        {
            if let StreamEvent::Content(t) = ev {
                content = Some(t);
            }
        }
        assert_eq!(content.as_deref(), Some("привет"));
    }
}
