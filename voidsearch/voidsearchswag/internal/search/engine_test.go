package search

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"voidsearchswag/internal/httpc"
	"voidsearchswag/internal/router"
	"voidsearchswag/internal/searchers"
	"voidsearchswag/internal/store"
)

func newEngine(t *testing.T, ss ...searchers.Searcher) (*Engine, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/s.db")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return &Engine{
		Store:    st,
		DDG:      ss,
		CacheTTL: time.Hour,
		DefaultN: 20,
	}, st
}

type fixedSearcher struct {
	name string
	res  []searchers.Result
	err  error
}

func (f *fixedSearcher) Name() string { return f.name }
func (f *fixedSearcher) Search(context.Context, string, int) ([]searchers.Result, error) {
	return f.res, f.err
}

func testDirectClient(t *testing.T) *httpc.Client {
	t.Helper()
	c, err := httpc.NewClient(context.Background(), httpc.Options{
		Transport: "direct",
		Timeout:   10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func newAhmiaStub(t *testing.T, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestSearchRejectsEmptyQuery(t *testing.T) {
	e, _ := newEngine(t)
	if _, err := e.Search(context.Background(), Options{Query: "   "}); err == nil {
		t.Fatal("пустой запрос должен давать ошибку")
	}
}

func TestSearchExplicitMode(t *testing.T) {
	e, _ := newEngine(t, &fixedSearcher{name: "s", res: []searchers.Result{
		{URL: "https://a.example/1", Title: "A"},
	}})
	out, err := e.Search(context.Background(), Options{Query: "anything", Mode: router.ModeFast})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if out.Mode != router.ModeFast {
		t.Errorf("mode=%s, ожидала fast", out.Mode)
	}
	if out.Count != 1 {
		t.Errorf("count=%d, ожидала 1", out.Count)
	}
	if out.Results[0].Rank != 1 {
		t.Errorf("rank=%d, ожидала 1", out.Results[0].Rank)
	}
}

func TestSearchAutoRoutesToDeep(t *testing.T) {
	e, _ := newEngine(t, &fixedSearcher{name: "onion", res: []searchers.Result{
		{URL: "http://abc.onion/x", Title: "Deep"},
	}})
	out, err := e.Search(context.Background(), Options{Query: "leak dump database"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if out.Decision.Mode != router.ModeDeep {
		t.Errorf("decision mode=%s, ожидала deep", out.Decision.Mode)
	}
	if out.Mode != router.ModeDeep {
		t.Errorf("итоговый mode=%s, ожидала deep", out.Mode)
	}
}

func TestSearchFallbackOnEmptyResults(t *testing.T) {
	// Этап 162: цепочка запасных режимов - поведение авто-режима, где
	// решение принимал роутер и сам может передумать. Явный mode с этапа 162
	// не подменяется никогда, поэтому сценарий фоллбэка живёт здесь с
	// пустым Mode: роутер для бытового запроса без onion-сигналов выбирает
	// fast, его пустая выдача передаёт ход запасному stealth.
	flaky := &flakySearcher{name: "flaky", emptyFirst: 1, res: []searchers.Result{
		{URL: "https://fallback.example/1", Title: "Fallback"},
	}}
	e, _ := newEngine(t, flaky)
	out, err := e.Search(context.Background(), Options{Query: "купить кофе", Mode: ""})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if out.Count != 1 {
		t.Fatalf("count=%d, ожидала 1 после фоллбэка", out.Count)
	}
	if !out.Degraded {
		t.Error("после фоллбэка degraded должен быть true")
	}
	if out.Mode != router.ModeFast {
		t.Errorf("решение роутера mode=%s, ожидала fast", out.Mode)
	}
	if out.UsedMode != router.ModeStealth {
		t.Errorf("used_mode=%s, ожидала stealth (первый фоллбэк для fast)", out.UsedMode)
	}
	if !strings.Contains(out.Decision.Reason, "фоллбэк") {
		t.Errorf("причина не отражает фоллбэк: %q", out.Decision.Reason)
	}
}

type flakySearcher struct {
	name       string
	emptyFirst int
	calls      int
	res        []searchers.Result
}

func (f *flakySearcher) Name() string { return f.name }

func (f *flakySearcher) Search(context.Context, string, int) ([]searchers.Result, error) {
	f.calls++
	if f.calls <= f.emptyFirst {
		return nil, nil
	}
	return f.res, nil
}

func TestDeepInternalDegradationReported(t *testing.T) {
	e, _ := newEngine(t, &fixedSearcher{name: "clear", res: []searchers.Result{
		{URL: "https://clear.example/1"},
	}})
	out, err := e.Search(context.Background(), Options{Query: "onion leak dump", Mode: router.ModeDeep})
	if err != nil {
		t.Fatal(err)
	}
	if out.Count == 0 {
		t.Fatal("ожидались результаты из clearnet")
	}
	if !out.Degraded {
		t.Error("deep без onion-движков должен помечать degraded")
	}
	if out.Mode != router.ModeDeep {
		t.Errorf("запрошенный mode=%s должен остаться deep", out.Mode)
	}
	if !strings.Contains(out.Report.Note, "onion") {
		t.Errorf("note не объясняет деградацию: %q", out.Report.Note)
	}
}

func TestSearchCachesOutcome(t *testing.T) {
	e, st := newEngine(t, &fixedSearcher{name: "s", res: []searchers.Result{
		{URL: "https://cache.example/1", Title: "C"},
	}})
	ctx := context.Background()
	first, err := e.Search(ctx, Options{Query: "cache me", Mode: router.ModeFast, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if first.Cached {
		t.Error("первый вызов не должен быть кэшированным")
	}

	key := store.CacheKey("cache me", string(router.ModeFast))
	if _, ok, err := st.CacheGet(ctx, key); err != nil || !ok {
		t.Fatalf("запись не легла в кэш: ok=%v err=%v", ok, err)
	}

	second, err := e.Search(ctx, Options{Query: "cache me", Mode: router.ModeFast})
	if err != nil {
		t.Fatal(err)
	}
	if !second.Cached {
		t.Error("второй вызов должен прийти из кэша")
	}
	if second.Count != first.Count {
		t.Errorf("count из кэша=%d, ожидала %d", second.Count, first.Count)
	}
}

// Этап 179: контракт no_cache перевёрнут. Прежний тест требовал «не
// писать в кэш», но смоук-агент пойрал живой конфликт: свежий прогон с
// no_cache не обновлял устаревшую запись, и следующий запрос без флага
// получал из кэша выдачу, которую свежий прогон уже опроверг. Флаг
// отключает чтение, запись обновляется всегда.
func TestNoCacheBypassesReadButWrites(t *testing.T) {
	e, st := newEngine(t, &fixedSearcher{name: "s", res: []searchers.Result{
		{URL: "https://nocache.example/1"},
	}})
	ctx := context.Background()
	out, err := e.Search(ctx, Options{Query: "no write", Mode: router.ModeFast, NoCache: true, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if out.Cached {
		t.Error("no_cache не должен читать кэш")
	}
	key := store.CacheKey("no write", string(router.ModeFast))
	if _, ok, _ := st.CacheGet(ctx, key); !ok {
		t.Error("при no_cache свежий прогон обязан обновлять запись кэша")
	}
}

func TestSearchEmptyResultsNotCached(t *testing.T) {
	e, st := newEngine(t, &fixedSearcher{name: "s", err: errors.New("boom")})
	ctx := context.Background()
	out, err := e.Search(ctx, Options{Query: "nothing", Mode: router.ModeFast, Limit: 20})
	if err != nil {
		t.Fatalf("пустой результат не должен быть ошибкой: %v", err)
	}
	if out.Count != 0 {
		t.Errorf("count=%d, ожидала 0", out.Count)
	}
	key := store.CacheKey("nothing", string(router.ModeFast))
	if _, ok, _ := st.CacheGet(ctx, key); ok {
		t.Error("пустой результат не должен кэшироваться")
	}
}

func TestCacheRespectsSmallerLimit(t *testing.T) {
	many := &fixedSearcher{name: "many", res: []searchers.Result{
		{URL: "https://a.example/1"}, {URL: "https://b.example/2"},
		{URL: "https://c.example/3"}, {URL: "https://d.example/4"},
		{URL: "https://e.example/5"},
	}}
	e, _ := newEngine(t, many)
	ctx := context.Background()

	first, err := e.Search(ctx, Options{Query: "limit cache", Mode: router.ModeFast, Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if first.Count != 5 {
		t.Fatalf("первый count=%d, ожидала 5", first.Count)
	}

	second, err := e.Search(ctx, Options{Query: "limit cache", Mode: router.ModeFast, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !second.Cached {
		t.Fatal("второй вызов должен быть из кэша")
	}
	if second.Count != 2 {
		t.Errorf("из кэша count=%d, ожидала 2 (ограничение лимитом)", second.Count)
	}
	if len(second.Results) != 2 {
		t.Errorf("результатов из кэша %d, ожидала 2", len(second.Results))
	}
}

func TestCacheKeyDistinguishesModes(t *testing.T) {
	e, st := newEngine(t, &fixedSearcher{name: "s", res: []searchers.Result{
		{URL: "https://mode.example/1"},
	}})
	ctx := context.Background()
	// Limit задан явно, чтобы ключ кэша в проверке совпадал с тем, который
	// построит движок. Без этого тест зависел бы от DefaultN.
	if _, err := e.Search(ctx, Options{Query: "same query", Mode: router.ModeFast, Limit: 20}); err != nil {
		t.Fatal(err)
	}
	fastKey := store.CacheKey("same query", string(router.ModeFast))
	stealthKey := store.CacheKey("same query", string(router.ModeStealth))
	if fastKey == stealthKey {
		t.Fatal("ключи разных режимов совпали")
	}
	if _, ok, _ := st.CacheGet(ctx, stealthKey); ok {
		t.Error("fast-запрос не должен заполнять stealth-кэш")
	}
	if _, ok, _ := st.CacheGet(ctx, fastKey); !ok {
		t.Error("fast-запись должна быть в кэше")
	}
}

func TestCacheKeyNormalizesQuery(t *testing.T) {
	a := store.CacheKey("  Hello World  ", "fast")
	b := store.CacheKey("hello world", "fast")
	if a != b {
		t.Errorf("регистр/пробелы должны нормализоваться: %s vs %s", a, b)
	}
}

func TestCacheDoesNotServeSmallerResultForLargerLimit(t *testing.T) {
	// Регресс на найденный вживую дефект: запрос с limit=5 заполнял кэш,
	// следующий запрос с limit=20 получал из него те же пять результатов, и
	// одна и та же команда через пару секунд давала разный ответ. Кэшированный
	// ответ оказывался хуже свежего.
	e, st := newEngine(t, &fixedSearcher{name: "s", res: []searchers.Result{
		{URL: "https://lim.example/1"}, {URL: "https://lim.example/2"},
		{URL: "https://lim.example/3"}, {URL: "https://lim.example/4"},
		{URL: "https://lim.example/5"}, {URL: "https://lim.example/6"},
	}})
	ctx := context.Background()

	small, err := e.Search(ctx, Options{Query: "limit probe", Mode: router.ModeFast, Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(small.Results) != 5 {
		t.Fatalf("первый запрос вернул %d, ожидала 5", len(small.Results))
	}

	big, err := e.Search(ctx, Options{Query: "limit probe", Mode: router.ModeFast, Limit: 6})
	if err != nil {
		t.Fatal(err)
	}
	if big.Cached {
		t.Error("запрос с большим limit получил урезанную запись из кэша")
	}
	if len(big.Results) != 6 {
		t.Errorf("вернулось %d результатов, ожидала 6", len(big.Results))
	}
	// Запись в кэше одна на запрос и режим, поэтому второй прогон её
	// перезаписывает более полной выдачей. Проверка важна: если бы запись
	// осталась урезанной, следующий запрос с большим limit снова ушёл бы в
	// сеть, и кэш никогда не «дозрел» бы.
	payload, ok, err := st.CacheGet(ctx, store.CacheKey("limit probe", string(router.ModeFast)))
	if err != nil || !ok {
		t.Fatalf("запись пропала из кэша: ok=%v err=%v", ok, err)
	}
	var stored Outcome
	if json.Unmarshal([]byte(payload), &stored) != nil {
		t.Fatal("в кэше невалидный JSON")
	}
	if stored.Limit != 6 {
		t.Errorf("в кэше остался limit=%d, ожидала 6", stored.Limit)
	}
	if len(stored.Results) != 6 {
		t.Errorf("в кэше %d результатов, ожидала 6", len(stored.Results))
	}

	// И теперь запрос на 6 из кэша обязан обслужиться, а не идти в сеть.
	again, err := e.Search(ctx, Options{Query: "limit probe", Mode: router.ModeFast, Limit: 6})
	if err != nil {
		t.Fatal(err)
	}
	if !again.Cached {
		t.Error("повторный запрос с тем же limit не попал в кэш")
	}
	if again.Count != 6 {
		t.Errorf("из кэша count=%d, ожидала 6", again.Count)
	}
}

func TestCachedAutoRequestReportsOwnDecision(t *testing.T) {
	// Регресс на второй найденный дефект: авто-запрос, попавший в запись,
	// оставленную явным `-mode fast`, рапортовал «режим задан явно», хотя
	// пользователь режим не выбирал. Кэш разделяется по режиму, но не по
	// способу его выбора, поэтому решение нужно подставлять текущее.
	e, _ := newEngine(t, &fixedSearcher{name: "s", res: []searchers.Result{
		{URL: "https://dec.example/1"},
	}})
	ctx := context.Background()

	explicit, err := e.Search(ctx, Options{Query: "новости про кванты", Mode: router.ModeFast, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if explicit.Decision.Reason != "режим задан явно" {
		t.Fatalf("явный запрос отчитался иначе: %q", explicit.Decision.Reason)
	}

	auto, err := e.Search(ctx, Options{Query: "новости про кванты", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if !auto.Cached {
		t.Fatal("авто-запрос не попал в кэш, тест ничего не проверяет")
	}
	if auto.Decision.Reason == "режим задан явно" {
		t.Error("авто-запрос отрапортовал чужое решение «режим задан явно»")
	}
	if auto.Decision.Reason == "" {
		t.Error("у авто-запроса пустая причина маршрутизации")
	}
	if auto.Decision.Mode != router.ModeFast {
		t.Errorf("авто-запрос смаршрутизирован в %s, ожидала fast", auto.Decision.Mode)
	}
}

func TestClearnetQueriesAllEnginesInParallel(t *testing.T) {
	big := &fixedSearcher{name: "big", res: []searchers.Result{
		{URL: "https://big.example/1"}, {URL: "https://big.example/2"},
		{URL: "https://big.example/3"}, {URL: "https://big.example/4"},
		{URL: "https://big.example/5"}, {URL: "https://big.example/6"},
	}}
	small := &fixedSearcher{name: "small", res: []searchers.Result{
		{URL: "https://small.example/1"},
	}}
	e, _ := newEngine(t, big, small)

	out, err := e.Search(context.Background(), Options{Query: "linux", Mode: router.ModeFast, Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Report.Engines) != 2 {
		names := make([]string, 0, len(out.Report.Engines))
		for _, r := range out.Report.Engines {
			names = append(names, r.Name)
		}
		t.Fatalf("движков в отчёте %d (%v), ожидала 2 - первый движок съел лимит", len(out.Report.Engines), names)
	}
	var sawSmall bool
	for _, r := range out.Report.Engines {
		if r.Name == "small" {
			sawSmall = true
		}
	}
	if !sawSmall {
		t.Error("второй движок не опрошен - цикл всё ещё последовательный")
	}
	if out.Count != 3 {
		t.Errorf("count=%d, ожидала 3 (лимит)", out.Count)
	}
}

func TestReportOrderStableAcrossRuns(t *testing.T) {
	a := &blockingSearcher{name: "a", delay: 25 * time.Millisecond, res: []searchers.Result{{URL: "https://a.example/1"}}}
	b := &blockingSearcher{name: "b", delay: 0, res: []searchers.Result{{URL: "https://b.example/1"}}}
	c := &blockingSearcher{name: "c", delay: 10 * time.Millisecond, res: []searchers.Result{{URL: "https://c.example/1"}}}

	for run := 0; run < 5; run++ {
		e, _ := newEngine(t, a, b, c)
		out, err := e.Search(context.Background(), Options{Query: "stable", Mode: router.ModeFast})
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"a", "b", "c"}
		if len(out.Report.Engines) != 3 {
			t.Fatalf("прогон %d: движков %d, ожидала 3", run, len(out.Report.Engines))
		}
		for i, r := range out.Report.Engines {
			if r.Name != want[i] {
				t.Errorf("прогон %d: позиция %d = %s, ожидала %s (гонка в порядке отчёта)",
					run, i, r.Name, want[i])
			}
		}
	}
}

type blockingSearcher struct {
	name  string
	delay time.Duration
	res   []searchers.Result
}

func (b *blockingSearcher) Name() string { return b.name }

func (b *blockingSearcher) Search(ctx context.Context, _ string, _ int) ([]searchers.Result, error) {
	if b.delay > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(b.delay):
		}
	}
	return b.res, nil
}

func TestGracePeriodSkipsHungEngine(t *testing.T) {
	fast := &blockingSearcher{name: "fast", delay: 0, res: []searchers.Result{
		{URL: "https://f1.example/1"}, {URL: "https://f2.example/2"},
		{URL: "https://f3.example/3"}, {URL: "https://f4.example/4"},
	}}
	hung := &blockingSearcher{name: "hung", delay: 60 * time.Second}
	e, _ := newEngine(t, fast)
	e.Grace = 50 * time.Millisecond

	start := time.Now()
	all, reports := e.querySearchersParallel(context.Background(),
		[]searchers.Searcher{fast, hung}, "q", 2)
	elapsed := time.Since(start)

	if len(all) == 0 {
		t.Fatal("нет результатов от быстрого движка")
	}
	if elapsed > 5*time.Second {
		t.Errorf("дозор не сработал, ждали %v вместо окна дозора", elapsed)
	}
	// Зависший движок не ждём, но из отчёта он больше не исчезает: оператор
	// обязан видеть, кто именно не ответил и почему. Прежняя версия требовала
	// здесь один движок, и прогон search против молчащего прокси возвращал
	// engines:null.
	if len(reports) != 2 {
		t.Fatalf("в отчёте %d движков, ожидала 2: ответивший и не успевший", len(reports))
	}
	if reports[0].Name != "fast" || !reports[0].OK {
		t.Errorf("первая запись отчёта: %+v", reports[0])
	}
	if reports[1].Name != "hung" {
		t.Errorf("вторая запись отчёта: %+v, хочу hung", reports[1])
	}
	if reports[1].Error != "окно дозора истекло, движок не успел ответить" {
		t.Errorf("причина у зависшего движка: %q", reports[1].Error)
	}
	if reports[1].OK || reports[1].Count != 0 {
		t.Errorf("зависший движок отмечен ответившим: %+v", reports[1])
	}
	if reports[1].Elapsed == "" {
		t.Error("у зависшего движка нет elapsed")
	}
}

func TestSlowEngineDoesNotBlockSearch(t *testing.T) {
	fast := &blockingSearcher{name: "fast", delay: 0, res: []searchers.Result{
		{URL: "https://f1.example/1"}, {URL: "https://f2.example/2"},
		{URL: "https://f3.example/3"}, {URL: "https://f4.example/4"},
		{URL: "https://f5.example/5"}, {URL: "https://f6.example/6"},
	}}
	hung := &blockingSearcher{name: "hung", delay: 60 * time.Second}
	e, _ := newEngine(t, fast, hung)
	e.Grace = 50 * time.Millisecond

	start := time.Now()
	out, err := e.Search(context.Background(), Options{Query: "linux", Mode: router.ModeFast, Limit: 2})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatal(err)
	}
	if out.Count == 0 {
		t.Fatal("быстрый движок не отдал результаты")
	}
	if elapsed > 5*time.Second {
		t.Errorf("поиск ждал зависший движок %v", elapsed)
	}
}

func TestDeepReportIsStableOrder(t *testing.T) {
	a := &fixedSearcher{name: "a", res: []searchers.Result{{URL: "https://a.example/1"}}}
	b := &fixedSearcher{name: "b", res: []searchers.Result{{URL: "https://b.example/1"}}}
	c := &fixedSearcher{name: "c", res: []searchers.Result{{URL: "https://c.example/1"}}}
	e, _ := newEngine(t, a, b, c)
	out, err := e.Search(context.Background(), Options{Query: "onion dump", Mode: router.ModeDeep})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a", "b", "c"}
	for i, r := range out.Report.Engines {
		if i < len(want) && r.Name != want[i] {
			t.Errorf("порядок отчёта нестабилен: позиция %d = %s, ожидала %s", i, r.Name, want[i])
		}
	}
}

func TestDeepPartialFailureKeepsGoodResults(t *testing.T) {
	good := &fixedSearcher{name: "good", res: []searchers.Result{{URL: "https://good.example/1"}}}
	bad := &fixedSearcher{name: "bad", err: errors.New("socks failure")}
	e, _ := newEngine(t, good, bad)
	out, err := e.Search(context.Background(), Options{Query: "onion leak", Mode: router.ModeDeep})
	if err != nil {
		t.Fatal(err)
	}
	if out.Count != 1 {
		t.Errorf("count=%d, ожидала 1 (упавший движок не должен съесть рабочий)", out.Count)
	}
	var badRep *EngineReport
	for i := range out.Report.Engines {
		if out.Report.Engines[i].Name == "bad" {
			badRep = &out.Report.Engines[i]
		}
	}
	if badRep == nil {
		t.Fatal("упавший движок потерян из отчёта")
	}
	if badRep.OK {
		t.Error("упавший движок помечен успешным")
	}
	if !strings.Contains(badRep.Error, "socks") {
		t.Errorf("ошибка не передана: %q", badRep.Error)
	}
}

func TestSearchReportPerEngine(t *testing.T) {
	ok1 := &fixedSearcher{name: "ok1", res: []searchers.Result{{URL: "https://a.example/1"}}}
	bad := &fixedSearcher{name: "bad", err: errors.New("network down")}
	e, _ := newEngine(t, ok1, bad)
	out, err := e.Search(context.Background(), Options{Query: "report", Mode: router.ModeFast})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Report.Engines) != 2 {
		t.Fatalf("движков в отчёте %d, ожидала 2", len(out.Report.Engines))
	}
	byName := map[string]EngineReport{}
	for _, r := range out.Report.Engines {
		byName[r.Name] = r
	}
	if !byName["ok1"].OK || byName["ok1"].Count != 1 {
		t.Errorf("ok1 в отчёте неверен: %+v", byName["ok1"])
	}
	if byName["bad"].OK {
		t.Error("упавший движок помечен как успешный")
	}
	if !strings.Contains(byName["bad"].Error, "network down") {
		t.Errorf("ошибка движка не передана: %q", byName["bad"].Error)
	}
	if out.Report.Live != 1 {
		t.Errorf("live=%d, ожидала 1", out.Report.Live)
	}
}

func TestSearchLimitRespected(t *testing.T) {
	many := &fixedSearcher{name: "many", res: []searchers.Result{
		{URL: "https://a.example/1"}, {URL: "https://b.example/2"},
		{URL: "https://c.example/3"}, {URL: "https://d.example/4"},
		{URL: "https://e.example/5"},
	}}
	e, _ := newEngine(t, many)
	out, err := e.Search(context.Background(), Options{Query: "limit", Mode: router.ModeFast, Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if out.Count != 3 {
		t.Errorf("count=%d, ожидала 3 (лимит)", out.Count)
	}
}

func TestSearchDurationSet(t *testing.T) {
	e, _ := newEngine(t, &fixedSearcher{name: "s", res: []searchers.Result{{URL: "https://d.example/1"}}})
	out, err := e.Search(context.Background(), Options{Query: "dur", Mode: router.ModeFast})
	if err != nil {
		t.Fatal(err)
	}
	if out.Duration == "" {
		t.Error("duration не заполнен")
	}
}

func TestSearchWithoutStoreStillWorks(t *testing.T) {
	e := &Engine{
		DDG:      []searchers.Searcher{&fixedSearcher{name: "s", res: []searchers.Result{{URL: "https://nostore.example/1"}}}},
		DefaultN: 10,
	}
	out, err := e.Search(context.Background(), Options{Query: "no store", Mode: router.ModeFast})
	if err != nil {
		t.Fatalf("без store поиск должен работать: %v", err)
	}
	if out.Count != 1 {
		t.Errorf("count=%d, ожидала 1", out.Count)
	}
}

func TestParseSeedsDefaults(t *testing.T) {
	seeds, err := searchers.ParseSeeds("")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(seeds) == 0 {
		t.Fatal("дефолтные сиды пусты")
	}
	for _, s := range seeds {
		if !strings.HasSuffix(s.Base, ".onion") {
			t.Errorf("сид %q не onion: %s", s.Name, s.Base)
		}
	}
}

func TestParseSeedsCustom(t *testing.T) {
	seeds, err := searchers.ParseSeeds("mine|http://x.onion|/s?q={q}|a.link|market")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(seeds) != 1 {
		t.Fatalf("сидов %d, ожидала 1", len(seeds))
	}
	s := seeds[0]
	if s.Name != "mine" || s.Base != "http://x.onion" || s.Path != "/s?q={q}" ||
		s.Selector != "a.link" || s.Category != "market" {
		t.Errorf("сид разобран неверно: %+v", s)
	}
}

func TestParseSeedsMarksClearnet(t *testing.T) {
	seeds, _ := searchers.ParseSeeds("ahmia-fi|https://ahmia.fi|/search/?q={q}")
	if len(seeds) != 1 {
		t.Fatalf("сидов %d", len(seeds))
	}
	if seeds[0].ViaTor {
		t.Error("clearnet-фронт не должен помечаться как tor-только")
	}
}

func TestEnginesFromSeeds(t *testing.T) {
	engines := searchers.EnginesFromSeeds(searchers.DefaultSeeds())
	if len(engines) == 0 {
		t.Fatal("движки не созданы")
	}
	for _, e := range engines {
		if e.Name() == "" || e.Base == "" {
			t.Errorf("движок пустой: %+v", e)
		}
	}
}

func TestStealthQueriesBrowserInParallel(t *testing.T) {
	ddg := &fixedSearcher{name: "ddg", res: []searchers.Result{{URL: "https://ddg.example/1"}}}
	browser := &fixedSearcher{name: "rod-browser", res: []searchers.Result{{URL: "https://bex.example/1"}}}
	e, _ := newEngine(t, ddg)
	e.Browser = browser

	out, err := e.Search(context.Background(), Options{Query: "avito квартира", Mode: router.ModeStealth})
	if err != nil {
		t.Fatal(err)
	}
	// Браузер обязан идти параллельно с HTTP-движками: при старом коде он
	// срабатывал только на пустой выдаче и его индекса в отчёте бы не было.
	names := map[string]bool{}
	for _, r := range out.Report.Engines {
		names[r.Name] = true
	}
	if !names["ddg"] || !names["rod-browser"] {
		t.Errorf("stealth опросил не всех: %v", names)
	}
	if out.Count != 2 {
		t.Errorf("count=%d, ожидала 2 (ddg + браузер)", out.Count)
	}
}

func TestFastKeepsBrowserAsLastResort(t *testing.T) {
	empty := &fixedSearcher{name: "ddg"}
	browser := &fixedSearcher{name: "rod-browser", res: []searchers.Result{{URL: "https://bex.example/1"}}}
	e, _ := newEngine(t, empty)
	e.Browser = browser

	out, err := e.Search(context.Background(), Options{Query: "weather", Mode: router.ModeFast})
	if err != nil {
		t.Fatal(err)
	}
	if out.Count != 1 || out.Results[0].URL != "https://bex.example/1" {
		t.Errorf("фолбэк на браузер не сработал: %+v", out.Results)
	}
}

func TestDeepIncludesAhmiaBridge(t *testing.T) {
	srv := newAhmiaStub(t, `<html><body><ol>
<li class="result"><h4><a href="http://abcdefabcdefabcd.onion/docs">Onion docs</a></h4></li>
</ol></body></html>`)
	e, _ := newEngine(t)
	e.Ahmia = &searchers.AhmiaClear{Client: testDirectClient(t), BaseURL: srv}

	out, err := e.Search(context.Background(), Options{Query: "leak dump", Mode: router.ModeDeep})
	if err != nil {
		t.Fatal(err)
	}
	if out.Degraded {
		t.Error("мост отработал, а ответ помечен деградацией")
	}
	found := false
	for _, r := range out.Results {
		if r.URL == "http://abcdefabcdefabcd.onion/docs" {
			found = true
		}
	}
	if !found {
		t.Errorf("onion-результат моста потерян: %+v", out.Results)
	}
}

func TestDeepNewnymRetryOnDeadOnion(t *testing.T) {
	calls := 0
	dead := &searchers.OnionEngine{Name_: "dead", Base: "http://a.onion", Path: "/search?q={q}"}
	e, _ := newEngine(t)
	e.Onion = &searchers.OnionCatalog{Engines: []*searchers.OnionEngine{dead}}
	e.Rot = &countingRotator{calls: &calls}

	// Без клиента движок всегда ошибается: оба прогона пустые, но ротация
	// цепи обязана случиться ровно один раз между ними.
	out, err := e.Search(context.Background(), Options{Query: "leak database", Mode: router.ModeDeep})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("ротаций %d, ожидала 1 (повтор после мёртвых движков)", calls)
	}
	if !out.Degraded {
		t.Error("пустой deep без деградации")
	}
}

type countingRotator struct{ calls *int }

func (c *countingRotator) Kind() string                 { return "tor" }
func (c *countingRotator) TransportSpec() string        { return "socks5://127.0.0.1:9050" }
func (c *countingRotator) Rotate(context.Context) error { *c.calls++; return nil }
func (c *countingRotator) Healthy() bool                { return true }
func (c *countingRotator) Close() error                 { return nil }
