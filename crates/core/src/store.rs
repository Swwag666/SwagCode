/*! Персистентность сессий и настроек (этап B-1).

Синхронный SQLite, вмурованный в бинарник (`rusqlite` + `bundled`): философия
better-sqlite — sync API без async-обвязки, потому что SQLite быстр, а async
над ним дал бы только сложность. Режим WAL: запись в конце хода не блокирует
читателей во время стрима.

Схема минимальна и соответствует домену: сессия, ходы, сообщения истории и
ключ-значение настроек. История хода пишется пачкой в одной транзакции в
конце хода — оборванный процесс не оставляет полупустой ход (WAL + транзакция).
*/

use std::path::Path;

use rusqlite::{params, Connection};

use crate::session::{Session, SessionId, SessionStatus, TurnRecord};
use swagcod_provider::types::{ChatMessage, Role, ToolCall};

#[derive(Debug, thiserror::Error)]
pub enum StoreError {
    #[error("база: {0}")]
    Db(#[from] rusqlite::Error),
    #[error("файл: {0}")]
    Io(#[from] std::io::Error),
    #[error("json: {0}")]
    Json(#[from] serde_json::Error),
    /// Ревизия 25: ошибки Node-sidecar (better-sqlite3) — процесс, канал, SQL.
    #[error("sidecar: {0}")]
    Sidecar(String),
    /// F-7: нарушение доменного правила (область памяти, длина строки).
    /// Отдельный вариант, а не `Sidecar`: проверки общие для обеих
    /// реализаций Store и к каналу с Node отношения не имеют.
    #[error("недопустимые данные: {0}")]
    Invalid(String),
}

pub type StoreResult<T> = Result<T, StoreError>;

/// B-7: запись журнала подтверждений.
#[derive(Debug, Clone, PartialEq, serde::Serialize)]
pub struct ApprovalEntry {
    pub session_id: String,
    pub turn_id: String,
    pub call_id: String,
    pub tool: String,
    pub summary: String,
    /// "approved" | "denied".
    pub decision: String,
    /// "user" — человек в диалоге; "system" — канал потерян (ход остановлен),
    /// молчаливое одобрение исключено политикой.
    pub actor: String,
    pub decided_ms: u64,
}

/// F-7: строка персистентной памяти.
#[derive(Debug, Clone, PartialEq, serde::Serialize, serde::Deserialize)]
pub struct MemoryEntry {
    pub id: i64,
    /// "global" — факты приложения (вшиваются в системный промпт каждого
    /// хода), "session" — заметка одной сессии, "folder" — факты рабочей
    /// папки (видны сессиям с этим cwd).
    pub scope: String,
    pub text: String,
    pub created_ms: u64,
    /// Заполнен у строк с scope="session".
    pub session_id: Option<String>,
    /// Заполнен у строк с scope="folder": рабочая папка, которой принадлежит
    /// факт. Хранится дословно как пришёл (путь из диалога или cwd сессии),
    /// сравнение при выборке — побайтовое.
    #[serde(default)]
    pub dir: Option<String>,
}

/// Область памяти: факты приложения.
pub const MEMORY_SCOPE_GLOBAL: &str = "global";
/// Область памяти: заметки одной сессии.
pub const MEMORY_SCOPE_SESSION: &str = "session";
/// Область памяти: факты рабочей папки — видны любой сессии с этим cwd и не
/// уезжают в остальные папки.
pub const MEMORY_SCOPE_FOLDER: &str = "folder";
/// Потолок одной строки памяти в символах.
///
/// Память вшивается в КАЖДЫЙ запрос хода (кап секции —
/// `context::FACTS_MAX_CHARS`), поэтому длинная «заметка» стоит токенов на
/// всех последующих ходах. Превышение — ошибка, а не тихая обрезка:
/// молча отрезанный факт выглядит сохранённым.
///
/// Потолок равен `FACTS_MAX_CHARS` (4000): ровно столько вмещает секция
/// фактов, значит один импортированный из файла промпт в неё помещается
/// целиком. Длиннее — не сюда: для большого промпта есть `.swagcod/rules.md`
/// (кап 8000, `folder_rules::FOLDER_RULES_MAX_CHARS`), он и создан для текста
/// на всю папку.
pub const MEMORY_TEXT_MAX_CHARS: usize = 4000;

/// Проверка текста памяти — одна на обе реализации Store, чтобы правило не
/// разъехалось между rusqlite и sidecar.
pub(crate) fn memory_check_text(text: &str) -> StoreResult<String> {
    let t = text.trim();
    if t.is_empty() {
        return Err(StoreError::Invalid("пустая строка памяти".into()));
    }
    if t.chars().count() > MEMORY_TEXT_MAX_CHARS {
        return Err(StoreError::Invalid(format!(
            "строка памяти длиннее {} символов: разбейте на несколько, а для большого промпта \
             возьмите .swagcod/rules.md (там кап {})",
            MEMORY_TEXT_MAX_CHARS,
            crate::folder_rules::FOLDER_RULES_MAX_CHARS
        )));
    }
    /* Внутренние переводы строк сохраняются дословно: владелец импортирует в
    память целые промпты из txt/md, и нумерованные шаги или markdown-заголовки
    в таком тексте — структура, а не мусор. В системный промпт факт уходит
    одним буллетом с отступом продолжения (см. `memory_facts_for_prompt`). */
    Ok(t.to_string())
}

/// Нормализовать scope: неизвестное имя — не «global по умолчанию», а ошибка.
/// Молча превратить заметку сессии в глобальный факт значит показать её во
/// всех остальных сессиях.
pub(crate) fn memory_check_scope(scope: &str) -> StoreResult<String> {
    match scope.trim() {
        MEMORY_SCOPE_GLOBAL | MEMORY_SCOPE_SESSION | MEMORY_SCOPE_FOLDER => {
            Ok(scope.trim().to_string())
        }
        other => Err(StoreError::Invalid(format!(
            "неизвестная область памяти {other:?} (global | session | folder)"
        ))),
    }
}

/// Владелец строки памяти обязан соответствовать области: у global его нет, у
/// session это id сессии, у folder — путь рабочей папки.
///
/// Проверка одна на обе реализации Store (как [`memory_check_text`]), поэтому
/// rusqlite и sidecar не могут разойтись в том, что считается валидной строкой.
/// Несовпадение — ошибка, а не «запишем что дали»: `session_id` и `dir` лежат в
/// разных колонках, и факт с проставленным не тем владельцем либо не выбрался бы
/// никогда (потеря молча), либо попал бы в чужую сессию или чужую папку.
pub(crate) fn memory_check_owner(
    scope: &str,
    session_id: Option<&str>,
    dir: Option<&str>,
) -> StoreResult<(Option<String>, Option<String>)> {
    let sid = session_id.map(str::trim).filter(|s| !s.is_empty());
    let d = dir.map(str::trim).filter(|s| !s.is_empty());
    match scope {
        MEMORY_SCOPE_GLOBAL => {
            if sid.is_some() || d.is_some() {
                return Err(StoreError::Invalid(
                    "глобальный факт не принадлежит ни сессии, ни папке".into(),
                ));
            }
            Ok((None, None))
        }
        MEMORY_SCOPE_SESSION => {
            let Some(sid) = sid else {
                return Err(StoreError::Invalid(
                    "заметка сессии без session_id: область session требует владельца".into(),
                ));
            };
            if d.is_some() {
                return Err(StoreError::Invalid(
                    "заметка сессии не может одновременно принадлежать папке".into(),
                ));
            }
            Ok((Some(sid.to_string()), None))
        }
        MEMORY_SCOPE_FOLDER => {
            let Some(d) = d else {
                return Err(StoreError::Invalid(
                    "факт папки без пути: область folder требует рабочую папку".into(),
                ));
            };
            if sid.is_some() {
                return Err(StoreError::Invalid(
                    "факт папки не может одновременно принадлежать сессии".into(),
                ));
            }
            Ok((None, Some(d.to_string())))
        }
        other => Err(StoreError::Invalid(format!(
            "неизвестная область памяти {other:?} (global | session | folder)"
        ))),
    }
}

/// F-10: совпадение полнотекстового поиска по истории сообщений.
#[derive(Debug, Clone, PartialEq, serde::Serialize)]
pub struct MessageHit {
    pub session_id: String,
    /// Заголовок сессии — в списке результатов важнее id: по нему человек
    /// узнаёт разговор.
    pub session_title: String,
    /// Позиция сообщения в истории сессии (тот же порядок, что в messages).
    pub ord: u32,
    /// "user" | "assistant" | "tool" | "system".
    pub role: String,
    /// Фрагмент текста с подсветкой: совпадения заключены в `[`…`]`.
    pub snippet: String,
    /// Ранг bm25: чем меньше (отрицательнее), тем лучше совпадение.
    pub rank: f64,
}

/// Потолок слов в одном поисковом запросе.
///
/// Каждое слово — отдельный префиксный токен MATCH; без потолка запрос из
/// вставленного абзаца превратился бы в выражение, которое FTS5 разбирает
/// дольше, чем человек его читал.
pub const SEARCH_MAX_TOKENS: usize = 16;
/// Потолок длины запроса в символах.
pub const SEARCH_MAX_QUERY_CHARS: usize = 200;
/// Потолок выдачи: список результатов всё равно листают, а не читают весь.
pub const SEARCH_MAX_LIMIT: usize = 200;

/* F-10: SQL поиска — один на обе реализации Store (rusqlite и sidecar),
иначе выборки разъехались бы по порядку колонок или по сниппету.
snippet(-1) сам выбирает колонку с лучшим совпадением: текст ответа и
рассуждения индексируются разными колонками. `?1`/`?2` — нумерованные
плейсхолдеры, их понимают и rusqlite, и better-sqlite3. */
pub(crate) const SEARCH_MESSAGES_SQL: &str = "SELECT m.session_id, s.title, m.ord, m.role,
       snippet(messages_fts, -1, '[', ']', '…', 12) AS snip,
       bm25(messages_fts) AS rank
FROM messages_fts
JOIN messages m ON m.id = messages_fts.rowid
JOIN sessions s ON s.id = m.session_id
WHERE messages_fts MATCH ?1
ORDER BY rank LIMIT ?2";

/* F-10: нужна ли разовая дозация индекса. Базы, созданные до FTS5, уже
содержат messages, а новый индекс пуст — без rebuild поиск в них не нашёл
бы ничего и молча.
   Пустота индекса проверяется по теневой таблице `messages_fts_docsize`
   (строка на каждый индексированный документ), а НЕ по `messages_fts`:
   у external-content индекса полное сканирование читает таблицу-содержимое,
   поэтому `SELECT rowid FROM messages_fts` вернул бы строки messages даже при
   пустом индексе, и дозация не запустилась бы никогда. Оба подзапроса —
   LIMIT 1, то есть O(1) на каждом старте: COUNT(*) по истории съел бы бюджет
   запуска (400 мс, D-013). */
pub(crate) const FTS_NEEDS_BACKFILL_SQL: &str =
    "SELECT (SELECT COUNT(*) FROM (SELECT rowid FROM messages_fts_docsize LIMIT 1)) = 0
     AND (SELECT COUNT(*) FROM (SELECT id FROM messages LIMIT 1)) = 1";
/// Пересборка external-content индекса из messages (однократно).
pub(crate) const FTS_REBUILD_SQL: &str = "INSERT INTO messages_fts(messages_fts) VALUES('rebuild')";

/// Запрос человека → безопасное выражение MATCH.
///
/// Проверка одна на обе реализации, и она обязательна: FTS5 на пустой строке
/// ведёт себя по-разному — rusqlite молча возвращает ноль строк, а
/// better-sqlite3 бросает «fts5: syntax error». То есть без общего правила
/// «пустой запрос» значил бы разное на двух бэкендах.
///
/// Слово режется по не-буквенно-цифровым символам, каждый кусок кавычится и
/// получает префикс. Кавычки обязательны: иначе `OR`, `NOT`, `*`, `(` из
/// запроса человека стали бы синтаксисом FTS5. А резать слово нужно потому,
/// что токенайзер `unicode61` сам считает пунктуацию разделителем: в индексе
/// лежит `привет`, а не `"привет"`, поэтому буквальный запрос с кавычками не
/// нашёл бы ничего (проверено пробой). Отсюда же честное поведение на
/// вставке: `c++` ищется как `c`, `3.14` — как `3` И `14`, а фраза в кавычках
/// — как И всех её слов.
pub(crate) fn fts_match_expr(query: &str) -> StoreResult<String> {
    let trimmed = query.trim();
    if trimmed.is_empty() {
        return Err(StoreError::Invalid("пустой поисковый запрос".into()));
    }
    if trimmed.chars().count() > SEARCH_MAX_QUERY_CHARS {
        return Err(StoreError::Invalid(format!(
            "поисковый запрос длиннее {SEARCH_MAX_QUERY_CHARS} символов"
        )));
    }
    let toks: Vec<&str> = trimmed.split_whitespace().collect();
    if toks.len() > SEARCH_MAX_TOKENS {
        return Err(StoreError::Invalid(format!(
            "в поисковом запросе больше {SEARCH_MAX_TOKENS} слов"
        )));
    }
    let mut parts: Vec<String> = Vec::new();
    for tok in toks {
        /* Управляемые символы — не буквы и не цифры, поэтому они работают как
        разделители и в SQL не доезжают. Кусок без букв и цифр («---», «***»)
        просто не даёт токена: внутри MATCH он стал бы синтаксической ошибкой. */
        for piece in tok.split(|c: char| !c.is_alphanumeric()) {
            if piece.is_empty() {
                continue;
            }
            // Удвоение кавычки — защита в глубину: после разреза по
            // не-алфавитному её здесь не бывает, но полагаться на это незачем.
            let quoted = piece.replace('"', "\"\"");
            // Префикс: «тест» находит и «тесты», иначе поиск по русскому
            // тексту требовал бы угадать словоформу.
            parts.push(format!("\"{quoted}\"*"));
        }
    }
    if parts.is_empty() {
        return Err(StoreError::Invalid(
            "в поисковом запросе нет ни одного слова".into(),
        ));
    }
    // Пробел между токенами — неявный AND: ищем сообщения, где есть все слова.
    Ok(parts.join(" "))
}

/// Предел выдачи: 0 или безумное число — не повод сканировать весь индекс.
pub(crate) fn search_limit(limit: usize) -> usize {
    limit.clamp(1, SEARCH_MAX_LIMIT)
}

/// Схема и режимы. WAL и NORMAL выставляются сразу: после перезапуска
/// приложения база обязана продолжать писать без настройки извне.
const SCHEMA: &str = r#"
PRAGMA journal_mode = WAL;
PRAGMA synchronous = NORMAL;
PRAGMA foreign_keys = ON;
CREATE TABLE IF NOT EXISTS sessions(
  id TEXT PRIMARY KEY,
  cwd TEXT NOT NULL,
  model TEXT NOT NULL,
  title TEXT NOT NULL DEFAULT '',
  summary TEXT NOT NULL DEFAULT '',
  approval_policy TEXT,
  created_ms INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS turns(
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  started_ms INTEGER NOT NULL,
  ended_ms INTEGER,
  ok INTEGER NOT NULL,
  failure TEXT,
  content TEXT NOT NULL,
  reasoning TEXT NOT NULL,
  tool_calls_json TEXT NOT NULL DEFAULT '[]',
  est_in INTEGER NOT NULL DEFAULT 0,
  est_out INTEGER NOT NULL DEFAULT 0,
  parent_turn_id TEXT,
  checkpoint_sha TEXT
);
CREATE TABLE IF NOT EXISTS messages(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  ord INTEGER NOT NULL,
  role TEXT NOT NULL,
  content TEXT NOT NULL,
  reasoning TEXT NOT NULL DEFAULT '',
  tool_call_id TEXT,
  tool_calls_json TEXT NOT NULL DEFAULT '[]'
);
CREATE TABLE IF NOT EXISTS prefs(
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
/* B-7: журнал подтверждений — кто, что, когда одобрил или отклонил.
   Автономная таблица (не FK на turns): запись обязана пережить любой
   обрыв хода, а аудит важнее нормализации. */
CREATE TABLE IF NOT EXISTS approvals(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id TEXT NOT NULL,
  turn_id TEXT NOT NULL,
  call_id TEXT NOT NULL,
  tool TEXT NOT NULL,
  summary TEXT NOT NULL DEFAULT '',
  decision TEXT NOT NULL,
  actor TEXT NOT NULL DEFAULT 'user',
  decided_ms INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS tasks(
  id TEXT PRIMARY KEY,
  kind TEXT NOT NULL,
  payload TEXT NOT NULL DEFAULT '{}',
  state TEXT NOT NULL DEFAULT 'queued',
  attempts INTEGER NOT NULL DEFAULT 0,
  next_try_ms INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  every_ms INTEGER,
  created_ms INTEGER NOT NULL,
  updated_ms INTEGER NOT NULL
);
/* E-8: история телеметрии — точки (ts, имя, значение) минутного
   сэмплера и латентности провайдеров. Автономная таблица без FK:
   телеметрия обязана пережить удаление любой сессии. */
CREATE TABLE IF NOT EXISTS metrics(
  ts_ms INTEGER NOT NULL,
  name TEXT NOT NULL,
  value REAL NOT NULL
);
/* F-7: персистентная память. scope='global' — факты приложения (вшиваются
   в системный промпт каждого хода), scope='session' — заметки одной сессии,
   scope='folder' — факты рабочей папки (владелец в колонке dir).
   Автономная таблица без FK на sessions: память обязана пережить удаление
   сессии, иначе «забыть сессию» молча стирало бы факты владельца.
   Отдельного индекса на (scope, dir) нет сознательно: префикс scope в
   idx_memory_scope уже сужает выборку до одной области, а dir — остаточный
   фильтр по десяткам строк. Индекс стоил бы записи на каждый memory_add и
   зеркального DDL в sidecar ради ненаблюдаемого выигрыша. */
CREATE TABLE IF NOT EXISTS memory(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  scope TEXT NOT NULL DEFAULT 'global',
  text TEXT NOT NULL,
  created_ms INTEGER NOT NULL,
  session_id TEXT,
  dir TEXT
);
CREATE INDEX IF NOT EXISTS idx_memory_scope ON memory(scope, created_ms);
CREATE INDEX IF NOT EXISTS idx_turns_session ON turns(session_id, started_ms);
CREATE INDEX IF NOT EXISTS idx_messages_session ON messages(session_id, ord);
CREATE INDEX IF NOT EXISTS idx_approvals_session ON approvals(session_id, decided_ms);
CREATE INDEX IF NOT EXISTS idx_tasks_due ON tasks(state, next_try_ms);
CREATE INDEX IF NOT EXISTS idx_metrics_name_ts ON metrics(name, ts_ms);
/* F-10: полнотекстовый поиск по истории. Индекс — external-content FTS5
   поверх messages: текст не дублируется, а синхронность держат три триггера
   (insert/delete/update). Поэтому и save_session со своим «DELETE + INSERT
   целиком», и каскадное удаление сессии оставляют индекс правильным без
   ручного сопровождения в коде. unicode61 + remove_diacritics 2: кириллица
   и регистр работают из коробки: «прив» находит «привет», «ПРИВЕТ» находит
   «привет». Диакритика сворачивается только латинская (résumé → resume),
   а кириллическая «ё» остаётся отдельной буквой: «зеленые» НЕ находит
   «зелёные» — проверено пробой, и это осознанное ограничение (свой
   токенайзер ради одной буквы не стоит зависимости).
   DDL обязан байт-в-байт совпадать с sidecar/store-server.js — сверяет тест
   schema_matches_js_sidecar. */
CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
  content,
  reasoning,
  content='messages',
  content_rowid='id',
  tokenize='unicode61 remove_diacritics 2'
);
CREATE TRIGGER IF NOT EXISTS messages_fts_ai AFTER INSERT ON messages BEGIN
  INSERT INTO messages_fts(rowid, content, reasoning) VALUES (new.id, new.content, new.reasoning);
END;
CREATE TRIGGER IF NOT EXISTS messages_fts_ad AFTER DELETE ON messages BEGIN
  INSERT INTO messages_fts(messages_fts, rowid, content, reasoning) VALUES ('delete', old.id, old.content, old.reasoning);
END;
CREATE TRIGGER IF NOT EXISTS messages_fts_au AFTER UPDATE ON messages BEGIN
  INSERT INTO messages_fts(messages_fts, rowid, content, reasoning) VALUES ('delete', old.id, old.content, old.reasoning);
  INSERT INTO messages_fts(rowid, content, reasoning) VALUES (new.id, new.content, new.reasoning);
END;
"#;

/// Хранилище: синхронный контракт домена над персистентностью.
///
/// Ревизия 25: реализаций две — основной [`crate::node_store::NodeStore`]
/// (better-sqlite3 в Node-sidecar, как просил пользователь) и аварийный
/// [`SqliteStore`] (встроенный rusqlite, если Node нет). Выбор — через
/// фабрики [`open`]/[`open_memory`]. Send обязателен: состояние приложения
/// носит трейт-объект за Mutex между потоками.
/// Минимальный снимок сессии для задач, которым не нужно её содержимое.
///
/// Автоочистка решает «удалить или нет» по трём полям: id, рабочая папка и
/// время создания. [`Store::load_all`] для этого не годится — он через
/// `fill_session` поднимает ВЕСЬ текст ходов и сообщений каждой сессии, то есть
/// на живой базе (46 МБ, 71 сессия, 6337 сообщений — замер D-193) стартовая
/// уборка прочитала бы всю историю в память и съела бюджет старта 400 мс
/// (D-013). Это тот же принцип, что D-121 для сайдбара: тяжёлое содержимое
/// грузится лениво и только тем, кому оно правда нужно.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SessionStub {
    pub id: String,
    pub cwd: String,
    pub created_ms: u64,
}

pub trait Store: Send {
    fn create_session(&self, s: &Session) -> StoreResult<()>;
    fn set_session_title(&self, id: &str, title: &str) -> StoreResult<()>;
    /// B-3: свёртка старых ходов живёт рядом с сессией.
    fn set_session_summary(&self, id: &str, summary: &str) -> StoreResult<()>;
    fn delete_session(&self, id: &str) -> StoreResult<()>;
    /// Все сессии с историей и ходами, в порядке создания.
    fn load_all(&self) -> StoreResult<Vec<Session>>;
    /// Все сессии БЕЗ истории и ходов, в порядке создания — тот же порядок,
    /// что у [`Store::load_all`]. Один SELECT, никакой десериализации
    /// содержимого; для автоочистки и подобных списочных задач.
    fn session_stubs(&self) -> StoreResult<Vec<SessionStub>>;
    fn load_session(&self, id: &str) -> StoreResult<Option<Session>>;
    /// Ход плюс снимок истории пишутся одной транзакцией.
    fn save_turn(
        &self,
        session_id: &str,
        turn: &TurnRecord,
        history: &[ChatMessage],
    ) -> StoreResult<()>;
    fn get_pref(&self, key: &str) -> StoreResult<Option<String>>;
    fn set_pref(&self, key: &str, value: &str) -> StoreResult<()>;
    /// B-7: per-session политика подтверждений (None = глобальная).
    fn set_session_policy(&self, id: &str, policy: Option<&str>) -> StoreResult<()>;
    /// B-7: журнал подтверждений.
    fn log_approval(&self, e: &ApprovalEntry) -> StoreResult<()>;
    fn approval_log(
        &self,
        session_id: Option<&str>,
        limit: usize,
    ) -> StoreResult<Vec<ApprovalEntry>>;
    /// F-7: добавить строку памяти, вернуть её id.
    ///
    /// Текст и область проверяются до записи ([`memory_check_text`],
    /// [`memory_check_scope`]): пустая строка или «память» неизвестной
    /// области — ошибка, а не молчаливое сохранение мусора.
    ///
    /// rev56: `dir` — владелец строки области folder (рабочая папка). Владелец
    /// обязан соответствовать области ([`memory_check_owner`]): global — оба
    /// None, session — только `session_id`, folder — только `dir`. Два соседних
    /// `Option<&str>` перепутать местами легко, но перестановка не проходит
    /// молча — проверка возвращает «не может одновременно принадлежать», то
    /// есть ошибка видна в первом же тесте, а не в чужой сессии через месяц.
    fn memory_add(
        &self,
        scope: &str,
        session_id: Option<&str>,
        dir: Option<&str>,
        text: &str,
    ) -> StoreResult<i64>;
    /// F-7: строки области (новые первыми). `session_id` фильтрует заметки
    /// конкретной сессии, `dir` — факты конкретной рабочей папки; для
    /// глобальных строк передают None в обоих.
    fn memory_list(
        &self,
        scope: &str,
        session_id: Option<&str>,
        dir: Option<&str>,
        limit: usize,
    ) -> StoreResult<Vec<MemoryEntry>>;
    /// F-7: забыть строку; true, если она существовала.
    fn memory_forget(&self, id: i64) -> StoreResult<bool>;
    /// E-5: очередь фоновых задач. id — ключ дедупликации (INSERT OR
    /// IGNORE): сид периодической задачи идемпотентен, отмена не
    /// воскрешается повторным сидом.
    fn task_enqueue(&self, t: &crate::tasks::Task) -> StoreResult<()>;
    /// E-5: забрать задачи с наступившим сроком (queued, next_try_ms <=
    /// now): перевод в running со счётчиком попыток и возврат списка.
    fn task_claim_due(&self, now_ms: u64) -> StoreResult<Vec<crate::tasks::Task>>;
    fn task_get(&self, id: &str) -> StoreResult<Option<crate::tasks::Task>>;
    /// E-5: исход задачи — состояние, сообщение журнала (last_error),
    /// попытки и следующий срок. updated_ms ставит база (strftime).
    fn task_update(
        &self,
        id: &str,
        state: &str,
        message: &str,
        attempts: u32,
        next_try_ms: u64,
    ) -> StoreResult<()>;
    fn tasks_list(&self, limit: usize) -> StoreResult<Vec<crate::tasks::Task>>;
    /// E-5: сироты после краша — вернуть running в очередь.
    fn tasks_recover_running(&self) -> StoreResult<()>;
    /// E-5: гигиена — удалить старые done.
    fn tasks_prune(&self, before_ms: u64) -> StoreResult<()>;
    /// E-8: точка телеметрии (сэмплер раз в минуту, латентность
    /// провайдеров). Один INSERT — телеметрия не должна быть дорогой.
    fn metrics_insert(&self, ts_ms: u64, name: &str, value: f64) -> StoreResult<()>;
    /// E-8: выборка по имени с ts_ms, старее-к-новее, с пределом.
    fn metrics_query(
        &self,
        name: &str,
        since_ms: u64,
        limit: usize,
    ) -> StoreResult<Vec<(u64, f64)>>;
    /// E-8: гигиена — удалить точки старше срока.
    fn metrics_prune(&self, before_ms: u64) -> StoreResult<()>;
    /// E-8: токены/день — агрегация оценочных токенов ходов, день UTC:
    /// (day_ms, tokens, turns). Ходы с нулевой оценкой не попадают.
    fn tokens_by_day(&self, since_ms: u64) -> StoreResult<Vec<(u64, u64, u64)>>;
    /// F-10: полнотекстовый поиск по истории всех сессий (FTS5 поверх
    /// `messages`), выдача по убыванию качества совпадения.
    ///
    /// Запрос приводится к безопасному выражению MATCH общим для обеих
    /// реализаций [`fts_match_expr`]: пустой запрос или запрос без единого
    /// слова — ошибка, а не «нашлось всё». `limit` зажимается в
    /// 1..=[`SEARCH_MAX_LIMIT`].
    fn search_messages(&self, query: &str, limit: usize) -> StoreResult<Vec<MessageHit>>;
    /// Обновление (rev37): остановить процесс бэкенда перед установкой
    /// новой версии — NSIS не перезапишет работающий node.exe. По
    /// умолчанию нет ничего: бэкендам без внешнего процесса (rusqlite)
    /// останавливать нечего.
    fn shutdown(&mut self) {}
}

/// SQLite-реализация контракта [`Store`].
pub struct SqliteStore {
    conn: Connection,
}

impl SqliteStore {
    /// Открыть (и создать) базу по пути, подняв схему и режимы.
    pub fn open(path: &Path) -> StoreResult<Self> {
        if let Some(dir) = path.parent() {
            if !dir.as_os_str().is_empty() {
                std::fs::create_dir_all(dir)?;
            }
        }
        let conn = Connection::open(path)?;
        conn.execute_batch(SCHEMA)?;
        // Миграция B-3: колонка свёртки для баз, созданных до ревизии 18.
        // «duplicate column» — нормальный повторный запуск, игнорируем.
        let _ = conn.execute(
            "ALTER TABLE sessions ADD COLUMN summary TEXT NOT NULL DEFAULT ''",
            [],
        );
        // Миграция B-7: per-session политика подтверждений.
        let _ = conn.execute("ALTER TABLE sessions ADD COLUMN approval_policy TEXT", []);
        // Миграция E-6: дерево суб-агентов — родительский ход.
        let _ = conn.execute("ALTER TABLE turns ADD COLUMN parent_turn_id TEXT", []);
        // Миграция F-5: git-чекпоинт хода (снимок worktree до хода).
        let _ = conn.execute("ALTER TABLE turns ADD COLUMN checkpoint_sha TEXT", []);
        /* Миграция rev56: область folder — владелец строки в колонке dir. Для
        баз, созданных до неё, колонку добавляет этот ALTER; для новых она уже в
        CREATE TABLE, и тогда ALTER отвечает «duplicate column» — тот же
        нормальный повторный запуск, что у B-3 выше. Колонка nullable без
        DEFAULT: у global и session владельца-папки нет, и NOT NULL потребовал бы
        значения-заглушки, которое пришлось бы отличать от настоящего пути. */
        let _ = conn.execute("ALTER TABLE memory ADD COLUMN dir TEXT", []);
        // F-10: индекс FTS5 создан вместе со схемой, но история в этой базе
        // могла появиться раньше него.
        Self::fts_backfill(&conn)?;
        Ok(Self { conn })
    }

    /// In-memory база для тестов: та же схема, те же режимы.
    pub fn in_memory() -> StoreResult<Self> {
        let conn = Connection::open_in_memory()?;
        conn.execute_batch(SCHEMA)?;
        Self::fts_backfill(&conn)?;
        Ok(Self { conn })
    }

    /// F-10: разовая пересборка FTS-индекса из `messages` — только если индекс
    /// пуст, а сообщения есть (база старше FTS5 или оборвавшийся rebuild).
    /// Условие проверяется двумя LIMIT 1, а не COUNT(*): на каждом запуске
    /// считать строки индекса значит платить за поиск в бюджете старта
    /// (400 мс, D-013).
    fn fts_backfill(conn: &Connection) -> StoreResult<()> {
        let needed: bool = conn.query_row(FTS_NEEDS_BACKFILL_SQL, [], |r| r.get(0))?;
        if needed {
            conn.execute_batch(FTS_REBUILD_SQL)?;
        }
        Ok(())
    }

    /// Целостность базы: используется краш-тестами после обрыва процесса.
    pub fn integrity_check(&self) -> StoreResult<String> {
        let ok: String = self
            .conn
            .query_row("PRAGMA integrity_check", [], |r| r.get(0))?;
        Ok(ok)
    }
}

pub(crate) fn role_to_str(role: Role) -> &'static str {
    match role {
        Role::System => "system",
        Role::User => "user",
        Role::Assistant => "assistant",
        Role::Tool => "tool",
    }
}

pub(crate) fn role_from_str(s: &str) -> Role {
    match s {
        "system" => Role::System,
        "user" => Role::User,
        "assistant" => Role::Assistant,
        _ => Role::Tool,
    }
}

/* B-7: политика сессии хранится строкой — теми же значениями, что serde
пишет в wire-формат (snake_case), чтобы журнал и конфиг читались одинаково. */
pub(crate) fn policy_to_str(p: crate::turn::ApprovalPolicy) -> &'static str {
    match p {
        crate::turn::ApprovalPolicy::Always => "always",
        crate::turn::ApprovalPolicy::Never => "never",
        crate::turn::ApprovalPolicy::OnDangerous => "on_dangerous",
    }
}

pub(crate) fn policy_from_str_opt(s: Option<String>) -> Option<crate::turn::ApprovalPolicy> {
    match s.as_deref() {
        Some("always") => Some(crate::turn::ApprovalPolicy::Always),
        Some("never") => Some(crate::turn::ApprovalPolicy::Never),
        Some("on_dangerous") => Some(crate::turn::ApprovalPolicy::OnDangerous),
        _ => None,
    }
}

/* Ревизия 25: выбор хранилища. better-sqlite3 через Node-sidecar —
основное хранилище (прямое требование пользователя); встроенный
rusqlite — аварийный фолбэк для окружений без Node. SWAGCOD_STORE
форсирует выбор: `node` — только sidecar (падение громкое, без
тихой подмены), `sqlite` — только встроенный, иначе auto. */

/// Открыть файловую базу: sidecar, при неудаче (в auto) — rusqlite.
pub fn open(path: &Path) -> StoreResult<Box<dyn Store>> {
    let mode = std::env::var("SWAGCOD_STORE").unwrap_or_else(|_| "auto".to_string());
    if mode != "sqlite" {
        match crate::node_store::NodeStore::open(path) {
            Ok(s) => return Ok(Box::new(s)),
            Err(e) => {
                if mode == "node" {
                    return Err(e);
                }
                eprintln!(
                    "store: better-sqlite3 sidecar недоступен, фолбэк на встроенный SQLite: {e}"
                );
            }
        }
    }
    Ok(Box::new(SqliteStore::open(path)?))
}

/// In-memory база: sidecar для тестов, rusqlite если Node не завёлся.
pub fn open_memory() -> Box<dyn Store> {
    let mode = std::env::var("SWAGCOD_STORE").unwrap_or_else(|_| "auto".to_string());
    if mode != "sqlite" {
        match crate::node_store::NodeStore::in_memory() {
            Ok(s) => return Box::new(s),
            Err(e) => {
                if mode == "node" {
                    panic!("SWAGCOD_STORE=node, но sidecar не поднялся: {e}");
                }
                eprintln!("store: sidecar недоступен, in-memory на встроенном SQLite: {e}");
            }
        }
    }
    Box::new(SqliteStore::in_memory().expect("in-memory SQLite открывается"))
}

impl Store for SqliteStore {
    fn create_session(&self, s: &Session) -> StoreResult<()> {
        self.conn.execute(
            "INSERT OR IGNORE INTO sessions(id, cwd, model, title, approval_policy, created_ms) VALUES (?1, ?2, ?3, ?4, ?5, ?6)",
            params![
                s.id.as_str(),
                s.cwd,
                s.model,
                s.title,
                s.approval_policy.map(policy_to_str),
                s.created_ms as i64
            ],
        )?;
        Ok(())
    }

    fn set_session_title(&self, id: &str, title: &str) -> StoreResult<()> {
        self.conn.execute(
            "UPDATE sessions SET title = ?2 WHERE id = ?1",
            params![id, title],
        )?;
        Ok(())
    }

    fn set_session_summary(&self, id: &str, summary: &str) -> StoreResult<()> {
        self.conn.execute(
            "UPDATE sessions SET summary = ?2 WHERE id = ?1",
            params![id, summary],
        )?;
        Ok(())
    }

    fn delete_session(&self, id: &str) -> StoreResult<()> {
        // Каскад удаляет ходы и сообщения следом за сессией.
        self.conn
            .execute("DELETE FROM sessions WHERE id = ?1", params![id])?;
        Ok(())
    }

    fn load_all(&self) -> StoreResult<Vec<Session>> {
        /// Строка выборки сессий: (id, cwd, model, title, summary, policy, created_ms).
        type SessionRow = (String, String, String, String, String, Option<String>, u64);
        let mut stmt = self.conn.prepare(
            "SELECT id, cwd, model, title, summary, approval_policy, created_ms FROM sessions ORDER BY created_ms, id",
        )?;
        let ids: Vec<SessionRow> = stmt
            .query_map([], |r| {
                Ok((
                    r.get(0)?,
                    r.get(1)?,
                    r.get(2)?,
                    r.get(3)?,
                    r.get(4)?,
                    r.get(5)?,
                    r.get::<_, i64>(6)? as u64,
                ))
            })?
            .collect::<Result<_, _>>()?;
        let mut out = Vec::with_capacity(ids.len());
        for (id, cwd, model, title, summary, policy, created_ms) in ids {
            let mut ses = Session::new(SessionId::new(&id), cwd, model);
            ses.title = title;
            ses.summary = summary;
            ses.approval_policy = policy_from_str_opt(policy);
            ses.created_ms = created_ms;
            ses.status = SessionStatus::Idle;
            fill_session(&self.conn, &mut ses)?;
            out.push(ses);
        }
        Ok(out)
    }

    fn session_stubs(&self) -> StoreResult<Vec<SessionStub>> {
        let mut stmt = self
            .conn
            .prepare("SELECT id, cwd, created_ms FROM sessions ORDER BY created_ms, id")?;
        let rows = stmt
            .query_map([], |r| {
                Ok(SessionStub {
                    id: r.get(0)?,
                    cwd: r.get(1)?,
                    created_ms: r.get::<_, i64>(2)? as u64,
                })
            })?
            .collect::<Result<Vec<_>, _>>()?;
        Ok(rows)
    }

    fn load_session(&self, id: &str) -> StoreResult<Option<Session>> {
        let row = self.conn.query_row(
            "SELECT cwd, model, title, summary, approval_policy, created_ms FROM sessions WHERE id = ?1",
            params![id],
            |r| {
                Ok((
                    r.get::<_, String>(0)?,
                    r.get::<_, String>(1)?,
                    r.get::<_, String>(2)?,
                    r.get::<_, String>(3)?,
                    r.get::<_, Option<String>>(4)?,
                    r.get::<_, i64>(5)? as u64,
                ))
            },
        );
        let (cwd, model, title, summary, policy, created_ms) = match row {
            Ok(v) => v,
            Err(rusqlite::Error::QueryReturnedNoRows) => return Ok(None),
            Err(e) => return Err(e.into()),
        };
        let mut ses = Session::new(SessionId::new(id), cwd, model);
        ses.title = title;
        ses.summary = summary;
        ses.approval_policy = policy_from_str_opt(policy);
        ses.created_ms = created_ms;
        ses.status = SessionStatus::Idle;
        fill_session(&self.conn, &mut ses)?;
        Ok(Some(ses))
    }

    fn save_turn(
        &self,
        session_id: &str,
        turn: &TurnRecord,
        history: &[ChatMessage],
    ) -> StoreResult<()> {
        let tx = self.conn.unchecked_transaction()?;
        let tools_json = serde_json::to_string(&turn.tool_calls)?;
        tx.execute(
            "INSERT OR REPLACE INTO turns(id, session_id, started_ms, ended_ms, ok, failure, content, reasoning, tool_calls_json, est_in, est_out, parent_turn_id, checkpoint_sha)
             VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12, ?13)",
            params![
                turn.id.as_str(),
                session_id,
                turn.started_ms as i64,
                turn.ended_ms.map(|v| v as i64),
                turn.ok,
                turn.failure,
                turn.content,
                turn.reasoning,
                tools_json,
                turn.est_input_tokens as i64,
                turn.est_output_tokens as i64,
                turn.parent_turn_id.as_ref().map(|p| p.as_str()),
                turn.checkpoint_sha,
            ],
        )?;
        // Снимок истории целиком: порядок и содержимое восстанавливаются
        // байт-в-байт, без учёта инкрементальных хвостов.
        tx.execute(
            "DELETE FROM messages WHERE session_id = ?1",
            params![session_id],
        )?;
        {
            let mut stmt = tx.prepare(
                "INSERT INTO messages(session_id, ord, role, content, reasoning, tool_call_id, tool_calls_json)
                 VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7)",
            )?;
            for (ord, m) in history.iter().enumerate() {
                let tools = serde_json::to_string(&m.tool_calls)?;
                stmt.execute(params![
                    session_id,
                    ord as i64,
                    role_to_str(m.role),
                    m.content,
                    m.reasoning,
                    m.tool_call_id,
                    tools,
                ])?;
            }
        }
        tx.commit()?;
        Ok(())
    }

