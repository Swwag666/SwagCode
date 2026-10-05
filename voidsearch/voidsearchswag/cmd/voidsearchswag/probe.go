package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"voidsearchswag/internal/config"
	"voidsearchswag/internal/discover"
	"voidsearchswag/internal/netx"
	"voidsearchswag/internal/search"
	"voidsearchswag/internal/store"
)

// buildProber собирает пробы живости поверх tor-клиента ядра.
//
// Пауза между пробами одного хоста приходит отдельным параметром: флаг --delay
// есть у collect и discover, а probe до правки брал паузу только из конфига, и
// замедлить одну волну без правки файла конфига было нельзя. Ноль означает
// «флаг не задан», и тогда действует значение конфига.
func buildProber(cfg config.Config, log netx.Logger, st *store.Store, e *search.Engine, delay time.Duration) *discover.Prober {
	if e == nil || e.Client == nil {
		return nil
	}
	// Пробе нужны свои параллелизм и таймаут, а не обходные: см. комментарий
	// у ProbeConcurrency в config. Обход выкачивает содержимое осторожно,
	// проба лишь проверяет живость и может позволить себе больше воркеров и
	// меньший потолок на адрес.
	return discover.NewProber(e.Client, st, log, discover.ProbeConfig{
		Concurrency: cfg.ProbeConcurrency,
		Timeout:     cfg.ProbeTimeout,
		Delay:       probeDelay(delay, cfg.DiscoverPerHostDelay),
	})
}

// probeDelay выбирает паузу волны проб: значение флага, если оно положительное,
// иначе значение конфига. Правило то же, что у flagOrConfig для целых чисел, но
// для длительностей.
func probeDelay(flagValue, cfgValue time.Duration) time.Duration {
	if flagValue > 0 {
		return flagValue
	}
	return cfgValue
}

func cmdProbe(args []string) {
	fs := flag.NewFlagSet("probe", flag.ExitOnError)
	limit := fs.Int("limit", 20, "сколько адресов проверить в волне охвата")
	addr := fs.String("addr", "", "проверить один конкретный адрес")
	jsonOut := fs.Bool("json", false, "вывод в JSON")
	timeout := fs.Duration("timeout", 10*time.Minute, "общий таймаут")
	delay := fs.Duration("delay", 0, "пауза между пробами одного хоста, не больше 10m (0 - значение из конфига)")
	parseFlags(fs, args)
	// Потолок берётся из хранилища: NextProbeWave всё равно не вернёт больше
	// MaxStoreLimit строк, и значение сверх него обещало бы оператору волну
	// проб, которой не существует.
	limitN := validLimitCeiling("limit", *limit, store.MaxStoreLimit)
	// Пауза проверяется тем же правилом, что у collect и discover: без проверки
	// отрицательное значение молча превратилось бы в «паузы нет», а сутки с
	// лишним остановили бы волну до первого же хоста. Ноль остаётся законным и
	// означает «взять значение из конфига».
	delayD := validDurationRange("delay", *delay, 0, maxFlagDelay)

	cfg, err := config.Load()
	if err != nil {
		fatalf("конфиг: %v", err)
	}

	log := stderrLogger{}
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

	prober := buildProber(cfg, log, st, engine, delayD)
	if prober == nil {
		fatalf("пробы не собрались: нет транспорта")
	}

	ctx, cancel := context.WithTimeout(context.Background(), validTimeout("timeout", *timeout))
	defer cancel()

	var rep discover.ProbeReport
	if *addr != "" {
		rep = prober.Probe(ctx, []string{*addr})
	} else {
		rep, err = prober.ProbeWave(ctx, limitN)
		if err != nil {
			fatalf("пробы: %v", err)
		}
	}

	if *jsonOut {
		writeJSON(rep)
	} else {
		printProbeReport(rep)
	}
	// Отчёт печатается до выхода: код возврата добавляет сигнал для обвязки, а не
	// заменяет объяснение. RunShutdown закрывает базу и tor, потому что os.Exit не
	// выполняет defer.
	if code := probeExitCode(rep); code != 0 {
		RunShutdown()
		os.Exit(code)
	}
}

// probeExitCode решает, состоялась ли волна проб. Ноль - хотя бы один запрос ушёл
// в сеть, либо проверять было нечего. Единица - все пробы пропущены: tor не поднят
// и прокси нет, поэтому обращений к сервисам не было вовсе, и «живых 0 из 7»
// означает «состояние пула неизвестно», а не «сервисы мертвы».
//
// Отдельный случай - волна не состоялась из-за отброшенных адресов: очередь
// непроверенных полна, но ни один адрес не дошёл до сети, потому что хосты не
// являются onion-адресами. До правки такой прогон возвращал ноль и читался как
// «проверять нечего», хотя в пуле лежали записи.
//
// Отказ отдельного адреса провалом не считается: диагностика затем и запускается,
// чтобы находить мёртвые, поэтому код обязан отличать «пул плох» от «проверка не
// состоялась». Правило одно на текстовый и машинный режим: два разных условия дали
// бы расхождение, видимое только тому, кто читает оба вывода сразу.
func probeExitCode(rep discover.ProbeReport) int {
	if rep.Total == 0 {
		if rep.Rejected > 0 {
			return 1
		}
		return 0
	}
	if rep.Skipped < rep.Total {
		return 0
	}
	return 1
}

