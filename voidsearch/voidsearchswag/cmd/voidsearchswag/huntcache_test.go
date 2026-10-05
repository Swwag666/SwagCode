package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"voidsearchswag/internal/hunt"
	"voidsearchswag/internal/metrics"
	"voidsearchswag/internal/router"
	"voidsearchswag/internal/search"
	"voidsearchswag/internal/searchers"
	"voidsearchswag/internal/store"
)

// poisonedURL - адрес, которого в живой выдаче нет и быть не может. Если он
// всплывает в результате прогона, значит выдача пришла из кэша, а не из движков.
const poisonedURL = "https://cached-poison.invalid/x"

// cacheEngine собирает ядро с живой выдачей и базой, где кэш работает по-настоящему:
// TTL час, как в дефолтном конфиге.
func cacheEngine(t *testing.T, urls ...string) (*store.Store, *search.Engine) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	eng := &search.Engine{
		Store:    st,
		Metrics:  metrics.New(),
		CacheTTL: time.Hour,
		DDG:      []searchers.Searcher{huntLiveSearcher{"ddg-html", urls}},
	}
	return st, eng
}

// poisonCache подменяет url в записи кэша так, как это делает чужой снимок
// выдачи часовой давности: ключ и режим те же, содержимое другое.
func poisonCache(t *testing.T, st *store.Store, query, mode, live string) {
	t.Helper()
	ctx := context.Background()
	key := store.CacheKey(query, mode)
	payload, ok, err := st.CacheGet(ctx, key)
	if err != nil {
		t.Fatalf("cache get: %v", err)
	}
	if !ok {
		t.Fatal("кэш пуст: подменять нечего, тест проверял бы сам себя")
	}
	if !strings.Contains(payload, live) {
		t.Fatalf("в кэше нет %q", live)
	}
	if err := st.CachePut(ctx, key, mode, strings.Replace(payload, live, poisonedURL, -1), time.Hour); err != nil {
		t.Fatalf("cache put: %v", err)
	}
}

func hasURL(urls []string, want string) bool {
	for _, u := range urls {
		if u == want {
			return true
		}
	}
	return false
}

// Охота обязана видеть живую выдачу. Кэш поиска живёт час, и прочитанный из него
// снимок охота сравнивает со своим последним hash как свежий факт: в пределах
// часа она рапортует «выдача стабильна» без единого запроса, а на чужом снимке -
// ложную находку.
func TestHuntSearchFreshIgnoresCache(t *testing.T) {
	st, eng := cacheEngine(t, "http://live.onion/a", "http://live.onion/b")
	ctx := context.Background()

	// Запись в кэш появляется обычным путём: так её оставляет любой search.
	if _, err := fileHostSearch(eng)(ctx, "leak database", "fast", 5); err != nil {
		t.Fatalf("первый прогон: %v", err)
	}
	poisonCache(t, st, "leak database", string(router.ModeFast), "http://live.onion/a")

	// Кэширующий адаптер обязан яд прочитать: без этой проверки невозможно
	// отличить «охота не читает кэш» от «кэш вообще не работает».
	cached, err := fileHostSearch(eng)(ctx, "leak database", "fast", 5)
	if err != nil {
		t.Fatalf("кэширующий прогон: %v", err)
	}
	if !hasURL(cached.URLs, poisonedURL) {
		t.Fatalf("кэш не прочитан, фикстура не сработала: %v", cached.URLs)
	}

	fresh, err := huntSearchFresh(eng)(ctx, "leak database", "fast", 5)
	if err != nil {
		t.Fatalf("свежий прогон: %v", err)
	}
	if hasURL(fresh.URLs, poisonedURL) {
		t.Errorf("охота прочитала кэш: %v", fresh.URLs)
	}
	if len(fresh.URLs) != 2 || fresh.URLs[0] != "http://live.onion/a" || fresh.URLs[1] != "http://live.onion/b" {
		t.Errorf("выдача %v, хочу живую [a b]", fresh.URLs)
	}
}

// Этап 179: контракт no_cache перевёрнут по живому дефекту (смоук-агент
// поймал стейл-выдачу: свежий no_cache-прогон не обновлял запись, и
// следующий запрос читал из кэша опровергнутые данные). Теперь no_cache
// отключает чтение, а запись обновляет. Для охоты это значит: её свежий
// прогон ложит в кэш живую выдачу - юзерский запрос после неё читает
// актуальный снимок, а не снимок часовой давности. Прежний тест требовал
// «не писать вовсе», но молчаливый стейл хуже записи.
func TestHuntSearchFreshWritesLiveSnapshot(t *testing.T) {
	st, eng := cacheEngine(t, "http://live.onion/a")
	ctx := context.Background()

	if _, err := huntSearchFresh(eng)(ctx, "leak database", "fast", 5); err != nil {
		t.Fatalf("прогон: %v", err)
	}
	key := store.CacheKey("leak database", string(router.ModeFast))
	payload, ok, err := st.CacheGet(ctx, key)
	if err != nil {
		t.Fatalf("cache get: %v", err)
	}
	if !ok {
		t.Fatal("свежий прогон охоты обязан обновлять запись кэша (этап 179: no_cache отключает чтение, не запись)")
	}
	if !strings.Contains(payload, "http://live.onion/a") {
		t.Errorf("запись не несёт живую выдачу: %.120s", payload)
	}

	// Кэширующий адаптер читает обновлённую запись и отдаёт живой адрес.
	cached, err := fileHostSearch(eng)(ctx, "leak database", "fast", 5)
	if err != nil {
		t.Fatalf("кэширующий прогон: %v", err)
	}
	if !hasURL(cached.URLs, "http://live.onion/a") {
		t.Errorf("кэш после охоты не отдал живой адрес: %v", cached.URLs)
	}
}

