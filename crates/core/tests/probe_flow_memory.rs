/*! probe_flow_memory.rs — зонд «Обычный Юзер» для памяти и FTS-поиска
 (crates/core/src/store.rs).

Юзер не криворучит: он валидными данными ведёт память трёх областей
(global/session/folder), вставляет многострочные заметки (список покупок,
тодо), экспортирует факты в txt и импортирует обратно, редактирует факт
перезаписью, ищет по истории всех сессий через FTS5 (русский + английский),
листает сессии и переживает перезапуск приложения.

Запуск: cargo test -p swagcod-core --test probe_flow_memory -- --nocapture
Кейсы, вскрывшие реальный дефект, помечаются `#[ignore = "FINDING: ..."]`
после прогона, чтобы набор оставался зелёным.
*/

use std::path::PathBuf;
use std::sync::atomic::{AtomicUsize, Ordering};

use swagcod_core::store::{
    MemoryEntry, SqliteStore, Store, StoreError, MEMORY_SCOPE_FOLDER, MEMORY_SCOPE_GLOBAL,
    MEMORY_SCOPE_SESSION, MEMORY_TEXT_MAX_CHARS,
};
use swagcod_core::{Session, SessionId, TurnId, TurnRecord};
use swagcod_provider::types::ChatMessage;

static DIR_SEQ: AtomicUsize = AtomicUsize::new(0);

fn tmpdir(tag: &str) -> PathBuf {
    let n = DIR_SEQ.fetch_add(1, Ordering::SeqCst);
    let d = std::env::temp_dir().join(format!("swagcod-user-{}-{tag}-{n}", std::process::id()));
    std::fs::create_dir_all(&d).unwrap();
    d
}

/// Живая сессия: заголовок, из которого человека узнаёт свой разговор.
fn user_session(id: &str, cwd: &str) -> Session {
    let mut s = Session::new(SessionId::new(id), cwd, "user-model");
    s.title = format!("t-{id}");
    s
}

/// Ход юзера: id глобально уникален между сессиями (turns.id — PRIMARY KEY).
fn turn(n: u64) -> TurnRecord {
    TurnRecord {
        id: TurnId::new(format!("t-{n}")),
        started_ms: 1000 + n,
        ended_ms: Some(2000 + n),
        content: format!("ответ {n}"),
        reasoning: String::new(),
        tool_calls: vec![],
        est_input_tokens: 10,
        est_output_tokens: 20,
        ok: true,
        failure: None,
        parent_turn_id: None,
        checkpoint_sha: None,
    }
}

/// Уникальный счётчик ходов внутри теста: turns.id не должен пересекаться
/// между сессиями одного прогона.
fn turn_seq() -> u64 {
    static SEQ: AtomicUsize = AtomicUsize::new(0);
    SEQ.fetch_add(1, Ordering::SeqCst) as u64 + 1
}

