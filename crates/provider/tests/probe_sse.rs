//! probe_sse — зондаж стриминговых парсеров мусорными кадрами.
//!
//! Роль: бета-тестер «Кривые Руки». Кормим SseParser/Anthropic рваными
//! кадрами, битыми чанками и дичью в полях; ищем паники, тихие потери
//! валидных кадров рядом с мусором и зависания.
//!
//! Запуск: `cargo test -p swagcod-provider --test probe_sse -- --nocapture`
//! Все FINDING-кейсы из протыкания закрыты ревизией 84 и остались
//! зелёными регрессионными (комментарии `// FINDING (N)` описывают,
//! что именно ловит кейс).

use std::time::Duration;

use swagcod_provider::anthropic::{anthropic_body, Acc, AnthropicProvider};
use swagcod_provider::presets::PRESETS;
use swagcod_provider::router::{backoff_delay, parse_fallbacks};
use swagcod_provider::sse::{FinishReason, SseParser, StreamEvent};
use swagcod_provider::types::{
    openai_body, ChatMessage, ChatRequest, ImageAttachment, Role, ToolCall,
};

/// Склеить события из кусков, имитируя сеть.
fn parse_chunks(chunks: &[&str]) -> Vec<StreamEvent> {
    let mut p = SseParser::new();
    let mut out = Vec::new();
    for c in chunks {
        out.extend(p.feed(c.as_bytes()));
    }
    out.extend(p.finish());
    out
}

/// Все Content-события в порядке прихода.
fn contents(evs: &[StreamEvent]) -> Vec<String> {
    evs.iter()
        .filter_map(|e| match e {
            StreamEvent::Content(s) => Some(s.clone()),
            _ => None,
        })
        .collect()
}

/// Кадр с контентом без finish_reason (поток остаётся открытым).
fn content_frame(text: &str) -> String {
    format!("data: {{\"choices\":[{{\"delta\":{{\"content\":\"{text}\"}}}}]}}\n\n")
}

// ===========================================================================
// Группа A: кадры-мусор — «data:» без пробела/данных, регистр, комментарии.
// ===========================================================================

#[test]
fn probe_data_without_space_still_yields_content() {
    // "data:{json}" без пробела: trim обязан снять разрыв.
    let evs = parse_chunks(&[
        "data:{\"choices\":[{\"delta\":{\"content\":\"tight\"},\"finish_reason\":\"stop\"}]}\n\n",
        "data: [DONE]\n\n",
    ]);
    assert_eq!(contents(&evs), vec!["tight".to_string()], "{evs:?}");
}

#[test]
fn probe_data_spaces_only_and_bare_colon_are_inert() {
    let mut p = SseParser::new();
    let mut evs = Vec::new();
    evs.extend(p.feed(b"data:\n\n"));
    evs.extend(p.feed(b"data:   \n\n"));
    evs.extend(p.feed(b"data:\t\n\n"));
    assert!(
        evs.is_empty(),
        "пустые data не должны давать события: {evs:?}"
    );
    // «data:   [DONE]  » с пробелами вокруг — это валидный терминатор.
    let term = p.feed(b"data:   [DONE]  \n\n");
    assert!(term.is_empty());
    assert!(p.is_finished(), "DONE с пробелами должен закрывать поток");
}

#[test]
fn probe_uppercase_data_field_is_inert() {
    // Спека SSE: имена полей регистрозависимы, «DATA:» — не data-поле.
    let evs = parse_chunks(&[
        "DATA: {\"choices\":[{\"delta\":{\"content\":\"nope\"}}]}\n\n",
        "Data: {\"choices\":[{\"delta\":{\"content\":\"nope2\"}}]}\n\n",
        "data: {\"choices\":[{\"delta\":{\"content\":\"low\"},\"finish_reason\":\"stop\"}]}\n\n",
        "data: [DONE]\n\n",
    ]);
    assert_eq!(contents(&evs), vec!["low".to_string()], "{evs:?}");
}

#[test]
fn probe_comments_ids_events_and_50_blank_lines_are_inert() {
    let mut noise = String::new();
    noise.push_str(": keepalive comment\n\n");
    noise.push_str("id: 42\n");
    noise.push_str("event: message\n\n");
    for _ in 0..50 {
        noise.push('\n');
    }
    let evs = parse_chunks(&[
        noise.as_str(),
        "data: {\"choices\":[{\"delta\":{\"content\":\"survivor\"},\"finish_reason\":\"stop\"}]}\n\n",
        "data: [DONE]\n\n",
    ]);
    assert_eq!(contents(&evs), vec!["survivor".to_string()], "{evs:?}");
}