// Runner, собранный общей фабрикой, игнорирует кэш на всём пути охоты: hash
// считается от живой выдачи. Именно этот Runner получают фон, hunt run и hunt watch.
func TestNewHuntRunnerIgnoresCache(t *testing.T) {
	st, eng := cacheEngine(t, "http://live.onion/a", "http://live.onion/b")
	ctx := context.Background()

	// Прогрев кэша идёт с limit 20: у Runner.Limit ноль, ядро подставляет свой
	// дефолт, а запись кэша годится только при cached.Limit >= limit. Прогрев с
	// меньшим limit оставил бы запись непригодной, и тест проверял бы не чтение
	// кэша, а собственную фикстуру.
	if _, err := fileHostSearch(eng)(ctx, "leak database", "fast", 20); err != nil {
		t.Fatalf("прогрев кэша: %v", err)
	}
	poisonCache(t, st, "leak database", string(router.ModeFast), "http://live.onion/a")

	// Кэш действительно ядовит и действительно читается: без этой проверки
	// «охота не прочитала кэш» неотличимо от «кэш не сработал».
	probe, err := fileHostSearch(eng)(ctx, "leak database", "fast", 20)
	if err != nil {
		t.Fatalf("контрольный прогон: %v", err)
	}
	if !hasURL(probe.URLs, poisonedURL) {
		t.Fatalf("фикстура не сработала, кэш не прочитан: %v", probe.URLs)
	}

	r := newHuntRunner(st, eng)
	id, err := r.Create(ctx, "leak database", "fast", 60)
	if err != nil {
		t.Fatal(err)
	}
	hit, err := r.RunOne(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if hasURL(hit.URLs, poisonedURL) {
		t.Errorf("базовый прогон охоты построен на кэше: %v", hit.URLs)
	}
	want := hunt.HashURLs([]string{"http://live.onion/a", "http://live.onion/b"})
	hunts, err := r.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(hunts) != 1 {
		t.Fatalf("охот %d, хочу 1", len(hunts))
	}
	if hunts[0].LastHash != want {
		t.Errorf("hash %s, хочу %s от живой выдачи: охота зафиксировала чужой снимок",
			hunts[0].LastHash, want)
	}

	// Второй прогон той же живой выдачи - не находка. На кэше он стал бы
	// находкой: hash от яда не совпал бы с hash от живых url.
	again, err := r.RunOne(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if again.Changed {
		t.Errorf("ложная находка на стабильной выдаче: %+v", again)
	}
	hits, err := r.Hits(ctx, id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Errorf("история находок непуста: %+v", hits)
	}
}

// Отказ по режиму не должен оставлять следов: охота с мусорным режимом
// останавливается с текстом причины и не трогает ни кэш, ни выдачу.
func TestHuntSearchFreshRejectsBadModeLeavesNoCache(t *testing.T) {
	st, eng := cacheEngine(t, "http://live.onion/a")
	if _, err := huntSearchFresh(eng)(context.Background(), "leak database", "телепорт", 5); err == nil {
		t.Error("режим-мусор принят")
	} else if !strings.Contains(err.Error(), "недопустимый режим охоты") {
		t.Errorf("ошибка = %v", err)
	}
	// Отказ по режиму не должен оставлять следов в кэше.
	if _, ok, err := st.CacheGet(context.Background(), store.CacheKey("leak database", "телепорт")); err != nil || ok {
		t.Errorf("кэш после отказа: ok=%v err=%v", ok, err)
	}
}

// findFileHosts ходит через кэширующий адаптер, и это остаётся так: вспомогательный
// прогон спрашивает одни и те же запросы подряд, а свежесть ему не нужна.
func TestFindFileHostsKeepsCacheSemantics(t *testing.T) {
	st, eng := cacheEngine(t, "http://files.onion/dump.zip")
	ctx := context.Background()
	fn := fileHostSearch(eng)
	if _, err := fn(ctx, "file host probe", "fast", 5); err != nil {
		t.Fatal(err)
	}
	key := store.CacheKey("file host probe", string(router.ModeFast))
	if _, ok, err := st.CacheGet(ctx, key); err != nil || !ok {
		t.Fatalf("записи кэша нет: ok=%v err=%v", ok, err)
	}
	second, err := fn(ctx, "file host probe", "fast", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.URLs) != 1 || second.URLs[0] != "http://files.onion/dump.zip" {
		t.Errorf("повторный прогон: %v", second.URLs)
	}
}