/// F01. Юзер ведёт память в трёх областях: пара глобальных фактов, заметка
/// своей сессии, факты двух разных рабочих папок. Читает каждую область —
/// и не видит чужого: глобальная не подмешала заметок, папка alpha не видит
/// beta, чужая сессия не видит заметок.
#[test]
fn probe_f01_three_scopes_kept_apart() {
    let store = SqliteStore::in_memory().unwrap();

    let g1 = store
        .memory_add(
            MEMORY_SCOPE_GLOBAL,
            None,
            None,
            "Владелец предпочитает короткие ответы без воды",
        )
        .unwrap();
    let g2 = store
        .memory_add(
            MEMORY_SCOPE_GLOBAL,
            None,
            None,
            "SwagCod — Rust workspace: core-крейт с SQLite и FTS5",
        )
        .unwrap();
    let s1 = store
        .memory_add(
            MEMORY_SCOPE_SESSION,
            Some("s-notes"),
            None,
            "Разобрались с FTS: сниппет берёт колонку с лучшим совпадением",
        )
        .unwrap();
    let f_a = store
        .memory_add(
            MEMORY_SCOPE_FOLDER,
            None,
            Some("D:/work-alpha"),
            "Папка work-alpha: фронтенд, запуск pnpm dev",
        )
        .unwrap();
    let f_b = store
        .memory_add(
            MEMORY_SCOPE_FOLDER,
            None,
            Some("D:/work-beta"),
            "Папка work-beta: бэкенд на Rust, cargo run -p swagcod-core",
        )
        .unwrap();
    println!("[F01] записано: global {g1}/{g2}, session {s1}, folder {f_a}/{f_b}");

    // Глобальная область: ровно два факта, новые первыми.
    let globals = store
        .memory_list(MEMORY_SCOPE_GLOBAL, None, None, 100)
        .unwrap();
    assert_eq!(
        globals.len(),
        2,
        "[F01] в global попало лишнее: {globals:?}"
    );
    assert_eq!(globals[0].id, g2, "[F01] новые не первыми: {globals:?}");
    assert_eq!(globals[1].id, g1);
    assert!(
        globals
            .iter()
            .all(|m| m.session_id.is_none() && m.dir.is_none()),
        "[F01] у global-строк появился владелец: {globals:?}"
    );

    // Заметка сессии видна только своей сессии.
    let notes = store
        .memory_list(MEMORY_SCOPE_SESSION, Some("s-notes"), None, 100)
        .unwrap();
    assert_eq!(notes.len(), 1, "[F01] заметка сессии потерялась: {notes:?}");
    assert_eq!(notes[0].id, s1);
    assert_eq!(notes[0].session_id.as_deref(), Some("s-notes"));
    let other = store
        .memory_list(MEMORY_SCOPE_SESSION, Some("s-other"), None, 100)
        .unwrap();
    assert!(
        other.is_empty(),
        "[F01] чужая сессия видит чужие заметки: {other:?}"
    );

    // Факты папок изолированы по dir.
    let alpha = store
        .memory_list(MEMORY_SCOPE_FOLDER, None, Some("D:/work-alpha"), 100)
        .unwrap();
    assert_eq!(alpha.len(), 1, "[F01] папка alpha видит лишнее: {alpha:?}");
    assert_eq!(alpha[0].id, f_a);
    assert_eq!(alpha[0].dir.as_deref(), Some("D:/work-alpha"));
    let beta = store
        .memory_list(MEMORY_SCOPE_FOLDER, None, Some("D:/work-beta"), 100)
        .unwrap();
    assert_eq!(beta.len(), 1, "[F01] папка beta видит лишнее: {beta:?}");
    assert_eq!(beta[0].id, f_b);

    // Папковые факты не утекли в глобальную выдачу.
    assert!(
        !globals
            .iter()
            .any(|m| m.text.contains("pnpm") || m.text.contains("cargo")),
        "[F01] факты папок утекли в global: {globals:?}"
    );
    println!("[F01] области изолированы: global 2, session 1/1, folder 1+1");
}

/// F02. Экспорт фактов в txt и импорт обратно — как кнопка на каждой строке
/// памяти (rev56): текст каждой строки уезжает в свой файл и возвращается
/// новой строкой той же области. Ни одна строка не теряется и не искажается,
/// многострочный текст доезжает дословно.
#[test]
fn probe_f02_export_txt_and_import_back() {
    let dir = tmpdir("export");
    let store = SqliteStore::in_memory().unwrap();

    store
        .memory_add(
            MEMORY_SCOPE_GLOBAL,
            None,
            None,
            "проверять сборку перед коммитом, всегда",
        )
        .unwrap();
    let multiline = "список покупок на неделю:\n- молоко\n- хлеб\n- кофе\n\nи батарейки для мыши";
    store
        .memory_add(MEMORY_SCOPE_GLOBAL, None, None, multiline)
        .unwrap();
    store
        .memory_add(
            MEMORY_SCOPE_SESSION,
            Some("s-notes"),
            None,
            "идея: экспорт памяти держать в репозитории команды",
        )
        .unwrap();
    store
        .memory_add(
            MEMORY_SCOPE_FOLDER,
            None,
            Some("D:/work-beta"),
            "cargo test -p swagcod-core перед пушем обязателен",
        )
        .unwrap();

    // Собрать все строки, как их видит вкладка Память.
    let mut all: Vec<MemoryEntry> = Vec::new();
    for scope_call in [
        store
            .memory_list(MEMORY_SCOPE_GLOBAL, None, None, 100)
            .unwrap(),
        store
            .memory_list(MEMORY_SCOPE_SESSION, Some("s-notes"), None, 100)
            .unwrap(),
        store
            .memory_list(MEMORY_SCOPE_FOLDER, None, Some("D:/work-beta"), 100)
            .unwrap(),
    ] {
        all.extend(scope_call);
    }
    assert_eq!(all.len(), 4, "[F02] во вкладке не все строки: {all:?}");

    // Экспорт: каждая строка — свой txt-файл, текст дословно.
    let mut files = Vec::new();
    for m in &all {
        let path = dir.join(format!("memory-{}.txt", m.id));
        std::fs::write(&path, &m.text).unwrap();
        files.push((m.clone(), path));
    }
    println!("[F02] выгружено {} фактов в txt", files.len());

    // Импорт обратно: юзер читает файл и сохраняет строку в ту же область.
    for (m, path) in &files {
        let text = std::fs::read_to_string(path).unwrap();
        let new_id = store
            .memory_add(&m.scope, m.session_id.as_deref(), m.dir.as_deref(), &text)
            .unwrap_or_else(|e| panic!("[F02] импорт строки {path:?} упал: {e}"));
        assert_ne!(new_id, m.id, "[F02] импорт вернул существующий id");
    }

    // После импорта каждая область удвоилась, тексты совпали.
    let globals = store
        .memory_list(MEMORY_SCOPE_GLOBAL, None, None, 100)
        .unwrap();
    assert_eq!(globals.len(), 4, "[F02] global после импорта: {globals:?}");
    let mut texts: Vec<&str> = globals.iter().map(|m| m.text.as_str()).collect();
    texts.sort();
    let mut expect: Vec<&str> = vec![
        "проверять сборку перед коммитом, всегда",
        "проверять сборку перед коммитом, всегда",
        multiline,
        multiline,
    ];
    expect.sort_unstable();
    assert_eq!(
        texts, expect,
        "[F02] тексты после экспорта/импорта разъехались"
    );

    let notes = store
        .memory_list(MEMORY_SCOPE_SESSION, Some("s-notes"), None, 100)
        .unwrap();
    assert_eq!(notes.len(), 2, "[F02] session после импорта: {notes:?}");
    let folders = store
        .memory_list(MEMORY_SCOPE_FOLDER, None, Some("D:/work-beta"), 100)
        .unwrap();
    assert_eq!(folders.len(), 2, "[F02] folder после импорта: {folders:?}");

    // Многострочный факт не потерял ни строки.
    let copies: Vec<&MemoryEntry> = globals
        .iter()
        .filter(|m| m.text.contains("список покупок"))
        .collect();
    assert_eq!(
        copies.len(),
        2,
        "[F02] многострочный факт потерялся: {globals:?}"
    );
    for c in copies {
        assert_eq!(c.text, multiline, "[F02] многострочный факт искажён");
        assert_eq!(c.text.lines().count(), 6, "[F02] строки списка потерялись");
    }
    println!("[F02] экспорт/импорт: 4 из 4 строк вернулись дословно");
    std::fs::remove_dir_all(&dir).ok();
}

