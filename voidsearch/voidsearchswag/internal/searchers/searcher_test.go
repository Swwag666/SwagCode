package searchers

import (
	"context"
	"strings"
	"testing"
)

func TestParseDDGHTMLResults(t *testing.T) {
	html := `<html><body>
	<div class="result">
	  <h2><a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2Fpage%3Fa%3D1">Example Page</a></h2>
	  <a class="result__snippet" href="#">First snippet about the topic</a>
	</div>
	<div class="result">
	  <h2><a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fother.org%2Fx">Other Site</a></h2>
	  <a class="result__snippet" href="#">Second snippet</a>
	</div>
	</body></html>`

	res, err := parseDDG([]byte(html), "ddg-html")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("результатов %d, ожидала 2", len(res))
	}
	if res[0].URL != "https://example.com/page?a=1" {
		t.Errorf("url0=%q, редирект не развёрнут", res[0].URL)
	}
	if res[0].Title != "Example Page" {
		t.Errorf("title0=%q", res[0].Title)
	}
	if res[0].Snippet != "First snippet about the topic" {
		t.Errorf("snippet0=%q", res[0].Snippet)
	}
	if res[0].Source != "ddg-html" {
		t.Errorf("source=%q", res[0].Source)
	}
	if res[1].URL != "https://other.org/x" {
		t.Errorf("url1=%q", res[1].URL)
	}
}

func TestParseDDGLiteTable(t *testing.T) {
	html := `<html><body><table>
	<tr><td><a class="result-link" href="https://lite.example/one">Lite One</a></td></tr>
	<tr><td class="result-snippet">Lite snippet one</td></tr>
	<tr><td><a class="result-link" href="https://lite.example/two">Lite Two</a></td></tr>
	<tr><td class="result-snippet">Lite snippet two</td></tr>
	</table></body></html>`

	res, err := parseDDG([]byte(html), "ddg-lite")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("результатов %d, ожидала 2", len(res))
	}
	if res[0].URL != "https://lite.example/one" || res[0].Title != "Lite One" {
		t.Errorf("res0=%+v", res[0])
	}
	if res[0].Snippet != "Lite snippet one" {
		t.Errorf("snippet=%q", res[0].Snippet)
	}
}

func TestParseDDGChallengeDetection(t *testing.T) {
	html := `<html><body><h1>Unusual traffic detected</h1>
	<p>Please complete the challenge</p></body></html>`
	_, err := parseDDG([]byte(html), "ddg-html")
	if err == nil {
		t.Fatal("антибот-заглушка должна давать ошибку")
	}
	if !strings.Contains(err.Error(), "антибот") {
		t.Errorf("err=%q, ожидала упоминание антибота", err.Error())
	}
}

func TestParseDDGEmptyPage(t *testing.T) {
	_, err := parseDDG([]byte(`<html><body></body></html>`), "ddg-html")
	if err == nil {
		t.Fatal("пустая страница должна давать ошибку")
	}
}

func TestParseDDGSkipsNonHTTP(t *testing.T) {
	html := `<div class="result">
	  <a class="result__a" href="javascript:void(0)">Bad</a>
	</div>
	<div class="result">
	  <a class="result__a" href="https://good.example/p">Good</a>
	</div>`
	res, err := parseDDG([]byte(html), "ddg-html")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, r := range res {
		if !strings.HasPrefix(r.URL, "http") {
			t.Errorf("не-HTTP ссылка прошла: %q", r.URL)
		}
	}
}

func TestResolveRedirect(t *testing.T) {
	cases := map[string]string{
		"//duckduckgo.com/l/?uddg=https%3A%2F%2Fx.com%2Fa":   "https://x.com/a",
		"https://duckduckgo.com/l/?uddg=https%3A%2F%2Fy.com": "https://y.com",
		"https://www.google.com/url?q=https://z.com/p&sa=D":  "https://z.com/p",
		"https://l.facebook.com/l.php?u=https%3A%2F%2Fq.com": "https://q.com",
		"https://plain.example/x":                            "https://plain.example/x",
		"//cdn.example/x":                                    "https://cdn.example/x",
		"":                                                   "",
	}
	for in, want := range cases {
		if got := ResolveRedirect(in); got != want {
			t.Errorf("ResolveRedirect(%q)=%q, ожидала %q", in, got, want)
		}
	}
}

