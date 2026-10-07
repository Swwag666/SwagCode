/*! Массовый прогон SSE-парсера случайными и мутированными байтами (B-7).

Зачем отдельный раннер, когда есть `fuzz/fuzz_targets/sse_parser.rs`:
libFuzzer на Windows-MSVC упирается в несовместимость asan-ABI — nightly
rustc линкует импорты (`__asan_new*`, `__sanitizer_cov_*_cleanup__dll`),
которых нет в штатных `clang_rt.asan_dynamic-x86_64.dll` из релизов LLVM,
а своего рантайма rustc на Windows не возит. Харнесс cargo-fuzz остаётся
в репозитории для Linux/WSL, а этот раннер гоняет те же три прохода на
stable здесь и сейчас: корпус (если есть), чистый случайный мусор и
структурные мутации валидных кадров.

Запуск:
```text
cargo run --release -p swagcod-provider --example fuzz_runner -- 2000000
```
Аргумент — число итераций (по умолчанию 2 000 000). Паника парсера роняет
процесс с ненулевым кодом — это и есть «найден баг».
*/

use swagcod_provider::{SseParser, StreamEvent};

/// LCG: детерминированный источник мусора. Сид фиксирован — найденное
/// падение воспроизводится побайтово.
struct Lcg(u64);

impl Lcg {
    fn next_u64(&mut self) -> u64 {
        self.0 = self
            .0
            .wrapping_mul(6364136223846793005)
            .wrapping_add(1442695040888963407);
        self.0
    }
    fn next_u8(&mut self) -> u8 {
        (self.next_u64() >> 33) as u8
    }
    fn below(&mut self, n: usize) -> usize {
        (self.next_u64() as usize) % n.max(1)
    }
}

/// Три прохода из cargo-fuzz харнесса — один в один, чтобы раннер и
/// харнесс искали баги одинаково.
fn exercise(data: &[u8]) {
    // Целым куском.
    let mut p = SseParser::new();
    let _ = p.feed(data);
    let _ = p.finish();

    // Дроблением: точки разреза из самих данных.
    let mut p2 = SseParser::new();
    let mut i = 0;
    while i < data.len() {
        let step = ((data[i] as usize) % 13).max(1);
        let end = (i + step).min(data.len());
        let _ = p2.feed(&data[i..end]);
        i = end;
    }
    let _ = p2.finish();

    // Валидный кадр в шуме: структура обязана распознаваться.
    let mut framed = Vec::with_capacity(data.len() + 96);
    framed.extend_from_slice(b"data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n");
    framed.extend_from_slice(data);
    framed.extend_from_slice(b"\ndata: [DONE]\n\n");
    let mut p3 = SseParser::new();
    let evs = p3.feed(&framed);
    let _ = p3.finish();
    let recognized = evs
        .iter()
        .any(|e| matches!(e, StreamEvent::Content(t) if t == "x"));
    let fake_done = data.windows(6).any(|w| w == b"[DONE]");
    assert!(
        recognized || fake_done,
        "валидный кадр потерялся в шуме: {} байт, сид воспроизводим",
        data.len()
    );
}

/// Заготовки валидных кадров: мутации бьют по реальной структуре SSE,
/// а не только по случайным байтам.
const SEED_FRAMES: &[&[u8]] = &[
    b"data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n",
    b"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"\\u043f\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n",
    b"event: message\ndata: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":null,\"total_tokens\":12}}\n\n",
    b"data: {\"choices\":[]}\n\rdata: {\"error\":{\"message\":\"boom\"}}\n\n",
    b": comment\n\ndata: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"function\":{\"name\":\"bash\",\"arguments\":\"{\\\"c\"}}]}}]}\n\n",
];

fn main() {
    let iters: usize = std::env::args()
        .nth(1)
        .and_then(|s| s.parse().ok())
        .unwrap_or(2_000_000);
    let mut rng = Lcg(0x00F2_77A5_F00D);
    let started = std::time::Instant::now();
    let mut bytes_fed: u64 = 0;

    // 0. Корпус cargo-fuzz, если он уже накоплен.
    let corpus = std::path::Path::new("fuzz/corpus/sse_parser");
    if corpus.is_dir() {
        if let Ok(entries) = std::fs::read_dir(corpus) {
            for e in entries.flatten() {
                if let Ok(data) = std::fs::read(e.path()) {
                    exercise(&data);
                    bytes_fed += data.len() as u64;
                }
            }
        }
    }

    for it in 0..iters {
        let buf = match it % 3 {
            // 1. Чистый случайный мусор произвольной длины.
            0 => {
                let len = rng.below(600);
                (0..len).map(|_| rng.next_u8()).collect::<Vec<u8>>()
            }
            // 2. Мутации валидных кадров: перевороты байт, обрезки, склейки.
            1 => {
                let seed = SEED_FRAMES[rng.below(SEED_FRAMES.len())];
                let mut v = seed.to_vec();
                let mutations = 1 + rng.below(8);
                for _ in 0..mutations {
                    if v.is_empty() {
                        v.push(rng.next_u8());
                        continue;
                    }
                    match rng.below(5) {
                        0 => {
                            let i = rng.below(v.len());
                            v[i] = rng.next_u8();
                        }
                        1 => {
                            let i = rng.below(v.len());
                            v.truncate(i);
                        }
                        2 => {
                            let i = rng.below(v.len());
                            v.insert(i, rng.next_u8());
                        }
                        3 => {
                            let other = SEED_FRAMES[rng.below(SEED_FRAMES.len())];
                            let at = rng.below(v.len() + 1);
                            v.splice(at..at, other.iter().copied());
                        }
                        _ => {
                            // UTF-8-разрыв: многобайтовый символ пополам.
                            let at = rng.below(v.len());
                            v.splice(at..at, [0xD0_u8, 0x9F_u8, 0xE2_u8, 0x82_u8]);
                        }
                    }
                }
                v
            }
            // 3. Случайные SSE-подобные строки: мусор в форме протокола.
            _ => {
                const PREFIXES: &[&[u8]] = &[b"data: ", b"event: ", b": ", b"data:", b"\n"];
                let mut v = Vec::new();
                let lines = 1 + rng.below(6);
                for _ in 0..lines {
                    let prefix = PREFIXES[rng.below(PREFIXES.len())];
                    v.extend_from_slice(prefix);
                    let len = rng.below(120);
                    for _ in 0..len {
                        v.push(rng.next_u8());
                    }
                    v.extend_from_slice(b"\n");
                }
                v.extend_from_slice(b"\n");
                v
            }
        };
        bytes_fed += buf.len() as u64;
        exercise(&buf);
    }

    let secs = started.elapsed().as_secs_f64();
    println!(
        "fuzz_runner: {iters} итераций, {bytes_fed} байт, {:.1} с ({:.1} МБ/с), падений нет",
        secs,
        bytes_fed as f64 / secs / 1e6
    );
}
