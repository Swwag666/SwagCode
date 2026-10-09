/*! Веб-поиск для агента (F-4): `web_search {query, max_results?}`.

Два вкуса выдачи, один интерфейс:

- **SearXNG** (`SWAGCOD_SEARCH_URL`) — свой движок, JSON (`format=json`),
  стабилен и не зависит от чужих фронтов;
- **DuckDuckGo HTML** (`https://html.duckduckgo.com/html/`) — дефолт без
  ключа и без настройки: работает из коробки, но это чужой публичный
  эндпоинт, и он вправе ответить капчей.

Поэтому пустая выдача — не молчаливый «ноль результатов», а ошибка с
подсказкой про `SWAGCOD_SEARCH_URL`: агент должен понимать, что поиск не
сработал, а не что «в интернете ничего нет».

Ограничения те же, что у `fetch_url` (B-5): только http(s), таймаут,
потолок тела. Отдельной SSRF-защиты здесь нет ровно как и там — эндпоинт
поиска задаёт владелец приложения через env, а не модель.

Парсинг — чистые функции от текста ответа ([`parse_searxng_json`],
[`parse_duckduckgo_html`]), поэтому проверяется на фикстурах без сети;
сеть покрыта одним сквозным тестом на mock-сервере.
!*/

use std::time::Duration;

use serde::{Deserialize, Serialize};

use crate::FsxError;

/// Потолок результатов: больше восьми модель всё равно не читает, а
/// контекст раздувает.
pub const MAX_RESULTS: usize = 8;

/// Дефолтное число результатов, если модель не задала `max_results`.
pub const DEFAULT_RESULTS: usize = 5;

/// Потолок тела ответа поиска: 4 МБ — с запасом на HTML-обвязку.
pub const MAX_SEARCH_BYTES: usize = 4 * 1024 * 1024;

/// Дефолтный эндпоинт: DuckDuckGo HTML, без ключа.
pub const DDG_HTML_ENDPOINT: &str = "https://html.duckduckgo.com/html/";

/// User-Agent: честное имя инструмента, а не маска браузера.
const UA: &str = "SwagCod/1.0 (web_search tool)";

/// Один результат поиска.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct SearchHit {
    pub title: String,
    pub url: String,
    pub snippet: String,
}

/// Собрать URL поиска.
///
/// `endpoint` — значение `SWAGCOD_SEARCH_URL`. Поддерживаются три формы:
/// `http://host/search` (добавим `?q=…&format=json`),
/// `http://host/search?cat=it` (добавим `&q=…&format=json`) и
/// `http://host/search?q={q}` (подставим в плейсхолдер, `format` — на
/// совести владельца). Без `endpoint` — DuckDuckGo HTML.
pub fn build_search_url(query: &str, endpoint: Option<&str>) -> Result<String, FsxError> {
    let q = query.trim();
    if q.is_empty() {
        return Err(FsxError::Other("web_search: пустой query".into()));
    }
    let enc = urlencode(q);
    match endpoint.map(str::trim).filter(|s| !s.is_empty()) {
        Some(base) => {
            let lower = base.to_lowercase();
            if !(lower.starts_with("http://") || lower.starts_with("https://")) {
                return Err(FsxError::Other(
                    "web_search: SWAGCOD_SEARCH_URL — только http(s) URL".into(),
                ));
            }
            if base.contains("{q}") {
                return Ok(base.replace("{q}", &enc));
            }
            let sep = if base.contains('?') { '&' } else { '?' };
            Ok(format!("{base}{sep}q={enc}&format=json"))
        }
        None => Ok(format!("{DDG_HTML_ENDPOINT}?q={enc}")),
    }
}

