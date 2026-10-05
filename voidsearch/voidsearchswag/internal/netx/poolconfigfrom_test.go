package netx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Предмет этапа 155: PoolConfigFrom и ProviderForPool - единственные места, где
// конфиг превращается в параметры прокси-пула. До этого этапа прогрев пула из
// CLI строил провайдера и PoolConfig своими руками и выбрасывал шесть полей
// конфига: ProxyProviderURL, ProxyScrapeAnonymity, ProxyScrapeSSL,
// ProxyScrapeTimeoutMS, ProxyProbeTimeout и ProxyProbeConcurrency, а путь state
// подставлял всегда дефолтным. Транспорт pool и poolcheck поэтому ходили на
// разные endpoint с разными параметрами пробы, и расхождение нигде не
// печаталось.
//
// queryServer отдаёт список прокси и запоминает строку запроса, с которой пришёл
// провайдер: проверка обязана видеть не только ответ, но и сам запрос, иначе
// выброшенные параметры провайдера остались бы невидимыми для теста.
func queryServer(t *testing.T, body string) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.URL.RawQuery)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
}

func TestPoolConfigFromPropagatesEveryField(t *testing.T) {
	prov := StaticProvider{"1.2.3.4:1080"}
	cfg := Config{
		ProxySkipVerify:       true,
		ProxyProbeTimeout:     3 * time.Second,
		ProxyProbeConcurrency: 5,
		ProxyFetchLimit:       4,
		ProxyPoolSize:         40,
		ProxyMinLive:          6,
		ProxyStatePath:        "/tmp/pool.json",
	}
	pc := PoolConfigFrom(cfg, prov)
	if pc.Provider == nil {
		t.Fatal("поставщик не подставлен в PoolConfig")
	}
	if got := pc.Provider.Name(); got != prov.Name() {
		t.Errorf("поставщик %q, ожидала %q", got, prov.Name())
	}
	if pc.Verify {
		t.Error("ProxySkipVerify=true обязан выключить проверку живости")
	}
	if pc.ProbeTimeout != 3*time.Second {
		t.Errorf("ProbeTimeout=%s, ожидала 3s", pc.ProbeTimeout)
	}
	if pc.Concurrency != 5 {
		t.Errorf("Concurrency=%d, ожидала 5", pc.Concurrency)
	}
	if pc.FetchLimit != 4 {
		t.Errorf("FetchLimit=%d, ожидала 4", pc.FetchLimit)
	}
	if pc.MaxLive != 40 {
		t.Errorf("MaxLive=%d, ожидала 40", pc.MaxLive)
	}
	// 6 живых при пуле 40: потолок 40/4=10, поэтому значение остаётся как задано.
	if pc.MinLive != 6 {
		t.Errorf("MinLive=%d, ожидала 6", pc.MinLive)
	}
	if pc.StatePath != "/tmp/pool.json" {
		t.Errorf("StatePath=%q, ожидала заданный путь", pc.StatePath)
	}
}

func TestPoolConfigFromKeepsPoolDefaultsForZeroConfig(t *testing.T) {
	// Нулевой конфиг не обязан ничего выдумывать: остаются дефолты пула, а не
	// дефолты Config. Разница видна на MaxLive - DefaultPoolConfig даёт 60, а
	// DefaultConfig конфига даёт 40; проверка фиксирует, какое число берёт
	// единственный путь к пулу.
	pc := PoolConfigFrom(Config{}, StaticProvider{"1.2.3.4:1080"})
	if !pc.Verify {
		t.Error("по умолчанию живость проверяется")
	}
	if pc.ProbeTimeout != 7*time.Second {
		t.Errorf("ProbeTimeout=%s, ожидала 7s", pc.ProbeTimeout)
	}
	if pc.Concurrency != 48 {
		t.Errorf("Concurrency=%d, ожидала 48", pc.Concurrency)
	}
	if pc.FetchLimit != 400 {
		t.Errorf("FetchLimit=%d, ожидала 400", pc.FetchLimit)
	}
	if pc.MaxLive != 60 {
		t.Errorf("MaxLive=%d, ожидала 60 из DefaultPoolConfig", pc.MaxLive)
	}
	if pc.MinLive != 3 {
		t.Errorf("MinLive=%d, ожидала 3", pc.MinLive)
	}
	if pc.RefillWait != 20*time.Second {
		t.Errorf("RefillWait=%s, ожидала 20s", pc.RefillWait)
	}
	// Пустой путь state - не «не сохранять», а «сохранять в каталог данных».
	if pc.StatePath != DefaultStatePath() {
		t.Errorf("StatePath=%q, ожидала %q", pc.StatePath, DefaultStatePath())
	}
}

