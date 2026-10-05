package discover

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"voidsearchswag/internal/filex"
	"voidsearchswag/internal/httpc"
)

// silentLog глушит вывод обхода в тестах: логи здесь не проверяются.
type silentLog struct{}

func (silentLog) Infof(string, ...any) {}
func (silentLog) Warnf(string, ...any) {}

const cHost = "abcdefghijklmnop.onion"

// stubCrawl отдаёт заранее заданные тела по пути. Обход валидирует хосты
// как onion-адреса, поэтому настоящий httptest-сервер здесь не подойдёт:
// подставной клиент отдаёт нужную разметку и не трогает сеть.
type stubCrawl struct {
	pages map[string]string
	head  http.Header
	err   error
	calls int
	sizes int
}

func (s *stubCrawl) Fetch(ctx context.Context, r httpc.Request) (*httpc.Response, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	if r.Method == http.MethodHead {
		s.sizes++
		h := s.head
		if h == nil {
			h = http.Header{}
		}
		return &httpc.Response{Status: 200, Header: h}, nil
	}
	body, ok := s.pages[r.URL]
	if !ok {
		return &httpc.Response{Status: 404}, nil
	}
	return &httpc.Response{Status: 200, Body: []byte(body)}, nil
}

func fastCrawl(c CrawlConfig) CrawlConfig {
	c.PerHostDelay = time.Millisecond
	return c
}

func TestCrawlNoClient(t *testing.T) {
	cr := NewCrawler(nil, silentLog{}, CrawlConfig{Depth: 1})
	cands, rep := cr.Crawl(context.Background(), []string{cHost})
	if len(cands) != 0 {
		t.Errorf("без клиента найдено %d", len(cands))
	}
	if rep.Failed == 0 {
		t.Error("отказ клиента не отмечен")
	}
}

func TestCrawlNoSeeds(t *testing.T) {
	cr := NewCrawler(&stubCrawl{}, silentLog{}, CrawlConfig{Depth: 1})
	_, rep := cr.Crawl(context.Background(), nil)
	if rep.Pages != 0 {
		t.Errorf("без семян пройдено страниц %d", rep.Pages)
	}
}

func TestCrawlCollectsFiles(t *testing.T) {
	f := &stubCrawl{pages: map[string]string{
		"http://" + cHost + "/": `<a href="/data/dump.sql">d</a>
<a href="/docs/manual.pdf">m</a>
<a href="/style.css">c</a>
<a href="/app.js">j</a>
<a href="/img/logo.png">p</a>`,
	}}
	cr := NewCrawler(f, silentLog{}, fastCrawl(CrawlConfig{Depth: 1}))
	_, rep := cr.Crawl(context.Background(), []string{cHost})

	if rep.Files != 2 {
		t.Fatalf("найдено файлов %d, ожидала 2 (sql, pdf): %+v", rep.Files, rep.FileRefs)
	}
	exts := map[string]bool{}
	for _, fr := range rep.FileRefs {
		exts[fr.Ext] = true
		if fr.SourcePage == "" {
			t.Errorf("страница-источник не записана для %s", fr.URL)
		}
	}
	if !exts["sql"] || !exts["pdf"] {
		t.Errorf("расширения неверны: %v", exts)
	}
}

func TestCrawlIgnoresForeignHostFiles(t *testing.T) {
	f := &stubCrawl{pages: map[string]string{
		"http://" + cHost + "/": `<a href="http://other.onion/x.zip">foreign</a>
<a href="/local.zip">local</a>`,
	}}
	cr := NewCrawler(f, silentLog{}, fastCrawl(CrawlConfig{Depth: 1}))
	_, rep := cr.Crawl(context.Background(), []string{cHost})

	if rep.Files != 1 {
		t.Fatalf("файлов %d, ожидала 1: %+v", rep.Files, rep.FileRefs)
	}
	if rep.FileRefs[0].Host != cHost {
		t.Errorf("чужой хост принят: %+v", rep.FileRefs[0])
	}
}