#[test]
fn probe_done_with_garbage_around_is_not_done_marker() {
    // Мусор, прилипший к [DONE] в той же строке, не терминатор.
    let mut p = SseParser::new();
    let mut evs = p.feed(b"data: x[DONE]\n\n");
    evs.extend(p.feed(b"data: [DONE]x\n\n"));
    assert!(
        !p.is_finished(),
        "мусорный [DONE] не должен закрывать поток"
    );
    evs.extend(p.feed(b"data: {\"choices\":[{\"delta\":{\"content\":\"after\"}}]}\n\n"));
    evs.extend(p.finish());
    assert_eq!(contents(&evs), vec!["after".to_string()], "{evs:?}");
    assert!(p.is_finished());
}

// ===========================================================================
// Группа B: битый JSON в data.
// ===========================================================================

#[test]
fn probe_broken_json_frames_are_skipped_not_fatal() {
    let evs = parse_chunks(&[
        "data: {\n\n",
        "data: }\n\n",
        "data: {\"choices\":[\n\n",
        // нуль-байт внутри JSON-строки: контрольный символ невалиден в JSON.
        "data: {\"choices\":[{\"delta\":{\"content\":\"a\0b\"}}]}\n\n",
        "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n",
        "data: [DONE]\n\n",
    ]);
    assert_eq!(contents(&evs), vec!["ok".to_string()], "{evs:?}");
    // Тихо, без Error-события: битые кадры проглатываются.
    assert!(
        !evs.iter().any(|e| matches!(e, StreamEvent::Error(_))),
        "{evs:?}"
    );
}

#[test]
fn probe_error_body_without_message_defaults() {
    let evs = parse_chunks(&["data: {\"error\":{\"type\":\"x\"}}\n\n"]);
    assert_eq!(
        evs,
        vec![StreamEvent::Error("provider error".to_string())],
        "{evs:?}"
    );
}

#[test]
fn probe_trailing_non_utf8_bytes_at_stream_end_are_harmless() {
    // Мусорные байты ПОСЛЕ [DONE]: поток уже закрыт, finish не паникует.
    let mut p = SseParser::new();
    let mut evs = p.feed(
        b"data: {\"choices\":[{\"delta\":{\"content\":\"done-before\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n",
    );
    evs.extend(p.feed(&[0xFF, 0xFE]));
    evs.extend(p.finish());
    assert_eq!(contents(&evs), vec!["done-before".to_string()], "{evs:?}");
}

// ===========================================================================
// Группа C: usage / choices.
// ===========================================================================

// FINDING (C1) [средний]: serde-ошибка в ЛЮБОМ поле ронает весь кадр
// вместе с валидным content. Кадр с валидным текстом и битым usage
// (отрицательное/дробное/строковое число) теряется целиком, тихо.
// Код: sse.rs:280-283 — «битый кадр пропускаем, не роняем ход», но
// «битый» здесь — любое поле, включая вспомогательный usage.
#[test]
fn probe_malformed_usage_kills_valid_content() {
    let evs = parse_chunks(&[
        "data: {\"choices\":[{\"delta\":{\"content\":\"hi1\"}}],\"usage\":{\"prompt_tokens\":-5}}\n\n",
        "data: {\"choices\":[{\"delta\":{\"content\":\"hi2\"}}],\"usage\":{\"prompt_tokens\":1.5}}\n\n",
        "data: {\"choices\":[{\"delta\":{\"content\":\"hi3\"}}],\"usage\":{\"prompt_tokens\":\"120\"}}\n\n",
        "data: [DONE]\n\n",
    ]);
    assert_eq!(
        contents(&evs),
        vec!["hi1".to_string(), "hi2".to_string(), "hi3".to_string()],
        "валидный content не должен теряться из-за битого соседнего поля: {evs:?}"
    );
}

#[test]
fn probe_usage_huge_values_saturate_without_panic() {
    let evs = parse_chunks(&[
        "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":4294967295,\"completion_tokens\":4294967295}}\n\n",
        "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":4294967296,\"completion_tokens\":999999999999}}\n\n",
    ]);
    assert!(
        evs.contains(&StreamEvent::Usage {
            input_tokens: u32::MAX,
            output_tokens: u32::MAX
        }),
        "{evs:?}"
    );
    assert_eq!(
        evs.len(),
        2,
        "оба кадра должны дать насыщенное usage: {evs:?}"
    );
}

#[test]
fn probe_empty_choices_and_missing_delta_are_safe() {
    let evs = parse_chunks(&[
        "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5}}\n\n",
        "data: {\"choices\":[{\"finish_reason\":\"stop\"}]}\n\n",
        "data: [DONE]\n\n",
    ]);
    assert!(
        evs.contains(&StreamEvent::Usage {
            input_tokens: 10,
            output_tokens: 5
        }),
        "{evs:?}"
    );
    assert!(
        matches!(
            evs.iter().find(|e| matches!(e, StreamEvent::Done { .. })),
            Some(StreamEvent::Done {
                finish: FinishReason::Stop
            })
        ),
        "{evs:?}"
    );
}

