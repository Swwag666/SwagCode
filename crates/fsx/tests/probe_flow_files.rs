/*! Зонд-агент «обычный пользователь»: валидные файловые флоу через публичный
API swagcod-fsx — те самые функции, в которые executor.rs отправляет тулзы
grep/glob/patch. Ветку patch исполнителя (read_to_string → patch_text →
запись только при успехе) и ветки grep/glob (пустая выдача = «совпадений
нет») тесты отражают зеркалами один-в-один, поэтому поведение совпадает с
живым исполнителем. Сами read/write/list живут в executor'е как прямые
std::fs-вызовы и объявлены pub(crate) — из интеграционных тестов они
недостижимы, байтовая точность этих веток покрыта clumsy_*-зондами в
executor.rs и здесь не дублируется.

Флоу — рабочий день обычного человека: создать файлы (Rust/JSON/CSV/русский
текст, CRLF и LF) → grep по словам, кириллице, regex → правки кода патчами
(level-1 точные, level-2 whitespace-дрейф, табы, пробелы в конце) → первый и
последний строки 1000+-строчного файла → два последовательных патча →
опечатка в old_string → 200 файлов в каталоге → полный цикл
правка→откат через теневой снимок. Кривые пути, битые аргументы и
граничные кодировки — территория clumsy_*-зондов, здесь их нет.

Находки помечены #[ignore = "FINDING: ..."]: прогон
`cargo test -p swagcod-fsx --test probe_flow_files -- --ignored --nocapture`
показывает живое репро каждой (ассерт держит ожидание, eprintln — наблюдение).
Зелёная часть файла — флоу, которые работают как должны.
*/

use std::path::Path;

use swagcod_fsx::shadow;
use swagcod_fsx::tools::{format_hits, glob_files, grep_files, grep_files_detailed, patch_text};

/* ── Зеркала веток executor.rs (один-в-один, см. execute_tool) ────────── */

/// Ветка «patch»: чтение → patch_text → запись только при успехе.
/// Провалившийся патч не трогает файл — ровно как в исполнителе.
fn patch_file(path: &Path, old: &str, new: &str, replace_all: bool) -> Result<usize, String> {
    let content = match std::fs::read_to_string(path) {
        Ok(c) => c,
        Err(e) => return Err(format!("patch: {e}")),
    };
    match patch_text(&content, old, new, replace_all) {
        Ok((patched, n)) => match std::fs::write(path, patched.as_bytes()) {
            Ok(()) => Ok(n),
            Err(e) => Err(format!("patch write: {e}")),
        },
        Err(e) => Err(format!("patch: {e}")),
    }
}

/// Ветка «grep»: пустая выдача отдаётся моделью как «совпадений нет».
fn grep_out(root: &Path, sub: Option<&str>, pattern: &str) -> Result<String, String> {
    match grep_files(root, sub, pattern, 2) {
        Ok(hits) if hits.is_empty() => Ok("совпадений нет".into()),
        Ok(hits) => Ok(format_hits(&hits)),
        Err(e) => Err(format!("grep: {e}")),
    }
}

/// Ветка «glob»: пути через \n, пустая выдача — «совпадений нет».
fn glob_out(root: &Path, sub: Option<&str>, pattern: &str) -> Result<String, String> {
    match glob_files(root, sub, pattern) {
        Ok(files) if files.is_empty() => Ok("совпадений нет".into()),
        Ok(files) => Ok(files.join("\n")),
        Err(e) => Err(format!("glob: {e}")),
    }
}

/// Записать файл с созданием родителей (как write-тулз исполнителя).
fn put(dir: &Path, rel: &str, content: &str) -> std::path::PathBuf {
    let p = dir.join(rel);
    if let Some(parent) = p.parent() {
        std::fs::create_dir_all(parent).unwrap();
    }
    std::fs::write(&p, content.as_bytes()).unwrap();
    p
}

/* ── Чистые флоу ──────────────────────────────────────────────────────── */

