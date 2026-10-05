package parser

import (
	"context"
	"strings"
	"testing"

	"voidsearchswag/internal/store"
)

type stubFetch struct {
	body  string
	err   error
	calls int
}

func (s *stubFetch) FetchURL(_ context.Context, _ string) (Response, error) {
	s.calls++
	if s.err != nil {
		return Response{}, s.err
	}
	return Response{Status: 200, Body: []byte(s.body)}, nil
}

func openParser(t *testing.T, body string) (*Parser, *stubFetch) {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/p.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	f := &stubFetch{body: body}
	return &Parser{Store: st, Fetch: f}, f
}

const sampleHTML = `<html><head><title>Магазин книг</title>
<meta name="description" content="книги дёшево"></head><body>
<h1 class="shop-title main">Каталог</h1>
<div class="price-tag old">1 990 ₽</div>
<a href="mailto:shop@example.com">написать</a>
</body></html>`

func TestHostPattern(t *testing.T) {
	got, err := HostPattern("https://Example.COM:8080/x?q=1")
	if err != nil || got != "example.com" {
		t.Errorf("pattern=%q err=%v", got, err)
	}
	if _, err := HostPattern("://мусор"); err == nil {
		t.Error("битый URL принят")
	}
	if _, err := HostPattern(""); err == nil {
		t.Error("пустой URL принят")
	}
}

func TestSkeletonSkipsScripts(t *testing.T) {
	body := `<html><head><script>var x=1</script><style>.a{}</style><title>T</title></head>
<body><h1 class="a">Hi</h1></body></html>`
	sk := SkeletonOf(body, 4000)
	if strings.Contains(sk, "script") || strings.Contains(sk, "style") {
		t.Errorf("мусор в скелете:\n%s", sk)
	}
	if !strings.Contains(sk, "h1.a") {
		t.Errorf("нет узла h1:\n%s", sk)
	}
	if !strings.Contains(sk, `"Hi"`) {
		t.Errorf("нет текста узла:\n%s", sk)
	}
}

func TestSkeletonCapsLength(t *testing.T) {
	body := "<html><body>" + strings.Repeat("<div class=\"x\">t</div>", 500) + "</body></html>"
	if got := SkeletonOf(body, 500); len(got) > 500 {
		t.Errorf("скелет %d при лимите 500", len(got))
	}
}

func TestParseExactSelector(t *testing.T) {
	p, _ := openParser(t, sampleHTML)
	ctx := context.Background()
	if err := p.Save(ctx, "example.com", "price", ".price-tag", "css"); err != nil {
		t.Fatal(err)
	}
	out, healed, err := p.Parse(ctx, "http://example.com/x", []string{"price"})
	if err != nil {
		t.Fatal(err)
	}
	r := out["price"]
	if r.Value != "1 990 ₽" {
		t.Errorf("value=%q", r.Value)
	}
	if r.Confidence != ConfExact || r.Strategy != "css" || healed {
		t.Errorf("точный разбор обязан быть без исцеления: %+v healed=%v", r, healed)
	}
}

func TestParseFuzzyHealsRenamedClass(t *testing.T) {
	p, _ := openParser(t, `<div class="price-tag-new v2">2 990 ₽</div>`)
	ctx := context.Background()
	// Селектор со старым классом: точное совпадение падает...
	if err := p.Save(ctx, "example.com", "price", ".price-tag", "css"); err != nil {
		t.Fatal(err)
	}
	// ...но класс содержит старый токен, нечеткий подбор спасает.
	// price-tag-new содержит токен price-tag как подстроку.
	out, healed, err := p.Parse(ctx, "http://example.com/x", []string{"price"})
	if err != nil {
		t.Fatal(err)
	}
	r := out["price"]
	if r.Value != "2 990 ₽" {
		t.Errorf("value=%q", r.Value)
	}
	if !healed || r.Strategy != "fuzzy" || r.Confidence != ConfFuzzy {
		t.Errorf("ожидалось нечеткое исцеление: %+v healed=%v", r, healed)
	}
}

