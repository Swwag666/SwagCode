/*! F-11: встроенный echo-провайдер — сквозная проверка хода без сети и ключей.

Включается env-флагом [`ECHO_ENV`] (паттерн fake-mcp: подделка живёт в
репозитории и не требует ни внешних зависимостей, ни секретов). Отдаёт тот же
поток [`StreamEvent`], что настоящий провайдер: рассуждение, контент несколькими
дельтами, счёт токенов и `Done`. Поэтому сквозная проверка проходит НАСКВОЗЬ —
драйвер хода, машина хода, хуки, шина событий, store и HTTP API, — а не упирается
в границу с сетью, где живой провайдер в CI недоступен и не должен быть доступен.

Ответ детерминирован: эхо последнего сообщения пользователя, нарезанное
одинаковыми кусками. Детерминизм принципиален — сквозной тест утверждает и
текст, и число токенов, а плавающий ответ превратил бы его в лотерею.

Токены считаются собственной оценкой «4 символа на токен», а не калибровкой ядра
(B-3): крейт `provider` не зависит от `core`, иначе получился бы цикл, а смысл
события [`StreamEvent::Usage`] в этой проверке — довести счёт до метрик и
истории, а не совпасть с боевой оценкой.
*/

use tokio::sync::mpsc;

use crate::http::ProviderError;
use crate::sse::{FinishReason, StreamEvent};
use crate::types::{ChatRequest, Role};

/// Env-флаг, включающий echo-провайдер.
pub const ECHO_ENV: &str = "SWAGCOD_ECHO_PROVIDER";

/// Имя провайдера для логов и UI: в журнале хода должно быть видно, что
/// ответило эхо, а не боевая модель.
pub const ECHO_NAME: &str = "echo";

/// Размер одной дельты контента в символах. Стрим обязан быть инкрементальным:
/// одна дельта на весь ответ прошла бы по другому пути и в UI, и в шине, и
/// сквозная проверка перестала бы проверять инкрементальность.
const CHUNK_CHARS: usize = 24;

/// Флаг выставлен и не означает «нет». Пустая строка, `0`, `false` и `off`
/// считаются выключенным: в CI переменную удобно объявлять пустой, чтобы
/// выключить, и это не должно молча включать эхо.
pub fn echo_enabled() -> bool {
    match std::env::var(ECHO_ENV) {
        Ok(raw) => {
            let v = raw.trim().to_ascii_lowercase();
            !v.is_empty() && v != "0" && v != "false" && v != "off"
        }
        Err(_) => false,
    }
}

/// Детерминированная оценка токенов: 4 символа на токен.
///
/// Пустой текст — честный ноль. Пол в один токен применяется к ИТОГУ запроса,
/// а не к каждому полю: иначе пустое `reasoning` стоило бы токен на каждом
/// сообщении, и счёт рос бы от числа сообщений, а не от текста.
///
/// Открыта наружу ради сквозного теста (F-11): он утверждает, что в запись
/// хода попал ИМЕННО счёт провайдера, а не оценка ядра, и для этого ему нужна
/// та же формула, а не её копия.
pub fn estimate_tokens(text: &str) -> u32 {
    (text.chars().count() as u32) / 4
}

/// Текст ответа: эхо последнего сообщения пользователя.
fn echo_body(last_user: &str) -> String {
    let text = last_user.trim();
    if text.is_empty() {
        "эхо: пустой запрос".to_string()
    } else {
        format!("эхо: {text}")
    }
}

/// Провайдер, который отвечает эхом. Состояния нет: он одинаков для всех
/// сессий и не ходит в сеть, поэтому экземпляр можно клонировать свободно.
#[derive(Debug, Clone, Copy, Default)]
pub struct EchoProvider;

impl EchoProvider {
    pub fn new() -> Self {
        Self
    }

    pub fn name(&self) -> &str {
        ECHO_NAME
    }

