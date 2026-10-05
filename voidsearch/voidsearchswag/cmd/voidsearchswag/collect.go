package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"voidsearchswag/internal/catalog"
	"voidsearchswag/internal/config"
	"voidsearchswag/internal/discover"
	"voidsearchswag/internal/netx"
	"voidsearchswag/internal/router"
	"voidsearchswag/internal/search"
	"voidsearchswag/internal/store"
)

// buildCollector собирает каталог файлов поверх tor-клиента ядра.
func buildCollector(cfg config.Config, log netx.Logger, st *store.Store, e *search.Engine) *catalog.Collector {
	if e == nil || e.Client == nil {
		return nil
	}
	return catalog.NewCollector(e.Client, st, discover.NewRateLimiter(cfg.DiscoverPerHostDelay), log,
		catalog.Config{
			MaxHosts:     cfg.DiscoverMaxHosts,
			PerHostDelay: cfg.DiscoverPerHostDelay,
			Concurrency:  cfg.DiscoverConcurrency,
			PageTimeout:  cfg.RequestTimeout,
		})
}

// findFileHosts ищет хосты, которые реально раздают файлы, через работающие
// onion-поисковики и возвращает их без дублей.
//
// Отбор идёт по числу упоминаний хоста в выдаче: адрес, который всплыл по
// нескольким файловым запросам, с большей вероятностью является файловым
// хостом, чем случайно попавший в один результат. Сортировка по убыванию
// счётчика оставляет в начале самых вероятных кандидатов, поэтому обрезка до
// limit отбрасывает хвост, а не случайные записи.
func findFileHosts(ctx context.Context, eng *search.Engine, log netx.Logger, limit, perQuery int) []string {
	if eng == nil || limit <= 0 {
		return nil
	}
	if perQuery <= 0 {
		perQuery = 20
	}
	searchFn := fileHostSearch(eng)
	score := map[string]int{}
	for _, q := range fileHostQueries {
		if ctx.Err() != nil {
			break
		}
		out, err := searchFn(ctx, q, string(router.ModeDeep), perQuery)
		if err != nil {
			if log != nil {
				log.Infof("file-hosts/%q: %v", q, err)
			}
			continue
		}
		if out.EnginesDown() {
			// Пустая выдача при мёртвых движках - не «кандидатов нет», а
			// «спрашивать было некого»: без этой строки прогон выглядел бы
			// штатным, а лог не объяснил бы, откуда пустой список хостов.
			if log != nil {
				log.Infof("file-hosts/%q: %s", q, out.Reason())
			}
			continue
		}
		for _, u := range out.URLs {
			h := discover.HostOf(u)
			if h == "" {
				continue
			}
			score[h]++
		}
	}
	if len(score) == 0 {
		return nil
	}
	out := rankHosts(score, limit)
	if log != nil {
		log.Infof("file-hosts: кандидатов %d из %d запросов", len(out), len(fileHostQueries))
	}
	return out
}

// rankHosts упорядочивает хосты по числу упоминаний в выдаче и обрезает до
// limit. Вынесено из findFileHosts, чтобы правило отбора проверялось без сети
// и без tor: именно здесь решается, какие хосты пойдут в обход первыми.
//
// При равенстве счёта порядок детерминирован по имени хоста. Без этого два
// одинаковых прогона давали бы разный набор кандидатов - обход карты в Go
// намеренно рандомизирован, - и каталог наполнялся бы непредсказуемо, а
// воспроизвести найденное было бы нельзя.
func rankHosts(score map[string]int, limit int) []string {
	if len(score) == 0 || limit <= 0 {
		return nil
	}
	type kv struct {
		host string
		n    int
	}
	ranked := make([]kv, 0, len(score))
	for h, n := range score {
		if strings.TrimSpace(h) == "" || n <= 0 {
			continue
		}
		ranked = append(ranked, kv{h, n})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].n != ranked[j].n {
			return ranked[i].n > ranked[j].n
		}
		return ranked[i].host < ranked[j].host
	})
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}
	out := make([]string, 0, len(ranked))
	for _, e := range ranked {
		out = append(out, e.host)
	}
	return out
}