// ===========================================================================
// Группа D: tool_calls.
// ===========================================================================

#[test]
fn probe_tool_calls_without_index_defaults_to_zero() {
    let evs = parse_chunks(&[
        "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"id\":\"c0\",\"function\":{\"name\":\"f0\",\"arguments\":\"{}\"}}]}}]}\n\n",
        "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n",
        "data: [DONE]\n\n",
    ]);
    assert!(
        evs.iter().any(|e| matches!(
            e,
            StreamEvent::ToolCallStart { index: 0, id, name } if id == "c0" && name == "f0"
        )),
        "{evs:?}"
    );
    assert!(
        evs.iter()
            .any(|e| matches!(e, StreamEvent::ToolCallComplete(_))),
        "{evs:?}"
    );
}

// FINDING (D1) [средний]: tool_calls с index неверного типа (строка/дробь/
// отрицательное) роняет весь кадр вместе с валидным content того же кадра.
// Причина та же, что в FINDING C1: serde-десериализация Chunk идёт целиком.
#[test]
fn probe_tool_index_wrong_type_kills_valid_content() {
    let evs = parse_chunks(&[
        "data: {\"choices\":[{\"delta\":{\"content\":\"t1\",\"tool_calls\":[{\"index\":\"0\",\"id\":\"c\",\"function\":{\"name\":\"f\"}}]}}]}\n\n",
        "data: {\"choices\":[{\"delta\":{\"content\":\"t2\",\"tool_calls\":[{\"index\":1.5,\"id\":\"c\",\"function\":{\"name\":\"f\"}}]}}]}\n\n",
        "data: {\"choices\":[{\"delta\":{\"content\":\"t3\",\"tool_calls\":[{\"index\":-1,\"id\":\"c\",\"function\":{\"name\":\"f\"}}]}}]}\n\n",
        "data: [DONE]\n\n",
    ]);
    assert_eq!(
        contents(&evs),
        vec!["t1".to_string(), "t2".to_string(), "t3".to_string()],
        "валидный content не должен теряться из-за битого index: {evs:?}"
    );
}

#[test]
fn probe_tool_args_chunks_arrive_in_order() {
    // Куски одного вызова: аргументы обязаны склеиться в порядке прихода.
    let evs = parse_chunks(&[
        "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"function\":{\"name\":\"f\",\"arguments\":\"{\\\"k\\\":\\\"\"}}]}}]}\n\n",
        "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"AB\"}}]}}]}\n\n",
        "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"CD\"}}]}}]}\n\n",
        "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"}\"}}]}}]}\n\n",
        "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n",
        "data: [DONE]\n\n",
    ]);
    let complete: Vec<_> = evs
        .iter()
        .filter_map(|e| match e {
            StreamEvent::ToolCallComplete(t) => Some(t),
            _ => None,
        })
        .collect();
    assert_eq!(complete.len(), 1, "{evs:?}");
    assert_eq!(
        complete[0].arguments["k"],
        serde_json::json!("ABCD"),
        "порядок кусков arguments: {evs:?}"
    );
}

// FINDING (D2) [средний]: два РАЗНЫХ вызова с одинаковым index сливаются
// в один. Оба дают ToolCallStart, но Complete один: id/name перезаписаны
// последним пришедшим, arguments сконкатенированы в невалидный JSON
// («{"x":1}{"y":2}») и деградируют в пустой объект. Первый вызов теряется
// тихо: Start был, Complete нет. Код: sse.rs:335-341 — pending_tools
// ищется по index без учёта смены id.
#[test]
fn probe_duplicate_tool_index_merges_two_calls() {
    let evs = parse_chunks(&[
        "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"cA\",\"function\":{\"name\":\"fA\",\"arguments\":\"{\\\"x\\\":1}\"}}]}}]}\n\n",
        "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"cB\",\"function\":{\"name\":\"fB\",\"arguments\":\"{\\\"y\\\":2}\"}}]}}]}\n\n",
        "data: [DONE]\n\n",
    ]);
    let completes: Vec<_> = evs
        .iter()
        .filter_map(|e| match e {
            StreamEvent::ToolCallComplete(t) => Some(t.clone()),
            _ => None,
        })
        .collect();
    assert_eq!(
        completes.len(),
        2,
        "два разных вызова (id cA и cB) должны дать два Complete: {evs:?}"
    );
}

// FINDING (D3) [низкий]: шлюз, повторяющий id в каждом чанке вызова
// (поведение ряда OpenAI-совместимых прокси), порождает дубликат
// ToolCallStart на каждый чанк. Аргументы при этом копятся корректно.
// Код: sse.rs:342-357 — каждый непустой id эмитит Start.
#[test]
fn probe_repeated_tool_id_duplicate_starts() {
    let evs = parse_chunks(&[
        "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"function\":{\"name\":\"f\",\"arguments\":\"{\"}}]}}]}\n\n",
        "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"function\":{\"arguments\":\"\\\"a\\\":1}\"}}]}}]}\n\n",
        "data: [DONE]\n\n",
    ]);
    let starts = evs
        .iter()
        .filter(|e| matches!(e, StreamEvent::ToolCallStart { .. }))
        .count();
    assert_eq!(starts, 1, "один вызов = один Start: {evs:?}");
}