func TestCrawlDeduplicatesFiles(t *testing.T) {
	f := &stubCrawl{pages: map[string]string{
		"http://" + cHost + "/": `<a href="/same.zip">a</a><a href="/same.zip">b</a>`,
	}}
	cr := NewCrawler(f, silentLog{}, fastCrawl(CrawlConfig{Depth: 1}))
	_, rep := cr.Crawl(context.Background(), []string{cHost})

	if rep.Files != 1 {
		t.Errorf("дубль не схлопнут: %d", rep.Files)
	}
}

func TestCrawlFilesSorted(t *testing.T) {
	f := &stubCrawl{pages: map[string]string{
		"http://" + cHost + "/": `<a href="/z.zip">z</a><a href="/a.zip">a</a><a href="/m.zip">m</a>`,
	}}
	cr := NewCrawler(f, silentLog{}, fastCrawl(CrawlConfig{Depth: 1}))
	_, rep := cr.Crawl(context.Background(), []string{cHost})

	for i := 1; i < len(rep.FileRefs); i++ {
		if rep.FileRefs[i-1].URL > rep.FileRefs[i].URL {
			t.Fatalf("файлы не упорядочены: %+v", rep.FileRefs)
		}
	}
}

func TestCrawlFilesFromMultiplePages(t *testing.T) {
	// Обход идёт по onion-адресам, поэтому внутренняя страница того же
	// хоста достаётся обходу только если на неё ведёт onion-ссылка. Здесь
	// проверяется, что файлы собираются с каждой пройденной страницы.
	f := &stubCrawl{pages: map[string]string{
		"http://" + cHost + "/":     `<a href="/data">data</a><a href="/root.zip">r</a>`,
		"http://" + cHost + "/data": `<a href="/deep/backup.tar">b</a>`,
		"http://" + v3a + ".onion/": `<a href="/other.pdf">o</a>`,
		"http://" + v2b + ".onion/": `<a href="/x.7z">x</a>`,
		"http://" + v2c + ".onion/": `<a href="/y.rar">y</a>`,
	}}
	cr := NewCrawler(f, silentLog{}, fastCrawl(CrawlConfig{Depth: 1, MaxHosts: 10}))
	_, rep := cr.Crawl(context.Background(), []string{cHost})

	urls := map[string]bool{}
	for _, fr := range rep.FileRefs {
		urls[fr.URL] = true
	}
	if !urls["http://"+cHost+"/root.zip"] {
		t.Errorf("файл с главной не найден: %+v", rep.FileRefs)
	}
}

func TestCrawlReportCountsPages(t *testing.T) {
	f := &stubCrawl{pages: map[string]string{
		"http://" + cHost + "/": `<html><title>x</title></html>`,
	}}
	cr := NewCrawler(f, silentLog{}, fastCrawl(CrawlConfig{Depth: 1, MaxHosts: 5}))
	_, rep := cr.Crawl(context.Background(), []string{cHost})

	if rep.Ok != 1 {
		t.Errorf("Ok=%d, ожидала 1", rep.Ok)
	}
	if rep.Pages != 1 {
		t.Errorf("Pages=%d, ожидала 1 (глубина 1)", rep.Pages)
	}
}

func TestCrawlHTTPErrorRecorded(t *testing.T) {
	// Пустая карта страниц отдаёт 404 на всё.
	f := &stubCrawl{pages: map[string]string{}}
	cr := NewCrawler(f, silentLog{}, fastCrawl(CrawlConfig{Depth: 1}))
	_, rep := cr.Crawl(context.Background(), []string{cHost})

	if rep.Failed != 1 {
		t.Errorf("Failed=%d, ожидала 1", rep.Failed)
	}
	if rep.FileRefs != nil {
		t.Errorf("файлы с ошибки: %+v", rep.FileRefs)
	}
}