func cmdCollect(args []string) {
	fs := flag.NewFlagSet("collect", flag.ExitOnError)
	limit := fs.Int("limit", 20, "сколько живых хостов обойти")
	maxFiles := fs.Int("max-files", 500, "потолок файлов за прогон")
	delay := fs.Duration("delay", 0, "пауза между запросами к хосту, не больше 10m (0 - значение из конфига)")
	concurrency := fs.Int("concurrency", 0, "одновременных обходов, не больше 128 (0 - значение из конфига)")
	fileHosts := fs.Bool("file-hosts", false, "отобрать хосты с файлами через onion-поисковики вместо первых живых из пула")
	jsonOut := fs.Bool("json", false, "вывод в JSON")
	parseFlags(fs, args)
	// Потолок берётся из хранилища: discover.Known идёт в ListOnions, а тот
	// обрезает выборку по MaxStoreLimit и подставляет сотню вместо незаданного
	// значения. Без проверки здесь минус пять хостов превращались в сто обходов
	// сети и сто записей failed в пуле, и отчёт об этом не говорил.
	limitN := validLimitCeiling("limit", *limit, store.MaxStoreLimit)
	// Потолок файлов и параллельность проверяются здесь же: оба уходили в
	// catalog.Config.withDefaults, которая молча подменяла нерабочие значения
	// дефолтами каталога. Замер на HEAD b718028, копия боевой базы, tor выключен,
	// collect --limit 2 --delay 1ms --json: --max-files -1 и --max-files 0 дали
	// rc=0 и отчёт без единого поля о применённом потолке, --concurrency 0,
	// --concurrency -5 и --concurrency 100000 дали то же. Программный зонд
	// withDefaults показал подмену: MaxFiles -1 и 0 превращались в 500,
	// Concurrency -5 и 0 в 4, а 100000 проходило без потолка.
	maxFilesN := validLimitCeiling("max-files", *maxFiles, maxFlagLimit)
	concurrencyN := 0
	if *concurrency != 0 {
		concurrencyN = validLimitCeiling("concurrency", *concurrency, maxCollectConcurrency)
	}
	// Пауза проверяется здесь же. Ноль законен, отрицательное значение молча
	// превращалось в две секунды сразу в двух местах: discover.NewRateLimiter и
	// catalog.Config.withDefaults подменяют неположительную паузу, и отчёт об
	// этом не говорил. Замер на HEAD 71d085a, копия боевой базы, тор выключен,
	// collect --limit 2 --json: --delay -1s и --delay -5m дали rc=0, hosts=2,
	// failed=2 и пустой stderr, --delay 0s дал то же.
	delayD := validDurationRange("delay", *delay, 0, maxFlagDelay)

	cfg, err := config.Load()
	if err != nil {
		fatalf("конфиг: %v", err)
	}
	// Ноль у паузы и у параллельности означает «значение из конфига», а не «без
	// паузы» и не «один поток». Дефолты флагов две секунды и четыре совпадали с
	// envDefault полей DiscoverPerHostDelay и DiscoverConcurrency, поэтому
	// переменные окружения не действовали вовсе. Замер до правки на HEAD bb08193,
	// копия боевой базы, тор выключен, транспорт direct:
	//
	//	VOIDSEARCH_DISCOVER_CONCURRENCY=9 VOIDSEARCH_DISCOVER_DELAY=7s
	//	    collect --limit 1 --json
	//	    rc=0, concurrency=4, per_host_delay_ms=2000, max_files=500
	//	collect --limit 1 --json --concurrency 0 --delay 0s
	//	    rc=1, ERROR: --concurrency должен быть положительным, получено 0
	//	collect -h
	//	    -concurrency int
	//	        одновременных обходов (default 4)
	//	    -delay duration
	//	        пауза между запросами к хосту, не больше 10m (default 2s)
	//
	// Правило выбора то же, что у probe для паузы и у discover для глубины и
	// потолка хостов: значение флага, а при нуле значение конфига.
	concurrencyN = flagOrConfig(concurrencyN, cfg.DiscoverConcurrency)
	delayD = probeDelay(delayD, cfg.DiscoverPerHostDelay)

	log := stderrLogger{}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		fatalf("база: %v", err)
	}
	defer AtExit("база", st.Close)()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	if err := st.Migrate(ctx); err != nil {
		fatalf("миграции: %v", err)
	}

	eng, cleanup := buildEngine(cfg, log, st)
	if cleanup != nil {
		defer AtExit("поисковое ядро", AsError(cleanup))()
	}

	hosts := fs.Args()
	if len(hosts) == 0 && *fileHosts {
		// Явно указанные адреса важнее отбора: если пользователь перечислил
		// хосты руками, искать их через поисковик бессмысленно.
		hosts = findFileHosts(ctx, eng, log, limitN, 20)
		if len(hosts) == 0 {
			finishEmptyCollect(*jsonOut, "файловые хосты не найдены: поисковики не дали результатов")
			return
		}
	}
	if len(hosts) == 0 {
		hosts, err = discover.Known(ctx, st, "live", limitN)
		if err != nil {
			fatalf("пул: %v", err)
		}
		if len(hosts) == 0 {
			hosts, err = discover.Known(ctx, st, "unknown", limitN)
			if err != nil {
				fatalf("пул: %v", err)
			}
		}
	}
	if len(hosts) == 0 {
		finishEmptyCollect(*jsonOut, "пул пуст: сначала соберите адреса через discover")
		return
	}

	// Миллисекунды - этап 168: два collect в одну секунду (пустой пул,
	// мгновенный отказ) получали один task_id, и метки задач в каталоге
	// смешивались. Формат остаётся сортируемым: время растёт лексикографически.
	taskID := "collect-" + time.Now().Format("20060102-150405.000")
	col := catalog.NewCollector(eng.Client, st, discover.NewRateLimiter(delayD), log,
		catalog.Config{
			MaxHosts:     len(hosts),
			MaxFiles:     maxFilesN,
			PerHostDelay: delayD,
			Concurrency:  concurrencyN,
			PageTimeout:  cfg.RequestTimeout,
		})

	rep, err := col.Collect(ctx, hosts, taskID)
	if err != nil {
		fatalf("сбор: %v", err)
	}

	if *jsonOut {
		writeJSON(rep)
		return
	}

	printCollectReport(ctx, st, rep, taskID)
}

