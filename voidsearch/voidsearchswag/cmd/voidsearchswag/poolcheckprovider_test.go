package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"voidsearchswag/internal/netx"
)

// Предмет этапа 155: poolcheck и setup --warm-pool обязаны ходить на тот же
// endpoint и с теми же параметрами, что и транспорт pool. До правки обе команды
// строили netx.ProxyScrape из двух строк и выбрасывали ProxyProviderURL,
// ProxyScrapeAnonymity, ProxyScrapeSSL, ProxyScrapeTimeoutMS,
// ProxyProbeTimeout и ProxyProbeConcurrency, а путь state подставляли всегда
// дефолтным. Проверки ниже полностью локальные: настоящий api.proxyscrape.com
// не нужен, а сам запрос виден серверу целиком.
//
// providerServer отдаёт список прокси и запоминает строку запроса: без записи
// запроса тест видел бы только ответ и не отличил «параметры дошли» от
// «серверу всё равно».
func providerServer(t *testing.T, body string) (*httptest.Server, func() []string) {
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

// setPoolEnv задаёт всю прокси-часть окружения разом: по одиночке эти
// переменные задаются тестами выше, а предмет этого файла - их совместный
// путь в поставщика пула.
func setPoolEnv(t *testing.T, endpoint string) {
	t.Helper()
	t.Setenv("VOIDSEARCH_PROXY_PROVIDER_URL", endpoint)
	t.Setenv("VOIDSEARCH_PROXYSCRAPE_PROTOCOL", "socks5")
	t.Setenv("VOIDSEARCH_PROXYSCRAPE_COUNTRY", "ru")
	t.Setenv("VOIDSEARCH_PROXYSCRAPE_ANONYMITY", "elite")
	t.Setenv("VOIDSEARCH_PROXYSCRAPE_SSL", "yes")
	t.Setenv("VOIDSEARCH_PROXYSCRAPE_TIMEOUT_MS", "4321")
	t.Setenv("VOIDSEARCH_PROXY_PROBE_TIMEOUT", "3s")
	t.Setenv("VOIDSEARCH_PROXY_PROBE_CONCURRENCY", "5")
	t.Setenv("VOIDSEARCH_PROXY_MIN_LIVE", "1")
	t.Setenv("VOIDSEARCH_PROXY_SKIP_VERIFY", "true")
	t.Setenv("VOIDSEARCH_PROXY_POOL_SIZE", "2")
	t.Setenv("VOIDSEARCH_PROXY_FETCH_LIMIT", "4")
}

func TestPoolcheckAsksConfiguredEndpointWithConfiguredQuery(t *testing.T) {
	srv, queries := providerServer(t, "127.0.0.1:9050\n127.0.0.2:9051\n")
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	setPoolEnv(t, srv.URL+"/v2/")

	stdout, stderr, code := runMainSplit(t, "poolcheck", "--show", "0")
	if code != 0 {
		t.Fatalf("код возврата %d, stderr %s", code, firstN(stderr, 400))
	}
	wantLimits := "пределы: размер пула 2, выборка 4, протокол socks5, страна ru, проверка живости нет"
	if !strings.Contains(stdout, wantLimits) {
		t.Errorf("строка пределов не совпала, хочу %q, stdout: %s", wantLimits, firstN(stdout, 400))
	}
	wantProvider := "поставщик: proxyscrape/socks5, endpoint " + srv.URL + "/v2/" +
		", анонимность elite, ssl yes, таймаут запроса 4321 мс, проба 3s x 5, минимум живых 1"
	if !strings.Contains(stdout, wantProvider) {
		t.Errorf("строка поставщика не совпала, хочу %q, stdout: %s", wantProvider, firstN(stdout, 600))
	}
	got := queries()
	if len(got) == 0 {
		t.Fatal("endpoint не получил ни одного запроса: провайдер ушёл не по конфигу")
	}
	wantQuery := "request=getproxies&protocol=socks5&timeout=4321&country=ru&ssl=yes&anonymity=elite"
	if got[0] != wantQuery {
		t.Errorf("запрос %q, ожидала %q", got[0], wantQuery)
	}
}

func TestPoolcheckProtocolFlagBeatsConfigQuery(t *testing.T) {
	srv, queries := providerServer(t, "127.0.0.1:9050\n")
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	setPoolEnv(t, srv.URL+"/v2/")

	stdout, _, code := runMainSplit(t, "poolcheck", "--show", "0", "--protocol", "https")
	if code != 0 {
		t.Fatalf("код возврата %d", code)
	}
	if !strings.Contains(stdout, "поставщик: proxyscrape/https, endpoint "+srv.URL+"/v2/") {
		t.Errorf("флаг --protocol не сменил поставщика, stdout: %s", firstN(stdout, 400))
	}
	got := queries()
	if len(got) == 0 {
		t.Fatal("endpoint не получил ни одного запроса")
	}
	if !strings.Contains(got[0], "protocol=https") {
		t.Errorf("флаг --protocol не дошёл до запроса: %q", got[0])
	}
}

func TestPoolcheckStaticListSkipsEndpoint(t *testing.T) {
	// VOIDSEARCH_PROXIES - тоже источник пула, как и в транспорте: если
	// оператор задал адреса сам, ходить на публичный API не нужно вовсе.
	// Проверка обязана видеть отсутствие запроса, а не только строку
	// поставщика: иначе endpoint мог бы запрашиваться и отбрасываться.
	srv, queries := providerServer(t, "127.0.0.1:9050\n")
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	setPoolEnv(t, srv.URL+"/v2/")
	t.Setenv("VOIDSEARCH_PROXIES", "127.0.0.1:9050")

	stdout, stderr, code := runMainSplit(t, "poolcheck", "--show", "0")
	if code != 0 {
		t.Fatalf("код возврата %d, stderr %s", code, firstN(stderr, 400))
	}
	wantLimits := "пределы: размер пула 2, выборка 4, проверка живости нет"
	if !strings.Contains(stdout, wantLimits) {
		t.Errorf("у статического списка нет протокола и страны, хочу %q, stdout: %s",
			wantLimits, firstN(stdout, 400))
	}
	if !strings.Contains(stdout, "поставщик: static, адресов 1, проба 3s x 5") {
		t.Errorf("строка поставщика не назвала статический список, stdout: %s", firstN(stdout, 400))
	}
	if got := queries(); len(got) != 0 {
		t.Errorf("endpoint получил %d запросов при явном списке адресов", len(got))
	}
}

func TestPoolcheckStatePathSwitchDisablesWrite(t *testing.T) {
	// Прогрев до этапа 155 всегда писал state в DefaultStatePath, поэтому
	// выключение записи не работало: файл появлялся там, где оператор запретил
	// его оставлять. Регистр ключевого слова - часть того же дефекта: остальные
	// ключевые слова конфига (tor, transport, имя провайдера) регистр не
	// различают, а VOIDSEARCH_PROXY_STATE_PATH=OFF молча создавал файл «OFF».
	srv, _ := providerServer(t, "127.0.0.1:9050\n")
	cases := []struct {
		name string
		path string
	}{
		{"off выключает запись", "off"},
		{"OFF выключает запись", "OFF"},
		{" off с пробелами выключает запись", " off "},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			setOfflineProbeEnv(t, dir)
			setPoolEnv(t, srv.URL+"/v2/")

			t.Setenv("VOIDSEARCH_PROXY_STATE_PATH", c.path)
			_, _, code := runMainSplit(t, "poolcheck", "--show", "0")
			if code != 0 {
				t.Fatalf("код возврата %d", code)
			}
			state := filepath.Join(dir, "proxy-pool.json")
			if _, err := os.Stat(state); err == nil {
				t.Errorf("state записан при выключенной записи: %s", state)
			}
		})
	}
}