func TestParseRegexFallback(t *testing.T) {
	p, _ := openParser(t, `<html><body><p>артикул: ABC-1234, цена договорная</p></body></html>`)
	ctx := context.Background()
	out, healed, err := p.Parse(ctx, "http://example.com/x", []string{"артикул"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out["артикул"].Value, "ABC-1234") {
		t.Errorf("regex не сработал: %+v", out["артикул"])
	}
	if !healed {
		t.Error("запасная стратегия обязана помечать healed")
	}
}

func TestParseStructuralEmail(t *testing.T) {
	p, _ := openParser(t, sampleHTML)
	ctx := context.Background()
	out, _, err := p.Parse(ctx, "http://example.com/x", []string{"email"})
	if err != nil {
		t.Fatal(err)
	}
	if out["email"].Value != "shop@example.com" {
		t.Errorf("email=%q", out["email"].Value)
	}
}

func TestParseGenericTitle(t *testing.T) {
	p, _ := openParser(t, sampleHTML)
	ctx := context.Background()
	out, _, err := p.Parse(ctx, "http://example.com/x", []string{"title"})
	if err != nil {
		t.Fatal(err)
	}
	if out["title"].Value != "Магазин книг" {
		t.Errorf("title=%q", out["title"].Value)
	}
}

func TestParseNothingFoundIsError(t *testing.T) {
	p, _ := openParser(t, `<html><body><p>пусто</p></body></html>`)
	if _, _, err := p.Parse(context.Background(), "http://example.com/x", []string{"price"}); err == nil {
		t.Error("пустое извлечение принято без ошибки")
	}
}

func TestParseEmptyFieldsRejected(t *testing.T) {
	p, _ := openParser(t, sampleHTML)
	if _, _, err := p.Parse(context.Background(), "http://example.com/x", nil); err == nil {
		t.Error("пустые поля приняты")
	}
}

func TestParseBadURLRejected(t *testing.T) {
	p, _ := openParser(t, sampleHTML)
	if _, _, err := p.Parse(context.Background(), "://мусор", []string{"title"}); err == nil {
		t.Error("битый URL принят")
	}
}

func TestSaveRequiresFields(t *testing.T) {
	p, _ := openParser(t, sampleHTML)
	ctx := context.Background()
	if err := p.Save(ctx, "", "title", "h1", ""); err == nil {
		t.Error("пустой pattern принят")
	}
	if err := p.Save(ctx, "example.com", "", "h1", ""); err == nil {
		t.Error("пустое поле принято")
	}
}

func TestSkeletonNeedsFetcher(t *testing.T) {
	p := &Parser{}
	if _, _, err := p.Skeleton(context.Background(), "http://example.com/", 100); err == nil {
		t.Error("скелет без фетчера принят")
	}
}

func TestHealedConfidenceBelowThreshold(t *testing.T) {
	// healed=true всегда идёт с confidence<1: вызывающий по этому признаку
	// решает, звать ли LLM-клиента на регенерацию.
	p, _ := openParser(t, sampleHTML)
	out, _, err := p.Parse(context.Background(), "http://example.com/x", []string{"email"})
	if err != nil {
		t.Fatal(err)
	}
	if out["email"].Confidence >= 1.0 {
		t.Errorf("исцелённое значение с confidence 1.0: %+v", out["email"])
	}
}

func TestParseXPathSelector(t *testing.T) {
	// Классы переименованы целиком: CSS и fuzzy мертвы, а структурный путь
	// жив - XPath переживает редизайн классов.
	p, _ := openParser(t, `<html><body><div class="zz9"><span class="qq1">42 000 ₽</span></div></body></html>`)
	ctx := context.Background()
	if err := p.Save(ctx, "example.com", "price", `//div/span`, "xpath"); err != nil {
		t.Fatal(err)
	}
	out, healed, err := p.Parse(ctx, "http://example.com/x", []string{"price"})
	if err != nil {
		t.Fatal(err)
	}
	r := out["price"]
	if r.Value != "42 000 ₽" {
		t.Errorf("value=%q", r.Value)
	}
	if healed || r.Strategy != "xpath" || r.Confidence != ConfXPath {
		t.Errorf("точный xpath обязан быть без исцеления: %+v healed=%v", r, healed)
	}
}

func TestParseXPathFallsThrough(t *testing.T) {
	// XPath мимо: падает дальше по цепочке, а не в ошибку.
	p, _ := openParser(t, sampleHTML)
	ctx := context.Background()
	if err := p.Save(ctx, "example.com", "price", `//table[@id="nope"]/tr/td`, "xpath"); err != nil {
		t.Fatal(err)
	}
	out, healed, err := p.Parse(ctx, "http://example.com/x", []string{"price"})
	if err != nil {
		t.Fatal(err)
	}
	// В sampleHTML цены в .price-tag нет, но есть текст "1 990 ₽" -
	// regex/structural/generic его не найдут как price... проверяем, что
	// промахнувшийся xpath не дал ложного точного попадания.
	if r := out["price"]; r.Strategy == "xpath" && !healed {
		t.Errorf("промахнувшийся xpath выдан за точный: %+v", r)
	}
}

func TestXPathTextRejectsGarbage(t *testing.T) {
	if got := xpathText(`<html><body><p>x</p></body></html>`, `///[[[`); got != "" {
		t.Errorf("битое выражение дало %q", got)
	}
	if got := xpathText(`<html><body><p>x</p></body></html>`, ""); got != "" {
		t.Errorf("пустое выражение дало %q", got)
	}
	if got := xpathText(`<html><body><p>x</p></body></html>`, `//table/tr`); got != "" {
		t.Errorf("отсутствующий узел дал %q", got)
	}
}