    fn get_pref(&self, key: &str) -> StoreResult<Option<String>> {
        let row = self.conn.query_row(
            "SELECT value FROM prefs WHERE key = ?1",
            params![key],
            |r| r.get::<_, String>(0),
        );
        match row {
            Ok(v) => Ok(Some(v)),
            Err(rusqlite::Error::QueryReturnedNoRows) => Ok(None),
            Err(e) => Err(e.into()),
        }
    }

    fn set_pref(&self, key: &str, value: &str) -> StoreResult<()> {
        self.conn.execute(
            "INSERT INTO prefs(key, value) VALUES (?1, ?2) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
            params![key, value],
        )?;
        Ok(())
    }

    fn set_session_policy(&self, id: &str, policy: Option<&str>) -> StoreResult<()> {
        self.conn.execute(
            "UPDATE sessions SET approval_policy = ?2 WHERE id = ?1",
            params![id, policy],
        )?;
        Ok(())
    }

    fn log_approval(&self, e: &ApprovalEntry) -> StoreResult<()> {
        self.conn.execute(
            "INSERT INTO approvals(session_id, turn_id, call_id, tool, summary, decision, actor, decided_ms)
             VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8)",
            params![
                e.session_id,
                e.turn_id,
                e.call_id,
                e.tool,
                e.summary,
                e.decision,
                e.actor,
                e.decided_ms as i64
            ],
        )?;
        Ok(())
    }

    fn approval_log(
        &self,
        session_id: Option<&str>,
        limit: usize,
    ) -> StoreResult<Vec<ApprovalEntry>> {
        let limit = limit.clamp(1, 5000) as i64;
        // Два запроса намеренно: у варианта без фильтра LIMIT — ?1,
        // иначе rusqlite считает дырку в нумерации недостающим параметром.
        let map_row = |r: &rusqlite::Row<'_>| -> rusqlite::Result<ApprovalEntry> {
            Ok(ApprovalEntry {
                session_id: r.get(0)?,
                turn_id: r.get(1)?,
                call_id: r.get(2)?,
                tool: r.get(3)?,
                summary: r.get(4)?,
                decision: r.get(5)?,
                actor: r.get(6)?,
                decided_ms: r.get::<_, i64>(7)? as u64,
            })
        };
        let select = "SELECT session_id, turn_id, call_id, tool, summary, decision, actor, decided_ms FROM approvals";
        let rows = match session_id {
            Some(sid) => {
                let sql = format!(
                    "{select} WHERE session_id = ?1 ORDER BY decided_ms DESC, id DESC LIMIT ?2"
                );
                let mut stmt = self.conn.prepare(&sql)?;
                let mapped = stmt.query_map(params![sid, limit], map_row)?;
                mapped.collect::<Result<Vec<_>, _>>()?
            }
            None => {
                let sql = format!("{select} ORDER BY decided_ms DESC, id DESC LIMIT ?1");
                let mut stmt = self.conn.prepare(&sql)?;
                let mapped = stmt.query_map(params![limit], map_row)?;
                mapped.collect::<Result<Vec<_>, _>>()?
            }
        };
        Ok(rows)
    }

    fn memory_add(
        &self,
        scope: &str,
        session_id: Option<&str>,
        dir: Option<&str>,
        text: &str,
    ) -> StoreResult<i64> {
        let scope = memory_check_scope(scope)?;
        let (sid, d) = memory_check_owner(&scope, session_id, dir)?;
        let text = memory_check_text(text)?;
        let now = crate::bus::now_ms() as i64;
        self.conn.execute(
            "INSERT INTO memory(scope, text, created_ms, session_id, dir) \
             VALUES (?1, ?2, ?3, ?4, ?5)",
            params![scope, text, now, sid, d],
        )?;
        Ok(self.conn.last_insert_rowid())
    }

    fn memory_list(
        &self,
        scope: &str,
        session_id: Option<&str>,
        dir: Option<&str>,
        limit: usize,
    ) -> StoreResult<Vec<MemoryEntry>> {
        let scope = memory_check_scope(scope)?;
        let (sid, d) = memory_check_owner(&scope, session_id, dir)?;
        let limit = limit.clamp(1, 1000) as i64;
        let map_row = |r: &rusqlite::Row<'_>| -> rusqlite::Result<MemoryEntry> {
            Ok(MemoryEntry {
                id: r.get(0)?,
                scope: r.get(1)?,
                text: r.get(2)?,
                created_ms: r.get::<_, i64>(3)? as u64,
                session_id: r.get(4)?,
                dir: r.get(5)?,
            })
        };
        /* Новые первыми: память читается как лента, и свежие факты важнее —
        при переполнении потолка контекста обрезается хвост, то есть
        старьё. */
        let select =
            "SELECT id, scope, text, created_ms, session_id, dir FROM memory WHERE scope = ?1";
        /* `memory_check_owner` гарантирует, что владелец ровно один или его
        нет, поэтому фильтр — одна колонка. Имя колонки — литерал из этого
        match, а не входные данные, так что склеивать SQL здесь безопасно. */
        let owner = match (sid.as_deref(), d.as_deref()) {
            (Some(sid), _) => Some(("session_id", sid)),
            (None, Some(d)) => Some(("dir", d)),
            (None, None) => None,
        };
        let rows = match owner {
            Some((col, val)) => {
                let sql =
                    format!("{select} AND {col} = ?2 ORDER BY created_ms DESC, id DESC LIMIT ?3");
                let mut stmt = self.conn.prepare(&sql)?;
                let mapped = stmt.query_map(params![scope, val, limit], map_row)?;
                mapped.collect::<Result<Vec<_>, _>>()?
            }
            None => {
                let sql = format!("{select} ORDER BY created_ms DESC, id DESC LIMIT ?2");
                let mut stmt = self.conn.prepare(&sql)?;
                let mapped = stmt.query_map(params![scope, limit], map_row)?;
                mapped.collect::<Result<Vec<_>, _>>()?
            }
        };
        Ok(rows)
    }

    fn memory_forget(&self, id: i64) -> StoreResult<bool> {
        let n = self
            .conn
            .execute("DELETE FROM memory WHERE id = ?1", params![id])?;
        Ok(n > 0)
    }

    fn task_enqueue(&self, t: &crate::tasks::Task) -> StoreResult<()> {
        self.conn.execute(
            "INSERT OR IGNORE INTO tasks(id, kind, payload, state, attempts, next_try_ms, last_error, every_ms, created_ms, updated_ms)
             VALUES (?1, ?2, ?3, 'queued', 0, ?4, '', ?5, ?6, ?6)",
            params![
                t.id,
                t.kind,
                t.payload.to_string(),
                t.next_try_ms as i64,
                t.every_ms.map(|v| v as i64),
                t.created_ms as i64
            ],
        )?;
        Ok(())
    }

    fn task_claim_due(&self, now_ms: u64) -> StoreResult<Vec<crate::tasks::Task>> {
        let ids: Vec<String> = {
            let mut stmt = self.conn.prepare(
                "SELECT id FROM tasks WHERE state = 'queued' AND next_try_ms <= ?1 ORDER BY next_try_ms, id LIMIT 8",
            )?;
            let mapped = stmt.query_map(params![now_ms as i64], |r| r.get::<_, String>(0))?;
            mapped.collect::<Result<Vec<_>, _>>()?
        };
        let mut out = Vec::new();
        for id in ids {
            // Guard state='queued': единственный воркер, но защита от
            // двойного claim дешевле расследования.
            self.conn.execute(
                "UPDATE tasks SET state = 'running', attempts = attempts + 1,
                 updated_ms = CAST(strftime('%s','now') AS INTEGER) * 1000
                 WHERE id = ?1 AND state = 'queued'",
                params![id],
            )?;
            if let Some(t) = self.task_get(&id)? {
                if t.state == crate::tasks::TASK_RUNNING {
                    out.push(t);
                }
            }
        }
        Ok(out)
    }

    fn task_get(&self, id: &str) -> StoreResult<Option<crate::tasks::Task>> {
        let sql = format!("SELECT {TASK_COLS} FROM tasks WHERE id = ?1");
        let mut stmt = self.conn.prepare(&sql)?;
        let mut mapped = stmt.query_map(params![id], task_from_row)?;
        Ok(mapped.next().transpose()?)
    }

    fn task_update(
        &self,
        id: &str,
        state: &str,
        message: &str,
        attempts: u32,
        next_try_ms: u64,
    ) -> StoreResult<()> {
        self.conn.execute(
            "UPDATE tasks SET state = ?2, last_error = ?3, attempts = ?4, next_try_ms = ?5,
             updated_ms = CAST(strftime('%s','now') AS INTEGER) * 1000
             WHERE id = ?1",
            params![id, state, message, attempts as i64, next_try_ms as i64],
        )?;
        Ok(())
    }

    fn tasks_list(&self, limit: usize) -> StoreResult<Vec<crate::tasks::Task>> {
        let limit = limit.clamp(1, 1000) as i64;
        let sql =
            format!("SELECT {TASK_COLS} FROM tasks ORDER BY created_ms DESC, id DESC LIMIT ?1");
        let mut stmt = self.conn.prepare(&sql)?;
        let mapped = stmt.query_map(params![limit], task_from_row)?;
        Ok(mapped.collect::<Result<Vec<_>, _>>()?)
    }

    fn tasks_recover_running(&self) -> StoreResult<()> {
        self.conn.execute(
            "UPDATE tasks SET state = 'queued', next_try_ms = 0,
             updated_ms = CAST(strftime('%s','now') AS INTEGER) * 1000
             WHERE state = 'running'",
            [],
        )?;
        Ok(())
    }

    fn tasks_prune(&self, before_ms: u64) -> StoreResult<()> {
        // u64::MAX as i64 дал бы -1: зажимаем до i64::MAX («удалить всё»).
        let before = before_ms.min(i64::MAX as u64) as i64;
        self.conn.execute(
            "DELETE FROM tasks WHERE state = 'done' AND updated_ms < ?1",
            params![before],
        )?;
        Ok(())
    }

    fn metrics_insert(&self, ts_ms: u64, name: &str, value: f64) -> StoreResult<()> {
        self.conn.execute(
            "INSERT INTO metrics(ts_ms, name, value) VALUES(?1, ?2, ?3)",
            params![ts_ms.min(i64::MAX as u64) as i64, name, value],
        )?;
        Ok(())
    }

    fn metrics_query(
        &self,
        name: &str,
        since_ms: u64,
        limit: usize,
    ) -> StoreResult<Vec<(u64, f64)>> {
        let limit = limit.clamp(1, 10_000) as i64;
        let mut stmt = self.conn.prepare(
            "SELECT ts_ms, value FROM metrics WHERE name = ?1 AND ts_ms >= ?2 ORDER BY ts_ms LIMIT ?3",
        )?;
        let mapped = stmt.query_map(
            params![name, since_ms.min(i64::MAX as u64) as i64, limit],
            |r| Ok((r.get::<_, i64>(0)?.max(0) as u64, r.get::<_, f64>(1)?)),
        )?;
        Ok(mapped.collect::<Result<Vec<_>, _>>()?)
    }

    fn metrics_prune(&self, before_ms: u64) -> StoreResult<()> {
        let before = before_ms.min(i64::MAX as u64) as i64;
        self.conn
            .execute("DELETE FROM metrics WHERE ts_ms < ?1", params![before])?;
        Ok(())
    }

    fn tokens_by_day(&self, since_ms: u64) -> StoreResult<Vec<(u64, u64, u64)>> {
        let mut stmt = self.conn.prepare(
            "SELECT (started_ms / 86400000) * 86400000 AS day,
                    SUM(est_in + est_out) AS tokens, COUNT(*) AS turns
             FROM turns WHERE started_ms >= ?1 AND (est_in + est_out) > 0
             GROUP BY day ORDER BY day",
        )?;
        let mapped = stmt.query_map(params![since_ms.min(i64::MAX as u64) as i64], |r| {
            Ok((
                r.get::<_, i64>(0)?.max(0) as u64,
                r.get::<_, i64>(1)?.max(0) as u64,
                r.get::<_, i64>(2)?.max(0) as u64,
            ))
        })?;
        Ok(mapped.collect::<Result<Vec<_>, _>>()?)
    }

    fn search_messages(&self, query: &str, limit: usize) -> StoreResult<Vec<MessageHit>> {
        let expr = fts_match_expr(query)?;
        let lim = search_limit(limit) as i64;
        let mut stmt = self.conn.prepare(SEARCH_MESSAGES_SQL)?;
        let rows = stmt.query_map(params![expr, lim], |r| {
            Ok(MessageHit {
                session_id: r.get(0)?,
                session_title: r.get(1)?,
                ord: r.get::<_, i64>(2)?.max(0) as u32,
                role: r.get(3)?,
                snippet: r.get(4)?,
                rank: r.get(5)?,
            })
        })?;
        Ok(rows.collect::<Result<Vec<_>, _>>()?)
    }
}

