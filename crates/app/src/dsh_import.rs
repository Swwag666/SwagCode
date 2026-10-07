/*! E-9: автоматический импорт сессий из DSH Desktop.

DSH хранит историю в `~/.dsh/sessions/<encoded-cwd>/<session-dir>/session.v3.jsonl.zstd`:
это JSONL-лог событий v3, упакованный **многокадровым** zstd (приложение
дописывает кадры по мере хода — один кадр на порцию записей). Формат кадра
проверен живьём на реальной базе (ревизия 29): 17 типов записей, из них
импортеру нужны `session`, `session/title`, `subagent/descriptor`,
`request/context`, `turn/start`, `turn/end`, `user/message`,
`assistant/message`, `tool/call`.

Отображение в нашу доменную модель:
- DSH-сессия → `Session` (id — оригинальный, идемпотентность через
  `INSERT OR IGNORE` в create_session; title — из session/title, иначе
  label суб-агента, иначе начало первой пользовательской реплики);
- turn/start..turn/end → `TurnRecord` (content — текст assistant/message,
  reasoning — его же reasoning-части, tool_calls — записи tool/call,
  ok — reason.kind == completed);
- user/assistant-реплики → накопительная wire-история: save_turn получает
  снимок истории, как в живом ходе.

Импорт read-only по отношению к DSH и идемпотентный: повторный прогон
не плодит дубли (те же id сессий и ходов).
*/

use std::io::Read;
use std::path::{Path, PathBuf};

use swagcod_core::session::{Session, SessionId, SessionStatus, TurnId, TurnRecord};
use swagcod_core::store::Store;
use swagcod_provider::types::{ChatMessage, ToolCall};

const ZSTD_MAGIC: [u8; 4] = [0x28, 0xB5, 0x2F, 0xFD];

/// Итог импорта: что перенесено, что пропущено, что сломалось.
#[derive(Debug, Default)]
pub struct ImportReport {
    pub sessions: usize,
    pub turns: usize,
    pub messages: usize,
    pub skipped: usize,
    pub errors: Vec<String>,
}

/// Корень сессий DSH: `~/.dsh/sessions`. None, если DSH на машине нет —
/// это нормальный случай, а не ошибка.
pub fn dsh_sessions_root() -> Option<PathBuf> {
    let home = std::env::var("USERPROFILE")
        .or_else(|_| std::env::var("HOME"))
        .ok()?;
    let p = PathBuf::from(home).join(".dsh").join("sessions");
    if p.is_dir() {
        Some(p)
    } else {
        None
    }
}

/// Декомпрессия многокадрового zstd. Кадры ищем по magic: DSH дописывает
/// их в конец файла, а one-shot декодер останавливается на первом.
/// Ложный magic внутри сжатых данных даёт ошибку кадра — такой кадр
/// пропускаем, остальные живут (проверено на живой базе: 49/49 кадров).
pub fn decompress_session(bytes: &[u8]) -> std::io::Result<String> {
    let mut offs = Vec::new();
    let mut i = 0usize;
    while i + 4 <= bytes.len() {
        if bytes[i..i + 4] == ZSTD_MAGIC {
            offs.push(i);
            i += 4;
        } else {
            i += 1;
        }
    }
    if offs.is_empty() {
        return Err(std::io::Error::new(
            std::io::ErrorKind::InvalidData,
            "zstd-кадры не найдены",
        ));
    }
    let mut out: Vec<u8> = Vec::new();
    let mut decoded_any = false;
    for (k, off) in offs.iter().enumerate() {
        let end = offs.get(k + 1).copied().unwrap_or(bytes.len());
        let mut decoder = match zstd::stream::Decoder::new(&bytes[*off..end]) {
            Ok(d) => d,
            Err(_) => continue, // ложный magic — кадр-приманка
        };
        if decoder.read_to_end(&mut out).is_ok() {
            decoded_any = true;
        }
    }
    if !decoded_any {
        return Err(std::io::Error::new(
            std::io::ErrorKind::InvalidData,
            "ни один zstd-кадр не декодировался",
        ));
    }
    Ok(String::from_utf8_lossy(&out).into_owned())
}

