package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"

	"voidsearchswag/internal/netx"
)

type Config struct {
	DataDir   string `env:"VOIDSEARCH_DATA_DIR"`
	LogLevel  string `env:"VOIDSEARCH_LOG_LEVEL" envDefault:"info"`
	HTTPAddr  string `env:"VOIDSEARCH_HTTP_ADDR"`
	HTTPToken string `env:"VOIDSEARCH_HTTP_TOKEN"`
	// Токен из файла вместо флага/переменной: --token светится в ps,
	// файл с 0600 - нет. Файл важнее пустого токена, но слабее явного.
	HTTPTokenFile string `env:"VOIDSEARCH_HTTP_TOKEN_FILE"`
	TLSCert       string `env:"VOIDSEARCH_TLS_CERT"`
	TLSKey        string `env:"VOIDSEARCH_TLS_KEY"`
	// AllowOpen разрешает слушать не-петлевой адрес без токена. Без него такое
	// сочетание отказывается поднимать сервер: политика проверяется в
	// mcpserver.CheckListenPolicy, а CLI дублирует проверку до тяжёлой
	// инициализации поискового ядра.
	HTTPAllowOpen bool `env:"VOIDSEARCH_HTTP_ALLOW_OPEN" envDefault:"false"`
	// RateLimitPerMin - потолок запросов в минуту с одного IP на HTTP-
	// сервере. Дефолт 120 режет сканеры и перебор, MCP-клиенту хватает
	// с запасом; 0 выключает лимит для приватных сетей за доверенным
	// реверс-прокси, который троттлит сам.
	RateLimitPerMin int    `env:"VOIDSEARCH_RATE_LIMIT" envDefault:"120"`
	Tor             string `env:"VOIDSEARCH_TOR" envDefault:"on"`

	Transport           string        `env:"VOIDSEARCH_TRANSPORT" envDefault:"tor"`
	Proxies             []string      `env:"VOIDSEARCH_PROXIES"`
	ProxyProvider       string        `env:"VOIDSEARCH_PROXY_PROVIDER"`
	ProxyProviderURL    string        `env:"VOIDSEARCH_PROXY_PROVIDER_URL"`
	ProxyScrapeProtocol string        `env:"VOIDSEARCH_PROXYSCRAPE_PROTOCOL" envDefault:"http"`
	ProxyScrapeCountry  string        `env:"VOIDSEARCH_PROXYSCRAPE_COUNTRY" envDefault:"all"`
	ProxyScrapeTimeout  int           `env:"VOIDSEARCH_PROXYSCRAPE_TIMEOUT_MS" envDefault:"8000"`
	ProxyScrapeAnonym   string        `env:"VOIDSEARCH_PROXYSCRAPE_ANONYMITY" envDefault:"all"`
	ProxyScrapeSSL      string        `env:"VOIDSEARCH_PROXYSCRAPE_SSL" envDefault:"all"`
	ProxyPoolSize       int           `env:"VOIDSEARCH_PROXY_POOL_SIZE" envDefault:"40"`
	ProxyMinLive        int           `env:"VOIDSEARCH_PROXY_MIN_LIVE" envDefault:"3"`
	ProxyFetchLimit     int           `env:"VOIDSEARCH_PROXY_FETCH_LIMIT" envDefault:"400"`
	ProxyProbeTimeout   time.Duration `env:"VOIDSEARCH_PROXY_PROBE_TIMEOUT" envDefault:"7s"`
	ProxyProbeConc      int           `env:"VOIDSEARCH_PROXY_PROBE_CONCURRENCY" envDefault:"48"`
	ProxySkipVerify     bool          `env:"VOIDSEARCH_PROXY_SKIP_VERIFY" envDefault:"false"`
	ProxyStatePath      string        `env:"VOIDSEARCH_PROXY_STATE_PATH"`

	TorBinary     string `env:"VOIDSEARCH_TOR_BINARY"`
	TorDataDir    string `env:"VOIDSEARCH_TOR_DATA_DIR"`
	TorOwnProcess bool   `env:"VOIDSEARCH_TOR_OWN_PROCESS" envDefault:"true"`
	// TorSocksAddr подключает процесс к уже запущенному tor вместо поднятия
	// собственного демона. Свой tor стартует 20-25 секунд, и каждый вызов CLI
	// платил эту цену заново; внешний демон греется один раз на всех.
	// Формат host:port, например 127.0.0.1:9050.
	TorSocksAddr      string        `env:"VOIDSEARCH_TOR_SOCKS"`
	TorControlAddr    string        `env:"VOIDSEARCH_TOR_CONTROL"`
	MinNewnymInterval time.Duration `env:"VOIDSEARCH_NEWNYM_INTERVAL" envDefault:"11s"`
	RequestTimeout    time.Duration `env:"VOIDSEARCH_REQUEST_TIMEOUT" envDefault:"45s"`
	// AllowPrivateTarget снимает барьер служебных и приватных адресов там, где
	// адрес диктует внешний вызывающий: инструмент fetch, команда classify и
	// разбор страницы по адресу. По умолчанию выключен, потому что запрос к
	// 127.0.0.1 или 169.254.169.254 от клиента MCP означает, что сервер
	// используют как прокси в чужую локальную сеть и в облачные метаданные.
	// Включать имеет смысл только для своего локального стенда и сознательно.
	AllowPrivateTarget bool `env:"VOIDSEARCH_ALLOW_PRIVATE" envDefault:"false"`

	ResultLimit  int           `env:"VOIDSEARCH_RESULT_LIMIT" envDefault:"20"`
	CacheTTL     time.Duration `env:"VOIDSEARCH_CACHE_TTL" envDefault:"1h"`
	Headless     bool          `env:"VOIDSEARCH_HEADLESS" envDefault:"false"`
	ChromePath   string        `env:"VOIDSEARCH_CHROME_PATH"`
	OnionEngines string        `env:"VOIDSEARCH_ONION_ENGINES"`

	DiscoverDepth        int           `env:"VOIDSEARCH_DISCOVER_DEPTH" envDefault:"2"`
	DiscoverMaxHosts     int           `env:"VOIDSEARCH_DISCOVER_MAX_HOSTS" envDefault:"50"`
	DiscoverPerHostDelay time.Duration `env:"VOIDSEARCH_DISCOVER_DELAY" envDefault:"2s"`
	DiscoverConcurrency  int           `env:"VOIDSEARCH_DISCOVER_CONCURRENCY" envDefault:"4"`
	ProbeOnDiscover      bool          `env:"VOIDSEARCH_PROBE_ON_DISCOVER" envDefault:"true"`

	// Настройки проб отделены от настроек обхода намеренно.
	//
	// Обход выкачивает содержимое и ищет ссылки: ему нужны осторожный
	// параллелизм (4) и полный таймаут запроса. Проба отвечает только на
	// вопрос «хост живой?», тело не читается. Раньше проба брала
	// DiscoverConcurrency=4 и RequestTimeout=45s, из-за чего один мёртвый
	// onion-адрес занимал воркер на 45 секунд, а волна из 10 адресов шла
	// минуту.
	//
	// Параллелизм проб выше обходного, потому что каждый воркер теперь ходит
	// собственной цепью tor (netx.IsolateSpec): они больше не выстраиваются в
	// очередь к одному выходу. Таймаут ниже полного, потому что живой
	// onion-сервис отвечает за секунды, а 20 секунд с запасом покрывают
	// медленную постройку цепи.
	//
	// Дефолт 16 выбран замером, а не на глаз. На общем демоне tor:
	//   12 воркеров - 1.36s на адрес, 16 - 1.19s, 24 - 0.86s, 40 - 0.50s.
	// Доля живых при этом скачет в пределах шума (57-67%), то есть до 24
	// точность не деградирует. 16 взято с запасом вниз от границы: на 40
	// воркерах tor-демон уже не успевает строить цепи, и живые хосты начинают
	// помечаться мёртвыми - это потеря данных, а не ускорение. Кто хочет
	// выжать больше, поднимает VOIDSEARCH_PROBE_CONCURRENCY осознанно.
	// Итог против исходного: 6.1s -> 1.19s на адрес, limit=1500 это ~30 минут
	// вместо часов.
	ProbeConcurrency int           `env:"VOIDSEARCH_PROBE_CONCURRENCY" envDefault:"16"`
	ProbeTimeout     time.Duration `env:"VOIDSEARCH_PROBE_TIMEOUT" envDefault:"20s"`

	SearXNGURL string `env:"VOIDSEARCH_SEARXNG_URL"`

	HuntBG             bool          `env:"VOIDSEARCH_HUNT_BG" envDefault:"true"`
	HuntInterval       time.Duration `env:"VOIDSEARCH_HUNT_INTERVAL" envDefault:"10m"`
	DiscoverBG         bool          `env:"VOIDSEARCH_DISCOVER_BG" envDefault:"true"`
	DiscoverBGInterval time.Duration `env:"VOIDSEARCH_DISCOVER_BG_INTERVAL" envDefault:"24h"`
	DiscoverBGProbe    int           `env:"VOIDSEARCH_DISCOVER_BG_PROBE" envDefault:"100"`
	PromoteLimit       int           `env:"VOIDSEARCH_PROMOTE_LIMIT" envDefault:"10"`

	BackupBG       bool          `env:"VOIDSEARCH_BACKUP_BG" envDefault:"true"`
	BackupInterval time.Duration `env:"VOIDSEARCH_BACKUP_INTERVAL" envDefault:"24h"`
	BackupKeep     int           `env:"VOIDSEARCH_BACKUP_KEEP" envDefault:"7"`
	BackupDir      string        `env:"VOIDSEARCH_BACKUP_DIR"`

	// Пиры локального кластера: адреса своих же инстансов через запятую.
	// Auth - общий Bearer-токен: инстансы обязаны делить один секрет.
	Peers []string `env:"VOIDSEARCH_PEERS"`
}