func TestPoolcheckStatePathDefaultLivesInDataDir(t *testing.T) {
	// Путь по умолчанию обязан лежать в каталоге данных, а не в рабочем
	// каталоге: прогрев из cron или systemd иначе оставлял бы state где попало.
	// Проверяется загрузкой: LoadPoolState читает этот путь и печатает факт
	// загрузки, а запись происходит только при изменениях состояния.
	srv, _ := providerServer(t, "127.0.0.1:9050\n")
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	setPoolEnv(t, srv.URL+"/v2/")
	t.Setenv("VOIDSEARCH_PROXY_STATE_PATH", "")
	t.Setenv("VOIDSEARCH_VERBOSE", "1")

	state := filepath.Join(dir, "proxy-pool.json")
	if err := os.WriteFile(state, []byte(`{"proxies":{},"banned_ip":{},"banned_net":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, stderr, code := runMainSplit(t, "poolcheck", "--show", "0")
	if code != 0 {
		t.Fatalf("код возврата %d, stderr %s", code, firstN(stderr, 400))
	}
	if !strings.Contains(stderr, "состояние пула: proxy-pool.json") {
		t.Errorf("state не загружен из каталога данных, stderr: %s", firstN(stderr, 600))
	}
}

func TestSetupWarmPoolAsksSameEndpointAsPoolcheck(t *testing.T) {
	// setup --warm-pool - второй путь к тому же пулу. Он обязан спросить тот
	// же endpoint тем же запросом и описать прогрев теми же строками, что
	// печатает poolcheck, иначе расхождение двух путей снова станет
	// невидимым. Полный локальный прогон: фиктивный бинарь tor в vendor
	// заставляет EnsureTor коротить без скачивания, браузер и bootstrap-проверка
	// отключаются флагами, а список прокси отдаёт локальный endpoint.
	// Ветка warm-pool не выполняется при --offline, поэтому прогон без него.
	srv, queries := providerServer(t, "127.0.0.1:9050\n127.0.0.2:9051\n")
	dir := t.TempDir()
	vendor := filepath.Join(dir, "vendor", "tor")
	if err := os.MkdirAll(vendor, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vendor, "tor.exe"), []byte("фиктивный бинарь"), 0o644); err != nil {
		t.Fatal(err)
	}
	setOfflineProbeEnv(t, dir)
	setPoolEnv(t, srv.URL+"/v2/")
	t.Setenv("VOIDSEARCH_VERBOSE", "1")

	_, stderr, code := runMainSplit(t, "setup",
		"--no-browser", "--skip-check", "--warm-pool")
	if code != 0 {
		t.Fatalf("код возврата %d, stderr %s", code, firstN(stderr, 800))
	}
	if !strings.Contains(stderr, "прогрев пула: размер пула 2, выборка 4, протокол socks5, страна ru, проверка живости нет") {
		t.Errorf("setup не назвал применённые пределы, stderr: %s", firstN(stderr, 800))
	}
	wantProvider := "поставщик: proxyscrape/socks5, endpoint " + srv.URL + "/v2/" +
		", анонимность elite, ssl yes, таймаут запроса 4321 мс, проба 3s x 5, минимум живых 1"
	if !strings.Contains(stderr, wantProvider) {
		t.Errorf("setup не описал поставщика как poolcheck, хочу %q, stderr: %s",
			wantProvider, firstN(stderr, 800))
	}
	if !strings.Contains(stderr, "пул прогрет: живых 2 из 2, state сохранён") {
		t.Errorf("пул не прогрет, stderr: %s", firstN(stderr, 800))
	}
	got := queries()
	if len(got) == 0 {
		t.Fatal("endpoint не получил ни одного запроса от setup --warm-pool")
	}
	wantQuery := "request=getproxies&protocol=socks5&timeout=4321&country=ru&ssl=yes&anonymity=elite"
	if got[0] != wantQuery {
		t.Errorf("запрос setup %q, ожидала %q (тот же, что у poolcheck)", got[0], wantQuery)
	}
}

// kindlessProvider - поставщик, не являющийся ни ProxyScrape, ни
// StaticProvider: ветка default в описании пула обязана называть его имя, а
// не падать.
type kindlessProvider struct{}

func (kindlessProvider) Name() string { return "самописный" }
func (kindlessProvider) Fetch(ctx context.Context, limit int) ([]string, error) {
	return nil, nil
}

func TestPoolParamsLinesDescribesAllProviderKinds(t *testing.T) {
	pc := netx.PoolConfig{
		Verify:       false,
		ProbeTimeout: 3 * time.Second,
		Concurrency:  5,
		MinLive:      1,
		MaxLive:      2,
		FetchLimit:   4,
	}

	lines := poolParamsLines(netx.ProxyScrape{Endpoint: "http://127.0.0.1:9/"}, pc)
	if !strings.Contains(lines[0], "протокол http, страна all") {
		t.Errorf("scrape: пределы не назвали дефолты Effective: %q", lines[0])
	}
	if !strings.Contains(lines[1], "endpoint http://127.0.0.1:9/, анонимность all, ssl all, таймаут запроса 8000 мс") {
		t.Errorf("scrape: поставщик не подставил дефолты Effective: %q", lines[1])
	}
	if !strings.Contains(lines[1], "проба 3s x 5, минимум живых 1") {
		t.Errorf("scrape: поставщик не назвал параметры пробы: %q", lines[1])
	}

	lines = poolParamsLines(netx.StaticProvider{"1.2.3.4:1080", "5.6.7.8:8080"}, pc)
	if strings.Contains(lines[0], "протокол") {
		t.Errorf("static: у списка нет протокола: %q", lines[0])
	}
	if !strings.Contains(lines[1], "static, адресов 2") {
		t.Errorf("static: поставщик не назвал список: %q", lines[1])
	}

	lines = poolParamsLines(kindlessProvider{}, pc)
	if !strings.Contains(lines[0], "размер пула 2, выборка 4") {
		t.Errorf("default: пределы не названы: %q", lines[0])
	}
	if !strings.Contains(lines[1], "самописный, проба 3s x 5, минимум живых 1") {
		t.Errorf("default: поставщик не назван: %q", lines[1])
	}
}