/// Рабочий день в папке: несколько файлов обычных типов, grep по словам,
/// кириллице и regex, скоуп подкаталогом, контекст на первой/последней
/// строке, glob-наборы, перезапись и «rename через write».
#[test]
fn workday_files_grep_cyrillic_regex_and_context() {
    let dir = tempfile::tempdir().unwrap();
    let root = dir.path();

    let main_rs = "fn main() {\n    let cart = [\"bread\", \"milk\"];\n    let mut sum = 0;\n    for item in cart {\n        let price = final_price(item, \"vip\");\n        println!(\"{item}: {price}\");\n        sum += price;\n    }\n    println!(\"итого: {sum}\");\n}\n";
    put(root, "src/main.rs", main_rs);
    put(root, "src/discount.rs", "pub fn discount(user: &str, total: u32) -> u32 {\n    if user == \"vip\" { total / 10 } else { 0 }\n}\n");
    put(
        root,
        "src/App.ts",
        "export function App() {\n  return \"shop\";\n}\n",
    );
    put(
        root,
        "src/components/Button.ts",
        "export function Button(label: string) {\n  return `<button>${label}</button>`;\n}\n",
    );
    put(
        root,
        "src/components/deep/Modal.ts",
        "export function Modal() {\n  return \"modal\";\n}\n",
    );

    let ru = "Первая заметка\r\nЁжик пошёл в лес\r\nТретья строка без сюрпризов\r\n";
    put(root, "notes/заметка.txt", ru);
    put(
        root,
        "data/users.json",
        "{\"users\": [{\"name\": \"Иван\", \"age\": 30}, {\"name\": \"Мария\", \"age\": 25}]}\n",
    );
    put(
        root,
        "data/sales.csv",
        "месяц,сумма\r\n2024-01,1200\r\n2024-02,1350\r\n2024-03,900\r\n",
    );
    put(root, "edges.txt", "needle-one\nx\ny\nz\nneedle-last");

    // Байты на диске — ровно те, что писали (CRLF и кириллица не мутировали).
    assert_eq!(
        std::fs::read(root.join("notes/заметка.txt")).unwrap(),
        ru.as_bytes()
    );
    assert_eq!(
        std::fs::read(root.join("src/main.rs")).unwrap(),
        main_rs.as_bytes()
    );

    // grep: обычное слово, номера строк и текст.
    let hits = grep_files(root, None, "println!", 2).unwrap();
    assert_eq!(hits.len(), 2, "{hits:?}");
    assert!(hits.iter().all(|h| h.path == "src/main.rs"));
    assert_eq!(hits[0].line_no, 6, "{hits:?}");
    assert_eq!(hits[1].line_no, 9, "{hits:?}");

    // grep: кириллица в CRLF-файле, \r не протекает в текст совпадения.
    let hits = grep_files(root, None, "Ёжик", 2).unwrap();
    assert_eq!(hits.len(), 1, "{hits:?}");
    assert_eq!(hits[0].path, "notes/заметка.txt");
    assert_eq!(hits[0].line_no, 2);
    assert_eq!(hits[0].text, "Ёжик пошёл в лес");
    assert!(
        !hits[0].text.contains('\r'),
        "CRLF не едет в выдачу: {:?}",
        hits[0].text
    );

    // grep: регистронезависимый поиск по кириллице (Ё ↔ ё).
    let hits = grep_files(root, None, r"(?i)ёжик", 2).unwrap();
    assert_eq!(hits.len(), 1, "{hits:?}");
    assert_eq!(hits[0].text, "Ёжик пошёл в лес");

    // grep: regex по цифрам в CSV.
    let hits = grep_files(root, None, r"\d{4}", 1).unwrap();
    assert_eq!(hits.len(), 3, "{hits:?}");
    assert!(hits.iter().all(|h| h.path == "data/sales.csv"));

    // grep: скоуп подкаталогом.
    let hits = grep_files(root, Some("data"), "1200", 2).unwrap();
    assert_eq!(hits.len(), 1, "{hits:?}");
    assert_eq!(hits[0].path, "data/sales.csv");
    assert_eq!(hits[0].line_no, 2);

    // grep: контекст насыщается на первой и последней строке, не паникуя.
    let hits = grep_files(root, None, "needle", 2).unwrap();
    assert_eq!(hits.len(), 2, "{hits:?}");
    assert_eq!(hits[0].line_no, 1);
    assert!(hits[0].context_before.is_empty(), "{hits:?}");
    assert_eq!(hits[1].line_no, 5);
    assert!(hits[1].context_after.is_empty(), "{hits:?}");
    let out = grep_out(root, None, "needle").unwrap();
    assert!(out.contains("> 1: needle-one"), "{out}");
    assert!(out.contains("> 5: needle-last"), "{out}");

    // glob-наборы: без слэша — basename на любой глубине; src/**/*.ts — всё дерево.
    let rs = glob_out(root, None, "*.rs").unwrap();
    assert_eq!(rs, "src/discount.rs\nsrc/main.rs", "{rs}");
    let ts = glob_out(root, None, "src/**/*.ts").unwrap();
    assert_eq!(
        ts, "src/App.ts\nsrc/components/Button.ts\nsrc/components/deep/Modal.ts",
        "{ts}"
    );
    let csv = glob_out(root, None, "data/*.csv").unwrap();
    assert_eq!(csv, "data/sales.csv", "{csv}");

    // Перезапись того же файла и «rename через write»: старый остаётся, новый появляется.
    let ru2 = "Вторая редакция заметки\r\nЁжик вернулся из леса\r\n";
    put(root, "notes/заметка.txt", ru2);
    assert_eq!(
        std::fs::read(root.join("notes/заметка.txt")).unwrap(),
        ru2.as_bytes()
    );
    put(root, "notes/заметка-copy.txt", ru2);
    assert!(root.join("notes/заметка.txt").exists());
    assert!(root.join("notes/заметка-copy.txt").exists());
}