/// Разобранная сессия: доменные поля + ходы с их репликами.
#[derive(Debug)]
struct ParsedTurn {
    rec: TurnRecord,
    /// Реплики хода в wire-порядке: пользовательские и ответ модели.
    msgs: Vec<ChatMessage>,
}

#[derive(Debug, Default)]
struct ParsedSession {
    id: String,
    title: String,
    cwd: String,
    model: String,
    created_ms: u64,
    label: String,
    turns: Vec<ParsedTurn>,
}

fn text_of(content: &serde_json::Value, part: &str) -> String {
    content
        .as_array()
        .map(|arr| {
            arr.iter()
                .filter(|p| p.get("type").and_then(|t| t.as_str()) == Some(part))
                .filter_map(|p| p.get("text").and_then(|t| t.as_str()))
                .collect::<Vec<_>>()
                .join("\n")
        })
        .unwrap_or_default()
}

/// Запись JSONL-лога разбирается как общий Value: у записи `session`
/// доменные поля (id/cwd/createdAt) лежат НА ВЕРХНЕМ уровне, а у
/// остальных событий — под `data`. Жёсткая структура ломалась бы и там.
fn parse_session_log(text: &str) -> Result<ParsedSession, String> {
    let mut s = ParsedSession::default();
    let mut first_user = String::new();
    let mut current: Option<ParsedTurn> = None;

    for line in text.lines() {
        let line = line.trim();
        if line.is_empty() {
            continue;
        }
        let rec: serde_json::Value = match serde_json::from_str(line) {
            Ok(r) => r,
            Err(_) => continue, // битая строка не роняет весь импорт
        };
        let kind = rec.get("type").and_then(|v| v.as_str()).unwrap_or("");
        let time = rec.get("time").and_then(|v| v.as_u64()).unwrap_or(0);
        let data = rec.get("data").cloned().unwrap_or(serde_json::Value::Null);
        match kind {
            "session" => {
                s.id = rec.get("id").and_then(|v| v.as_str()).unwrap_or_default().to_string();
                s.cwd = rec.get("cwd").and_then(|v| v.as_str()).unwrap_or_default().to_string();
                s.created_ms = rec.get("createdAt").and_then(|v| v.as_u64()).unwrap_or(0);
            }
            "session/title" => {
                if let Some(t) = data.get("title").and_then(|v| v.as_str()) {
                    s.title = t.to_string();
                }
            }
            "subagent/descriptor" => {
                if let Some(l) = data.get("label").and_then(|v| v.as_str()) {
                    s.label = l.to_string();
                }
            }
            "request/context" => {
                if s.model.is_empty() {
                    if let Some(m) = data.get("model").and_then(|v| v.as_str()) {
                        s.model = m.to_string();
                    }
                }
            }
            "turn/start" => {
                let n = data.get("turn").and_then(|v| v.as_u64()).unwrap_or(1);
                current = Some(ParsedTurn {
                    rec: TurnRecord {
                        // turns.id — ГЛОБАЛЬНЫЙ первичный ключ: без id сессии
                        // «dsh-t1» разных сессий сталкивались и крали друг у
                        // друга ходы (поймано живой приёмкой, D-118).
                        id: TurnId::new(format!("dsh-{}-t{}", s.id, n)),
                        started_ms: time,
                        ended_ms: None,
                        content: String::new(),
                        reasoning: String::new(),
                        tool_calls: Vec::new(),
                        est_input_tokens: 0,
                        est_output_tokens: 0,
                        ok: false,
                        failure: None,
                        parent_turn_id: None,
                    },
                    msgs: Vec::new(),
                });
            }
            "user/message" => {
                let text = text_of(data.get("content").unwrap_or(&serde_json::Value::Null), "text");
                if text.is_empty() {
                    continue;
                }
                if first_user.is_empty() {
                    first_user = text.chars().take(60).collect();
                }
                if let Some(t) = current.as_mut() {
                    t.msgs.push(ChatMessage::user(text));
                }
            }
            "assistant/message" => {
                let Some(t) = current.as_mut() else { continue };
                let msg = data.get("message").cloned().unwrap_or(serde_json::Value::Null);
                let content = msg.get("content").cloned().unwrap_or(serde_json::Value::Null);
                let text = text_of(&content, "text");
                let reasoning = text_of(&content, "reasoning");
                if !text.is_empty() {
                    if !t.rec.content.is_empty() {
                        t.rec.content.push('\n');
                    }
                    t.rec.content.push_str(&text);
                    t.msgs.push(ChatMessage::assistant(text));
                }
                if !reasoning.is_empty() {
                    if !t.rec.reasoning.is_empty() {
                        t.rec.reasoning.push('\n');
                    }
                    t.rec.reasoning.push_str(&reasoning);
                }
            }
            "tool/call" => {
                let Some(t) = current.as_mut() else { continue };
                let id = data.get("callId").and_then(|v| v.as_str()).unwrap_or_default().to_string();
                let name = data.get("name").and_then(|v| v.as_str()).unwrap_or_default().to_string();
                // DSH хранит аргументы JSON-строкой — парсим в Map.
                let args_raw = data.get("arguments").and_then(|v| v.as_str()).unwrap_or("{}");
                let arguments = serde_json::from_str::<serde_json::Value>(args_raw)
                    .ok()
                    .and_then(|v| v.as_object().cloned())
                    .unwrap_or_default();
                t.rec.tool_calls.push(ToolCall { id, name, arguments });
            }
            "turn/end" => {
                if let Some(mut t) = current.take() {
                    let kind = data
                        .get("reason")
                        .and_then(|r| r.get("kind"))
                        .and_then(|v| v.as_str())
                        .unwrap_or("unknown")
                        .to_string();
                    t.rec.ended_ms = Some(time);
                    t.rec.ok = kind == "completed";
                    if !t.rec.ok {
                        t.rec.failure = Some(format!("dsh: {kind}"));
                    }
                    s.turns.push(t);
                }
            }
            _ => {} // остальные 8 типов (sandbox, approvals, inbox...) — не домен
        }
    }

    if s.id.is_empty() {
        return Err("в логе нет записи session".into());
    }
    if s.title.is_empty() {
        s.title = if !s.label.is_empty() {
            s.label.clone()
        } else if !first_user.is_empty() {
            first_user
        } else {
            "DSH-сессия".to_string()
        };
    }
    if s.model.is_empty() {
        s.model = "imported".to_string();
    }
    Ok(s)
}

