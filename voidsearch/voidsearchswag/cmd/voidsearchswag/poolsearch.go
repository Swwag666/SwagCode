package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"voidsearchswag/internal/config"
	"voidsearchswag/internal/hunt"
	"voidsearchswag/internal/search"
	"voidsearchswag/internal/store"
)

func cmdPoolSearch(args []string) {
	fs := flag.NewFlagSet("poolsearch", flag.ExitOnError)
	// Собственная справка нужна потому, что запрос у команды позиционный, а
	// пакет flag печатает только флаги. Без этого `poolsearch -h` показывал
	// три флага и ни слова о том, куда девать само слово для поиска, -
	// единственный обязательный аргумент команды оставался невидимым.
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `Использование: voidsearchswag poolsearch [флаги] <запрос>

Ищет по собранной базе onion-сервисов: название, описание или адрес.
Запрос позиционный и необязательный - без него выводятся лучшие записи пула.

Совпадения ранжируются: название важнее описания, описание важнее адреса.
Потолок выдачи - 500 записей, и совпадений может быть больше: шапка печатает
точное число и называет причину усечения («найдено 1200, показано 500: потолок
выдачи 500» или «найдено 11, показано 3: действует --limit 3»), а при --json
оно уходит предупреждением в stderr, чтобы stdout остался чистым списком.
Адрес onion-сервиса - 56 случайных символов, поэтому короткое слово может
встретиться в нём просто по случайности; такие записи уходят в конец выдачи,
но не теряются: искать сервис по фрагменту адреса, когда название неизвестно,
остается рабочим сценарием.

Флаги:
`)
		fs.PrintDefaults()
		fmt.Fprintf(os.Stderr, `
Примеры:
  poolsearch library              сервисы со словом library в названии
  poolsearch book -status live    только живые
  poolsearch -limit 100 -json     первые 100 записей пула в JSON
`)
	}
	limit := fs.Int("limit", 30, "сколько записей показать")
	status := fs.String("status", "", "фильтр по статусу: live|unknown|dead")
	jsonOut := fs.Bool("json", false, "вывод в JSON")
	parseFlags(fs, args)

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

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	text := strings.Join(fs.Args(), " ")
	includeDead := *status == "dead"
	onions, err := st.SearchOnions(ctx, text, *status, *limit, includeDead)
	if err != nil {
		fatalf("поиск по пулу: %v", err)
	}
	// Точное число совпадений берётся отдельным запросом. Выдача ограничена
	// потолком store.OnionSearchLimit(), поэтому её размер нельзя печатать под
	// словом «найдено»: при 1200 совпадениях команда рапортовала «найдено 500»,
	// и отличить полный ответ от усечённого было нельзя. Ошибка счётчика не
	// отменяет полученную выдачу, но и заменять факт догадкой нельзя - число
	// совпадений в этом случае не печатается вовсе.
	matched, matchErr := st.CountOnionSearch(ctx, text, *status, includeDead)

	if *jsonOut {
		// Схема ответа остаётся списком записей: потребители разбирают его как
		// массив, и превращать его в объект значит ломать их. Сведения об
		// усечении и о непрочитанном счётчике уходят в stderr, чтобы stdout
		// остался разбираемым JSON целиком.
		if matchErr == nil && matched > len(onions) {
			fmt.Fprintf(os.Stderr, "WARN: совпадений %d, показано %d: %s\n",
				matched, len(onions), poolTruncationReason(matched, len(onions), *limit))
		}
		if matchErr != nil {
			fmt.Fprintf(os.Stderr, "ERROR: poolsearch: число совпадений не прочитано: %v\n", matchErr)
		}
		writeJSON(onions)
		if matchErr != nil {
			RunShutdown()
			os.Exit(1)
		}
		return
	}

	total, live, statsErr := st.OnionStats(ctx)
	problems := printPoolHeader(total, live, statsErr, matched, matchErr, len(onions), *limit)
	for _, o := range onions {
		title := o.Title
		if title == "" {
			title = "(без названия)"
		}
		fmt.Printf("  [%-7s] %s\n", o.Status, clipLine(title, 70))
		fmt.Printf("            %s", o.URL)
		if o.LatencyAvg > 0 {
			fmt.Printf("  %dms", o.LatencyAvg)
		}
		fmt.Println()
	}
	// Непрочитанный источник обязан быть виден: без этого счётчик, который не
	// удалось получить, неотличим от настоящего нуля. Нули в строке «пул: всего
	// 0, живых 0» раньше печатались как факт, потому что ошибка OnionStats
	// уходила в _.
	for _, p := range problems {
		fmt.Fprintf(os.Stderr, "ERROR: poolsearch: %s\n", p)
	}
	if len(problems) > 0 {
		RunShutdown()
		os.Exit(1)
	}
}