#[test]
fn probe_invalid_tool_arguments_silently_become_empty_map() {
    // Документация поведения (тихая деградация по дизайну): невалидный
    // JSON аргументов → пустой объект, БЕЗ Error-события. Ошибка видна
    // только по отсутствию ожидаемых полей.
    let evs = parse_chunks(&[
        "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"a\",\"function\":{\"name\":\"f\",\"arguments\":\"{broken\"}}]}}]}\n\n",
        "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n",
        "data: [DONE]\n\n",
    ]);
    let t = evs
        .iter()
        .find_map(|e| match e {
            StreamEvent::ToolCallComplete(t) => Some(t),
            _ => None,
        })
        .expect("Complete обязан быть");
    assert!(t.arguments.is_empty(), "{evs:?}");
    assert!(
        !evs.iter().any(|e| matches!(e, StreamEvent::Error(_))),
        "{evs:?}"
    );
}

// FINDING (A1) [средний]: пустой finish_reason ("") закрывает поток на
// первом же чанке. Кадр 2 с валидным контентом отбрасывается молча.
// Шлюзы, шлющие "" как placeholder вместо null, обрежут ход до нуля.
// Код: sse.rs:297-304 — Some("") считается finish_reason.
#[test]
fn probe_empty_finish_reason_closes_stream_early() {
    let evs = parse_chunks(&[
        "data: {\"choices\":[{\"delta\":{\"content\":\"a\"},\"finish_reason\":\"\"}]}\n\n",
        "data: {\"choices\":[{\"delta\":{\"content\":\"b\"}}]}\n\n",
        "data: [DONE]\n\n",
    ]);
    assert_eq!(
        contents(&evs),
        vec!["a".to_string(), "b".to_string()],
        "пустая строка — не finish_reason, ход не должен закрываться: {evs:?}"
    );
}

// ===========================================================================
// Группа E: UTF-8 разрывы и разрывы кадров.
// ===========================================================================

#[test]
fn probe_multibyte_japan_split_across_chunks() {
    // 日 = 3 байта (E6 97 A5). Режем на 1+1+1 байт.
    let frame = "data: {\"choices\":[{\"delta\":{\"content\":\"日本\"}}]}\n\ndata: [DONE]\n\n";
    let bytes = frame.as_bytes();
    let pos = bytes
        .windows(3)
        .position(|w| w == [0xE6, 0x97, 0xA5])
        .expect("日 в кадре");

    let mut p = SseParser::new();
    let mut evs = p.feed(&bytes[..pos + 1]);
    evs.extend(p.feed(&bytes[pos + 1..pos + 2]));
    evs.extend(p.feed(&bytes[pos + 2..]));
    evs.extend(p.finish());
    assert_eq!(contents(&evs), vec!["日本".to_string()], "{evs:?}");
}

#[test]
fn probe_frame_split_inside_data_prefix() {
    // Разрез ровно между «dat» и «a:».
    let mut p = SseParser::new();
    let mut evs = p.feed(b"dat");
    evs.extend(p.feed(
        b"a: {\"choices\":[{\"delta\":{\"content\":\"x\"},\"finish_reason\":\"stop\"}]}\n\n",
    ));
    evs.extend(p.feed(b"data: [DONE]\n\n"));
    assert_eq!(contents(&evs), vec!["x".to_string()], "{evs:?}");
}

#[test]
fn probe_frame_split_between_newline_pair() {
    // Разрез ровно между двумя \n разделителя кадров.
    let mut p = SseParser::new();
    let mut evs = p.feed(
        b"data: {\"choices\":[{\"delta\":{\"content\":\"y\"},\"finish_reason\":\"stop\"}]}\n",
    );
    evs.extend(p.feed(b"\ndata: [DONE]\n\n"));
    evs.extend(p.finish());
    assert_eq!(contents(&evs), vec!["y".to_string()], "{evs:?}");
}

#[test]
fn probe_reasoning_alias_priority() {
    // Приоритет полей: reasoning_content → reasoning → thinking.
    let evs = parse_chunks(&[
        "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"rc\",\"reasoning\":\"r\",\"thinking\":\"t\"}}]}\n\n",
        "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"\",\"reasoning\":\"r2\",\"thinking\":\"t2\"}}]}\n\n",
        "data: {\"choices\":[{\"delta\":{\"thinking\":\"t3\"}}]}\n\n",
        "data: [DONE]\n\n",
    ]);
    let rs: Vec<_> = evs
        .iter()
        .filter_map(|e| match e {
            StreamEvent::Reasoning(s) => Some(s.clone()),
            _ => None,
        })
        .collect();
    assert_eq!(
        rs,
        vec!["rc".to_string(), "r2".to_string(), "t3".to_string()],
        "{evs:?}"
    );
}

