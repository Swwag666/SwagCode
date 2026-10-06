/*! Контекст, токены и сжатие (этап B-3).

Три честных счёта токенов вместо одного врущего нуля:
1. калибровочная таблица «символов на токен» по семействам моделей,
   переопределяемая JSON-объектом из prefs (ключ `token_calibration`);
2. точный счёт от провайдера, когда он отдаёт `usage` в стриме;
3. старая грубая оценка как fallback.

Сжатие: когда оценка превышает [`COMPACT_THRESHOLD`] контекстного окна,
старые ходы сворачиваются сайд-запросом к суммаризатору, а в контексте
остаются system + свёртка + свежие [`KEEP_RECENT_TURNS`] ходов. Все функции
модуля чистые и детерминированные — свёртка проверяется headless на
записанных историях без ключа и сети.
*/

use swagcod_provider::types::{ChatMessage, Role};

/// Порог компакции: est > 0.7 * ctx (план B-3).
pub const COMPACT_THRESHOLD: f64 = 0.7;

/// Сколько свежих ходов остаются несжатыми: они нужны модели дословно.
pub const KEEP_RECENT_TURNS: usize = 3;

/// Потолок свёртки в символах: сама сводка не должна съедать контекст.
pub const SUMMARY_MAX_CHARS: usize = 6000;

/// Потолок MEMORY.md в системном промпте.
pub const MEMORY_MAX_CHARS: usize = 4000;

/// Семейство модели по подстрокам имени. Порядок проверок значим:
/// «deepseek-chat» не должен попасть в default.
pub fn model_family(model: &str) -> &'static str {
    let m = model.to_lowercase();
    if m.contains("deepseek") {
        "deepseek"
    } else if m.contains("claude") {
        "claude"
    } else if m.contains("gpt") || m.contains("o1") || m.contains("o3") || m.contains("o4") {
        "openai"
    } else if m.contains("gemini") {
        "gemini"
    } else {
        "default"
    }
}

/// Контекстное окно по семейству модели, в токенах.
pub fn model_context_tokens(model: &str) -> u32 {
    match model_family(model) {
        "deepseek" => 64_000,
        "claude" => 200_000,
        "openai" => 128_000,
        "gemini" => 1_000_000,
        _ => 128_000,
    }
}

/// Калибровка «символов на токен». `calibration_json` — необязательный
/// объект из prefs: `{"deepseek": 3.2, "default": 4.0}`. Мусор и выход
/// за разумные пределы игнорируются: таблица не должна ронять ход.
pub fn chars_per_token(model: &str, calibration_json: Option<&str>) -> f64 {
    let family = model_family(model);
    if let Some(json) = calibration_json {
        if let Ok(v) = serde_json::from_str::<serde_json::Value>(json) {
            let pick = v.get(family).or_else(|| v.get("default"));
            if let Some(n) = pick.and_then(|x| x.as_f64()) {
                if n > 0.5 && n < 20.0 {
                    return n;
                }
            }
        }
    }
    match family {
        // У CJK-тяжёлых и «плотных» токенайзеров символов на токен меньше.
        "deepseek" | "claude" => 3.5,
        _ => 4.0,
    }
}

/// Оценка токенов текста с калибровкой. CJK-символы весят вдвое: их
/// плотность на токен выше латиницы.
pub fn estimate_tokens(text: &str, cpt: f64) -> u32 {
    if text.is_empty() {
        return 0;
    }
    let cpt = if cpt > 0.5 { cpt } else { 4.0 };
    let mut weight = 0.0f64;
    for c in text.chars() {
        weight += if is_cjk(c) { 2.0 } else { 1.0 };
    }
    (weight / cpt).ceil().max(1.0) as u32
}

fn is_cjk(c: char) -> bool {
    matches!(c as u32,
        0x4E00..=0x9FFF | 0x3400..=0x4DBF | 0x3040..=0x30FF | 0xAC00..=0xD7AF)
}

/// Оценка всей истории в токенах.
pub fn estimate_history_tokens(history: &[ChatMessage], cpt: f64) -> u32 {
    history
        .iter()
        .map(|m| estimate_tokens(&m.content, cpt) + estimate_tokens(&m.reasoning, cpt))
        .sum()
}

/// Пора ли сжимать: оценка выше порога плана.
pub fn should_compact(est_tokens: u32, ctx_tokens: u32) -> bool {
    est_tokens as f64 > COMPACT_THRESHOLD * ctx_tokens as f64
}