/// F03. Многострочная заметка: юзер вставляет список покупок и тодо —
/// переводы строк (включая блокнотовские CRLF) обязаны доехать дословно.
#[test]
fn probe_f03_multiline_note_keeps_formatting() {
    let store = SqliteStore::in_memory().unwrap();

    let todo = "тодо на вечер:\n1. дочитать документацию FTS5\n2. ответить в ревью\n\nпотом отдых";
    let id = store
        .memory_add(MEMORY_SCOPE_GLOBAL, None, None, todo)
        .unwrap();
    let list = store
        .memory_list(MEMORY_SCOPE_GLOBAL, None, None, 10)
        .unwrap();
    let saved = list
        .iter()
        .find(|m| m.id == id)
        .expect("[F03] заметка не нашлась");
    assert_eq!(saved.text, todo, "[F03] текст исказился: {:?}", saved.text);
    assert_eq!(
        saved.text.lines().count(),
        5,
        "[F03] структура списка потерялась: {:?}",
        saved.text
    );
    assert!(saved.text.contains("\n\n"), "[F03] пустая строка съедена");
    println!("[F03] многострочное тодо (5 строк) сохранено дословно");

    // Блокнотовский файл с CRLF: юзер вставляет текст прямо из него.
    let notepad = "список покупок:\r\n- молоко\r\n- хлеб";
    let id2 = store
        .memory_add(MEMORY_SCOPE_SESSION, Some("s-note"), None, notepad)
        .unwrap();
    let notes = store
        .memory_list(MEMORY_SCOPE_SESSION, Some("s-note"), None, 10)
        .unwrap();
    let saved2 = notes
        .iter()
        .find(|m| m.id == id2)
        .expect("[F03] блокнотовская заметка не нашлась");
    assert_eq!(
        saved2.text, notepad,
        "[F03] внутренние CRLF искажены: {:?}",
        saved2.text
    );
    println!(
        "[F03] CRLF из блокнота сохранён дословно ({} байт)",
        notepad.len()
    );
}