func TestNormalizeURLStripsNoise(t *testing.T) {
	cases := map[string]string{
		"https://example.com/path#frag":                  "https://example.com/path",
		"https://www.example.com/":                       "https://example.com",
		"https://example.com/a?b=2&a=1":                  "https://example.com/a?a=1&b=2",
		"https://example.com/a?utm_source=x&gclid=y&z=1": "https://example.com/a?z=1",
		"HTTPS://EXAMPLE.COM/Path":                       "https://example.com/Path",
		"not a url":                                      "",
		"":                                               "",
	}
	for in, want := range cases {
		if got := NormalizeURL(in); got != want {
			t.Errorf("NormalizeURL(%q)=%q, ожидала %q", in, got, want)
		}
	}
}

func TestNormalizeURLDedupesEquivalent(t *testing.T) {
	a := NormalizeURL("https://www.example.com/page?utm_source=tg")
	b := NormalizeURL("https://example.com/page")
	if a != b {
		t.Errorf("эквивалентные URL не совпали: %q vs %q", a, b)
	}
}

func TestDedupeKeepsOrderAndLimit(t *testing.T) {
	in := []Result{
		{URL: "https://a.com/1", Title: "A"},
		{URL: "https://www.a.com/1", Title: "A dup"},
		{URL: "https://b.com/2", Title: "B"},
		{URL: "https://c.com/3", Title: "C"},
	}
	out := Dedupe(in, 2)
	if len(out) != 2 {
		t.Fatalf("после дедупа %d, ожидала 2", len(out))
	}
	if out[0].Title != "A" || out[1].Title != "B" {
		t.Errorf("порядок сломан: %+v", out)
	}
	if out[0].Rank != 1 || out[1].Rank != 2 {
		t.Errorf("rank не проставлен: %d %d", out[0].Rank, out[1].Rank)
	}
}

func TestDedupeDropsEmptyURL(t *testing.T) {
	out := Dedupe([]Result{{URL: ""}, {URL: "  "}, {URL: "https://ok.com"}}, 10)
	if len(out) != 1 {
		t.Errorf("пустые URL не отфильтрованы: %d", len(out))
	}
}

func TestDedupeMarksOnion(t *testing.T) {
	out := Dedupe([]Result{{URL: "http://abcdefghijklmnop.onion/x"}}, 10)
	if len(out) != 1 || !out[0].Onion {
		t.Errorf("onion-флаг не выставлен: %+v", out)
	}
}

func TestIsOnion(t *testing.T) {
	yes := []string{
		"http://abcdefghijklmnop.onion/",
		"http://duckduckgogg42xjoc72x3sjasowoarfbgcmvfimaftt6twagswzczad.onion/",
		"https://" + strings.Repeat("a", 56) + ".onion/x",
	}
	for _, u := range yes {
		if !IsOnion(u) {
			t.Errorf("%q не распознан как onion", u)
		}
	}
	no := []string{
		"https://example.com/onion",
		"https://onion.com",
		"http://short.onion/",
		"",
	}
	for _, u := range no {
		if IsOnion(u) {
			t.Errorf("%q ошибочно распознан как onion", u)
		}
	}
}

func TestParseBingHTTPOk(t *testing.T) {
	body := []byte(`<html><body><ol>
	<li class="b_algo"><h2><a href="https://h1.example/a">H one</a></h2><div class="b_caption"><p>S one</p></div></li>
	</ol></body></html>`)
	res, err := parseBing(body, "bing-html")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(res) != 1 || res[0].Source != "bing-html" {
		t.Errorf("результат неверен: %+v", res)
	}
}

func TestParseBing(t *testing.T) {
	body := []byte(`<html><body><ol>
	<li class="b_algo"><h2><a href="https://b1.example/x">Bing First</a></h2><div class="b_caption"><p>About first</p></div></li>
	<li class="b_algo"><h2><a href="https://bing.com/search?q=x">своя навигация</a></h2></li>
	</ol></body></html>`)
	res, err := parseBing(body, "rod-browser")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("результатов %d, ожидала 1 (bing-навигация отсечена)", len(res))
	}
	if res[0].Title != "Bing First" || res[0].Snippet != "About first" {
		t.Errorf("результат разобран неверно: %+v", res[0])
	}
}

