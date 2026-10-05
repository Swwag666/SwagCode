package main

import (
	"context"
	"fmt"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/server"

	"voidsearchswag/internal/config"
	"voidsearchswag/internal/discover"
	"voidsearchswag/internal/hunt"
	"voidsearchswag/internal/mcpserver"
	"voidsearchswag/internal/metrics"
	"voidsearchswag/internal/netx"
	"voidsearchswag/internal/promote"
	"voidsearchswag/internal/search"
	"voidsearchswag/internal/store"
)

// startBackground запускает фоновые циклы сервера: охота прогоняет due-запросы
// и пушит находки клиентам, разведка пополняет пул onion-адресов и проверяет
// их живость. Циклы останавливаются отменой ctx при завершении run.
// Без них сервер - витрина статичной базы: пул протухает, охоты спят.
func startBackground(ctx context.Context, log netx.Logger, cfg config.Config, st *store.Store, engine *search.Engine, hunter *hunt.Runner, pool *discover.Pool, prober *discover.Prober, promoter *promote.Promoter, srv *server.MCPServer) {
	if cfg.HuntBG && hunter != nil {
		interval := cfg.HuntInterval
		if interval < time.Minute {
			interval = time.Minute
		}
		// Этап 178: раньше строка обещала только «каждые», и первый тик
		// через минуту был скрыт: смоук читал про 10m и ловил сетевую
		// активность раньше срока. Фраза обязана называть оба интервала.
		// Этап 178 (смоук-H): заодно названо молчание пустого прогона -
		// смоук искал тик охоты в логе и не нашёл ни одной строки за 26 минут.
		log.Infof("фон: охота: первый тик через %s, далее каждые %s; пустой прогон молчит, отказы - WARN",
			time.Minute, interval)
		every(ctx, time.Minute, interval, func(ctx context.Context) {
			huntTick(ctx, log, hunter, func(hits []hunt.Hit) {
				srv.SendNotificationToAllClients("notifications/hunt_update", map[string]any{
					"hits":  hits,
					"count": len(hits),
				})
			})
		})
	}
	if cfg.DiscoverBG && pool != nil {
		interval := cfg.DiscoverBGInterval
		if interval < time.Hour {
			interval = time.Hour
		}
		// Этап 178: первый тик разведки - через 5 минут (разогрев на свежих
		// данных), и фраза называет это: два смоука этапа видели сетевую
		// активность через ~5m после старта при логе «каждые 24h».
		// Этап 178 (смоук-H/I): тик разведки трёхфазный, и все фазы названы в
		// анонсе: пробы и промоут - фазы того же тика, а не отдельные циклы.
		// До правки в логе появлялись строки «фон: пробы», «фон: промоут»,
		// которых не было в стартовом анонсе, - два смоука посчитали их
		// незаявленными фоновыми задачами.
		probe := cfg.DiscoverBGProbe
		if probe <= 0 {
			probe = 0
		}
		log.Infof("фон: разведка: первый тик через %s, далее каждые %s; тик трёхфазный: адресов, пробы (до %d за тик), промоут движков",
			5*time.Minute, interval, probe)
		every(ctx, 5*time.Minute, interval, func(ctx context.Context) {
			discoverTick(ctx, log, cfg, pool, prober)
			promoteTick(ctx, log, st, engine, promoter, cfg.PromoteLimit)
		})
	}
	if cfg.BackupBG && st != nil {
		interval := cfg.BackupInterval
		if interval < time.Hour {
			interval = time.Hour
		}
		keep := cfg.BackupKeep
		if keep <= 0 {
			keep = 7
		}
		dir := cfg.BackupDirOrDefault()
		log.Infof("фон: бэкап: первый тик через %s, далее каждые %s (держим %d)", 10*time.Minute, interval, keep)
		every(ctx, 10*time.Minute, interval, func(ctx context.Context) {
			backupTick(ctx, log, st, dir, keep)
		})
	}
	if engine != nil {
		log.Infof("фон: судья: пересчёт весов: первый тик через %s, далее каждые %s", 12*time.Hour, 24*time.Hour)
		every(ctx, 12*time.Hour, 24*time.Hour, func(ctx context.Context) {
			if n := engine.RetuneBoosts(ctx); n > 0 {
				log.Infof("фон: судья: хостов с бонусом %d", n)
			}
		})
	}
	if len(cfg.Peers) > 0 && st != nil {
		log.Infof("фон: пиры: синк: первый тик через %s, далее каждые %s (%d)", 5*time.Minute, time.Hour, len(cfg.Peers))
		every(ctx, 5*time.Minute, time.Hour, func(ctx context.Context) {
			peerSyncTick(ctx, log, st, cfg.Peers, cfg.HTTPToken)
		})
	}
	if st != nil {
		// Чистка истёкшего кэша.
		//
		// Раньше CachePurgeExpired вызывался ровно один раз - при старте в
		// cmdRun. Сервер спроектирован долгоживущим и работает сутками, а
		// CachePut пишет полный JSON выдачи (до 200 результатов, десятки КБ) на
		// каждый непопавший в кэш поиск. Просроченные строки накапливались
		// бесконечно: файл базы рос монотонно, а VACUUM INTO в ночном бэкапе
		// становился медленнее с каждым днём, и это при единственном соединении
		// с базой означало всё более долгую паузу всех инструментов.
		//
		// Запросы не замедлялись - idx_cache_expires фильтр по сроку
		// обслуживает, - поэтому дефект не был виден в работе, только в размере
		// файла и времени бэкапа.
		every(ctx, 15*time.Minute, 6*time.Hour, func(ctx context.Context) {
			if n, err := st.CachePurgeExpired(ctx); err != nil {
				log.Warnf("фон: чистка кэша: %v", err)
			} else if n > 0 {
				log.Infof("фон: кэш: вычищено %d истёкших записей", n)
			}
		})
	}
}