/// Разделить историю на старую часть и `keep_turns` свежих ходов.
///
/// Граница хода — сообщение `Role::User`. Рез всегда по этой границе,
/// поэтому инвариант машины (assistant с tool_calls закрыт tool-ответами)
/// не разрывается: срезанный хвост всегда начинается с user.
pub fn split_history(history: &[ChatMessage], keep_turns: usize) -> (Vec<ChatMessage>, Vec<ChatMessage>) {
    let starts: Vec<usize> = history
        .iter()
        .enumerate()
        .filter(|(_, m)| m.role == Role::User)
        .map(|(i, _)| i)
        .collect();
    if starts.len() <= keep_turns {
        return (Vec::new(), history.to_vec());
    }
    let cut = starts[starts.len() - keep_turns];
    (history[..cut].to_vec(), history[cut..].to_vec())
}

/// Промпт сайд-запроса суммаризатора. Чистая функция от прошлой свёртки и
/// старой истории: детерминированная сборка, тестируется на записанных
/// стримах без сети.
pub fn summarizer_prompt(previous_summary: &str, old: &[ChatMessage]) -> String {
    let mut p = String::new();
    p.push_str(
        "Сожми историю диалога код-агента в короткую сводку для будущего контекста. \
         Передай: какие задачи решались, какие файлы и команды важны, какие решения \
         приняты, что осталось незакрытым. Только факты, без вступлений и без вопросов.\n\n",
    );
    if !previous_summary.trim().is_empty() {
        p.push_str("## Прошлая сводка\n");
        p.push_str(previous_summary.trim());
        p.push_str("\n\n");
    }
    p.push_str("## История для сжатия\n");
    for m in old {
        let role = match m.role {
            Role::System => "system",
            Role::User => "user",
            Role::Assistant => "assistant",
            Role::Tool => "tool",
        };
        p.push('[');
        p.push_str(role);
        p.push_str("] ");
        p.push_str(&truncate_chars(&m.content, 1200));
        p.push('\n');
    }
    p
}

/// Обрезать строку до `max` символов по границе char.
pub fn truncate_chars(s: &str, max: usize) -> String {
    if s.chars().count() <= max {
        return s.to_string();
    }
    let cut: String = s.chars().take(max).collect();
    format!("{cut}\n[... обрезано ...]")
}

