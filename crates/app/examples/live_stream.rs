/// Быстрый тест живого стрима: отправляет запрос провайдеру и печатает события.
/// Запуск: cargo run --release -p swagcod-app --example live_stream
use swagcod_provider::{ChatMessage, ChatRequest, Provider, StreamEvent};

#[tokio::main]
async fn main() {
    let _ = dotenvy::dotenv();

    let provider = match Provider::from_env() {
        Ok(p) => p,
        Err(e) => {
            eprintln!("провайдер: {e}");
            std::process::exit(1);
        }
    };

    println!("base_url: {}", provider.base_url());

    let request = ChatRequest::new(
        std::env::var("SWAGCOD_MODEL").unwrap_or_else(|_| "fable-ultra-promax".into()),
        vec![ChatMessage::user("Скажи одно слово: привет")],
    );

    println!("запускаю стрим…");
    let start = std::time::Instant::now();

    match provider.stream(request) {
        Ok((mut rx, _handle)) => {
            let mut content = String::new();
            let mut reasoning = String::new();
            let mut events = 0u32;

            while let Some(event) = rx.recv().await {
                events += 1;
                match &event {
                    StreamEvent::Reasoning(t) => {
                        reasoning.push_str(t);
                    }
                    StreamEvent::Content(t) => {
                        content.push_str(t);
                    }
                    StreamEvent::Done { finish } => {
                        println!("\n--- DONE ({finish:?}) ---");
                    }
                    StreamEvent::Error(msg) => {
                        eprintln!("ошибка: {msg}");
                        std::process::exit(1);
                    }
                    _ => {}
                }
            }

            let elapsed = start.elapsed();
            println!("событий: {events}");
            println!("reasoning: {} симв.", reasoning.len());
            println!("content: \"{content}\"");
            println!("время: {elapsed:?}");
        }
        Err(e) => {
            eprintln!("стрим: {e}");
            std::process::exit(1);
        }
    }
}
