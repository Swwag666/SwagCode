package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"voidsearchswag/internal/config"
	"voidsearchswag/internal/promote"
	"voidsearchswag/internal/store"
)

func cmdPromote(args []string) {
	fs := flag.NewFlagSet("promote", flag.ExitOnError)
	limit := fs.Int("limit", 0, "сколько живых сервисов проверить, не больше 200 (0 - значение из конфига)")
	budget := fs.Duration("budget", promote.DefaultBudget, "общий потолок времени прогона")
	noTor := fs.Bool("no-tor", false, "не поднимать tor")
	jsonOut := fs.Bool("json", false, "вывод в JSON")
	parseFlags(fs, args)

	cfg, err := config.Load()
	if err != nil {
		fatalf("конфиг: %v", err)
	}
	if *noTor {
		cfg.Tor = "off"
	}
	// Потолок у promote свой, а не общий для флагов: каждая проверка идёт через
	// tor по живому адресу и тратит бюджет прогона. Значения вне диапазона
	// подменялись молча, и отчёт не говорил, сколько проверок состоялось на самом
	// деле. Замер на HEAD 435cee1, копия боевой базы, --no-tor, --json:
	// --limit 0 дал checked=30, --limit -5 дал checked=30, --limit 3 дал
	// checked=9, --limit 500 дал checked=150. Ни один прогон не назвал подмену.
	limitN := 0
	if *limit != 0 {
		limitN = validLimitCeiling("limit", *limit, maxPromoteLimit)
	}
	// Ноль означает «значение из конфига», а не отказ и не десять. Дефолт флага
	// десять совпадал с envDefault поля PromoteLimit, поэтому переменная
	// окружения VOIDSEARCH_PROMOTE_LIMIT не действовала вовсе. Замер до правки на
	// HEAD 5cd38c0, копия боевой базы, --no-tor, --json, --budget 4s:
	//
	//	VOIDSEARCH_PROMOTE_LIMIT=3 promote ...
	//	    rc=1, checked=10, promoted=0, failed=10
	//	promote ... --limit 0
	//	    rc=1, ERROR: --limit должен быть положительным, получено 0
	//	promote ... --limit 200
	//	    rc=1, ERROR: --limit слишком большой: 200 (предел 50)
	//	promote -h
	//	    -limit int
	//	        сколько живых сервисов проверить (default 10)
	limitN = flagOrConfig(limitN, cfg.PromoteLimit)
	log := stderrLogger{}
	st, engine, cleanup := openStoreAndEngine(cfg, log)
	defer AtExit("поисковое ядро", AsError(cleanup))()

	known := map[string]bool{}
	if engine.Onion != nil {
		// Snapshot для единообразия: команда однопоточная и гонки здесь нет,
		// но прямой обход Engines - единственный способ получить её обратно,
		// если команду когда-нибудь позовут из серверного контекста.
		for _, e := range engine.Onion.Snapshot() {
			known[strings.ToLower(e.Base)] = true
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	onions, err := st.ListOnions(ctx, "live", limitN*3)
	if err != nil {
		fatalf("пул: %v", err)
	}
	promoter := &promote.Promoter{Client: engine.Client, Log: log, Budget: *budget}
	type hit struct {
		Name  string `json:"name"`
		Base  string `json:"base"`
		Hits  int    `json:"onion_hits"`
		Found bool   `json:"attached"`
	}
	out := []hit{}
	pr := promote.NewProgress(*budget)
	for _, o := range onions {
		// Флаг обещает число проверок, а не число поднятых сидов, поэтому цикл
		// останавливается по счётчику проверок. Запас втрое в выборке нужен под
		// фильтр уже известных движков: такие адреса пропускаются без обращения к
		// сети и счётчик не двигают. До правки условие считало только поднятые
		// сиды, и на пуле, где ничего не поднимается, команда делала втрое больше
		// проверок, чем просили: замер на HEAD 5810aca, копия боевой базы,
		// --no-tor, --json дал checked=3 на --limit 1, checked=30 на --limit 10 и
		// checked=150 на --limit 50.
		if pr.Checked >= limitN || len(out) >= limitN {
			break
		}
		if pr.Exceeded() {
			break
		}
		if known[strings.ToLower(o.URL)] {
			continue
		}
		pr.Checked++
		// Остаток бюджета ограничивает контекст одной проверки: медленный
		// кандидат не должен оставить остальных непроверенными.
		cctx := ctx
		if left := pr.Remaining(); left > 0 {
			var ccancel context.CancelFunc
			cctx, ccancel = context.WithTimeout(ctx, left)
			defer ccancel()
		}
		cand, n, err := promoter.Check(cctx, o.URL)
		if err != nil {
			// Отказ проверки больше не проглатывается. Checked к этому моменту
			// уже увеличен, и без счётчика отказов «проверено 6, поднято 0»
			// читалось как «шесть живых сервисов не дали движков», хотя на деле
			// ни одна проверка не состоялась - тор мёртв или пул состоит из
			// недоступных адресов.
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
			log.Warnf("сид %s не сохранён: %v", cand.Name, err)
			pr.FailSave(err)
			continue
		}
		attached := false
		if engine.Onion != nil {
			attached = promote.Attach(engine.Onion, engine.Health, engine.Client, cand)
		}
		_ = st.TouchEngineSeed(ctx, cand.Name)
		known[strings.ToLower(cand.Base)] = true
		out = append(out, hit{Name: cand.Name, Base: cand.Base, Hits: n, Found: attached})
	}

	if *jsonOut {
		res := map[string]any{
			"checked":     pr.Checked,
			"promoted":    out,
			"elapsed":     pr.Elapsed().String(),
			"failed":      pr.Failed,
			"save_failed": pr.SaveFailed,
		}
		if pr.LastError != "" {
			res["last_error"] = pr.LastError
		}
		if pr.StoppedReason != "" {
			res["stopped"] = pr.StoppedReason
		}
		writeJSON(res)
	} else {
		fmt.Printf("проверено %d, поднято %d за %s\n", pr.Checked, len(out), pr.Elapsed())
		// Отказы печатаются сразу за итоговой строкой. Без них «поднято 0» звучит
		// как вывод о пуле - движков не нашлось, - а не как признание, что проверки
		// не состоялись и судить о пуле не по чему.
		if note := pr.Note(); note != "" {
			fmt.Println(note)
		}
		if pr.StoppedReason != "" {
			fmt.Printf("остановлено досрочно: %s\n", pr.StoppedReason)
		}
		for _, h := range out {
			fmt.Printf("  %s  %s  (onion-ссылок %d)\n", h.Name, h.Base, h.Hits)
		}
	}
	// Отчёт печатается до выхода: код возврата добавляет сигнал для обвязки, а
	// не заменяет объяснение. RunShutdown закрывает базу и tor, потому что
	// os.Exit не выполняет defer.
	if code := promoteExitCode(pr.Checked, len(out), pr.Failed, pr.SaveFailed); code != 0 {
		RunShutdown()
		os.Exit(code)
	}
}

// promoteExitCode решает, выполнен ли прогон промоута. Ноль - работа сделана:
// движки подняты, либо проверки состоялись и просто не дали кандидатов, либо
// проверять было нечего. Единица - ни одна проверка не состоялась или найденный
// сид не удалось сохранить: tor мёртв, адреса недоступны, база не пишет.
//
// Частичный успех (поднят хотя бы один движок при отказах остальных) остаётся
// нулём: обвязка должна реагировать на «ничего не сделано», а не на «пул не
// идеален», иначе регулярный прогон превратится в источник ложных тревог.
//
// Правило вынесено в функцию, потому что оно одно на текстовый и машинный
// режим: два разных условия дали бы расхождение, которое видно только тому, кто
// читает оба вывода сразу.
func promoteExitCode(checked, promoted, failed, saveFailed int) int {
	if promoted > 0 || checked == 0 {
		return 0
	}
	if failed > 0 || saveFailed > 0 {
		return 1
	}
	return 0
}