func TestCrawlCancelled(t *testing.T) {
	f := &stubCrawl{pages: map[string]string{
		"http://" + cHost + "/": `<a href="/x.zip">x</a>`,
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cr := NewCrawler(f, silentLog{}, fastCrawl(CrawlConfig{Depth: 2}))
	_, rep := cr.Crawl(ctx, []string{cHost})
	if rep.Files != 0 {
		t.Errorf("при отмене найдено файлов %d", rep.Files)
	}
}

func TestCrawlTitleRecorded(t *testing.T) {
	f := &stubCrawl{pages: map[string]string{
		"http://" + cHost + "/": `<html><title>Магазин вещей</title></html>`,
	}}
	cr := NewCrawler(f, silentLog{}, fastCrawl(CrawlConfig{Depth: 1}))
	_, rep := cr.Crawl(context.Background(), []string{cHost})
	if rep.Titles[cHost] != "Магазин вещей" {
		t.Errorf("заголовок не записан: %+v", rep.Titles)
	}
}

func TestCrawlSkipsNonOnionSeeds(t *testing.T) {
	f := &stubCrawl{pages: map[string]string{}}
	cr := NewCrawler(f, silentLog{}, fastCrawl(CrawlConfig{Depth: 1}))
	_, rep := cr.Crawl(context.Background(), []string{"example.com", ""})
	if rep.Pages != 0 || f.calls != 0 {
		t.Errorf("не-onion семена обошлись: страниц %d, запросов %d", rep.Pages, f.calls)
	}
}

func TestDedupeFileRefs(t *testing.T) {
	in := []filex.Ref{
		{URL: "a"}, {URL: "b"}, {URL: "a"}, {URL: "c"}, {URL: "b"},
	}
	got := dedupeFileRefs(in)
	if len(got) != 3 {
		t.Fatalf("после дедупа %d, ожидала 3: %+v", len(got), got)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].URL == got[i].URL {
			t.Fatalf("дубль остался: %+v", got)
		}
	}
}

func TestDedupeFileRefsEmpty(t *testing.T) {
	if got := dedupeFileRefs(nil); len(got) != 0 {
		t.Errorf("из nil получено %+v", got)
	}
}

func TestMeasureSizes(t *testing.T) {
	f := &stubCrawl{
		pages: map[string]string{},
		head:  http.Header{"Content-Length": []string{"4242"}},
	}
	cr := NewCrawler(f, silentLog{}, fastCrawl(CrawlConfig{Depth: 1}))
	refs := []filex.Ref{
		{URL: "http://a.onion/x.zip", Host: "a.onion"},
		{URL: "http://a.onion/y.pdf", Host: "a.onion"},
	}
	got := cr.MeasureSizes(context.Background(), refs, 0)
	for i, r := range got {
		if r.Size != 4242 {
			t.Errorf("размер %d не замерен: %d", i, r.Size)
		}
	}
}

func TestMeasureSizesSkipsKnown(t *testing.T) {
	f := &stubCrawl{
		pages: map[string]string{},
		head:  http.Header{"Content-Length": []string{"999"}},
	}
	cr := NewCrawler(f, silentLog{}, fastCrawl(CrawlConfig{Depth: 1}))
	refs := []filex.Ref{{URL: "http://a.onion/x.zip", Host: "a.onion", Size: 777}}
	got := cr.MeasureSizes(context.Background(), refs, 0)
	if got[0].Size != 777 {
		t.Errorf("известный размер перезаписан: %d", got[0].Size)
	}
	if f.sizes != 0 {
		t.Errorf("запрос по известному размеру сделан: %d", f.sizes)
	}
}

func TestMeasureSizesLimit(t *testing.T) {
	f := &stubCrawl{
		pages: map[string]string{},
		head:  http.Header{"Content-Length": []string{"10"}},
	}
	cr := NewCrawler(f, silentLog{}, fastCrawl(CrawlConfig{Depth: 1}))
	refs := []filex.Ref{
		{URL: "http://a.onion/1.zip", Host: "a.onion"},
		{URL: "http://a.onion/2.zip", Host: "a.onion"},
		{URL: "http://a.onion/3.zip", Host: "a.onion"},
	}
	got := cr.MeasureSizes(context.Background(), refs, 2)
	if len(got) != 2 {
		t.Errorf("потолок замеров не сработал: %d", len(got))
	}
}

func TestMeasureSizesWithoutClient(t *testing.T) {
	cr := NewCrawler(nil, silentLog{}, CrawlConfig{Depth: 1})
	refs := []filex.Ref{{URL: "http://a.onion/x.zip"}}
	got := cr.MeasureSizes(context.Background(), refs, 0)
	if len(got) != 1 || got[0].Size != 0 {
		t.Errorf("без клиента результат изменился: %+v", got)
	}
}

func TestMeasureSizesEmpty(t *testing.T) {
	cr := NewCrawler(&stubCrawl{}, silentLog{}, fastCrawl(CrawlConfig{Depth: 1}))
	if got := cr.MeasureSizes(context.Background(), nil, 0); len(got) != 0 {
		t.Errorf("из nil получено %+v", got)
	}
}

func TestHeaderSize(t *testing.T) {
	if n, ok := headerSize(http.Header{"Content-Length": []string{"123"}}); !ok || n != 123 {
		t.Errorf("headerSize = %d, %v", n, ok)
	}
	if _, ok := headerSize(http.Header{"Content-Length": []string{"abc"}}); ok {
		t.Error("нечисловое значение принято")
	}
	if _, ok := headerSize(nil); ok {
		t.Error("nil-заголовки приняты")
	}
	if _, ok := headerSize(http.Header{}); ok {
		t.Error("пустой заголовок принят")
	}
}

func TestCrawlPageTitle(t *testing.T) {
	cases := []struct{ in, want string }{
		{"<html><title>hello</title></html>", "hello"},
		{"<TITLE>UP</TITLE>", "UP"},
		{"<title>  spaced  </title>", "spaced"},
		{"no title", ""},
		{"<title>unclosed", "unclosed"},
	}
	for _, c := range cases {
		if got := pageTitle(c.in); got != c.want {
			t.Errorf("pageTitle(%q) = %q, ожидала %q", c.in, got, c.want)
		}
	}
}

func TestCrawlPageTitleTruncates(t *testing.T) {
	long := strings.Repeat("а", 200)
	got := pageTitle("<title>" + long + "</title>")
	if len([]rune(got)) > 120 {
		t.Errorf("заголовок не обрезан: %d", len([]rune(got)))
	}
}

func TestCrawlReportJSONNamesFileRefs(t *testing.T) {
	// Этап 168: под-отчёт crawl обязан отличать найденные ССЫЛКИ от
	// записанных файлов. Живой BEFORE (стенд after167b, повторный
	// discover): «files: 0» наверху и «files: 6» внутри crawl - ответ
	// противоречил сам себе, и читалось это как «нашли и потеряли»,
	// хотя записывать было нечего: всё уже в базе под прежней меткой.
	// Ключи json-именованы file_refs_found / file_refs, ключ files в
	// под-отчёте обязан исчезнуть.
	rep := CrawlReport{
		Pages:    1,
		Ok:       1,
		Found:    0,
		Files:    1,
		FileRefs: []filex.Ref{{URL: "http://" + cHost + "/d.zip", Filename: "d.zip", Ext: "zip"}},
		PageDetail: []Page{{
			Host:  cHost,
			URL:   "http://" + cHost + "/",
			Files: []filex.Ref{{URL: "http://" + cHost + "/d.zip", Filename: "d.zip", Ext: "zip"}},
		}},
	}
	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(raw)
	if !strings.Contains(s, `"file_refs_found":1`) {
		t.Errorf("счётчик найденных ссылок не назван file_refs_found: %s", s)
	}
	if strings.Contains(s, `"files":`) {
		t.Errorf("ключ files в под-отчёте жив: противоречие с верхним files не закрыто: %s", s)
	}
	page, err := json.Marshal(rep.PageDetail[0])
	if err != nil {
		t.Fatalf("marshal страницы: %v", err)
	}
	if !strings.Contains(string(page), `"file_refs":[{`) {
		t.Errorf("ссылки страницы не названы file_refs: %s", page)
	}
	if strings.Contains(string(page), `"files":`) {
		t.Errorf("страница всё ещё называет ссылки files: %s", page)
	}
}