func TestPoolConfigFromClampsMinLiveToQuarterOfPool(t *testing.T) {
	// Минимум живых выше четверти пула делает прогрев недостижимым: пул будет
	// вечно догреваться и ждать RefillWait. Правило то же, что у транспорта pool.
	cases := []struct {
		name     string
		size     int
		minLive  int
		wantMin  int
		wantSize int
	}{
		{"минимум ниже четверти", 40, 6, 6, 40},
		{"минимум равен четверти", 8, 2, 2, 8},
		{"минимум выше четверти", 8, 8, 2, 8},
		{"минимум при крошечном пуле", 3, 3, 1, 3},
		{"минимум при пуле из двух", 2, 1, 1, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pc := PoolConfigFrom(Config{ProxyPoolSize: c.size, ProxyMinLive: c.minLive}, StaticProvider{"1.2.3.4:1080"})
			if pc.MinLive != c.wantMin {
				t.Errorf("MinLive=%d, ожидала %d", pc.MinLive, c.wantMin)
			}
			if pc.MaxLive != c.wantSize {
				t.Errorf("MaxLive=%d, ожидала %d", pc.MaxLive, c.wantSize)
			}
		})
	}
}

func TestPoolConfigFromStatePathVariants(t *testing.T) {
	// Прогрев пула из CLI до этапа 155 всегда писал state в DefaultStatePath,
	// поэтому VOIDSEARCH_PROXY_STATE_PATH=off не выключал запись: setup --warm-pool
	// оставлял файл там, где оператор запретил его оставлять.
	cases := []struct {
		name string
		in   string
		want func() string
	}{
		{"пусто - каталог данных", "", func() string { return DefaultStatePath() }},
		{"off", "off", func() string { return "" }},
		{"none", "none", func() string { return "" }},
		{"дефис", "-", func() string { return "" }},
		{"OFF с пробелами и регистром", "  OFF  ", func() string { return "" }},
		{"свой путь", "/var/lib/vss/pool.json", func() string { return "/var/lib/vss/pool.json" }},
		{"свой путь с пробелами", "  /var/lib/vss/pool.json ", func() string { return "/var/lib/vss/pool.json" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pc := PoolConfigFrom(Config{ProxyStatePath: c.in}, StaticProvider{"1.2.3.4:1080"})
			if want := c.want(); pc.StatePath != want {
				t.Errorf("StatePath=%q, ожидала %q", pc.StatePath, want)
			}
		})
	}
}

func TestProxyScrapeEffectiveFillsDefaults(t *testing.T) {
	d := DefaultProxyScrape()
	if got := (ProxyScrape{}).Effective(); got != d {
		t.Errorf("пустой провайдер дал %+v, ожидала дефолт %+v", got, d)
	}
	partial := ProxyScrape{Protocol: "SOCKS5", TimeoutMS: 4321}.Effective()
	if partial.Protocol != "SOCKS5" || partial.TimeoutMS != 4321 {
		t.Errorf("заданные поля затёрты: %+v", partial)
	}
	if partial.Country != d.Country || partial.Anonymity != d.Anonymity ||
		partial.SSL != d.SSL || partial.Endpoint != d.Endpoint {
		t.Errorf("пустые поля не подставлены дефолтом: %+v", partial)
	}
	// Effective не приводит регистр: это дело url и Name. Иначе строка описания
	// пула показывала бы «socks5» там, где оператор задал SOCKS5.
	if got := (ProxyScrape{Country: "DE"}).Effective().Country; got != "DE" {
		t.Errorf("страна %q приведена к нижнему регистру", got)
	}
}

func TestProviderForPoolPrefersNamedProvider(t *testing.T) {
	// Явное имя провайдера сильнее списка адресов: оператор сказал, откуда брать
	// прокси, и список в этом случае - не источник пула.
	cfg := Config{
		ProxyProvider:        "  SCRAPE  ",
		Proxies:              []string{"1.2.3.4:1080"},
		ProxyProviderURL:     "http://127.0.0.1:1/mylist",
		ProxyScrapeProtocol:  "socks5",
		ProxyScrapeCountry:   "DE",
		ProxyScrapeTimeoutMS: 4321,
		ProxyScrapeAnonymity: "elite",
		ProxyScrapeSSL:       "yes",
	}
	p, ok := ProviderForPool(cfg).(ProxyScrape)
	if !ok {
		t.Fatalf("тип %T, ожидала ProxyScrape", ProviderForPool(cfg))
	}
	if p.Endpoint != "http://127.0.0.1:1/mylist" {
		t.Errorf("endpoint %q: явный адрес выброшен", p.Endpoint)
	}
	if p.Protocol != "socks5" || p.Country != "DE" || p.TimeoutMS != 4321 ||
		p.Anonymity != "elite" || p.SSL != "yes" {
		t.Errorf("параметры scrape потеряны: %+v", p)
	}
}

