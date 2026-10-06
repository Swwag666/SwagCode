//! Fuzz SSE-парсера (B-7): главный вход недоверенных данных.
//!
//! Запуск (нужен nightly):
//! ```text
//! cargo fuzz run sse_parser -- -max_total_time=300
//! ```
//! Парсер обязан пережить любые байты и любое дробление на чанки без
//! паники, зависания и неограниченного роста потребления.

#![no_main]

use libfuzzer_sys::fuzz_target;
use swagcod_provider::SseParser;

fuzz_target!(|data: &[u8]| {
    // Проход 1: всё одним куском.
    let mut p = SseParser::new();
    let _ = p.feed(data);
    let _ = p.finish();

    // Проход 2: дробление на чанки, точки разреза определяют сами байты —
    // покрывает разрывы посреди UTF-8 символа, посреди "data:" и полей JSON.
    let mut p2 = SseParser::new();
    let mut i = 0;
    while i < data.len() {
        let step = ((data[i] as usize) % 13).max(1);
        let end = (i + step).min(data.len());
        let _ = p2.feed(&data[i..end]);
        i = end;
    }
    let _ = p2.finish();

    // Проход 3: валидная рама, обёрнутая мусором, — структура кадра
    // обязана распознаваться даже в шуме.
    let mut framed = Vec::with_capacity(data.len() + 96);
    framed.extend_from_slice(b"data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n");
    framed.extend_from_slice(data);
    framed.extend_from_slice(b"\ndata: [DONE]\n\n");
    let mut p3 = SseParser::new();
    let evs = p3.feed(&framed);
    let _ = p3.finish();
    assert!(
        evs.iter()
            .any(|e| matches!(e, swagcod_provider::StreamEvent::Content(t) if t == "x"))
            || data.windows(6).any(|w| w == b"[DONE]"),
        "валидный кадр потерялся в шуме"
    );
});
