package search

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"voidsearchswag/internal/httpc"
	"voidsearchswag/internal/metrics"
	"voidsearchswag/internal/netx"
	"voidsearchswag/internal/searchers"
)

type DeepConfig struct {
	Transport    string
	Proxies      []string
	Timeout      time.Duration
	OnionEngines []*searchers.OnionEngine
	AhmiaBase    string
	SearXNGBase  string
	Headless     bool
	ChromePath   string
	Pool         *netx.ProxyPool
	Rotator      netx.Rotator
	Logger       netx.Logger
	CacheTTL     time.Duration
	Limit        int
	// AllowPrivateTarget разрешает FetchURL ходить на служебные и приватные
	// адреса: нужно локальному стенду и тестам, публичные входы не включают.
	AllowPrivateTarget bool
}

func Build(cfg DeepConfig) (*Engine, *httpc.Client, error) {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	client, err := httpc.NewClient(context.Background(), httpc.Options{
		Transport: cfg.Transport,
		Proxies:   cfg.Proxies,
		Timeout:   timeout,
		Pool:      cfg.Pool,
		Rotator:   cfg.Rotator,
		Logger:    cfg.Logger,
	})
	if err != nil {
		return nil, nil, err
	}

	// Прямой клиент для clearnet-движков.
	//
	// Он нужен всякий раз, когда основной клиент ходит через tor, то есть когда
	// задан ротатор. NewClient при ненулевом Rotator использует его и полностью
	// игнорирует Transport, поэтому один общий клиент означал, что запросы к
	// DuckDuckGo и SearXNG тоже идут через tor.
	//
	// Для быстрого режима это ломало саму идею режима: fast задуман как короткий
	// clearnet-путь, а фактически платил за tor-цепи. Tor exit-ноды медленные и
	// DuckDuckGo их регулярно блокирует, поэтому быстрый режим становился и
	// медленнее, и ненадёжнее глубокого - ровно наоборот.
	//
	// Отдельный прямой клиент здесь уже создавался для ahmia-моста с тем же
	// обоснованием в обратную сторону: «поиск по даркнету не должен зависеть от
	// tor-цепей». Теперь правило общее: onion-движки ходят через tor, clearnet -
	// напрямую, и ни один из двух путей не зависит от чужого транспорта.
	//
	// Когда ротатора нет, основной клиент и так прямой (static/pool/direct
	// достигают clearnet сами), поэтому второй клиент не создаётся: лишний
	// экземпляр плодил бы соединения и отпечатки без нужды.
	direct := client
	var directOwned bool
	if cfg.Rotator != nil {
		if dc, derr := httpc.NewClient(context.Background(), httpc.Options{
			Transport: "direct",
			Timeout:   timeout,
			Logger:    cfg.Logger,
		}); derr == nil {
			direct = dc
			directOwned = true
		} else if cfg.Logger != nil {
			cfg.Logger.Warnf("clearnet-движки без прямого клиента: %v", derr)
		}
	}

	// DuckDuckGo и SearXNG - clearnet-движки, поэтому идут через прямой клиент.
	ddg := []searchers.Searcher{
		&searchers.DuckDuckGo{Client: direct, Log: cfg.Logger},
		&searchers.DuckDuckGo{Client: direct, Log: cfg.Logger, Lite: true},
	}
	ahmia := &searchers.AhmiaClear{Client: direct, Log: cfg.Logger, BaseURL: cfg.AhmiaBase}

	var searxng *searchers.SearXNG
	if strings.TrimSpace(cfg.SearXNGBase) != "" {
		searxng = &searchers.SearXNG{Client: direct, Log: cfg.Logger, BaseURL: cfg.SearXNGBase}
	}

	var browser *searchers.Browser
	if cfg.Headless {
		browser = searchers.NewBrowser("", cfg.ChromePath, true, cfg.Logger)
	}

	// Onion-движки остаются на основном клиенте: им нужен tor.
	var catalog *searchers.OnionCatalog
	var health *searchers.HealthPool
	if len(cfg.OnionEngines) > 0 {
		for _, e := range cfg.OnionEngines {
			e.Client = client
		}
		catalog = &searchers.OnionCatalog{Engines: cfg.OnionEngines, Log: cfg.Logger}
		health = searchers.NewHealthPool(client, cfg.OnionEngines)
	}

	eng := &Engine{
		Client:             client,
		Metrics:            metrics.Default,
		Health:             health,
		Onion:              catalog,
		SearXNG:            searxng,
		Ahmia:              ahmia,
		DDG:                ddg,
		Log:                cfg.Logger,
		CacheTTL:           cfg.CacheTTL,
		DefaultN:           cfg.Limit,
		Rot:                cfg.Rotator,
		AllowPrivateTarget: cfg.AllowPrivateTarget,
	}
	if directOwned {
		eng.Direct = direct
	}
	if browser != nil {
		eng.Browser = browser
	}
	return eng, client, nil
}

