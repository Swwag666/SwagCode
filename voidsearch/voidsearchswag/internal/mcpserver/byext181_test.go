package mcpserver

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"voidsearchswag/internal/catalog"
)

// Кэтчер этапа 181 (смоук-180): by_ext обязан присутствовать в ответе
// collect_files ВСЕГДА - и при пустом сборе, и при непустом. Прежде поле
// публиковалось только при len(rep.ByExt) > 0, и слепой смоук-180 увидел
// несогласованную схему: в вызове с находками by_ext был, в двух соседних
// без находок - отсутствовал. Клиент не может отличить «нет файлов» от
// «ответ неполон» по отсутствию поля.
func TestCollectByExtAlwaysInResponse(t *testing.T) {
	dir := t.TempDir()
	st := exportStore(t, dir)
	seedMCPBase(t, st, dir)

	// Две страницы одного fetcher'а не нужны: пустой и непустой прогоны
	// делаются двумя разными collector'ами с общим store.
	mkDeps := func(body string) Deps {
		col := catalog.NewCollector(&stubFetcher{body: body}, st, nil, nil,
			catalog.Config{PerHostDelay: time.Millisecond})
		return Deps{Version: "test", Store: st, Collector: col, Started: time.Now()}
	}

	collect := func(d Deps) map[string]any {
		t.Helper()
		req := mcp.CallToolRequest{
			Params: mcp.CallToolParams{
				Name:      "collect_files",
				Arguments: map[string]any{"hosts": []any{"abcdefghijklmnop.onion"}, "timeout": 60},
			},
		}
		res, err := d.collectFilesHandler(context.Background(), req)
		if err != nil {
			t.Fatalf("collectFilesHandler: %v", err)
		}
		if len(res.Content) == 0 {
			t.Fatal("пустой content в результате")
		}
		tc, ok := res.Content[0].(mcp.TextContent)
		if !ok {
			t.Fatalf("content[0] = %T, хочу TextContent", res.Content[0])
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(tc.Text), &out); err != nil {
			t.Fatalf("разбор ответа: %v\n%s", err, tc.Text)
		}
		return out
	}

	// Прогон без файловых ссылок: страница живая, находок ноль.
	empty := collect(mkDeps(`<html><body><a href="/about">about</a></body></html>`))
	if v, ok := empty["by_ext"]; !ok {
		t.Fatalf("by_ext отсутствует при пустом сборе: %v", empty)
	} else if m, ok := v.(map[string]any); !ok || len(m) != 0 {
		t.Fatalf("by_ext при пустом сборе = %v (тип %T), хочу пустой объект", v, v)
	}

	// Контроль непустого прогона: фикс не должен выкинуть поле и там.
	rich := collect(mkDeps(`<a href="/dump.sql">d</a>`))
	v, ok := rich["by_ext"]
	if !ok {
		t.Fatalf("by_ext отсутствует при непустом сборе: %v", rich)
	}
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("by_ext при непустом сборе = %T, хочу объект", v)
	}
	if m["sql"] == nil {
		t.Fatalf("by_ext при непустом сборе без sql: %v", m)
	}
	if saved, _ := rich["saved"].(float64); saved != 1 {
		t.Fatalf("saved = %v, хочу 1: контрольный прогон обязан записать файл", rich["saved"])
	}
}