// printProbeReport печатает волну проб. Вынесено из cmdProbe по той же причине,
// что printDiscoverSummary: команда поднимает tor и конфиг, а строки отчёта
// должны проверяться без сети и без tor.
func printProbeReport(rep discover.ProbeReport) {
	// Пропуски названы в итоговой строке, а не только в построчном списке:
	// «живых 0 из 1» без них читалось как смерть адреса, хотя запрос не ушёл в
	// сеть из-за отсутствия tor.
	fmt.Printf("пробы: живых %d из %d", rep.Live, rep.Total)
	if rep.Skipped > 0 {
		fmt.Printf(" (пропущено %d: нужен tor или прокси)", rep.Skipped)
	}
	fmt.Printf(" за %s\n", rep.Elapsed)
	// Пределы волны печатаются всегда, а не только когда сработал потолок. Без
	// них «живых 0 из 3» не отличить от «живых 0 из 3 при выборке три на очередь
	// в десять тысяч адресов», и нельзя понять, с какой паузой и каким таймаутом
	// волна шла. Замер до правки на HEAD 45c45db, копия боевой базы, тор
	// выключен, транспорт direct:
	//
	//	probe --limit 3
	//	    пробы: живых 0 из 3 (пропущено 3: нужен tor или прокси) за 1ms
	//
	// Ни одного числа о пределах, хотя JSON того же прогона после этого этапа их
	// несёт.
	fmt.Printf("пределы: выборка %d, потоков %d, таймаут %s, пауза %s\n\n",
		rep.RequestedLimit, rep.Concurrency,
		appliedDelay(rep.TimeoutMS), appliedDelay(rep.DelayMS))
	// Отброшенные адреса названы отдельным блоком: без него выборка из пяти
	// непроверенных записей, где три не являются onion-адресами, печатала
	// «живых 0 из 2» и три потерянных адреса не фигурировали нигде.
	if rep.Rejected > 0 {
		fmt.Printf("отброшено %d: не onion-адрес или повтор в выборке\n", rep.Rejected)
		for _, a := range rep.RejectedAddrs {
			fmt.Printf("  %s\n", clipLine(a, 90))
		}
		fmt.Println()
	}
	if rep.LimitHit != "" {
		fmt.Printf("предел: %s\n\n", rep.LimitHit)
	}
	// Этап 174: отмена волны печатается отдельной строкой, а не строкой
	// «предел: контекст отменён». Прежний текст врал дважды: называл
	// отмену пределом и терял различие с потолком выборки, который у пробы
	// не событие отчёта, а настройка волны.
	if rep.Cancelled {
		fmt.Printf("волна прервана отменой контекста (таймаут вызова или сигнал)\n\n")
	}
	for _, r := range rep.Results {
		// Исход этой пробы и состояние пула - разные вещи, и вывод их разделяет.
		//
		// Прежняя версия печатала «мертв» по флагу r.OK, то есть утверждала
		// вердикт пула, который одна проба не устанавливает: порог «dead»
		// срабатывает на третьей неудаче подряд, поэтому первый таймаут нового
		// адреса давал в базе «unknown», а в выводе «мертв». Пользователь читал
		// подтверждённую смерть адреса и противоречие с poolsearch.
		state := "нет ответа"
		switch {
		case r.NoTransport:
			// Отдельное состояние: «нет ответа» утверждало бы, что сервис
			// промолчал, хотя обращения к нему не было вовсе.
			state = "нет tor"
		case r.OK:
			state = "отвечает"
		}
		fmt.Printf("  %-11s %6dms  %s\n", state, r.LatencyMS, r.URL)
		// Статус пула показывается только когда он расходится с исходом пробы:
		// печатать «live» рядом с «отвечает» значило бы дублировать одно и то же
		// двумя словами и растянуть строку без пользы.
		if r.PoolStatus != "" && !poolStatusMatchesProbe(r.PoolStatus, r.OK) {
			fmt.Printf("              пул: %s\n", poolStatusNote(r.PoolStatus, r.OK))
		}
		if r.Error != "" {
			fmt.Printf("              %s\n", clipLine(r.Error, 90))
		}
	}
}

// poolStatusMatchesProbe сообщает, согласован ли статус пула с исходом пробы.
// Согласованные пары не печатаются отдельно: они не несут новой информации.
func poolStatusMatchesProbe(status string, ok bool) bool {
	switch status {
	case "live":
		return ok
	case "dead":
		return !ok
	default:
		// «unknown» и прочее - переходное состояние, его стоит показать.
		return false
	}
}

// poolStatusNote объясняет расхождение статуса пула с исходом пробы.
//
// Объяснение нужно именно потому, что расхождение выглядит как ошибка: адрес не
// ответил, но мёртвым не объявлен. Без пояснения пользователь решил бы, что
// статистика врёт, или что порог смерти сломан.
func poolStatusNote(status string, ok bool) string {
	switch {
	case status == "dead" && ok:
		return "адрес считался мёртвым, эта проба вернула его в работу"
	case status == "unknown" && !ok:
		return "одной неудачи мало для вердикта, адрес останется под наблюдением"
	case status == "dead" && !ok:
		return "третья неудача подряд, адрес исключён из живых"
	default:
		return "статус в пуле: " + status
	}
}
