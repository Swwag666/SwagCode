package netx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// listServer отдаёт список прокси в формате провайдера: строка на адрес.
// Через Endpoint провайдера сюда заворачивается Fetch, поэтому тесты пула
// идут без настоящей сети.
func listServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

const proxyList = `1.2.3.4:8080
5.6.7.8:3128
1.2.3.4:8080
# комментарий
мусор-без-порта

9.9.9.9:1080
`

func TestProxyRotatorNormalizesAndRotates(t *testing.T) {
	r, err := NewProxyRotator([]string{"1.2.3.4:1080", "  socks5://5.6.7.8:1080  ", "", "   "})
	if err != nil {
		t.Fatal(err)
	}
	if r.Kind() != "proxy" {
		t.Errorf("Kind=%q", r.Kind())
	}
	// Адрес без схемы обязан получить socks5:// - иначе транспорт
	// соберётся с неверным диалером.
	if got := r.TransportSpec(); got != "socks5://1.2.3.4:1080" {
		t.Errorf("первый spec %q, ожидала socks5://1.2.3.4:1080", got)
	}
	if err := r.Rotate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := r.TransportSpec(); got != "socks5://5.6.7.8:1080" {
		t.Errorf("после Rotate spec %q", got)
	}
	if err := r.Rotate(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Список зациклен: третий поворот возвращает первый адрес.
	if got := r.TransportSpec(); got != "socks5://1.2.3.4:1080" {
		t.Errorf("цикл сломан: %q", got)
	}
	if !r.Healthy() {
		t.Error("статичный ротатор не жив")
	}
	if err := r.Close(); err != nil {
		t.Errorf("закрытие: %v", err)
	}
}

func TestProxyRotatorEmptyList(t *testing.T) {
	for _, list := range [][]string{nil, {}, {"", "  ", "\t"}} {
		if _, err := NewProxyRotator(list); err == nil {
			t.Errorf("пустой список %v принят", list)
		}
	}
}

func TestProxyRotatorKeepsScheme(t *testing.T) {
	r, err := NewProxyRotator([]string{"http://1.2.3.4:8080"})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.TransportSpec(); got != "http://1.2.3.4:8080" {
		t.Errorf("схема перезаписана: %q", got)
	}
}

func TestDirectRotator(t *testing.T) {
	r, err := NewRotator(context.Background(), Config{Transport: "direct"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Kind() != "direct" {
		t.Errorf("Kind=%q", r.Kind())
	}
	if got := r.TransportSpec(); got != "" {
		t.Errorf("у прямого транспорта spec %q, ожидала пустой", got)
	}
	if err := r.Rotate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !r.Healthy() {
		t.Error("прямой ротатор не жив")
	}
	if err := r.Close(); err != nil {
		t.Error(err)
	}
}

func TestNewRotatorTransportCaseAndSpace(t *testing.T) {
	// Транспорт приходит из конфига, то есть из строки пользователя:
	// регистр и пробелы не должны ломать выбор ветки.
	for _, tr := range []string{"DIRECT", " direct ", "Direct"} {
		r, err := NewRotator(context.Background(), Config{Transport: tr})
		if err != nil {
			t.Fatalf("%q: %v", tr, err)
		}
		if r.Kind() != "direct" {
			t.Errorf("%q: Kind=%q", tr, r.Kind())
		}
	}
}

func TestNewRotatorUnknownTransport(t *testing.T) {
	_, err := NewRotator(context.Background(), Config{Transport: "телепорт"})
	if err == nil {
		t.Fatal("неизвестный транспорт принят")
	}
	if !strings.Contains(err.Error(), "телепорт") {
		t.Errorf("в ошибке не назван транспорт: %v", err)
	}
}

func TestNewRotatorTorWithoutBinary(t *testing.T) {
	// Без бинаря StartTor обязан отказать сразу и внятно: иначе tor молча
	// стартует без него и запросы падают по таймауту.
	// TorNoReuse обязателен: рядом может работать живой демон, опубликованный
	// командой tord, и переиспользование его эндпоинта законно проходит без
	// бинаря. Тест проверяет именно spawn-путь, поэтому отключаем поиск.
	_, err := NewRotator(context.Background(), Config{Transport: "tor", TorBinary: "   ", TorNoReuse: true})
	if err == nil {
		t.Fatal("tor без бинаря принят")
	}
	if !strings.Contains(err.Error(), "tor-бинарь") {
		t.Errorf("ошибка не объясняет причину: %v", err)
	}
}

func TestNewRotatorTorMissingBinaryFile(t *testing.T) {
	_, err := NewRotator(context.Background(), Config{
		Transport:  "tor",
		TorBinary:  filepath.Join(t.TempDir(), "нет-такого-tor.exe"),
		TorNoReuse: true,
	})
	if err == nil {
		t.Fatal("несуществующий бинарь принят")
	}
}

func TestNewRotatorProxyWithList(t *testing.T) {
	r, err := NewRotator(context.Background(), Config{
		Transport: "proxy",
		Proxies:   []string{"1.2.3.4:1080"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.Kind() != "proxy" {
		t.Errorf("Kind=%q", r.Kind())
	}
}

func TestNewRotatorProxyWithoutListOrProvider(t *testing.T) {
	_, err := NewRotator(context.Background(), Config{Transport: "proxy"})
	if err == nil {
		t.Fatal("proxy без списка и провайдера принят")
	}
	if !strings.Contains(err.Error(), "провайдер") {
		t.Errorf("ошибка не объясняет, чего не хватает: %v", err)
	}
}

func TestNewRotatorProxyWithProvider(t *testing.T) {
	srv := listServer(t, proxyList)
	dir := t.TempDir()

	r, err := NewRotator(context.Background(), Config{
		Transport:         "proxy",
		ProxyProvider:     "proxyscrape",
		ProxyProviderURL:  srv.URL + "/",
		ProxySkipVerify:   true,
		ProxyPoolSize:     3,
		ProxyFetchLimit:   10,
		ProxyStatePath:    filepath.Join(dir, "state.json"),
		ProxyProbeTimeout: time.Second,
		ProxyMinLive:      1,
		Logger:            nopLogger{},
	})
	if err != nil {
		t.Fatalf("провайдер не подхватился: %v", err)
	}
	if r.Kind() != "proxy" {
		t.Errorf("Kind=%q", r.Kind())
	}
	spec := r.TransportSpec()
	if !strings.Contains(spec, "1.2.3.4:8080") {
		t.Errorf("spec %q не из списка провайдера", spec)
	}
	if err := r.Close(); err != nil {
		t.Errorf("закрытие: %v", err)
	}
}

func TestNewRotatorPoolTransport(t *testing.T) {
	srv := listServer(t, proxyList)
	for _, tr := range []string{"pool", "proxyscrape"} {
		r, err := NewRotator(context.Background(), Config{
			Transport:        tr,
			ProxyProviderURL: srv.URL + "/",
			ProxySkipVerify:  true,
			ProxyPoolSize:    2,
			ProxyStatePath:   "off",
			Logger:           nopLogger{},
		})
		if err != nil {
			t.Fatalf("%s: %v", tr, err)
		}
		if r.Kind() != "proxy" {
			t.Errorf("%s: Kind=%q", tr, r.Kind())
		}
		r.Close()
	}
}

func TestNewRotatorPoolWithoutProviderUsesDefault(t *testing.T) {
	// Без имени провайдера pool обязан собрать провайдера по умолчанию, а не
	// упасть: иначе транспорт pool молча превращается в нерабочий.
	cfg := Config{Transport: "pool", ProxySkipVerify: true, ProxyPoolSize: 1}
	// Битый endpoint: NewProxyPool вызовет Fetch сразу, поэтому ошибка
	// провайдера вернётся детерминированно и без выхода в интернет.
	cfg.ProxyProviderURL = "http://127.0.0.1:1/"
	_, err := NewRotator(context.Background(), cfg)
	if err == nil {
		t.Fatal("битый endpoint не вернул ошибку")
	}
	if strings.Contains(err.Error(), "не задан поставщик") {
		t.Errorf("дефолтный провайдер не подставился: %v", err)
	}
}

func TestDefaultPoolProviderHonoursExplicitEndpoint(t *testing.T) {
	// Дефект, который здесь зафиксирован: при пустом ProxyProvider ветка pool
	// брала DefaultProxyScrape() с пустым Endpoint, и ProxyProviderURL молча
	// выбрасывался - запрос уходил на публичный api.proxyscrape.com вместо
	// указанного адреса. Проверка обязана оставаться локальной.
	p := defaultPoolProvider(Config{ProxyProviderURL: "http://127.0.0.1:1/mylist"})
	ps, ok := p.(ProxyScrape)
	if !ok {
		t.Fatalf("тип %T, ожидала ProxyScrape", p)
	}
	if ps.Endpoint != "http://127.0.0.1:1/mylist" {
		t.Errorf("endpoint %q: явный адрес выброшен", ps.Endpoint)
	}
	// Остальные параметры дефолта сохраняются.
	if ps.Protocol != "http" || ps.Country != "all" {
		t.Errorf("дефолты затёрты: %+v", ps)
	}
}

func TestDefaultPoolProviderHonoursScrapeParams(t *testing.T) {
	p := defaultPoolProvider(Config{
		ProxyProviderURL:     "http://127.0.0.1:1/",
		ProxyScrapeProtocol:  "socks5",
		ProxyScrapeCountry:   "DE",
		ProxyScrapeTimeoutMS: 4321,
		ProxyScrapeAnonymity: "elite",
		ProxyScrapeSSL:       "yes",
	})
	ps := p.(ProxyScrape)
	if ps.Protocol != "socks5" || ps.Country != "DE" || ps.TimeoutMS != 4321 ||
		ps.Anonymity != "elite" || ps.SSL != "yes" {
		t.Errorf("параметры scrape не проброшены: %+v", ps)
	}
	if ps.Name() != "proxyscrape/socks5" {
		t.Errorf("Name=%q", ps.Name())
	}
}

func TestDefaultPoolProviderPrefersExplicitList(t *testing.T) {
	// Пользователь задал адреса сам - ходить на публичный API не нужно
	// вовсе: список имеет приоритет над scrape.
	p := defaultPoolProvider(Config{Proxies: []string{"1.2.3.4:1080", "5.6.7.8:8080"}})
	sp, ok := p.(StaticProvider)
	if !ok {
		t.Fatalf("тип %T, ожидала StaticProvider", p)
	}
	if len(sp) != 2 {
		t.Errorf("адресов %d, ожидала 2", len(sp))
	}
	got, err := sp.Fetch(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != "socks5://1.2.3.4:1080" {
		t.Errorf("первый %q", got[0])
	}
}

func TestDefaultPoolProviderIgnoresBlankList(t *testing.T) {
	// Список из пустых строк не должен порождать провайдера без единого
	// адреса: тогда пул собрался бы пустым и каждый запрос падал.
	p := defaultPoolProvider(Config{Proxies: []string{"", "   ", "\t"}})
	if _, ok := p.(ProxyScrape); !ok {
		t.Fatalf("тип %T, ожидала откат к ProxyScrape", p)
	}
}

func TestDefaultPoolProviderFullDefault(t *testing.T) {
	// Пустой конфиг обязан дать рабочий публичный провайдер с его endpoint:
	// иначе транспорт pool без настроек не поднимется вовсе.
	p := defaultPoolProvider(Config{})
	ps := p.(ProxyScrape)
	if ps.Endpoint != DefaultProxyScrape().Endpoint {
		t.Errorf("endpoint %q, ожидала публичный %q", ps.Endpoint, DefaultProxyScrape().Endpoint)
	}
}

func TestNewRotatorPoolUsesExplicitListWithoutNetwork(t *testing.T) {
	// Со списком прокси транспорт pool обязан собраться без единого запроса
	// наружу: это проверка того, что приоритет списка доходит до NewRotator.
	r, err := NewRotator(context.Background(), Config{
		Transport:       "pool",
		Proxies:         []string{"1.2.3.4:1080"},
		ProxySkipVerify: true,
		ProxyPoolSize:   2,
		ProxyStatePath:  "off",
		Logger:          nopLogger{},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if r.Kind() != "proxy" {
		t.Errorf("Kind=%q", r.Kind())
	}
}

func TestProviderFromConfig(t *testing.T) {
	if p := providerFromConfig(Config{}); p != nil {
		t.Errorf("без имени провайдера вернулся %+v", p)
	}
	if p := providerFromConfig(Config{ProxyProvider: "неведомый"}); p != nil {
		t.Errorf("неизвестный провайдер принят: %+v", p)
	}

	for _, name := range []string{"proxyscrape", "ProxyScrape", " proxy-scrape ", "SCRAPE"} {
		p := providerFromConfig(Config{
			ProxyProvider:        name,
			ProxyScrapeProtocol:  "socks5",
			ProxyScrapeCountry:   "DE",
			ProxyScrapeTimeoutMS: 4321,
			ProxyScrapeAnonymity: "elite",
			ProxyScrapeSSL:       "yes",
			ProxyProviderURL:     "http://example.test/",
		})
		if p == nil {
			t.Fatalf("%q: провайдер не собран", name)
		}
		ps, ok := p.(ProxyScrape)
		if !ok {
			t.Fatalf("%q: тип %T, ожидала ProxyScrape", name, p)
		}
		if ps.Protocol != "socks5" || ps.Country != "DE" || ps.TimeoutMS != 4321 {
			t.Errorf("%q: параметры не проброшены: %+v", name, ps)
		}
		if ps.Endpoint != "http://example.test/" {
			t.Errorf("%q: endpoint %q", name, ps.Endpoint)
		}
	}
}

func TestTorRotatorWithoutProcess(t *testing.T) {
	// Все методы обязаны пережить nil-состояние: rotator создаётся до
	// подключения control-порта, и паника здесь уронила бы весь поиск.
	tr := &torRotator{proxyURL: "socks5://127.0.0.1:9050", cooldown: time.Second, log: nopLogger{}}

	if tr.Kind() != "tor" {
		t.Errorf("Kind=%q", tr.Kind())
	}
	if got := tr.TransportSpec(); got != "socks5://127.0.0.1:9050" {
		t.Errorf("TransportSpec=%q", got)
	}
	if err := tr.Rotate(context.Background()); err == nil {
		t.Error("Rotate без control не вернул ошибку")
	} else if !strings.Contains(err.Error(), "control") {
		t.Errorf("ошибка не называет причину: %v", err)
	}
	if tr.Healthy() {
		t.Error("Healthy()=true без процесса и control")
	}
	if err := tr.Close(); err != nil {
		t.Errorf("Close на пустом ротаторе: %v", err)
	}
	// Повторное закрытие не должно паниковать.
	if err := tr.Close(); err != nil {
		t.Errorf("повторный Close: %v", err)
	}
	tr.logf("сообщение %d", 1)
}

func TestTorRotatorHealthyWithNilCtrl(t *testing.T) {
	tr := &torRotator{}
	if tr.Healthy() {
		t.Error("без control-клиента ротатор не может быть жив")
	}
}

func TestDefaultConfigValues(t *testing.T) {
	c := DefaultConfig()
	if c.Transport != "tor" {
		t.Errorf("Transport=%q", c.Transport)
	}
	if !c.TorOwnProcess {
		t.Error("TorOwnProcess должен быть включён: иначе tor остаётся висеть после выхода")
	}
	if c.MinNewnymInterval <= 0 {
		t.Errorf("MinNewnymInterval=%v", c.MinNewnymInterval)
	}
	if c.RequestTimeout <= 0 {
		t.Errorf("RequestTimeout=%v", c.RequestTimeout)
	}
	// Нули в прокси-параметрах делают пул нерабочим: размер 0 не поднимет
	// ни одного прокси, таймаут 0 обрывает пробу мгновенно.
	if c.ProxyPoolSize <= 0 || c.ProxyMinLive <= 0 || c.ProxyFetchLimit <= 0 {
		t.Errorf("нулевые размеры пула: %+v", c)
	}
	if c.ProxyProbeTimeout <= 0 || c.ProxyProbeConcurrency <= 0 {
		t.Errorf("нулевые параметры пробы: %+v", c)
	}
	if c.ProxyScrapeProtocol == "" || c.ProxyScrapeCountry == "" {
		t.Errorf("пустые параметры scrape: %+v", c)
	}
}

func TestWithDefaultsFillsZeros(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	t.Setenv("PATH", "")

	c := Config{}
	c.WithDefaults()

	if c.Transport != "tor" {
		t.Errorf("Transport=%q", c.Transport)
	}
	if c.MinNewnymInterval != DefaultConfig().MinNewnymInterval {
		t.Errorf("MinNewnymInterval=%v", c.MinNewnymInterval)
	}
	if c.RequestTimeout != DefaultConfig().RequestTimeout {
		t.Errorf("RequestTimeout=%v", c.RequestTimeout)
	}
	if c.TorDataDir != DefaultTorDataDir() {
		t.Errorf("TorDataDir=%q, ожидала %q", c.TorDataDir, DefaultTorDataDir())
	}
}

func TestWithDefaultsKeepsExplicitValues(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	t.Setenv("PATH", "")

	c := Config{
		Transport:         "direct",
		MinNewnymInterval: 3 * time.Second,
		RequestTimeout:    9 * time.Second,
		TorBinary:         "C:/свой/tor.exe",
		TorDataDir:        filepath.Join(dir, "свой-tor"),
	}
	c.WithDefaults()

	if c.Transport != "direct" {
		t.Errorf("Transport затёрт: %q", c.Transport)
	}
	if c.MinNewnymInterval != 3*time.Second {
		t.Errorf("MinNewnymInterval затёрт: %v", c.MinNewnymInterval)
	}
	if c.RequestTimeout != 9*time.Second {
		t.Errorf("RequestTimeout затёрт: %v", c.RequestTimeout)
	}
	if c.TorBinary != "C:/свой/tor.exe" {
		t.Errorf("TorBinary затёрт: %q", c.TorBinary)
	}
	if c.TorDataDir != filepath.Join(dir, "свой-tor") {
		t.Errorf("TorDataDir затёрт: %q", c.TorDataDir)
	}
}

func TestTimeoutOrDefault(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want time.Duration
	}{
		{0, 90 * time.Second},
		{10 * time.Second, 90 * time.Second},
		{30 * time.Second, 90 * time.Second},
		{31 * time.Second, 91 * time.Second},
		{2 * time.Minute, 3 * time.Minute},
	}
	for _, c := range cases {
		if got := (Config{RequestTimeout: c.in}).timeoutOrDefault(); got != c.want {
			t.Errorf("timeoutOrDefault(%v)=%v, ожидала %v", c.in, got, c.want)
		}
	}
}

func TestFindTorBinaryFromEnv(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, TorBinaryName())
	if err := os.WriteFile(bin, []byte("заглушка"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VOIDSEARCH_TOR_BINARY", bin)

	if got := FindTorBinary(); got != bin {
		t.Errorf("FindTorBinary=%q, ожидала %q", got, bin)
	}
}

func TestFindTorBinaryIgnoresMissingEnvPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	t.Setenv("PATH", "")
	// Переменная задана, но файла нет: искать нужно дальше, а не
	// возвращать несуществующий путь - tor на нём не стартует.
	t.Setenv("VOIDSEARCH_TOR_BINARY", filepath.Join(dir, "нет-такого-tor"))

	if got := FindTorBinary(); got != "" {
		t.Errorf("вернулся несуществующий путь %q", got)
	}
}

func TestFindTorBinaryFromVendorDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	t.Setenv("VOIDSEARCH_TOR_BINARY", "")
	t.Setenv("PATH", "")

	vendor := filepath.Join(dir, "vendor", "tor")
	if err := os.MkdirAll(vendor, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(vendor, TorBinaryName())
	if err := os.WriteFile(bin, []byte("заглушка"), 0o755); err != nil {
		t.Fatal(err)
	}

	if got := FindTorBinary(); got != bin {
		t.Errorf("FindTorBinary=%q, ожидала vendor-путь %q", got, bin)
	}
}

func TestFindTorBinarySkipsVendorDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	t.Setenv("VOIDSEARCH_TOR_BINARY", "")
	t.Setenv("PATH", "")

	// Каталог с именем бинаря не должен приниматься за бинарь.
	if err := os.MkdirAll(filepath.Join(dir, "vendor", "tor", TorBinaryName()), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := FindTorBinary(); got != "" {
		t.Errorf("каталог принят за бинарь: %q", got)
	}
}

func TestRotatorInterfaceSatisfied(t *testing.T) {
	// Все три ротатора обязаны удовлетворять интерфейсу: иначе сборка ядра
	// не компилируется на месте подстановки.
	var _ Rotator = &torRotator{}
	var _ Rotator = &proxyRotator{}
	var _ Rotator = &directRotator{}
	var _ Rotator = &poolRotator{}
}