/// Найти в вебе: запрос → список результатов (не больше `max_results`,
/// потолок [`MAX_RESULTS`]).
pub async fn web_search(
    query: &str,
    max_results: usize,
    endpoint: Option<&str>,
    timeout: Duration,
) -> Result<Vec<SearchHit>, FsxError> {
    let url = build_search_url(query, endpoint)?;
    let cap = max_results.clamp(1, MAX_RESULTS);
    let client = reqwest::Client::builder()
        .timeout(timeout)
        .user_agent(UA)
        .redirect(reqwest::redirect::Policy::limited(5))
        .build()
        .map_err(|e| FsxError::Other(format!("web_search: {e}")))?;
    let resp = client
        .get(&url)
        .send()
        .await
        .map_err(|e| FsxError::Other(format!("web_search: {e}")))?;
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
        .map_err(|e| FsxError::Other(format!("web_search: {e}")))?;
    if status >= 400 {
        return Err(FsxError::Other(format!(
            "web_search: HTTP {status} от {url}"
        )));
    }
    let body = String::from_utf8_lossy(&bytes[..bytes.len().min(MAX_SEARCH_BYTES)]);
    let hits = dedupe(parse_results(&body, &ct), cap);
    if hits.is_empty() {
        return Err(FsxError::Other(format!(
            "web_search: выдача пуста (HTTP {status}). Публичный поиск мог ответить капчей — \
             задайте свой движок в SWAGCOD_SEARCH_URL (SearXNG с format=json)."
        )));
    }
    Ok(hits)
}

/// Разобрать тело ответа: JSON (SearXNG) или HTML (DuckDuckGo).
///
/// Выбор по content-type, с фолбэком на первый непробельный символ: часть
/// движков отдаёт JSON с текстовым content-type, и наоборот.
pub fn parse_results(body: &str, content_type: &str) -> Vec<SearchHit> {
    let ct = content_type.to_lowercase();
    let looks_json = body.trim_start().starts_with(['{', '[']);
    if ct.contains("json") || (looks_json && !ct.contains("html")) {
        let json = parse_searxng_json(body);
        if !json.is_empty() || looks_json {
            return json;
        }
    }
    parse_duckduckgo_html(body)
}

/// SearXNG-вкус: `{"results":[{"title","url","content"}]}`.
///
/// Незнакомые поля игнорируются, отсутствующий `content` — пустой сниппет:
/// движки настроены по-разному, и падать на этом нельзя.
pub fn parse_searxng_json(body: &str) -> Vec<SearchHit> {
    #[derive(Deserialize)]
    struct Raw {
        #[serde(default)]
        results: Vec<RawHit>,
    }
    #[derive(Deserialize)]
    struct RawHit {
        #[serde(default)]
        title: String,
        #[serde(default)]
        url: String,
        #[serde(default)]
        content: String,
    }
    let Ok(parsed) = serde_json::from_str::<Raw>(body) else {
        return Vec::new();
    };
    parsed
        .results
        .into_iter()
        .filter(|h| !h.url.trim().is_empty())
        .map(|h| SearchHit {
            title: collapse_ws(&h.title),
            url: h.url.trim().to_string(),
            snippet: collapse_ws(&h.content),
        })
        .collect()
}

/// DuckDuckGo HTML-вкус: блоки `.result__body` с `a.result__a` и сниппетом.
///
/// Ссылки DDG обёрнуты в редирект `//duckduckgo.com/l/?uddg=<encoded>` —
/// распаковываем, иначе модель получит бесполезный адрес промежуточной
/// страницы. Рекламные блоки (`result--ad`, `y.js`) выбрасываем: это не
/// результаты поиска, и платить за них контекстом незачем.
pub fn parse_duckduckgo_html(body: &str) -> Vec<SearchHit> {
    use scraper::{Html, Selector};
    let doc = Html::parse_document(body);
    let Ok(link) = Selector::parse("a.result__a") else {
        return Vec::new();
    };
    /* Блоки ищем по внутреннему `.result__body`, а не по внешнему
    `.web-result`: внешний содержит внутренний, и оба селектора дали бы
    каждый результат дважды. Внешний — только фолбэк, если разметка
    изменилась и внутреннего нет вовсе. */
    let inner = Selector::parse("div.result__body").ok();
    let outer = Selector::parse("div.web-result, div.result").ok();
    let mut blocks: Vec<_> = inner.iter().flat_map(|sel| doc.select(sel)).collect();
    if blocks.is_empty() {
        blocks = outer.iter().flat_map(|sel| doc.select(sel)).collect();
    }
    let snippet = Selector::parse(".result__snippet").ok();
    let mut out = Vec::new();
    for node in blocks {
        if is_ad(&node) {
            continue;
        }
        let Some(a) = node.select(&link).next() else {
            continue;
        };
        let href = a.value().attr("href").unwrap_or_default();
        // Рекламные переходы DDG идут через y.js — это не результат.
        if href.contains("y.js") {
            continue;
        }
        let url = unpack_ddg_href(href);
        if url.is_empty() {
            continue;
        }
        let title = collapse_ws(&a.text().collect::<String>());
        let snip = snippet
            .as_ref()
            .and_then(|sel| node.select(sel).next())
            .map(|s| collapse_ws(&s.text().collect::<String>()))
            .unwrap_or_default();
        out.push(SearchHit {
            title,
            url,
            snippet: snip,
        });
    }
    out
}