/// F04. Редактирование факта перезаписью: юзер меняет устаревший факт
/// (забыть старую строку, записать новую). Старая версия не дублируется,
/// в списке остаётся ровно одна строка с новым текстом.
#[test]
fn probe_f04_editing_fact_no_duplicate_left() {
    let store = SqliteStore::in_memory().unwrap();

    let old_id = store
        .memory_add(MEMORY_SCOPE_GLOBAL, None, None, "порт сервера 8080")
        .unwrap();

    // Юзер узнал новый порт: правит факт.
    assert!(
        store.memory_forget(old_id).unwrap(),
        "[F04] забыть существующую строку вернуло false"
    );
    let new_id = store
        .memory_add(MEMORY_SCOPE_GLOBAL, None, None, "порт сервера 9090")
        .unwrap();

    let list = store
        .memory_list(MEMORY_SCOPE_GLOBAL, None, None, 10)
        .unwrap();
    assert_eq!(
        list.len(),
        1,
        "[F04] после правки строк стало {}",
        list.len()
    );
    assert_eq!(
        list[0].id, new_id,
        "[F04] в списке не новая строка: {list:?}"
    );
    assert_eq!(list[0].text, "порт сервера 9090");
    assert!(
        list.iter().all(|m| m.text != "порт сервера 8080"),
        "[F04] старая версия осталась: {list:?}"
    );
    println!("[F04] факт отредактирован: 1 строка, старой версии нет");

    // Повторное «забыть» уже удалённой — false, не паника и не ложь.
    assert!(
        !store.memory_forget(old_id).unwrap(),
        "[F04] повторный forget существующей вернул true"
    );
    // Стор жив: следующая запись проходит.
    store
        .memory_add(MEMORY_SCOPE_GLOBAL, None, None, "ещё факт после правки")
        .unwrap();
    assert_eq!(
        store
            .memory_list(MEMORY_SCOPE_GLOBAL, None, None, 10)
            .unwrap()
            .len(),
        2
    );
}

/// F05. FTS-поиск по истории трёх сессий (русский + английский): слово
/// «connection» в разных сообщениях находит каждое ровно один раз, сниппет
/// подсвечивает совпадение, ord указывает на реальное сообщение; фраза из
/// двух слов ищется как пересечение; русское слово ищется префиксом.
#[test]
fn probe_f05_search_word_and_phrase() {
    let store = SqliteStore::in_memory().unwrap();

    // Сессия 1: англоязычный чат про базу.
    let mut s_db = user_session("s-db", "D:/work-beta");
    assert!(
        s_db.push_user_message("the connection to the database keeps dropping every few minutes")
    );
    s_db.push_assistant_message(ChatMessage::assistant(
        "I will check the connection pool settings and the retry policy",
    ));
    store.create_session(&s_db).unwrap();
    store
        .save_turn("s-db", &turn(turn_seq()), &s_db.history)
        .unwrap();

    // Сессия 2: чат про сеть.
    let mut s_net = user_session("s-net", "D:/work-beta");
    assert!(s_net.push_user_message("network connection timeout on file upload"));
    s_net.push_assistant_message(ChatMessage::assistant(
        "sounds like a proxy misconfiguration, checking next",
    ));
    store.create_session(&s_net).unwrap();
    store
        .save_turn("s-net", &turn(turn_seq()), &s_net.history)
        .unwrap();

    // Сессия 3: русскоязычный чат — слово connection не встречается.
    let mut s_ru = user_session("s-ru", "D:/work-alpha");
    assert!(s_ru.push_user_message("соединение с сервером обрывается каждые пять минут"));
    s_ru.push_assistant_message(ChatMessage::assistant("проверь настройки сети и прокси"));
    store.create_session(&s_ru).unwrap();
    store
        .save_turn("s-ru", &turn(turn_seq()), &s_ru.history)
        .unwrap();
    println!("[F05] три сессии записаны (англ ×2, рус ×1)");

    // Поиск по слову: три сообщения, каждое один раз.
    let hits = store.search_messages("connection", 100).unwrap();
    assert_eq!(
        hits.len(),
        3,
        "[F05] по слову connection должны найтись 3 сообщения: {hits:?}"
    );
    let mut pairs: Vec<(String, u32)> =
        hits.iter().map(|h| (h.session_id.clone(), h.ord)).collect();
    pairs.sort();
    pairs.dedup();
    assert_eq!(
        pairs.len(),
        hits.len(),
        "[F05] выдача задвоила сообщения: {hits:?}"
    );
    // Русская сессия не попадает под английское слово.
    assert!(
        hits.iter().all(|h| h.session_id != "s-ru"),
        "[F05] русский текст попал под английское слово: {hits:?}"
    );
    // ord указывает на реальное сообщение со словом.
    for h in &hits {
        let ses = store
            .load_session(&h.session_id)
            .unwrap()
            .unwrap_or_else(|| panic!("[F05] сессии {} нет в базе", h.session_id));
        let msg = &ses.history[h.ord as usize];
        assert!(
            msg.content.to_lowercase().contains("connection"),
            "[F05] hit {}:{} указывает на сообщение без слова: {:?}",
            h.session_id,
            h.ord,
            msg.content
        );
        assert_eq!(
            h.session_title, ses.title,
            "[F05] заголовок сессии в хите не совпал"
        );
    }
    // Сниппет с подсветкой совпадения.
    for h in &hits {
        assert!(
            h.snippet.contains("[connection]"),
            "[F05] сниппет без подсветки: {:?} (hit {}:{})",
            h.snippet,
            h.session_id,
            h.ord
        );
    }
    println!(
        "[F05] слово connection: {} хита, подсветка в каждом сниппете",
        hits.len()
    );

    // Фраза из двух слов: только сообщение, где есть оба слова.
    let phrase = store.search_messages("connection timeout", 100).unwrap();
    assert_eq!(
        phrase.len(),
        1,
        "[F05] фраза connection timeout должна найти 1 сообщение: {phrase:?}"
    );
    assert_eq!(phrase[0].session_id, "s-net");
    assert_eq!(
        phrase[0].ord, 0,
        "[F05] фраза нашла не то сообщение: {phrase:?}"
    );
    assert!(
        phrase[0].snippet.contains("[connection]"),
        "[F05] во фразовом сниппете нет первого слова: {:?}",
        phrase[0].snippet
    );
    assert!(
        phrase[0].snippet.contains("[timeout]"),
        "[F05] во фразовом сниппете нет второго слова: {:?}",
        phrase[0].snippet
    );
    println!("[F05] фраза из двух слов: 1 хит, оба слова подсвечены");

    // Русское слово ищется и находит русское сообщение.
    let ru = store.search_messages("соединение", 100).unwrap();
    assert_eq!(ru.len(), 1, "[F05] русское слово не нашлось: {ru:?}");
    assert_eq!(ru[0].session_id, "s-ru");
    assert_eq!(ru[0].ord, 0);
    assert!(
        ru[0].snippet.contains("[соединение]"),
        "[F05] русский сниппет без подсветки: {:?}",
        ru[0].snippet
    );
    println!("[F05] русское слово: 1 хит, подсветка на месте");
}

