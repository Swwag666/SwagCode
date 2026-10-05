package mcpserver

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"voidsearchswag/internal/hunt"
)

func TestHuntRunToolReportsFailedSearches(t *testing.T) {
	// Инструмент hunt_run сериализует hunt.Report целиком, поэтому поле failed
	// появляется в ответе само. Проверка фиксирует, что оно действительно
	// доходит до клиента: до разделения счётчиков отказ поиска выглядел как
	// skipped, и клиент не мог отличить «ещё не пора» от «поиск лёг».
	st, p, _ := newExtendedServer(t, `<html><body></body></html>`)
	h := &hunt.Runner{Store: st, Search: func(context.Context, string, string, int) (hunt.SearchOutcome, error) {
		return hunt.SearchOutcome{}, errors.New("tor мёртв")
	}}
	srv := New(Deps{Version: "test", Store: st, Parser: p, Hunter: h, Started: time.Now()})
	c := inProcess(t, srv)
	initClient(t, c)

	callToolArgs(t, c, "hunt_create", map[string]any{"query": "leak"})
	out := callToolArgs(t, c, "hunt_run", map[string]any{})

	if failed, _ := out["failed"].(float64); failed != 1 {
		t.Errorf("failed = %v, ожидала 1: отказ поиска не дошёл до клиента (%+v)", out["failed"], out)
	}
	if checked, _ := out["checked"].(float64); checked != 0 {
		t.Errorf("checked = %v, ожидала 0", out["checked"])
	}
	if le, _ := out["last_error"].(string); !strings.Contains(le, "tor мёртв") {
		t.Errorf("last_error = %q, ожидала причину отказа", out["last_error"])
	}
}

func TestHuntWatchToolReportsFailedSearches(t *testing.T) {
	// Диагностика hunt_watch строится вручную, поэтому failed нужно добавлять
	// явно. Без него клиент видел бы checked=0, timeout=true и note, который до
	// правки утверждал: «охота не сломана по таймауту, она пуста».
	st, p, _ := newExtendedServer(t, `<html><body></body></html>`)
	h := &hunt.Runner{Store: st, Search: func(context.Context, string, string, int) (hunt.SearchOutcome, error) {
		return hunt.SearchOutcome{}, errors.New("tor мёртв")
	}}
	srv := New(Deps{Version: "test", Store: st, Parser: p, Hunter: h, Started: time.Now()})
	c := inProcess(t, srv)
	initClient(t, c)

	callToolArgs(t, c, "hunt_create", map[string]any{"query": "leak"})
	out := callToolArgs(t, c, "hunt_watch", map[string]any{"timeout_s": 1, "interval_s": 1})

	if out["timeout"] != true {
		t.Errorf("ожидание должно закончиться таймаутом: %+v", out)
	}
	if failed, _ := out["failed"].(float64); failed < 1 {
		t.Errorf("failed = %v, ожидала не меньше 1: отказы потеряны в диагностике (%+v)", out["failed"], out)
	}
	if le, _ := out["last_error"].(string); !strings.Contains(le, "tor мёртв") {
		t.Errorf("last_error = %q, ожидала причину отказа", out["last_error"])
	}
	note, _ := out["note"].(string)
	if strings.Contains(note, "не сломана") {
		t.Errorf("note утверждает, что охота не сломана, хотя поиск не выполнился: %q", note)
	}
	if !strings.Contains(note, "отказ") {
		t.Errorf("note не говорит об отказах: %q", note)
	}
}
