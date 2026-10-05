package main

import (
	"context"
	"flag"
	"fmt"
	"strings"
	"time"

	"voidsearchswag/internal/config"
	"voidsearchswag/internal/router"
	"voidsearchswag/internal/search"
	"voidsearchswag/internal/store"
)

func cmdSearch(args []string) {
	fs := flag.NewFlagSet("search", flag.ExitOnError)
	mode := fs.String("mode", "auto", "auto|fast|stealth|deep")
	limit := fs.Int("limit", 0, "сколько результатов, не больше 1000 (0 - значение из конфига)")
	noCache := fs.Bool("no-cache", false, "игнорировать кэш")
	validate := fs.Bool("validate", false, "проверить первые 8 адресов живыми запросами, выкинуть мёртвые")
	jsonOut := fs.Bool("json", false, "вывод в JSON")
	noTor := fs.Bool("no-tor", false, "не поднимать tor")
	parseFlags(fs, args)

	if fs.NArg() == 0 {
		fatalf("нужен поисковый запрос: voidsearchswag search [флаги] <запрос>")
	}
	warnStrayFlags(fs, args)
	query := strings.Join(fs.Args(), " ")
	// Проверка количества идёт до конфига и базы: опечатка во флаге обязана
	// называться собой, а не выглядеть как сбой сети или хранилища. Потолок
	// отдельный, а не общий maxFlagLimit: у этой величины есть пара в конфиге, и
	// границы обязаны совпадать. Ноль проверяется отдельно и означает «взять
	// ResultLimit из конфига», поэтому отказывать на нём нельзя.
	if *limit != 0 {
		validLimitCeiling("limit", *limit, maxFlagResultLimit)
	}

	cfg, err := config.Load()
	if err != nil {
		fatalf("конфиг: %v", err)
	}
	if *noTor {
		cfg.Tor = "off"
	}
	// Дефолт флага больше не двадцать: он совпадал с envDefault ResultLimit, и
	// команда не могла отличить «флаг не задан» от «флаг задан двадцатью»,
	// поэтому VOIDSEARCH_RESULT_LIMIT не действовал вовсе. Замер до правки на
	// HEAD a33a707, копия боевой базы, тор выключен, транспорт direct:
	//
	//	VOIDSEARCH_RESULT_LIMIT=7   search --no-tor --json test   rc=0, limit=20
	//	VOIDSEARCH_RESULT_LIMIT=300 search --no-tor --json test   rc=0, limit=20
	//	search --no-tor --json --limit 0 test                     rc=1,
	//	    ERROR: --limit должен быть положительным, получено 0
	//
	// Конфиг задавал семь и триста, поиск шёл с двадцатью, а ноль, которым
	// discover и probe говорят «возьми значение из конфига», здесь отвергался.
	limitN := flagOrConfig(*limit, cfg.ResultLimit)

	log := stderrLogger{}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		fatalf("база: %v", err)
	}
	defer AtExit("база", st.Close)()
	if err := st.Migrate(context.Background()); err != nil {
		fatalf("миграции: %v", err)
	}

	pmode, ok := router.Parse(*mode)
	if !ok {
		fatalf("неизвестный режим %q (auto|fast|stealth|deep)", *mode)
	}

	engine, cleanup := buildEngine(cfg, log, st)
	defer AtExit("поисковое ядро", AsError(cleanup))()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	out, err := engine.Search(ctx, search.Options{
		Query:    query,
		Mode:     pmode,
		Limit:    limitN,
		NoCache:  *noCache,
		Validate: *validate,
	})
	if err != nil {
		fatalf("поиск: %v", err)
	}

	if *jsonOut {
		writeJSON(out)
		return
	}

	// Текст выдачи вынесен в функцию: ответ из кэша обязан отличаться от
	// живого прогона, а проверить это можно только на функции - cmdSearch
	// требует конфиг и tor.
	printSearchOutcome(out, limitN, cfg.CacheTTL)
}

