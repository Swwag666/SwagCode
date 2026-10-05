package searchers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseSeedsDefaults(t *testing.T) {
	for _, spec := range []string{"", "   ", "\t\n"} {
		got, err := ParseSeeds(spec)
		if err != nil {
			t.Fatalf("%q: %v", spec, err)
		}
		if len(got) != len(DefaultSeeds()) {
			t.Errorf("%q: сидов %d, ожидала %d", spec, len(got), len(DefaultSeeds()))
		}
	}
}

func TestParseSeedsGarbageFallsBackToDefaults(t *testing.T) {
	// Мусор в конфиге не должен оставлять пользователя без поисковиков:
	// молчаливый откат к дефолтам лучше пустого каталога движков.
	for _, spec := range []string{"мусор", "a,b,c", "|||", ",,,"} {
		got, err := ParseSeeds(spec)
		if err != nil {
			t.Fatalf("%q: %v", spec, err)
		}
		if len(got) == 0 {
			t.Errorf("%q: сидов не осталось", spec)
		}
		if len(got) != len(DefaultSeeds()) {
			t.Errorf("%q: сидов %d, ожидала откат к %d", spec, len(got), len(DefaultSeeds()))
		}
	}
}

func TestParseSeedsRejectsEmptyFields(t *testing.T) {
	// Ни один вариант пустого имени или базы не должен породить движок:
	// такой движок не может построить URL и тихо отдаёт пустоту.
	for _, spec := range []string{
		"|http://a.onion",
		"имя|",
		"|",
		"   |   ",
		"|||",
		"|||,|||",
	} {
		got, err := ParseSeeds(spec)
		if err != nil {
			t.Fatalf("%q: %v", spec, err)
		}
		for _, s := range got {
			if strings.TrimSpace(s.Name) == "" || strings.TrimSpace(s.Base) == "" {
				t.Errorf("%q: сид с пустым полем %+v", spec, s)
			}
		}
	}
}

func TestParseSeedsRejectsBaseWithoutHost(t *testing.T) {
	// Без хоста target() склеивает относительный путь, и запрос уходит в
	// никуда: такой сид обязан отбрасываться на разборе конфига.
	for _, spec := range []string{
		"имя|/search?q={q}",
		"имя|мусор",
		"имя|http://",
	} {
		got, err := ParseSeeds(spec)
		if err != nil {
			t.Fatalf("%q: %v", spec, err)
		}
		if len(got) != len(DefaultSeeds()) {
			t.Errorf("%q: сидов %d, ожидала откат к дефолтам (%d)", spec, len(got), len(DefaultSeeds()))
		}
	}
}

func TestParseSeedsDedupesByName(t *testing.T) {
	// Пул здоровья хранит записи в map по имени: два движка с одним именем
	// перетрут друг друга, и отчёт покажет один вместо двух.
	got, err := ParseSeeds("дубль|http://aaaaaaaaaaaaaaaa.onion,дубль|http://bbbbbbbbbbbbbbbb.onion")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("сидов %d, ожидала 1", len(got))
	}
	if got[0].Base != "http://aaaaaaaaaaaaaaaa.onion" {
		t.Errorf("первый дубль не выиграл: %q", got[0].Base)
	}
}

func TestParseSeedsPartialGarbageKeepsValid(t *testing.T) {
	// Мусор рядом с валидным сидом не должен утягивать его за собой:
	// откат к дефолтам нужен только когда валидных сидов нет вовсе.
	got, err := ParseSeeds("мусор,годный|http://abcdefghijklmnop.onion,|||")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("сидов %d, ожидала 1", len(got))
	}
	if got[0].Name != "годный" {
		t.Errorf("имя=%q", got[0].Name)
	}
}

func TestParseSeedsTrimsWhitespace(t *testing.T) {
	got, err := ParseSeeds("  имя  |  http://abcdefghijklmnop.onion  |  /f?s={q}  ")
	if err != nil {
		t.Fatal(err)
	}
	s := got[0]
	if s.Name != "имя" || s.Base != "http://abcdefghijklmnop.onion" || s.Path != "/f?s={q}" {
		t.Errorf("пробелы не обрезаны: %+v", s)
	}
}