/// Рекламный/забаненный блок: класс может висеть как на самом блоке, так и
/// на внешнем контейнере результата.
fn is_ad(node: &scraper::ElementRef<'_>) -> bool {
    let flagged = |el: &scraper::node::Element| {
        el.attr("class")
            .map(|c| {
                let c = c.to_lowercase();
                c.contains("result--ad") || c.contains("result--ban") || c.contains("js-result--ad")
            })
            .unwrap_or(false)
    };
    if flagged(node.value()) {
        return true;
    }
    node.ancestors().any(|a| match a.value() {
        scraper::Node::Element(el) => flagged(el),
        _ => false,
    })
}

/// Распаковать href DDG: редирект `…/l/?uddg=<encoded>` → целевой URL,
/// относительный `//host/path` → `https://host/path`, прочее — как есть.
pub fn unpack_ddg_href(href: &str) -> String {
    let h = href.trim();
    if h.is_empty() {
        return String::new();
    }
    if let Some(pos) = h.find("uddg=") {
        let raw = &h[pos + "uddg=".len()..];
        let value = raw.split(['&', ';']).next().unwrap_or(raw);
        let decoded = percent_decode(value);
        if !decoded.is_empty() {
            return decoded;
        }
    }
    if let Some(rest) = h.strip_prefix("//") {
        return format!("https://{rest}");
    }
    h.to_string()
}

/// Минимальный percent-decoder: `+` → пробел, `%XX` → байт.
///
/// Свой, а не крейт: нужен один вызов на ссылку, а тянуть `url`/
/// `percent-encoding` ради шести строк — лишняя зависимость в бюджете.
pub fn percent_decode(input: &str) -> String {
    let bytes = input.as_bytes();
    let mut out: Vec<u8> = Vec::with_capacity(bytes.len());
    let mut i = 0;
    while i < bytes.len() {
        match bytes[i] {
            b'+' => {
                out.push(b' ');
                i += 1;
            }
            b'%' if i + 2 < bytes.len() => {
                let hi = (bytes[i + 1] as char).to_digit(16);
                let lo = (bytes[i + 2] as char).to_digit(16);
                match (hi, lo) {
                    (Some(h), Some(l)) => {
                        out.push((h * 16 + l) as u8);
                        i += 3;
                    }
                    _ => {
                        out.push(bytes[i]);
                        i += 1;
                    }
                }
            }
            b => {
                out.push(b);
                i += 1;
            }
        }
    }
    String::from_utf8_lossy(&out).into_owned()
}

/// Percent-encoding для значения query-параметра.
pub fn urlencode(input: &str) -> String {
    let mut out = String::with_capacity(input.len());
    for b in input.as_bytes() {
        match b {
            b'A'..=b'Z' | b'a'..=b'z' | b'0'..=b'9' | b'-' | b'_' | b'.' | b'~' => {
                out.push(*b as char)
            }
            _ => out.push_str(&format!("%{b:02X}")),
        }
    }
    out
}