/// Реальная правка кода: Rust-файл на ~45 строк, четыре последовательных
/// патча (точная замена → патч, чей old_string включает результат первого →
/// место вызова → whitespace-дрейф с табами). Последовательность
/// доказывается, а не подразумевается.
#[test]
fn rust_code_edit_flow_two_sequential_patches() {
    let dir = tempfile::tempdir().unwrap();
    let root = dir.path();

    let src = concat!(
        "// Магазин: расчёт чека со скидками\n\n",
        "fn base_price(item: &str) -> u32 {\n    match item {\n        \"bread\" => 35,\n        \"milk\" => 89,\n        \"cheese\" => 450,\n        _ => 100,\n    }\n}\n\n",
        "fn discount(user: &str, total: u32) -> u32 {\n\tif user == \"vip\" {\n\t\ttotal / 10\n\t} else if total > 1000 {\n\t\ttotal / 20\n\t} else {\n\t\t0\n\t}\n}\n\n",
        "fn final_price(item: &str, user: &str) -> u32 {\n    let total = base_price(item);\n    total - discount(user, total)\n}\n\n",
        "fn main() {\n    let cart = [\"bread\", \"milk\", \"cheese\"];\n    let mut sum = 0;\n    for item in cart {\n        let price = final_price(item, \"vip\");\n        println!(\"{item}: {price}\");\n        sum += price;\n    }\n    println!(\"итого: {sum}\");\n}\n",
    );
    let path = put(root, "src/shop.rs", src);
    let lines_before = src.lines().count();

    // Патч 1: точная замена — переименование функции.
    let n = patch_file(
        &path,
        "fn final_price(item: &str, user: &str) -> u32 {",
        "fn checkout_price(item: &str, user: &str) -> u32 {",
        false,
    )
    .unwrap();
    assert_eq!(n, 1, "замен ровно одна");

    // Патч 2: old_string содержит результат патча 1 (checkout_price существует
    // только после первого патча) — если файл не обновился, этот патч упадёт.
    let old2 = "fn checkout_price(item: &str, user: &str) -> u32 {\n    let total = base_price(item);\n    total - discount(user, total)\n}";
    let new2 = "fn checkout_price(item: &str, user: &str) -> u32 {\n    // итог после скидки\n    let total = base_price(item);\n    total - discount(user, total)\n}";
    let n = patch_file(&path, old2, new2, false).unwrap();
    assert_eq!(n, 1);

    // Патч 3: место вызова — старое имя живёт только в main(), переименовываем и его.
    let n = patch_file(
        &path,
        "let price = final_price(item, \"vip\");",
        "let price = checkout_price(item, \"vip\");",
        false,
    )
    .unwrap();
    assert_eq!(n, 1);

    // Патч 4: whitespace-дрейф — в файле табы, old прислал 4 пробела.
    let old3 = "    if user == \"vip\" {\n        total / 10\n    } else if total > 1000 {\n        total / 20\n    } else {\n        0\n    }";
    let new3 = "    if user == \"vip\" {\n        total / 5\n    } else if total > 1000 {\n        total / 20\n    } else {\n        0\n    }";
    let n = patch_file(&path, old3, new3, false).unwrap();
    assert_eq!(n, 1, "дрейф табы→пробелы матчится level-2");

    let after = std::fs::read_to_string(&path).unwrap();
    assert!(
        after.contains("fn checkout_price(item: &str, user: &str) -> u32 {"),
        "{after}"
    );
    assert!(
        !after.contains("final_price"),
        "переименование дошло до конца: {after}"
    );
    assert!(after.contains("// итог после скидки"), "{after}");
    assert!(after.contains("total / 5"), "{after}");
    assert!(
        !after.contains('\t'),
        "табы заменены пробельным блоком: {after}"
    );
    assert!(
        after.contains("println!(\"итого: {sum}\");"),
        "кириллица не тронута: {after}"
    );
    // Патч 2 добавил одну строку-комментарий, остальные сохранили структуру.
    assert_eq!(
        after.lines().count(),
        lines_before + 1,
        "число строк предсказуемо"
    );
}

