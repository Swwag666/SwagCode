package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"voidsearchswag/internal/config"
	"voidsearchswag/internal/parser"
)

func cmdParse(args []string) {
	fs := flag.NewFlagSet("parse", flag.ExitOnError)
	var fields listFlag
	fs.Var(&fields, "fields", "поля через запятую: title,price; флаг можно повторять")
	noTor := fs.Bool("no-tor", false, "не поднимать tor")
	jsonOut := fs.Bool("json", false, "вывод в JSON")
	parseFlags(fs, args)
	if fs.NArg() == 0 {
		fatalf("нужен URL: voidsearchswag parse --fields title,price <url>")
	}
	rawURL := fs.Arg(0)

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

	p := &parser.Parser{Store: st, Fetch: engineFetch{engine}}
	list := fields.listOr("title")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	values, healed, warn, err := p.ParseDetailed(ctx, rawURL, list)
	if err != nil && len(values) == 0 {
		fatalf("разбор: %v", err)
	}
	if *jsonOut {
		writeJSON(parseJSONPayload(values, healed, warn, err))
	} else {
		printParseOutcome(list, values, healed, warn, err)
	}
	// Код возврата при ошибке разбора ненулевой в обоих форматах. Parse
	// возвращает непустую карту значений вместе с ошибкой «ни одно поле не
	// извлечено», поэтому проверка выше не срабатывает, и до правки команда
	// завершалась кодом 0: скрипт, который разбирает поля из JSON, считал
	// пустую выдачу успехом.
	if err != nil {
		RunShutdown()
		os.Exit(1)
	}
}

// parseJSONPayload собирает машинный ответ parse.
//
// Ошибка разбора обязана доехать до JSON. До правки она печаталась только в
// stderr текстовой ветки, а машинный вывод отдавал fields с пустыми значениями,
// healed=true и код возврата 0. Замер на странице без единого извлекаемого поля:
//
//	{"fields":{"price":{"value":"","confidence":0,"strategy":"none","healed":true},
//	           "title":{"value":"","confidence":0,"strategy":"none","healed":true}},
//	 "healed":true}
//
// и пустой stderr - то есть потребитель не имел способа узнать, что разбор не
// состоялся, кроме как проверять каждое value на пустоту.
//
// Поле warning доносит деградацию, которая ошибкой не является: сохранённые
// селекторы хоста не прочитаны, разбор шёл запасными стратегиями, и healed=true
// здесь не означает, что селекторы плохи.
func parseJSONPayload(values map[string]parser.Result, healed bool, warn string, err error) map[string]any {
	out := map[string]any{"fields": values, "healed": healed}
	if warn != "" {
		out["warning"] = warn
	}
	if err != nil {
		out["error"] = err.Error()
	}
	return out
}

// printParseOutcome печатает человекочитаемый ответ parse.
//
// Пустое значение больше не выдаётся за результат. Прежняя строка
// «title [none 0.0]: » читалась как извлечённое поле с пустым содержимым, а не
// как признание, что поля нет, и стояла рядом с healed=true - советом
// перегенерировать селекторы, который при полном провале уводит от настоящей
// причины: страница не содержит этих полей или не загрузилась.
func printParseOutcome(list []string, values map[string]parser.Result, healed bool, warn string, err error) {
	extracted := 0
	for _, f := range list {
		name := strings.TrimSpace(f)
		r, ok := values[name]
		if !ok {
			continue
		}
		if r.Value == "" {
			fmt.Printf("%s: не извлечено\n", name)
			continue
		}
		extracted++
		fmt.Printf("%s [%s %.1f]: %s\n", name, r.Strategy, r.Confidence, clipLine(r.Value, 200))
	}
	// Предупреждение печатается раньше совета о селекторах: при недоступной базе
	// совет перегенерировать их уводит от настоящей причины, а значения, взятые
	// запасной стратегией, выглядят как обычный результат разбора.
	if warn != "" {
		fmt.Printf("внимание: %s\n", warn)
	}
	if healed && extracted > 0 {
		fmt.Println("healed=true: часть полей взята запасной стратегией, селекторы стоит перегенерировать")
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: parse: %v\n", err)
	}
}