// ProbeOnion обходит onion-движки и возвращает число живых, общее число и
// количество проверок, которые не состоялись из-за отсутствия tor или прокси.
// Третье число обязательно: без него «живых 0 из 7» при выключенном tor
// читалось как массовая смерть движков, а не как несостоявшаяся проверка.
func (e *Engine) ProbeOnion(ctx context.Context) (int, int, int) {
	if e.Health == nil || e.Onion == nil {
		return 0, 0, 0
	}
	rep := e.Health.ProbeAll(ctx, e.Onion.Snapshot())
	live, total := e.Health.HealthyCount()
	return live, total, rep.Skipped
}

func (e *Engine) Close() error {
	var errs []error
	if b, ok := e.Browser.(*searchers.Browser); ok && b != nil {
		if err := b.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if e.Direct != nil {
		if err := e.Direct.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if e.Client != nil {
		if err := e.Client.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// FetchURL забирает одну страницу по адресу, который диктует вызывающий: им
// пользуются инструмент fetch, разбор страницы по адресу и команда CLI.
//
// Адрес приходит извне, поэтому цель проходит барьер служебных сетей. Без него
// вызов инструмента читал локальные сервисы машины: живой замер ДО на бинаре
// 79d24a8 дал fetch("http://127.0.0.1:18099/") -> статус 200 и тело локального
// стенда, и fetch("http://localhost:18099/internal") -> то же самое, потому что
// единственный фильтр транспорта (onionBlocked) проверяет зону .onion. Схема
// file:// отсеивалась и раньше, но уже транспортом и после попытки запроса.
//
// Барьер стоит здесь, а не в сессии транспорта, намеренно: внутри продукта
// приватные адреса легальны - пиры синхронизации могут жить в одной локальной
// сети, список прокси способен отдавать локальный стенд, а tor-сокет всегда
// 127.0.0.1. Диктует адрес именно внешний вызывающий этих трёх путей.
//
// Onion-адрес барьеру не показывается вовсе. Зона .onion не резолвится
// прямым DNS - имена живёт только резолвер тора, - поэтому CheckPublicTarget
// на таком адресе обречён: lookup отдаёт NXDOMAIN, и fetch возвращал «имя не
// разрешается» за 50 мс на живом onion-сайте. Жалоба смоук-агента этапа 165:
// validate признавал адрес живым (он ходит через tor), а fetch тут же ронял
// тот же адрес - инструменты противоречили друг другу. Опасности нет: адрес
// .onion не может указать в приватную сеть, это не IP, а маршрутизирует его
// tor-сокет.
func (e *Engine) FetchURL(ctx context.Context, rawURL string) (*httpc.Response, error) {
	if e.Client == nil {
		return nil, errors.New("http-клиент не инициализирован")
	}
	if !e.AllowPrivateTarget && !strings.HasSuffix(urlHost(rawURL), ".onion") {
		if err := netx.CheckPublicTarget(ctx, rawURL); err != nil {
			return nil, err
		}
	}
	return e.Client.Fetch(ctx, httpc.Request{URL: rawURL, Method: http.MethodGet})
}

func (e *Engine) HealthReport() []searchers.EngineHealth {
	if e.Health == nil {
		return nil
	}
	return e.Health.All()
}