    /// Тот же контракт, что у [`crate::router::Provider::stream`]: канал
    /// событий и ручка задачи. Ошибка невозможна — сети нет, — но сигнатура
    /// совпадает, чтобы [`crate::any::AnyProvider`] переключался без особых
    /// случаев.
    #[allow(clippy::type_complexity)]
    pub fn stream(
        &self,
        req: ChatRequest,
    ) -> Result<(mpsc::Receiver<StreamEvent>, tokio::task::JoinHandle<()>), ProviderError> {
        let (tx, rx) = mpsc::channel(16);

        let last_user = req
            .messages
            .iter()
            .rev()
            .find(|m| m.role == Role::User)
            .map(|m| m.content.clone())
            .unwrap_or_default();
        // Считаем и контент, и рассуждения: в истории они оба хранятся и оба
        // стоят токенов.
        // Пол в один токен — у итога запроса: ноль в журнале неотличим от
        // «провайдер не прислал счёт», а такой ход должен быть виден.
        let input_tokens: u32 = req
            .messages
            .iter()
            .map(|m| estimate_tokens(&m.content) + estimate_tokens(&m.reasoning))
            .sum::<u32>()
            .max(1);
        let model = req.model.clone();

        let handle = tokio::spawn(async move {
            let _ = tx
                .send(StreamEvent::Reasoning(format!(
                    "echo({model}): принимаю запрос"
                )))
                .await;

            let body = echo_body(&last_user);
            let mut rest = body.as_str();
            while !rest.is_empty() {
                // Режем по границе символа, а не байта: текст русский, и
                // split_at посреди UTF-8 последовательности запаниковал бы.
                let cut = rest
                    .char_indices()
                    .nth(CHUNK_CHARS)
                    .map(|(i, _)| i)
                    .unwrap_or(rest.len());
                let (head, tail) = rest.split_at(cut);
                // Получатель мог исчезнуть (ход остановили) — выходим тихо,
                // иначе задача висела бы до конца стима.
                if tx
                    .send(StreamEvent::Content(head.to_string()))
                    .await
                    .is_err()
                {
                    return;
                }
                rest = tail;
            }

            let _ = tx
                .send(StreamEvent::Usage {
                    input_tokens,
                    output_tokens: estimate_tokens(&body),
                })
                .await;
            let _ = tx
                .send(StreamEvent::Done {
                    finish: FinishReason::Stop,
                })
                .await;
        });
        Ok((rx, handle))
    }