// printSearchOutcome печатает человекочитаемый ответ поиска.
//
// Выдача из кэша отмечается в каждой строке, где речь идёт о движках, а не
// только в заголовке. До правки текст кэшированного ответа не отличался от
// живого вовсе: замер на двух одинаковых запросах дал
//
//	режим: fast  |  5 результатов  |  1.366s   движки: ddg-html:5, ddg-lite:fail
//	режим: fast  |  5 результатов  |  1ms      движки: ddg-html:5, ddg-lite:fail
//
// то есть вторая строка утверждала, что ddg-lite лёг сейчас, хотя запись была
// сделана в прошлом прогоне и ни один движок не запрашивался. JSON при этом
// нёс "cached": true - поле существовало, текст его не использовал. Единственным
// намёком оставалась длительность в миллисекундах, и она ничего не объясняла.
func printSearchOutcome(out *search.Outcome, limit int, cacheTTL time.Duration) {
	fmt.Printf("запрос: %s\n", out.Query)
	fmt.Printf("режим: %s", out.Mode)
	if out.Degraded {
		fmt.Printf(" -> отработал %s (деградация)", out.UsedMode)
	}
	fmt.Printf("  |  %d результатов  |  %s\n", out.Count, out.Duration)
	if out.Cached {
		if cacheTTL > 0 {
			fmt.Printf("выдача из кэша: движки в этом прогоне не запрашивались, запись живёт %s\n", cacheTTL)
		} else {
			fmt.Println("выдача из кэша: движки в этом прогоне не запрашивались")
		}
	}
	// Поломка кэша печатается явно и отдельно от строки «выдача из кэша»: это
	// противоположные факты, и смешивать их в одной строке нельзя. Без этой
	// оговорки мёртвый кэш выглядел как обычный прогон - только каждый запрос
	// шёл в движки заново, и пользователь об этом не знал.
	if out.CacheNote != "" {
		fmt.Printf("кэш: %s\n", out.CacheNote)
	}
	// Нехватка против запрошенного количества объясняется явно.
	//
	// Без этой строки `-limit 30` выглядел сломанным: пользователь просил 30,
	// получал 8 и не имел способа понять, где потолок. А потолок здесь не наш -
	// html- и lite-эндпоинты DuckDuckGo отдают фиксированную страницу без
	// параметра числа результатов и без пагинации, поэтому запросить больше,
	// чем движок вернул, нельзя в принципе. Молчание заставляло думать, что
	// флаг не работает, и подбирать его значение вслепую.
	if limit > 0 && out.Count < limit {
		if out.Cached {
			// Та же нехватка, но причина другая: движки в этом прогоне не
			// участвовали, и ссылаться на них было бы ложью.
			fmt.Printf("запрошено %d, получено %d: столько лежит в записи кэша, движки не запрашивались\n",
				limit, out.Count)
		} else {
			fmt.Printf("запрошено %d, получено %d: больше не дали сами движки, а не фильтр\n",
				limit, out.Count)
		}
	}
	if out.Count == 0 {
		// Пустая выдача - отдельное состояние, а не «0 результатов» в общей
		// строке. По нулевому счёту не отличить «ничего не найдено» от
		// «движки упали», поэтому причина печатается явно.
		var failed []search.EngineReport
		for _, er := range out.Report.Engines {
			if !er.OK {
				failed = append(failed, er)
			}
		}
		// Отчёт движков в кэшированном ответе описывает прошлый прогон, поэтому
		// вывод о причине пустоты помечается как сделанный по записи.
		stamp := ""
		if out.Cached {
			stamp = " (по записи кэша)"
		}
		if len(failed) > 0 && len(failed) == len(out.Report.Engines) {
			fmt.Println("ничего не найдено: все движки отвалились" + stamp)
		} else if len(failed) > 0 {
			fmt.Println("ничего не найдено: часть движков отвалилась, остальные дали пусто" + stamp)
		} else {
			fmt.Println("ничего не найдено: движки ответили, но по этому запросу пусто" + stamp)
		}
		printEngineFailures(out.Report.Engines)
	}
	fmt.Printf("решение: %s\n", out.Decision.Reason)
	printSearchFallbacks(out.Report.Fallbacks)
	// Примечание отчёта печатается всегда, когда оно есть: там лежит объяснение
	// деградации («onion-выдача пуста, отработал clearnet») и причина пропущенных
	// запасных режимов. До правки оно уходило только в JSON и в ответ MCP, поэтому
	// в живом прогоне оператор видел «ничего не найдено» и не знал, что поиск даже
	// не пытался уйти в другой режим.
	if out.Report.Note != "" {
		fmt.Printf("примечание: %s\n", out.Report.Note)
	}
	if len(out.Report.Engines) > 0 {
		if out.Cached {
			fmt.Printf("движки (запись кэша): ")
		} else {
			fmt.Printf("движки: ")
		}
		for i, e := range out.Report.Engines {
			if i > 0 {
				fmt.Printf(", ")
			}
			if e.OK {
				fmt.Printf("%s:%d", displayEngineName(e.Name), e.Count)
			} else {
				fmt.Printf("%s:fail", displayEngineName(e.Name))
			}
			// Факт повторного опроса виден в отчёте: он объясняет, почему поиск
			// занял дольше ожидаемого и почему обычно отвечающий движок здесь
			// лёг. Повтор делается после смены tor-цепи, когда первая попытка
			// не дала ни одного живого движка.
			if e.Retried {
				fmt.Printf("(повтор после смены цепи)")
			}
		}
		fmt.Println()
		// Частичный отказ при непустой выдаче: компактная строка называет только
		// факт отказа, а его причина лежит в отчёте движка и до правки уходила
		// исключительно в JSON. При нулевой выдаче причины напечатаны выше,
		// вместе с объяснением пустоты, и второй вызов удвоил бы счёт одной и той
		// же поломки.
		if out.Count > 0 {
			printEngineFailures(out.Report.Engines)
		}
	}
	fmt.Println()
	for _, r := range out.Results {
		fmt.Printf("%2d. %s\n    %s\n", r.Rank, r.Title, r.URL)
		if r.Snippet != "" {
			fmt.Printf("    %s\n", clipLine(r.Snippet, 160))
		}
	}
}

