package search

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"voidsearchswag/internal/httpc"
	"voidsearchswag/internal/router"
	"voidsearchswag/internal/searchers"
)

// Этап 164, жалоба смоук-агентов на v0.1.0: deep-выдача несла результаты
// без единого токена запроса. Поиск обязан выкинуть их с пометкой в Note.
func TestSearchDropsResultsWithoutQueryTokens(t *testing.T) {
	e, _ := newEngine(t, &fixedSearcher{name: "s", res: []searchers.Result{
		{URL: "http://acme.example/files", Title: "acme corp files", Snippet: "leak database"},
		{URL: "http://shop.example/vitrina", Title: "онлайн аптека", Snippet: "скидки"},
	}})
	out, err := e.Search(context.Background(), Options{Query: "acme corp", Mode: router.ModeFast})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(out.Results) != 1 {
		t.Fatalf("результатов %d, хочу 1 (аптека без токенов выкидывается)", len(out.Results))
	}
	if out.Results[0].Title != "acme corp files" {
		t.Errorf("оставлен %q, хочу совпадение", out.Results[0].Title)
	}
	if out.Count != 1 {
		t.Errorf("count=%d, хочу 1", out.Count)
	}
	if !strings.Contains(out.Report.Note, "выкинуто 1 результатов без единого токена запроса") {
		t.Errorf("note не объясняет отсев: %q", out.Report.Note)
	}
}

// Тот же мусор, но без единого совпадения в выдаче: фильтр снимается,
// потому что пустой ответ хуже шумного, а движки отвечают синонимами.
func TestSearchKeepsGarbageWhenNothingMatches(t *testing.T) {
	e, _ := newEngine(t, &fixedSearcher{name: "s", res: []searchers.Result{
		{URL: "http://shop.example/1", Title: "витрина", Snippet: ""},
		{URL: "http://shop.example/2", Title: "витрина 2", Snippet: ""},
	}})
	out, err := e.Search(context.Background(), Options{Query: "acme corp", Mode: router.ModeFast})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(out.Results) != 2 {
		t.Fatalf("результатов %d, хочу 2 (фильтр снят, пусто не оставляем)", len(out.Results))
	}
	if !strings.Contains(out.Report.Note, "фильтр шума снят") {
		t.Errorf("note не объясняет снятие фильтра: %q", out.Report.Note)
	}
}

// Кэш-хит не фильтрует заново: запись уже отфильтрована при живом прогоне,
// а то же сравнение тех же строк дало бы тот же итог и съело бы только
// время. Проверяется повторным запросом без --no-cache.
func TestSearchNoiseFilterSurvivesCacheRoundtrip(t *testing.T) {
	e, _ := newEngine(t, &fixedSearcher{name: "s", res: []searchers.Result{
		{URL: "http://acme.example/files", Title: "acme files", Snippet: ""},
		{URL: "http://shop.example/1", Title: "аптека", Snippet: ""},
	}})
	first, err := e.Search(context.Background(), Options{Query: "acme", Mode: router.ModeFast})
	if err != nil {
		t.Fatalf("первый поиск: %v", err)
	}
	if len(first.Results) != 1 {
		t.Fatalf("первый прогон: результатов %d, хочу 1", len(first.Results))
	}
	second, err := e.Search(context.Background(), Options{Query: "acme", Mode: router.ModeFast})
	if err != nil {
		t.Fatalf("второй поиск: %v", err)
	}
	if !second.Cached {
		t.Fatalf("второй прогон не из кэша")
	}
	if len(second.Results) != 1 {
		t.Fatalf("кэш вернул %d результатов, хочу 1 (запись уже отфильтрована)", len(second.Results))
	}
	// Note фильтра из записи - правда о выдаче, а не повтор работы: фильтр
	// не гоняется на кэш-хите заново. Проверяется косвенно: кэш-хит при
	// том же движке-заглушке не изменил ни число результатов, ни саму
	// выдачу - сравниваются URL, потому что повторный фильтр (или его
	// отсутствие) не должны переставлять выдачу.
	if second.Results[0].URL != first.Results[0].URL {
		t.Errorf("кэш-хит вернул другую выдачу: %q против %q", second.Results[0].URL, first.Results[0].URL)
	}
}

