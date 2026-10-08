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
    /// Каркасы от убитых прогонов, которым догрузили недостающие ходы.
    pub resumed: usize,
    pub errors: Vec<String>,
    pub mcp_imported: usize,
    pub mcp_skipped: Vec<String>,
    pub plugin_notes: Vec<String>,
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

/// Инжект делегирования («Ты Abyss... Your parent agent id is ...»):
/// его шлёт родительский агент, а не человек. В историю попадает как
/// есть (субагент его реально получил), но титлом становиться не должен —
/// иначе сотни сессий называются «You are Abyss, created by» и неузнаваемы.
fn is_persona_prompt(text: &str) -> bool {
    text.to_lowercase().contains("parent agent id")
}

/// Запись JSONL-лога разбирается как общий Value: у записи `session`
/// доменные поля (id/cwd/createdAt) лежат НА ВЕРХНЕМ уровне, а у
/// остальных событий — под `data`. Жёсткая структура ломалась бы и там.
fn parse_session_log(text: &str) -> Result<ParsedSession, String> {
    let mut s = ParsedSession::default();
    let mut first_user = String::new();
    let mut first_any = String::new();
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
                // Оркестрация DSH тоже идёт ролью user: снимки систем-промпта,
                // notices tool-jobs, компакшн, итоги суб-агентов (на живой базе
                // таких 5045 из 8296). В чат берём только kind == "user".
                // Поле старое и опциональное: нет source — считаем пользователем.
                let is_user = data
                    .get("source")
                    .and_then(|s| s.get("kind"))
                    .and_then(|k| k.as_str())
                    .map(|k| k == "user")
                    .unwrap_or(true);
                if !is_user {
                    continue;
                }
                let text = text_of(data.get("content").unwrap_or(&serde_json::Value::Null), "text");
                if text.is_empty() {
                    continue;
                }
                if first_any.is_empty() {
                    first_any = text.chars().take(60).collect();
                }
                if first_user.is_empty() && !is_persona_prompt(&text) {
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

    // Хвост без turn/end: DSH дописывает лог наживую, и последний ход
    // живой сессии не закрыт. Ронять весь разговор из-за этого нельзя —
    // забираем как есть (на живой базе таких файлов 16, из них 8 вообще
    // без единого turn/end и раньше импортировались ПУСТЫМИ).
    if let Some(mut t) = current.take() {
        t.rec.ok = false;
        t.rec.failure = Some("dsh: open (нет turn/end — лог оборван)".into());
        s.turns.push(t);
    }

    if s.id.is_empty() {
        return Err("в логе нет записи session".into());
    }
    if s.title.is_empty() {
        s.title = if !s.label.is_empty() {
            s.label.clone()
        } else if !first_user.is_empty() {
            first_user
        } else if !first_any.is_empty() {
            // Только персона-инжект: лучше он, чем «DSH-сессия».
            first_any
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
        if parsed.turns.is_empty() {
            // Черновик без единого хода (открыли DSH и ничего не спросили) —
            // сессию-пустышку в базу не кладём, это пропуск, а не перенос.
            report.skipped += 1;
            continue;
        }
        match store.load_session(&parsed.id) {
            Ok(Some(existing)) => {
                // Догрузка вместо глухого пропуска: убитый прошлый прогон
                // оставляет в базе каркас без ходов, и старый код считал его
                // «уже перенесённым» навсегда. Сверяем id ходов и тащим
                // только недостающие (save_turn — INSERT OR REPLACE).
                let have: std::collections::HashSet<&str> = existing
                    .turns
                    .iter()
                    .map(|t| t.id.as_str())
                    .collect();
                let missing: Vec<&ParsedTurn> = parsed
                    .turns
                    .iter()
                    .filter(|t| !have.contains(t.rec.id.as_str()))
                    .collect();
                if missing.is_empty() {
                    report.skipped += 1;
                    continue;
                }
                if existing.title == "DSH-сессия" && parsed.title != "DSH-сессия" {
                    let _ = store.set_session_title(&parsed.id, &parsed.title);
                }
                let mut history = existing.history.clone();
                let mut added = 0;
                for t in missing {
                    history.extend(t.msgs.iter().cloned());
                    match store.save_turn(&parsed.id, &t.rec, &history) {
                        Ok(()) => {
                            added += 1;
                            report.turns += 1;
                            report.messages += t.msgs.len();
                        }
                        Err(e) => {
                            report.errors.push(format!("{}: save_turn: {e}", file.display()));
                        }
                    }
                }
                if added > 0 {
                    report.resumed += 1;
                }
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

/* ============ E-9b: конфиги MCP и плагинов из DSH ============
 *
 * DSH хранит их в `~/.dsh/profiles/<profile>/cordis.patch.yml`:
 * верхний массив, где записи либо лежат в `insert:`-списках
 * (MCP-серверы `mcp-*` через dsh-mcp-client), либо идут напрямую
 * (tool-web, agent-presets, theme-plugin...).
 *
 * Перенос честный, а не тихий:
 * - stdio-MCP → реестр SwagCod (`mcp_servers` в prefs), слияние по
 *   имени — повторный импорт дублей не плодит;
 * - не-stdio транспорт (streamable-http) — пропуск с причиной:
 *   наш клиент умеет только stdio;
 * - не-MCP записи — прямого аналога в SwagCod нет (наши плагины —
 *   JS-файлы с swagcod.define), поэтому сырой конфиг кладётся в
 *   преф `dsh_plugins_raw` для справки, а не выдумывается.
 */

/// Запись cordis-патча DSH: элемент insert-списка или прямая запись.
#[derive(Debug, Clone)]
pub struct DshPluginEntry {
    pub id: String,
    pub name: String,
    pub disabled: bool,
    pub config: serde_json::Value,
}

/// Итог переноса конфигов.
#[derive(Debug, Default)]
pub struct ConfigReport {
    pub mcp_imported: usize,
    pub mcp_skipped: Vec<String>,
    pub plugin_notes: Vec<String>,
}

/// Все патчи профилей DSH. Пусто, если DSH на машине нет.
pub fn dsh_cordis_patches() -> Vec<PathBuf> {
    let home = std::env::var("USERPROFILE")
        .or_else(|_| std::env::var("HOME"))
        .unwrap_or_default();
    if home.is_empty() {
        return Vec::new();
    }
    let profiles = PathBuf::from(home).join(".dsh").join("profiles");
    let Ok(dirs) = std::fs::read_dir(&profiles) else {
        return Vec::new();
    };
    let mut out = Vec::new();
    for d in dirs.flatten() {
        let f = d.path().join("cordis.patch.yml");
        if f.is_file() {
            out.push(f);
        }
    }
    out.sort();
    out
}

fn yaml_key<'a>(m: &'a serde_yaml::Mapping, key: &str) -> Option<&'a serde_yaml::Value> {
    m.get(serde_yaml::Value::String(key.to_string()))
}

fn yaml_scalar_to_string(v: &serde_yaml::Value) -> Option<String> {
    match v {
        serde_yaml::Value::Null => None,
        serde_yaml::Value::Bool(b) => Some(b.to_string()),
        serde_yaml::Value::Number(n) => Some(n.to_string()),
        serde_yaml::Value::String(s) => Some(s.clone()),
        _ => None,
    }
}

fn yaml_to_json(v: &serde_yaml::Value) -> serde_json::Value {
    match v {
        serde_yaml::Value::Null => serde_json::Value::Null,
        serde_yaml::Value::Bool(b) => serde_json::Value::Bool(*b),
        serde_yaml::Value::Number(n) => {
            if let Some(i) = n.as_i64() {
                serde_json::Value::from(i)
            } else if let Some(u) = n.as_u64() {
                serde_json::Value::from(u)
            } else if let Some(f) = n.as_f64() {
                serde_json::Value::from(f)
            } else {
                serde_json::Value::Null
            }
        }
        serde_yaml::Value::String(s) => serde_json::Value::String(s.clone()),
        serde_yaml::Value::Sequence(arr) => {
            serde_json::Value::Array(arr.iter().map(yaml_to_json).collect())
        }
        serde_yaml::Value::Mapping(m) => {
            let mut obj = serde_json::Map::new();
            for (k, val) in m.iter() {
                let key = yaml_scalar_to_string(k).unwrap_or_else(|| "?".to_string());
                obj.insert(key, yaml_to_json(val));
            }
            serde_json::Value::Object(obj)
        }
        serde_yaml::Value::Tagged(t) => yaml_to_json(&t.value),
    }
}

fn parse_patch_entry(v: &serde_yaml::Value) -> Option<DshPluginEntry> {
    let serde_yaml::Value::Mapping(m) = v else {
        return None;
    };
    let id = yaml_key(m, "id").and_then(yaml_scalar_to_string)?;
    if id.is_empty() {
        return None;
    }
    let name = yaml_key(m, "name")
        .and_then(yaml_scalar_to_string)
        .unwrap_or_else(|| id.clone());
    let disabled = yaml_key(m, "disabled")
        .and_then(|d| d.as_bool())
        .unwrap_or(false);
    let config = yaml_key(m, "config")
        .map(yaml_to_json)
        .unwrap_or(serde_json::Value::Null);
    Some(DshPluginEntry {
        id,
        name,
        disabled,
        config,
    })
}

/// Разобрать один cordis.patch.yml: insert-списки + прямые записи.
pub fn parse_cordis_patch(text: &str) -> Result<Vec<DshPluginEntry>, String> {
    let v: serde_yaml::Value =
        serde_yaml::from_str(text).map_err(|e| format!("yaml: {e}"))?;
    let serde_yaml::Value::Sequence(items) = &v else {
        return Err("корень патча — не массив".into());
    };
    let mut out = Vec::new();
    for item in items {
        if let serde_yaml::Value::Mapping(m) = item {
            if let Some(serde_yaml::Value::Sequence(arr)) = yaml_key(m, "insert") {
                for e in arr {
                    if let Some(en) = parse_patch_entry(e) {
                        out.push(en);
                    }
                }
                continue;
            }
            if let Some(en) = parse_patch_entry(item) {
                out.push(en);
            }
        }
    }
    Ok(out)
}

fn json_scalar_to_string(v: &serde_json::Value) -> String {
    match v {
        serde_json::Value::String(s) => s.clone(),
        serde_json::Value::Null => String::new(),
        _ => v.to_string(),
    }
}

/// Запись DSH → конфиг MCP-сервера SwagCod. Ошибка — честная причина
/// пропуска (чужой транспорт, нет команды), а не тихий дроп.
pub fn mcp_from_entry(
    e: &DshPluginEntry,
) -> Result<crate::mcp::McpServerConfig, String> {
    if !e.id.starts_with("mcp-") {
        return Err(format!("{}: не MCP-запись", e.id));
    }
    let transport = e
        .config
        .get("transport")
        .and_then(|t| t.as_str())
        .unwrap_or("stdio");
    if transport != "stdio" {
        return Err(format!(
            "{}: транспорт {transport} не поддерживается (только stdio)",
            e.id
        ));
    }
    let command = e
        .config
        .get("command")
        .and_then(|c| c.as_str())
        .unwrap_or("")
        .to_string();
    if command.is_empty() {
        return Err(format!("{}: нет command", e.id));
    }
    let name = e
        .config
        .get("serverName")
        .and_then(|s| s.as_str())
        .map(|s| s.to_string())
        .unwrap_or_else(|| e.id.trim_start_matches("mcp-").to_string());
    let args = e
        .config
        .get("args")
        .and_then(|a| a.as_array())
        .map(|arr| arr.iter().map(json_scalar_to_string).collect())
        .unwrap_or_default();
    let env = e
        .config
        .get("env")
        .and_then(|e| e.as_object())
        .map(|obj| {
            obj.iter()
                .map(|(k, v)| (k.clone(), json_scalar_to_string(v)))
                .collect()
        })
        .unwrap_or_default();
    Ok(crate::mcp::McpServerConfig {
        name,
        command,
        args,
        env,
        enabled: !e.disabled,
    })
}

/// Слияние с реестром без дублей (чистая функция — тестируется).
/// Возвращает (реестр, импортировано, имена-дубли).
pub fn merge_mcp_configs(
    mut existing: Vec<crate::mcp::McpServerConfig>,
    incoming: Vec<crate::mcp::McpServerConfig>,
) -> (Vec<crate::mcp::McpServerConfig>, usize, Vec<String>) {
    let mut imported = 0;
    let mut dups = Vec::new();
    for cfg in incoming {
        if existing.iter().any(|e| e.name == cfg.name) {
            dups.push(cfg.name);
        } else {
            existing.push(cfg);
            imported += 1;
        }
    }
    (existing, imported, dups)
}

/// Перенести конфиги MCP/плагинов из всех профилей DSH в prefs.
/// Идемпотентно: повторный прогон никого не дублирует.
pub fn import_dsh_configs(store: &dyn Store) -> ConfigReport {
    let mut report = ConfigReport::default();
    let files = dsh_cordis_patches();
    if files.is_empty() {
        return report; // DSH нет — тихо, это норма
    }
    let mut entries = Vec::new();
    for f in &files {
        match std::fs::read_to_string(f) {
            Ok(text) => match parse_cordis_patch(&text) {
                Ok(mut e) => entries.append(&mut e),
                Err(e) => report
                    .mcp_skipped
                    .push(format!("{}: {e}", f.display())),
            },
            Err(e) => report
                .mcp_skipped
                .push(format!("{}: чтение: {e}", f.display())),
        }
    }
    let mut incoming = Vec::new();
    let mut raws = serde_json::Map::new();
    for e in &entries {
        match mcp_from_entry(e) {
            Ok(cfg) => {
                if e.config.get("cwd").and_then(|c| c.as_str()).is_some() {
                    report.plugin_notes.push(format!(
                        "mcp {}: cwd в SwagCod не поддерживается — если сервер не поднимется, смотри его рабочий каталог в DSH",
                        cfg.name
                    ));
                }
                incoming.push(cfg);
            }
            Err(reason) => {
                if e.id.starts_with("mcp-") {
                    report.mcp_skipped.push(reason);
                } else {
                    // Конфиг плагина DSH без аналога у нас — сохраняем
                    // сырьём в преф для справки, а не выдумываем маппинг.
                    raws.insert(
                        e.id.clone(),
                        serde_json::json!({
                            "name": e.name,
                            "disabled": e.disabled,
                            "config": e.config,
                        }),
                    );
                    report.plugin_notes.push(format!(
                        "{}: прямого аналога в SwagCod нет, конфиг сохранён в преф dsh_plugins_raw",
                        e.id
                    ));
                }
            }
        }
    }
    if !raws.is_empty() {
        if let Err(e) = store.set_pref(
            "dsh_plugins_raw",
            &serde_json::Value::Object(raws).to_string(),
        ) {
            report
                .plugin_notes
                .push(format!("dsh_plugins_raw не записан: {e}"));
        }
    }
    let existing: Vec<crate::mcp::McpServerConfig> = store
        .get_pref("mcp_servers")
        .ok()
        .flatten()
        .and_then(|s| serde_json::from_str(&s).ok())
        .unwrap_or_default();
    let (merged, imported, dups) = merge_mcp_configs(existing, incoming);
    report.mcp_imported = imported;
    for d in dups {
        report
            .mcp_skipped
            .push(format!("mcp {d}: уже есть в реестре"));
    }
    match serde_json::to_string(&merged) {
        Ok(s) => {
            if let Err(e) = store.set_pref("mcp_servers", &s) {
                report
                    .mcp_skipped
                    .push(format!("mcp_servers не записан: {e}"));
            }
        }
        Err(e) => report
            .mcp_skipped
            .push(format!("mcp_servers не сериализован: {e}")),
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

    /// Хвост без turn/end (живая сессия, лог оборван) — ход забираем как
    /// есть, а не роняем весь разговор. 8 файлов живой базы без единого
    /// turn/end раньше импортировались ПУСТЫМИ.
    #[test]
    fn trailing_open_turn_is_flushed() {
        let log: String = sample_log()
            .lines()
            .filter(|l| !l.contains(r#""type":"turn/end""#))
            .collect::<Vec<_>>()
            .join("\n");
        let p = parse_session_log(&log).unwrap();
        assert_eq!(p.turns.len(), 1);
        assert!(!p.turns[0].rec.ok);
        assert!(p.turns[0].rec.failure.as_deref().unwrap().contains("open"));
        assert_eq!(p.turns[0].rec.content, "Запускаю проверку.");
        assert_eq!(p.turns[0].msgs.len(), 2);
    }

    /// Оркестрационный мусор ролью user (снимки систем-промпта, notices)
    /// в чат не лезет. Без source (старые логи) — считаем пользователем.
    #[test]
    fn plugin_user_messages_ignored() {
        let mut log = sample_log();
        log.push_str(r#"
{"type":"user/message","seq":9,"time":1791037650500,"data":{"content":[{"type":"text","text":"Current DSH file policy: danger-full-access"}],"source":{"kind":"plugin","plugin":"tool-jobs"},"role":"user"}}"#);
        let p = parse_session_log(&log).unwrap();
        assert_eq!(p.turns.len(), 1);
        // Мусор вне хода и так не попадал бы в msgs; проверяем напрямую:
        // титл-фолбэк и первое сообщение — от настоящего пользователя.
        assert_eq!(p.title, "Правка платежей");
        assert_eq!(p.turns[0].msgs.len(), 2);
        assert_eq!(p.turns[0].msgs[0].content, "почини оплату");
    }

    /// Черновик без ходов сессию-пустышку не создаёт: пропуск, а не перенос.
    #[test]
    fn empty_session_is_skipped_not_created() {
        let dir = std::env::temp_dir().join(format!(
            "swagcod-dsh-empty-{}",
            std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).unwrap().as_nanos()
        ));
        let ses = dir.join("sessions").join("--D-proj--").join("session-empt");
        std::fs::create_dir_all(&ses).unwrap();
        let log = r#"{"type":"session","version":3,"id":"session-empt","createdAt":1791037650240,"cwd":"D:\\proj"}"#.to_string();
        let packed = compress_frames(&[&log]);
        std::fs::write(ses.join("session.v3.jsonl.zstd"), &packed).unwrap();

        let store = swagcod_core::store::open_memory();
        let r = import_dsh_sessions(store.as_ref(), &dir.join("sessions"));
        assert_eq!(r.sessions, 0);
        assert_eq!(r.skipped, 1);
        assert!(r.errors.is_empty(), "{:?}", r.errors);
        assert!(store.load_all().unwrap().is_empty());

        let _ = std::fs::remove_dir_all(&dir);
    }

    /// Догрузка: убитый прогон оставил каркас (сессия без ходов) — повтор
    /// тащит недостающее, а не считает «уже перенесено».
    fn two_turn_log() -> String {
        let mut log = sample_log();
        log.push_str(r#"
{"type":"turn/start","seq":9,"time":1791037650500,"data":{"turn":2}}
{"type":"user/message","seq":10,"time":1791037650510,"data":{"content":[{"type":"text","text":"а теперь тесты"}],"source":{"kind":"user"},"role":"user"}}
{"type":"assistant/message","seq":11,"time":1791037650520,"data":{"turn":2,"step":1,"message":{"role":"assistant","content":[{"type":"text","text":"Гоняю тесты."}]}}}
{"type":"turn/end","seq":12,"time":1791037650600,"data":{"turn":2,"reason":{"kind":"completed"}}}"#);
        log
    }

    #[test]
    fn resume_backfills_missing_turns() {
        let dir = std::env::temp_dir().join(format!(
            "swagcod-dsh-resume-{}",
            std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).unwrap().as_nanos()
        ));
        let ses = dir.join("sessions").join("--D-proj--").join("session-aaaa");
        std::fs::create_dir_all(&ses).unwrap();
        let f = ses.join("session.v3.jsonl.zstd");

        // Прогон 1 оборван на первом ходе (в базе каркас + 1 ход).
        let packed = compress_frames(&[&sample_log()]);
        std::fs::write(&f, &packed).unwrap();
        let store = swagcod_core::store::open_memory();
        let r1 = import_dsh_sessions(store.as_ref(), &dir.join("sessions"));
        assert_eq!(r1.sessions, 1);

        // DSH дописала второй ход. Прогон 2 догружает только его.
        let packed = compress_frames(&[&two_turn_log()]);
        std::fs::write(&f, &packed).unwrap();
        let r2 = import_dsh_sessions(store.as_ref(), &dir.join("sessions"));
        assert_eq!(r2.sessions, 0);
        assert_eq!(r2.skipped, 0);
        assert_eq!(r2.resumed, 1);
        assert_eq!(r2.turns, 1);
        assert_eq!(r2.messages, 2);
        assert!(r2.errors.is_empty(), "{:?}", r2.errors);

        let s = store.load_session("session-aaaa").unwrap().unwrap();
        assert_eq!(s.turns.len(), 2);
        assert_eq!(s.history.len(), 4);
        assert_eq!(s.history[2].content, "а теперь тесты");
        assert_eq!(s.history[3].content, "Гоняю тесты.");

        // Прогон 3: всё на месте — честный пропуск.
        let r3 = import_dsh_sessions(store.as_ref(), &dir.join("sessions"));
        assert_eq!(r3.resumed, 0);
        assert_eq!(r3.skipped, 1);

        let _ = std::fs::remove_dir_all(&dir);
    }

    /// Титл пропускает персона-инжект делегирования и берёт первую
    /// настоящую реплику; инжект — только запасной вариант.
    #[test]
    fn title_skips_persona_prompt() {
        let log = [
            r#"{"type":"session","version":3,"id":"session-pp","createdAt":1791037650240,"cwd":"D:\\proj"}"#,
            r#"{"type":"turn/start","seq":1,"time":1791037650280,"data":{"turn":1}}"#,
            r#"{"type":"user/message","seq":2,"time":1791037650290,"data":{"content":[{"type":"text","text":"Ты Abyss. Your parent agent id is \"session-1\". Выдай полный разбор."}],"source":{"kind":"user"},"role":"user"}}"#,
            r#"{"type":"user/message","seq":3,"time":1791037650300,"data":{"content":[{"type":"text","text":"продолжи работу за прошлым агентом"}],"source":{"kind":"user"},"role":"user"}}"#,
            r#"{"type":"turn/end","seq":4,"time":1791037650400,"data":{"turn":1,"reason":{"kind":"completed"}}}"#,
        ]
        .join("\n");
        let p = parse_session_log(&log).unwrap();
        assert_eq!(p.title, "продолжи работу за прошлым агентом");
        // Обе реплики в истории — инжект реально был получен.
        assert_eq!(p.turns[0].msgs.len(), 2);
    }

    /// Срез реального cordis.patch.yml: insert-список MCP + прямая запись.
    fn sample_patch() -> &'static str {
        r#"
- insert:
    - id: mcp-codebase-memory
      name: '@deepseek-ai/dsh-mcp-client'
      config:
        serverName: codebase-memory
        transport: stdio
        command: C:/Users/norw/.local/bin/codebase-memory-mcp.exe
        args: []
        toolCallTimeoutMs: 60000
        failOnStartupError: false
    - id: mcp-ida-pro
      name: '@deepseek-ai/dsh-mcp-client'
      config:
        serverName: ida-pro
        transport: streamable-http
        url: http://127.0.0.1:13337/mcp
        toolCallTimeoutMs: 60000
        failOnStartupError: false
- id: tool-web
  name: '@deepseek-ai/dsh-tool-web'
  disabled: false
  config:
    search: false
    fetch: true
    fetchTimeoutMs: 60000
"#
    }

    #[test]
    fn parses_patch_inserts_and_direct_entries() {
        let entries = parse_cordis_patch(sample_patch()).unwrap();
        assert_eq!(entries.len(), 3);
        assert_eq!(entries[0].id, "mcp-codebase-memory");
        assert_eq!(entries[1].id, "mcp-ida-pro");
        assert_eq!(entries[2].id, "tool-web");
        assert!(!entries[2].disabled);
        assert_eq!(
            entries[2].config.get("fetch").and_then(|v| v.as_bool()),
            Some(true)
        );
    }

    #[test]
    fn parses_empty_patch_to_nothing() {
        let entries = parse_cordis_patch("[]").unwrap();
        assert!(entries.is_empty());
    }

    #[test]
    fn maps_stdio_mcp_entry() {
        let entries = parse_cordis_patch(sample_patch()).unwrap();
        let cfg = mcp_from_entry(&entries[0]).unwrap();
        assert_eq!(cfg.name, "codebase-memory");
        assert_eq!(
            cfg.command,
            "C:/Users/norw/.local/bin/codebase-memory-mcp.exe"
        );
        assert!(cfg.args.is_empty());
        assert!(cfg.enabled);
    }

    #[test]
    fn rejects_http_transport_with_reason() {
        let entries = parse_cordis_patch(sample_patch()).unwrap();
        let err = mcp_from_entry(&entries[1]).unwrap_err();
        assert!(err.contains("streamable-http"), "{err}");
    }

    #[test]
    fn rejects_non_mcp_entry() {
        let entries = parse_cordis_patch(sample_patch()).unwrap();
        let err = mcp_from_entry(&entries[2]).unwrap_err();
        assert!(err.contains("не MCP"), "{err}");
    }

    #[test]
    fn merge_mcp_dedups_by_name() {
        use crate::mcp::McpServerConfig;
        let mk = |name: &str| McpServerConfig {
            name: name.to_string(),
            command: "srv".to_string(),
            args: vec![],
            env: Default::default(),
            enabled: true,
        };
        let existing = vec![mk("a")];
        let (merged, imported, dups) = merge_mcp_configs(existing, vec![mk("a"), mk("b")]);
        assert_eq!(imported, 1);
        assert_eq!(dups, vec!["a".to_string()]);
        assert_eq!(merged.len(), 2);
    }

    #[test]
    fn config_import_moves_stdio_and_stashes_plugins() {
        let dir = std::env::temp_dir().join(format!(
            "swagcod-dsh-cfg-{}",
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        ));
        let prof = dir.join(".dsh").join("profiles").join("desktop");
        std::fs::create_dir_all(&prof).unwrap();
        std::fs::write(prof.join("cordis.patch.yml"), sample_patch()).unwrap();

        // Подменяем HOME точечно: dsh_cordis_patches читает USERPROFILE/HOME.
        let old_profile = std::env::var_os("USERPROFILE");
        let old_home = std::env::var_os("HOME");
        std::env::set_var("USERPROFILE", &dir);
        std::env::set_var("HOME", &dir);
        let store = swagcod_core::store::open_memory();
        let r = import_dsh_configs(store.as_ref());
        if let Some(v) = old_profile.as_ref() {
            std::env::set_var("USERPROFILE", v);
        } else {
            std::env::remove_var("USERPROFILE");
        }
        if let Some(v) = old_home.as_ref() {
            std::env::set_var("HOME", v);
        } else {
            std::env::remove_var("HOME");
        }

        assert_eq!(r.mcp_imported, 1, "report: {r:?}");
        assert!(r.mcp_skipped.iter().any(|s| s.contains("ida-pro")));
        assert!(r.plugin_notes.iter().any(|s| s.contains("tool-web")));

        // Реестр записан, сырьё плагинов — в префе.
        let raw = store.get_pref("mcp_servers").unwrap().unwrap();
        let cfgs: Vec<crate::mcp::McpServerConfig> = serde_json::from_str(&raw).unwrap();
        assert_eq!(cfgs.len(), 1);
        assert_eq!(cfgs[0].name, "codebase-memory");
        let raws = store.get_pref("dsh_plugins_raw").unwrap().unwrap();
        assert!(raws.contains("tool-web"));

        // Повтор — без дублей.
        std::env::set_var("USERPROFILE", &dir);
        std::env::set_var("HOME", &dir);
        let r2 = import_dsh_configs(store.as_ref());
        if let Some(v) = old_profile.as_ref() {
            std::env::set_var("USERPROFILE", v);
        } else {
            std::env::remove_var("USERPROFILE");
        }
        if let Some(v) = old_home.as_ref() {
            std::env::set_var("HOME", v);
        } else {
            std::env::remove_var("HOME");
        }
        assert_eq!(r2.mcp_imported, 0);
        let raw2 = store.get_pref("mcp_servers").unwrap().unwrap();
        let cfgs2: Vec<crate::mcp::McpServerConfig> = serde_json::from_str(&raw2).unwrap();
        assert_eq!(cfgs2.len(), 1);

        let _ = std::fs::remove_dir_all(&dir);
    }
}
