/*! Инструменты агента поверх файловой системы (этап B-5).

`grep` — regex-поиск по содержимому с контекстными строками, `glob` —
поиск путей шаблоном, `patch` — литеральная замена блока с similar-
диагностикой и допуском к whitespace-дрейфу, `fetch_url` — HTTP-запрос
с превращением HTML в читаемый текст через `scraper`.

Всё, кроме fetch, работает строго внутри root, который передаёт вызывающий
(app-слой резолвит его как cwd сессии через sandbox_path): сам модуль
песочницу не повторяет, но и не покидает переданный корень — обход идёт
через `ignore::WalkBuilder` от root.
*/

use std::path::{Path, PathBuf};

use crate::index::FileIndex;
use crate::FsxError;

/* ── grep ─────────────────────────────────────────────────────────────── */

/// Потолок совпадений: вывод всё равно режется лимитом контекста тулза,
/// но не гоняем гигабайты через канал зря.
pub const MAX_GREP_HITS: usize = 300;
/// Файлы крупнее не читаем: бинарники и логи — не код.
pub const MAX_GREP_FILE_BYTES: u64 = 4 * 1024 * 1024;

/// Одно совпадение grep.
#[derive(Debug, Clone, PartialEq)]
pub struct GrepHit {
    /// Относительный путь от root с '/'-разделителем.
    pub path: String,
    /// Номер строки (1-based).
    pub line_no: usize,
    pub text: String,
    pub context_before: Vec<String>,
    pub context_after: Vec<String>,
}

/// Regex-поиск по файлам root (опционально в поддереве `sub`).
///
/// Обход уважает .gitignore; бинарные файлы (NUL-байт в первых 8 КБ)
/// пропускаются; совпадения ограничены [`MAX_GREP_HITS`].
pub fn grep_files(
    root: &Path,
    sub: Option<&str>,
    pattern: &str,
    context_lines: usize,
) -> Result<Vec<GrepHit>, FsxError> {
    let re = regex::Regex::new(pattern)
        .map_err(|e| FsxError::Other(format!("некорректный regex: {e}")))?;
    let scan_root = match sub {
        Some(s) if !s.is_empty() => root.join(s),
        _ => root.to_path_buf(),
    };
    if !scan_root.is_dir() {
        return Err(FsxError::NotFound(scan_root));
    }
    let context_lines = context_lines.min(10);
    let mut hits = Vec::new();
    let walker = ignore::WalkBuilder::new(&scan_root)
        .hidden(false)
        .git_ignore(true)
        .require_git(false)
        .build();
    for entry in walker {
        if hits.len() >= MAX_GREP_HITS {
            break;
        }
        let Ok(entry) = entry else { continue };
        if !entry.file_type().map(|t| t.is_file()).unwrap_or(false) {
            continue;
        }
        let path = entry.path();
        if path
            .components()
            .any(|c| matches!(c.as_os_str().to_str(), Some(".git") | Some("node_modules") | Some("target")))
        {
            continue;
        }
        if entry.metadata().map(|m| m.len() > MAX_GREP_FILE_BYTES).unwrap_or(true) {
            continue;
        }
        let bytes = match std::fs::read(path) {
            Ok(b) => b,
            Err(_) => continue,
        };
        if bytes.iter().take(8192).any(|&b| b == 0) {
            continue; // бинарник
        }
        let content = String::from_utf8_lossy(&bytes);
        let rel = path
            .strip_prefix(root)
            .unwrap_or(path)
            .to_string_lossy()
            .replace('\\', "/");
        let lines: Vec<&str> = content.lines().collect();
        for (i, line) in lines.iter().enumerate() {
            if !re.is_match(line) {
                continue;
            }
            let from = i.saturating_sub(context_lines);
            let to = (i + context_lines + 1).min(lines.len());
            hits.push(GrepHit {
                path: rel.clone(),
                line_no: i + 1,
                text: line.to_string(),
                context_before: lines[from..i].iter().map(|s| s.to_string()).collect(),
                context_after: lines[i + 1..to].iter().map(|s| s.to_string()).collect(),
            });
            if hits.len() >= MAX_GREP_HITS {
                break;
            }
        }
    }
    Ok(hits)
}