func TestParseSeedsCustomMinimal(t *testing.T) {
	got, err := ParseSeeds("мой|http://abcdefghijklmnop.onion")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("сидов %d", len(got))
	}
	s := got[0]
	if s.Name != "мой" || s.Base != "http://abcdefghijklmnop.onion" {
		t.Errorf("имя/база: %q / %q", s.Name, s.Base)
	}
	// Путь и селектор подставляются по умолчанию, иначе движок не найдёт
	// ни результаты, ни ссылку на запрос.
	if s.Path != "/search?q={q}" {
		t.Errorf("Path=%q", s.Path)
	}
	if s.Selector != "a[href*='.onion']" {
		t.Errorf("Selector=%q", s.Selector)
	}
	if !s.ViaTor {
		t.Error("onion-сид не помечен как tor")
	}
}

func TestParseSeedsCustomFull(t *testing.T) {
	got, err := ParseSeeds("мой|http://abcdefghijklmnop.onion|/find?s={q}|div.hit a|files")
	if err != nil {
		t.Fatal(err)
	}
	s := got[0]
	if s.Path != "/find?s={q}" || s.Selector != "div.hit a" || s.Category != "files" {
		t.Errorf("поля не проброшены: %+v", s)
	}
}

func TestParseSeedsEmptyFieldsKeepDefaults(t *testing.T) {
	// Пустые поля между разделителями не должны сбрасывать значения в "":
	// пустой Path сломает построение URL запроса.
	got, err := ParseSeeds("мой|http://abcdefghijklmnop.onion|||")
	if err != nil {
		t.Fatal(err)
	}
	s := got[0]
	if s.Path != "/search?q={q}" || s.Selector != "a[href*='.onion']" {
		t.Errorf("дефолты затёрты: %+v", s)
	}
}

func TestParseSeedsMultipleAndWhitespace(t *testing.T) {
	got, err := ParseSeeds(" a|http://aaaaaaaaaaaaaaaa.onion , , b|http://bbbbbbbbbbbbbbbb.onion ")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("сидов %d, ожидала 2 (пустые части должны отбрасываться)", len(got))
	}
	if got[0].Name != "a" || got[1].Name != "b" {
		t.Errorf("имена %q / %q", got[0].Name, got[1].Name)
	}
}

func TestParseSeedsClearnetBaseDisablesTor(t *testing.T) {
	// Сид без .onion в базе - это clearnet-каталог: через tor exit-ноды
	// отдают challenge Cloudflare вместо страницы, поэтому метка обязана
	// сняться.
	got, err := ParseSeeds("clear|https://thehiddenwiki.org/")
	if err != nil {
		t.Fatal(err)
	}
	if got[0].ViaTor {
		t.Error("clearnet-сид помечен как tor")
	}

	onion, err := ParseSeeds("tor|http://abcdefghijklmnop.onion/")
	if err != nil {
		t.Fatal(err)
	}
	if !onion[0].ViaTor {
		t.Error("onion-сид с завершающим слешем не помечен как tor")
	}
}

func TestDefaultSeedsCopyIsIndependent(t *testing.T) {
	// DefaultSeeds возвращает копию: иначе вызывающий изменит общий срез и
	// следующий прогон получит испорченные сиды.
	a := DefaultSeeds()
	if len(a) == 0 {
		t.Fatal("дефолтных сидов нет")
	}
	a[0].Name = "испорчен"
	a[0].Base = "http://мусор"

	b := DefaultSeeds()
	if b[0].Name == "испорчен" {
		t.Error("изменение копии повлияло на общий список")
	}
	if b[0].Base == "http://мусор" {
		t.Error("база испорчена через копию")
	}
}

func TestEnginesFromSeeds(t *testing.T) {
	seeds := []Seed{
		{Name: "a", Base: "http://aaaaaaaaaaaaaaaa.onion", Path: "/s?q={q}", Selector: "a.x"},
		{Name: "b", Base: "http://bbbbbbbbbbbbbbbb.onion", Path: "/f", Selector: "a.y"},
	}
	got := EnginesFromSeeds(seeds)
	if len(got) != 2 {
		t.Fatalf("движков %d", len(got))
	}
	for i, e := range got {
		if e.Name_ != seeds[i].Name || e.Base != seeds[i].Base ||
			e.Path != seeds[i].Path || e.Selector != seeds[i].Selector {
			t.Errorf("движок %d не совпадает с сидом: %+v", i, e)
		}
		if e.Client != nil {
			t.Errorf("движок %d получил клиент до сборки ядра", i)
		}
	}
}