func TestParseBingEmpty(t *testing.T) {
	if _, err := parseBing([]byte(`<html><body><p>пусто</p></body></html>`), "rod-browser"); err == nil {
		t.Error("пустая выдача принята без ошибки")
	}
}

func TestParseOnionLinks(t *testing.T) {
	html := `<html><body>
	<a href="http://abc.onion/page">Onion Page</a>
	<a href="https://clear.example/x">Clear</a>
	</body></html>`
	res, err := parseOnion([]byte(html), "test-onion", "")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("ссылок %d, ожидала 2", len(res))
	}
	if !res[0].Onion {
		t.Errorf(".onion не помечен: %+v", res[0])
	}
	if res[1].Onion {
		t.Errorf("clearnet ошибочно помечен onion: %+v", res[1])
	}
}

func TestParseOnionSelector(t *testing.T) {
	html := `<html><body>
	<div class="item"><a href="http://a.onion/1">One</a></div>
	<div class="other"><a href="http://b.onion/2">Two</a></div>
	</body></html>`
	res, err := parseOnion([]byte(html), "test", "div.item a")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("по селектору %d, ожидала 1", len(res))
	}
	if res[0].URL != "http://a.onion/1" {
		t.Errorf("url=%q", res[0].URL)
	}
}

func TestParseOnionNoLinks(t *testing.T) {
	// Этап 182: пустая выдача - не отказ движка, ок с нулём результатов.
	res, err := parseOnion([]byte(`<html><body>no links</body></html>`), "t", "")
	if err != nil {
		t.Fatalf("пустая страница - не ошибка: %v", err)
	}
	if len(res) != 0 {
		t.Errorf("пустая страница отдала %d результатов", len(res))
	}
}

func TestParseGenericHTMLFiltersNoise(t *testing.T) {
	html := `<html><body>
	<a href="https://google.com/search?q=x">Search</a>
	<a href="https://facebook.com/page">FB</a>
	<a href="https://real-site.example/article">Real Article</a>
	<a href="#anchor">Anchor</a>
	<a href="javascript:void(0)">JS</a>
	</body></html>`
	res, err := ParseGenericHTML([]byte(html), "generic")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("после фильтра %d, ожидала 1: %+v", len(res), res)
	}
	if res[0].URL != "https://real-site.example/article" {
		t.Errorf("url=%q", res[0].URL)
	}
}

func TestParseGenericHTMLNoLinks(t *testing.T) {
	// Этап 182: универсальный парсер повторяет семантику parseOnion -
	// пустая выдача ок, а не errNoLinks.
	res, err := ParseGenericHTML([]byte(`<html><body>text only</body></html>`), "g")
	if err != nil {
		t.Fatalf("без ссылок - не ошибка: %v", err)
	}
	if len(res) != 0 {
		t.Errorf("без ссылок отдало %d результатов", len(res))
	}
}

func TestOnionEngineTargetBuilding(t *testing.T) {
	e := &OnionEngine{Name_: "t", Base: "http://x.onion", Path: "/search?q={q}"}
	got := e.target("hello world")
	if !strings.Contains(got, "hello+world") {
		t.Errorf("target=%q, запрос не закодирован", got)
	}
	if !strings.HasPrefix(got, "http://x.onion/search?q=") {
		t.Errorf("target=%q", got)
	}

	e2 := &OnionEngine{Name_: "t2", Base: "http://y.onion/", Path: "/find"}
	got2 := e2.target("q1")
	if got2 != "http://y.onion/find?q=q1" {
		t.Errorf("target=%q", got2)
	}

	e3 := &OnionEngine{Name_: "t3", Base: "http://z.onion", Path: ""}
	got3 := e3.target("q2")
	if got3 != "http://z.onion/search?q=q2" {
		t.Errorf("target по умолчанию=%q", got3)
	}
}