// ===========================================================================
// Группа F: потоковое отравление.
// ===========================================================================

// FINDING (F1) [высокий]: ОДИН не-UTF8 байт (0xFF/0xFE) посреди потока
// останавливает парсер НАВСЕГДА. Механизм: feed() не различает «символ
// оборван, ждём продолжения» и «заведомо битый байт» — оба копятся в
// pending_bytes (sse.rs:209-220). Битый байт в голове даёт
// valid_up_to()==0 на каждом следующем чанке: ВЕСЬ валидный контент
// каждого чанка уезжает обратно в pending_bytes, в buf не попадает
// ничего. feed возвращает пустые векторы до конца потока, finish()
// молчит. Один мусорный байт от прокси = тихая потеря всего ответа.
#[test]
fn probe_non_utf8_byte_stalls_stream_forever() {
    // Сценарий 1: мусорный байт между двумя валидными кадрами.
    let mut p = SseParser::new();
    let mut evs = p.feed(content_frame("a").as_bytes());
    evs.extend(p.feed(&[0xFF]));
    evs.extend(p.feed(content_frame("b").as_bytes()));
    evs.extend(p.finish());
    assert!(
        evs.iter()
            .any(|e| matches!(e, StreamEvent::Content(c) if c == "b")),
        "кадр ПОСЛЕ мусорного байта обязан разбираться (максимум потерь — один кадр): {evs:?}"
    );
}

// FINDING (F1b) [высокий]: тот же механизм, худший случай — мусорный байт
// первым: парсер не выдаёт НИ ОДНОГО события за весь поток.
#[test]
fn probe_non_utf8_first_byte_kills_everything() {
    // Сценарий 2: мусорный байт первым — поток мёртв с самого начала.
    let mut p = SseParser::new();
    let mut evs = p.feed(&[0xFF, 0xFE]);
    evs.extend(p.feed(content_frame("z").as_bytes()));
    evs.extend(p.feed(b"data: [DONE]\n\n"));
    evs.extend(p.finish());
    assert!(
        evs.iter()
            .any(|e| matches!(e, StreamEvent::Content(c) if c == "z")),
        "первый мусорный байт не должен убивать весь поток: {evs:?}"
    );
}

// FINDING (F2) [средний]: UTF-8 BOM (EF BB BF) в начале event-stream
// прилипает к первой строке, strip_prefix("data:") не срабатывает —
// ПЕРВЫЙ валидный кадр теряется тихо. BOM перед text/event-stream
// встречается от ряда прокси/серверов.
// Код: sse.rs:267 — префикс сравнивается без снятия BOM.
#[test]
fn probe_utf8_bom_swallows_first_frame() {
    let stream = "\u{FEFF}data: {\"choices\":[{\"delta\":{\"content\":\"первый\"}}]}\n\n\
                  data: {\"choices\":[{\"delta\":{\"content\":\"второй\"}}]}\n\n\
                  data: [DONE]\n\n";
    let evs = parse_chunks(&[stream]);
    assert_eq!(
        contents(&evs),
        vec!["первый".to_string(), "второй".to_string()],
        "первый кадр не должен теряться из-за BOM: {evs:?}"
    );
}

// FINDING (F3) [средний]: строки с чисто-CR («\r») терминатором —
// валидный SSE по спеке (CR, LF или CRLF) — теряются ЦЕЛИКОМ. Парсер
// ищет только '\n' (sse.rs:225), весь поток копится в buf; finish()
// обрабатывает его одной строкой, JSON с внутристрировочным \r не
// парсится — ноль событий.
#[test]
fn probe_cr_only_line_endings_lose_all_frames() {
    let stream = "data: {\"choices\":[{\"delta\":{\"content\":\"cr1\"}}]}\r\r\
                  data: {\"choices\":[{\"delta\":{\"content\":\"cr2\"},\"finish_reason\":\"stop\"}]}\r\r\
                  data: [DONE]\r\r";
    let evs = parse_chunks(&[stream]);
    assert!(
        evs.iter()
            .any(|e| matches!(e, StreamEvent::Content(c) if c == "cr1")),
        "SSE-спека допускает CR-терминатор строк: {evs:?}"
    );
}

// ===========================================================================
// Группа G: types.rs.
// ===========================================================================