func TestEnginesFromSeedsEmpty(t *testing.T) {
	if got := EnginesFromSeeds(nil); len(got) != 0 {
		t.Errorf("из пустого списка движков %d", len(got))
	}
}

func TestEnginesFromSeedsRoundtrip(t *testing.T) {
	// Дефолтные сиды обязаны превращаться в рабочие движки: это основной
	// путь сборки ядра, и потеря поля здесь ломает весь onion-поиск.
	got := EnginesFromSeeds(DefaultSeeds())
	if len(got) != len(DefaultSeeds()) {
		t.Fatalf("движков %d, сидов %d", len(got), len(DefaultSeeds()))
	}
	for _, e := range got {
		if e.Name_ == "" || e.Base == "" || e.Path == "" || e.Selector == "" {
			t.Errorf("движок с пустым полем: %+v", e)
		}
		if !strings.Contains(e.Path, "{q}") {
			t.Errorf("путь %q без плейсхолдера запроса", e.Path)
		}
	}
}

func TestCacheGetPut(t *testing.T) {
	c := NewCache()
	if _, ok := c.Get("нет"); ok {
		t.Error("пустой кэш что-то вернул")
	}

	res := []Result{{URL: "http://a.example/1", Title: "A"}}
	c.Put("ключ", res)

	got, ok := c.Get("ключ")
	if !ok {
		t.Fatal("запись не найдена")
	}
	if len(got) != 1 || got[0].Title != "A" {
		t.Errorf("содержимое %v", got)
	}
}

func TestCacheOverwrite(t *testing.T) {
	c := NewCache()
	c.Put("k", []Result{{URL: "http://a.example/1"}})
	c.Put("k", []Result{{URL: "http://b.example/2"}})

	got, _ := c.Get("k")
	if len(got) != 1 || got[0].URL != "http://b.example/2" {
		t.Errorf("перезапись не сработала: %v", got)
	}
}

func TestCacheEmptyKey(t *testing.T) {
	c := NewCache()
	c.Put("", []Result{{URL: "http://a.example/1"}})
	if _, ok := c.Get(""); !ok {
		t.Error("пустой ключ не сохранён")
	}
}

func TestChainNames(t *testing.T) {
	c := NewChain(nil,
		&fakeSearcher{name: "ddg-html"},
		&fakeSearcher{name: "bing-html"},
		&fakeSearcher{name: "torch"},
	)
	got := c.Names()
	want := []string{"ddg-html", "bing-html", "torch"}
	if len(got) != len(want) {
		t.Fatalf("имён %d, ожидала %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("позиция %d: %q, ожидала %q", i, got[i], want[i])
		}
	}
}

func TestChainNamesEmpty(t *testing.T) {
	if got := NewChain(nil).Names(); len(got) != 0 {
		t.Errorf("пустая цепочка дала %v", got)
	}
}

func TestChainSearchNoEngines(t *testing.T) {
	_, err := NewChain(nil).Search(context.Background(), "q", 5)
	if err == nil {
		t.Fatal("пустая цепочка не вернула ошибку")
	}
	if !strings.Contains(err.Error(), "ни одного поисковика") {
		t.Errorf("ошибка не объясняет причину: %v", err)
	}
}

func TestChainPerEngineDefault(t *testing.T) {
	c := NewChain(nil)
	if c.PerEngine <= 0 {
		t.Errorf("PerEngine=%v: нулевой бюджет движка убьёт весь поиск", c.PerEngine)
	}
}

func TestChainLogsWithoutLogger(t *testing.T) {
	c := NewChain(nil, &fakeSearcher{name: "a", err: errFake})
	c.logf("сообщение %d", 1)
}

func TestDuckDuckGoName(t *testing.T) {
	if got := (&DuckDuckGo{}).Name(); got != "ddg-html" {
		t.Errorf("Name=%q", got)
	}
	if got := (&DuckDuckGo{Lite: true}).Name(); got != "ddg-lite" {
		t.Errorf("lite Name=%q", got)
	}
}