// validateAliveServer поднимает стенд, где /ok отвечает 200, /gone - 404,
// а /dead закрывает соединение без ответа. Это три состояния, которые
// валидация обязана различать.
func validateServer(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("live"))
		case "/gone":
			http.Error(w, "not found", http.StatusNotFound)
		case "/gone410":
			http.Error(w, "gone", http.StatusGone)
		default:
			panic("неизвестный путь стенда: " + r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func newValidateEngine(t *testing.T) (*Engine, string) {
	t.Helper()
	e, _ := newEngine(t)
	// AllowPrivateTarget: стенд живёт на 127.0.0.1, а барьер служебных
	// сетей в urlAlive честно режет петлевые адреса. Прод оставляет флаг
	// выключенным, тесты поднимают его ради стенда.
	e.AllowPrivateTarget = true
	c, err := httpc.NewClient(context.Background(), httpc.Options{
		Transport: "direct",
		Timeout:   5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	e.Direct = c
	return e, validateServer(t)
}

func TestSearchValidateDropsDeadAndGone(t *testing.T) {
	e, srv := newValidateEngine(t)
	base := "http://127.0.0.1:1"
	e.DDG = []searchers.Searcher{&fixedSearcher{name: "s", res: []searchers.Result{
		{URL: srv + "/ok", Title: "live acme page", Snippet: "acme"},
		{URL: srv + "/gone", Title: "acme gone page", Snippet: "acme"},
		{URL: base + "/nope", Title: "acme dead page", Snippet: "acme"},
		{URL: srv + "/gone410", Title: "acme 410 page", Snippet: "acme"},
	}}}
	out, err := e.Search(context.Background(), Options{Query: "acme", Mode: router.ModeFast, Validate: true})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(out.Results) != 1 {
		t.Fatalf("результатов %d, хочу 1 (404, 410 и недоступный адрес выкинуты)", len(out.Results))
	}
	if out.Results[0].URL != srv+"/ok" {
		t.Errorf("оставлен %q, хочу живую страницу", out.Results[0].URL)
	}
	if !strings.Contains(out.Report.Note, "выкинуто 3 мёртвых") {
		t.Errorf("note не называет число мёртвых: %q", out.Report.Note)
	}
}

// validate не вправе оставить пустоту: если не подтвердился ни один адрес,
// фильтр снимается - «все мёртвые» чаще транспорт лежит, а не ссылки битые.
func TestSearchValidateDoesNotEmptyResultset(t *testing.T) {
	e, _ := newValidateEngine(t)
	e.DDG = []searchers.Searcher{&fixedSearcher{name: "s", res: []searchers.Result{
		{URL: "http://127.0.0.1:1/a", Title: "acme a", Snippet: "acme"},
		{URL: "http://127.0.0.1:1/b", Title: "acme b", Snippet: "acme"},
	}}}
	out, err := e.Search(context.Background(), Options{Query: "acme", Mode: router.ModeFast, Validate: true})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(out.Results) != 2 {
		t.Fatalf("результатов %d, хочу 2 (validate не выкашивает всё под ноль)", len(out.Results))
	}
	if !strings.Contains(out.Report.Note, "фильтр снят") {
		t.Errorf("note не объясняет снятие: %q", out.Report.Note)
	}
}

// Без транспорта validate честно признаётся в Note, а не молчит.
func TestSearchValidateSkipsWithoutTransport(t *testing.T) {
	e, _ := newEngine(t, &fixedSearcher{name: "s", res: []searchers.Result{
		{URL: "http://acme.example/1", Title: "acme", Snippet: "acme"},
	}})
	out, err := e.Search(context.Background(), Options{Query: "acme", Mode: router.ModeFast, Validate: true})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(out.Results) != 1 {
		t.Fatalf("результатов %d, хочу 1 (без транспорта выдача не трогается)", len(out.Results))
	}
	if !strings.Contains(out.Report.Note, "validate пропущен") {
		t.Errorf("note не признаётся в пропуске: %q", out.Report.Note)
	}
}

// Кэш-хит с validate проверяет адреса заново: живость - свойство сети в
// момент вызова, а не момент записи кэша. Живая страница между прогонами
// умирает - кэш обязан это увидеть и снять фильтр по страховке.
func TestSearchValidateRerunsOnCacheHit(t *testing.T) {
	e, _ := newEngine(t, &fixedSearcher{name: "s", res: nil})
	e.AllowPrivateTarget = true
	e.Direct = directClientFor(t)
	e.CacheTTL = time.Hour
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("live"))
	}))
	t.Cleanup(srv.Close)
	e.DDG = []searchers.Searcher{&fixedSearcher{name: "s", res: []searchers.Result{
		{URL: srv.URL + "/ok", Title: "acme live", Snippet: "acme"},
	}}}
	first, err := e.Search(context.Background(), Options{Query: "acme", Mode: router.ModeFast, Validate: true})
	if err != nil {
		t.Fatalf("первый поиск: %v", err)
	}
	if len(first.Results) != 1 {
		t.Fatalf("первый прогон: результатов %d, хочу 1", len(first.Results))
	}
	// Стенд закрывается: страница была живой при записи кэша и стала
	// мёртвой к моменту повторного чтения.
	srv.Close()
	second, err := e.Search(context.Background(), Options{Query: "acme", Mode: router.ModeFast, Validate: true})
	if err != nil {
		t.Fatalf("второй поиск: %v", err)
	}
	if !second.Cached {
		t.Fatalf("второй прогон не из кэша")
	}
	if !strings.Contains(second.Report.Note, "validate не подтвердил") {
		t.Errorf("кэш-хит не прогнал validate или не снял фильтр: %q", second.Report.Note)
	}
	if len(second.Results) != 1 {
		t.Fatalf("кэш-хит: результатов %d, хочу 1 по страховке", len(second.Results))
	}
}

