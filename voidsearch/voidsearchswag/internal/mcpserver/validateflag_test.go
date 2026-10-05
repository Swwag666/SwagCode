package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"

	"voidsearchswag/internal/search"
)

// Этап 164: параметр validate доехал от MCP-аргумента до Options движка.
// Жалоба смоук-агентов была про dead-URL в выдаче; флаг обязан
// прокидываться, а не молча теряться на границе протокола.
func TestSearchToolValidateFlagReachesEngine(t *testing.T) {
	eng, st := testEngineWithStub(t)
	srv := New(Deps{Version: "test", Store: st, Search: eng, Started: time.Now()})
	c, err := client.NewInProcessClient(srv)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	res, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "search", Arguments: map[string]any{
			"query": "validate probe", "mode": "fast", "validate": true,
		}},
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if res.IsError {
		t.Fatalf("search вернул ошибку: %+v", res.Content)
	}
	var out search.Outcome
	if err := json.Unmarshal([]byte(textOf(t, res)), &out); err != nil {
		t.Fatalf("ответ не JSON: %v", err)
	}
	// Транспорт в тестовом движке не поднят: validate обязан честно
	// признаться в Note, а не сделать вид, что проверки были.
	if !strings.Contains(out.Report.Note, "validate пропущен") {
		t.Errorf("note не объясняет пропуск validate: %q", out.Report.Note)
	}
}

// Описание инструмента обязано называть validate: смоук-агенты читают
// tool-схемы, а не исходники, и параметр без описания для них не существует.
func TestSearchToolSchemaDescribesValidate(t *testing.T) {
	_, c := newTestServer(t)
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	for _, tool := range res.Tools {
		if tool.Name != "search" {
			continue
		}
		if !strings.Contains(tool.Description, "validate") {
			t.Errorf("описание search не упоминает validate: %q", tool.Description)
		}
		var has bool
		for k := range tool.InputSchema.Properties {
			if k == "validate" {
				has = true
			}
		}
		if !has {
			t.Error("в схеме search нет параметра validate")
		}
		return
	}
	t.Fatal("инструмент search не найден в списке")
}