/// F05b. Юзер продолжает чат: второй ход дописывает историю той же сессии
/// (save_turn пишет полный снимок). Прежние сообщения не плодят дублей в
/// поиске — каждое по-прежнему один хит, новые находятся тоже.
#[test]
fn probe_f05b_second_turn_no_duplicate_hits() {
    let store = SqliteStore::in_memory().unwrap();

    let mut s_net = user_session("s-net", "D:/work-beta");
    assert!(s_net.push_user_message("network connection timeout on file upload"));
    s_net.push_assistant_message(ChatMessage::assistant(
        "sounds like a proxy misconfiguration, checking next",
    ));
    store.create_session(&s_net).unwrap();
    store
        .save_turn("s-net", &turn(turn_seq()), &s_net.history)
        .unwrap();

    let before = store.search_messages("connection", 100).unwrap();
    assert_eq!(
        before.len(),
        1,
        "[F05b] до второго хода хитов {}",
        before.len()
    );

    // Второй ход: история выросла, сохраняется целиком.
    assert!(s_net.push_user_message("retry worked, the file finally uploaded"));
    s_net.push_assistant_message(ChatMessage::assistant("good, closing the ticket"));
    store
        .save_turn("s-net", &turn(turn_seq()), &s_net.history)
        .unwrap();

    let after = store.search_messages("connection", 100).unwrap();
    assert_eq!(
        after.len(),
        1,
        "[F05b] второй ход породил дубли в поиске: {after:?}"
    );
    let pairs: Vec<(String, u32)> = after
        .iter()
        .map(|h| (h.session_id.clone(), h.ord))
        .collect();
    assert_eq!(
        pairs,
        vec![("s-net".into(), 0)],
        "[F05b] хит сместился: {after:?}"
    );

    // Новое сообщение ищется своим словом, старые никуда не делись.
    let retry = store.search_messages("uploaded", 100).unwrap();
    assert_eq!(
        retry.len(),
        1,
        "[F05b] новое сообщение не ищется: {retry:?}"
    );
    assert_eq!(retry[0].ord, 2);
    let proxy = store.search_messages("proxy", 100).unwrap();
    assert_eq!(
        proxy.len(),
        1,
        "[F05b] старое сообщение потерялось: {proxy:?}"
    );
    assert_eq!(proxy[0].ord, 1);

    // История в базе не задвоилась.
    let ses = store.load_session("s-net").unwrap().unwrap();
    assert_eq!(
        ses.history.len(),
        4,
        "[F05b] история сессии не 4 сообщения: {}",
        ses.history.len()
    );
    println!("[F05b] второй ход: история 4 сообщения, поиск без дублей");
}