    /// Список моделей: одна запись, чтобы вкладка провайдеров и `/v1/models`
    /// не падали с эхом, а показывали честный пустяк.
    pub async fn list_models(&self) -> Result<serde_json::Value, ProviderError> {
        Ok(serde_json::json!({
            "object": "list",
            "data": [{ "id": ECHO_NAME, "object": "model", "owned_by": "swagcod" }],
        }))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::types::ChatMessage;

    fn request() -> ChatRequest {
        ChatRequest::new(
            "echo-model",
            vec![
                ChatMessage::system("ты эхо"),
                ChatMessage::user("привет мир"),
            ],
        )
    }

    async fn collect(req: ChatRequest) -> Vec<StreamEvent> {
        let (mut rx, handle) = EchoProvider::new().stream(req).expect("эхо не ошибается");
        let mut out = Vec::new();
        while let Some(ev) = rx.recv().await {
            out.push(ev);
        }
        handle.await.expect("задача эха не падает");
        out
    }

    #[tokio::test]
    async fn stream_yields_reasoning_content_usage_done_in_order() {
        let events = collect(request()).await;

        assert!(
            matches!(events.first(), Some(StreamEvent::Reasoning(_))),
            "первым идёт рассуждение: {events:?}"
        );
        assert!(
            matches!(
                events.last(),
                Some(StreamEvent::Done {
                    finish: FinishReason::Stop
                })
            ),
            "последним — Done/Stop: {events:?}"
        );
        let usage_at = events
            .iter()
            .position(|e| matches!(e, StreamEvent::Usage { .. }))
            .expect("событие Usage обязано быть");
        let first_content = events
            .iter()
            .position(|e| matches!(e, StreamEvent::Content(_)))
            .expect("контент обязан быть");
        assert!(
            first_content < usage_at,
            "счёт приходит после контента, как у живого провайдера: {events:?}"
        );
    }

    #[tokio::test]
    async fn content_is_an_echo_split_into_several_deltas() {
        let events = collect(request()).await;
        let text: String = events
            .iter()
            .filter_map(|e| match e {
                StreamEvent::Content(s) => Some(s.as_str()),
                _ => None,
            })
            .collect();
        assert_eq!(text, "эхо: привет мир");

        let deltas = events
            .iter()
            .filter(|e| matches!(e, StreamEvent::Content(_)))
            .count();
        // 15 символов короче куска, поэтому одна дельта; проверяем на длинном
        // тексте, что инкрементальность настоящая.
        assert_eq!(deltas, 1, "короткое эхо — одна дельта: {text}");

        let long = ChatRequest::new("echo-model", vec![ChatMessage::user("слово ".repeat(40))]);
        let events = collect(long).await;
        let deltas = events
            .iter()
            .filter(|e| matches!(e, StreamEvent::Content(_)))
            .count();
        assert!(deltas > 3, "длинное эхо обязано прийти кусками: {deltas}");
    }

    #[tokio::test]
    async fn usage_counts_input_and_output_deterministically() {
        let a = collect(request()).await;
        let b = collect(request()).await;
        let usage = |v: &Vec<StreamEvent>| {
            v.iter()
                .find_map(|e| match e {
                    StreamEvent::Usage {
                        input_tokens,
                        output_tokens,
                    } => Some((*input_tokens, *output_tokens)),
                    _ => None,
                })
                .expect("Usage обязателен: иначе сквозной тест не проверит токены")
        };
        assert_eq!(usage(&a), usage(&b), "счёт детерминирован");
        let (input, output) = usage(&a);
        assert!(
            input > 0 && output > 0,
            "нули неотличимы от отсутствия счёта"
        );
        // system «ты эхо» (6 символов → 1) + user «привет мир» (10 → 2) = 3;
        // пустые reasoning ничего не стоят — пол в один токен живёт у итога.
        assert_eq!(input, 3, "{a:?}");
        assert_eq!(output, estimate_tokens("эхо: привет мир"));
    }

    #[tokio::test]
    async fn empty_user_message_still_answers() {
        let events = collect(ChatRequest::new("m", vec![ChatMessage::user("   ")])).await;
        let text: String = events
            .iter()
            .filter_map(|e| match e {
                StreamEvent::Content(s) => Some(s.as_str()),
                _ => None,
            })
            .collect();
        assert_eq!(text, "эхо: пустой запрос");
    }

    #[tokio::test]
    async fn dropping_the_receiver_does_not_hang_the_task() {
        let (mut rx, handle) = EchoProvider::new()
            .stream(ChatRequest::new(
                "m",
                vec![ChatMessage::user("длинный ответ ".repeat(50))],
            ))
            .expect("поток");
        let first = rx.recv().await.expect("первое событие");
        assert!(matches!(first, StreamEvent::Reasoning(_)));
        drop(rx);
        // Остановка хода роняет получателя: задача обязана завершиться, а не
        // висеть вечно (иначе каждый остановленный ход утекал бы задачу).
        tokio::time::timeout(std::time::Duration::from_secs(5), handle)
            .await
            .expect("задача завершилась после drop получателя")
            .expect("без паники");
    }

    #[test]
    fn flag_treats_empty_and_zero_as_off() {
        // Один тест на всю работу с env: переменные процесса глобальны, и два
        // параллельных теста, которые их пишут, устроили бы гонку.
        let key = ECHO_ENV;
        let saved = std::env::var(key).ok();
        for (value, want) in [
            ("1", true),
            ("true", true),
            ("on", true),
            ("", false),
            ("0", false),
            ("false", false),
            ("off", false),
            ("  1  ", true),
        ] {
            std::env::set_var(key, value);
            assert_eq!(echo_enabled(), want, "значение {value:?}");
        }
        std::env::remove_var(key);
        assert!(!echo_enabled(), "без переменной эхо выключено");
        if let Some(v) = saved {
            std::env::set_var(key, v);
        }
    }

    #[test]
    fn estimate_is_four_chars_per_token() {
        assert_eq!(estimate_tokens(""), 0);
        assert_eq!(estimate_tokens("abc"), 0);
        assert_eq!(estimate_tokens("abcd"), 1);
        assert_eq!(estimate_tokens("abcde"), 1);
        assert_eq!(estimate_tokens("abcdefgh"), 2);
    }

    #[tokio::test]
    async fn request_without_messages_still_reports_one_token() {
        // Пол у итога: запрос без сообщений обязан дать 1, а не 0, иначе
        // «счёт не пришёл» и «счёт нулевой» в журнале неотличимы.
        let events = collect(ChatRequest::new("m", vec![])).await;
        let usage = events.iter().find_map(|e| match e {
            StreamEvent::Usage {
                input_tokens,
                output_tokens,
            } => Some((*input_tokens, *output_tokens)),
            _ => None,
        });
        assert_eq!(
            usage,
            Some((1, estimate_tokens("эхо: пустой запрос"))),
            "{events:?}"
        );
    }

    #[tokio::test]
    async fn list_models_answers_with_one_entry() {
        let v = EchoProvider::new().list_models().await.expect("список");
        assert_eq!(v["data"][0]["id"], ECHO_NAME);
    }
}
