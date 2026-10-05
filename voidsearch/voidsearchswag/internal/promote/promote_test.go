package promote

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"voidsearchswag/internal/httpc"
	"voidsearchswag/internal/searchers"
)

func TestDetectFindsSearchForm(t *testing.T) {
	body := []byte(`<html><body>
<form action="/search" method="get">
<input type="text" name="q"><input type="submit" value="Go">
</form></body></html>`)
	cand, ok := Detect(body, "http://abcdefabcdefabcd.onion/")
	if !ok {
		t.Fatal("форма не распознана")
	}
	if cand.Name != "abcdefabcdefabcd" {
		t.Errorf("имя %q", cand.Name)
	}
	if cand.Base != "http://abcdefabcdefabcd.onion" {
		t.Errorf("база %q", cand.Base)
	}
	if cand.Path != "/search?q={q}" {
		t.Errorf("путь %q", cand.Path)
	}
}

func TestDetectSkipsLoginForm(t *testing.T) {
	body := []byte(`<html><body>
<form action="/login" method="post">
<input type="text" name="user"><input type="password" name="pw">
</form></body></html>`)
	if _, ok := Detect(body, "http://abcdefabcdefabcd.onion/login"); ok {
		t.Error("форма входа признана поисковой")
	}
}

func TestDetectRejectsForeignAction(t *testing.T) {
	body := []byte(`<html><body>
<form action="https://evil.example/search"><input type="text" name="q"></form>
</body></html>`)
	if _, ok := Detect(body, "http://abcdefabcdefabcd.onion/"); ok {
		t.Error("форма на чужой хост принята")
	}
}

func TestDetectNoForm(t *testing.T) {
	if _, ok := Detect([]byte(`<html><body><p>текст</p></body></html>`), "http://a.onion/"); ok {
		t.Error("поиск найден там, где форм нет")
	}
	if _, ok := Detect([]byte(`<form></form>`), "://мусор"); ok {
		t.Error("битый URL принят")
	}
}

func directPromoter(t *testing.T) *Promoter {
	t.Helper()
	c, err := httpc.NewClient(context.Background(), httpc.Options{Transport: "direct", Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return &Promoter{Client: c, MinOnionHits: 2, Timeout: 10 * time.Second}
}

func TestCheckEndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Write([]byte(`<html><body>
<form action="/s" method="get"><input type="search" name="query"></form>
</body></html>`))
			return
		}
		w.Write([]byte(`<html><body>
<a href="http://a1a1a1a1a1a1a1a1.onion/1">one has text</a>
<a href="http://b2b2b2b2b2b2b2.onion/2">two has text</a>
</body></html>`))
	}))
	defer srv.Close()

	p := directPromoter(t)
	cand, n, err := p.Check(context.Background(), srv.URL+"/")
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if n != 2 {
		t.Errorf("ссылок %d, ожидала 2", n)
	}
	if !strings.HasPrefix(cand.Path, "/s?") || !strings.Contains(cand.Path, "{q}") {
		t.Errorf("путь кандидата %q", cand.Path)
	}
}

func TestCheckNoForm(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html><body><p>пусто</p></body></html>`))
	}))
	defer srv.Close()

	p := directPromoter(t)
	if _, _, err := p.Check(context.Background(), srv.URL+"/"); err == nil {
		t.Error("страница без формы прошла проверку")
	}
}

func TestCheckNeedsClient(t *testing.T) {
	p := &Promoter{}
	if _, _, err := p.Check(context.Background(), "http://a.onion/"); err == nil {
		t.Error("проверка без клиента прошла")
	}
}

func TestAttachAddsEngine(t *testing.T) {
	cat := &searchers.OnionCatalog{}
	hp := searchers.NewHealthPool(nil, nil)
	cand := Candidate{Name: "newengine", Base: "http://newengine.onion", Path: "/s?q={q}", Selector: "a[href*='.onion']"}

	if !Attach(cat, hp, nil, cand) {
		t.Fatal("кандидат не прицепился")
	}
	if len(cat.Engines) != 1 || cat.Engines[0].Name() != "newengine" {
		t.Errorf("каталог неверен: %+v", cat.Engines)
	}
	if _, ok := hp.Get("newengine"); !ok {
		t.Error("движок не зарегистрирован в пуле здоровья")
	}
	// Повтор - дубликат, а не второй движок.
	if Attach(cat, hp, nil, cand) {
		t.Error("дубликат прицепился второй раз")
	}
	if len(cat.Engines) != 1 {
		t.Errorf("движков %d", len(cat.Engines))
	}
}

func TestAttachRejectsGarbage(t *testing.T) {
	cat := &searchers.OnionCatalog{}
	if Attach(nil, nil, nil, Candidate{Name: "x", Base: "http://x.onion"}) {
		t.Error("цепка к nil-каталогу прошла")
	}
	if Attach(cat, nil, nil, Candidate{Name: "", Base: "http://x.onion"}) {
		t.Error("безымянный кандидат принят")
	}
	if Attach(cat, nil, nil, Candidate{}) {
		t.Error("пустой кандидат принят")
	}
	if len(cat.Engines) != 0 {
		t.Errorf("мусор в каталоге: %+v", cat.Engines)
	}
}

// TestCheckNormalizesBareHost - регрессия на дефект, из-за которого промоут
// не работал вовсе. Вызывающие (CLI, MCP, фоновый тик) передают store.Onion.URL,
// а пул хранит голые хосты без схемы. url.Parse на таком адресе не находит
// схему, и клиент падал с «invalid URL scheme: []»: каждый живой сервис
// выглядел мёртвым, и в каталог не поднималось ничего.
func TestCheckNormalizesBareHost(t *testing.T) {
	p := directPromoter(t)
	ctx := context.Background()

	// Адрес без схемы на закрытом порту: ошибка обязана быть про соединение,
	// а не про отсутствие схемы. Именно это отличает нормализованный запрос
	// от прежнего поведения.
	_, _, err := p.Check(ctx, "127.0.0.1:1")
	if err == nil {
		t.Fatal("запрос на закрытый порт прошёл")
	}
	msg := err.Error()
	if strings.Contains(msg, "invalid URL scheme") {
		t.Errorf("голый хост не нормализован, схема так и не добавлена: %v", err)
	}
	if !strings.Contains(msg, "127.0.0.1:1") {
		t.Errorf("в ошибке нет адреса - запрос ушёл не туда: %v", err)
	}
}

func TestCheckRejectsEmptyURL(t *testing.T) {
	p := directPromoter(t)
	for _, in := range []string{"", "   ", "\t"} {
		if _, _, err := p.Check(context.Background(), in); err == nil {
			t.Errorf("пустой адрес %q принят", in)
		} else if !strings.Contains(err.Error(), "пустой адрес") {
			t.Errorf("ошибка не объясняет причину для %q: %v", in, err)
		}
	}
}

func TestCheckAcceptsFullURL(t *testing.T) {
	// Полный URL нормализация обязана оставить рабочим: иначе починка
	// голых хостов сломала бы вызовы, которые и так передавали схему.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`<html><body><form action="/s" method="get">
<input type="search" name="q"></form></body></html>`))
	}))
	defer srv.Close()

	p := directPromoter(t)
	// Формы мало для промоута, но запрос обязан дойти до сервера и получить
	// ответ, а не упасть на разборе адреса.
	_, _, err := p.Check(context.Background(), srv.URL+"/")
	if err == nil {
		return
	}
	if strings.Contains(err.Error(), "invalid URL scheme") {
		t.Errorf("полный URL сломан нормализацией: %v", err)
	}
}