// every выполняет fn сразу через firstDelay, затем по interval до отмены ctx.
// Нижние границы интервалов выставляет вызывающий: тикер без клампа при
// кривом конфиге превращает сервер в DDoS-машину по своим же движкам.
func every(ctx context.Context, firstDelay, interval time.Duration, fn func(context.Context)) {
	if interval <= 0 {
		return
	}
	if firstDelay < 0 {
		firstDelay = 0
	}
	go func() {
		timer := time.NewTimer(firstDelay)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				// Повторная проверка: отмена между срабатыванием таймера
				// и вызовом не должна запускать лишний тик.
				if ctx.Err() != nil {
					return
				}
				safeTick(ctx, fn)
				timer.Reset(interval)
			}
		}
	}()
}

// safeTick выполняет один тик, переживая панику внутри него.
//
// Без recover любая паника в huntTick, discoverTick, promoteTick, backupTick
// или peerSyncTick уносила весь процесс: фоновые тики живут в отдельных
// гортутинах, и паника в горутине без recover - это смерть программы, а не
// только цикла. Вместе с ней падали все активные MCP-сессии.
//
// Сам планировщик при этом написан верно и его трогать нельзя: interval <= 0
// отсекается до NewTicker (иначе паника на нулевом тикере), Reset вызывается
// после возврата fn (поэтому тики не могут перекрываться), ctx проверяется
// заново после пробуждения, timer останавливается через defer. Меняется
// только то, что происходит внутри одного тика.
//
// Отдельно важно: цикл обязан пережить панику и продолжить работу. Тик,
// который упал один раз из-за переходного состояния базы, не должен
// отключать фоновую охоту до перезапуска сервера.
func safeTick(ctx context.Context, fn func(context.Context)) {
	defer func() {
		if r := recover(); r != nil {
			metrics.Default.Inc("bg_panic")
			fmt.Fprintf(os.Stderr, "WARNING: фон: паника в тике: %v\n%s\n", r, debug.Stack())
		}
	}()
	fn(ctx)
}

