package mcpserver

import (
	"context"
	"strings"
	"testing"
	"time"

	"voidsearchswag/internal/hunt"
)

// deadEngineSearch возвращает исход поиска при мёртвых движках: выдача пуста,
// ошибка nil. Именно так ядро отвечает, когда легли все источники, и до
// появления SearchOutcome охота не могла отличить это от честной пустой выдачи.
func deadEngineSearch(total int) hunt.SearchFunc {
	return func(context.Context, string, string, int) (hunt.SearchOutcome, error) {
		return hunt.SearchOutcome{EnginesFailed: total, EnginesTotal: total}, nil
	}
}

func TestHuntRunToolReportsDeadEngines(t *testing.T) {
	// Инструмент hunt_run сериализует hunt.Report целиком, поэтому отказ обязан
	// доехать до клиента полями failed и last_error, а база - остаться нетронутой.
	st, p, _ := newExtendedServer(t, `<html><body></body></html>`)
	h := &hunt.Runner{Store: st, Search: deadEngineSearch(2)}
	srv := New(Deps{Version: "test", Store: st, Parser: p, Hunter: h, Started: time.Now()})
	c := inProcess(t, srv)
	initClient(t, c)

	callToolArgs(t, c, "hunt_create", map[string]any{"query": "leak"})
	out := callToolArgs(t, c, "hunt_run", map[string]any{})

	if failed, _ := out["failed"].(float64); failed != 1 {
		t.Errorf("failed = %v, ожидала 1: отказ всех движков не дошёл до клиента (%+v)", out["failed"], out)
	}
	if checked, _ := out["checked"].(float64); checked != 0 {
		t.Errorf("checked = %v, ожидала 0: прогон не выполнен", out["checked"])
	}
	if le, _ := out["last_error"].(string); !strings.Contains(le, "все движки поиска отказали") {
		t.Errorf("last_error = %q, ожидала причину отказа", le)
	}
	if hits, ok := out["hits"].([]any); ok && len(hits) != 0 {
		t.Errorf("находки при мёртвых движках: %v", hits)
	}

	hunts, err := st.ListHunts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(hunts) != 1 {
		t.Fatalf("охот %d, ожидала 1", len(hunts))
	}
	if hunts[0].LastHash != "" {
		t.Errorf("базовый hash записан прогоном без поиска: %q (sha256 пустой строки = %q)",
			hunts[0].LastHash, hunt.HashURLs(nil))
	}
}

func TestHuntWatchToolNamesDeadEngines(t *testing.T) {
	// Пояснение hunt_watch уводило править запрос: «охота не сломана по
	// таймауту, она пуста» при failed=0. Клиент обязан видеть отказы.
	st, p, _ := newExtendedServer(t, `<html><body></body></html>`)
	h := &hunt.Runner{Store: st, Search: deadEngineSearch(3)}
	srv := New(Deps{Version: "test", Store: st, Parser: p, Hunter: h, Started: time.Now()})
	c := inProcess(t, srv)
	initClient(t, c)

	callToolArgs(t, c, "hunt_create", map[string]any{"query": "leak"})
	out := callToolArgs(t, c, "hunt_watch", map[string]any{"timeout_s": 1, "interval_s": 1})

	if failed, _ := out["failed"].(float64); failed < 1 {
		t.Errorf("failed = %v, ожидала не меньше 1 (%+v)", out["failed"], out)
	}
	note, _ := out["note"].(string)
	if strings.Contains(note, "не сломана") {
		t.Errorf("note утверждает, что охота не сломана, при мёртвых движках: %q", note)
	}
	if !strings.Contains(note, "отказ") {
		t.Errorf("note не называет отказы поиска: %q", note)
	}
	if le, _ := out["last_error"].(string); !strings.Contains(le, "все движки поиска отказали (3 из 3)") {
		t.Errorf("last_error = %q", le)
	}
}