/// F06. Пустой поисковый запрос — честная ошибка Invalid, а не «нашлось всё»
/// и не паника; мусор из пробелов и пунктуации — та же честная ошибка.
/// После ошибки поиск продолжает работать.
#[test]
fn probe_f06_empty_query_is_honest_error() {
    let store = SqliteStore::in_memory().unwrap();

    let mut s = user_session("s-live", "D:/work-beta");
    assert!(s.push_user_message("database connection is stable now"));
    store.create_session(&s).unwrap();
    store
        .save_turn("s-live", &turn(turn_seq()), &s.history)
        .unwrap();

    for bad in ["", "   ", " \t\n ", "??? !!! ---"] {
        match store.search_messages(bad, 50) {
            Err(StoreError::Invalid(msg)) => {
                assert!(
                    msg.contains("пуст") || msg.contains("нет ни одного слова"),
                    "[F06] непонятное сообщение об ошибке: {msg}"
                );
                println!("[F06] запрос {bad:?} → Invalid({msg})");
            }
            other => {
                panic!("[F06] запрос {bad:?} не дал честной ошибки: {other:?} (не «нашлось всё»)")
            }
        }
    }

    // После отказов поиск жив и находит реальное слово.
    let hits = store.search_messages("connection", 10).unwrap();
    assert_eq!(hits.len(), 1, "[F06] поиск сломался после ошибок: {hits:?}");
    println!("[F06] после четырёх отказов поиск жив");
}

/// F07. Юзер удаляет старую сессию: её сообщения выпадают из поиска, чужие
/// сессии ищутся как раньше, глобальная память переживает удаление.
#[test]
fn probe_f07_delete_session_drops_its_hits_keeps_memory() {
    let store = SqliteStore::in_memory().unwrap();

    store
        .memory_add(
            MEMORY_SCOPE_GLOBAL,
            None,
            None,
            "любимая сборка — nightly, но релиз стабильнее",
        )
        .unwrap();

    let mut s_db = user_session("s-db", "D:/work-beta");
    assert!(s_db.push_user_message("the connection to the database keeps dropping"));
    s_db.push_assistant_message(ChatMessage::assistant("checking the connection pool"));
    store.create_session(&s_db).unwrap();
    store
        .save_turn("s-db", &turn(turn_seq()), &s_db.history)
        .unwrap();

    let mut s_net = user_session("s-net", "D:/work-beta");
    assert!(s_net.push_user_message("network connection timeout on file upload"));
    store.create_session(&s_net).unwrap();
    store
        .save_turn("s-net", &turn(turn_seq()), &s_net.history)
        .unwrap();

    assert_eq!(
        store.search_messages("connection", 100).unwrap().len(),
        3,
        "[F07] до удаления должно быть 3 хита"
    );

    // Юзер удаляет сессию про сеть.
    store.delete_session("s-net").unwrap();
    assert!(
        store.load_session("s-net").unwrap().is_none(),
        "[F07] сессия не удалилась"
    );

    // Её сообщения выпали из поиска, чужие — остались.
    let hits = store.search_messages("connection", 100).unwrap();
    assert_eq!(hits.len(), 2, "[F07] после удаления хитов {}", hits.len());
    assert!(
        hits.iter().all(|h| h.session_id != "s-net"),
        "[F07] удалённая сессия всё ещё находится: {hits:?}"
    );
    let gone = store.search_messages("timeout", 100).unwrap();
    assert!(
        gone.is_empty(),
        "[F07] сообщение удалённой сессии всё ещё ищется: {gone:?}"
    );
    println!("[F07] удалённая сессия выпала из поиска, живые остались");

    // Глобальная память пережила удаление сессии.
    let globals = store
        .memory_list(MEMORY_SCOPE_GLOBAL, None, None, 10)
        .unwrap();
    assert_eq!(
        globals.len(),
        1,
        "[F07] глобальная память пострадала: {globals:?}"
    );
    assert_eq!(
        globals[0].text,
        "любимая сборка — nightly, но релиз стабильнее"
    );
    println!("[F07] глобальная память пережила удаление сессии");
}