// poolTruncationReason объясняет, почему показано меньше, чем найдено, и
// возвращает пустую строку, если выдача полная.
//
// Причина не одна: выдачу режет либо --limit, либо потолок хранилища
// store.OnionSearchLimit(). Прежний текст всегда называл потолок. Живой замер до
// правки на HEAD ffb3312, копия боевой базы, пул 11088 записей, из них 11
// совпадений со словом leak:
//
//	poolsearch --limit 3 leak
//	  rc=0, «пул: всего 11088, живых 1773; найдено 11, показано 3: потолок выдачи 500»
//	poolsearch --limit 3 leak --json
//	  rc=0, count=3, stderr: WARN: совпадений 11, показано 3: потолок выдачи 500
//	poolsearch --limit 1000 leak
//	  rc=0, «найдено 11» без оговорки
//
// До потолка в 500 было далеко, совпадений всего 11, и три записи показал сам
// оператор. Сообщение отправляло его разбираться с пределом хранилища, который ни
// при чём, а настоящее усечение - при 1200 совпадениях и потолке 500 - теряло
// силу: обе ситуации звучали одинаково.
func poolTruncationReason(matched, shown, limit int) string {
	if matched <= shown {
		return ""
	}
	ceiling := store.OnionSearchLimit()
	switch {
	case limit > 0 && limit <= ceiling && shown >= limit:
		return fmt.Sprintf("действует --limit %d", limit)
	case shown >= ceiling:
		return fmt.Sprintf("потолок выдачи %d", ceiling)
	default:
		return "действует дефолт выборки хранилища"
	}
}

// printPoolHeader печатает шапку выдачи poolsearch и возвращает список проблем.
//
// Три числа в шапке имеют разную природу, и смешивать их нельзя: total и live -
// размер всего пула, matched - точное число совпадений с запросом, shown -
// размер выдачи, который упирается в потолок. Прежняя шапка печатала shown под
// словом «найдено» и подставляла нули при ошибке OnionStats. Причина усечения
// берётся из poolTruncationReason: limit нужен шапке, чтобы отличить волю
// оператора от предела хранилища.
func printPoolHeader(total, live int, statsErr error, matched int, matchErr error, shown int, limit int) []string {
	var problems []string
	if statsErr != nil {
		problems = append(problems, fmt.Sprintf("статистика пула не прочитана: %v", statsErr))
		fmt.Println("пул: статистика не прочитана")
	} else {
		fmt.Printf("пул: всего %d, живых %d; ", total, live)
	}
	if matchErr != nil {
		problems = append(problems, fmt.Sprintf("число совпадений не прочитано: %v", matchErr))
		fmt.Printf("показано %d, точное число совпадений не прочитано\n\n", shown)
		return problems
	}
	if matched > shown {
		fmt.Printf("найдено %d, показано %d: %s\n\n", matched, shown, poolTruncationReason(matched, shown, limit))
		return problems
	}
	fmt.Printf("найдено %d\n\n", matched)
	return problems
}

func onionCount(e *search.Engine) int {
	if e == nil || e.Onion == nil {
		return 0
	}
	// Snapshot: функция вызывается из status и других команд, которые могут
	// работать при запущенном фоновом промоуте.
	return len(e.Onion.Snapshot())
}

// fileHostSearch - кэширующий адаптер для вспомогательных прогонов. Единственный
// потребитель - findFileHosts: он обходит список файловых запросов подряд, и
// повтор в пределах TTL экономит сеть, не меняя смысла.
//
// Имя намеренно не «huntSearch»: охота этим адаптером больше не пользуется и не
// должна. Пока функция называлась huntSearch, собрать на ней мониторинг было
// одной опечаткой, и эта опечатка стоила охоте способности видеть живую выдачу.
func fileHostSearch(eng *search.Engine) hunt.SearchFunc {
	return huntSearchWith(eng, false)
}
