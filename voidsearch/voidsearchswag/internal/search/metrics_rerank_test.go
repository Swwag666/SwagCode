package search

import (
	"context"
	"testing"

	"voidsearchswag/internal/metrics"
	"voidsearchswag/internal/router"
	"voidsearchswag/internal/searchers"
)

func TestSearchRecordsMetricsAndReranks(t *testing.T) {
	e, _ := newEngine(t,
		&fixedSearcher{name: "ddg", res: []searchers.Result{{URL: "https://noise.example/x", Title: "Погода", Source: "ddg"}}},
		&fixedSearcher{name: "ddg2", res: []searchers.Result{{URL: "https://hit.example/leak-database", Title: "Leak database dump", Snippet: "leak database", Source: "ddg2"}}},
	)
	m := metrics.New()
	e.Metrics = m

	out, err := e.Search(context.Background(), Options{Query: "leak database", Mode: router.ModeFast, NoCache: true})
	if err != nil {
		t.Fatal(err)
	}
	// Релевантный результат обязан всплыть первым после реранка.
	if out.Results[0].URL != "https://hit.example/leak-database" {
		t.Errorf("топ неверен: %+v", out.Results[0])
	}
	snap := m.Snapshot()
	if snap["search_total"] != 1 || snap["search_mode_fast"] != 1 {
		t.Errorf("счётчики поиска: %+v", snap)
	}
	if snap["engine_ok_ddg"] != 1 || snap["engine_ok_ddg2"] != 1 {
		t.Errorf("счётчики движков: %+v", snap)
	}
	// Этап 164: результат «Погода» не содержит ни одного токена запроса
	// «leak database» и отсекается фильтром шума до счётчика итога, поэтому
	// search_results_total теперь честно считает живую выдачу (1), а сам
	// отсев виден отдельным счётчиком search_query_miss_dropped.
	if snap["search_results_total"] != 1 {
		t.Errorf("счётчик результатов: %+v", snap)
	}
	if snap["search_query_miss_dropped"] != 1 {
		t.Errorf("счётчик отсева шума: %+v", snap)
	}
}