/// F08. Длинный факт на границе капа: юзер вставляет в память целый промпт
/// ~4000 символов — пишется и читается дословно; промпт длиннее капа —
/// честный отказ, а не тихая обрезка.
#[test]
fn probe_f08_long_fact_at_cap_writes_and_reads() {
    let store = SqliteStore::in_memory().unwrap();

    // Реалистичный многострочный промпт ровно 4000 символов.
    let line =
        "Инструкция: держать ответы короткими, по делу, с числами; без повторов и вступлений.\n";
    let mut long = String::new();
    while long.chars().count() + line.chars().count() <= MEMORY_TEXT_MAX_CHARS {
        long.push_str(line);
    }
    let filler = MEMORY_TEXT_MAX_CHARS - long.chars().count();
    long.push_str(&"х".repeat(filler));
    assert_eq!(
        long.chars().count(),
        MEMORY_TEXT_MAX_CHARS,
        "[F08] генератор промпта собрал не то число символов"
    );

    let id = store
        .memory_add(MEMORY_SCOPE_GLOBAL, None, None, &long)
        .unwrap_or_else(|e| panic!("[F08] факт на границе капа не записался: {e}"));
    let list = store
        .memory_list(MEMORY_SCOPE_GLOBAL, None, None, 10)
        .unwrap();
    let saved = list
        .iter()
        .find(|m| m.id == id)
        .expect("[F08] длинный факт не читается");
    assert_eq!(
        saved.text,
        long,
        "[F08] длинный факт исказился: длины {} против {}",
        saved.text.chars().count(),
        long.chars().count()
    );
    println!(
        "[F08] факт {} символов ({} байт) записан и прочитан дословно",
        long.chars().count(),
        long.len()
    );

    // Промпт длиннее капа: честный отказ, не тихая обрезка.
    let mut over = long.clone();
    over.push('!');
    match store.memory_add(MEMORY_SCOPE_GLOBAL, None, None, &over) {
        Err(StoreError::Invalid(msg)) => {
            assert!(msg.contains("длиннее"), "[F08] странное сообщение: {msg}");
            println!("[F08] промпт 4001 симв. → честный Invalid");
        }
        other => panic!("[F08] превышение капа прошло молча: {other:?}"),
    }
    // И ничего не записалось сверх первого факта.
    assert_eq!(
        store
            .memory_list(MEMORY_SCOPE_GLOBAL, None, None, 10)
            .unwrap()
            .len(),
        1
    );
}

/// F09. Двадцать фактов подряд: список стабилен от вызова к вызову, порядок
/// предсказуем (новые первыми, id убывает), limit обрезает с головы.
#[test]
fn probe_f09_twenty_facts_stable_order() {
    let store = SqliteStore::in_memory().unwrap();

    for i in 0..20 {
        store
            .memory_add(
                MEMORY_SCOPE_SESSION,
                Some("s-list"),
                None,
                &format!("fact {i:02}: задача недели номер {i}"),
            )
            .unwrap();
    }
    let list = store
        .memory_list(MEMORY_SCOPE_SESSION, Some("s-list"), None, 100)
        .unwrap();
    assert_eq!(
        list.len(),
        20,
        "[F09] строк в списке {list_len}",
        list_len = list.len()
    );

    // Новые первыми, порядок предсказуем.
    assert_eq!(
        list[0].text, "fact 19: задача недели номер 19",
        "[F09] первой не самая свежая: {:?}",
        list[0]
    );
    assert_eq!(list[19].text, "fact 00: задача недели номер 0");
    let ids: Vec<i64> = list.iter().map(|m| m.id).collect();
    assert!(
        ids.windows(2).all(|w| w[0] > w[1]),
        "[F09] id не убывают (порядок не предсказуем): {ids:?}"
    );

    // Стабильность: повторный вызов даёт тот же список.
    let list2 = store
        .memory_list(MEMORY_SCOPE_SESSION, Some("s-list"), None, 100)
        .unwrap();
    assert_eq!(list, list2, "[F09] второй вызов выдал другой порядок");

    // limit обрезает с головы: первые пять — самые свежие.
    let top5 = store
        .memory_list(MEMORY_SCOPE_SESSION, Some("s-list"), None, 5)
        .unwrap();
    assert_eq!(top5.len(), 5);
    assert_eq!(top5[0].text, "fact 19: задача недели номер 19");
    assert_eq!(top5[4].text, "fact 15: задача недели номер 15");
    println!("[F09] 20 фактов: порядок стабилен, лимит режет с головы");
}

