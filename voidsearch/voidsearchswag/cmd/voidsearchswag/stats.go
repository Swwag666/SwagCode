package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"voidsearchswag/internal/config"
	"voidsearchswag/internal/store"
)

func cmdStats(args []string) {
	fs := flag.NewFlagSet("stats", flag.ExitOnError)
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
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		fatalf("миграции: %v", err)
	}
	out, problems := statsData(ctx, st)
	if *jsonOut {
		// В машинном выводе причины уходят в поле errors и НЕ дублируются в
		// stderr: потребитель читает поток целиком, и примешанный текст сделал бы
		// JSON неразбираемым. Сигналом неполноты остаётся ненулевой код возврата.
		out.Problems = problems
		writeJSON(out)
	} else {
		fmt.Printf("пул: %d всего / %d живых\n", out.OnionTotal, out.OnionLive)
		fmt.Printf("файлы: %d (%s)\n", out.FilesTotal, humanBytes(out.FilesBytes))
		fmt.Printf("задачи: %d (running %d)  охоты: %d  селекторы: %d\n",
			out.TasksTotal, out.TasksRunning, out.HuntsTotal, out.SelectorsTotal)
		// Судейская база печатывается двумя числами: объём голосов и число
		// хостов, которым бонус реально выдаётся. Одно только число голосов
		// вводило бы в заблуждение: голоса есть, а доверия нет, пока судей
		// меньше трёх.
		fmt.Printf("судьи: %d голосов, %d запросов, %d хостов  бонусы: %d хостов\n",
			out.JudgeVotes, out.JudgeQueries, out.JudgeHosts, out.BoostedHosts)
		// Версия схемы печатается всегда, а не только когда что-то не так: это
		// единственное место, где оператор может сверить базу с бинарём до того,
		// как расхождение выйдет боком в произвольном запросе.
		fmt.Printf("схема: %d/%d\n", out.SchemaApplied, out.SchemaKnown)
		if len(out.SchemaRepaired) > 0 {
			fmt.Printf("восстановлены пропущенные миграции: %v\n", out.SchemaRepaired)
		}

		// Непрочитанный источник обязан быть виден: без этого счётчик, который не
		// удалось получить, неотличим от настоящего нуля.
		//
		// Измерено на базе, где таблица hunts удалена: stats печатал «охоты: 0»
		// рядом с точными «пул: 1» и «файлы: 2», JSON отдавал hunts_total: 0, а код
		// возврата был 0. До поломки та же команда печатала «охоты: 3». То есть
		// диагностика, ради которой команду и запускают, сообщала об отсутствии
		// данных там, где не смогла их прочитать, и скрипт мониторинга считал
		// такой ответ успешным.
		//
		// Предупреждения печатаются после чисел, потому что остальные источники
		// прочитаны верно и терять их нельзя: пользователь видит и то, что удалось
		// снять, и то, чему верить нельзя.
		for _, p := range problems {
			fmt.Fprintf(os.Stderr, "ERROR: stats: %s\n", p)
		}
	}

	// Код возврата при неполной диагностике ненулевой: молчаливый успех и был
	// сутью дефекта.
	if len(problems) > 0 {
		// os.Exit не выполняет defer, поэтому стек очистки опустошается явно, как
		// это делает fatalf: иначе база осталась бы с несброшенным WAL.
		RunShutdown()
		os.Exit(1)
	}
}

// statsData собирает сводку и возвращает список источников, которые прочитать не
// удалось.
//
// Ошибки собираются по всем источникам, а не прерывают сбор на первой: иначе
// поломка одной таблицы спрятала бы поломку второй, и после её починки
// пользователь обнаружил бы новую проблему, о которой команда знала, но
// промолчала. Пропуск ошибки здесь недопустим - именно он и был дефектом.
func statsData(ctx context.Context, st *store.Store) (statsDataOut, []string) {
	var out statsDataOut
	var problems []string

	if total, live, err := st.OnionStats(ctx); err != nil {
		problems = append(problems, fmt.Sprintf("пул: %v", err))
	} else {
		out.OnionTotal, out.OnionLive = total, live
	}
	if total, bytes, byExt, err := st.FileStats(ctx); err != nil {
		problems = append(problems, fmt.Sprintf("файлы: %v", err))
	} else {
		out.FilesTotal, out.FilesBytes, out.FilesByExt = total, bytes, byExt
	}
	if total, running, err := st.TaskStats(ctx); err != nil {
		problems = append(problems, fmt.Sprintf("задачи: %v", err))
	} else {
		out.TasksTotal, out.TasksRunning = total, running
	}
	// Число охот берётся длиной списка, потому что отдельного счётчика нет, а
	// ListHunts читает таблицу без LIMIT: значение точное, в отличие от
	// ListFiles, который обрезан внутренним пределом store.
	if hunts, err := st.ListHunts(ctx); err != nil {
		problems = append(problems, fmt.Sprintf("охоты: %v", err))
	} else {
		out.HuntsTotal = len(hunts)
	}
	if n, err := st.SelectorCount(ctx); err != nil {
		problems = append(problems, fmt.Sprintf("селекторы: %v", err))
	} else {
		out.SelectorsTotal = n
	}
	// Судейская база. До этого блока выученное ранжирование не наблюдалось
	// нигде в CLI: judge_submit писал оценки, а проверить, чему система
	// научилась, было нечем. Разница между «голоса есть» и «ранжирование
	// выучено» принципиальна: хост получает бонус только при трёх разных
	// судьях, поэтому число голосов само по себе ничего не обещает.
	if votes, queries, hosts, err := st.VoteStats(ctx); err != nil {
		problems = append(problems, fmt.Sprintf("судьи: %v", err))
	} else {
		out.JudgeVotes, out.JudgeQueries, out.JudgeHosts = votes, queries, hosts
	}
	// Порог 0 означает значение по умолчанию внутри HostQuality - те же три
	// судьи, с которыми бонусы читает поисковое ядро.
	if boosts, err := st.HostQuality(ctx, 0); err != nil {
		problems = append(problems, fmt.Sprintf("бонусы хостов: %v", err))
	} else {
		out.BoostedHosts = len(boosts)
	}
	// Состояние схемы берётся из итога последнего Migrate. Команды вызывают
	// миграции до сбора сводки, поэтому цифры описывают текущую базу, а
	// восстановленные пропущенные версии видны оператору: молча починить журнал и
	// не сказать было бы тем же замалчиванием, из-за которого дырка в журнале
	// вообще стала возможной.
	run := st.Migrations()
	out.SchemaKnown, out.SchemaApplied = run.Known, run.Applied
	out.SchemaRepaired = run.Repaired
	return out, problems
}
