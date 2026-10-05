package mcpserver

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"voidsearchswag/internal/hunt"
	"voidsearchswag/internal/parser"
	"voidsearchswag/internal/store"
)

type stubParserFetch struct {
	body string
}

func (s stubParserFetch) FetchURL(_ context.Context, rawURL string) (parser.Response, error) {
	return parser.Response{Status: 200, Body: []byte(s.body), URL: rawURL}, nil
}

func newExtendedServer(t *testing.T, body string) (*store.Store, *parser.Parser, *hunt.Runner) {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/ext.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	p := &parser.Parser{Store: st, Fetch: stubParserFetch{body: body}}
	h := &hunt.Runner{Store: st, Search: func(context.Context, string, string, int) (hunt.SearchOutcome, error) {
		return hunt.SearchOutcome{URLs: []string{"http://a.onion/x"}}, nil
	}}
	return st, p, h
}

func inProcess(t *testing.T, srv *server.MCPServer) *client.Client {
	t.Helper()
	c, err := client.NewInProcessClient(srv)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestParseToolExact(t *testing.T) {
	_, p, h := newExtendedServer(t, `<html><head><title>Shop</title></head><body><div class="price">100 ₽</div></body></html>`)
	srv := New(Deps{Version: "test", Store: p.Store, Parser: p, Hunter: h, Started: time.Now()})
	_ = srv
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := p.Save(ctx, "example.com", "price", ".price", "css"); err != nil {
		t.Fatal(err)
	}
	c := inProcess(t, srv)
	initClient(t, c)
	res, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "parse", Arguments: map[string]any{
			"url": "http://example.com/x", "fields": []any{"price"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("parse вернул ошибку: %+v", res.Content)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(textOf(t, res)), &out); err != nil {
		t.Fatal(err)
	}
	fields := out["fields"].(map[string]any)
	if fields["price"].(map[string]any)["value"] != "100 ₽" {
		t.Errorf("parse: %+v", out)
	}
	if out["healed"] == true {
		t.Errorf("точный разбор помечен healed: %+v", out)
	}
}

func TestParseToolRequiresFields(t *testing.T) {
	_, p, h := newExtendedServer(t, `<html><body><p>x</p></body></html>`)
	srv := New(Deps{Version: "test", Store: p.Store, Parser: p, Hunter: h, Started: time.Now()})
	c := inProcess(t, srv)
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "parse", Arguments: map[string]any{"url": "http://example.com/"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Error("parse без fields обязан вернуть ошибку")
	}
}

func TestBuildParserAndSaveSelectorRoundtrip(t *testing.T) {
	_, p, h := newExtendedServer(t, `<html><head><title>T</title></head><body><h1 class="t">Hi</h1></body></html>`)
	srv := New(Deps{Version: "test", Store: p.Store, Parser: p, Hunter: h, Started: time.Now()})
	c := inProcess(t, srv)
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "build_parser", Arguments: map[string]any{"url": "http://example.com/x"}},
	})
	if err != nil || res.IsError {
		t.Fatalf("build_parser: %v %+v", err, res)
	}
	var bp map[string]any
	if err := json.Unmarshal([]byte(textOf(t, res)), &bp); err != nil {
		t.Fatal(err)
	}
	if bp["pattern"] != "example.com" {
		t.Errorf("pattern=%v", bp["pattern"])
	}
	if bp["skel"] == nil || bp["skel"] == "" {
		t.Error("скелет пуст")
	}

	res, err = c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "save_selector", Arguments: map[string]any{
			"url_pattern": "example.com", "field": "title", "selector": "h1.t",
		}},
	})
	if err != nil || res.IsError {
		t.Fatalf("save_selector: %v %+v", err, res)
	}

	res, err = c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "parse", Arguments: map[string]any{
			"url": "http://example.com/x", "fields": []any{"title"},
		}},
	})
	if err != nil || res.IsError {
		t.Fatalf("parse после save: %v %+v", err, res)
	}
	var pr map[string]any
	if err := json.Unmarshal([]byte(textOf(t, res)), &pr); err != nil {
		t.Fatal(err)
	}
	if pr["fields"].(map[string]any)["title"].(map[string]any)["value"] != "Hi" {
		t.Errorf("parse: %+v", pr)
	}
}

func TestClassifyToolPrivate(t *testing.T) {
	body := `<html><head><title>Login</title></head><body><form><input type="password"></form>sign in to continue</body></html>`
	_, p, h := newExtendedServer(t, body)
	srv := New(Deps{Version: "test", Store: p.Store, Parser: p, Hunter: h, Started: time.Now()})
	c := inProcess(t, srv)
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "classify_source", Arguments: map[string]any{"url": "https://example.com/login"}},
	})
	if err != nil || res.IsError {
		t.Fatalf("classify: %v %+v", err, res)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(textOf(t, res)), &v); err != nil {
		t.Fatal(err)
	}
	if v["type"] != "private" {
		t.Errorf("type=%v, ожидала private", v["type"])
	}
}