func TestDuckDuckGoEndpoint(t *testing.T) {
	d := &DuckDuckGo{}
	if got := d.endpoint("hello world"); !strings.HasPrefix(got, "https://html.duckduckgo.com/html/?q=hello+world") {
		t.Errorf("endpoint=%q", got)
	}
	lite := &DuckDuckGo{Lite: true}
	if got := lite.endpoint("a b"); !strings.HasPrefix(got, "https://lite.duckduckgo.com/lite/?q=a+b") {
		t.Errorf("lite endpoint=%q", got)
	}
}

func TestDuckDuckGoSearchWithoutClient(t *testing.T) {
	if _, err := (&DuckDuckGo{}).Search(context.Background(), "q", 5); err == nil {
		t.Error("поиск без клиента не вернул ошибку")
	}
}

func TestDuckDuckGoSearchParses(t *testing.T) {
	body := `<div class="result">
<a class="result__a" href="http://aaaaaaaaaaaaaaaa.onion/">Каталог</a>
<span class="result__snippet">файлы и дампы</span>
</div>
<div class="result">
<h2><a href="http://bbbbbbbbbbbbbbbb.onion/">Форум</a></h2>
</div>`
	// Endpoint у DuckDuckGo зашит в код, поэтому проверяется не сам поиск,
	// а разбор той же разметки через parseDDG - это то, что Search отдаёт
	// на выход.
	res, err := parseDDG([]byte(body), "ddg-html")
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 {
		t.Fatalf("результатов %d, ожидала 2", len(res))
	}
	if res[0].Title != "Каталог" || res[0].Snippet != "файлы и дампы" {
		t.Errorf("поля: %q / %q", res[0].Title, res[0].Snippet)
	}
	if res[1].Title != "Форум" {
		t.Errorf("fallback на h2 a не сработал: %q", res[1].Title)
	}
}

func TestDuckDuckGoSearchCancelledContext(t *testing.T) {
	d := &DuckDuckGo{Client: directClient(t)}
	// Контекст отменён до вызова: запрос обязан упасть сразу, не дойдя до
	// сети. Проверяется, что ошибка возвращается как ошибка, а не как
	// пустой результат - иначе движок выглядел бы «отработавшим впустую».
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.Search(ctx, "q", 5); err == nil {
		t.Error("отменённый контекст не остановил поиск")
	}
}

func TestAhmiaClearName(t *testing.T) {
	if got := (&AhmiaClear{}).Name(); got != "ahmia-clear" {
		t.Errorf("Name=%q", got)
	}
}

func TestSearXNGName(t *testing.T) {
	if got := (&SearXNG{}).Name(); got != "searxng" {
		t.Errorf("Name=%q", got)
	}
}

func TestSearXNGSearchWithoutClient(t *testing.T) {
	s := &SearXNG{BaseURL: "https://sx.example"}
	if _, err := s.Search(context.Background(), "q", 5); err == nil {
		t.Error("поиск без клиента принят")
	}
}

func TestSearXNGSearchWithoutBase(t *testing.T) {
	s := &SearXNG{Client: directClient(t)}
	if _, err := s.Search(context.Background(), "q", 5); err == nil {
		t.Error("поиск без инстанса принят")
	}
}

func TestSearXNGSearchParsesStub(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("format") != "json" {
			t.Errorf("запрос без format=json: %s", r.URL.String())
		}
		fmt.Fprint(w, `{"results":[
{"title":"Hit one","url":"https://s.example/1","content":"First"},
{"title":"Мусор","url":"notaurl","content":"x"}
]}`)
	}))
	defer srv.Close()

	s := &SearXNG{Client: directClient(t), BaseURL: srv.URL + "/"}
	res, err := s.Search(context.Background(), "test", 5)
	if err != nil {
		t.Fatal(err)
	}
	// Вторая запись с битым URL отсечена, слеш в конце базы не двоится.
	if len(res) != 1 || res[0].URL != "https://s.example/1" {
		t.Errorf("результат неверен: %+v", res)
	}
	if res[0].Snippet != "First" {
		t.Errorf("сниппет неверен: %+v", res[0])
	}
}

func TestParseSearXNGEmpty(t *testing.T) {
	if _, err := parseSearXNG([]byte(`{"results":[]}`), "searxng"); err == nil {
		t.Error("пустая выдача принята")
	}
	if _, err := parseSearXNG([]byte(`{битый`), "searxng"); err == nil {
		t.Error("битый JSON принят")
	}
}
func TestAhmiaClearSearchWithoutClient(t *testing.T) {
	a := &AhmiaClear{}
	if _, err := a.Search(context.Background(), "q", 5); err == nil {
		t.Error("поиск без клиента принят")
	}
}