/// Убрать дубли по URL и обрезать до `cap`.
pub fn dedupe(hits: Vec<SearchHit>, cap: usize) -> Vec<SearchHit> {
    let mut seen = std::collections::HashSet::new();
    let mut out = Vec::with_capacity(hits.len().min(cap));
    for h in hits {
        if h.url.trim().is_empty() || !seen.insert(h.url.clone()) {
            continue;
        }
        out.push(h);
        if out.len() >= cap {
            break;
        }
    }
    out
}

/// Схлопнуть пробелы и переводы строк в один пробел (HTML-текст сниппета).
fn collapse_ws(s: &str) -> String {
    let mut out = String::with_capacity(s.len());
    let mut space = false;
    for c in s.chars() {
        if c.is_whitespace() {
            space = true;
            continue;
        }
        if space && !out.is_empty() {
            out.push(' ');
        }
        space = false;
        out.push(c);
    }
    out.trim().to_string()
}

/// Текст результата для траектории: модель видит заголовок, URL и сниппет
/// пронумерованным списком — так ссылку удобно цитировать в ответе.
pub fn format_results(query: &str, hits: &[SearchHit]) -> String {
    if hits.is_empty() {
        return format!("[web_search: «{query}» — результатов нет]");
    }
    let mut out = format!("[web_search: «{query}» — {} рез.]\n", hits.len());
    for (i, h) in hits.iter().enumerate() {
        out.push_str(&format!("\n{}. {}\n   {}", i + 1, h.title, h.url));
        if !h.snippet.is_empty() {
            out.push_str(&format!("\n   {}", h.snippet));
        }
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    const SEARXNG: &str = r#"{"query":"rust async","results":[
        {"title":"Async book","url":"https://rust-lang.github.io/async-book/","content":"Описание\tасинхронности"},
        {"title":"Tokio","url":"https://tokio.rs","content":"Runtime"},
        {"title":"без url","content":"мусор"},
        {"title":"Async book","url":"https://rust-lang.github.io/async-book/","content":"дубль"}
    ],"unresponsive_engines":["google"]}"#;

    const DDG: &str = r#"<html><body>
<div class="result results_links results_links_deep web-result">
  <div class="result__body">
    <h2 class="result__title"><a rel="noopener" class="result__a"
      href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2Fpage%3Fx%3D1&amp;rut=abc">Пример
      страница</a></h2>
    <a class="result__snippet" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com">Текст
      <b>сниппета</b> с пробелами</a>
  </div>
</div>
<div class="result result--ad web-result">
  <div class="result__body">
    <h2><a class="result__a" href="//duckduckgo.com/y.js?ad=1">Реклама</a></h2>
    <a class="result__snippet">Купи</a>
  </div>
</div>
<div class="web-result">
  <div class="result__body">
    <h2><a class="result__a" href="https://plain.example/2">Прямая ссылка</a></h2>
  </div>