/// Человекочитаемый (и моделечитаемый) вывод grep: `путь:строка:текст`
/// с контекстом, сгруппированный по файлу.
pub fn format_hits(hits: &[GrepHit]) -> String {
    let mut out = String::new();
    let mut last_path = String::new();
    for h in hits {
        if h.path != last_path {
            if !out.is_empty() {
                out.push('\n');
            }
            out.push_str(&h.path);
            out.push('\n');
            last_path = h.path.clone();
        }
        let mut block = String::new();
        for (off, c) in h
            .context_before
            .iter()
            .enumerate()
            .map(|(k, c)| (h.line_no - h.context_before.len() + k, c))
        {
            block.push_str(&format!("  {off}: {c}\n"));
        }
        block.push_str(&format!("> {}: {}\n", h.line_no, h.text));
        for (off, c) in h
            .context_after
            .iter()
            .enumerate()
            .map(|(k, c)| (h.line_no + 1 + k, c))
        {
            block.push_str(&format!("  {off}: {c}\n"));
        }
        out.push_str(&block);
    }
    out
}

/* ── glob ─────────────────────────────────────────────────────────────── */

/// Потолок числа путей в выдаче glob.
pub const MAX_GLOB_RESULTS: usize = 500;

/// Поиск файлов шаблоном glob внутри root (.gitignore уважается).
///
/// Шаблон без '/' матчит basename на любой глубине — семантика эталонного
/// тулза: «*.rs» находит всё дерево, «src/**/*.rs» — только под src.
pub fn glob_files(root: &Path, sub: Option<&str>, pattern: &str) -> Result<Vec<String>, FsxError> {
    let bare = !pattern.contains('/');
    // Шаблон без '/' — поиск по basename на любой глубине: '*' может
    // пересекать разделители. Шаблон с '/' — якорь глубины: '*' остаётся
    // в пределах сегмента (literal_separator), '**' по-прежнему ныряет.
    let matcher = globset::GlobBuilder::new(pattern)
        .literal_separator(!bare)
        .build()
        .map_err(|e| FsxError::Other(format!("некорректный glob: {e}")))?
        .compile_matcher();
    let scan_root = match sub {
        Some(s) if !s.is_empty() => root.join(s),
        _ => root.to_path_buf(),
    };
    let idx = FileIndex::build(&scan_root)?;
    // Пути в индексе относительны от scan_root; для выдачи — от root.
    let prefix = scan_root
        .strip_prefix(root)
        .ok()
        .map(|p| p.to_string_lossy().replace('\\', "/"))
        .filter(|s| !s.is_empty());
    let mut out = Vec::new();
    for f in &idx.files {
        let hit = if bare {
            let base = f.rsplit('/').next().unwrap_or(f);
            matcher.is_match(f.as_str()) || matcher.is_match(base)
        } else {
            matcher.is_match(f.as_str())
        };
        if hit {
            let full = match &prefix {
                Some(p) => format!("{p}/{f}"),
                None => f.clone(),
            };
            out.push(full);
            if out.len() >= MAX_GLOB_RESULTS {
                break;
            }
        }
    }
    out.sort_unstable();
    Ok(out)
}

/* ── patch ────────────────────────────────────────────────────────────── */

/// Заменить литеральный блок в тексте. Возвращает новый текст и число
/// замен. Три уровня строгости:
/// 1. точное совпадение (единственное, если `replace_all` не запрошен);
/// 2. whitespace-дрейф: блоки совпадают после нормализации пробелов —
///    частая беда моделей с отступами, лечится similar-подходом к строкам;
/// 3. иначе ошибка, а в ней — ближайший похожий регион (similar ratio),
///    чтобы модель могла поправиться сама.
pub fn patch_text(content: &str, old: &str, new: &str, replace_all: bool) -> Result<(String, usize), String> {
    if old.is_empty() {
        return Err("old_string пуст: для создания файла есть write".into());
    }
    let count = content.matches(old).count();
    if count == 1 || (count > 1 && replace_all) {
        let patched = if replace_all {
            content.replace(old, new)
        } else {
            content.replacen(old, new, 1)
        };
        return Ok((patched, if replace_all { count } else { 1 }));
    }
    if count > 1 {
        return Err(format!(
            "old_string встречается {count} раз: уточните блок или передайте replace_all=true"
        ));
    }
    // Уровень 2: whitespace-нормализация. Структура строк сохраняется,
    // поэтому отображение «нормализованная строка ↔ исходная» тривиально.
    let norm = |s: &str| s.split_whitespace().collect::<Vec<_>>().join(" ");
    let content_lines: Vec<&str> = content.lines().collect();
    let o_norm: Vec<String> = old.lines().map(&norm).collect();
    let n = o_norm.len();
    if n > 0 && content_lines.len() >= n {
        let c_norm: Vec<String> = content_lines.iter().map(|l| norm(l)).collect();
        let positions: Vec<usize> = c_norm
            .windows(n)
            .enumerate()
            .filter(|(_, w)| **w == o_norm[..])
            .map(|(i, _)| i)
            .collect();
        if positions.len() == 1 {
            let start = positions[0];
            let mut out: Vec<&str> = Vec::new();
            out.extend_from_slice(&content_lines[..start]);
            out.push(new);
            out.extend_from_slice(&content_lines[start + n..]);
            let mut patched = out.join("\n");
            if content.ends_with('\n') && !patched.ends_with('\n') {
                patched.push('\n');
            }
            return Ok((patched, 1));
        }
        if positions.len() > 1 {
            return Err(format!(
                "old_string неоднозначен даже с точностью до пробелов ({} совпадений)",
                positions.len()
            ));
        }
    }
    // Уровень 3: similar-диагностика — где ближайший похожий регион.
    Err(format!("old_string не найден в файле{}", closest_region(content, old)))
}

