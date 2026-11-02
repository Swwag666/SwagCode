/*! probe_store.rs — пробы «Кривые Руки» для SqliteStore (crates/core/src/store.rs).

Зона A: контракт Store на rusqlite. Каждая проба печатает факт прогона.
Запуск: cargo test -p swagcod-core --test probe_store -- --nocapture
Кейсы, вскрывшие реальный дефект, помечаются `#[ignore = "FINDING: ..."]`
после прогона, чтобы набор оставался зелёным.
*/

use std::path::PathBuf;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::time::{Duration, Instant};

use swagcod_core::store::{SqliteStore, Store, StoreError, MEMORY_TEXT_MAX_CHARS};
use swagcod_core::{Session, SessionId, TurnId, TurnRecord};
use swagcod_provider::types::{ChatMessage, Role};

static DIR_SEQ: AtomicUsize = AtomicUsize::new(0);

fn tmpdir(tag: &str) -> PathBuf {
    let n = DIR_SEQ.fetch_add(1, Ordering::SeqCst);
    let d = std::env::temp_dir().join(format!("swagcod-probe-{}-{tag}-{n}", std::process::id()));
    std::fs::create_dir_all(&d).unwrap();
    d
}

fn session_with(id: &str) -> Session {
    let mut s = Session::new(SessionId::new(id), "D:/probe", "probe-model");
    s.title = format!("t-{id}");
    let _ = s.push_user_message("привет");
    s
}