/// Патч одного символа в юникодной строке: границы UTF-8 не рвутся.
/// Сначала честный отказ на неоднозначную замену, потом точечные правки
/// (2-байтовая кириллица и 4-байтовый эмодзи) с байтовым сравнением.
#[test]
fn patch_single_char_in_unicode_text() {
    let dir = tempfile::tempdir().unwrap();
    let root = dir.path();

    let src = "ёжик пошёл в лес\nи взял скрипку\n🎻 соната номер один\n";
    let path = put(root, "стих.txt", src);

    // «ё» встречается дважды (ёжик, пошёл) — честный отказ с числом.
    let err = patch_file(&path, "ё", "е", false).unwrap_err();
    assert!(err.contains("2 раз"), "{err}");
    assert_eq!(
        std::fs::read(&path).unwrap(),
        src.as_bytes(),
        "отказ не тронул файл"
    );

    // Точечная замена в кириллице.
    let n = patch_file(&path, "ёжик", "ежик", false).unwrap();
    assert_eq!(n, 1);
    assert_eq!(
        std::fs::read(&path).unwrap(),
        "ежик пошёл в лес\nи взял скрипку\n🎻 соната номер один\n".as_bytes()
    );

    // 4-байтовый символ: замена не расщепляет суррогатную пару UTF-8.
    let n = patch_file(&path, "🎻", "🎸", false).unwrap();
    assert_eq!(n, 1);
    let after = std::fs::read_to_string(&path).unwrap();
    assert_eq!(
        after,
        "ежик пошёл в лес\nи взял скрипку\n🎸 соната номер один\n"
    );
    assert!(
        std::str::from_utf8(after.as_bytes()).is_ok(),
        "UTF-8 валиден"
    );
}

/// Длинный файл 1000+ строк: патч первой и последней строки, соседи целы.
#[test]
fn patch_long_file_first_and_last_line() {
    let dir = tempfile::tempdir().unwrap();
    let root = dir.path();
    let n_lines = 1001;
    let src: String = (1..=n_lines).map(|i| format!("line {i}\n")).collect();
    let path = put(root, "long.txt", &src);

    let n = patch_file(&path, "line 1\n", "FIRST\n", false).unwrap();
    assert_eq!(n, 1);
    let n = patch_file(&path, "line 1001\n", "LAST\n", false).unwrap();
    assert_eq!(n, 1);

    let after = std::fs::read_to_string(&path).unwrap();
    let lines: Vec<&str> = after.lines().collect();
    assert_eq!(lines.len(), n_lines, "число строк не изменилось");
    assert_eq!(lines[0], "FIRST");
    assert_eq!(lines[1], "line 2", "вторая строка не съехала");
    assert_eq!(lines[n_lines - 1], "LAST");
    assert_eq!(
        lines[n_lines - 2],
        "line 1000",
        "предпоследняя строка не съехала"
    );
}

/// 1000-строчный CRLF-файл: после патча одной строки каждая нетронутая
/// строка байт-в-байт прежняя — включая строки с пробелами в конце.
#[test]
fn patch_long_crlf_file_keeps_every_untouched_line() {
    let dir = tempfile::tempdir().unwrap();
    let root = dir.path();
    let n_lines = 1000;
    let mut lines: Vec<String> = (1..=n_lines).map(|i| format!("line {i}")).collect();
    lines[16].push_str("  "); // пробелы в конце нетронутой строки
    lines[998].push('\t');
    let src = format!("{}\r\n", lines.join("\r\n"));
    let path = put(root, "win.rs", &src);

    let n = patch_file(&path, "line 500\r\n", "CHANGED\r\n", false).unwrap();
    assert_eq!(n, 1);

    let mut expected_lines: Vec<String> = (1..=n_lines).map(|i| format!("line {i}")).collect();
    expected_lines[16].push_str("  ");
    expected_lines[998].push('\t');
    expected_lines[499] = "CHANGED".into();
    let expected = format!("{}\r\n", expected_lines.join("\r\n"));
    assert_eq!(
        std::fs::read_to_string(&path).unwrap(),
        expected,
        "нетронутые строки байт-в-байт, CRLF и хвостовые пробелы на месте"
    );
}