// printSearchFallbacks печатает запасные режимы, в которые ушёл поиск после пустой
// выдачи.
//
// Без этой строки длительность охватывала все прогоны движков, а отчёт называл один
// режим и его движки. Замер до правки на HEAD d4c9f03, молчащий socks-прокси на
// 127.0.0.1:18999, VOIDSEARCH_REQUEST_TIMEOUT=2s, search test --limit 2 --no-cache:
// прогон длился 16184 мс, напечатал «режим: fast | 0 результатов | 16.011s» и два
// движка с elapsed 4.002s каждый. Двенадцать секунд из шестнадцати не объясняло
// ничто: stealth и deep отработали и исчезли из отчёта, потому что их выдача тоже
// оказалась пустой, а отчёт запасного режима не сохранялся.
//
// Пустой список не печатается вовсе: строка «запасные режимы:» без продолжения
// выглядела бы оборванной.
func printSearchFallbacks(fbs []search.FallbackReport) {
	if len(fbs) == 0 {
		return
	}
	fmt.Printf("запасные режимы после пустой выдачи: ")
	for i, fb := range fbs {
		if i > 0 {
			fmt.Printf(", ")
		}
		fmt.Printf("%s %s (движков %d, живых %d)", fb.Mode, fb.Duration, fb.Engines, fb.Live)
	}
	fmt.Println()
}

// printEngineFailures печатает причины отказа движков построчно.
//
// Причина всегда лежала в отчёте движка и всегда уходила в JSON, а в тексте
// печаталась только при нулевой выдаче. Частичный отказ - самый частый случай -
// оставался необъяснённым: живой deep-прогон без tor печатал строку «движки:
// tornet:fail, ahmia:fail, tor66:fail, ahmia-clear:fail, ddg-html:3,
// ddg-lite:fail» и ни слова о том, что четыре отказа из пяти означают «onion-адрес
// недостижим напрямую: нужен tor или прокси». Без причины пользователь не может
// отличить отсутствие tor от таймаута, блокировки и поломки разметки, а лечится
// каждое по-разному.
//
// Строка «движки:» остаётся компактной: причины идут отдельными строками под ней,
// иначе на пяти движках её нельзя было бы прочитать.
//
// Пустая причина отмечается явно. «Без описания» лучше молчания: молчание
// выглядит как потерянная строка вывода, а не как отсутствие данных.
//
// Причина усекается до той же ширины, что и сниппет результата: onion-адрес в
// конце сообщения об отказе длиннее строки терминала, и перенос ломал бы
// выравнивание списка.
func printEngineFailures(engines []search.EngineReport) {
	for _, er := range engines {
		if er.OK {
			continue
		}
		reason := er.Error
		if reason == "" {
			reason = "без описания"
		}
		fmt.Printf("  %s: %s\n", displayEngineName(er.Name), clipLine(reason, 160))
	}
}