/// Найти все файлы сессий под корнем DSH (два уровня: workspace/сессия).
fn session_files(root: &Path) -> Vec<PathBuf> {
    let mut out = Vec::new();
    let Ok(workspaces) = std::fs::read_dir(root) else { return out };
    for ws in workspaces.flatten() {
        let Ok(sessions) = std::fs::read_dir(ws.path()) else { continue };
        for ses in sessions.flatten() {
            let f = ses.path().join("session.v3.jsonl.zstd");
            if f.is_file() {
                out.push(f);
            }
        }
    }
    out.sort();
    out
}

/// Импорт всех сессий DSH в наше хранилище. Идемпотентно: существующие
/// id пропускаются (skipped), битые файлы логируются (errors) и не роняют
/// остальные.
pub fn import_dsh_sessions(store: &dyn Store, root: &Path) -> ImportReport {
    let mut report = ImportReport::default();
    for file in session_files(root) {
        let bytes = match std::fs::read(&file) {
            Ok(b) => b,
            Err(e) => {
                report.errors.push(format!("{}: {e}", file.display()));
                continue;
            }
        };
        let text = match decompress_session(&bytes) {
            Ok(t) => t,
            Err(e) => {
                report.errors.push(format!("{}: zstd: {e}", file.display()));
                continue;
            }
        };
        let parsed = match parse_session_log(&text) {
            Ok(p) => p,
            Err(e) => {
                report.errors.push(format!("{}: {e}", file.display()));
                continue;
            }
        };
        match store.load_session(&parsed.id) {
            Ok(Some(_)) => {
                report.skipped += 1;
                continue;
            }
            Ok(None) => {}
            Err(e) => {
                report.errors.push(format!("{}: store: {e}", file.display()));
                continue;
            }
        }

        let session = Session {
            id: SessionId::new(parsed.id.clone()),
            title: parsed.title.clone(),
            cwd: parsed.cwd.clone(),
            model: parsed.model.clone(),
            status: SessionStatus::Idle,
            current_turn: None,
            history: Vec::new(),
            summary: String::new(),
            approval_policy: None,
            turns: Vec::new(),
            created_ms: parsed.created_ms,
        };
        if let Err(e) = store.create_session(&session) {
            report.errors.push(format!("{}: create_session: {e}", file.display()));
            continue;
        }
        report.sessions += 1;

        // Ходы пишутся по одному с накопительным снимком истории — как в
        // живом ходе: load_session потом видит и журнал, и wire-историю.
        let mut history: Vec<ChatMessage> = Vec::new();
        for t in &parsed.turns {
            history.extend(t.msgs.iter().cloned());
            match store.save_turn(&parsed.id, &t.rec, &history) {
                Ok(()) => {
                    report.turns += 1;
                    report.messages += t.msgs.len();
                }
                Err(e) => {
                    report.errors.push(format!("{}: save_turn: {e}", file.display()));
                }
            }
        }
    }
    report
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Синтетический лог v3 в реальном формате (срез живой базы).
    fn sample_log() -> String {
        [
            r#"{"type":"session","version":3,"id":"session-aaaa","createdAt":1791037650240,"cwd":"D:\\proj","isSeeded":false,"delegationDepth":0}"#,
            r#"{"type":"subagent/descriptor","seq":0,"time":1791037650250,"data":{"label":"Проверка кода"}}"#,
            r#"{"type":"session/title","seq":1,"time":1791037650260,"data":{"title":"Правка платежей"}}"#,
            r#"{"type":"request/context","seq":2,"time":1791037650270,"data":{"provider":"rustvy","model":"claude-opus-5-5","contextWindow":262144}}"#,
            r#"{"type":"turn/start","seq":3,"time":1791037650280,"data":{"turn":1}}"#,
            r#"{"type":"user/message","seq":4,"time":1791037650290,"data":{"content":[{"type":"text","text":"почини оплату"}]}}"#,
            r#"{"type":"assistant/message","seq":5,"time":1791037650300,"data":{"turn":1,"step":1,"message":{"role":"assistant","content":[{"type":"reasoning","text":"думаю"},{"type":"text","text":"Запускаю проверку."},{"type":"tool-call","id":"call_1","name":"pwsh","arguments":"{}"}]}}}"#,
            r#"{"type":"tool/call","seq":6,"time":1791037650310,"data":{"turn":1,"step":1,"callId":"call_1","name":"pwsh","arguments":"{\"command\":\"cargo test\"}"}}"#,
            r#"{"type":"tool/result","seq":7,"time":1791037650320,"data":{"turn":1,"step":1,"message":{"role":"user","content":[{"type":"tool-result","toolCallId":"call_1","content":[{"type":"text","text":"ok"}],"isError":false}]}}}"#,
            r#"{"type":"turn/end","seq":8,"time":1791037650400,"data":{"turn":1,"reason":{"kind":"completed"}}}"#,
        ]
        .join("\n")
    }

    #[test]
    fn parses_records_into_domain() {
        let p = parse_session_log(&sample_log()).unwrap();
        assert_eq!(p.id, "session-aaaa");
        assert_eq!(p.title, "Правка платежей");
        assert_eq!(p.cwd, "D:\\proj");
        assert_eq!(p.model, "claude-opus-5-5");
        assert_eq!(p.created_ms, 1791037650240);
        assert_eq!(p.turns.len(), 1);
        let t = &p.turns[0];
        assert_eq!(t.rec.id.to_string(), "dsh-session-aaaa-t1");
        assert!(t.rec.ok);
        assert_eq!(t.rec.content, "Запускаю проверку.");
        assert_eq!(t.rec.reasoning, "думаю");
        assert_eq!(t.rec.tool_calls.len(), 1);
        assert_eq!(t.rec.tool_calls[0].name, "pwsh");
        assert_eq!(t.rec.tool_calls[0].arguments["command"], "cargo test");
        // wire-история: пользователь + ответ модели.
        assert_eq!(t.msgs.len(), 2);
        assert_eq!(t.msgs[0].content, "почини оплату");
        assert_eq!(t.msgs[1].content, "Запускаю проверку.");
    }

    #[test]
    fn title_falls_back_to_label_then_first_user() {
        let no_title: String = sample_log()
            .lines()
            .filter(|l| !l.contains("session/title"))
            .collect::<Vec<_>>()
            .join("\n");
        let p = parse_session_log(&no_title).unwrap();
        assert_eq!(p.title, "Проверка кода", "label суб-агента — второй приоритет");

        let no_both: String = no_title
            .lines()
            .filter(|l| !l.contains("subagent/descriptor"))
            .collect::<Vec<_>>()
            .join("\n");
        let p = parse_session_log(&no_both).unwrap();
        assert_eq!(p.title, "почини оплату", "первая реплика — третий приоритет");
    }

    #[test]
    fn failed_turn_end_marks_failure() {
        let log = sample_log().replace(r#""reason":{"kind":"completed"}"#, r#""reason":{"kind":"interrupted"}"#);
        let p = parse_session_log(&log).unwrap();
        assert!(!p.turns[0].rec.ok);
        assert_eq!(p.turns[0].rec.failure.as_deref(), Some("dsh: interrupted"));
    }

    #[test]
    fn missing_session_record_is_error() {
        assert!(parse_session_log(r#"{"type":"turn/start","data":{"turn":1}}"#).is_err());
    }

    /// Многокадровая упаковка: каждый кадр сжимаем отдельно и склеиваем —
    /// ровно так DSH дописывает лог.
    fn compress_frames(parts: &[&str]) -> Vec<u8> {
        let mut out = Vec::new();
        for part in parts {
            out.extend(zstd::stream::encode_all(part.as_bytes(), 3).unwrap());
        }
        out
    }

    #[test]
    fn decompress_handles_multiframe() {
        let log = sample_log();
        let half = log.len() / 2;
        let packed = compress_frames(&[&log[..half], &log[half..]]);
        let back = decompress_session(&packed).unwrap();
        assert_eq!(back, log);
    }

    #[test]
    fn decompress_rejects_garbage() {
        assert!(decompress_session(b"not zstd at all").is_err());
    }

    #[test]
    fn import_is_idempotent_and_fills_store() {
        let dir = std::env::temp_dir().join(format!(
            "swagcod-dsh-{}",
            std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).unwrap().as_nanos()
        ));
        let ses = dir.join("sessions").join("--D-proj--").join("session-aaaa");
        std::fs::create_dir_all(&ses).unwrap();
        let log = sample_log();
        let packed = compress_frames(&[&log]);
        std::fs::write(ses.join("session.v3.jsonl.zstd"), &packed).unwrap();

        let store = swagcod_core::store::open_memory();
        let r1 = import_dsh_sessions(store.as_ref(), &dir.join("sessions"));
        assert_eq!(r1.sessions, 1);
        assert_eq!(r1.turns, 1);
        assert_eq!(r1.messages, 2);
        assert!(r1.errors.is_empty(), "{:?}", r1.errors);

        let all = store.load_all().unwrap();
        assert_eq!(all.len(), 1);
        let s = &all[0];
        assert_eq!(s.id.as_str(), "session-aaaa");
        assert_eq!(s.title, "Правка платежей");
        assert_eq!(s.cwd, "D:\\proj");
        assert_eq!(s.model, "claude-opus-5-5");
        assert_eq!(s.created_ms, 1791037650240);
        assert_eq!(s.turns.len(), 1);
        assert_eq!(s.history.len(), 2);
        assert_eq!(s.history[0].content, "почини оплату");

        // Повторный импорт — тихий пропуск, без дублей.
        let r2 = import_dsh_sessions(store.as_ref(), &dir.join("sessions"));
        assert_eq!(r2.sessions, 0);
        assert_eq!(r2.skipped, 1);
        assert_eq!(store.load_all().unwrap().len(), 1);

        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn import_missing_root_is_quiet() {
        let store = swagcod_core::store::open_memory();
        let bogus = std::env::temp_dir().join("swagcod-dsh-nope-1a2b3c");
        let r = import_dsh_sessions(store.as_ref(), &bogus);
        assert_eq!(r.sessions, 0);
        assert!(r.errors.is_empty());
    }
}