/// Подсказка «ближайший похожий регион» через similar ratio. Считается
/// только на файлах до 5000 строк: на больших диагностика дороже лечения.
fn closest_region(content: &str, old: &str) -> String {
    use similar::TextDiff;
    let content_lines: Vec<&str> = content.lines().collect();
    let old_lines: Vec<&str> = old.lines().collect();
    let n = old_lines.len().max(1);
    if content_lines.len() > 5000 || content_lines.len() < n {
        return String::new();
    }
    let old_joined = old_lines.join("\n");
    let mut best: Option<(f64, usize)> = None;
    for start in 0..=(content_lines.len() - n) {
        let window = content_lines[start..start + n].join("\n");
        let ratio = TextDiff::from_lines(&old_joined, &window).ratio() as f64;
        if best.map(|(r, _)| ratio > r).unwrap_or(true) {
            best = Some((ratio, start + 1));
        }
    }
    match best {
        // Порог 0.4: половина общих строк окна — уже полезная подсказка.
        Some((r, line)) if r > 0.4 => format!(
            "; ближайший похожий блок у строки {line} (сходство {:.0}%)",
            r * 100.0
        ),
        _ => String::new(),
    }
}

/* ── fetch_url ────────────────────────────────────────────────────────── */

/// Потолок тела ответа: 2 МБ текста модели за глаза.
pub const MAX_FETCH_BYTES: usize = 2 * 1024 * 1024;

/// Скачать URL (только http/https) и вернуть текст: HTML редуцируется
/// в читаемый (script/style/noscript выбрасываются), остальное отдаётся
/// как есть. В начале — строка статуса, в конце — отметка об обрезке.
pub async fn fetch_url(url: &str, timeout: std::time::Duration) -> Result<String, FsxError> {
    let lower = url.trim().to_lowercase();
    if !(lower.starts_with("http://") || lower.starts_with("https://")) {
        return Err(FsxError::Other(
            "fetch_url: разрешены только http(s) URL".into(),
        ));
    }
    let client = reqwest::Client::builder()
        .timeout(timeout)
        .user_agent("SwagCod/1.0 (fetch_url tool)")
        .redirect(reqwest::redirect::Policy::limited(5))
        .build()
        .map_err(|e| FsxError::Other(format!("fetch_url: {e}")))?;
    let resp = client
        .get(url.trim())
        .send()
        .await
        .map_err(|e| FsxError::Other(format!("fetch_url: {e}")))?;
    let status = resp.status().as_u16();
    let ct = resp
        .headers()
        .get(reqwest::header::CONTENT_TYPE)
        .and_then(|v| v.to_str().ok())
        .unwrap_or("")
        .to_lowercase();
    let bytes = resp
        .bytes()
        .await
        .map_err(|e| FsxError::Other(format!("fetch_url: {e}")))?;
    let mut data: &[u8] = bytes.as_ref();
    let truncated = data.len() > MAX_FETCH_BYTES;
    if truncated {
        data = &data[..MAX_FETCH_BYTES];
    }
    let text = if ct.contains("html") || looks_html(data) {
        html_to_text(data)
    } else {
        String::from_utf8_lossy(data).into_owned()
    };
    let mut out = format!("[status {status}]\n{text}");
    if truncated {
        out.push_str("\n[обрезано на 2 МБ]");
    }
    Ok(out)
}

fn looks_html(data: &[u8]) -> bool {
    let head = String::from_utf8_lossy(&data[..data.len().min(512)]).to_lowercase();
    head.contains("<!doctype html") || head.contains("<html")
}