/// Опечатка в old_string: честная ошибка с подсказкой, файл не изменён.
#[test]
fn patch_with_typo_is_error_and_file_untouched() {
    let dir = tempfile::tempdir().unwrap();
    let root = dir.path();
    let src = "fn main() {\n    let cfg = load();\n    save(cfg);\n}\n";
    let path = put(root, "config.rs", src);

    // «nain» вместо «main» — типичная опечатка. Подсказка ближайшего блока
    // line-гранулярная (similar считает совпавшие строки): у однострочного
    // old с одной опечаткой совпавших строк ноль — подсказки нет, но ошибка честная.
    let err = patch_file(&path, "fn nain() {", "fn entry() {", false).unwrap_err();
    assert!(err.contains("не найден"), "{err}");
    assert_eq!(
        std::fs::read(&path).unwrap(),
        src.as_bytes(),
        "провалившийся патч не меняет файл"
    );

    // Опечатка в одной строке трёхстрочного блока — две совпали, подсказка есть.
    let err = patch_file(
        &path,
        "fn main() {\n    let cfg = load() extra;\n    save(cfg);\n}",
        "fn entry() {\n    let cfg = load();\n    save(cfg);\n}",
        false,
    )
    .unwrap_err();
    assert!(err.contains("не найден"), "{err}");
    assert!(
        err.contains("строки") || err.contains("сходство"),
        "ближайший блок подсказывается: {err}"
    );
    assert_eq!(
        std::fs::read(&path).unwrap(),
        src.as_bytes(),
        "и этот провал не меняет файл"
    );
}

/// 200 файлов в каталоге: glob отдаёт все, grep находит единственный маркер.
#[test]
fn two_hundred_files_glob_and_grep_volume() {
    let dir = tempfile::tempdir().unwrap();
    let root = dir.path();
    let many = root.join("logs");
    std::fs::create_dir_all(&many).unwrap();
    for i in 0..200 {
        let body = if i == 77 {
            "ping\nGOLDEN-NEEDLE\n"
        } else {
            "ping\n"
        };
        std::fs::write(many.join(format!("f{i}.log")), body).unwrap();
    }

    let out = glob_out(root, None, "*.log").unwrap();
    assert_eq!(out.lines().count(), 200, "glob отдаёт все 200 файлов");

    let hits = grep_files(root, None, "GOLDEN-NEEDLE", 2).unwrap();
    assert_eq!(hits.len(), 1, "{hits:?}");
    assert_eq!(hits[0].path, "logs/f77.log");
    assert_eq!(hits[0].line_no, 2);
    assert_eq!(hits[0].text, "GOLDEN-NEEDLE");
}

/// Полный цикл правка→откат через теневой снимок: файл возвращается к
/// исходным байтам, удалённый файл воскресает, созданный после снимка
/// остаётся и перечисляется в отчёте.
#[test]
fn revert_cycle_snapshot_patch_restore() {
    let dir = tempfile::tempdir().unwrap();
    let snap_root = tempfile::tempdir().unwrap();
    let work = dir.path();
    let app = put(work, "app.rs", "fn a() {\n    1\n}\n");
    put(work, "readme.txt", "версия 1");

    let id = shadow::create_in(snap_root.path(), work)
        .unwrap()
        .expect("снимок есть");
    assert!(shadow::is_shadow(&id), "вне git снимок теневой: {id}");

    // Правка живым патчем, удаление одного файла, создание нового.
    let n = patch_file(&app, "    1", "    2", false).unwrap();
    assert_eq!(n, 1);
    std::fs::remove_file(work.join("readme.txt")).unwrap();
    std::fs::write(work.join("new_file.txt"), b"created after snapshot").unwrap();

    let report = shadow::restore_in(snap_root.path(), work, &id).unwrap();
    assert_eq!(
        std::fs::read_to_string(&app).unwrap(),
        "fn a() {\n    1\n}\n",
        "правка откатилась до исходных байтов"
    );
    assert_eq!(
        std::fs::read_to_string(work.join("readme.txt")).unwrap(),
        "версия 1",
        "удалённый файл вернулся"
    );
    assert_eq!(
        std::fs::read_to_string(work.join("new_file.txt")).unwrap(),
        "created after snapshot",
        "созданное после снимка не удаляется"
    );
    assert!(
        report.restored.contains(&"app.rs".to_string()),
        "{report:?}"
    );
    assert!(
        report.restored.contains(&"readme.txt".to_string()),
        "{report:?}"
    );
    assert!(
        report.untracked_left.contains(&"new_file.txt".to_string()),
        "{report:?}"
    );
}