/// F11. Юзер помнит неточно: слово было в размышлениях модели или в выводе
/// инструмента — поиск обязан находить оба (FTS индексирует content и
/// reasoning), сниппет брать оттуда, где совпадение. Неточное слово юзер
/// пишет началом («connect» вместо «connection») и капсом — оба должны
/// находить.
#[test]
fn probe_f11_search_hits_reasoning_and_tool_output() {
    let store = SqliteStore::in_memory().unwrap();

    let mut s_think = user_session("s-think", "D:/work-beta");
    assert!(s_think.push_user_message("проверь пул соединений, что-то течёт"));
    let mut thought = ChatMessage::assistant("готово, проверил настройки");
    thought.reasoning = "думаю, connection pool переполняется из-за утечки курсоров".to_string();
    s_think.push_assistant_message(thought);
    s_think.push_tool_result("c-1", "psql: connection to server closed");
    store.create_session(&s_think).unwrap();
    store
        .save_turn("s-think", &turn(turn_seq()), &s_think.history)
        .unwrap();

    // Слово в reasoning и в tool-выводе: оба сообщения находятся.
    let hits = store.search_messages("connection", 100).unwrap();
    let mut found: Vec<(String, u32)> =
        hits.iter().map(|h| (h.session_id.clone(), h.ord)).collect();
    found.sort();
    assert_eq!(
        found,
        vec![("s-think".into(), 1), ("s-think".into(), 2)],
        "[F11] слово из reasoning/tool найдено не там: {hits:?}"
    );
    for h in &hits {
        assert!(
            h.snippet.contains("[connection]"),
            "[F11] сниппет без подсветки: {:?} (hit {}:{})",
            h.snippet,
            h.session_id,
            h.ord
        );
        assert_eq!(
            h.role,
            if h.ord == 1 { "assistant" } else { "tool" },
            "[F11] роль в хите не совпала с сообщением: {h:?}"
        );
    }
    println!(
        "[F11] слово нашлось в reasoning (ord 1) и tool-выводе (ord 2), сниппеты с подсветкой"
    );

    // Неточное слово: юзер помнит начало — «connect».
    let prefix = store.search_messages("connect", 100).unwrap();
    assert_eq!(
        prefix.len(),
        2,
        "[F11] поиск по началу слова не нашёл оба сообщения: {prefix:?}"
    );

    // Капсом: регистр не должен прятать слово.
    let caps = store.search_messages("CONNECTION", 100).unwrap();
    assert_eq!(
        caps.len(),
        2,
        "[F11] запрос капсом не нашёл сообщения: {caps:?}"
    );
    println!("[F11] префикс и капс находят оба сообщения");
}

/// F10. Приложение закрылось и открылось снова: глобальная и папковая память
/// на месте, сообщения сессии ищутся, история сессии жива.
#[test]
fn probe_f10_memory_survives_reopen() {
    let dir = tmpdir("reopen");
    let path = dir.join("user.db");

    {
        let store = SqliteStore::open(&path).unwrap();
        store
            .memory_add(
                MEMORY_SCOPE_GLOBAL,
                None,
                None,
                "любимый редактор — блокнот, и это не обсуждается",
            )
            .unwrap();
        store
            .memory_add(
                MEMORY_SCOPE_FOLDER,
                None,
                Some("D:/work-beta"),
                "cargo test перед пушем обязателен",
            )
            .unwrap();
        let mut s_live = user_session("s-live", "D:/work-beta");
        assert!(s_live.push_user_message("connection established after restart"));
        store.create_session(&s_live).unwrap();
        store
            .save_turn("s-live", &turn(turn_seq()), &s_live.history)
            .unwrap();
    }

    // Переоткрытие: тот же файл, новое соединение.
    let store = SqliteStore::open(&path).unwrap();
    let globals = store
        .memory_list(MEMORY_SCOPE_GLOBAL, None, None, 10)
        .unwrap();
    assert_eq!(
        globals.len(),
        1,
        "[F10] глобальная память потерялась: {globals:?}"
    );
    assert_eq!(
        globals[0].text,
        "любимый редактор — блокнот, и это не обсуждается"
    );

    let folders = store
        .memory_list(MEMORY_SCOPE_FOLDER, None, Some("D:/work-beta"), 10)
        .unwrap();
    assert_eq!(folders.len(), 1, "[F10] факт папки потерялся: {folders:?}");
    assert_eq!(folders[0].text, "cargo test перед пушем обязателен");

    let hits = store.search_messages("connection", 10).unwrap();
    assert_eq!(
        hits.len(),
        1,
        "[F10] поиск после переоткрытия пуст: {hits:?}"
    );
    assert_eq!(hits[0].session_id, "s-live");
    assert!(
        hits[0].snippet.contains("[connection]"),
        "[F10] сниппет без подсветки: {:?}",
        hits[0].snippet
    );

    let ses = store
        .load_session("s-live")
        .unwrap()
        .expect("[F10] сессия не выжила");
    assert_eq!(ses.title, "t-s-live");
    assert_eq!(ses.history.len(), 1);
    assert_eq!(
        ses.history[0].content,
        "connection established after restart"
    );
    println!("[F10] после переоткрытия: память 2 областей, сессия и поиск живы");
    drop(store);
    std::fs::remove_dir_all(&dir).ok();
}