// FINDING (G1) [низкий, latent]: сериализация ChatMessage асимметрична
// десериализации. Serialize пишет wire-формат OpenAI (tool_calls →
// {id, type, function:{name, arguments-СТРОКА}}), Deserialize ждёт
// внутренний плоский формат ({id, name, arguments-ОБЪЕКТ}, полей без
// дефолтов). to_string → from_str ломается на первом же сообщении с
// tool_calls («missing field `name`»). Любой слой персистентности,
// пишущий историю через serde, не прочитает её обратно. Сегодня store.rs
// собирает историю вручную, так что дефект латентный.
#[test]
fn probe_chatmessage_tool_calls_roundtrip_asymmetry() {
    let msg = ChatMessage {
        role: Role::Assistant,
        content: String::new(),
        reasoning: String::new(),
        tool_calls: vec![ToolCall {
            id: "c1".to_string(),
            name: "read".to_string(),
            arguments: serde_json::json!({"path": "a.txt"})
                .as_object()
                .cloned()
                .unwrap(),
        }],
        tool_call_id: None,
    };
    let s = serde_json::to_string(&msg).unwrap();
    let back: Result<ChatMessage, _> = serde_json::from_str(&s);
    assert!(
        back.is_ok(),
        "round-trip to_string→from_str: {back:?} (сериализовано: {s})"
    );
}

// FINDING (G2) [низкий]: картинки тихо выбрасываются, если в запросе нет
// ни одного user-сообщения. openai_body: rposition(user) → None → return
// без images (types.rs:201-209); anthropic_body: last_user → None →
// блоки image не строятся (anthropic.rs:236). Запрос уходит без картинок,
// модель отвечает вслепую, ошибка не surfaced.
#[test]
fn probe_openai_body_drops_images_without_user() {
    let img = ImageAttachment {
        media_type: "image/png".to_string(),
        data_base64: "aGk=".to_string(),
    };
    let req =
        ChatRequest::new("m", vec![ChatMessage::system("sys")]).with_images(vec![img.clone()]);
    let body = openai_body(&req);
    assert!(
        body.to_string().contains("image_url"),
        "картинки не должны выбрасываться молча: {body}"
    );
    let abody = anthropic_body(&req);
    assert!(
        abody.to_string().contains("\"type\":\"image\""),
        "anthropic: картинки не должны выбрасываться молча: {abody}"
    );
}

#[test]
fn probe_openai_body_images_on_empty_user_content() {
    // Картинки + пустой текст: допустимое мультимодальное тело.
    let img = ImageAttachment {
        media_type: "image/png".to_string(),
        data_base64: "aGk=".to_string(),
    };
    let with_text =
        ChatRequest::new("m", vec![ChatMessage::user("hi")]).with_images(vec![img.clone()]);
    let body = openai_body(&with_text);
    let s = body.to_string();
    assert!(s.contains("image_url"), "{s}");
    assert!(s.contains("\"type\":\"text\""), "{s}");

    let empty_text = ChatRequest::new("m", vec![ChatMessage::user("")]).with_images(vec![img]);
    let body2 = openai_body(&empty_text);
    let s2 = body2.to_string();
    assert!(s2.contains("image_url"), "{s2}");
    assert!(
        !s2.contains("\"type\":\"text\""),
        "пустого text-блока быть не должно: {s2}"
    );
}

// ===========================================================================
// Группа H: presets / router (статика и чистые функции).
// ===========================================================================

#[test]
fn probe_presets_all_twelve_have_host_and_fields() {
    assert_eq!(PRESETS.len(), 12, "в каталоге 12 пресетов");
    for p in PRESETS {
        assert!(!p.id.is_empty(), "пустой id: {p:?}");
        assert!(!p.label.is_empty(), "пустой label: {p:?}");
        assert!(!p.base_url.is_empty(), "пустой base_url: {p:?}");
        assert!(!p.key_url.is_empty(), "пустой key_url: {p:?}");
        let (scheme, rest) = p
            .base_url
            .split_once("://")
            .unwrap_or_else(|| panic!("{}: base_url без схемы: {}", p.id, p.base_url));
        assert!(
            scheme == "http" || scheme == "https",
            "{}: схема {scheme}",
            p.id
        );
        let host = rest.split('/').next().unwrap_or("");
        assert!(!host.is_empty(), "{}: пустой host в {}", p.id, p.base_url);
    }
}

#[test]
fn probe_fallbacks_extra_fields_and_backoff_extremes() {
    // Лишние поля после model молча отбрасываются.
    let parsed = parse_fallbacks("https://a|k|m|extra|junk");
    assert_eq!(
        parsed,
        vec![(
            "https://a".to_string(),
            "k".to_string(),
            Some("m".to_string())
        )]
    );
    assert!(parse_fallbacks("|||").is_empty());
    assert!(parse_fallbacks(" ; ").is_empty());
    // Крайние attempt не дают переполнения/паники.
    let d = backoff_delay(u32::MAX, 12345);
    assert!((5600..=10400).contains(&d.as_millis()), "{d:?}");
    let d0 = backoff_delay(0, 0);
    assert!((350..=650).contains(&d0.as_millis()), "{d0:?}");
}