func TestChainFallsThroughEngines(t *testing.T) {
	failing := &fakeSearcher{name: "bad", err: errFake}
	working := &fakeSearcher{name: "good", res: []Result{{URL: "https://ok.example/1", Title: "OK"}}}

	chain := NewChain(nil, failing, working)
	res, err := chain.Search(newCtx(), "q", 10)
	if err != nil {
		t.Fatalf("цепочка должна была выжить: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("результатов %d, ожидала 1", len(res))
	}
}

func TestChainAllFailGivesError(t *testing.T) {
	chain := NewChain(nil,
		&fakeSearcher{name: "a", err: errFake},
		&fakeSearcher{name: "b", err: errFake})
	_, err := chain.Search(newCtx(), "q", 10)
	if err == nil {
		t.Fatal("все упали - должна быть ошибка")
	}
	if !strings.Contains(err.Error(), "a") || !strings.Contains(err.Error(), "b") {
		t.Errorf("в ошибке нет имён движков: %v", err)
	}
}

func TestChainEmptyMeansFallthrough(t *testing.T) {
	empty := &fakeSearcher{name: "empty", res: nil}
	real := &fakeSearcher{name: "real", res: []Result{{URL: "https://r.example/1"}}}
	chain := NewChain(nil, empty, real)
	rep := chain.SearchDetailed(newCtx(), "q", 10)
	if len(rep.Results) != 1 {
		t.Fatalf("результатов %d, ожидала 1", len(rep.Results))
	}
	// Пустой движок отмечается отдельно от упавшего. Раньше он попадал в Failed
	// с текстом "пустая выдача", и Search из-за этого сообщал «все поисковики
	// упали» в ситуации, когда поиск сработал и честно ничего не нашёл.
	if len(rep.Empty) != 1 || rep.Empty[0] != "empty" {
		t.Errorf("Empty = %v, ожидала [empty]", rep.Empty)
	}
	if _, ok := rep.Failed["empty"]; ok {
		t.Error("пустая выдача записана в Failed: это не отказ движка")
	}
}

func TestChainAllEmptyIsNotAnError(t *testing.T) {
	// Все движки отработали и ничего не нашли. Это не поломка: ошибка заставила
	// бы клиента повторять запрос, менять режим или сообщать, что сервер сломан.
	chain := NewChain(nil,
		&fakeSearcher{name: "a", res: nil},
		&fakeSearcher{name: "b", res: nil})
	res, err := chain.Search(newCtx(), "q", 10)
	if err != nil {
		t.Fatalf("честно пустая выдача вернула ошибку: %v", err)
	}
	if len(res) != 0 {
		t.Errorf("результатов %d, ожидала 0", len(res))
	}
	rep := chain.SearchDetailed(newCtx(), "q", 10)
	if len(rep.Failed) != 0 {
		t.Errorf("Failed не пуст: %v", rep.Failed)
	}
	if len(rep.Empty) != 2 {
		t.Errorf("Empty = %v, ожидала оба движка", rep.Empty)
	}
}

func TestChainMixedEmptyAndFailedReportsBoth(t *testing.T) {
	// Один упал, один пуст. «Все поисковики упали» здесь неверно, и молчать
	// тоже нельзя: частичный отказ обязан быть виден.
	chain := NewChain(nil,
		&fakeSearcher{name: "down", err: errFake},
		&fakeSearcher{name: "empty", res: nil})
	_, err := chain.Search(newCtx(), "q", 10)
	if err == nil {
		t.Fatal("отказ движка не вызвал ошибку")
	}
	if !strings.Contains(err.Error(), "down") {
		t.Errorf("в ошибке нет упавшего движка: %v", err)
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("в ошибке нет движка с пустой выдачей: %v", err)
	}
}

func TestChainStopsAtLimit(t *testing.T) {
	big := &fakeSearcher{name: "big", res: []Result{
		{URL: "https://a.example/1"}, {URL: "https://b.example/2"},
		{URL: "https://c.example/3"}, {URL: "https://d.example/4"},
	}}
	never := &fakeSearcher{name: "never", err: errFake}
	chain := NewChain(nil, big, never)
	res, err := chain.Search(newCtx(), "q", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 {
		t.Errorf("результатов %d, ожидала 2 (лимит)", len(res))
	}
}

type fakeSearcher struct {
	name string
	res  []Result
	err  error
}

func (f *fakeSearcher) Name() string { return f.name }
func (f *fakeSearcher) Search(_ context.Context, _ string, _ int) ([]Result, error) {
	return f.res, f.err
}

var errFake = errFakeT{}

type errFakeT struct{}

func (errFakeT) Error() string { return "fake failure" }

func newCtx() context.Context { return context.Background() }
