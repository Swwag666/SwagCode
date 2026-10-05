package main

import (
	"context"

	"voidsearchswag/internal/config"
	"voidsearchswag/internal/netx"
	"voidsearchswag/internal/search"
	"voidsearchswag/internal/searchers"
	"voidsearchswag/internal/store"
)

func buildEngine(cfg config.Config, log netx.Logger, st *store.Store) (*search.Engine, func()) {
	ctx := context.Background()

	var rot netx.Rotator
	if cfg.TorEnabled() {
		nc := cfg.NetxConfig(log)
		// Бинарь нужен только для собственного демона: к внешнему tor
		// подключаемся по socks-адресу, и требовать при этом установленный
		// tor.exe значит отрезать пользователей системного tor.
		if nc.TorBinary != "" || nc.TorSocksAddr != "" {
			// Tor поднимается лениво, при первом обращении к транспорту.
			//
			// Быстрый режим не использует tor: DuckDuckGo и SearXNG ходят через
			// прямой клиент, а onion-движки в нём не вызываются. Прежняя версия
			// запускала StartTor безусловно, поэтому «search -mode fast» платила
			// 20-25 секунд bootstrap, которые не окупались ничем.
			//
			// Не запускать tor в быстром режиме вовсе нельзя: FallbacksFor(ModeFast)
			// содержит ModeDeep, и быстрый поиск без результатов проваливается в
			// deep, которому tor нужен. Ленивый ротатор сохраняет фолбэк и убирает
			// стоимость, когда она не нужна.
			rot = netx.LazyTor(nc)
		}
	}

	seeds, err := searchers.ParseSeeds(cfg.OnionEngines)
	if err != nil {
		log.Warnf("onion-сиды: %v (беру дефолтные)", err)
	}
	engines := searchers.EnginesFromSeeds(seeds)
	engines = mergeDBSeeds(ctx, log, st, engines)

	transport := cfg.Transport
	if rot != nil {
		transport = ""
	} else if transport == "tor" {
		transport = "direct"
	}

	engine, _, err := search.Build(search.DeepConfig{
		Transport:          transport,
		Proxies:            cfg.Proxies,
		Timeout:            cfg.RequestTimeout,
		OnionEngines:       engines,
		SearXNGBase:        cfg.SearXNGURL,
		Headless:           cfg.Headless,
		ChromePath:         cfg.ChromePath,
		Rotator:            rot,
		Logger:             log,
		CacheTTL:           cfg.CacheTTL,
		Limit:              cfg.ResultLimit,
		AllowPrivateTarget: cfg.AllowPrivateTarget,
	})
	if err != nil {
		fatalf("поисковое ядро: %v", err)
	}
	engine.Store = st
	wireHealthPersist(ctx, log, st, engine)

	cleanup := func() {
		engine.Close()
		if rot != nil {
			rot.Close()
		}
	}
	_ = ctx
	return engine, cleanup
}

// wireHealthPersist связывает пул здоровья движков с SQLite: статистика
// прошлого запуска подтягивается, новая пишется после каждого обновления.
// Без этого рестарт сервера обнулял счёт и движки учились заново.
func wireHealthPersist(ctx context.Context, log netx.Logger, st *store.Store, engine *search.Engine) {
	if engine.Health == nil {
		return
	}
	if saved, err := st.LoadEngineHealth(ctx); err != nil {
		log.Warnf("здоровье движков не прочитано: %v", err)
	} else if n := engine.Health.Restore(toSearcherHealth(saved)); n > 0 {
		log.Infof("здоровье движков восстановлено: %d", n)
	}
	engine.Health.SetPersist(func(eh searchers.EngineHealth) {
		_ = st.SaveEngineHealth(context.Background(), toStoreHealth(eh))
	})
}

func toSearcherHealth(list []store.EngineHealth) []searchers.EngineHealth {
	out := make([]searchers.EngineHealth, 0, len(list))
	for _, h := range list {
		out = append(out, searchers.EngineHealth{
			Name: h.Name, URL: h.URL, Live: h.Live, LatencyAvg: h.LatencyAvg,
			SuccessRate: h.SuccessRate, FailStreak: h.FailStreak, Probes: h.Probes,
			Successes: h.Successes, LastProbe: h.LastProbe, Disabled: h.Disabled,
		})
	}
	return out
}

func toStoreHealth(h searchers.EngineHealth) store.EngineHealth {
	return store.EngineHealth{
		Name: h.Name, URL: h.URL, Live: h.Live, LatencyAvg: h.LatencyAvg,
		SuccessRate: h.SuccessRate, FailStreak: h.FailStreak, Probes: h.Probes,
		Successes: h.Successes, LastProbe: h.LastProbe, Disabled: h.Disabled,
	}
}

// mergeDBSeeds доклеивает движки из таблицы engines к зашитым сидам.
// Имена обязаны быть уникальны: пул здоровья хранит записи в map по имени.
func mergeDBSeeds(ctx context.Context, log netx.Logger, st *store.Store, base []*searchers.OnionEngine) []*searchers.OnionEngine {
	saved, err := st.ListEngineSeeds(ctx)
	if err != nil {
		log.Warnf("движки из базы не прочитаны: %v", err)
		return base
	}
	seen := map[string]bool{}
	for _, e := range base {
		seen[e.Name()] = true
	}
	for _, s := range saved {
		if s.Name == "" || s.Base == "" || seen[s.Name] {
			continue
		}
		seen[s.Name] = true
		base = append(base, &searchers.OnionEngine{
			Name_:    s.Name,
			Base:     s.Base,
			Path:     s.Path,
			Selector: s.Selector,
			Client:   nil,
		})
	}
	return base
}
