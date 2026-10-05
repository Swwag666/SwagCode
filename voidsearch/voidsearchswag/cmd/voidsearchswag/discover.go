package main

import (
	"context"
	"flag"
	"fmt"
	"time"

	"voidsearchswag/internal/config"
	"voidsearchswag/internal/discover"
	"voidsearchswag/internal/httpc"
	"voidsearchswag/internal/netx"
	"voidsearchswag/internal/search"
	"voidsearchswag/internal/store"
)

// buildDiscover собирает discovery-слой. Clearnet-каталоги тянутся прямым
// транспортом, а не через tor: exit-ноды Cloudflare блокирует и отдаёт
// challenge вместо страницы — через tor hidden-wiki возвращал ноль адресов,
// напрямую те же 79. Обход .onion, наоборот, идёт через tor-клиент ядра.
func buildDiscover(cfg config.Config, log netx.Logger, st *store.Store, e *search.Engine) *discover.Pool {
	if e == nil || e.Client == nil {
		return nil
	}

	direct, err := httpc.NewClient(context.Background(), httpc.Options{
		Transport: "direct",
		Timeout:   cfg.RequestTimeout,
		Logger:    log,
	})
	if err != nil {
		log.Warnf("discover: прямой клиент не собрался (%v), беру тор-транспорт", err)
		direct = e.Client
	}

	finder := discover.NewFinder(direct, log)
	crawler := discover.NewCrawler(e.Client, log, discover.CrawlConfig{
		Depth:        cfg.DiscoverDepth,
		MaxHosts:     cfg.DiscoverMaxHosts,
		PerHostDelay: cfg.DiscoverPerHostDelay,
		PageTimeout:  cfg.RequestTimeout,
		Concurrency:  cfg.DiscoverConcurrency,
	})
	return &discover.Pool{
		Store:  st,
		Finder: finder,
		Crawl:  crawler,
		Log:    log,
	}
}

// newDiscoverFlags регистрирует флаги discover на переданном наборе.
//
// seeds объявлен через listFlag, а не через fs.String: повтор флага обязан
// накапливать значения, как это делают -ext, -verdict и --fields. Замер до правки
// на HEAD 2ebd63f: «--seeds http://abcdefghijklmnop.onion/list --seeds
// http://qrstuvwxyz012345.onion/list» давал flag.String значение
// "http://qrstuvwxyz012345.onion/list", первый сид исчезал без единого слова
// предупреждения, а живой прогон discover --crawl с теми же двумя флагами
// завершился с rc=0 и пустым stderr.
func newDiscoverFlags(fs *flag.FlagSet) *discoverFlags {
	o := &discoverFlags{}
	fs.IntVar(&o.depth, "depth", 0, "глубина обхода .onion от 1 до 8 (0 - значение из конфига)")
	fs.IntVar(&o.maxHosts, "max-hosts", 0, "потолок обходимых хостов от 1 до 5000 (0 - значение из конфига)")
	fs.DurationVar(&o.delay, "delay", 0, "пауза между запросами к одному хосту, не больше 10m (0 - значение из конфига)")
	fs.BoolVar(&o.crawl, "crawl", false, "обходить найденные сервисы")
	fs.Var(&o.seeds, "seeds", "стартовые хосты обхода через запятую, применяются вместе с --crawl")
	fs.BoolVar(&o.jsonOut, "json", false, "вывод в JSON")
	fs.DurationVar(&o.timeout, "timeout", 10*time.Minute, "общий таймаут")
	return o
}

func cmdDiscover(args []string) {
	fs := flag.NewFlagSet("discover", flag.ExitOnError)
	o := newDiscoverFlags(fs)
	parseFlags(fs, args)
	// Глубина проверяется до конфига и до базы. Ноль законен и означает «возьми
	// значение из конфига», поэтому правило не положительность, а диапазон:
	// отрицательное значение молча подменялось конфигом, а значение выше восьми
	// обходило config.Validate и доходило до обхода как есть.
	depthN := validRange("depth", o.depth, 0, maxDiscoverDepth)
	// Потолок хостов проверяется рядом и по тому же правилу: ноль законен, всё
	// прочее обязано лечь в границы конфига. Без проверки минус один и ноль молча
	// заменялись значением конфига, а сто тысяч проходили в обход как есть.
	hostsN := validRange("max-hosts", o.maxHosts, 0, maxDiscoverHosts)
	// Пауза проверяется здесь же. Ноль законен и означает «значение из конфига»,
	// а отрицательная пауза молча превращалась в две секунды: подмену делают и
	// discover.NewRateLimiter, и CrawlConfig.withDefaults.
	delayD := validDurationRange("delay", o.delay, 0, maxFlagDelay)

	// Предупреждения о сидах идут до конфига, базы и транспорта: они обязаны
	// дойти до человека даже тогда, когда прогон позже упадёт. Первое отвечает на
	// вопрос про режим, второе - про значение флага.
	log := stderrLogger{}
	warnUnusedSeeds(log, o.seeds.value(), o.crawl)
	warnInvalidSeeds(log, o.seedList())

	cfg, err := config.Load()
	if err != nil {
		fatalf("конфиг: %v", err)
	}
	cfg.DiscoverDepth = discoverDepth(depthN, cfg.DiscoverDepth)
	cfg.DiscoverMaxHosts = discoverMaxHosts(hostsN, cfg.DiscoverMaxHosts)
	if delayD > 0 {
		cfg.DiscoverPerHostDelay = delayD
	}

	st, err := store.Open(cfg.DBPath())
	if err != nil {
		fatalf("база: %v", err)
	}
	defer AtExit("база", st.Close)()
	if err := st.Migrate(context.Background()); err != nil {
		fatalf("миграции: %v", err)
	}

	engine, cleanup := buildEngine(cfg, log, st)
	defer AtExit("поисковое ядро", AsError(cleanup))()

	pool := buildDiscover(cfg, log, st, engine)
	if pool == nil {
		fatalf("discovery-слой не собрался: нет транспорта")
	}

	opts := discover.Options{
		Depth:     cfg.DiscoverDepth,
		MaxHosts:  cfg.DiscoverMaxHosts,
		WithCrawl: o.crawl,
		Seeds:     o.seedList(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), validTimeout("timeout", o.timeout))
	defer cancel()

	res, err := pool.Run(ctx, opts)
	if err != nil {
		fatalf("разведка: %v", err)
	}

	if o.jsonOut {
		writeJSON(res)
		return
	}

	printDiscoverSummary(res)
}