// collectEmptyReport собирает ответ collect для прогона, который не состоялся:
// обходить нечего. Схема та же, что у настоящего отчёта, поэтому потребитель
// разбирает один формат и видит нули вместо чисел, а причина лежит в note.
func collectEmptyReport(note string) catalog.Report {
	return catalog.Report{Elapsed: "0s", Note: note}
}

// finishEmptyCollect печатает ответ прогона без работы: в машинном режиме JSON, в
// текстовом - прежнюю подсказку. До правки обе ветки cmdCollect печатали русский
// текст в stdout независимо от --json: замер на пустой базе давал rc=0 и 102 байта
// «пул пуст: сначала соберите адреса через discover» там, где скрипт ждал JSON, а
// ветка --file-hosts после 109 секунд поиска выдавала 120 байт такого же текста.
func finishEmptyCollect(jsonOut bool, note string) {
	if jsonOut {
		writeJSON(collectEmptyReport(note))
		return
	}
	fmt.Println(note)
}

// appliedDelay печатает применённую паузу в привычном формате: миллисекунда
// становится «1ms», две секунды - «2s». В отчёте пауза лежит числом миллисекунд,
// потому что так её можно сравнивать и строить по ней графики, а человеку нужен
// текст.
func appliedDelay(ms int64) string {
	return (time.Duration(ms) * time.Millisecond).String()
}

