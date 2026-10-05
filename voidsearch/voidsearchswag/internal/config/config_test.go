package config

import (
	"path/filepath"
	"testing"
	"time"

	"voidsearchswag/internal/netx"
)

func TestLoadDefaults(t *testing.T) {
	// Пустое окружение: все значения должны прийти из envDefault.
	for _, k := range []string{
		"VOIDSEARCH_TRANSPORT", "VOIDSEARCH_LOG_LEVEL", "VOIDSEARCH_TOR",
		"VOIDSEARCH_RESULT_LIMIT", "VOIDSEARCH_CACHE_TTL",
		"VOIDSEARCH_DISCOVER_DEPTH", "VOIDSEARCH_DISCOVER_MAX_HOSTS",
		"VOIDSEARCH_DISCOVER_DELAY", "VOIDSEARCH_DISCOVER_CONCURRENCY",
		"VOIDSEARCH_TOR_OWN_PROCESS", "VOIDSEARCH_PROBE_ON_DISCOVER",
		"VOIDSEARCH_REQUEST_TIMEOUT", "VOIDSEARCH_PROXY_POOL_SIZE",
	} {
		t.Setenv(k, "")
	}
	// t.Setenv с пустым значением выставляет пустую строку, а не удаляет
	// переменную, поэтому проверяем только те поля, что имеют дефолт и
	// разбираются из пустой строки.

	c, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.ResultLimit != 20 {
		t.Errorf("ResultLimit=%d, ожидала 20", c.ResultLimit)
	}
	if c.CacheTTL != time.Hour {
		t.Errorf("CacheTTL=%v, ожидала 1h", c.CacheTTL)
	}
	if c.DiscoverDepth != 2 {
		t.Errorf("DiscoverDepth=%d, ожидала 2", c.DiscoverDepth)
	}
	if c.DiscoverMaxHosts != 50 {
		t.Errorf("DiscoverMaxHosts=%d, ожидала 50", c.DiscoverMaxHosts)
	}
	if c.DiscoverPerHostDelay != 2*time.Second {
		t.Errorf("DiscoverPerHostDelay=%v, ожидала 2s", c.DiscoverPerHostDelay)
	}
	if c.DiscoverConcurrency != 4 {
		t.Errorf("DiscoverConcurrency=%d, ожидала 4", c.DiscoverConcurrency)
	}
	if !c.ProbeOnDiscover {
		t.Error("ProbeOnDiscover выключен по умолчанию")
	}
}

func TestLoadReadsEnv(t *testing.T) {
	t.Setenv("VOIDSEARCH_RESULT_LIMIT", "77")
	t.Setenv("VOIDSEARCH_CACHE_TTL", "15m")
	t.Setenv("VOIDSEARCH_DISCOVER_DEPTH", "5")
	t.Setenv("VOIDSEARCH_HEADLESS", "true")
	t.Setenv("VOIDSEARCH_PROXIES", "http://a:1,http://b:2")
	t.Setenv("VOIDSEARCH_REQUEST_TIMEOUT", "90s")

	c, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.ResultLimit != 77 {
		t.Errorf("ResultLimit=%d", c.ResultLimit)
	}
	if c.CacheTTL != 15*time.Minute {
		t.Errorf("CacheTTL=%v", c.CacheTTL)
	}
	if c.DiscoverDepth != 5 {
		t.Errorf("DiscoverDepth=%d", c.DiscoverDepth)
	}
	if !c.Headless {
		t.Error("Headless не прочитан")
	}
	if len(c.Proxies) != 2 || c.Proxies[0] != "http://a:1" {
		t.Errorf("Proxies=%v", c.Proxies)
	}
	if c.RequestTimeout != 90*time.Second {
		t.Errorf("RequestTimeout=%v", c.RequestTimeout)
	}
}

// Барьер служебных адресов выключен по умолчанию: это защита публичного входа,
// а не удобство, поэтому снимать её обязан явный переключатель, а не отсутствие
// настройки.
func TestLoadAllowPrivateTarget(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.AllowPrivateTarget {
		t.Error("барьер снят по умолчанию")
	}

	for _, v := range []string{"1", "true", "TRUE"} {
		t.Setenv("VOIDSEARCH_ALLOW_PRIVATE", v)
		c, err = Load()
		if err != nil {
			t.Fatalf("load при %q: %v", v, err)
		}
		if !c.AllowPrivateTarget {
			t.Errorf("значение %q не сняло барьер", v)
		}
	}

	t.Setenv("VOIDSEARCH_ALLOW_PRIVATE", "0")
	c, err = Load()
	if err != nil {
		t.Fatalf("load при 0: %v", err)
	}
	if c.AllowPrivateTarget {
		t.Error("значение 0 сняло барьер")
	}
}