// printDiscoverSummary печатает итог разведки. Вынесено из cmdDiscover, потому
// что та команда поднимает tor и конфиг, а строки об отказах записи должны
// проверяться без сети.
func printDiscoverSummary(res discover.Result) {
	fmt.Printf("источники: %d, найдено адресов: %d (новых %d, обновлено %d) за %s\n",
		len(res.Sources), res.Found, res.New, res.Updated, res.Elapsed)
	// Отказы записи печатаются сразу за итоговой строкой. Секция обхода в этом
	// же отчёте ошибки считает - «страниц N (успешно M, ошибок K)» - и называет
	// предел, а секция записи молчала: «найдено 3, новых 0» читалось как
	// «адреса уже известны», хотя на деле база не приняла ни одной записи.
	if n := res.SaveFailed + res.FileSaveFailed + res.ProbeFailed; n > 0 {
		fmt.Printf("отказов записи %d (адресов %d, файлов %d, проб %d)",
			n, res.SaveFailed, res.FileSaveFailed, res.ProbeFailed)
		if res.LastError != "" {
			fmt.Printf(": %s", clipLine(res.LastError, 160))
		}
		fmt.Println()
	}
	if res.Crawl != nil {
		fmt.Printf("обход: страниц %d (успешно %d, ошибок %d",
			res.Crawl.Pages, res.Crawl.Ok, res.Crawl.Failed)
		// Страницы, запрос которых не ушёл в сеть, считаются отдельно: «ошибок 1»
		// утверждало бы, что сервис отказал, хотя его никто не спросил.
		if res.Crawl.NoTransport > 0 {
			fmt.Printf(", без tor %d", res.Crawl.NoTransport)
		}
		// Строка обхода называет все три применённых предела, а не только глубину.
		// Замер до правки на HEAD e22e51e, копия боевой базы, тор выключен,
		// транспорт direct:
		//
		//	discover --crawl -depth 2 -max-hosts 3 --delay 1ms -timeout 8s --seeds ...
		//	    обход: страниц 1 (успешно 0, ошибок 0, без tor 1), глубина 2, за 1ms
		//	discover --crawl -timeout 8s --seeds ...
		//	    обход: страниц 1 (успешно 0, ошибок 0, без tor 1), глубина 2, за 0s
		//
		// Потолок три и потолок пятьдесят, пауза миллисекунда и пауза две секунды
		// дали неразличимую строку, хотя JSON того же прогона называл все три числа
		// после этапа 137.
		fmt.Printf("), глубина %d, потолок хостов %d, пауза %s, за %s\n",
			res.Crawl.Depth, res.Crawl.MaxHosts,
			appliedDelay(res.Crawl.PerHostDelayMS), res.Crawl.Elapsed)
		if res.Crawl.LimitHit != "" {
			fmt.Printf("       предел: %s\n", res.Crawl.LimitHit)
		}
		// Этап 172: отмена обхода - отдельное событие, не «предел». До
		// правки оно приезжало строкой «контекст отменён» в limit_hit и
		// затирало уже поставленный потолок: JSON-отчёт discover с
		// обрезанным по таймауту обходом называл ложную причину стопа.
		if res.Crawl.Cancelled {
			fmt.Printf("       обход прерван отменой контекста (таймаут вызова или сигнал)\n")
		}
		// Этап 174: срез входа назван числом. Прежде «обошли 3 из 14217» не
		// имело объяснения: сиды выбрал потолок, и 14214 кандидатов исчезли
		// молча. limit_hit при этом молчит и обязан: потолок отрезал вход,
		// а не остановил обход.
		if res.Crawl.SeedsCut > 0 {
			fmt.Printf("       сидов отрезано потолком: %d\n", res.Crawl.SeedsCut)
		}
	}
	if res.Files > 0 {
		fmt.Printf("каталог: добавлено файлов %d\n", res.Files)
	}
	if res.Probed > 0 {
		fmt.Printf("живость: обновлено по обходу %d хостов\n", res.Probed)
	}
	if res.ProbeSkipped > 0 {
		fmt.Printf("живость: пропущено %d проб, нужен tor или прокси\n", res.ProbeSkipped)
	}
	fmt.Println()
	for _, s := range res.Sources {
		state := "ок"
		if !s.OK {
			state = "ошибка"
		}
		fmt.Printf("  %-16s %-8s %4d адресов  %s\n", s.Name, state, s.Found, s.Elapsed)
		if s.Error != "" {
			fmt.Printf("                   %s\n", clipLine(s.Error, 100))
		}
	}
}
