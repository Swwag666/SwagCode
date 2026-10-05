package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"

	"voidsearchswag/internal/catalog"
	"voidsearchswag/internal/httpc"
)

// failingFetcher отвечает ошибкой на любой запрос: stubFetcher из tools_test.go
// всегда отдаёт 200, поэтому отказ хоста на нём не воспроизвести.
type failingFetcher struct{}

func (failingFetcher) Fetch(ctx context.Context, r httpc.Request) (*httpc.Response, error) {
	return nil, errors.New("хост недоступен")
}

// callToolArgs вызывает инструмент с аргументами и разбирает его JSON-ответ.
func callToolArgs(t *testing.T, c *client.Client, name string, args map[string]any) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: name, Arguments: args},
	})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("%s вернул ошибку инструмента: %s", name, textOf(t, res))
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(textOf(t, res)), &out); err != nil {
		t.Fatalf("%s: ответ не JSON: %v", name, err)
	}
	return out
}

func TestCollectFilesReportsUnreadableCatalog(t *testing.T) {
	// Инструмент собирает файлы и в конце добавляет к ответу состояние каталога.
	// Прежняя версия читала его так:
	//
	//     total, bytes, byExt, ferr := d.Store.FileStats(ctx)
	//     if ferr == nil { out["catalog_total"] = ... }
	//
	// При ошибке три ключа просто исчезали, и клиент получал ответ без признака
	// того, что статистика не прочитана. Случай достижим ровно потому, что сбор
	// терпит незаписанные файлы (этап 67 считает их в skipped), а итог каталога
	// читается уже после - на сломанной таблице сбор «проходит», статистика нет.
	dir := t.TempDir()
	st := exportStore(t, dir)
	seedMCPBase(t, st, dir)
	dropMCPTable(t, dir, "file_catalog")

	col := catalog.NewCollector(&stubFetcher{body: `<a href="/dump.sql">d</a>`}, st, nil, nil,
		catalog.Config{PerHostDelay: time.Millisecond})
	srv := New(Deps{Version: "test", Store: st, Collector: col, Started: time.Now()})
	c, err := client.NewInProcessClient(srv)
	if err != nil {
		t.Fatalf("клиент: %v", err)
	}
	defer c.Close()
	initClient(t, c)

	out := callToolArgs(t, c, "collect_files", map[string]any{"hosts": []any{"abcdefghijklmnop.onion"}})
	if _, ok := out["catalog_total"]; ok {
		t.Error("catalog_total присутствует при нечитаемом каталоге: число взято не из базы")
	}
	requireProblem(t, out, "файлы")

	// Заодно проверяется связка с этапом 67: файл не записан, и это сказано.
	if skipped, _ := out["skipped"].(float64); skipped < 1 {
		t.Errorf("skipped = %v, ожидала не меньше 1: незаписанный файл не учтён", out["skipped"])
	}
}

func TestFileSearchCarriesCatalogStats(t *testing.T) {
	// Здоровая база обязана отдавать состояние каталога, а правка не должна
	// превратить ключи в постоянно отсутствующие.
	dir := t.TempDir()
	st := exportStore(t, dir)
	seedMCPBase(t, st, dir)

	srv := New(Deps{Version: "test", Store: st, Started: time.Now()})
	c, err := client.NewInProcessClient(srv)
	if err != nil {
		t.Fatalf("клиент: %v", err)
	}
	defer c.Close()
	initClient(t, c)

	out := callToolArgs(t, c, "file_search", map[string]any{"query": "book"})
	if total, _ := out["catalog_total"].(float64); total != 2 {
		t.Errorf("catalog_total = %v, ожидала 2", out["catalog_total"])
	}
	if b, _ := out["catalog_bytes"].(float64); b != 2048+4096 {
		t.Errorf("catalog_bytes = %v, ожидала %d", out["catalog_bytes"], 2048+4096)
	}
	if byExt, ok := out["by_ext"].(map[string]any); !ok || byExt["epub"] != float64(1) {
		t.Errorf("by_ext = %v, ожидала epub=1", out["by_ext"])
	}
	if _, ok := out["errors"]; ok {
		t.Errorf("здоровая база не должна давать errors: %v", out["errors"])
	}
}

func TestOnionSearchCarriesPoolStats(t *testing.T) {
	dir := t.TempDir()
	st := exportStore(t, dir)
	seedMCPBase(t, st, dir)

	srv := New(Deps{Version: "test", Store: st, Started: time.Now()})
	c, err := client.NewInProcessClient(srv)
	if err != nil {
		t.Fatalf("клиент: %v", err)
	}
	defer c.Close()
	initClient(t, c)

	out := callToolArgs(t, c, "onion_search", map[string]any{"query": "a.onion"})
	if total, _ := out["pool_total"].(float64); total != 1 {
		t.Errorf("pool_total = %v, ожидала 1", out["pool_total"])
	}
	if live, _ := out["pool_live"].(float64); live != 1 {
		t.Errorf("pool_live = %v, ожидала 1", out["pool_live"])
	}
	if _, ok := out["errors"]; ok {
		t.Errorf("здоровая база не должна давать errors: %v", out["errors"])
	}
}

func TestCollectFilesReportsFailedHosts(t *testing.T) {
	// Хост, до которого не удалось добраться, обязан быть виден в ответе.
	// До правки в out не было ни failed, ни skipped, поэтому saved=0 читался
	// одинаково и для «ссылок на файлы не нашлось», и для «все хосты легли»,
	// и для «запись в базу не удалась».
	dir := t.TempDir()
	st := exportStore(t, dir)
	seedMCPBase(t, st, dir)

	col := catalog.NewCollector(failingFetcher{}, st, nil, nil,
		catalog.Config{PerHostDelay: time.Millisecond})
	srv := New(Deps{Version: "test", Store: st, Collector: col, Started: time.Now()})
	c, err := client.NewInProcessClient(srv)
	if err != nil {
		t.Fatalf("клиент: %v", err)
	}
	defer c.Close()
	initClient(t, c)

	out := callToolArgs(t, c, "collect_files", map[string]any{"hosts": []any{"127.0.0.1:1"}})
	if failed, _ := out["failed"].(float64); failed != 1 {
		t.Errorf("failed = %v, ожидала 1: отказавший хост не виден клиенту", out["failed"])
	}
	if hosts, _ := out["hosts"].(float64); hosts != 1 {
		t.Errorf("hosts = %v, ожидала 1", out["hosts"])
	}
	if saved, _ := out["saved"].(float64); saved != 0 {
		t.Errorf("saved = %v, ожидала 0", out["saved"])
	}
	if skipped, ok := out["skipped"]; !ok {
		t.Error("в ответе нет ключа skipped: счётчик этапа 67 не доходит до MCP-клиента")
	} else if n, _ := skipped.(float64); n != 0 {
		t.Errorf("skipped = %v, ожидала 0: незаписанных файлов не было", skipped)
	}
	// База здорова, поэтому состояние каталога обязано остаться на месте.
	if total, _ := out["catalog_total"].(float64); total != 2 {
		t.Errorf("catalog_total = %v, ожидала 2", out["catalog_total"])
	}
	if _, ok := out["errors"]; ok {
		t.Errorf("здоровая база не должна давать errors: %v", out["errors"])
	}
}