/// HTML → читаемый текст: скрипты/стили/шаблоны выбрасываются, блочные
/// теги дают переводы строк, сущности декодирует scraper.
pub fn html_to_text(data: &[u8]) -> String {
    use scraper::{Html, Node};
    const SKIP: [&str; 5] = ["script", "style", "noscript", "template", "svg"];
    const BREAK: [&str; 10] = [
        "p", "div", "br", "li", "tr", "h1", "h2", "h3", "h4", "h5",
    ];
    let doc = Html::parse_document(&String::from_utf8_lossy(data));
    let mut out = String::new();
    for node in doc.tree.nodes() {
        match node.value() {
            Node::Element(e) => {
                if BREAK.contains(&e.name()) {
                    out.push('\n');
                }
            }
            Node::Text(t) => {
                let skipped = node
                    .ancestors()
                    .any(|a| matches!(a.value(), Node::Element(e) if SKIP.contains(&e.name())));
                if skipped {
                    continue;
                }
                let s = t.text.trim();
                if s.is_empty() {
                    continue;
                }
                if !out.is_empty() && !out.ends_with('\n') {
                    out.push(' ');
                }
                out.push_str(s);
            }
            _ => {}
        }
    }
    // Схлопнуть пустыри из переводов строк.
    let mut cleaned = String::with_capacity(out.len());
    let mut nl_run = 0;
    for c in out.chars() {
        if c == '\n' {
            nl_run += 1;
            if nl_run <= 2 {
                cleaned.push(c);
            }
        } else {
            nl_run = 0;
            cleaned.push(c);
        }
    }
    cleaned.trim().to_string()
}