func Load() (Config, error) {
	if err := applyConfigFile(); err != nil {
		return Config{}, err
	}
	var c Config
	if err := env.Parse(&c); err != nil {
		return c, err
	}
	if err := c.ApplyTokenFile(); err != nil {
		return c, err
	}
	if err := c.Validate(); err != nil {
		return c, err
	}
	return c, nil
}

// clampInt приводит значение к диапазону, возвращая def для неположительных.
func clampInt(v, def, lo, hi int) int {
	if v <= 0 {
		return def
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// Validate приводит числовые и временные настройки к рабочим диапазонам и
// отклоняет то, что починить нельзя.
//
// env.Parse проверяет только разбираемость значения, но не его осмысленность,
// и раньше Load не делал вообще никакой проверки диапазонов. Непроверенные
// значения уходили дальше по коду и давали три разных класса отказа.
//
// Первый - переполнение. VOIDSEARCH_RESULT_LIMIT=9223372036854775807
// превращал выражение limit*2 в search.Engine в отрицательную границу, из-за
// чего окно добора открывалось после первого же результата и каждый поиск
// молча возвращал выдачу одного движка. VOIDSEARCH_PROMOTE_LIMIT того же
// порядка переполнял limit*3 в promoteTick.
//
// Второй - выделение памяти. Значения доходили до make([]T, 0, limit) в слое
// хранилища: PROMOTE_LIMIT=1000000000 означал запрос на сотни гигабайт и OOM
// через час после старта, в фоновой горутине. Сейчас хранилище защищено
// собственным потолком normLimit, но защита обязана быть и на входе.
//
// Третий - молчаливая деградация. VOIDSEARCH_CACHE_TTL=0 писал в кэш строки,
// которые уже истекли: попаданий не было никогда, а таблица cache росла
// бесконечно. Очень большой TTL переполнял time.Now().Add(ttl) в отрицательную
// метку, и кэш умирал навсегда без единого сообщения.
//
// Отрицательные длительности отклоняются, а не clamp-ятся: это почти всегда
// ошибка в значении, и тихая подстановка дефолта спрятала бы её.
func (c *Config) Validate() error {
	c.ResultLimit = clampInt(c.ResultLimit, 20, 1, 1000)
	c.PromoteLimit = clampInt(c.PromoteLimit, 10, 1, 200)
	c.DiscoverBGProbe = clampInt(c.DiscoverBGProbe, 100, 1, 5000)
	c.DiscoverDepth = clampInt(c.DiscoverDepth, 2, 1, 8)
	c.DiscoverMaxHosts = clampInt(c.DiscoverMaxHosts, 50, 1, 5000)
	c.DiscoverConcurrency = clampInt(c.DiscoverConcurrency, 4, 1, 128)
	c.ProbeConcurrency = clampInt(c.ProbeConcurrency, 16, 1, 256)
	c.BackupKeep = clampInt(c.BackupKeep, 7, 1, 365)
	c.ProxyPoolSize = clampInt(c.ProxyPoolSize, 40, 1, 10000)
	c.ProxyMinLive = clampInt(c.ProxyMinLive, 3, 1, c.ProxyPoolSize)
	c.ProxyFetchLimit = clampInt(c.ProxyFetchLimit, 400, 1, 100000)
	c.ProxyProbeConc = clampInt(c.ProxyProbeConc, 48, 1, 512)

	durations := []struct {
		name string
		d    *time.Duration
		def  time.Duration
		max  time.Duration
	}{
		{"VOIDSEARCH_CACHE_TTL", &c.CacheTTL, time.Hour, 30 * 24 * time.Hour},
		{"VOIDSEARCH_REQUEST_TIMEOUT", &c.RequestTimeout, 45 * time.Second, 10 * time.Minute},
		{"VOIDSEARCH_PROBE_TIMEOUT", &c.ProbeTimeout, 20 * time.Second, 10 * time.Minute},
		{"VOIDSEARCH_NEWNYM_INTERVAL", &c.MinNewnymInterval, 11 * time.Second, time.Hour},
		{"VOIDSEARCH_HUNT_INTERVAL", &c.HuntInterval, 10 * time.Minute, 30 * 24 * time.Hour},
		{"VOIDSEARCH_DISCOVER_BG_INTERVAL", &c.DiscoverBGInterval, 24 * time.Hour, 90 * 24 * time.Hour},
		{"VOIDSEARCH_BACKUP_INTERVAL", &c.BackupInterval, 24 * time.Hour, 90 * 24 * time.Hour},
		{"VOIDSEARCH_DISCOVER_DELAY", &c.DiscoverPerHostDelay, 2 * time.Second, 10 * time.Minute},
	}
	for _, e := range durations {
		if *e.d < 0 {
			return fmt.Errorf("config: %s не может быть отрицательным: %s", e.name, *e.d)
		}
		if *e.d == 0 {
			*e.d = e.def
		}
		// Отдельная проверка на переполнение при сложении с текущим временем:
		// очень большой TTL давал отрицательную метку истечения, и кэш умирал
		// молча и навсегда.
		if *e.d > e.max || time.Now().Add(*e.d).Before(time.Now()) {
			return fmt.Errorf("config: %s слишком большой: %s (предел %s)", e.name, *e.d, e.max)
		}
	}
	return nil
}

// ApplyTokenFile подтягивает Bearer-токен из файла, если явно не задан.
//
// Возвращает ошибку, и это принципиальное изменение. Прежняя версия глотала
// ошибку чтения молча, и опечатка в пути, неверные права или относительный
// путь, разрешённый от чужого cwd, приводили к тому, что HTTPToken оставался
// пустым. Пустой токен означает «аутентификация выключена»: checkBearer
// возвращает true всем. Сервер поднимался полностью открытым, включая fetch
// (SSRF), collect_files, backup_create (пишет снимок базы по заданному пути),
// /peer/export (весь пул и охоты) и /metrics. Единственным сигналом было
// общее предупреждение «HTTP без токена открыт всем», которое человек,
// уверенный, что он --token-file передал, читал как шум.
//
// Комментарий к прежней версии утверждал, что функция «предупреждает», - она
// физически не могла: ни возвращаемого значения, ни логирования у неё не было.
//
// Порядок проверки тоже важен: токен из окружения (VOIDSEARCH_HTTP_TOKEN)
// имеет приоритет, и файл его не затирает.
func (c *Config) ApplyTokenFile() error {
	if strings.TrimSpace(c.HTTPToken) != "" {
		return nil
	}
	path := strings.TrimSpace(c.HTTPTokenFile)
	if path == "" {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("файл токена %s: %w", path, err)
	}
	tok := strings.TrimSpace(string(raw))
	if tok == "" {
		return fmt.Errorf("файл токена %s пуст", path)
	}
	c.HTTPToken = tok
	return nil
}

// BackupDirOrDefault отдаёт каталог снимков: явный или backups/ в дате.
func (c Config) BackupDirOrDefault() string {
	if strings.TrimSpace(c.BackupDir) != "" {
		return c.BackupDir
	}
	return filepath.Join(c.EffectiveDataDir(), "backups")
}

// applyConfigFile подкладывает значения из JSON-файла в окружение до
// env.Parse - только те ключи, что не заданы явно. Приоритет:
// окружение > файл > дефолты. Файл ищется в VOIDSEARCH_CONFIG или в
// каталоге данных (config.json). Формат - плоский объект с env-именами:
//
//	{"VOIDSEARCH_TOR": "off", "VOIDSEARCH_RESULT_LIMIT": "50"}
//
// Неизвестные ключи игнорируются молча: файл - удобство, а не второй парсер
// конфига. Но «файла нет» и «файл сломан» - разные состояния, и различать их
// обязательно.
//
// Прежняя версия молча выбрасывала весь файл при любой ошибке разбора. Одна
// лишняя запятая или комментарий // означали, что сервер стартует на дефолтах
// без единого сообщения, и человек час отлаживал настройки, которые просто не
// применились.
//
// Пустая строка в окружении означает «не задано», и это сквозное соглашение
// проекта: так же трактует пустое значение TorEnabled, и на нём построены
// тесты дефолтов. Поэтому здесь Getenv, а не LookupEnv - переменная,
// выставленная в "", не должна блокировать значение из файла.
func applyConfigFile() error {
	path := strings.TrimSpace(os.Getenv("VOIDSEARCH_CONFIG"))
	explicit := path != ""
	if path == "" {
		path = filepath.Join(netx.DefaultDataDir(), "config.json")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		// Файла нет - нормальное состояние, работаем на окружении и дефолтах.
		// Но если путь задан явно через VOIDSEARCH_CONFIG, молчать нельзя:
		// человек просил конкретный файл, и опечатка в пути иначе выглядит как
		// «настройки игнорируются».
		if explicit {
			return fmt.Errorf("конфиг %s: %w", path, err)
		}
		return nil
	}
	var kv map[string]string
	if err := json.Unmarshal(raw, &kv); err != nil {
		// Битый файл не роняет загрузку - это решение осознанное и покрыто
		// тестом: config.json является удобством, а не критичным конфигом, и
		// отказ стартовать из-за лишней запятой хуже старта на дефолтах. Но
		// молчать больше нельзя: раньше весь файл выбрасывался без единого
		// слова, и настройки просто не применялись.
		fmt.Fprintf(os.Stderr, "WARNING: конфиг %s не разобран, работают дефолты: %v\n", path, err)
		return nil
	}
	for k, v := range kv {
		k = strings.TrimSpace(k)
		if !strings.HasPrefix(k, "VOIDSEARCH_") || k == "" {
			continue
		}
		if os.Getenv(k) != "" {
			continue
		}
		_ = os.Setenv(k, v)
	}
	return nil
}

func (c Config) TorEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(c.Tor)) {
	case "off", "0", "false", "no":
		return false
	}
	return true
}