// Открытый слушатель без токена запрещён политикой, поэтому переключатель
// осознанности тоже обязан быть выключен по умолчанию.
func TestLoadHTTPAllowOpen(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.HTTPAllowOpen {
		t.Error("открытый слушатель разрешён по умолчанию")
	}

	for _, v := range []string{"1", "true", "TRUE"} {
		t.Setenv("VOIDSEARCH_HTTP_ALLOW_OPEN", v)
		c, err = Load()
		if err != nil {
			t.Fatalf("load при %q: %v", v, err)
		}
		if !c.HTTPAllowOpen {
			t.Errorf("значение %q не включило переключатель", v)
		}
	}

	t.Setenv("VOIDSEARCH_HTTP_ALLOW_OPEN", "0")
	c, err = Load()
	if err != nil {
		t.Fatalf("load при 0: %v", err)
	}
	if c.HTTPAllowOpen {
		t.Error("значение 0 включило переключатель")
	}
}

func TestLoadRejectsBadValue(t *testing.T) {
	t.Setenv("VOIDSEARCH_RESULT_LIMIT", "не число")
	if _, err := Load(); err == nil {
		t.Error("нечисловое значение принято без ошибки")
	}
}

func TestTorEnabled(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"on", true},
		{"ON", true},
		{"true", true},
		{"", true},
		{"off", false},
		{"OFF", false},
		{"0", false},
		{"false", false},
		{"no", false},
		{"  off  ", false},
		{"да", true},
	}
	for _, c := range cases {
		cfg := Config{Tor: c.in}
		if got := cfg.TorEnabled(); got != c.want {
			t.Errorf("TorEnabled(%q) = %v, ожидала %v", c.in, got, c.want)
		}
	}
}

func TestEffectiveDataDir(t *testing.T) {
	c := Config{DataDir: "/tmp/custom"}
	if got := c.EffectiveDataDir(); got != "/tmp/custom" {
		t.Errorf("свой каталог не принят: %q", got)
	}

	c = Config{DataDir: "  "}
	if got := c.EffectiveDataDir(); got == "" || got == "  " {
		t.Errorf("пустой каталог не заменён дефолтом: %q", got)
	}
}

func TestDBPath(t *testing.T) {
	c := Config{DataDir: "/tmp/vss"}
	want := filepath.Join("/tmp/vss", "voidsearchswag.db")
	if got := c.DBPath(); got != want {
		t.Errorf("DBPath=%q, ожидала %q", got, want)
	}
}

func TestDBPathFollowsDataDir(t *testing.T) {
	a := Config{DataDir: "/one"}
	b := Config{DataDir: "/two"}
	if a.DBPath() == b.DBPath() {
		t.Error("путь базы не следует за каталогом")
	}
}

func TestNetxConfigCarriesTransport(t *testing.T) {
	c := Config{
		Transport:       "proxy",
		Proxies:         []string{"http://p:8080"},
		ProxyPoolSize:   11,
		TorOwnProcess:   false,
		RequestTimeout:  30 * time.Second,
		ProxySkipVerify: true,
	}
	nc := c.NetxConfig(nil)

	if nc.Transport != "proxy" {
		t.Errorf("Transport=%q", nc.Transport)
	}
	if len(nc.Proxies) != 1 || nc.Proxies[0] != "http://p:8080" {
		t.Errorf("Proxies=%v", nc.Proxies)
	}
	if nc.ProxyPoolSize != 11 {
		t.Errorf("ProxyPoolSize=%d", nc.ProxyPoolSize)
	}
	if nc.TorOwnProcess {
		t.Error("TorOwnProcess не проброшен")
	}
	if nc.RequestTimeout != 30*time.Second {
		t.Errorf("RequestTimeout=%v", nc.RequestTimeout)
	}
	if !nc.ProxySkipVerify {
		t.Error("ProxySkipVerify не проброшен")
	}
}

func TestNetxConfigAppliesDefaults(t *testing.T) {
	// Нулевые поля должны получить дефолты netx, иначе клиент получит
	// пустой конфиг и упадёт на старте.
	c := Config{}
	nc := c.NetxConfig(nil)
	if nc.ProxyPoolSize <= 0 {
		t.Errorf("ProxyPoolSize не заполнен: %d", nc.ProxyPoolSize)
	}
	if nc.RequestTimeout <= 0 {
		t.Errorf("RequestTimeout не заполнен: %v", nc.RequestTimeout)
	}
	if nc.ProxyProbeTimeout <= 0 {
		t.Errorf("ProxyProbeTimeout не заполнен: %v", nc.ProxyProbeTimeout)
	}
}

func TestNetxConfigLogger(t *testing.T) {
	var log netx.Logger
	c := Config{}
	nc := c.NetxConfig(log)
	if nc.Logger != nil {
		t.Error("nil-логгер заменён на непустой")
	}
}

func TestConfigStructHasNoMissingDefaults(t *testing.T) {
	// Дефолты читаются из envDefault: конфиг без окружения должен быть
	// полностью пригоден, без нулевых таймаутов.
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.RequestTimeout <= 0 {
		t.Error("RequestTimeout пуст")
	}
	if c.MinNewnymInterval <= 0 {
		t.Error("MinNewnymInterval пуст")
	}
	if c.ProxyProbeTimeout <= 0 {
		t.Error("ProxyProbeTimeout пуст")
	}
	if c.ProxyProbeConc <= 0 {
		t.Error("ProxyProbeConc пуст")
	}
	if c.LogLevel == "" {
		t.Error("LogLevel пуст")
	}
}