// Бюджет validate: восемь адресов проверяются, девятый и дальше не
// тратят сеть. Замер жалобы смоука шёл по тройке, восьми за глаза.
func TestSearchValidateChecksAtMostEight(t *testing.T) {
	e, _ := newEngine(t)
	e.AllowPrivateTarget = true
	e.Direct = directClientFor(t)
	e.CacheTTL = time.Hour
	var checked int
	mux := http.NewServeMux()
	mux.HandleFunc("/p", func(w http.ResponseWriter, r *http.Request) { checked++ })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	res := make([]searchers.Result, 0, 20)
	for i := 0; i < 20; i++ {
		res = append(res, searchers.Result{
			URL: srv.URL + "/p?i=" + string(rune('a'+i)), Title: "acme " + string(rune('A'+i)), Snippet: "acme " + string(rune('a'+i)),
		})
	}
	e.DDG = []searchers.Searcher{&fixedSearcher{name: "s", res: res}}
	out, err := e.Search(context.Background(), Options{Query: "acme", Mode: router.ModeFast, Validate: true, NoCache: true})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(out.Results) != 20 {
		t.Fatalf("результатов %d, хочу 20 (валидация не режет объём)", len(out.Results))
	}
	if checked != validateBatch {
		t.Fatalf("проверено %d адресов, хочу потолок %d", checked, validateBatch)
	}
}

// directClientFor - клиент clearnet без барьера приватных целей: стенды
// живут на петле, а валидация в тесте должна до них дотянуться.
func directClientFor(t *testing.T) *httpc.Client {
	t.Helper()
	c, err := httpc.NewClient(context.Background(), httpc.Options{
		Transport: "direct",
		Timeout:   5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}