// huntTick - один фоновый прогон охот. Находки уходят в notify (push
// клиентам); пустой прогон молчит, ошибка - в лог, цикл не падает.
func huntTick(ctx context.Context, log netx.Logger, hunter *hunt.Runner, notify func([]hunt.Hit)) {
	if hunter == nil {
		return
	}
	rep, err := hunter.RunDue(ctx)
	if err != nil {
		log.Warnf("фон: охота: %v", err)
		return
	}
	// Этап 178 (смоук-I): счётчик фоновых прогонов. Описание metrics обещает
	// «фоновые прогоны», а до правки фон не писал в счётчики ничего: ответ был
	// пуст даже после часа живых тиков. Анонс + счётчик закрывают расхождение.
	metrics.Default.Inc("bg_hunt_runs")
	metrics.Default.Add("bg_hunt_hits", int64(len(rep.Hits)))
	// Отказы поиска печатаются даже когда находок нет. Молчание здесь означало
	// бы, что охоты перестали работать, а оператор видит пустой лог и считает
	// фон здоровым. Warnf, а не Infof: Infof по умолчанию выключен, и ровно в
	// той ситуации, когда tor лёг, предупреждение важнее всего.
	if rep.Failed > 0 {
		log.Warnf("фон: охота: отказов поиска %d из %d (%s)",
			rep.Failed, rep.Failed+rep.Checked, rep.LastError)
	}
	if len(rep.Hits) == 0 {
		return
	}
	log.Infof("фон: охота: находок %d (проверено %d)", len(rep.Hits), rep.Checked)
	if notify != nil {
		notify(rep.Hits)
	}
}

// discoverTick - один фоновый прогон разведки: сбор адресов из каталогов с
// обходом, затем пробы живости в пределах бюджета. Лимиты из конфига:
// потолок хостов и число проб, чтобы цикл не жег tor-канал без края.
func discoverTick(ctx context.Context, log netx.Logger, cfg config.Config, pool *discover.Pool, prober *discover.Prober) {
	if pool == nil {
		return
	}
	res, err := pool.Run(ctx, discover.Options{
		Depth:     cfg.DiscoverDepth,
		MaxHosts:  cfg.DiscoverMaxHosts,
		WithCrawl: true,
	})
	if err != nil {
		log.Warnf("фон: разведка: %v", err)
		return
	}
	// Этап 178 (смоук-I): фоновые прогоны в metrics - см. комментарий в
	// huntTick. Число новых адресов - Add, а не Inc: одна строка находит
	// тысячи адресов, и счётчик «прогонов» от этого не отличал бы пул из
	// тысячи записей от пула из одной.
	metrics.Default.Inc("bg_discover_runs")
	metrics.Default.Add("bg_discover_new", int64(res.New))
	log.Infof("фон: разведка: адресов %d (новых %d)", res.Found, res.New)
	if prober == nil || cfg.DiscoverBGProbe <= 0 {
		return
	}
	prep, err := prober.ProbeWave(ctx, cfg.DiscoverBGProbe)
	if err != nil {
		log.Warnf("фон: пробы: %v", err)
		return
	}
	metrics.Default.Inc("bg_probe_runs")
	log.Infof("фон: пробы: живых %d из %d", prep.Live, prep.Total)
}