func TestHuntToolsLifecycle(t *testing.T) {
	_, p, h := newExtendedServer(t, `<html><body></body></html>`)
	srv := New(Deps{Version: "test", Store: p.Store, Parser: p, Hunter: h, Started: time.Now()})
	c := inProcess(t, srv)
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "hunt_create", Arguments: map[string]any{"query": "leak", "mode": "deep"}},
	})
	if err != nil || res.IsError {
		t.Fatalf("hunt_create: %v %+v", err, res)
	}
	res, err = c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "hunt_list", Arguments: map[string]any{}},
	})
	if err != nil || res.IsError {
		t.Fatalf("hunt_list: %v %+v", err, res)
	}
	var list map[string]any
	if err := json.Unmarshal([]byte(textOf(t, res)), &list); err != nil {
		t.Fatal(err)
	}
	if list["count"] != float64(1) {
		t.Errorf("hunts=%v", list)
	}
	res, err = c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "hunt_run", Arguments: map[string]any{}},
	})
	if err != nil || res.IsError {
		t.Fatalf("hunt_run: %v %+v", err, res)
	}
}

func TestHuntWatchTimeout(t *testing.T) {
	_, p, h := newExtendedServer(t, `<html><body></body></html>`)
	srv := New(Deps{Version: "test", Store: p.Store, Parser: p, Hunter: h, Started: time.Now()})
	c := inProcess(t, srv)
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "hunt_create", Arguments: map[string]any{"query": "leak"}},
	})
	if err != nil || res.IsError {
		t.Fatalf("hunt_create: %v %+v", err, res)
	}
	// Первый watch фиксирует базу...
	res, err = c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "hunt_watch", Arguments: map[string]any{"timeout_s": 2, "interval_s": 1}},
	})
	if err != nil || res.IsError {
		t.Fatalf("hunt_watch: %v %+v", err, res)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(textOf(t, res)), &out); err != nil {
		t.Fatal(err)
	}
	// ...второй ждать нечего: выдача не менялась, обязан выйти по таймауту,
	// а не висеть и не врать про находки.
	if out["timeout"] != true {
		t.Errorf("watch без смены выдачи обязан выйти по таймауту: %+v", out)
	}
}

func TestHuntWatchSeesChange(t *testing.T) {
	_, p, _ := newExtendedServer(t, `<html><body></body></html>`)
	urls := []string{"http://a.onion/x"}
	h2 := &hunt.Runner{Store: p.Store, Search: func(context.Context, string, string, int) (hunt.SearchOutcome, error) {
		return hunt.SearchOutcome{URLs: urls}, nil
	}}
	srv := New(Deps{Version: "test", Store: p.Store, Parser: p, Hunter: h2, Started: time.Now()})
	c := inProcess(t, srv)
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "hunt_create", Arguments: map[string]any{"query": "leak", "schedule_min": 1}},
	})
	if err != nil || res.IsError {
		t.Fatalf("hunt_create: %v %+v", err, res)
	}
	var created map[string]any
	if err := json.Unmarshal([]byte(textOf(t, res)), &created); err != nil {
		t.Fatal(err)
	}
	id := created["hunt_id"]

	// База фиксируется первым прогоном...
	res, err = c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "hunt_run", Arguments: map[string]any{"id": id}},
	})
	if err != nil || res.IsError {
		t.Fatalf("hunt_run: %v %+v", err, res)
	}
	// ...выдача меняется...
	urls = []string{"http://a.onion/x", "http://new.onion/y"}
	// ...watch обязан заметить смену, а не выйти по таймауту.
	res, err = c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "hunt_watch", Arguments: map[string]any{"id": id, "timeout_s": 10, "interval_s": 1}},
	})
	if err != nil || res.IsError {
		t.Fatalf("hunt_watch: %v %+v", err, res)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(textOf(t, res)), &out); err != nil {
		t.Fatal(err)
	}
	if out["timeout"] == true {
		t.Errorf("watch не заметил смену выдачи: %+v", out)
	}
}

func TestStatsTool(t *testing.T) {
	st, p, h := newExtendedServer(t, `<html><body></body></html>`)
	srv := New(Deps{Version: "test", Store: st, Parser: p, Hunter: h, Started: time.Now()})
	c := inProcess(t, srv)
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "stats", Arguments: map[string]any{}},
	})
	if err != nil || res.IsError {
		t.Fatalf("stats: %v %+v", err, res)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(textOf(t, res)), &out); err != nil {
		t.Fatal(err)
	}
	if _, ok := out["db"]; !ok {
		t.Errorf("в stats нет блока db: %+v", out)
	}
}

func TestExtendedToolsNeedDeps(t *testing.T) {
	srv := New(Deps{Version: "test", Started: time.Now()})
	c := inProcess(t, srv)
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"parse", map[string]any{"url": "http://x/", "fields": []any{"title"}}},
		{"build_parser", map[string]any{"url": "http://x/"}},
		{"classify_source", map[string]any{"url": "http://x/"}},
		{"hunt_create", map[string]any{"query": "x"}},
		{"hunt_list", map[string]any{}},
		{"hunt_run", map[string]any{}},
		{"hunt_watch", map[string]any{"timeout_s": 1, "interval_s": 1}},
	} {
		res, err := c.CallTool(ctx, mcp.CallToolRequest{
			Params: mcp.CallToolParams{Name: tc.name, Arguments: tc.args},
		})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !res.IsError {
			t.Errorf("%s без зависимостей обязан вернуть ошибку", tc.name)
		}
	}
}
