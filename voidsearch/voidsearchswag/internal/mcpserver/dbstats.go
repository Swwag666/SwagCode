package mcpserver

import (
	"context"
	"fmt"

	"voidsearchswag/internal/store"
)

// dbSummary собирает сводку по базе и возвращает её вместе со списком
// источников, которые прочитать не удалось.
//
// Список обязателен, потому что прежняя схема молча теряла данные: каждый
// источник был обёрнут в условие «если ошибки нет», и при ошибке ключ просто не
// попадал в map. Клиент видел ответ без hunts_total и не мог отличить «охот нет»
// от «таблицу не удалось прочитать» - ни нуля, ни ошибки, ни причины. Тот же
// дефект в CLI-команде stats измерялся на этапе 65: там при удалённой таблице
// hunts печатался ноль с кодом возврата 0.
//
// Здесь сборка одна на все вызывающие места намеренно. До правки одинаковые пять
// блоков жили в statsHandler (extended.go) и statsResource (resources.go), а два
// блока - в statusHandler (server.go) и poolResource; всего шестнадцать условий
// с проглоченной ошибкой. Общий сборщик означает, что критерии и форма отчёта не
// разойдутся между инструментом и его ресурсом-двойником.
//
// Ошибки собираются по всем источникам, а не прерывают сбор на первой: иначе
// поломка одной таблицы спрятала бы поломку второй.
func dbSummary(ctx context.Context, st *store.Store) (map[string]any, []string) {
	db := map[string]any{}
	var problems []string

	if total, live, err := st.OnionStats(ctx); err != nil {
		problems = append(problems, fmt.Sprintf("пул: %v", err))
	} else {
		db["onion_total"] = total
		db["onion_live"] = live
	}
	if total, bytes, byExt, err := st.FileStats(ctx); err != nil {
		problems = append(problems, fmt.Sprintf("файлы: %v", err))
	} else {
		db["files_total"] = total
		db["files_bytes"] = bytes
		db["files_by_ext"] = byExt
	}
	if total, running, err := st.TaskStats(ctx); err != nil {
		problems = append(problems, fmt.Sprintf("задачи: %v", err))
	} else {
		db["tasks_total"] = total
		db["tasks_running"] = running
	}
	// Число охот берётся длиной списка: отдельного счётчика нет, а ListHunts
	// читает таблицу без LIMIT, поэтому значение точное. Обрезан ListFiles, у
	// которого внутри нормализация к MaxStoreLimit = 5000.
	if hunts, err := st.ListHunts(ctx); err != nil {
		problems = append(problems, fmt.Sprintf("охоты: %v", err))
	} else {
		db["hunts_total"] = len(hunts)
	}
	if n, err := st.SelectorCount(ctx); err != nil {
		problems = append(problems, fmt.Sprintf("селекторы: %v", err))
	} else {
		db["selectors_total"] = n
	}
	// Судейская база. До этого блока выученное ранжирование не наблюдалось
	// нигде: judge_submit писал оценки, а узнать, сколько голосов накоплено и
	// скольким хостам они дали бонус, было нечем - VoteStats не имел ни одного
	// продуктового вызывающего и жил только в тестах. Молчание здесь означает,
	// что оператор не может отличить «судья не работала» от «оценки легли, но
	// порог не пройден».
	if votes, queries, hosts, err := st.VoteStats(ctx); err != nil {
		problems = append(problems, fmt.Sprintf("судьи: %v", err))
	} else {
		db["judge_votes"] = votes
		db["judge_queries"] = queries
		db["judge_hosts"] = hosts
	}
	// Порог 0 - это значение по умолчанию внутри HostQuality, то есть те же три
	// судьи, с которыми бонусы читает поисковое ядро: число хостов обязано
	// совпадать с тем, что реально участвует в реранке.
	if boosts, err := st.HostQuality(ctx, 0); err != nil {
		problems = append(problems, fmt.Sprintf("бонусы хостов: %v", err))
	} else {
		db["boosted_hosts"] = len(boosts)
	}

	withProblems(db, problems)
	return db, problems
}

// poolSummary собирает сводку по пулу onion-адресов: общее число, число живых и
// разбивку по статусам. Список непрочитанных источников обязателен по той же
// причине, что и в dbSummary.
func poolSummary(ctx context.Context, st *store.Store) (map[string]any, []string) {
	out := map[string]any{}
	var problems []string

	if total, live, err := st.OnionStats(ctx); err != nil {
		problems = append(problems, fmt.Sprintf("пул всего: %v", err))
	} else {
		out["total"] = total
		out["live"] = live
	}
	if byStatus, err := st.OnionStatusBreakdown(ctx); err != nil {
		problems = append(problems, fmt.Sprintf("разбивка пула по статусам: %v", err))
	} else {
		out["by_status"] = byStatus
	}

	withProblems(out, problems)
	return out, problems
}

// withProblems кладёт список непрочитанных источников в ответ только когда он
// непуст: на здоровой базе схема вывода остаётся прежней, и клиент не видит
// пустого поля, которое легко принять за «всё прочитано».
func withProblems(out map[string]any, problems []string) {
	if len(problems) > 0 {
		out["errors"] = problems
	}
}
