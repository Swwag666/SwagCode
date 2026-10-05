package main

import (
	"context"
	"flag"
	"fmt"
	"strings"
	"time"

	"voidsearchswag/internal/config"
	"voidsearchswag/internal/hunt"
	"voidsearchswag/internal/netx"
	"voidsearchswag/internal/router"
	"voidsearchswag/internal/search"
	"voidsearchswag/internal/store"
)

// huntSearchFresh - адаптер для мониторинга: охота обязана видеть живую выдачу.
//
// Кэш поиска живёт час (VOIDSEARCH_CACHE_TTL, по умолчанию 1h), и до этой правки
// hunt run, hunt watch и фоновый тик читали его на общих основаниях. В пределах
// часа охота получала тот же самый список URL, hash не менялся, и watch
// рапортовал «выдача стабильна, изменений за время ожидания не было», не сделав
// ни одного запроса: живой замер показывал 4 опроса за 25 секунд при пустом
// stderr и нетронутой записи кэша.
//
// Обратная сторона кэша хуже молчания. Запись в кэше - чужой снимок выдачи, и
// охота сравнивает его со своим последним hash как свежий факт. Живой замер с
// подменённым payload дал за 79 мс вместо 14942 мс находку changed=true с
// адресом, которого в выдаче никогда не было, и эта ложная находка уехала в
// историю hunt_hits.
//
// NoCache отключает и чтение, и запись: прогон охоты не должен ни читать чужой
// снимок, ни подменять им кэш пользовательского search.
func huntSearchFresh(eng *search.Engine) hunt.SearchFunc {
	return huntSearchWith(eng, true)
}

// newHuntRunner собирает Runner охоты. Фон, CLI и MCP обязаны искать одинаково:
// при трёх независимых сборках расхождение «в фоне свежая выдача, в CLI кэш»
// находилось бы только чтением кода.
func newHuntRunner(st *store.Store, eng *search.Engine) *hunt.Runner {
	return &hunt.Runner{Store: st, Search: huntSearchFresh(eng)}
}

// huntSearchWith - общий адаптер: охоте нужны URL выдачи для hash и отчёт
// движков, потому что по одному списку URL отказ всех движков неотличим от
// честной пустой выдачи. Без отчёта охота записывала sha256 пустой строки
// базовым hash, а следующий живой прогон приносил ложную находку.
func huntSearchWith(eng *search.Engine, noCache bool) hunt.SearchFunc {
	return func(ctx context.Context, query, mode string, limit int) (hunt.SearchOutcome, error) {
		m, ok := router.Parse(mode)
		if !ok {
			// Подмена на auto молча превращала охоту «в даркнете» в обычный
			// clearnet-поиск: находки не те, отчёт здоровый, причина не видна.
			// Ошибка уходит в счётчик Failed и доходит до оператора с текстом -
			// в CLI строкой «отказов поиска», в фоне предупреждением huntTick,
			// в MCP полями failed и last_error.
			//
			// Сюда попадают охоты, заведённые до введения проверки режима:
			// Create больше не пропускает мусор, но в существующей базе такое
			// значение ещё может лежать.
			return hunt.SearchOutcome{}, fmt.Errorf("недопустимый режим охоты %q: auto|fast|stealth|deep", mode)
		}
		out, err := eng.Search(ctx, search.Options{Query: query, Mode: m, Limit: limit, NoCache: noCache})
		if err != nil {
			return hunt.SearchOutcome{}, err
		}
		urls := make([]string, 0, len(out.Results))
		for _, r := range out.Results {
			if r.URL != "" {
				urls = append(urls, r.URL)
			}
		}
		// Live и Total берутся из отчёта ядра: Live считает ответившие движки,
		// Total - все опрошенные, поэтому Total - Live даёт число отказов.
		return hunt.SearchOutcome{
			URLs:          urls,
			EnginesFailed: out.Report.Total - out.Report.Live,
			EnginesTotal:  out.Report.Total,
			Degraded:      out.Degraded || out.Report.Degraded,
			Note:          out.Report.Note,
		}, nil
	}
}

func openStoreAndEngine(cfg config.Config, log netx.Logger) (*store.Store, *search.Engine, func()) {
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		fatalf("база: %v", err)
	}
	if err := st.Migrate(context.Background()); err != nil {
		fatalf("миграции: %v", err)
	}
	engine, cleanup := buildEngine(cfg, log, st)
	return st, engine, func() {
		cleanup()
		st.Close()
	}
}