/// Системный промпт хода: роль + свёртка прошлых ходов + память проекта.
///
/// Не хранится в истории сессии: драйвер подставляет его в каждый запрос
/// свежим, иначе system-сообщения множились бы от хода к ходу.
pub fn system_prompt(summary: &str, memory_md: &str) -> ChatMessage {
    let mut text = String::from(
        "Ты SwagCod — агент-программист в рабочем каталоге пользователя. \
         Отвечай на языке пользователя. Инструменты работают только внутри \
         рабочего каталога сессии.\n",
    );
    if !summary.trim().is_empty() {
        text.push_str("\n## Сводка прошлых ходов\n");
        text.push_str(&truncate_chars(summary.trim(), SUMMARY_MAX_CHARS));
        text.push('\n');
    }
    if !memory_md.trim().is_empty() {
        text.push_str("\n## Память проекта (.swagcod/MEMORY.md)\n");
        text.push_str(&truncate_chars(memory_md.trim(), MEMORY_MAX_CHARS));
        text.push('\n');
    }
    ChatMessage::system(text)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn turn(n: usize) -> Vec<ChatMessage> {
        vec![
            ChatMessage::user(format!("задача {n}")),
            ChatMessage::assistant(format!("решение {n}")),
            ChatMessage {
                role: Role::Tool,
                content: format!("вывод тулза {n}"),
                reasoning: String::new(),
                tool_calls: Vec::new(),
                tool_call_id: Some(format!("c-{n}")),
            },
        ]
    }

    fn history_of(n: usize) -> Vec<ChatMessage> {
        (1..=n).flat_map(turn).collect()
    }

    #[test]
    fn family_detection() {
        assert_eq!(model_family("deepseek-chat"), "deepseek");
        assert_eq!(model_family("claude-opus-4"), "claude");
        assert_eq!(model_family("gpt-4o"), "openai");
        assert_eq!(model_family("gemini-2.5-pro"), "gemini");
        assert_eq!(model_family("fable-ultra-promax"), "default");
    }

    #[test]
    fn calibration_from_prefs_overrides_default() {
        assert_eq!(chars_per_token("deepseek-chat", None), 3.5);
        let json = r#"{"deepseek": 3.0, "default": 5.0}"#;
        assert_eq!(chars_per_token("deepseek-chat", Some(json)), 3.0);
        assert_eq!(chars_per_token("fable-ultra-promax", Some(json)), 5.0);
        // Мусор игнорируется, ход не падает.
        assert_eq!(chars_per_token("deepseek-chat", Some("не json")), 3.5);
        assert_eq!(chars_per_token("deepseek-chat", Some(r#"{"deepseek": 0.01}"#)), 3.5);
    }

    #[test]
    fn cjk_weighs_double() {
        assert_eq!(estimate_tokens("abcd", 4.0), 1);
        assert_eq!(estimate_tokens("你好你好", 4.0), 2);
        assert_eq!(estimate_tokens("", 4.0), 0);
    }

    #[test]
    fn compact_threshold_math() {
        assert!(should_compact(45_000, 64_000)); // 0.703 > 0.7
        assert!(!should_compact(44_000, 64_000));
        assert!(!should_compact(0, 64_000));
    }

    #[test]
    fn split_keeps_last_three_turns_at_user_boundary() {
        let h = history_of(6);
        let (old, recent) = split_history(&h, KEEP_RECENT_TURNS);
        assert_eq!(old.len(), 9);
        assert_eq!(recent.len(), 9);
        assert_eq!(recent[0].role, Role::User);
        assert_eq!(recent[0].content, "задача 4");
        // Инвариант машины: assistant с tool_calls не висит без tool-ответа.
        for w in recent.windows(2) {
            if w[0].role == Role::Assistant && !w[0].tool_calls.is_empty() {
                assert_eq!(w[1].role, Role::Tool);
            }
        }
    }

    #[test]
    fn short_history_is_not_split() {
        let h = history_of(2);
        let (old, recent) = split_history(&h, KEEP_RECENT_TURNS);
        assert!(old.is_empty());
        assert_eq!(recent.len(), h.len());
    }

    #[test]
    fn summarizer_prompt_is_deterministic_and_folded() {
        let h = history_of(4);
        let (old, _) = split_history(&h, KEEP_RECENT_TURNS);
        let p1 = summarizer_prompt("", &old);
        let p2 = summarizer_prompt("", &old);
        assert_eq!(p1, p2, "сборка промпта обязана быть детерминированной");
        assert!(p1.contains("задача 1"));
        assert!(!p1.contains("задача 4"), "свежие ходы в свёртку не попадают");
        let p3 = summarizer_prompt("прошлая сводка", &old);
        assert!(p3.contains("прошлая сводка"));
    }

    #[test]
    fn system_prompt_sections_in_order() {
        let m = system_prompt("сводка тут", "# память\nважный факт");
        assert_eq!(m.role, Role::System);
        let pos_summary = m.content.find("сводка тут").unwrap();
        let pos_memory = m.content.find("важный факт").unwrap();
        assert!(pos_summary < pos_memory, "сводка идёт раньше памяти");
        // Пустые секции не оставляют заголовков.
        let empty = system_prompt("", "");
        assert!(!empty.content.contains("Сводка"));
        assert!(!empty.content.contains("Память"));
    }

    #[test]
    fn compacted_context_fits_budget() {
        // После свёртки контекст = system + сводка + свежие ходы: проверяем,
        // что он влезает в порог, из-за которого сжатие запускалось.
        let model = "deepseek-chat";
        let ctx = model_context_tokens(model);
        let cpt = chars_per_token(model, None);
        let mut h = Vec::new();
        while !should_compact(estimate_history_tokens(&h, cpt), ctx) {
            let n = h.len() / 3 + 1;
            h.extend(turn(n));
            h.iter_mut().for_each(|m| {
                m.content.push_str(&"слово ".repeat(400));
            });
            if h.len() > 3000 {
                panic!("история не достигла порога — тест сломан");
            }
        }
        let (old, recent) = split_history(&h, KEEP_RECENT_TURNS);
        assert!(!old.is_empty());
        // Сводка ограничена потолком: даже щедрый суммаризатор не вылезет.
        let summary = "с".repeat(SUMMARY_MAX_CHARS);
        let compacted_est = estimate_history_tokens(&recent, cpt)
            + estimate_tokens(&summary, cpt)
            + estimate_tokens(&system_prompt(&summary, "").content, cpt);
        assert!(
            compacted_est < ctx,
            "сжатый контекст {compacted_est} не влез в окно {ctx}"
        );
        assert!(
            compacted_est < estimate_history_tokens(&h, cpt),
            "сжатие должно уменьшать оценку"
        );
    }
}