</div>
</body></html>"#;

    #[test]
    fn builds_default_ddg_url_with_encoding() {
        let url = build_search_url("rust async/await", None).unwrap();
        assert_eq!(
            url,
            "https://html.duckduckgo.com/html/?q=rust%20async%2Fawait"
        );
    }

    #[test]
    fn builds_searxng_url_with_and_without_query() {
        let a = build_search_url("tokio", Some("http://127.0.0.1:8080/search")).unwrap();
        assert_eq!(a, "http://127.0.0.1:8080/search?q=tokio&format=json");
        let b = build_search_url("tokio", Some("http://h/search?cat=it")).unwrap();
        assert_eq!(b, "http://h/search?cat=it&q=tokio&format=json");
        let c = build_search_url("a b", Some("https://h/s?query={q}&fmt=json")).unwrap();
        assert_eq!(c, "https://h/s?query=a%20b&fmt=json");
    }

    #[test]
    fn rejects_empty_query_and_non_http_endpoint() {
        assert!(build_search_url("   ", None).is_err());
        let e = build_search_url("x", Some("ftp://host/search")).unwrap_err();
        assert!(e.to_string().contains("только http(s)"), "{e}");
    }

    #[test]
    fn parses_searxng_json_skipping_junk_and_duplicates_source() {
        let hits = parse_searxng_json(SEARXNG);
        // «без url» отброшен, дубль остался (его снимает dedupe).
        assert_eq!(hits.len(), 3);
        assert_eq!(hits[0].title, "Async book");
        assert_eq!(hits[0].url, "https://rust-lang.github.io/async-book/");
        assert_eq!(hits[0].snippet, "Описание асинхронности");
        assert_eq!(hits[1].snippet, "Runtime");
    }

    #[test]
    fn parses_json_by_content_sniffing() {
        let hits = parse_results(SEARXNG, "text/plain");
        assert_eq!(hits.len(), 3);
        let html = parse_results(DDG, "text/html; charset=utf-8");
        assert_eq!(html.len(), 2);
    }

    #[test]
    fn invalid_json_yields_nothing() {
        assert!(parse_searxng_json("{oops").is_empty());
        assert!(parse_searxng_json("{\"results\":null}").is_empty());
    }

    #[test]
    fn parses_ddg_html_unpacking_redirects_and_dropping_ads() {
        let hits = parse_duckduckgo_html(DDG);
        assert_eq!(hits.len(), 2, "реклама и y.js не результаты: {hits:?}");
        assert_eq!(hits[0].title, "Пример страница");
        assert_eq!(hits[0].url, "https://example.com/page?x=1");
        assert_eq!(hits[0].snippet, "Текст сниппета с пробелами");
        assert_eq!(hits[1].url, "https://plain.example/2");
        assert_eq!(hits[1].snippet, "");
    }

    #[test]
    fn ddg_anomaly_page_gives_empty_list() {
        let html = "<html><body><h2>If this error persists, please let us know</h2></body></html>";
        assert!(parse_duckduckgo_html(html).is_empty());
    }

    #[test]
    fn unpack_href_handles_relative_and_encoded_forms() {
        assert_eq!(
            unpack_ddg_href("//duckduckgo.com/l/?uddg=https%3A%2F%2Fa.b%2Fc%3Fd%3D1&rut=x"),
            "https://a.b/c?d=1"
        );
        assert_eq!(unpack_ddg_href("//example.org/x"), "https://example.org/x");
        assert_eq!(unpack_ddg_href("https://a.b"), "https://a.b");
        assert_eq!(unpack_ddg_href("  "), "");
        // Битый процент не роняет разбор: остаётся как есть.
        assert_eq!(unpack_ddg_href("?uddg=%zz"), "%zz");
    }

    #[test]
    fn percent_codec_roundtrip() {
        assert_eq!(urlencode("a b/c?d=e"), "a%20b%2Fc%3Fd%3De");
        assert_eq!(urlencode("е"), "%D0%B5");
        assert_eq!(percent_decode("a%20b%2Fc%3Fd%3De"), "a b/c?d=e");
        assert_eq!(percent_decode("a+b"), "a b");
        assert_eq!(percent_decode("%E2%9C%93"), "✓");
    }

    #[test]
    fn dedupe_caps_and_drops_repeated_urls() {
        let hits = vec![
            SearchHit {
                title: "a".into(),
                url: "https://x/1".into(),
                snippet: String::new(),
            },
            SearchHit {
                title: "a dup".into(),
                url: "https://x/1".into(),
                snippet: String::new(),
            },
            SearchHit {
                title: "b".into(),
                url: "https://x/2".into(),
                snippet: String::new(),
            },
            SearchHit {
                title: "no url".into(),
                url: "  ".into(),
                snippet: String::new(),
            },
        ];
        let out = dedupe(hits, 8);
        assert_eq!(out.len(), 2);
        assert_eq!(out[1].url, "https://x/2");
        // Кап respected: из двух остаётся один.
        let capped = dedupe(parse_searxng_json(SEARXNG), 1);
        assert_eq!(capped.len(), 1);
    }

    #[test]
    fn format_results_is_numbered_and_cites_urls() {
        let hits = parse_searxng_json(SEARXNG);
        let text = format_results("rust async", &hits[..2]);
        assert!(text.starts_with("[web_search: «rust async» — 2 рез.]"));
        assert!(text.contains("1. Async book"));
        assert!(text.contains("https://tokio.rs"));
        assert!(format_results("q", &[]).contains("результатов нет"));
    }

    // ---- сквозные тесты на mock-сервере (паттерн provider::router) ----

    use std::sync::{Arc, Mutex};
    use tokio::io::{AsyncReadExt, AsyncWriteExt};
    use tokio::net::TcpListener;

    /// Сервер на один запрос: отдаёт тело с указанным content-type и
    /// запоминает строку запроса (проверка URL-билдера).
    async fn one_shot_server(
        body: String,
        ct: &'static str,
    ) -> (String, Arc<Mutex<Option<String>>>) {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        let seen: Arc<Mutex<Option<String>>> = Arc::new(Mutex::new(None));
        let seen_task = seen.clone();
        tokio::spawn(async move {
            if let Ok((mut sock, _)) = listener.accept().await {
                let mut buf = [0u8; 4096];
                let n = tokio::time::timeout(Duration::from_secs(5), sock.read(&mut buf))
                    .await
                    .map(|r| r.unwrap_or(0))
                    .unwrap_or(0);
                let head = String::from_utf8_lossy(&buf[..n]).to_string();
                if let Ok(mut g) = seen_task.lock() {
                    *g = head.lines().next().map(|l| l.to_string());
                }
                let resp = format!(
                    "HTTP/1.1 200 OK\r\ncontent-type: {ct}\r\ncontent-length: {}\r\n\r\n{}",
                    body.len(),
                    body
                );
                let _ = sock.write_all(resp.as_bytes()).await;
                let _ = sock.shutdown().await;
            }
        });
        (format!("http://{addr}/search"), seen)
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn web_search_reads_searxng_json_over_http() {
        let (base, seen) = one_shot_server(SEARXNG.to_string(), "application/json").await;
        let hits = web_search("rust async", 3, Some(&base), Duration::from_secs(10))
            .await
            .unwrap();
        assert_eq!(hits.len(), 2, "дубль URL снят, кап 3: {hits:?}");
        assert_eq!(hits[0].url, "https://rust-lang.github.io/async-book/");
        let line = seen.lock().unwrap().clone().unwrap();
        assert!(line.contains("q=rust%20async"), "{line}");
        assert!(line.contains("format=json"), "{line}");
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn web_search_reads_ddg_html_flavour() {
        let (base, _seen) = one_shot_server(DDG.to_string(), "text/html").await;
        // endpoint задан (mock), но ответ HTML — парсер выбирается по телу.
        let hits = web_search("example", 8, Some(&base), Duration::from_secs(10))
            .await
            .unwrap();
        assert_eq!(hits.len(), 2);
        assert_eq!(hits[0].url, "https://example.com/page?x=1");
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn web_search_reports_empty_result_set_as_error() {
        let (base, _) = one_shot_server("{\"results\":[]}".to_string(), "application/json").await;
        let e = web_search("ничего", 5, Some(&base), Duration::from_secs(10))
            .await
            .unwrap_err()
            .to_string();
        assert!(e.contains("выдача пуста"), "{e}");
        assert!(e.contains("SWAGCOD_SEARCH_URL"), "{e}");
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn web_search_propagates_http_error_status() {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        tokio::spawn(async move {
            if let Ok((mut sock, _)) = listener.accept().await {
                let mut buf = [0u8; 2048];
                let _ = tokio::time::timeout(Duration::from_secs(5), sock.read(&mut buf)).await;
                let resp = "HTTP/1.1 503 Service Unavailable\r\ncontent-length: 0\r\n\r\n";
                let _ = sock.write_all(resp.as_bytes()).await;
                let _ = sock.shutdown().await;
            }
        });
        let e = web_search(
            "x",
            5,
            Some(&format!("http://{addr}/search")),
            Duration::from_secs(10),
        )
        .await
        .unwrap_err()
        .to_string();
        assert!(e.contains("HTTP 503"), "{e}");
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn web_search_refuses_non_http_endpoint_before_network() {
        let e = web_search("x", 5, Some("file:///etc/passwd"), Duration::from_secs(1))
            .await
            .unwrap_err()
            .to_string();
        assert!(e.contains("только http(s)"), "{e}");
    }
}