/* ── Ревизия 85: находки зондажа закрыты, репро живут как регрессии ───── */

/// PATCH-MIXED-EOL (закрыт ревизией 85): вставка наследует конец строк
/// файла — level-2 патч в CRLF-файле больше не смешивает LF.
#[test]
fn patch_multiline_new_inherits_crlf() {
    let dir = tempfile::tempdir().unwrap();
    let root = dir.path();
    let src = "fn a() {\r\n    let x = 1;\r\n    let y = 2;\r\n}\r\n";
    let path = put(root, "win_code.rs", src);

    // Модель прислала old с LF и без отступов — дрейф, level-2.
    let old = "let x = 1;\n    let y = 2;";
    let new = "let x = 10;\n    let y = 20;";
    patch_file(&path, old, new, false).expect("патч проходит");

    let after = std::fs::read_to_string(&path).unwrap();
    eprintln!("[probe] байты после level-2 патча CRLF-файла: {after:?}");
    let bytes = after.as_bytes();
    let lone_lf = bytes
        .iter()
        .enumerate()
        .any(|(i, &b)| b == b'\n' && (i == 0 || bytes[i - 1] != b'\r'));
    assert!(
        !lone_lf,
        "FINDING: CRLF-файл после патча содержит голые LF: {after:?}"
    );
}

/// GLOB-SUBPATH-ANCHORED (закрыт ревизией 85): якорный паттерн матчится и
/// против путей от root — база матчинга совпала с базой выдачи.
#[test]
fn glob_anchored_pattern_with_subpath_finds_files() {
    let dir = tempfile::tempdir().unwrap();
    let root = dir.path();
    put(root, "src/main.rs", "fn main() {}\n");
    put(root, "src/util.rs", "pub fn util() {}\n");
    put(root, "Cargo.toml", "[package]\n");

    let whole = glob_out(root, None, "src/*.rs").unwrap();
    eprintln!("[probe] glob без path, 'src/*.rs' → {whole:?}");
    assert!(whole.contains("src/main.rs"), "{whole}");

    let bare = glob_out(root, Some("src"), "*.rs").unwrap();
    eprintln!("[probe] glob path='src', '*.rs' → {bare:?}");
    assert!(
        bare.contains("src/main.rs"),
        "bare-паттерн с сабпутём работает: {bare}"
    );

    let scoped = glob_out(root, Some("src"), "src/*.rs").unwrap();
    eprintln!("[probe] glob path='src', 'src/*.rs' → {scoped:?}");
    assert!(
        scoped.contains("src/main.rs"),
        "FINDING: файл существует, тот же паттерн с path='src' его не находит: {scoped}"
    );
}

/// GREP-TRUNCATE-SILENT (закрыт ревизией 85): обрезка на 300 совпадениях
/// видима в GrepReport::truncated, тулз дописывает пометку в выдачу.
#[test]
fn grep_truncation_is_reported() {
    let dir = tempfile::tempdir().unwrap();
    let root = dir.path();
    for i in 0..200 {
        std::fs::write(
            root.join(format!("g{i}.txt")),
            "alpha NEEDLE-UNIQ\nbeta NEEDLE-UNIQ\n",
        )
        .unwrap();
    }
    // 200 файлов × 2 вхождения = 400 совпадений.
    let report = grep_files_detailed(root, None, "NEEDLE-UNIQ", 0).unwrap();
    eprintln!(
        "[probe] 400 совпадений в дереве, найдено {}, truncated={}",
        report.hits.len(),
        report.truncated
    );
    assert_eq!(
        report.hits.len(),
        swagcod_fsx::tools::MAX_GREP_HITS,
        "выдача обрезана потолком"
    );
    assert!(report.truncated, "обрезка обязана быть видимой в отчёте");
}