// promoteTick ищет поисковики среди живых сервисов и поднимает их в каталог.
// Бюджет - limit проверок за прогон: каждая проверка это два tor-захода.
func promoteTick(ctx context.Context, log netx.Logger, st *store.Store, engine *search.Engine, promoter *promote.Promoter, limit int) int {
	// engine.Health здесь обязателен наравне с остальными: дальше он уходит в
	// promote.Attach, которая регистрирует поднятые движки в таблице здоровья.
	// Все прочие зависимости функции защищены, а Health - нет, и это была
	// единственная асимметрия в иначе полностью защитной проверке.
	if st == nil || engine == nil || engine.Onion == nil || engine.Health == nil || promoter == nil || limit <= 0 {
		return 0
	}
	known := map[string]bool{}
	// Snapshot: тик работает в фоновой горутине, а инструмент promote_engines
	// может добавлять движки в каталог одновременно с ним.
	for _, e := range engine.Onion.Snapshot() {
		known[strings.ToLower(e.Base)] = true
	}
	// Множитель 3 даёт запас кандидатов на те адреса, что уже в каталоге или
	// не пройдут проверку. Переполнение при огромном limit невозможно: значение
	// нормализует normLimit внутри ListOnions, поэтому отрицательный результат
	// превратится в дефолт, а не в запрос на гигабайты.
	onions, err := st.ListOnions(ctx, "live", limit*3)
	if err != nil {
		log.Warnf("фон: промоут: пул: %v", err)
		return 0
	}
	promoted := 0
	// Потолок времени в фоне важнее, чем в CLI: тик выполняется внутри общего
	// планировщика, и зависший промоут задержал бы охоты и бэкапы.
	pr := promote.NewProgress(promoter.Budget)
	for _, o := range onions {
		if promoted >= limit {
			break
		}
		if pr.Exceeded() {
			log.Infof("фон: промоут: %s", pr.StoppedReason)
			break
		}
		if known[strings.ToLower(o.URL)] {
			continue
		}
		pr.Checked++
		cctx := ctx
		if left := pr.Remaining(); left > 0 {
			var cancel context.CancelFunc
			cctx, cancel = context.WithTimeout(ctx, left)
			defer cancel()
		}
		cand, _, err := promoter.Check(cctx, o.URL)
		if err != nil {
			pr.Fail(err)
			continue
		}
		if known[strings.ToLower(cand.Base)] {
			continue
		}
		if err := st.UpsertEngineSeed(ctx, store.EngineSeed{
			Name: cand.Name, Base: cand.Base, Path: cand.Path,
			Selector: cand.Selector, Category: "general", Auto: true,
		}); err != nil {
			log.Warnf("фон: промоут: сид %s не сохранён: %v", cand.Name, err)
			pr.FailSave(err)
			continue
		}
		if promote.Attach(engine.Onion, engine.Health, promoter.Client, cand) {
			promoted++
			// Этап 178 (смоук G-Q7, I-Q16): каждый подняты движок назван по
			// имени сразу в момент подъёма. Итоговой строки «поднято N»
			// слепому прогону не хватало, чтобы связать рост onion_engines
			// с фоновым промоутом: движки «появлялись молча».
			log.Infof("фон: промоут: поднят движок %s (%s)", cand.Name, cand.Base)
		}
		_ = st.TouchEngineSeed(ctx, cand.Name)
		known[strings.ToLower(cand.Base)] = true
	}
	if note := pr.Note(); note != "" {
		// В фоне отказ потерять легче всего: тик не печатает итогов, и без этой
		// строки регулярные падения проверок выглядели бы как «движки просто не
		// находятся», хотя причина в мёртвом торе или в недоступном пуле.
		log.Warnf("фон: промоут: %s", note)
	}
	if promoted > 0 {
		log.Infof("фон: промоут: поднято движков %d", promoted)
	}
	metrics.Default.Inc("bg_promote_runs")
	metrics.Default.Add("bg_promote_engines", int64(promoted))
	return promoted
}

// backupTick делает ротируемый снимок базы. Ошибка - в лог, цикл не падает:
// бэкап обязан не мешать основному серверу никогда.
func backupTick(ctx context.Context, log netx.Logger, st *store.Store, dir string, keep int) {
	if st == nil {
		return
	}
	bctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	dest, err := st.BackupRotate(bctx, dir, keep)
	if err != nil {
		log.Warnf("фон: бэкап: %v", err)
		return
	}
	metrics.Default.Inc("bg_backup_runs")
	log.Infof("фон: бэкап: %s", dest)
}

// peerSyncTick тянет выгрузки пиров и мержит в свою базу. Мёртвый пир -
// запись в лог, остальные синкаются: один недоступный не стопает цикл.
func peerSyncTick(ctx context.Context, log netx.Logger, st *store.Store, peers []string, token string) {
	if st == nil {
		return
	}
	for _, p := range peers {
		base := strings.TrimRight(strings.TrimSpace(p), "/")
		if base == "" {
			continue
		}
		pctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		onions, hunts, err := mcpserver.SyncPeer(pctx, st, base, token, 500)
		cancel()
		if err != nil {
			log.Warnf("фон: пир %s: %v", base, err)
			continue
		}
		metrics.Default.Inc("bg_peer_runs")
		if onions+hunts > 0 {
			log.Infof("фон: пир %s: адресов %d, охот %d", base, onions, hunts)
		}
	}
}