// ===========================================================================
// Группа I: anthropic.rs — Acc и anthropic_body.
// ===========================================================================

#[test]
fn probe_anthropic_acc_garbage_is_inert() {
    let mut acc = Acc::default();
    assert!(acc.feed_frame("event: ping\ndata: {}").is_empty());
    assert!(acc
        .feed_frame("event: message_delta\ndata: не json")
        .is_empty());
    assert!(
        acc.feed_frame("data: {\"x\":1}").is_empty(),
        "кадр без event:"
    );
    assert!(
        acc.feed_frame("EVENT: message_start\ndata: {}").is_empty(),
        "регистр event:"
    );
    assert!(acc
        .feed_frame("event: unknown_thing\ndata: {\"x\":1}")
        .is_empty());
    assert!(acc.feed_frame("мусор без формата").is_empty());
    assert!(acc.feed_frame("").is_empty());

    // После мусора живой минискрипт работает.
    let evs = acc.feed_frame("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}");
    assert!(evs.is_empty());
    let evs = acc.feed_frame("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}");
    assert_eq!(evs, vec![StreamEvent::Content("ok".to_string())]);
}

#[test]
fn probe_anthropic_acc_delta_before_start_is_ignored() {
    let mut acc = Acc::default();
    assert!(acc
        .feed_frame("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":7,\"delta\":{\"type\":\"text_delta\",\"text\":\"lost\"}}")
        .is_empty());
    assert!(acc
        .feed_frame("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":7,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{}\"}}")
        .is_empty());
    assert!(acc
        .feed_frame(
            "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":7}"
        )
        .is_empty());
}

#[test]
fn probe_anthropic_acc_million_index_no_panic() {
    // index=1_000_000 не паникует (выделяет ~80 МБ слотов — само по себе
    // находка, см. FINDING I2), малые индексы после него живут.
    let mut acc = Acc::default();
    let _ = acc.feed_frame(
        "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1000000,\"content_block\":{\"type\":\"text\"}}",
    );
    let _ = acc.feed_frame(
        "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"text\"}}",
    );
    let evs = acc.feed_frame(
        "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"alive\"}}",
    );
    assert_eq!(evs, vec![StreamEvent::Content("alive".to_string())]);
}

// FINDING (I2) [средний]: content_block_start с index из кадра не
// ограничен: `while self.slots.len() <= index { push }` без потолка
// (anthropic.rs:469-470), index читается из JSON как u64. Кадр
// {"index": 1000000000} заставляет выделить ~80 ГБ (10⁹ × ~80 Б на
// Slot) — процесс абортится на аллокации. Кривой/враждебный endpoint
// кладёт приложение одной строкой. Прогон этого теста не выполнялся,
// чтобы не убить раннер: тело ниже реально абортит процесс.
#[test]
fn probe_anthropic_giant_index_unbounded_alloc() {
    let mut acc = Acc::default();
    let _ = acc.feed_frame(
        "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1000000000,\"content_block\":{\"type\":\"text\"}}",
    );
}

// FINDING (I3) [низкий]: message_delta без usage эмитит Usage{0,0} —
// «точный счёт: ноль токенов». Семантика sse.rs (usage_events) нули
// событием не делает; ядро получит ложный ноль вместо отсутствия данных
// и не станет считать оценкой. Код: anthropic.rs:553-575.
#[test]
fn probe_anthropic_usage_zero_when_absent() {
    let mut acc = Acc::default();
    let _ = acc.feed_frame(
        "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}",
    );
    let evs = acc.feed_frame(
        "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}",
    );
    assert!(
        !evs.iter().any(|e| matches!(e, StreamEvent::Usage { .. })),
        "usage нет в кадре — события Usage быть не должно: {evs:?}"
    );
}

// FINDING (I4) [низкий]: повторный content_block_start для того же
// index ПЕРЕЗАПИСЫВАЕТ слот (buf сбрасывается, anthropic.rs:469-482) —
// накопленные куски input_json_delta тихо теряются, собранные аргументы
// вызова деградируют в пустой объект.
#[test]
fn probe_anthropic_duplicate_block_start_resets_args() {
    let mut acc = Acc::default();
    acc.feed_frame("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"read\"}}");
    acc.feed_frame("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"a\\\":\"}}");
    // Дубликат start (прокси повторил кадр).
    acc.feed_frame("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"read\"}}");
    acc.feed_frame("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"1}\"}}");
    let evs = acc.feed_frame(
        "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}",
    );
    let tc = evs
        .iter()
        .find_map(|e| match e {
            StreamEvent::ToolCallComplete(t) => Some(t),
            _ => None,
        })
        .expect("Complete обязан быть: {evs:?}");
    assert_eq!(
        tc.arguments.get("a"),
        Some(&serde_json::json!(1)),
        "аргументы не должны теряться на дубликате start: {evs:?}"
    );
}