// printCollectReport печатает человекочитаемый отчёт о сборе и возвращает список
// того, что прочитать не удалось.
//
// Печать вынесена из cmdCollect, чтобы её можно было проверить тестом: сама
// команда строит поисковое ядро и ходит по сети, а состав отчёта - ровно то, что
// нужно защищать от регресса.
//
// Отказавшие хосты и незаписанные файлы печатаются обязательно, когда их число
// больше нуля. Измерено на двух недоступных адресах: JSON того же прогона
// содержал "failed": 2, а текст показывал только «сбор: хостов 2, страниц 0,
// ссылок 0, файлов 0 за 5ms». Полный отказ обоих хостов выглядел как обход живых
// источников, на которых просто нет файлов.
func printCollectReport(ctx context.Context, st *store.Store, rep catalog.Report, taskID string) []string {
	var problems []string

	fmt.Printf("сбор: хостов %d, страниц %d, ссылок %d, файлов %d за %s\n",
		rep.Hosts, rep.Pages, rep.Links, rep.Saved, rep.Elapsed)
	fmt.Printf("пределы: файлов %d, потоков %d, пауза %s\n",
		rep.MaxFiles, rep.Concurrency, appliedDelay(rep.PerHostDelayMS))
	// Применённые пределы печатаются всегда, а не только когда сработали. Без них
	// «файлов 7» не отличить от «файлов 7 при потолке 500», и прогон с флагами
	// выглядит в точности как прогон без них. Замер до правки на HEAD 58a8751,
	// копия боевой базы, тор выключен, транспорт direct:
	//
	//	collect --limit 2 --max-files 7 --concurrency 3 --delay 1ms
	//	    сбор: хостов 2, страниц 0, ссылок 0, файлов 0 за 0s
	//	    отказавших хостов: 2 из 2
	//	    задача: collect-20260929-132128
	//	    каталог всего: 19 файлов, 27.9 MiB
	//	collect --limit 2
	//	    сбор: хостов 2, страниц 0, ссылок 0, файлов 0 за 1ms
	//	    отказавших хостов: 2 из 2
	//	    задача: collect-20260929-132129
	//	    каталог всего: 19 файлов, 27.9 MiB
	//
	// Два прогона с потолком семь и с потолком пятьсот, с паузой миллисекунда и с
	// паузой две секунды, напечатали один и тот же текст. JSON при этом уже называл
	// все три числа после этапа 137, то есть расхождение было только в текстовой
	// ветке.
	if rep.Failed > 0 {
		fmt.Printf("отказавших хостов: %d из %d\n", rep.Failed, rep.Hosts)
	}
	if rep.Skipped > 0 {
		fmt.Printf("не записано в каталог: %d (причины в служебном логе через --verbose)\n", rep.Skipped)
	}
	if rep.LimitHit != "" {
		fmt.Printf("потолок: %s\n", rep.LimitHit)
	}
	if len(rep.ByExt) > 0 {
		exts := make([]string, 0, len(rep.ByExt))
		for e, n := range rep.ByExt {
			exts = append(exts, fmt.Sprintf("%s=%d", e, n))
		}
		sort.Strings(exts)
		fmt.Printf("по расширениям: %s\n", strings.Join(exts, " "))
	}
	// Примеры адресов печатаются так же, как отдаются в JSON: без них «файлов 3»
	// не даёт понять, что именно собралось, и проверять приходится запросом к базе.
	if len(rep.SampleURLs) > 0 {
		n := len(rep.SampleURLs)
		if n > 3 {
			n = 3
		}
		fmt.Printf("примеры: %s\n", strings.Join(rep.SampleURLs[:n], " "))
	}
	fmt.Printf("задача: %s\n", taskID)

	// Итог каталога снимается точным счётчиком. Если прочитать его не удалось,
	// строка исчезала молча и отчёт обрывался без объяснения; теперь причина
	// называется в stderr. Код возврата остаётся нулевым: сам сбор прошёл
	// успешно, отказ касается только итоговой строки, и валить из-за неё весь
	// прогон значило бы сообщить о несуществующей проблеме со сбором.
	total, bytes, _, err := st.FileStats(ctx)
	if err != nil {
		problems = append(problems, fmt.Sprintf("итог каталога: %v", err))
		fmt.Fprintf(os.Stderr, "ERROR: итог каталога: %v\n", err)
		return problems
	}
	fmt.Printf("каталог всего: %d файлов, %s\n", total, humanBytes(bytes))
	return problems
}