func TestAhmiaClearTimeoutCapsHangingBackend(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			fmt.Fprint(w, `<html><body><form action="/search/" method="get"></form></body></html>`)
			return
		}
		// Backend висит: без капа движок держал бы deep до общего таймаута.
		time.Sleep(500 * time.Millisecond)
		fmt.Fprint(w, `<html><body></body></html>`)
	}))
	defer srv.Close()

	old := ahmiaTimeout
	ahmiaTimeout = 100 * time.Millisecond
	defer func() { ahmiaTimeout = old }()

	a := &AhmiaClear{Client: directClient(t), BaseURL: srv.URL}
	if _, err := a.Search(context.Background(), "test", 5); err == nil {
		t.Error("висящий backend не урезан таймаутом")
	}
}

func TestAhmiaClearSearchParsesStub(t *testing.T) {
	var sawToken bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/":
			fmt.Fprint(w, `<html><body><form action="/search/" method="get">
<input type="hidden" name="tok123" value="val456"></form></body></html>`)
		case strings.HasPrefix(r.URL.Path, "/search/"):
			if r.URL.Query().Get("tok123") == "val456" {
				sawToken = true
			}
			fmt.Fprint(w, `<html><body><ol>
<li class="result"><h4><a href="http://abcdefabcdefabcd.onion/page">Onion hit</a></h4></li>
</ol></body></html>`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	a := &AhmiaClear{Client: directClient(t), BaseURL: srv.URL}
	res, err := a.Search(context.Background(), "test", 5)
	if err != nil {
		t.Fatal(err)
	}
	if !sawToken {
		t.Error("токен с главной не проброшен в поиск")
	}
	if len(res) != 1 || !strings.HasSuffix(hostOf(res[0].URL), ".onion") {
		t.Errorf("onion-результат неверен: %+v", res)
	}
	if !res[0].Onion {
		t.Error("флаг Onion не проставлен на .onion-ссылке")
	}
}

func TestCloakUAsSane(t *testing.T) {
	// Cloak-профиль обязан держать несколько UA: один зашитый на все запросы
	// связывается антиботом в сессии.
	if len(cloakUAs) < 2 {
		t.Fatalf("UA для ротации %d, ожидала >=2", len(cloakUAs))
	}
	for _, ua := range cloakUAs {
		if !strings.Contains(ua, "Mozilla/5.0") {
			t.Errorf("UA не похож на десктопный: %q", ua)
		}
	}
}

func TestBrowserNewAndName(t *testing.T) {
	b := NewBrowser("socks5://127.0.0.1:9050", "", true, nil)
	if b.Name() != "rod-browser" {
		t.Errorf("Name=%q", b.Name())
	}
	if b.timeout <= 0 {
		t.Errorf("timeout=%v", b.timeout)
	}
	if !b.headless {
		t.Error("headless не проброшен")
	}
	if b.proxy != "socks5://127.0.0.1:9050" {
		t.Errorf("proxy=%q", b.proxy)
	}
}

func TestBrowserAvailableWithExplicitPath(t *testing.T) {
	// Явный путь к бинарю используется без поиска в PATH: пользователь сам
	// указал, где лежит chromium.
	//
	// Тест переписан намеренно. Прежняя версия подставляла «/usr/bin/chromium» и
	// ожидала true, то есть кодировала дефект: available() возвращала true для
	// любого непустого пути, не проверяя существование файла. На Windows такого
	// пути нет, и тест проходил только потому, что проверки не было. Из-за этого
	// неверно заданный VOIDSEARCH_CHROME_PATH давал сбой запуска, который
	// считался переходным и повторялся каждые две минуты бесконечно, вместо
	// понятного сообщения об ошибке конфигурации.
	//
	// Смысл «не искать в PATH» сохраняется: существующий файл принимается, даже
	// если chromium в PATH отсутствует.
	existing := filepath.Join(t.TempDir(), "chromium")
	if err := os.WriteFile(existing, []byte("stub"), 0o755); err != nil {
		t.Fatal(err)
	}
	b := NewBrowser("", existing, true, nil)
	if !b.available() {
		t.Error("существующий явный путь не принят")
	}
}

func TestBrowserUnavailableWithMissingExplicitPath(t *testing.T) {
	// Несуществующий явный путь обязан определяться как недоступность, а не
	// приниматься вслепую: это ошибка конфигурации, и она сама не проходит.
	b := NewBrowser("", filepath.Join(t.TempDir(), "нет-такого-chromium"), true, nil)
	if b.available() {
		t.Error("несуществующий путь принят как доступный")
	}
}

func TestBrowserCloseWithoutLaunch(t *testing.T) {
	b := NewBrowser("", "", true, nil)
	if err := b.Close(); err != nil {
		t.Errorf("закрытие не запущенного браузера: %v", err)
	}
	// Повторное закрытие не должно паниковать.
	if err := b.Close(); err != nil {
		t.Errorf("повторное закрытие: %v", err)
	}
}

func TestBrowserLogsWithoutLogger(t *testing.T) {
	b := NewBrowser("", "", true, nil)
	b.logf("сообщение %d", 1)
}

func TestBrowserWarmupWithoutChromium(t *testing.T) {
	// Прогрев с несуществующим бинарём обязан вернуть ошибку, а не висеть:
	// setup по ней решает, предупреждать ли про урезанный stealth.
	b := NewBrowser("", filepath.Join(t.TempDir(), "нет-такого-chromium"), true, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := b.Warmup(ctx); err == nil {
		t.Fatal("прогрев несуществующего chromium прошёл")
	}
}

func TestBrowserWarmupWhenDead(t *testing.T) {
	b := NewBrowser("", "", true, nil)
	b.markPermanentLocked(errors.New("браузер недоступен"))
	if err := b.Warmup(context.Background()); err == nil {
		t.Error("прогрев мёртвого браузера прошёл")
	}
}

func TestBrowserSearchWhenDead(t *testing.T) {
	b := NewBrowser("", "", true, nil)
	b.markPermanentLocked(errors.New("браузер недоступен"))
	_, err := b.Search(context.Background(), "q", 5)
	if err == nil {
		t.Fatal("мёртвый браузер не вернул ошибку")
	}
	if !strings.Contains(err.Error(), "недоступен") {
		t.Errorf("ошибка не объясняет причину: %v", err)
	}
}

func TestBrowserSearchWithoutChromium(t *testing.T) {
	// Бинарь заведомо отсутствует: ensure обязан отметить браузер непригодным и
	// вернуть понятную ошибку, а не паниковать и не висеть.
	b := NewBrowser("", filepath.Join(t.TempDir(), "нет-такого-chromium"), true, nil)
	_, err := b.Search(context.Background(), "q", 5)
	if err == nil {
		t.Fatal("запуск несуществующего chromium прошёл")
	}
	if b.blocked() == nil {
		t.Error("браузер не отмечен непригодным после неудачи")
	}
	// Второй вызов обязан выйти сразу, не пытаясь запустить бинарь снова.
	if _, err2 := b.Search(context.Background(), "q", 5); err2 == nil {
		t.Error("повторный поиск на мёртвом браузере прошёл")
	}
}

func TestBrowserMissingChromiumIsPermanent(t *testing.T) {
	// Отсутствие бинаря - постоянный отказ: он не пройдёт сам, поэтому повторная
	// проверка launcher.LookPath на каждый поиск только жгла бы время.
	b := NewBrowser("", filepath.Join(t.TempDir(), "нет-такого-chromium"), true, nil)
	if _, err := b.Search(context.Background(), "q", 5); err == nil {
		t.Fatal("первый поиск прошёл")
	}

	b.mu.Lock()
	permanent := b.permanent
	deadUntil := b.deadUntil
	b.mu.Unlock()

	if !permanent {
		t.Error("отсутствие бинаря не помечено постоянным отказом")
	}
	if !deadUntil.IsZero() {
		t.Errorf("постоянному отказу выставлен cooldown до %v", deadUntil)
	}
}

func TestBrowserTransientFailureRecoversAfterCooldown(t *testing.T) {
	// Ключевая правка: прежняя версия защёлкивала dead навсегда, поэтому один
	// сбой запуска (Chromium занят, нехватка памяти) выключал stealth-режим до
	// рестарта процесса. Для долгоживущего MCP-сервера это означало потерю
	// браузера из-за одной случайности.
	b := NewBrowser("", "chromium", true, nil)
	b.mu.Lock()
	b.markTransientLocked(errors.New("запуск chromium: ресурс занят"))
	b.mu.Unlock()

	// Пока cooldown не истёк, попытка блокируется и причина видна вызывающему.
	err := b.blocked()
	if err == nil {
		t.Fatal("браузер не заблокирован во время cooldown")
	}
	if !strings.Contains(err.Error(), "временно") {
		t.Errorf("ошибка не отличает переходный отказ от постоянного: %v", err)
	}
	if !strings.Contains(err.Error(), "ресурс занят") {
		t.Errorf("ошибка потеряла исходную причину: %v", err)
	}

	// После истечения cooldown попытка разрешена снова, и состояние сброшено.
	b.mu.Lock()
	b.deadUntil = time.Now().Add(-time.Second)
	b.mu.Unlock()

	if err := b.blocked(); err != nil {
		t.Errorf("после истечения cooldown браузер остался заблокирован: %v", err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.deadUntil.IsZero() {
		t.Errorf("deadUntil не сброшен: %v", b.deadUntil)
	}
	if b.lastErr != nil {
		t.Errorf("lastErr не сброшен: %v", b.lastErr)
	}
}

func TestBrowserTransientFailureBlocksDuringCooldown(t *testing.T) {
	// Обратная сторона: без блокировки каждый поиск дёргал бы запуск Chromium
	// заново и платил секунды на попытку, которая заведомо провалится.
	b := NewBrowser("", "chromium", true, nil)
	b.mu.Lock()
	b.deadUntil = time.Now().Add(browserRetryCooldown)
	b.lastErr = errors.New("запуск chromium: сбой")
	b.mu.Unlock()

	for i := 0; i < 3; i++ {
		if err := b.blocked(); err == nil {
			t.Fatalf("итерация %d: браузер не заблокирован во время cooldown", i)
		}
	}
}

func TestBrowserSuccessfulLaunchClearsStaleError(t *testing.T) {
	// Успешный запуск обязан снимать отметку об отказе: прежняя версия оставляла
	// lastErr заполненным, и последующая проверка могла сообщить устаревшую
	// причину.
	b := NewBrowser("", "chromium", true, nil)
	b.mu.Lock()
	b.deadUntil = time.Now().Add(-time.Second)
	b.lastErr = errors.New("старая причина")
	b.mu.Unlock()

	if err := b.blocked(); err != nil {
		t.Fatalf("браузер не восстановился: %v", err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.lastErr != nil {
		t.Errorf("устаревшая ошибка осталась: %v", b.lastErr)
	}
}

func TestBrowserPermanentErrorReturnedNotGeneric(t *testing.T) {
	// Вызывающий должен получать конкретную причину, а не безличное «браузер
	// недоступен»: по общему сообщению невозможно отличить отсутствие бинаря от
	// сбоя запуска.
	b := NewBrowser("", "chromium", true, nil)
	b.mu.Lock()
	b.markPermanentLocked(errors.New("chromium не найден на этой машине"))
	b.mu.Unlock()

	err := b.blocked()
	if err == nil {
		t.Fatal("постоянный отказ не вернул ошибку")
	}
	if !strings.Contains(err.Error(), "не найден") {
		t.Errorf("ошибка потеряла причину: %v", err)
	}
}

func TestBrowserConcurrentBlockedIsRaceFree(t *testing.T) {
	// Прежняя версия писала dead под мьютексом, а читала в Search и Warmup без
	// него - это гонка данных, которую ловит -race в CI. Все обращения к
	// состоянию обязаны идти через blocked().
	b := NewBrowser("", "chromium", true, nil)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%4 == 0 {
				b.mu.Lock()
				b.markTransientLocked(errors.New("сбой"))
				b.mu.Unlock()
			}
			_ = b.blocked()
		}(i)
	}
	wg.Wait()
}

func TestSearcherInterfaceSatisfied(t *testing.T) {
	// Одиночные движки обязаны удовлетворять интерфейсу Searcher: их
	// подставляют в Chain. Каталог и цепочка - агрегаторы, у них свои
	// методы (SearchAll / SearchDetailed), в цепочку они не идут.
	var _ Searcher = &DuckDuckGo{}
	var _ Searcher = &SearXNG{}
	var _ Searcher = &AhmiaClear{}
	var _ Searcher = &OnionEngine{}
	var _ Searcher = &Browser{}
}