func cmdHunt(args []string) {
	if len(args) == 0 {
		fatalf("нужна подкоманда: voidsearchswag hunt create|list|run|watch|hits ...")
	}
	switch args[0] {
	case "create":
		fs := flag.NewFlagSet("hunt create", flag.ExitOnError)
		mode := fs.String("mode", "auto", "auto|fast|stealth|deep")
		sched := fs.Int("schedule", 360, "период в минутах")
		jsonOut := fs.Bool("json", false, "вывод в JSON")
		parseFlagsRaw(fs, args[1:])
		if fs.NArg() == 0 {
			fatalf("нужен запрос: voidsearchswag hunt create --mode deep \"leak database\"")
		}
		cfg, err := config.Load()
		if err != nil {
			fatalf("конфиг: %v", err)
		}
		st, err := store.Open(cfg.DBPath())
		if err != nil {
			fatalf("база: %v", err)
		}
		defer AtExit("база", st.Close)()
		if err := st.Migrate(context.Background()); err != nil {
			fatalf("миграции: %v", err)
		}
		r := &hunt.Runner{Store: st}
		id, err := r.Create(context.Background(), strings.Join(fs.Args(), " "), *mode, *sched)
		if err != nil {
			fatalf("охота: %v", err)
		}
		if *jsonOut {
			// До правки --json здесь не объявлялся, и FlagSet с ExitOnError
			// завершал процесс кодом 2: команда, которую попросили отдать
			// машинный ответ, не выполнялась вообще (замер: rc=2, stdout 0 байт).
			writeJSON(map[string]any{
				"id":           id,
				"query":        strings.Join(fs.Args(), " "),
				"mode":         *mode,
				"schedule_min": *sched,
			})
			return
		}
		fmt.Printf("охота %d заведена\n", id)
	case "list":
		fs := flag.NewFlagSet("hunt list", flag.ExitOnError)
		jsonOut := fs.Bool("json", false, "вывод в JSON")
		parseFlags(fs, args[1:])
		cfg, err := config.Load()
		if err != nil {
			fatalf("конфиг: %v", err)
		}
		st, err := store.Open(cfg.DBPath())
		if err != nil {
			fatalf("база: %v", err)
		}
		defer AtExit("база", st.Close)()
		if err := st.Migrate(context.Background()); err != nil {
			fatalf("миграции: %v", err)
		}
		r := &hunt.Runner{Store: st}
		hunts, err := r.List(context.Background())
		if err != nil {
			fatalf("охоты: %v", err)
		}
		if *jsonOut {
			// Список отдаётся и пустым: машина отличает «охот нет» (count 0) от
			// «список не читается» (отказ с кодом 1), тогда как подсказка ниже
			// адресована человеку. До правки ветки list вообще не разбирала
			// аргументы, и --json молча игнорировался: rc=0 и 148 байт текста.
			rows := make([]map[string]any, 0, len(hunts))
			for _, h := range hunts {
				row := map[string]any{
					"id":           h.ID,
					"query":        h.Query,
					"mode":         h.Mode,
					"schedule_min": h.ScheduleMin,
					"last_hash":    h.LastHash,
					"created_at":   h.CreatedAt,
					// Охота, которая ещё не бегала, последнего прогона не имеет:
					// нулевое время сериализовалось бы как 0001-01-01T00:00:00Z и
					// читалось бы как дата, а не как «не бегала».
					"last_run": nil,
				}
				if !h.LastRun.IsZero() {
					row["last_run"] = h.LastRun
				}
				rows = append(rows, row)
			}
			writeJSON(map[string]any{"count": len(rows), "hunts": rows})
			return
		}
		if len(hunts) == 0 {
			fmt.Println("охот нет: заведи через hunt create")
			return
		}
		for _, h := range hunts {
			last := "не бегала"
			if !h.LastRun.IsZero() {
				last = h.LastRun.Format("2006-01-02 15:04")
			}
			fmt.Printf("  %d  [%s/%s] каждые %dм  %s  %s\n", h.ID, h.Query, h.Mode, h.ScheduleMin, last, clipLine(h.LastHash, 12))
		}
	case "run":
		fs := flag.NewFlagSet("hunt run", flag.ExitOnError)
		id := fs.Int64("id", 0, "id охоты, пусто - все с вышедшим расписанием")
		noTor := fs.Bool("no-tor", false, "не поднимать tor")
		jsonOut := fs.Bool("json", false, "вывод в JSON")
		parseFlagsRaw(fs, args[1:])
		cfg, err := config.Load()
		if err != nil {
			fatalf("конфиг: %v", err)
		}
		if *noTor {
			cfg.Tor = "off"
		}
		log := stderrLogger{}
		st, engine, cleanup := openStoreAndEngine(cfg, log)
		defer AtExit("поисковое ядро", AsError(cleanup))()
		r := newHuntRunner(st, engine)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if *id > 0 {
			hit, err := r.RunOne(ctx, *id)
			if err != nil {
				fatalf("прогон: %v", err)
			}
			if *jsonOut {
				writeJSON(hit)
				return
			}
			fmt.Printf("охота %d (%s): changed=%v, URL %d\n", hit.HuntID, hit.Query, hit.Changed, hit.Count)
			for _, u := range hit.URLs {
				fmt.Printf("  %s\n", u)
			}
			if hit.Changed {
				// Число новых url печатается отдельно от размера выдачи: hash
				// меняется и от перестановки адресов, и тогда «находка» не
				// приносит ничего нового, хотя URL в отчёте десять.
				fmt.Printf("в историю добавлено %d новых url\n", hit.Saved)
			}
			if hit.SaveError != "" {
				fmt.Printf("история не записана: %s\n", hit.SaveError)
			}
			return
		}
		rep, err := r.RunDue(ctx)
		if err != nil {
			fatalf("прогон: %v", err)
		}
		if *jsonOut {
			writeJSON(rep)
			return
		}
		fmt.Printf("проверено %d, пропущено %d, находок %d\n", rep.Checked, rep.Skipped, len(rep.Hits))
		// Отказы поиска печатаются отдельно от пропусков: «пропущено» означает
		// «расписание ещё не вышло», а «отказов» - «прогон не получился». Пока
		// счётчик не был отделён, строка «проверено 0, пропущено 3» при мёртвом
		// tor читалась как «ещё не пора».
		if rep.Failed > 0 {
			fmt.Printf("отказов поиска: %d (%s)\n", rep.Failed, rep.LastError)
		}
		for _, h := range rep.Hits {
			fmt.Printf("  охота %d (%s): %d URL, в историю добавлено %d\n", h.HuntID, h.Query, h.Count, h.Saved)
			if h.SaveError != "" {
				fmt.Printf("    история не записана: %s\n", h.SaveError)
			}
		}
	case "watch":
		fs := flag.NewFlagSet("hunt watch", flag.ExitOnError)
		id := fs.Int64("id", 0, "id охоты, пусто - все")
		timeout := fs.Duration("timeout", 2*time.Minute, "ждать не дольше")
		interval := fs.Duration("interval", 30*time.Second, "пауза между прогонами")
		noTor := fs.Bool("no-tor", false, "не поднимать tor")
		jsonOut := fs.Bool("json", false, "вывод в JSON")
		parseFlagsRaw(fs, args[1:])
		cfg, err := config.Load()
		if err != nil {
			fatalf("конфиг: %v", err)
		}
		if *noTor {
			cfg.Tor = "off"
		}
		log := stderrLogger{}
		st, engine, cleanup := openStoreAndEngine(cfg, log)
		defer AtExit("поисковое ядро", AsError(cleanup))()
		r := newHuntRunner(st, engine)
		ctx, cancel := context.WithTimeout(context.Background(), addTimeout(validTimeout("timeout", *timeout), time.Minute))
		defer cancel()
		rep, err := r.WatchDetailed(ctx, *id, *timeout, *interval)
		if err != nil {
			fatalf("ожидание: %v", err)
		}
		if *jsonOut {
			out := map[string]any{
				"hits":       rep.Hits,
				"timeout":    rep.Timeout,
				"polls":      rep.Polls,
				"last_count": rep.LastCount,
				"checked":    rep.Checked,
				"failed":     rep.Failed,
				"elapsed":    rep.Elapsed,
				"note":       rep.Note(),
			}
			if rep.LastError != "" {
				out["last_error"] = rep.LastError
			}
			writeJSON(out)
			return
		}
		if rep.Timeout {
			// Голый «таймаут» не отличал стабильную выдачу от сломанной охоты:
			// в обоих случаях список находок пуст. Пояснение и счётчики
			// показывают, что именно произошло.
			if *id > 0 {
				fmt.Printf("таймаут за %s: %s (опросов %d, последняя выдача %d)\n",
					rep.Elapsed, rep.Note(), rep.Polls, rep.LastCount)
			} else {
				// В режиме «все охоты» размер последней выдачи не измеряется:
				// охот много, и ноль здесь означал бы «не считали», а не
				// «выдача пуста». Печатаются числа, которые в этом режиме
				// действительно есть: сколько охот проверено и сколько
				// прогонов поиска отказало.
				fmt.Printf("таймаут за %s: %s (опросов %d, проверено охот %d, отказов поиска %d)\n",
					rep.Elapsed, rep.Note(), rep.Polls, rep.Checked, rep.Failed)
			}
			return
		}
		fmt.Printf("опросов %d за %s\n", rep.Polls, rep.Elapsed)
		for _, h := range rep.Hits {
			fmt.Printf("находка: охота %d (%s), URL %d, в историю добавлено %d\n",
				h.HuntID, h.Query, h.Count, h.Saved)
			for _, u := range h.URLs {
				fmt.Printf("  %s\n", u)
			}
			if h.SaveError != "" {
				fmt.Printf("  история не записана: %s\n", h.SaveError)
			}
		}
	case "hits":
		fs := flag.NewFlagSet("hunt hits", flag.ExitOnError)
		id := fs.Int64("id", 0, "id охоты, пусто - все охоты")
		limit := fs.Int("limit", 50, "сколько строк истории показать")
		clear := fs.Bool("clear", false, "стереть историю находок охоты (нужен --id)")
		jsonOut := fs.Bool("json", false, "вывод в JSON")
		parseFlags(fs, args[1:])
		// Проверка идёт до конфига и базы и берёт потолок самого хранилища:
		// значение сверх него ListHuntHits молча урезал бы, а отчёт напечатал
		// заявленное число.
		limitN := validLimitCeiling("limit", *limit, store.MaxHuntHitsLimit)
		cfg, err := config.Load()
		if err != nil {
			fatalf("конфиг: %v", err)
		}
		st, err := store.Open(cfg.DBPath())
		if err != nil {
			fatalf("база: %v", err)
		}
		defer AtExit("база", st.Close)()
		if err := st.Migrate(context.Background()); err != nil {
			fatalf("миграции: %v", err)
		}
		r := &hunt.Runner{Store: st}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if *clear {
			// Без явного id чистка стёрла бы историю всех охот разом: слишком
			// дорогой побочный эффект для команды, которую запускают по памяти.
			if *id <= 0 {
				fatalf("нужен --id: стирать историю всех охот одной командой нельзя")
			}
			n, err := r.ClearHits(ctx, *id)
			if err != nil {
				fatalf("история: %v", err)
			}
			if *jsonOut {
				writeJSON(map[string]any{"hunt_id": *id, "deleted": n})
				return
			}
			fmt.Printf("история охоты %d очищена: удалено %d строк\n", *id, n)
			return
		}
		hits, err := r.Hits(ctx, *id, limitN)
		if err != nil {
			fatalf("история: %v", err)
		}
		if *jsonOut {
			writeJSON(map[string]any{"hunt_id": *id, "count": len(hits), "limit": limitN, "hits": hits})
			return
		}
		if len(hits) == 0 {
			// Пустой ответ обязан объяснять себя: «находок нет» - это не то же
			// самое, что «история не читается» или «охоты с таким id нет».
			if *id > 0 {
				fmt.Printf("находок у охоты %d нет: выдача ещё не менялась\n", *id)
			} else {
				fmt.Println("находок нет ни у одной охоты: выдача ещё не менялась")
			}
			return
		}
		fmt.Printf("история находок: %d строк\n", len(hits))
		if *limit > 0 && len(hits) >= *limit {
			fmt.Printf("показаны %d последних, в истории может быть больше: подними --limit\n", len(hits))
		}
		for _, h := range hits {
			fmt.Printf("  охота %d [%s/%s] %s  x%d  %s\n",
				h.HuntID, h.Query, h.Mode, h.LastFound.Format("2006-01-02 15:04"), h.TimesSeen, h.URL)
		}
	default:
		fatalf("неизвестная подкоманда %q (create|list|run|watch|hits)", args[0])
	}
}