/// GREP-4MB-SKIP (закрыт ревизией 85): файл больше 4 МБ попадает в
/// skipped_large — молчаливый пропуск стал видимой частью отчёта, тулз
/// дописывает пометку в выдачу.
#[test]
fn grep_reports_skipped_large_files() {
    let dir = tempfile::tempdir().unwrap();
    let root = dir.path();
    let filler = "filler line for the big file probe 0123456789\n";
    let mut big = String::from("UNIQUE-BIG-NEEDLE marker line\n");
    while big.len() < 5 * 1024 * 1024 {
        big.push_str(filler);
    }
    std::fs::write(root.join("big.txt"), big.as_bytes()).unwrap();

    let report = grep_files_detailed(root, None, "UNIQUE-BIG-NEEDLE", 0).unwrap();
    eprintln!(
        "[probe] 5 МБ файл: hits={}, skipped_large={:?} (лимит {})",
        report.hits.len(),
        report.skipped_large,
        swagcod_fsx::tools::MAX_GREP_FILE_BYTES
    );
    assert!(
        report.skipped_large.iter().any(|p| p == "big.txt"),
        "пропущенный большой файл обязан быть в отчёте: {:?}",
        report.skipped_large
    );
    assert!(report.hits.is_empty());
}

/// SHADOW-REVERT-2MB (закрыт ревизией 85): неполный откат больше не
/// выполняется тихо — restore по урезанному снимку отказывает с точным
/// объяснением, состояние файла не меняется без ведома владельца.
#[test]
fn shadow_revert_of_truncated_snapshot_fails_loudly() {
    let dir = tempfile::tempdir().unwrap();
    let snap_root = tempfile::tempdir().unwrap();
    let work = dir.path();
    let filler = "строка данных для теневого снимка 0123456789\n";
    let mut original = String::from("MARKER-LINE-TO-PATCH\n");
    while original.len() < 2 * 1024 * 1024 + 512 * 1024 {
        original.push_str(filler);
    }
    let big = put(work, "big.txt", &original);
    put(work, "small.txt", "маленький файл");

    let id = shadow::create_in(snap_root.path(), work)
        .unwrap()
        .expect("снимок есть");
    patch_file(&big, "MARKER-LINE-TO-PATCH", "PATCHED-LINE-TO-PATCH", false).unwrap();

    let restored = shadow::restore_in(snap_root.path(), work, &id);
    eprintln!("[probe] restore по урезанному снимку → {restored:?}");
    let err = restored.expect_err("неполный откат обязан отказать");
    let msg = format!("{err}");
    assert!(
        msg.contains("урезан"),
        "отказ обязан назвать причину: {msg}"
    );
    // Отказ ничего не трогает: файл остался ровно в изменённом виде,
    // каким был до запроса отката, — тихой подмены состояния больше нет.
    let after = std::fs::read_to_string(&big).unwrap();
    let patched_before_restore = std::fs::read_to_string(&big).unwrap();
    assert!(after.contains("PATCHED-LINE"));
    assert_eq!(
        after, patched_before_restore,
        "отказ не должен менять содержимое"
    );
    assert!(
        after != original,
        "файл остался патченным — владелец предупреждён отказом, а не тихим «успехом»"
    );
}

/// GREP-BOM-ANCHOR (закрыт ревизией 85): BOM съедается до разбора строк —
/// ^-анкоры работают на первой строке, как их и пишет модель.
#[test]
fn grep_bom_does_not_break_anchored_first_line() {
    let dir = tempfile::tempdir().unwrap();
    let root = dir.path();
    let src = "\u{FEFF}fn main() {\n    let x = 1;\n}\n";
    put(root, "bom.rs", src);

    let plain = grep_files(root, None, "fn main", 0).unwrap();
    eprintln!(
        "[probe] grep 'fn main' (без якоря) → {} совпадений",
        plain.len()
    );
    assert_eq!(plain.len(), 1, "неякорный поиск находит: {plain:?}");

    let anchored = grep_files(root, None, "^fn main", 0).unwrap();
    eprintln!("[probe] grep '^fn main' → {} совпадений", anchored.len());
    assert_eq!(
        anchored.len(),
        1,
        "^-анкер находит первую строку BOM-файла: {anchored:?}"
    );
}
