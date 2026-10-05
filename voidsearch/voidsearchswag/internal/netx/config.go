package netx

import (
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

type Config struct {
	TorBinary     string
	TorDataDir    string
	TorOwnProcess bool

	// TorSocksAddr подключает процесс к уже запущенному tor вместо поднятия
	// собственного демона. Формат host:port, например 127.0.0.1:9050.
	// TorControlAddr необязател: без него недоступны NEWNYM и проверка версии,
	// но транспорт работает, а здоровость проверяется доступностью socks-порта.
	TorSocksAddr   string
	TorControlAddr string

	// TorNoReuse запрещает подключаться к опубликованному эндпоинту живого
	// демона и требует поднимать собственный tor. Нужно команде tord: она сама
	// является источником эндпоинта, и подключение к себе же лишило бы смысла
	// весь запуск.
	TorNoReuse bool

	Transport string
	Proxies   []string

	ProxyProvider         string
	ProxyProviderURL      string
	ProxyScrapeProtocol   string
	ProxyScrapeCountry    string
	ProxyScrapeTimeoutMS  int
	ProxyScrapeAnonymity  string
	ProxyScrapeSSL        string
	ProxyPoolSize         int
	ProxyMinLive          int
	ProxyFetchLimit       int
	ProxyProbeTimeout     time.Duration
	ProxyProbeConcurrency int
	ProxySkipVerify       bool
	ProxyStatePath        string

	MinNewnymInterval time.Duration
	RequestTimeout    time.Duration

	Logger Logger
}

func DefaultConfig() Config {
	return Config{
		Transport:             "tor",
		TorOwnProcess:         true,
		MinNewnymInterval:     11 * time.Second,
		RequestTimeout:        45 * time.Second,
		ProxyScrapeProtocol:   "http",
		ProxyScrapeCountry:    "all",
		ProxyScrapeAnonymity:  "all",
		ProxyScrapeSSL:        "all",
		ProxyScrapeTimeoutMS:  8000,
		ProxyPoolSize:         40,
		ProxyMinLive:          3,
		ProxyFetchLimit:       400,
		ProxyProbeTimeout:     7 * time.Second,
		ProxyProbeConcurrency: 48,
	}
}

func (c *Config) WithDefaults() {
	d := DefaultConfig()
	if c.Transport == "" {
		c.Transport = d.Transport
	}
	if c.MinNewnymInterval <= 0 {
		c.MinNewnymInterval = d.MinNewnymInterval
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = d.RequestTimeout
	}
	if c.TorBinary == "" {
		c.TorBinary = FindTorBinary()
	}
	if c.TorDataDir == "" {
		c.TorDataDir = DefaultTorDataDir()
	}
}

func (c Config) timeoutOrDefault() time.Duration {
	if c.RequestTimeout > 30*time.Second {
		return c.RequestTimeout + 60*time.Second
	}
	return 90 * time.Second
}

func FindTorBinary() string {
	if v := os.Getenv("VOIDSEARCH_TOR_BINARY"); v != "" {
		if _, err := os.Stat(v); err == nil {
			return v
		}
	}
	vendor := filepath.Join(VendorDir(), TorBinaryName())
	if st, err := os.Stat(vendor); err == nil && !st.IsDir() {
		return vendor
	}
	if p, err := exec.LookPath("tor"); err == nil {
		return p
	}
	exe, _ := os.Executable()
	wdir, _ := os.Getwd()
	candidates := []string{
		filepath.Join(wdir, "bin", TorBinaryName()),
	}
	if exe != "" {
		dir := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(dir, TorBinaryName()),
			filepath.Join(dir, "bin", TorBinaryName()),
		)
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	return ""
}
