package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"voidsearchswag/internal/httpc"
	"voidsearchswag/internal/metrics"
	"voidsearchswag/internal/promote"
	"voidsearchswag/internal/search"
	"voidsearchswag/internal/searchers"
	"voidsearchswag/internal/store"
)

// engineStub отдаёт страницу с поисковой формой на / и onion-ссылки на
// остальных путях: минимальный "поисковик" для промоута без tor.
func engineStub() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Write([]byte(`<html><body>
<form action="/s" method="get"><input type="text" name="q"></form>
</body></html>`))
			return
		}
		w.Write([]byte(`<html><body>
<a href="http://a1a1a1a1a1a1a1a1.onion/1">one has text here</a>
<a href="http://b2b2b2b2b2b2b2b2.onion/2">two has text here</a>
</body></html>`))
	}
}

func promoteDeps(t *testing.T, handler http.HandlerFunc) (*store.Store, *search.Engine, *promote.Promoter) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	st, err := store.Open(t.TempDir() + "/promo.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	cl, err := httpc.NewClient(context.Background(), httpc.Options{Transport: "direct", Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cl.Close() })

	eng := &search.Engine{
		Client: cl,
		Onion:  &searchers.OnionCatalog{},
		Health: searchers.NewHealthPool(cl, nil),
	}
	p := &promote.Promoter{Client: cl, MinOnionHits: 2, Timeout: 10 * time.Second}
	t.Cleanup(func() {})
	_ = srv
	return st, eng, p
}

func TestPromoteToolFindsEngine(t *testing.T) {
	srv := httptest.NewServer(engineStub())
	defer srv.Close()

	st, eng, p := promoteDeps(t, engineStub())
	mcpSrv := New(Deps{Version: "test", Store: st, Search: eng, Promoter: p, Started: time.Now()})
	c := inProcess(t, mcpSrv)
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := st.UpsertOnion(ctx, store.Onion{URL: srv.URL + "/", Status: "live"}); err != nil {
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
		t.Errorf("проверено %+v", out)
	}
	promoted, _ := out["promoted"].([]any)
	if len(promoted) != 1 {
		t.Fatalf("поднято %+v", out)
	}
	entry := promoted[0].(map[string]any)
	if entry["onion_hits"] != float64(2) {
		t.Errorf("запись неверна: %+v", entry)
	}
	// Движок прицеплен к живому каталогу и лежит в базе сидов.
	if len(eng.Onion.Engines) != 1 {
		t.Errorf("каталог не пополнен: %d", len(eng.Onion.Engines))
	}
	seeds, err := st.ListEngineSeeds(ctx)
	if err != nil || len(seeds) != 1 {
		t.Errorf("сид не сохранён: %+v %v", seeds, err)
	}
	// Повторный прогон - дубликата нет: адрес уже движок.
	res, err = c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "promote_engines", Arguments: map[string]any{"limit": 5}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var out2 map[string]any
	if err := json.Unmarshal([]byte(textOf(t, res)), &out2); err != nil {
		t.Fatal(err)
	}
	if len(out2["promoted"].([]any)) != 0 {
		t.Errorf("дубликат поднят повторно: %+v", out2)
	}
}

func TestPromoteToolNeedsDeps(t *testing.T) {
	srv := New(Deps{Version: "test", Started: time.Now()})
	c := inProcess(t, srv)
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "promote_engines", Arguments: map[string]any{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Error("promote без зависимостей обязан вернуть ошибку")
	}
}

func TestMetricsTool(t *testing.T) {
	st, eng, _ := promoteDeps(t, engineStub())
	eng.Metrics = metrics.New()
	eng.Metrics.Inc("search_total")
	srv := New(Deps{Version: "test", Store: st, Search: eng, Started: time.Now()})
	c := inProcess(t, srv)
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "metrics", Arguments: map[string]any{}},
	})
	if err != nil || res.IsError {
		t.Fatalf("metrics: %v %+v", err, res)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(textOf(t, res)), &out); err != nil {
		t.Fatal(err)
	}
	if out["search_total"] != float64(1) {
		t.Errorf("счётчик потерян: %+v", out)
	}
}

func TestBackupTool(t *testing.T) {
	st, eng, _ := promoteDeps(t, engineStub())
	dir := t.TempDir()
	srv := New(Deps{Version: "test", Store: st, Search: eng, BackupDir: dir, BackupKeep: 3, Started: time.Now()})
	c := inProcess(t, srv)
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "backup_create", Arguments: map[string]any{}},
	})
	if err != nil || res.IsError {
		t.Fatalf("backup: %v %+v", err, res)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(textOf(t, res)), &out); err != nil {
		t.Fatal(err)
	}
	if out["snapshot"] == nil || out["snapshot"] == "" {
		t.Errorf("снимок не создан: %+v", out)
	}
}

func TestBackupToolNeedsDir(t *testing.T) {
	st, eng, _ := promoteDeps(t, engineStub())
	srv := New(Deps{Version: "test", Store: st, Search: eng, Started: time.Now()})
	c := inProcess(t, srv)
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "backup_create", Arguments: map[string]any{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Error("backup без каталога обязан вернуть ошибку")
	}
}
