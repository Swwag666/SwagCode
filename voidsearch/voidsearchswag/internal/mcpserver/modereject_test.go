package mcpserver

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

func TestHuntCreateToolRejectsUnknownMode(t *testing.T) {
	// Инструмент hunt_create описывает режим как «auto|fast|stealth|deep», но
	// значение не проверял и отдавал его в базу как есть. Клиент получал hunt_id
	// и уверенность, что мониторинг заведён в заказанном режиме, а huntSearch
	// позже молча подменял режим на auto.
	st, p, h := newExtendedServer(t, `<html><body></body></html>`)
	srv := New(Deps{Version: "test", Store: st, Parser: p, Hunter: h, Started: time.Now()})
	c := inProcess(t, srv)
	initClient(t, c)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "hunt_create",
			Arguments: map[string]any{"query": "leak database", "mode": "depp"},
		},
	})
	if err != nil {
		t.Fatalf("hunt_create: %v", err)
	}
	if !res.IsError {
		t.Errorf("инструмент принял недопустимый режим: %s", textOf(t, res))
	}
	body := textOf(t, res)
	if !strings.Contains(body, "недопустимый режим") {
		t.Errorf("в ответе нет причины: %q", body)
	}
	if !strings.Contains(body, "depp") {
		t.Errorf("в ответе не названо отвергнутое значение: %q", body)
	}

	// Охота не должна остаться в базе.
	out := callToolArgs(t, c, "hunt_list", map[string]any{})
	if n, _ := out["count"].(float64); n != 0 {
		t.Errorf("count = %v, ожидала 0: отвергнутая охота записана (%+v)", out["count"], out)
	}
}

func TestHuntCreateToolAcceptsModes(t *testing.T) {
	st, p, h := newExtendedServer(t, `<html><body></body></html>`)
	srv := New(Deps{Version: "test", Store: st, Parser: p, Hunter: h, Started: time.Now()})
	c := inProcess(t, srv)
	initClient(t, c)

	// Пустой режим и синоним обязаны проходить: первый означает «выберет
	// роутер», второй приводится к deep.
	for _, mode := range []string{"", "auto", "fast", "stealth", "deep", "onion"} {
		args := map[string]any{"query": "запрос " + mode}
		if mode != "" {
			args["mode"] = mode
		}
		out := callToolArgs(t, c, "hunt_create", args)
		if id, _ := out["hunt_id"].(float64); id < 1 {
			t.Errorf("режим %q не создал охоту: %+v", mode, out)
		}
	}
	list := callToolArgs(t, c, "hunt_list", map[string]any{})
	if n, _ := list["count"].(float64); n != 6 {
		t.Errorf("count = %v, ожидала 6", list["count"])
	}
}