/// E-5: колонки tasks — порядок общий для rusqlite и NodeStore.
pub(crate) const TASK_COLS: &str =
    "id, kind, payload, state, attempts, next_try_ms, last_error, every_ms, created_ms, updated_ms";

fn task_from_row(r: &rusqlite::Row<'_>) -> rusqlite::Result<crate::tasks::Task> {
    let payload: String = r.get(2)?;
    let every: Option<i64> = r.get(7)?;
    Ok(crate::tasks::Task {
        id: r.get(0)?,
        kind: r.get(1)?,
        payload: serde_json::from_str(&payload).unwrap_or_else(|_| serde_json::json!({})),
        state: r.get(3)?,
        attempts: r.get::<_, i64>(4)?.max(0) as u32,
        next_try_ms: r.get::<_, i64>(5)?.max(0) as u64,
        last_error: r.get(6)?,
        every_ms: every.map(|v| v.max(0) as u64),
        created_ms: r.get::<_, i64>(8)?.max(0) as u64,
        updated_ms: r.get::<_, i64>(9)?.max(0) as u64,
    })
}

/// Ходы и сообщения сессии из базы в живую структуру.
fn fill_session(conn: &Connection, ses: &mut Session) -> StoreResult<()> {
    let mut tstmt = conn.prepare(
        "SELECT id, started_ms, ended_ms, ok, failure, content, reasoning, tool_calls_json, est_in, est_out, parent_turn_id, checkpoint_sha
         FROM turns WHERE session_id = ?1 ORDER BY started_ms, id",
    )?;
    let turns = tstmt
        .query_map(params![ses.id.as_str()], |r| {
            let tools_json: String = r.get(7)?;
            Ok(TurnRecord {
                id: crate::session::TurnId::new(r.get::<_, String>(0)?),
                started_ms: r.get::<_, i64>(1)? as u64,
                ended_ms: r.get::<_, Option<i64>>(2)?.map(|v| v as u64),
                ok: r.get(3)?,
                failure: r.get(4)?,
                content: r.get(5)?,
                reasoning: r.get(6)?,
                tool_calls: serde_json::from_str(&tools_json).unwrap_or_default(),
                est_input_tokens: r.get::<_, i64>(8)? as u32,
                est_output_tokens: r.get::<_, i64>(9)? as u32,
                parent_turn_id: r
                    .get::<_, Option<String>>(10)?
                    .map(crate::session::TurnId::new),
                checkpoint_sha: r.get::<_, Option<String>>(11)?,
            })
        })?
        .collect::<Result<Vec<_>, _>>()?;
    ses.turns = turns;

    let mut mstmt = conn.prepare(
        "SELECT role, content, reasoning, tool_call_id, tool_calls_json
         FROM messages WHERE session_id = ?1 ORDER BY ord, id",
    )?;
    let history = mstmt
        .query_map(params![ses.id.as_str()], |r| {
            Ok((
                r.get::<_, String>(0)?,
                r.get::<_, String>(1)?,
                r.get::<_, String>(2)?,
                r.get::<_, Option<String>>(3)?,
                r.get::<_, String>(4)?,
            ))
        })?
        .map(|row| {
            let (role, content, reasoning, tool_call_id, tools_json) = row?;
            let tool_calls: Vec<ToolCall> = serde_json::from_str(&tools_json)?;
            Ok::<_, StoreError>(ChatMessage {
                role: role_from_str(&role),
                content,
                reasoning,
                tool_calls,
                tool_call_id,
            })
        })
        .collect::<Result<Vec<_>, _>>()?;
    ses.history = history;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::session::TurnId;

    fn sample_session() -> Session {
        let mut ses = Session::new(SessionId::new("s-1"), "D:/proj", "test-model");
        ses.title = "проба".to_string();
        ses.push_user_message("привет");
        ses.push_assistant_message(ChatMessage {
            role: Role::Assistant,
            content: "здравствуй".to_string(),
            reasoning: "думаю".to_string(),
            tool_calls: vec![ToolCall {
                id: "c-1".to_string(),
                name: "read".to_string(),
                arguments: serde_json::Map::new(),
            }],
            tool_call_id: None,
        });
        ses.push_tool_result("c-1", "содержимое файла");
        ses
    }

    fn sample_turn(n: u64) -> TurnRecord {
        TurnRecord {
            id: TurnId::new(format!("t-{n}")),
            started_ms: 1000 + n,
            ended_ms: Some(2000 + n),
            content: format!("ответ {n}"),
            reasoning: format!("мысли {n}"),
            tool_calls: vec![],
            est_input_tokens: 10,
            est_output_tokens: 20,
            ok: true,
            failure: None,
            parent_turn_id: None,
            checkpoint_sha: None,
        }
    }

    #[test]
    fn roundtrip_history_is_byte_equal() {
        let store = SqliteStore::in_memory().unwrap();
        let ses = sample_session();
        store.create_session(&ses).unwrap();
        let turn = sample_turn(1);
        store
            .save_turn(ses.id.as_str(), &turn, &ses.history)
            .unwrap();

        let loaded = store.load_session("s-1").unwrap().expect("сессия есть");
        assert_eq!(
            serde_json::to_string(&loaded.history).unwrap(),
            serde_json::to_string(&ses.history).unwrap()
        );
        assert_eq!(loaded.turns.len(), 1);
        assert_eq!(loaded.turns[0], turn);
        assert_eq!(loaded.title, "проба");
        assert_eq!(loaded.cwd, "D:/proj");
    }

    #[test]
    fn turn_parent_roundtrip_sqlite() {
        // E-6: дерево ходов — parent_turn_id живёт в базе и читается назад.
        let store = SqliteStore::in_memory().unwrap();
        let ses = sample_session();
        store.create_session(&ses).unwrap();
        store.save_turn("s-1", &sample_turn(1), &[]).unwrap();
        let mut child = sample_turn(2);
        child.id = TurnId::new("t-1-sub-xyz");
        child.parent_turn_id = Some(TurnId::new("t-1"));
        store.save_turn("s-1", &child, &[]).unwrap();

        let loaded = store.load_session("s-1").unwrap().unwrap();
        assert_eq!(loaded.turns.len(), 2);
        assert_eq!(loaded.turns[0].parent_turn_id, None);
        assert_eq!(loaded.turns[1], child);
        assert_eq!(
            loaded.turns[1].parent_turn_id.as_ref().map(|p| p.as_str()),
            Some("t-1")
        );
    }

    #[test]
    fn turn_checkpoint_sha_roundtrip_and_legacy_migration() {
        // F-5: снимок хода живёт в базе и читается назад.
        let store = SqliteStore::in_memory().unwrap();
        let ses = sample_session();
        store.create_session(&ses).unwrap();
        let mut turn = sample_turn(1);
        let sha = "a".repeat(40);
        turn.checkpoint_sha = Some(sha.clone());
        store.save_turn("s-1", &turn, &[]).unwrap();
        store.save_turn("s-1", &sample_turn(2), &[]).unwrap();

        let loaded = store.load_session("s-1").unwrap().unwrap();
        assert_eq!(loaded.turns.len(), 2);
        assert_eq!(
            loaded.turns[0].checkpoint_sha.as_deref(),
            Some(sha.as_str())
        );
        assert_eq!(
            loaded.turns[1].checkpoint_sha, None,
            "ход без снимка — NULL, а не пустая строка"
        );

        /* База, созданная до F-5 (в turns нет ни checkpoint_sha, ни
        parent_turn_id), открывается прежним кодом: SCHEMA не пересоздаёт
        существующую таблицу, а ALTER добирает колонки. Без этого обновления
        на машине владельца означали бы «журнал не читается». */
        const LEGACY_TURNS: &str = "CREATE TABLE turns(
          id TEXT PRIMARY KEY,
          session_id TEXT NOT NULL,
          started_ms INTEGER NOT NULL,
          ended_ms INTEGER,
          ok INTEGER NOT NULL,
          failure TEXT,
          content TEXT NOT NULL,
          reasoning TEXT NOT NULL,
          tool_calls_json TEXT NOT NULL DEFAULT '[]',
          est_in INTEGER NOT NULL DEFAULT 0,
          est_out INTEGER NOT NULL DEFAULT 0
        );";
        let dir = std::env::temp_dir().join(format!("swagcod-legacy-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("legacy.db");
        let _ = std::fs::remove_file(&path);
        {
            let conn = Connection::open(&path).unwrap();
            conn.execute_batch(LEGACY_TURNS).unwrap();
        }
        let opened = SqliteStore::open(&path).unwrap();
        assert_eq!(opened.integrity_check().unwrap(), "ok");
        let cols: Vec<String> = {
            let mut stmt = opened.conn.prepare("PRAGMA table_info(turns)").unwrap();
            stmt.query_map([], |r| r.get::<_, String>(1))
                .unwrap()
                .filter_map(|c| c.ok())
                .collect()
        };
        assert!(
            cols.iter().any(|c| c == "checkpoint_sha"),
            "миграция не добавила checkpoint_sha: {cols:?}"
        );
        assert!(
            cols.iter().any(|c| c == "parent_turn_id"),
            "миграция не добавила parent_turn_id: {cols:?}"
        );
        std::fs::remove_dir_all(&dir).ok();
    }

    #[test]
    fn metrics_roundtrip_and_prune() {
        // E-8: точки телеметрии — вставка, выборка по имени/периоду, prune.
        let store = SqliteStore::in_memory().unwrap();
        store.metrics_insert(1000, "lagging_events", 3.0).unwrap();
        store.metrics_insert(60_000, "lagging_events", 5.0).unwrap();
        store.metrics_insert(60_000, "sessions", 2.0).unwrap();
        let q = store.metrics_query("lagging_events", 0, 100).unwrap();
        assert_eq!(q, vec![(1000, 3.0), (60_000, 5.0)]);
        let q = store.metrics_query("lagging_events", 59_000, 100).unwrap();
        assert_eq!(q, vec![(60_000, 5.0)]);
        // limit честно режет выборку.
        let q = store.metrics_query("lagging_events", 0, 1).unwrap();
        assert_eq!(q, vec![(1000, 3.0)]);
        // prune удаляет старое и не трогает другие имена.
        store.metrics_prune(59_000).unwrap();
        assert_eq!(
            store.metrics_query("lagging_events", 0, 100).unwrap(),
            vec![(60_000, 5.0)]
        );
        assert_eq!(
            store.metrics_query("sessions", 0, 100).unwrap(),
            vec![(60_000, 2.0)]
        );
    }

    #[test]
    fn tokens_by_day_aggregates_utc_days() {
        // E-8: токены/день из существующих колонок turns (est_in/est_out).
        let store = SqliteStore::in_memory().unwrap();
        let ses = sample_session();
        store.create_session(&ses).unwrap();
        // День 0 (UTC): два хода по 30 оценочных токенов.
        store.save_turn("s-1", &sample_turn(1), &[]).unwrap();
        let mut t2 = sample_turn(2);
        t2.started_ms = 5000;
        store.save_turn("s-1", &t2, &[]).unwrap();
        // День 1 UTC: ход с токенами и «нулевой» ход (не попадает).
        let mut t3 = sample_turn(3);
        t3.started_ms = 86_400_000 + 7000;
        store.save_turn("s-1", &t3, &[]).unwrap();
        let mut t4 = sample_turn(4);
        t4.started_ms = 86_400_000 + 8000;
        t4.est_input_tokens = 0;
        t4.est_output_tokens = 0;
        store.save_turn("s-1", &t4, &[]).unwrap();

        assert_eq!(
            store.tokens_by_day(0).unwrap(),
            vec![(0, 60, 2), (86_400_000, 30, 1)]
        );
        // since_ms фильтрует дни.
        assert_eq!(
            store.tokens_by_day(86_400_000).unwrap(),
            vec![(86_400_000, 30, 1)]
        );
    }

    #[test]
    fn load_all_keeps_creation_order_and_delete_cascades() {
        let store = SqliteStore::in_memory().unwrap();
        let a = sample_session();
        let mut b = Session::new(SessionId::new("s-2"), "D:/other", "test-model");
        b.created_ms = a.created_ms + 5;
        store.create_session(&a).unwrap();
        store.create_session(&b).unwrap();
        store
            .save_turn(a.id.as_str(), &sample_turn(1), &a.history)
            .unwrap();

        let all = store.load_all().unwrap();
        assert_eq!(all.len(), 2);
        assert_eq!(all[0].id.as_str(), "s-1");
        assert_eq!(all[1].id.as_str(), "s-2");

        store.delete_session("s-1").unwrap();
        let all = store.load_all().unwrap();
        assert_eq!(all.len(), 1);
        assert_eq!(all[0].id.as_str(), "s-2");
        // Каскад: ходов и сообщений удалённой сессии в базе не осталось.
        let turns: i64 = store
            .conn
            .query_row(
                "SELECT COUNT(*) FROM turns WHERE session_id = 's-1'",
                [],
                |r| r.get(0),
            )
            .unwrap();
        let msgs: i64 = store
            .conn
            .query_row(
                "SELECT COUNT(*) FROM messages WHERE session_id = 's-1'",
                [],
                |r| r.get(0),
            )
            .unwrap();
        assert_eq!(turns, 0);
        assert_eq!(msgs, 0);
    }

    /// Лёгкий снимок для автоочистки: тот же порядок и те же поля, что даёт
    /// `load_all`, но без ходов и сообщений. Порядок сверяется с `load_all`
    /// попарно, потому что «самые старые» в лимите `max_sessions` определён
    /// именно этим ORDER BY, и расхождение молча резало бы не те сессии.
    #[test]
    fn session_stubs_match_load_all_order_without_payload() {
        let store = SqliteStore::in_memory().unwrap();
        let a = sample_session();
        let mut b = Session::new(SessionId::new("s-2"), "D:/other", "test-model");
        b.created_ms = a.created_ms + 5;
        let mut c = Session::new(SessionId::new("s-3"), "D:/third", "test-model");
        c.created_ms = a.created_ms + 1;
        store.create_session(&a).unwrap();
        store.create_session(&b).unwrap();
        store.create_session(&c).unwrap();
        // Тяжёлое содержимое: снимок обязан его не тащить, но и не терять.
        store
            .save_turn(a.id.as_str(), &sample_turn(1), &a.history)
            .unwrap();

        let stubs = store.session_stubs().unwrap();
        let all = store.load_all().unwrap();
        assert_eq!(stubs.len(), 3);
        assert_eq!(all.len(), 3);
        for (stub, full) in stubs.iter().zip(all.iter()) {
            assert_eq!(stub.id, full.id.as_str());
            assert_eq!(stub.cwd, full.cwd);
            assert_eq!(stub.created_ms, full.created_ms);
        }
        assert_eq!(
            stubs.iter().map(|s| s.id.as_str()).collect::<Vec<_>>(),
            vec!["s-1", "s-3", "s-2"],
            "порядок created_ms asc"
        );
        // Снимок не несёт содержимого — проверять нечего, но полный ход
        // при этом остаётся в базе и доступен тому, кому он нужен.
        assert!(!all[0].turns.is_empty());

        store.delete_session("s-3").unwrap();
        let stubs = store.session_stubs().unwrap();
        assert_eq!(stubs.len(), 2);
        assert_eq!(
            stubs.iter().map(|s| s.id.as_str()).collect::<Vec<_>>(),
            vec!["s-1", "s-2"]
        );
    }

    /// Пустая база — пустой снимок, а не ошибка: первый запуск приложения
    /// проходит уборку до того, как появилась хотя бы одна сессия.
    #[test]
    fn session_stubs_on_empty_store() {
        let store = SqliteStore::in_memory().unwrap();
        assert!(store.session_stubs().unwrap().is_empty());
    }

    #[test]
    fn prefs_set_get_overwrite() {
        let store = SqliteStore::in_memory().unwrap();
        assert_eq!(store.get_pref("theme").unwrap(), None);
        store.set_pref("theme", "dark").unwrap();
        store.set_pref("theme", "matrix").unwrap();
        assert_eq!(store.get_pref("theme").unwrap().as_deref(), Some("matrix"));
    }

    #[test]
    fn crash_between_writes_keeps_db_intact() {
        // База в файле: первый владелец роняет соединение без закрытия
        // (имитация обрыва процесса), WAL обязана пережить это.
        let dir = std::env::temp_dir().join(format!("swagcod-store-test-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("crash.db");
        let _ = std::fs::remove_file(&path);
        {
            let store = SqliteStore::open(&path).unwrap();
            let ses = sample_session();
            store.create_session(&ses).unwrap();
            store
                .save_turn(ses.id.as_str(), &sample_turn(1), &ses.history)
                .unwrap();
            // Намеренно не закрываем: соединение уходит без drop-семантики
            // закрытия, как при kill процесса.
            std::mem::forget(store);
        }
        let store = SqliteStore::open(&path).unwrap();
        assert_eq!(store.integrity_check().unwrap(), "ok");
        let loaded = store
            .load_session("s-1")
            .unwrap()
            .expect("сессия пережила обрыв");
        assert_eq!(loaded.turns.len(), 1);
        assert_eq!(loaded.history.len(), 3);
        drop(store);
        let _ = std::fs::remove_dir_all(&dir);
    }

    // ---- B-7: журнал подтверждений и per-session политика ----

    fn entry(call: &str, decision: &str, actor: &str, ms: u64) -> ApprovalEntry {
        ApprovalEntry {
            session_id: "s-1".into(),
            turn_id: "t-1".into(),
            call_id: call.into(),
            tool: "bash".into(),
            summary: "rm -rf build".into(),
            decision: decision.into(),
            actor: actor.into(),
            decided_ms: ms,
        }
    }

    #[test]
    fn approval_journal_roundtrip() {
        let store = SqliteStore::in_memory().unwrap();
        let ses = sample_session();
        store.create_session(&ses).unwrap();
        store
            .log_approval(&entry("c1", "approved", "user", 100))
            .unwrap();
        store
            .log_approval(&entry("c2", "denied", "user", 200))
            .unwrap();
        store
            .log_approval(&entry("c3", "denied", "system", 300))
            .unwrap();

        let all = store.approval_log(None, 10).unwrap();
        assert_eq!(all.len(), 3);
        // Свежайшие первыми: журнал читается как лента.
        assert_eq!(all[0].call_id, "c3");
        assert_eq!(all[0].actor, "system");
        assert_eq!(all[2].call_id, "c1");
        assert_eq!(all[2].decision, "approved");

        let limited = store.approval_log(Some("s-1"), 2).unwrap();
        assert_eq!(limited.len(), 2);
        assert!(store.approval_log(Some("s-2"), 10).unwrap().is_empty());
    }

    #[test]
    fn approval_journal_survives_session_delete_in_file_db() {
        // Журнал автономный (без FK на turns) намеренно: аудит переживает
        // удаление сессии из списка — но по session_id остаётся фильтруемым.
        let dir = std::env::temp_dir().join(format!("swagcod-approvals-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("a.db");
        let _ = std::fs::remove_file(&path);
        {
            let store = SqliteStore::open(&path).unwrap();
            let ses = sample_session();
            store.create_session(&ses).unwrap();
            store
                .log_approval(&entry("c1", "approved", "user", 100))
                .unwrap();
            store.delete_session("s-1").unwrap();
            let all = store.approval_log(None, 10).unwrap();
            assert_eq!(all.len(), 1, "аудит не исчезает вместе с сессией");
        }
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn session_policy_persists_and_clears() {
        use crate::turn::ApprovalPolicy;
        let store = SqliteStore::in_memory().unwrap();
        let ses = sample_session();
        store.create_session(&ses).unwrap();
        // По умолчанию политика не задана: наследуется глобальная.
        let loaded = store.load_session("s-1").unwrap().unwrap();
        assert_eq!(loaded.approval_policy, None);

        store.set_session_policy("s-1", Some("always")).unwrap();
        let loaded = store.load_session("s-1").unwrap().unwrap();
        assert_eq!(loaded.approval_policy, Some(ApprovalPolicy::Always));

        // load_all видит то же, что load_session.
        let all = store.load_all().unwrap();
        assert_eq!(all[0].approval_policy, Some(ApprovalPolicy::Always));

        store.set_session_policy("s-1", None).unwrap();
        let loaded = store.load_session("s-1").unwrap().unwrap();
        assert_eq!(loaded.approval_policy, None);

        // Мусорная строка не становится политикой.
        store.set_session_policy("s-1", Some("yolo")).unwrap();
        let loaded = store.load_session("s-1").unwrap().unwrap();
        assert_eq!(loaded.approval_policy, None);
    }

    #[test]
    fn create_session_persists_policy() {
        use crate::turn::ApprovalPolicy;
        let store = SqliteStore::in_memory().unwrap();
        let mut ses = sample_session();
        ses.approval_policy = Some(ApprovalPolicy::OnDangerous);
        store.create_session(&ses).unwrap();
        let loaded = store.load_session("s-1").unwrap().unwrap();
        assert_eq!(loaded.approval_policy, Some(ApprovalPolicy::OnDangerous));
    }

    /* ── F-7: персистентная память ── */

    #[test]
    fn memory_roundtrip_scopes_and_forget() {
        let store = SqliteStore::in_memory().unwrap();
        let g1 = store
            .memory_add(
                MEMORY_SCOPE_GLOBAL,
                None,
                None,
                "  сборка: cargo test --workspace  ",
            )
            .unwrap();
        let g2 = store
            .memory_add(
                MEMORY_SCOPE_GLOBAL,
                None,
                None,
                "деплой: tools\\build-release.ps1",
            )
            .unwrap();
        let s1 = store
            .memory_add(MEMORY_SCOPE_SESSION, Some("s-1"), None, "заметка сессии 1")
            .unwrap();
        assert_ne!(g1, g2);

        let globals = store
            .memory_list(MEMORY_SCOPE_GLOBAL, None, None, 50)
            .unwrap();
        assert_eq!(globals.len(), 2, "сессионная строка не попала в глобальные");
        // Текст пишется trimmed: пробелы вокруг факта — мусор в промпте.
        assert!(
            globals
                .iter()
                .any(|m| m.text == "сборка: cargo test --workspace"),
            "{globals:?}"
        );
        assert!(globals.iter().all(|m| m.scope == MEMORY_SCOPE_GLOBAL));
        assert!(globals.iter().all(|m| m.created_ms > 0));
        // У глобального факта владельца нет: обе колонки пусты.
        assert!(
            globals
                .iter()
                .all(|m| m.session_id.is_none() && m.dir.is_none()),
            "{globals:?}"
        );
        // Новые первыми: при переполнении потолка обрезается старьё.
        assert_eq!(globals[0].id, g2, "свежий факт в начале: {globals:?}");

        let sess = store
            .memory_list(MEMORY_SCOPE_SESSION, Some("s-1"), None, 50)
            .unwrap();
        assert_eq!(sess.len(), 1);
        assert_eq!(sess[0].id, s1);
        assert_eq!(sess[0].session_id.as_deref(), Some("s-1"));
        // Чужая сессия не видит заметку.
        assert!(store
            .memory_list(MEMORY_SCOPE_SESSION, Some("s-2"), None, 50)
            .unwrap()
            .is_empty());

        assert!(store.memory_forget(s1).unwrap());
        assert!(
            !store.memory_forget(s1).unwrap(),
            "повторное удаление — false"
        );
        assert!(store
            .memory_list(MEMORY_SCOPE_SESSION, Some("s-1"), None, 50)
            .unwrap()
            .is_empty());
        // Предел выборки соблюдается и не ломает порядок.
        let one = store
            .memory_list(MEMORY_SCOPE_GLOBAL, None, None, 1)
            .unwrap();
        assert_eq!(one.len(), 1);
        assert_eq!(one[0].id, g2);
    }

    /// rev56: область folder — факт принадлежит рабочей папке и виден только
    /// ей. Проверка на трёх папках: без фильтра по `dir` факты соседей уехали
    /// бы в системный промпт чужого проекта, и владелец увидел бы их запреты и
    /// соглашения там, где они не действуют.
    #[test]
    fn memory_folder_scope_is_bound_to_dir() {
        let store = SqliteStore::in_memory().unwrap();
        let f1 = store
            .memory_add(
                MEMORY_SCOPE_FOLDER,
                None,
                Some("D:/proj-a"),
                "здесь pnpm, не npm",
            )
            .unwrap();
        store
            .memory_add(
                MEMORY_SCOPE_FOLDER,
                None,
                Some("D:/proj-b"),
                "здесь cargo, тесты обязательны",
            )
            .unwrap();
        store
            .memory_add(
                MEMORY_SCOPE_GLOBAL,
                None,
                None,
                "владелец любит краткие ответы",
            )
            .unwrap();

        let a = store
            .memory_list(MEMORY_SCOPE_FOLDER, None, Some("D:/proj-a"), 50)
            .unwrap();
        assert_eq!(a.len(), 1, "факт соседней папки не виден: {a:?}");
        assert_eq!(a[0].id, f1);
        assert_eq!(a[0].dir.as_deref(), Some("D:/proj-a"));
        assert_eq!(a[0].text, "здесь pnpm, не npm");
        assert_eq!(a[0].session_id, None, "у факта папки нет сессии-владельца");

        let b = store
            .memory_list(MEMORY_SCOPE_FOLDER, None, Some("D:/proj-b"), 50)
            .unwrap();
        assert_eq!(b.len(), 1);
        assert!(b[0].text.contains("cargo"), "{b:?}");

        // Неизвестная папка — пусто, а не «все факты папок подряд».
        assert!(store
            .memory_list(MEMORY_SCOPE_FOLDER, None, Some("D:/proj-c"), 50)
            .unwrap()
            .is_empty());
        // Глобальная выборка не подмешивает папочные строки.
        let g = store
            .memory_list(MEMORY_SCOPE_GLOBAL, None, None, 50)
            .unwrap();
        assert_eq!(g.len(), 1, "{g:?}");
        // И наоборот: папочная выборка не подмешивает глобальные.
        assert!(!a.iter().any(|m| m.text.contains("краткие")), "{a:?}");
    }

    /// rev56: владелец обязан соответствовать области. Проверка живёт в ядре
    /// (`memory_check_owner`), поэтому обе реализации Store отвечают одинаково;
    /// тест держит и текст ошибки — его видит человек в UI и модель в
    /// результате инструмента.
    #[test]
    fn memory_owner_must_match_scope() {
        let store = SqliteStore::in_memory().unwrap();
        // folder без пути: строка не нашлась бы ни одной выборкой.
        let e = store
            .memory_add(MEMORY_SCOPE_FOLDER, None, None, "факт")
            .unwrap_err();
        assert!(e.to_string().contains("требует рабочую папку"), "{e}");
        // session без id — то же самое.
        let e = store
            .memory_add(MEMORY_SCOPE_SESSION, None, None, "заметка")
            .unwrap_err();
        assert!(e.to_string().contains("требует владельца"), "{e}");
        // global с владельцем: молча сохранить значило бы потерять строку.
        let e = store
            .memory_add(MEMORY_SCOPE_GLOBAL, Some("s-1"), None, "факт")
            .unwrap_err();
        assert!(e.to_string().contains("не принадлежит"), "{e}");
        let e = store
            .memory_add(MEMORY_SCOPE_GLOBAL, None, Some("D:/x"), "факт")
            .unwrap_err();
        assert!(e.to_string().contains("не принадлежит"), "{e}");
        // Перестановка двух соседних Option местами не проходит молча.
        let e = store
            .memory_add(MEMORY_SCOPE_SESSION, Some("D:/x"), Some("s-1"), "заметка")
            .unwrap_err();
        assert!(e.to_string().contains("одновременно"), "{e}");
        let e = store
            .memory_add(MEMORY_SCOPE_FOLDER, Some("s-1"), Some("D:/x"), "факт")
            .unwrap_err();
        assert!(e.to_string().contains("одновременно"), "{e}");
        // Выборка подчиняется тем же правилам.
        let e = store
            .memory_list(MEMORY_SCOPE_FOLDER, None, None, 10)
            .unwrap_err();
        assert!(e.to_string().contains("требует рабочую папку"), "{e}");
        // Пустая строка вместо владельца — не владелец.
        let e = store
            .memory_add(MEMORY_SCOPE_FOLDER, None, Some("   "), "факт")
            .unwrap_err();
        assert!(e.to_string().contains("требует рабочую папку"), "{e}");
    }

    #[test]
    fn memory_rejects_empty_overlong_and_unknown_scope() {
        let store = SqliteStore::in_memory().unwrap();
        let e = store
            .memory_add(MEMORY_SCOPE_GLOBAL, None, None, "   ")
            .unwrap_err();
        assert!(e.to_string().contains("пустая"), "{e}");
        let e = store
            .memory_add(
                MEMORY_SCOPE_GLOBAL,
                None,
                None,
                &"ф".repeat(MEMORY_TEXT_MAX_CHARS + 1),
            )
            .unwrap_err();
        assert!(e.to_string().contains("длиннее"), "{e}");
        /* rev56: многострочный текст — не повод для отказа. Владелец
        импортирует в память целые промпты из txt/md, а тримятся только края. */
        let multi = store
            .memory_add(
                MEMORY_SCOPE_GLOBAL,
                None,
                None,
                "  # Роль\nТы ревьюер.\n\nПроверяй тесты.  ",
            )
            .unwrap();
        let rows = store
            .memory_list(MEMORY_SCOPE_GLOBAL, None, None, 10)
            .unwrap();
        let saved = rows.iter().find(|m| m.id == multi).unwrap();
        assert_eq!(
            saved.text, "# Роль\nТы ревьюер.\n\nПроверяй тесты.",
            "переводы строк обязаны доехать дословно: {:?}",
            saved.text
        );
        // Молча превратить заметку сессии в глобальный факт нельзя.
        let e = store.memory_add("private", None, None, "факт").unwrap_err();
        assert!(e.to_string().contains("неизвестная область"), "{e}");
        let e = store.memory_list("private", None, None, 10).unwrap_err();
        assert!(e.to_string().contains("неизвестная область"), "{e}");
        assert!(
            store
                .memory_list(MEMORY_SCOPE_GLOBAL, None, None, 10)
                .unwrap()
                .len()
                == 1
        );
    }

    /// База, созданная до F-7 (таблицы `memory` нет), открывается прежним
    /// путём: SCHEMA добирает таблицу через IF NOT EXISTS, поэтому обновление
    /// на машине владельца не означает «память не работает».
    #[test]
    fn memory_table_appears_on_legacy_database() {
        let dir = std::env::temp_dir().join(format!("swagcod-mem-legacy-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("legacy.db");
        let _ = std::fs::remove_file(&path);
        {
            let conn = Connection::open(&path).unwrap();
            conn.execute_batch("CREATE TABLE prefs(key TEXT PRIMARY KEY, value TEXT NOT NULL);")
                .unwrap();
        }
        let store = SqliteStore::open(&path).unwrap();
        assert_eq!(store.integrity_check().unwrap(), "ok");
        let id = store
            .memory_add(MEMORY_SCOPE_GLOBAL, None, None, "факт после миграции")
            .unwrap();
        assert!(id > 0);
        assert_eq!(
            store
                .memory_list(MEMORY_SCOPE_GLOBAL, None, None, 10)
                .unwrap()
                .len(),
            1
        );
        std::fs::remove_dir_all(&dir).ok();
    }

    /// rev56: база, созданная ДО области folder (таблица `memory` есть, но
    /// колонки `dir` нет), открывается и доезжает до новой схемы через ALTER.
    ///
    /// Это сценарий обновления на машине владельца: без миграции каждый INSERT
    /// в память падал бы на неизвестной колонке, то есть память молча
    /// превратилась бы в «не работает» ровно после обновления приложения.
    /// Старые строки обязаны выжить с пустым `dir`.
    #[test]
    fn memory_dir_column_appears_on_pre_rev56_database() {
        let dir = std::env::temp_dir().join(format!("swagcod-mem-dir-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("pre56.db");
        let _ = std::fs::remove_file(&path);
        {
            /* Схема ровно та, что была до rev56: четыре колонки, без dir. */
            let conn = Connection::open(&path).unwrap();
            conn.execute_batch(
                "CREATE TABLE memory(
                   id INTEGER PRIMARY KEY AUTOINCREMENT,
                   scope TEXT NOT NULL DEFAULT 'global',
                   text TEXT NOT NULL,
                   created_ms INTEGER NOT NULL,
                   session_id TEXT
                 );
                 INSERT INTO memory(scope, text, created_ms, session_id)
                   VALUES ('global', 'старый факт', 1, NULL);",
            )
            .unwrap();
        }
        let store = SqliteStore::open(&path).unwrap();
        assert_eq!(store.integrity_check().unwrap(), "ok");
        let old = store
            .memory_list(MEMORY_SCOPE_GLOBAL, None, None, 10)
            .unwrap();
        assert_eq!(old.len(), 1, "старая строка переживает миграцию");
        assert_eq!(old[0].text, "старый факт");
        assert_eq!(old[0].dir, None, "у прежних фактов владельца-папки нет");
        // После миграции область folder работает в той же базе.
        store
            .memory_add(
                MEMORY_SCOPE_FOLDER,
                None,
                Some("D:/proj"),
                "новый факт папки",
            )
            .unwrap();
        let f = store
            .memory_list(MEMORY_SCOPE_FOLDER, None, Some("D:/proj"), 10)
            .unwrap();
        assert_eq!(f.len(), 1);
        assert_eq!(f[0].text, "новый факт папки");
        std::fs::remove_dir_all(&dir).ok();
    }

    /* ── F-10: полнотекстовый поиск по истории (FTS5). ── */

    /// Сессия с историей, записанной в базу через save_turn.
    fn store_with_history() -> (SqliteStore, Session) {
        let store = SqliteStore::in_memory().unwrap();
        let ses = sample_session();
        store.create_session(&ses).unwrap();
        store
            .save_turn(ses.id.as_str(), &sample_turn(1), &ses.history)
            .unwrap();
        (store, ses)
    }

    #[test]
    fn fts_search_finds_history_with_snippet() {
        let (store, _ses) = store_with_history();
        let hits = store.search_messages("привет", 10).unwrap();
        assert_eq!(hits.len(), 1, "найдено одно сообщение пользователя");
        let h = &hits[0];
        assert_eq!(h.session_id, "s-1");
        assert_eq!(
            h.session_title, "проба",
            "заголовок сессии приезжает с находкой"
        );
        assert_eq!(h.role, "user");
        assert_eq!(h.ord, 0, "ord — позиция в истории");
        assert!(
            h.snippet.contains('[') && h.snippet.contains(']'),
            "сниппет подсвечивает совпадение: {}",
            h.snippet
        );
        assert!(
            h.rank < 0.0,
            "bm25 лучшего совпадения отрицателен: {}",
            h.rank
        );
    }

    #[test]
    fn fts_search_covers_reasoning_and_tool_rows() {
        let (store, _ses) = store_with_history();
        // «думаю» есть только в reasoning ответа — колонка индексируется.
        let hits = store.search_messages("думаю", 10).unwrap();
        assert_eq!(hits.len(), 1, "рассуждение найдено: {hits:?}");
        assert_eq!(hits[0].role, "assistant");
        assert!(hits[0].snippet.contains("[думаю]"), "{}", hits[0].snippet);
        // Вывод инструмента — тоже часть истории, и его можно найти.
        let tools = store.search_messages("содержимое", 10).unwrap();
        assert_eq!(tools.len(), 1);
        assert_eq!(tools[0].role, "tool");
    }

    #[test]
    fn fts_search_is_prefix_and_case_tolerant() {
        let (store, _ses) = store_with_history();
        assert_eq!(
            store.search_messages("прив", 10).unwrap().len(),
            1,
            "префикс находит словоформу"
        );
        assert_eq!(
            store.search_messages("ПРИВЕТ", 10).unwrap().len(),
            1,
            "регистр не важен"
        );
    }

    /// Ограничение токенайзера зафиксировано тестом, чтобы не выглядело
    /// багом поиска: `unicode61 remove_diacritics 2` сворачивает латинскую
    /// диакритику, а кириллическая «ё» остаётся отдельной буквой. То есть
    /// «зеленые» НЕ находит «зелёные». Обход на стороне человека — искать
    /// префиксом («зелё»), что и проверяется рядом.
    #[test]
    fn fts_search_does_not_fold_cyrillic_yo() {
        let store = SqliteStore::in_memory().unwrap();
        let mut ses = Session::new(SessionId::new("s-2"), "D:/proj", "test-model");
        ses.title = "диакритика".to_string();
        ses.push_user_message("тесты зелёные");
        store.create_session(&ses).unwrap();
        store
            .save_turn("s-2", &sample_turn(2), &ses.history)
            .unwrap();

        assert_eq!(store.search_messages("зелёные", 10).unwrap().len(), 1);
        assert_eq!(
            store.search_messages("зеленые", 10).unwrap().len(),
            0,
            "ё и е — разные токены: это ограничение unicode61, а не пропавшая строка"
        );
        assert_eq!(
            store.search_messages("зелё", 10).unwrap().len(),
            1,
            "префикс спасает: достаточно первых букв"
        );
    }

    #[test]
    fn fts_search_rejects_empty_punctuation_and_overlong_queries() {
        let (store, _ses) = store_with_history();
        // Пустой запрос — ошибка, а не «нашлось всё»: FTS5 на пустой строке
        // ведёт себя по-разному на двух бэкендах (rusqlite молчит,
        // better-sqlite3 бросает syntax error), поэтому правило общее.
        let e = store.search_messages("", 10).unwrap_err();
        assert!(e.to_string().contains("пустой"), "{e}");
        let e = store.search_messages("   \t ", 10).unwrap_err();
        assert!(e.to_string().contains("пустой"), "{e}");
        let e = store.search_messages("--- *** (((", 10).unwrap_err();
        assert!(e.to_string().contains("ни одного слова"), "{e}");
        let e = store
            .search_messages(&"слово ".repeat(SEARCH_MAX_TOKENS + 1), 10)
            .unwrap_err();
        assert!(e.to_string().contains("больше"), "{e}");
        let e = store
            .search_messages(&"д".repeat(SEARCH_MAX_QUERY_CHARS + 1), 10)
            .unwrap_err();
        assert!(e.to_string().contains("длиннее"), "{e}");
    }

    #[test]
    fn fts_search_respects_limit_and_orders_by_rank() {
        let store = SqliteStore::in_memory().unwrap();
        for n in 0..5 {
            let mut ses = Session::new(SessionId::new(format!("s-{n}")), "D:/proj", "test-model");
            ses.title = format!("сессия {n}");
            // Одно и то же слово в пяти сессиях: выдача обязана резаться.
            ses.push_user_message("общий маркер поиска");
            store.create_session(&ses).unwrap();
            store
                .save_turn(ses.id.as_str(), &sample_turn(n), &ses.history)
                .unwrap();
        }
        assert_eq!(store.search_messages("маркер", 100).unwrap().len(), 5);
        assert_eq!(store.search_messages("маркер", 2).unwrap().len(), 2);
        // limit=0 не значит «ноль строк»: зажимается в 1, иначе SQL LIMIT 0
        // выглядел бы как «ничего не нашлось».
        assert_eq!(store.search_messages("маркер", 0).unwrap().len(), 1);
        let ranks: Vec<f64> = store
            .search_messages("маркер", 100)
            .unwrap()
            .iter()
            .map(|h| h.rank)
            .collect();
        assert!(
            ranks.windows(2).all(|w| w[0] <= w[1]),
            "выдача по убыванию качества: {ranks:?}"
        );
    }

    #[test]
    fn fts_search_drops_hits_with_the_session() {
        let (store, _ses) = store_with_history();
        assert_eq!(store.search_messages("привет", 10).unwrap().len(), 1);
        store.delete_session("s-1").unwrap();
        assert!(
            store.search_messages("привет", 10).unwrap().is_empty(),
            "каскадное удаление сессии чистит индекс триггером"
        );
        assert_eq!(store.integrity_check().unwrap(), "ok");
    }

    #[test]
    fn fts_search_survives_rewrite_of_history() {
        // save_session пишет историю целиком (DELETE + INSERT): индекс обязан
        // остаться синхронным, иначе поиск показывал бы удалённые сообщения.
        let (store, ses) = store_with_history();
        let mut ses2 = ses.clone();
        ses2.history.clear();
        ses2.push_user_message("совершенно другой текст");
        store
            .save_turn("s-1", &sample_turn(2), &ses2.history)
            .unwrap();
        assert!(
            store.search_messages("привет", 10).unwrap().is_empty(),
            "старое сообщение не ищется после перезаписи истории"
        );
        assert_eq!(store.search_messages("другой", 10).unwrap().len(), 1);
    }

    /// База, созданная до F-10 (индекса FTS5 нет, история есть), после
    /// открытия ищет по старой истории: дозация индекса одноразовая.
    #[test]
    fn fts_index_backfills_legacy_database() {
        /* Схема без FTS5 — ровно то, что было до ревизии 48. Копия намеренная:
        тест обязан открывать базу прежней формы, а не текущую. */
        const LEGACY_NO_FTS: &str = r#"
CREATE TABLE sessions(
  id TEXT PRIMARY KEY,
  cwd TEXT NOT NULL,
  model TEXT NOT NULL,
  title TEXT NOT NULL DEFAULT '',
  summary TEXT NOT NULL DEFAULT '',
  approval_policy TEXT,
  created_ms INTEGER NOT NULL
);
CREATE TABLE messages(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  ord INTEGER NOT NULL,
  role TEXT NOT NULL,
  content TEXT NOT NULL,
  reasoning TEXT NOT NULL DEFAULT '',
  tool_call_id TEXT,
  tool_calls_json TEXT NOT NULL DEFAULT '[]'
);
"#;
        let dir = std::env::temp_dir().join(format!("swagcod-fts-legacy-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("legacy.db");
        let _ = std::fs::remove_file(&path);
        {
            let conn = Connection::open(&path).unwrap();
            conn.execute_batch(LEGACY_NO_FTS).unwrap();
            conn.execute(
                "INSERT INTO sessions(id, cwd, model, title, created_ms) VALUES ('s-9','D:/proj','m','старая',1)",
                [],
            )
            .unwrap();
            conn.execute(
                "INSERT INTO messages(session_id, ord, role, content) VALUES ('s-9',0,'user','доисторическое сообщение')",
                [],
            )
            .unwrap();
        }
        let store = SqliteStore::open(&path).unwrap();
        let hits = store.search_messages("доисторическое", 10).unwrap();
        assert_eq!(hits.len(), 1, "старая история найдена после дозации");
        assert_eq!(hits[0].session_title, "старая");
        // Повторное открытие не должно ни падать, ни дублировать находки.
        drop(store);
        let again = SqliteStore::open(&path).unwrap();
        assert_eq!(
            again.search_messages("доисторическое", 10).unwrap().len(),
            1
        );
        std::fs::remove_dir_all(&dir).ok();
    }

    #[test]
    fn fts_match_expr_quotes_specials_and_adds_prefix() {
        // Каждое слово кавычится и получает префикс; между словами — AND.
        assert_eq!(fts_match_expr("тест").unwrap(), "\"тест\"*");
        assert_eq!(
            fts_match_expr("  тест  буг  ").unwrap(),
            "\"тест\"* \"буг\"*"
        );
        // Спецсимволы FTS5 внутри кавычек — текст, а не синтаксис.
        assert_eq!(fts_match_expr("a* OR b").unwrap(), "\"a\"* \"OR\"* \"b\"*");
        // Вставленная фраза в кавычках ищется как И всех её слов.
        assert_eq!(fts_match_expr("say \"hi\"").unwrap(), "\"say\"* \"hi\"*");
        // `c++` токенайзер режет на `c` — запрос обязан совпасть с индексом.
        assert_eq!(fts_match_expr("c++").unwrap(), "\"c\"*");
        assert_eq!(fts_match_expr("3.14").unwrap(), "\"3\"* \"14\"*");
        // Спецсимволы FTS5 не доезжают до MATCH: слово режется по
        // не-алфавитному, поэтому `a*` даёт токен `a`, а `OR` — литерал.
        // Знаки без букв и цифр пропускаются, а не роняют запрос.
        assert_eq!(fts_match_expr("тест ---").unwrap(), "\"тест\"*");
        assert!(fts_match_expr("...").is_err());
        assert!(fts_match_expr("").is_err());
        // Управляемые символы работают как разделители и в SQL не доезжают.
        let e = fts_match_expr("те\u{0}ст").unwrap();
        assert!(!e.contains('\u{0}'), "{e:?}");
        assert_eq!(e, "\"те\"* \"ст\"*");
    }

    /// Пунктуация в запросе — разделитель, а не литерал: иначе вставка фразы
    /// в кавычках или слова с плюсами молча не находили бы ничего.
    #[test]
    fn fts_search_treats_punctuation_as_separators() {
        let store = SqliteStore::in_memory().unwrap();
        let mut ses = Session::new(SessionId::new("s-3"), "D:/proj", "test-model");
        ses.title = "пунктуация".to_string();
        ses.push_user_message("он сказал \"привет мир\" и собрал c++ проект");
        store.create_session(&ses).unwrap();
        store
            .save_turn("s-3", &sample_turn(3), &ses.history)
            .unwrap();

        assert_eq!(store.search_messages("сказал", 10).unwrap().len(), 1);
        assert_eq!(
            store.search_messages("\"привет мир\"", 10).unwrap().len(),
            1,
            "фраза в кавычках ищется по словам"
        );
        assert_eq!(store.search_messages("c++", 10).unwrap().len(), 1);
        assert_eq!(store.search_messages("проект", 10).unwrap().len(), 1);
        /* Операторы FTS5 в запросе человека становятся обычными словами:
        запрос не падает и не меняет смысл. В этом сообщении «OR» и «NOT» нет,
        поэтому честно ноль находок — а не синтаксическая ошибка и не
        «нашлось всё». */
        assert_eq!(
            store.search_messages("OR NOT (проект)", 10).unwrap().len(),
            0
        );

        /* Обратная проверка нейтральности: если бы «OR»/«NOT» доехали до MATCH
        операторами, такой запрос был бы синтаксической ошибкой (OR без левого
        операнда). Он находит сообщение, где эти слова — текст. */
        let mut ses2 = Session::new(SessionId::new("s-4"), "D:/proj", "test-model");
        ses2.title = "операторы".to_string();
        ses2.push_user_message("справка: OR NOT AND NEAR — это слова, а не синтаксис");
        store.create_session(&ses2).unwrap();
        store
            .save_turn("s-4", &sample_turn(4), &ses2.history)
            .unwrap();
        let hits = store.search_messages("OR NOT", 10).unwrap();
        assert_eq!(
            hits.len(),
            1,
            "операторы ищутся как обычные слова: {hits:?}"
        );
        assert_eq!(hits[0].session_id, "s-4");
        assert!(store.search_messages("***", 10).is_err());
    }

    /// Семантика зафиксирована тестом, чтобы не выглядела багом в UI: AND
    /// действует внутри ОДНОГО сообщения, а не внутри сессии. Слова из разных
    /// сообщений одной сессии вместе не находятся.
    #[test]
    fn fts_search_is_per_message_not_per_session() {
        let (store, _ses) = store_with_history();
        // «привет» — в сообщении пользователя, «здравствуй» — в ответе.
        assert_eq!(store.search_messages("привет", 10).unwrap().len(), 1);
        assert_eq!(store.search_messages("здравствуй", 10).unwrap().len(), 1);
        assert!(
            store
                .search_messages("привет здравствуй", 10)
                .unwrap()
                .is_empty(),
            "в одном сообщении обоих слов нет"
        );
    }

    #[test]
    fn fts_search_follows_in_place_update() {
        // Триггер messages_au: UPDATE на месте (не перезапись истории целиком)
        // тоже обязан держать индекс в синхроне.
        let (store, _ses) = store_with_history();
        assert_eq!(store.search_messages("привет", 10).unwrap().len(), 1);
        store
            .conn
            .execute(
                "UPDATE messages SET content = 'обновлённый текст' WHERE ord = 0",
                [],
            )
            .unwrap();
        assert!(
            store.search_messages("привет", 10).unwrap().is_empty(),
            "старый текст не ищется после обновления строки"
        );
        assert_eq!(store.search_messages("обновлённый", 10).unwrap().len(), 1);
    }
}