func TestProviderForPoolFallsBackOnUnknownName(t *testing.T) {
	// Неизвестное имя не должно ронять прогрев: откат к общему правилу, где
	// список адресов сильнее endpoint.
	cfg := Config{ProxyProvider: "неведомый", Proxies: []string{"1.2.3.4:1080", "5.6.7.8:8080"}}
	sp, ok := ProviderForPool(cfg).(StaticProvider)
	if !ok {
		t.Fatalf("тип %T, ожидала StaticProvider", ProviderForPool(cfg))
	}
	if len(sp) != 2 {
		t.Errorf("адресов %d, ожидала 2", len(sp))
	}
}

func TestProviderForPoolKeepsEndpointWithoutName(t *testing.T) {
	// Без имени провайдера endpoint и параметры scrape обязаны дойти до
	// провайдера: это ровно тот дефект, из-за которого poolcheck ходил на
	// публичный api.proxyscrape.com при явно заданном адресе.
	cfg := Config{
		ProxyProviderURL:     "http://127.0.0.1:1/list",
		ProxyScrapeProtocol:  "socks5",
		ProxyScrapeAnonymity: "elite",
		ProxyScrapeSSL:       "yes",
		ProxyScrapeTimeoutMS: 4321,
	}
	ps, ok := ProviderForPool(cfg).(ProxyScrape)
	if !ok {
		t.Fatalf("тип %T, ожидала ProxyScrape", ProviderForPool(cfg))
	}
	if ps.Endpoint != "http://127.0.0.1:1/list" || ps.Protocol != "socks5" ||
		ps.Anonymity != "elite" || ps.SSL != "yes" || ps.TimeoutMS != 4321 {
		t.Errorf("параметры не проброшены: %+v", ps)
	}
	if ps.Name() != "proxyscrape/socks5" {
		t.Errorf("Name=%q", ps.Name())
	}
}

func TestNewRotatorPoolAsksEndpointWithConfiguredQuery(t *testing.T) {
	// Транспорт pool и прогрев пула обязаны спрашивать одно и то же. Проверка
	// идёт через настоящий NewRotator: он строит поставщика тем же
	// ProviderForPool, которым теперь пользуется CLI, поэтому совпадение строки
	// запроса доказывает совпадение обоих путей, а не только формулы внутри
	// PoolConfigFrom.
	srv, queries := queryServer(t, proxyList)
	cfg := Config{
		Transport:            "pool",
		ProxyProvider:        "proxyscrape",
		ProxyProviderURL:     srv.URL + "/v2/",
		ProxyScrapeProtocol:  "socks5",
		ProxyScrapeCountry:   "DE",
		ProxyScrapeTimeoutMS: 4321,
		ProxyScrapeAnonymity: "elite",
		ProxyScrapeSSL:       "yes",
		ProxySkipVerify:      true,
		ProxyPoolSize:        2,
		ProxyMinLive:         1,
		ProxyFetchLimit:      4,
		ProxyStatePath:       "off",
		Logger:               nopLogger{},
	}
	r, err := NewRotator(context.Background(), cfg)
	if err != nil {
		t.Fatalf("транспорт pool не поднялся: %v", err)
	}
	defer r.Close()

	got := queries()
	if len(got) == 0 {
		t.Fatal("endpoint не получил ни одного запроса")
	}
	want := "request=getproxies&protocol=socks5&timeout=4321&country=de&ssl=yes&anonymity=elite"
	if got[0] != want {
		t.Errorf("запрос %q, ожидала %q", got[0], want)
	}
	// Тот же конфиг глазами CLI: описание прогрева обязано назвать ровно эти
	// параметры, иначе оператор видит одно, а запрос уходит другой.
	pc := PoolConfigFrom(cfg, ProviderForPool(cfg))
	if pc.Verify {
		t.Error("проверка живости не выключена")
	}
	if pc.FetchLimit != 4 || pc.MaxLive != 2 || pc.MinLive != 1 {
		t.Errorf("пределы %+v, ожидала выборку 4, пул 2, минимум 1", pc)
	}
	if pc.StatePath != "" {
		t.Errorf("StatePath=%q, ожидала пустой для off", pc.StatePath)
	}
	if ps, ok := pc.Provider.(ProxyScrape); !ok {
		t.Fatalf("тип поставщика %T, ожидала ProxyScrape", pc.Provider)
	} else if !strings.Contains(ps.url(), want) {
		t.Errorf("url прогрева %q не содержит %q", ps.url(), want)
	}
}