/// Абсолютный путь из относительного (для app-слоя).
pub fn resolve_rel(root: &Path, rel: &str) -> PathBuf {
    root.join(rel)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn temp_root(tag: &str) -> PathBuf {
        let dir = std::env::temp_dir().join(format!(
            "swagcod-tools-{tag}-{}-{}",
            std::process::id(),
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        ));
        std::fs::create_dir_all(&dir).unwrap();
        dir
    }

    fn write(root: &Path, rel: &str, content: &str) {
        let p = root.join(rel);
        if let Some(parent) = p.parent() {
            std::fs::create_dir_all(parent).unwrap();
        }
        std::fs::write(p, content).unwrap();
    }

    /* ---- grep ---- */

    #[test]
    fn grep_finds_matches_with_context_and_line_numbers() {
        let root = temp_root("grep");
        write(&root, "src/a.rs", "fn one() {}\nfn target() {}\nfn three() {}\n");
        write(&root, "b.txt", "nothing here\n");
        let hits = grep_files(&root, None, "target", 1).unwrap();
        assert_eq!(hits.len(), 1);
        assert_eq!(hits[0].path, "src/a.rs");
        assert_eq!(hits[0].line_no, 2);
        assert_eq!(hits[0].context_before, vec!["fn one() {}"]);
        assert_eq!(hits[0].context_after, vec!["fn three() {}"]);
        let _ = std::fs::remove_dir_all(&root);
    }

    #[test]
    fn grep_is_regex_not_contains() {
        let root = temp_root("grep-re");
        write(&root, "x.rs", "foo123\nbar\nfoo456\n");
        let hits = grep_files(&root, None, r"foo\d+", 0).unwrap();
        assert_eq!(hits.len(), 2);
        let _ = std::fs::remove_dir_all(&root);
    }

    #[test]
    fn grep_respects_gitignore_and_skips_binary() {
        let root = temp_root("grep-gi");
        write(&root, ".gitignore", "ignored/\n");
        write(&root, "ignored/secret.rs", "target\n");
        write(&root, "code.rs", "target\n");
        std::fs::write(root.join("blob.bin"), b"target\x00\x01\x02").unwrap();
        let hits = grep_files(&root, None, "target", 0).unwrap();
        assert_eq!(hits.len(), 1);
        assert_eq!(hits[0].path, "code.rs");
        let _ = std::fs::remove_dir_all(&root);
    }

    #[test]
    fn grep_subdir_scopes_the_search() {
        let root = temp_root("grep-sub");
        write(&root, "a/x.rs", "needle\n");
        write(&root, "b/y.rs", "needle\n");
        let hits = grep_files(&root, Some("a"), "needle", 0).unwrap();
        assert_eq!(hits.len(), 1);
        assert_eq!(hits[0].path, "a/x.rs");
        let _ = std::fs::remove_dir_all(&root);
    }

    #[test]
    fn grep_bad_regex_is_error_not_panic() {
        let root = temp_root("grep-bad");
        write(&root, "f.txt", "x\n");
        assert!(grep_files(&root, None, "([", 0).is_err());
        let _ = std::fs::remove_dir_all(&root);
    }

    #[test]
    fn format_hits_groups_and_marks_match() {
        let hits = vec![GrepHit {
            path: "f.rs".into(),
            line_no: 2,
            text: "match here".into(),
            context_before: vec!["before".into()],
            context_after: vec!["after".into()],
        }];
        let out = format_hits(&hits);
        assert!(out.starts_with("f.rs\n"));
        assert!(out.contains("> 2: match here"));
        assert!(out.contains("  1: before"));
        assert!(out.contains("  3: after"));
    }

    /* ---- glob ---- */

    #[test]
    fn glob_matches_pattern_with_and_without_slash() {
        let root = temp_root("glob");
        write(&root, "src/main.rs", "x");
        write(&root, "src/deep/util.rs", "x");
        write(&root, "Cargo.toml", "x");
        let all_rs = glob_files(&root, None, "*.rs").unwrap();
        assert_eq!(all_rs, vec!["src/deep/util.rs", "src/main.rs"]);
        let scoped = glob_files(&root, None, "src/*.rs").unwrap();
        assert_eq!(scoped, vec!["src/main.rs"]);
        let deep = glob_files(&root, None, "src/**/*.rs").unwrap();
        assert_eq!(deep.len(), 2);
        let _ = std::fs::remove_dir_all(&root);
    }

    #[test]
    fn glob_respects_gitignore() {
        let root = temp_root("glob-gi");
        write(&root, ".gitignore", "build/\n");
        write(&root, "build/out.js", "x");
        write(&root, "src/app.js", "x");
        let js = glob_files(&root, None, "*.js").unwrap();
        assert_eq!(js, vec!["src/app.js"]);
        let _ = std::fs::remove_dir_all(&root);
    }

    /* ---- patch ---- */

    #[test]
    fn patch_exact_single_replacement() {
        let (out, n) = patch_text("aaa\nbbb\nccc", "bbb", "XXX", false).unwrap();
        assert_eq!(out, "aaa\nXXX\nccc");
        assert_eq!(n, 1);
    }

    #[test]
    fn patch_requires_unique_old_string() {
        let err = patch_text("dup\ndup", "dup", "x", false).unwrap_err();
        assert!(err.contains("2 раз"), "{err}");
        let (out, n) = patch_text("dup\ndup", "dup", "x", true).unwrap();
        assert_eq!(out, "x\nx");
        assert_eq!(n, 2);
    }

    #[test]
    fn patch_tolerates_whitespace_drift() {
        // Модель прислала блок с другими отступами — патч всё равно проходит.
        let content = "fn main() {\n    let x = 1;\n    println!(\"{x}\");\n}\n";
        let old = "let x = 1;\n println!(\"{x}\");";
        let (out, n) = patch_text(content, old, "let x = 2;", false).unwrap();
        assert_eq!(n, 1);
        assert!(out.contains("let x = 2;"));
        assert!(!out.contains("println"));
        assert!(out.ends_with('\n'), "хвостовой перевод строки жив");
    }

    #[test]
    fn patch_missing_block_points_to_closest_region() {
        let content = (1..=20).map(|i| format!("line {i}\n")).collect::<String>();
        let err = patch_text(&content, "line 10\nline 111", "x", false).unwrap_err();
        assert!(err.contains("не найден"), "{err}");
        assert!(err.contains("строки 10") || err.contains("сходство"), "{err}");
    }

    #[test]
    fn patch_empty_old_string_rejected() {
        assert!(patch_text("abc", "", "x", false).is_err());
    }

    /* ---- fetch/html ---- */

    #[test]
    fn html_to_text_drops_scripts_keeps_content() {
        let html = b"<html><head><style>body{color:red}</style></head><body>\
            <script>var secret = 1;</script>\
            <h1>Title</h1><p>First &amp; second</p><ul><li>one</li><li>two</li></ul>\
            </body></html>";
        let text = html_to_text(html);
        assert!(text.contains("Title"));
        assert!(text.contains("First & second"), "сущности декодируются: {text}");
        assert!(!text.contains("secret"));
        assert!(!text.contains("color:red"));
        assert!(text.contains("one") && text.contains("two"));
    }

    #[test]
    fn looks_html_sniffs_content() {
        assert!(looks_html(b"<!DOCTYPE html><html>x"));
        assert!(!looks_html(b"plain text"));
    }

    #[tokio::test]
    async fn fetch_url_rejects_non_http_schemes() {
        let err = fetch_url("file:///C:/Windows/win.ini", std::time::Duration::from_secs(5))
            .await
            .unwrap_err();
        assert!(err.to_string().contains("http"), "{err}");
    }
}