#[test]
fn probe_anthropic_body_garbage_inputs_no_panic() {
    // Пустые сообщения: массив пуст, без паники.
    let empty = ChatRequest::new("m", vec![]);
    let b = anthropic_body(&empty);
    assert_eq!(b["messages"], serde_json::json!([]), "{b}");

    // user с пустым content → сообщение пропущено.
    let b2 = anthropic_body(&ChatRequest::new("m", vec![ChatMessage::user("")]));
    assert_eq!(b2["messages"], serde_json::json!([]), "{b2}");

    // Пустой system → ключа system нет.
    let b3 = anthropic_body(&ChatRequest::new(
        "m",
        vec![ChatMessage::system(""), ChatMessage::user("q")],
    ));
    assert!(b3.get("system").is_none(), "{b3}");

    // tool-результат без id пропускается; assistant без контента тоже.
    let tool_no_id = ChatMessage {
        role: Role::Tool,
        content: "ok".to_string(),
        reasoning: String::new(),
        tool_calls: vec![],
        tool_call_id: None,
    };
    let b4 = anthropic_body(&ChatRequest::new(
        "m",
        vec![
            tool_no_id,
            ChatMessage::assistant(""),
            ChatMessage::user("u"),
        ],
    ));
    let msgs = b4["messages"].as_array().unwrap();
    assert_eq!(msgs.len(), 1, "только user: {b4}");

    // user с пустым текстом + картинка → только image-блок.
    let img = ImageAttachment {
        media_type: "image/png".to_string(),
        data_base64: "aGk=".to_string(),
    };
    let b5 =
        anthropic_body(&ChatRequest::new("m", vec![ChatMessage::user("")]).with_images(vec![img]));
    let blocks = b5["messages"][0]["content"].as_array().unwrap();
    assert_eq!(blocks.len(), 1, "один image-блок: {b5}");
    assert_eq!(blocks[0]["type"], serde_json::json!("image"));
}

// FINDING (I5) [высокий]: Anthropic-клиент портит многобайтовые символы
// на границах сетевых чанков. В отличие от sse.rs (pending_bytes),
// anthropic.rs:189 делает String::from_utf8_lossy(&bytes) НА КАЖДОМ
// чанке: байты недописанного символа заменяются на U+FFFD, а хвост
// следующего чанка становится отдельным мусором. Живой прогон ниже:
// TCP-сервер рвёт тело ровно посередине «日» (E6 97 A5).
#[tokio::test(flavor = "multi_thread")]
async fn probe_anthropic_multibyte_split_corrupts() {
    use tokio::io::{AsyncReadExt, AsyncWriteExt};
    use tokio::net::TcpListener;

    let body = concat!(
        "event: message_start\n",
        "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n",
        "event: content_block_start\n",
        "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n",
        "event: content_block_delta\n",
        "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"日\"}}\n\n",
        "event: content_block_stop\n",
        "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
        "event: message_delta\n",
        "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n",
        "event: message_stop\n",
        "data: {\"type\":\"message_stop\"}\n\n",
    );
    let bytes = body.as_bytes();
    let split = bytes
        .windows(3)
        .position(|w| w == [0xE6, 0x97, 0xA5])
        .expect("日 в теле")
        + 1;
    let total = bytes.len();

    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    tokio::spawn(async move {
        let (mut sock, _) = listener.accept().await.unwrap();
        let mut buf = [0u8; 4096];
        let _ = tokio::time::timeout(Duration::from_millis(500), sock.read(&mut buf)).await;
        let head = format!(
            "HTTP/1.1 200 OK\r\ncontent-type: text/event-stream\r\ncontent-length: {total}\r\n\r\n"
        );
        sock.write_all(head.as_bytes()).await.unwrap();
        sock.write_all(&bytes[..split]).await.unwrap();
        sock.flush().await.unwrap();
        // Пауза гарантирует, что reqwest отдаст две части отдельными чанками.
        tokio::time::sleep(Duration::from_millis(200)).await;
        sock.write_all(&bytes[split..]).await.unwrap();
        sock.flush().await.unwrap();
        let _ = sock.shutdown().await;
    });

    let provider = AnthropicProvider::new_open(format!("http://{addr}/v1")).unwrap();
    let req = ChatRequest::new("claude-x", vec![ChatMessage::user("hi")]);
    let (mut rx, _handle) = provider.stream(req).unwrap();
    let mut text = String::new();
    while let Some(ev) = tokio::time::timeout(Duration::from_secs(10), rx.recv())
        .await
        .expect("клиент не завис")
    {
        if let StreamEvent::Content(t) = ev {
            text.push_str(&t);
        }
    }
    assert_eq!(
        text, "日",
        "многобайтовый символ не должен портиться: получено {text:?}"
    );
}