func (c Config) EffectiveDataDir() string {
	if strings.TrimSpace(c.DataDir) != "" {
		return c.DataDir
	}
	return netx.DefaultDataDir()
}

func (c Config) DBPath() string {
	return filepath.Join(c.EffectiveDataDir(), "voidsearchswag.db")
}

func (c Config) NetxConfig(log netx.Logger) netx.Config {
	nc := netx.DefaultConfig()
	nc.Logger = log

	// Поля пробрасываются только когда заданы. Пустое значение в конфиге
	// означает «не задано», а не «ноль»: слепая перезапись затирала дефолты
	// netx нулями, и клиент получал пустой пул прокси с нулевым таймаутом.
	setStr(&nc.Transport, c.Transport)
	setStr(&nc.ProxyProvider, c.ProxyProvider)
	setStr(&nc.ProxyProviderURL, c.ProxyProviderURL)
	setStr(&nc.ProxyScrapeProtocol, c.ProxyScrapeProtocol)
	setStr(&nc.ProxyScrapeCountry, c.ProxyScrapeCountry)
	setStr(&nc.ProxyScrapeAnonymity, c.ProxyScrapeAnonym)
	setStr(&nc.ProxyScrapeSSL, c.ProxyScrapeSSL)
	setStr(&nc.ProxyStatePath, c.ProxyStatePath)
	setStr(&nc.TorBinary, c.TorBinary)
	setStr(&nc.TorDataDir, c.TorDataDir)
	setStr(&nc.TorSocksAddr, c.TorSocksAddr)
	setStr(&nc.TorControlAddr, c.TorControlAddr)

	setInt(&nc.ProxyScrapeTimeoutMS, c.ProxyScrapeTimeout)
	setInt(&nc.ProxyPoolSize, c.ProxyPoolSize)
	setInt(&nc.ProxyMinLive, c.ProxyMinLive)
	setInt(&nc.ProxyFetchLimit, c.ProxyFetchLimit)
	setInt(&nc.ProxyProbeConcurrency, c.ProxyProbeConc)

	setDur(&nc.ProxyProbeTimeout, c.ProxyProbeTimeout)
	setDur(&nc.MinNewnymInterval, c.MinNewnymInterval)
	setDur(&nc.RequestTimeout, c.RequestTimeout)

	if len(c.Proxies) > 0 {
		nc.Proxies = c.Proxies
	}
	if c.ProxySkipVerify {
		nc.ProxySkipVerify = true
	}
	if !c.TorOwnProcess {
		// Явный false: значение по умолчанию - true, поэтому отличие
		// от дефолта означает осознанное выключение.
		nc.TorOwnProcess = false
	}

	nc.WithDefaults()
	return nc
}

func setStr(dst *string, v string) {
	if strings.TrimSpace(v) != "" {
		*dst = v
	}
}

func setInt(dst *int, v int) {
	if v > 0 {
		*dst = v
	}
}

func setDur(dst *time.Duration, v time.Duration) {
	if v > 0 {
		*dst = v
	}
}
