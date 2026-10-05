package mcpserver

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"voidsearchswag/internal/store"
)

func TestPromoteToolReportsFailedChecks(t *testing.T) {
	// Замер: кандидат недостижим, проверка падает, и ответ инструмента выглядит
	// как «проверено 1, ничего не найдено». До правки отказ проверки уходил в
	// continue без счёта и без причины, хотя pr.Checked уже был увеличен.
	st, eng, p := promoteDeps(t, engineStub())
	mcpSrv := New(Deps{Version: "test", Store: st, Search: eng, Promoter: p, Started: time.Now()})
	c := inProcess(t, mcpSrv)
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Порт 1 на локальном адресе отвечает отказом сразу: ошибка проверки
	// гарантирована и не зависит от внешней сети.
	if err := st.UpsertOnion(ctx, store.Onion{URL: "http://127.0.0.1:1/", Status: "live"}); err != nil {
		t.Fatal(err)
	}
	res, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "promote_engines", Arguments: map[string]any{"limit": 5}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("promote вернул ошибку: %+v", res.Content)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(textOf(t, res)), &out); err != nil {
		t.Fatal(err)
	}
	if out["checked"] != float64(1) {
		t.Fatalf("проверено %+v, ожидала 1: %+v", out["checked"], out)
	}
	promoted, _ := out["promoted"].([]any)
	if len(promoted) != 0 {
		t.Fatalf("поднято %d при недоступном кандидате", len(promoted))
	}
	if out["failed"] != float64(1) {
		t.Errorf("отказ проверки не виден в ответе: %+v", out)
	}
	if reason, _ := out["last_error"].(string); reason == "" {
		t.Errorf("в ответе нет причины отказа: %+v", out)
	}
}