fn turn(n: u64) -> TurnRecord {
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

fn ms_since(t: &Instant) -> f64 {
    t.elapsed().as_secs_f64() * 1000.0
}

/// Зоопарк session_id: юникод, NUL-байты, SQL-инъекция, 10 000 символов.
/// Контракт: id проходит насквозь байт-в-байт, инъекция не тянет чужие строки,
/// ничего не падает и не портится.
#[test]
fn probe_a01_session_id_zoo() {
    let dir = tmpdir("id-zoo");
    let store = SqliteStore::open(&dir.join("zoo.db")).unwrap();

    // Контрольная сессия: кривые id не должны её задеть или подменить.
    let control = session_with("s-1");
    store.create_session(&control).unwrap();

    let weird: Vec<(&str, String)> = vec![
        ("unicode", "ünïcode-🎉-日本語-Ω-ёж".to_string()),
        ("null-byte", "s\0i\0d-\0".to_string()),
        ("sql-inject-or", "' OR 1=1 --".to_string()),
        ("sql-inject-drop", "'; DROP TABLE sessions; --".to_string()),
        ("10k-chars", "x".repeat(10_000)),
    ];

    for (tag, id) in weird {
        let t0 = Instant::now();
        let ses = session_with(&id);
        let r = store.create_session(&ses);
        assert!(r.is_ok(), "[A01/{tag}] create_session упал: {r:?}");
        let r = store.save_turn(&id, &turn(1), &ses.history);
        assert!(r.is_ok(), "[A01/{tag}] save_turn упал: {r:?}");

        let loaded = store
            .load_session(&id)
            .unwrap()
            .unwrap_or_else(|| panic!("[A01/{tag}] load_session не нашёл id (len {})", id.len()));
        assert_eq!(
            loaded.id.as_str(),
            id,
            "[A01/{tag}] id не совпал байт-в-байт (len {})",
            id.len()
        );
        assert_eq!(
            loaded.history, ses.history,
            "[A01/{tag}] история не совпала"
        );

        // Инъекционный id не должен ни подменить контроль, ни вытащить лишнее.
        let c = store.load_session("s-1").unwrap().expect("контроль жив");
        assert_eq!(
            c.id.as_str(),
            "s-1",
            "[A01/{tag}] инъекция достала чужую сессию"
        );
        assert_eq!(
            store.load_all().unwrap().len(),
            2,
            "[A01/{tag}] load_all насчитал лишнего"
        );

        // Удаление снимает только свою сессию.
        store.delete_session(&id).unwrap();
        assert!(
            store.load_session(&id).unwrap().is_none(),
            "[A01/{tag}] delete_session не удалил"
        );
        assert!(
            store.load_session("s-1").unwrap().is_some(),
            "[A01/{tag}] удаление задело чужую сессию"
        );

        println!(
            "[A01/{tag}] ok: id {} байт, roundtrip байт-в-байт, {:.1} мс",
            id.len(),
            ms_since(&t0)
        );
    }

    assert_eq!(store.load_all().unwrap().len(), 1);
    drop(store);
    std::fs::remove_dir_all(&dir).ok();
}

/// save_turn в несуществующую сессию: FK violation должен выйти громкой
/// ошибкой Db, транзакция откатиться, стор остаться рабочим.
/// (Сюда же относится «session_id в messages»: оба FK ссылаются на тот же
/// несуществующий id, через публичный API ход messages отдельно не задеть —
/// turn вставляется первым и падает первым.)
#[test]
fn probe_a02_fk_missing_session() {
    let dir = tmpdir("fk");
    let store = SqliteStore::open(&dir.join("fk.db")).unwrap();

    let t0 = Instant::now();
    let r = store.save_turn("ghost-session", &turn(1), &[ChatMessage::user("x")]);
    let dbg = format!("{r:?}");
    println!(
        "[A02] save_turn в несуществующую сессию за {:.1} мс → {dbg}",
        ms_since(&t0)
    );
    assert!(r.is_err(), "[A02] FK-нарушение прошло молча: {dbg}");
    assert!(
        dbg.contains("FOREIGN KEY") || format!("{r:#?}").contains("FOREIGN KEY"),
        "[A02] ошибка не похожа на FK: {dbg}"
    );

    // Стор не отравлен: следующая транзакция проходит целиком.
    let ses = session_with("s-real");
    store.create_session(&ses).unwrap();
    store.save_turn("s-real", &turn(1), &ses.history).unwrap();
    let loaded = store.load_session("s-real").unwrap().expect("живая сессия");
    assert_eq!(loaded.history, ses.history);
    assert_eq!(loaded.turns.len(), 1);
    println!("[A02] после FK-ошибки стор жив, транзакция откатилась");
    drop(store);
    std::fs::remove_dir_all(&dir).ok();
}

/// save_turn с failure 1 МБ и content 1 МБ: без обрезки, без деградации.
#[test]
fn probe_a03_save_turn_failure_1mb() {
    let dir = tmpdir("fail1mb");
    let store = SqliteStore::open(&dir.join("f.db")).unwrap();
    store.create_session(&session_with("s-big")).unwrap();

    let mut t = turn(1);
    t.failure = Some("F".repeat(1_048_576));
    t.content = "C".repeat(1_048_576);

    let t0 = Instant::now();
    store.save_turn("s-big", &t, &[]).unwrap();
    let write_ms = ms_since(&t0);

    let t1 = Instant::now();
    let loaded = store.load_session("s-big").unwrap().unwrap();
    let read_ms = ms_since(&t1);

    println!("[A03] failure 1 МБ + content 1 МБ: write {write_ms:.0} мс, read {read_ms:.0} мс");
    assert_eq!(loaded.turns.len(), 1);
    assert_eq!(loaded.turns[0], t, "[A03] 1 МБ поля не совпали байт-в-байт");
    drop(store);
    std::fs::remove_dir_all(&dir).ok();
}

/// История сообщений: пустой content и 5 МБ content (мультибайтный UTF-8),
/// плюс 1 МБ reasoning. Roundtrip байт-в-байт.
#[test]
fn probe_a04_messages_empty_and_5mb() {
    let dir = tmpdir("msg5mb");
    let store = SqliteStore::open(&dir.join("m.db")).unwrap();
    store.create_session(&session_with("s-msg")).unwrap();

    let big = format!("{}{}", "a".repeat(2_500_000), "ы".repeat(1_250_000));
    assert_eq!(big.len(), 5_000_000, "ровно 5 МБ UTF-8 байт");

    let history = vec![
        ChatMessage::user(""),
        ChatMessage::user(big),
        ChatMessage {
            role: Role::Assistant,
            content: "ок".to_string(),
            reasoning: "R".repeat(1_000_000),
            tool_calls: vec![],
            tool_call_id: None,
        },
    ];

    let t0 = Instant::now();
    store.save_turn("s-msg", &turn(1), &history).unwrap();
    let write_ms = ms_since(&t0);

    let t1 = Instant::now();
    let loaded = store.load_session("s-msg").unwrap().unwrap();
    let read_ms = ms_since(&t1);

    println!("[A04] empty + 5 МБ + reasoning 1 МБ: write {write_ms:.0} мс, read {read_ms:.0} мс");
    assert_eq!(
        loaded.history, history,
        "[A04] история не совпала байт-в-байт"
    );
    assert_eq!(loaded.history[1].content.len(), 5_000_000);
    drop(store);
    std::fs::remove_dir_all(&dir).ok();
}

/// Кап памяти 4000 символов: ровно на границе, за ней, мультибайт, переводы
/// строк, пробельный мусор. Ошибка — громкая Invalid, не тихая обрезка.
#[test]
fn probe_a05_memory_boundary_4000() {
    let dir = tmpdir("mem4000");
    let store = SqliteStore::open(&dir.join("m.db")).unwrap();

    // Ровно 4000 символов мультибайтных (8000 байт): кап в символах.
    let exact = "м".repeat(MEMORY_TEXT_MAX_CHARS);
    let id = store
        .memory_add("global", None, None, &exact)
        .expect("[A05] ровно 4000 символов обязано проходить");
    let list = store.memory_list("global", None, None, 10).unwrap();
    let saved = list
        .iter()
        .find(|m| m.id == id)
        .expect("[A05] строка не найдена");
    assert_eq!(
        saved.text, exact,
        "[A05] текст на границе не совпал байт-в-байт"
    );
    println!(
        "[A05] 4000 симв. ({} байт) — записан и прочитан дословно",
        exact.len()
    );

    // 4001 → Invalid, не тихая обрезка.
    let over = "м".repeat(MEMORY_TEXT_MAX_CHARS + 1);
    match store.memory_add("global", None, None, &over) {
        Err(StoreError::Invalid(msg)) => {
            println!("[A05] 4001 симв. → Invalid({msg})");
        }
        other => panic!("[A05] 4001 должен давать Invalid, получили {other:?}"),
    }

    // 4000 эмодзи = 16 000 байт: кап считает символы, не байты.
    let emoji = "🎉".repeat(MEMORY_TEXT_MAX_CHARS);
    let id2 = store
        .memory_add("global", None, None, &emoji)
        .expect("[A05] 4000 эмодзи (кап в символах) обязано проходить");
    let list2 = store.memory_list("global", None, None, 10).unwrap();
    let saved2 = list2
        .iter()
        .find(|m| m.id == id2)
        .expect("[A05] эмодзи не найден");
    assert_eq!(saved2.text, emoji);
    println!(
        "[A05] 4000 эмодзи ({} байт) — прошёл, кап действительно в символах",
        emoji.len()
    );

    // Внутренние переводы строк сохраняются дословно.
    let nl = "шаг1\nшаг2\n\nхвост".to_string();
    let id3 = store.memory_add("global", None, None, &nl).unwrap();
    let list3 = store.memory_list("global", None, None, 10).unwrap();
    let saved3 = list3
        .iter()
        .find(|m| m.id == id3)
        .expect("[A05] многострочный не найден");
    assert_eq!(
        saved3.text, nl,
        "[A05] переводы строк не сохранились дословно"
    );
    println!("[A05] внутренние \\n сохранены дословно");

    // Пробельный мусор → Invalid.
    match store.memory_add("global", None, None, "   \n\t  ") {
        Err(StoreError::Invalid(_)) => println!("[A05] пробельная строка → Invalid"),
        other => panic!("[A05] пробельная строка должна давать Invalid, получили {other:?}"),
    }
    drop(store);
    std::fs::remove_dir_all(&dir).ok();
}

/// prefs с кривыми значениями. &str в Rust всегда валидный UTF-8, так что
/// «не-UTF8» через публичный API невозможен типами; давим NUL, управляющие
/// символы, replacement char, эмодзи и 10 000 символов — roundtrip.
#[test]
fn probe_a06_prefs_weird_values() {
    let dir = tmpdir("prefs");
    let store = SqliteStore::open(&dir.join("p.db")).unwrap();

    let cases: Vec<(&str, String, String)> = vec![
        ("nul", "k\0ey".to_string(), "v\0\0al".to_string()),
        (
            "ctrl",
            "ctrl\u{01}\u{1f}".to_string(),
            "val\u{FFFD}🎉".to_string(),
        ),
        ("empty-key", String::new(), "empty-key-value".to_string()),
        ("unicode", "ключ-🎉".to_string(), "值-🎉".to_string()),
        ("10k", "x".repeat(10_000), "y".repeat(10_000)),
    ];

    for (tag, k, v) in cases {
        store.set_pref(&k, &v).expect("[A06/{tag}] set_pref упал");
        let got = store.get_pref(&k).expect("[A06/{tag}] get_pref упал");
        assert_eq!(
            got.as_deref(),
            Some(v.as_str()),
            "[A06/{tag}] значение не совпало байт-в-байт"
        );
        println!(
            "[A06/{tag}] ok: key {} байт, value {} байт, roundtrip дословно",
            k.len(),
            v.len()
        );
    }

    // Отсутствующий ключ — тихое None, не ошибка.
    assert_eq!(store.get_pref("нет-такого").unwrap(), None);
    println!("[A06] отсутствующий ключ → Ok(None)");
    drop(store);
    std::fs::remove_dir_all(&dir).ok();
}

/// metrics: NaN, ±inf, отрицательные. NaN не имеет права пройти молча
/// (SQLite хранит NaN как NULL → NOT NULL); inf обязан вернуться точным.
#[test]
fn probe_a07_metrics_nan_inf_negative() {
    let dir = tmpdir("metrics");
    let store = SqliteStore::open(&dir.join("mt.db")).unwrap();

    // NaN: либо громкая ошибка, либо точный roundtrip. Тишина/подмена — брак.
    match store.metrics_insert(1, "nan", f64::NAN) {
        Err(e) => println!("[A07] NaN → Err({e})"),
        Ok(()) => {
            let q = store.metrics_query("nan", 0, 10).unwrap();
            let vals: Vec<f64> = q.iter().map(|(_, v)| *v).collect();
            println!("[A07] NaN записан молча, прочитано {vals:?}");
            assert!(
                vals.iter().any(|v| v.is_nan()),
                "[A07] FINDING: NaN записался, но читается {vals:?}"
            );
        }
    }

    // +inf / -inf: точный roundtrip.
    store.metrics_insert(2, "inf", f64::INFINITY).unwrap();
    store.metrics_insert(3, "inf", f64::NEG_INFINITY).unwrap();
    let q = store.metrics_query("inf", 0, 10).unwrap();
    println!("[A07] ±inf roundtrip → {q:?}");
    assert_eq!(q, vec![(2, f64::INFINITY), (3, f64::NEG_INFINITY)]);

    // Отрицательные: точный roundtrip.
    store.metrics_insert(4, "neg", -123.5).unwrap();
    let q = store.metrics_query("neg", 0, 10).unwrap();
    assert_eq!(q, vec![(4, -123.5)]);
    println!("[A07] -123.5 roundtrip точный");
    drop(store);
    std::fs::remove_dir_all(&dir).ok();
}

/// Удаление сессии с подвешенными turns/messages: каскад обязан снять всё,
/// включая тень FTS-индекса (messages_fts_docsize) — иначе индекс растёт
/// мусором, а поиск молча опирается на JOIN.
#[test]
fn probe_a08_delete_session_cascade_and_fts() {
    let dir = tmpdir("cascade");
    let path = dir.join("c.db");
    let store = SqliteStore::open(&path).unwrap();

    let mut ses = session_with("s-1");
    ses.history.clear();
    let _ = ses.push_user_message("зеброморф уникальноеслово");
    ses.push_assistant_message(ChatMessage::assistant("ответ без искомого слова"));
    store.create_session(&ses).unwrap();
    store.save_turn("s-1", &turn(1), &ses.history).unwrap();

    let hits = store.search_messages("зеброморф", 10).unwrap();
    assert_eq!(hits.len(), 1, "[A08] до удаления поиск не видит сообщение");
    println!("[A08] до удаления: поиск находит {} хит", hits.len());

    store.delete_session("s-1").unwrap();
    assert!(
        store.load_session("s-1").unwrap().is_none(),
        "[A08] сессия не удалилась"
    );
    let hits2 = store.search_messages("зеброморф", 10).unwrap();
    assert!(
        hits2.is_empty(),
        "[A08] после удаления поиск всё ещё находит хиты"
    );
    drop(store);

    // Тень FTS: доки в docsize обязаны уйти вместе с messages.
    let raw = rusqlite::Connection::open(&path).unwrap();
    let msgs: i64 = raw
        .query_row("SELECT COUNT(*) FROM messages", [], |r| r.get(0))
        .unwrap();
    let turns: i64 = raw
        .query_row("SELECT COUNT(*) FROM turns", [], |r| r.get(0))
        .unwrap();
    let docsz: i64 = raw
        .query_row("SELECT COUNT(*) FROM messages_fts_docsize", [], |r| {
            r.get(0)
        })
        .unwrap();
    let integrity: String = raw
        .query_row("PRAGMA integrity_check", [], |r| r.get(0))
        .unwrap();
    println!("[A08] после каскада: messages={msgs}, turns={turns}, fts_docsize={docsz}, integrity={integrity}");

    assert_eq!((msgs, turns), (0, 0), "[A08] каскад оставил хвосты");
    assert_eq!(
        docsz, 0,
        "[A08] FINDING: каскад не вычистил FTS-индекс — messages пуст, а в messages_fts_docsize осталось {docsz} строк"
    );
    assert_eq!(integrity, "ok");
    drop(raw);
    std::fs::remove_dir_all(&dir).ok();
}

/// Два SqliteStore на одном файле одновременно: видимость коммитов (WAL) и
/// запись под чужим write-lock (ожидание — громкая ошибка, не зависание).
#[test]
fn probe_a09_two_stores_same_file() {
    let dir = tmpdir("two");
    let path = dir.join("two.db");
    let a = SqliteStore::open(&path).unwrap();
    let b = SqliteStore::open(&path).unwrap();

    a.create_session(&session_with("sa")).unwrap();
    b.create_session(&session_with("sb")).unwrap();
    let seen = b.load_all().unwrap().len();
    println!("[A09] второй стор видит {seen} сессий из 2 (WAL)");
    assert_eq!(seen, 2, "[A09] второй стор не видит коммитов первого");

    a.set_pref("who", "A").unwrap();
    assert_eq!(b.get_pref("who").unwrap().as_deref(), Some("A"));
    println!("[A09] кросс-видимость prefs ок");

    // Чужой write-lock: держим пишущую транзакцию сырым соединением.
    let guard = rusqlite::Connection::open(&path).unwrap();
    guard.execute_batch("BEGIN IMMEDIATE").unwrap();
    guard
        .execute("INSERT INTO prefs(key, value) VALUES ('guard', '1')", [])
        .unwrap();

    let (sx, rx) = std::sync::mpsc::channel::<String>();
    let t0 = Instant::now();
    std::thread::spawn(move || {
        let r = b.set_pref("locked", "x");
        let _ = sx.send(format!("{r:?}"));
    });
    match rx.recv_timeout(Duration::from_secs(10)) {
        Ok(report) => {
            let elapsed = ms_since(&t0);
            println!("[A09] запись под чужим write-lock: {report} за {elapsed:.0} мс");
            assert!(
                report.starts_with("Err"),
                "[A09] под блокировкой запись прошла: {report}"
            );
            assert!(
                elapsed < 10_000.0,
                "[A09] FINDING: запись под блокировкой зависла на {elapsed:.0} мс"
            );
        }
        Err(_) => panic!("[A09] FINDING: set_pref под чужим write-lock завис дольше 10 с"),
    }
    guard.execute_batch("ROLLBACK").unwrap();
    drop(guard);

    // После снятия блокировки запись проходит.
    a.set_pref("after", "ok").unwrap();
    assert_eq!(a.get_pref("after").unwrap().as_deref(), Some("ok"));
    println!("[A09] после снятия lock запись проходит");
    drop(a);
    std::fs::remove_dir_all(&dir).ok();
}
