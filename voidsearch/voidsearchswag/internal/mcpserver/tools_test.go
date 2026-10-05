package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"

	"voidsearchswag/internal/catalog"
	"voidsearchswag/internal/discover"
	"voidsearchswag/internal/httpc"
	"voidsearchswag/internal/netx"
	"voidsearchswag/internal/router"
	"voidsearchswag/internal/search"
	"voidsearchswag/internal/searchers"
	"voidsearchswag/internal/store"
)

func TestToolsListContainsAll(t *testing.T) {
	_, c := newTestServer(t)
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	got := map[string]mcp.Tool{}
	for _, tool := range res.Tools {
		got[tool.Name] = tool
	}
	for _, want := range []string{"status", "search", "route", "onion_health", "fetch", "hunt_watch", "promote_engines", "metrics", "backup_create", "judge_submit", "peer_list", "peer_sync"} {
		if _, ok := got[want]; !ok {
			names := make([]string, 0, len(got))
			for n := range got {
				names = append(names, n)
			}
			t.Fatalf("инструмент %q не найден, есть: %v", want, names)
		}
	}
	searchTool := got["search"]
	props := searchTool.InputSchema.Properties
	if _, ok := props["query"]; !ok {
		t.Error("у search нет параметра query")
	}
	required := searchTool.InputSchema.Required
	foundReq := false
	for _, r := range required {
		if r == "query" {
			foundReq = true
		}
	}
	if !foundReq {
		t.Errorf("query не помечен обязательным: %v", required)
	}
}

func TestRouteToolDetectsModes(t *testing.T) {
	_, c := newTestServer(t)
	initClient(t, c)
	cases := map[string]router.Mode{
		"leak dump database": router.ModeDeep,
		"weather in london":  router.ModeFast,
		"avito search":       router.ModeStealth,
	}
	for q, want := range cases {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		res, err := c.CallTool(ctx, mcp.CallToolRequest{
			Params: mcp.CallToolParams{Name: "route", Arguments: map[string]any{"query": q}},
		})
		cancel()
		if err != nil {
			t.Fatalf("route(%q): %v", q, err)
		}
		if res.IsError {
			t.Fatalf("route(%q) вернул ошибку: %+v", q, res.Content)
		}
		var d router.Decision
		if err := json.Unmarshal([]byte(textOf(t, res)), &d); err != nil {
			t.Fatalf("route(%q) не JSON: %v", q, err)
		}
		if d.Mode != want {
			t.Errorf("route(%q)=%s, ожидала %s", q, d.Mode, want)
		}
		if d.Reason == "" {
			t.Errorf("route(%q): пустая причина", q)
		}
	}
}

func TestRouteToolRequiresQuery(t *testing.T) {
	_, c := newTestServer(t)
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "route", Arguments: map[string]any{}},
	})
	if err == nil && (res == nil || !res.IsError) {
		t.Error("route без query должен давать ошибку")
	}
}

func TestSearchToolWithoutEngine(t *testing.T) {
	_, c := newTestServer(t)
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "search", Arguments: map[string]any{"query": "test"}},
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if !res.IsError {
		t.Error("search без движка должен вернуть ошибку инструмента")
	}
}

func TestSearchToolRejectsBadMode(t *testing.T) {
	eng, st := testEngineWithStub(t)
	srv := New(Deps{Version: "test", Store: st, Search: eng, Started: time.Now()})
	c, err := client.NewInProcessClient(srv)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "search", Arguments: map[string]any{
			"query": "x", "mode": "bogus",
		}},
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if !res.IsError {
		t.Error("мусорный режим должен давать ошибку инструмента")
	}
	if !strings.Contains(textOf(t, res), "bogus") {
		t.Errorf("в ошибке нет имени режима: %s", textOf(t, res))
	}
}

func TestSearchToolReturnsResults(t *testing.T) {
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
			"query": "test query", "mode": "fast", "limit": 5,
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
	if out.Query != "test query" {
		t.Errorf("query=%q", out.Query)
	}
	if out.Count != 2 {
		t.Errorf("count=%d, ожидала 2", out.Count)
	}
	if out.Mode != router.ModeFast {
		t.Errorf("mode=%s", out.Mode)
	}
}

func TestSearchToolCachesSecondCall(t *testing.T) {
	eng, st := testEngineWithStub(t)
	srv := New(Deps{Version: "test", Store: st, Search: eng, Started: time.Now()})
	c, err := client.NewInProcessClient(srv)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	initClient(t, c)

	call := func() search.Outcome {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		res, err := c.CallTool(ctx, mcp.CallToolRequest{
			Params: mcp.CallToolParams{Name: "search", Arguments: map[string]any{
				"query": "cached query", "mode": "fast",
			}},
		})
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		var out search.Outcome
		if err := json.Unmarshal([]byte(textOf(t, res)), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	first := call()
	if first.Cached {
		t.Error("первый вызов не должен быть из кэша")
	}
	second := call()
	if !second.Cached {
		t.Error("второй вызов должен прийти из кэша")
	}
	if second.Count != first.Count {
		t.Errorf("из кэша count=%d, ожидала %d", second.Count, first.Count)
	}
}

func TestSearchToolNoCacheFlag(t *testing.T) {
	eng, st := testEngineWithStub(t)
	srv := New(Deps{Version: "test", Store: st, Search: eng, Started: time.Now()})
	c, err := client.NewInProcessClient(srv)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	initClient(t, c)

	run := func(noCache bool) search.Outcome {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		res, _ := c.CallTool(ctx, mcp.CallToolRequest{
			Params: mcp.CallToolParams{Name: "search", Arguments: map[string]any{
				"query": "nocache query", "mode": "fast", "no_cache": noCache,
			}},
		})
		var out search.Outcome
		if err := json.Unmarshal([]byte(textOf(t, res)), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	run(false)
	second := run(true)
	if second.Cached {
		t.Error("no_cache=true должен обходить кэш")
	}
}

func TestOnionHealthToolWithoutEngines(t *testing.T) {
	eng, st := testEngineWithStub(t)
	srv := New(Deps{Version: "test", Store: st, Search: eng, Started: time.Now()})
	c, err := client.NewInProcessClient(srv)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "onion_health", Arguments: map[string]any{}},
	})
	if err != nil {
		t.Fatalf("onion_health: %v", err)
	}
	if res.IsError {
		t.Fatalf("onion_health вернул ошибку: %+v", res.Content)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(textOf(t, res)), &out); err != nil {
		t.Fatal(err)
	}
	if _, ok := out["engines"]; !ok {
		t.Error("в ответе нет engines")
	}
	if out["probed"] != false {
		t.Errorf("probed=%v, ожидала false", out["probed"])
	}
}

func TestFetchToolRequiresURL(t *testing.T) {
	eng, st := testEngineWithStub(t)
	srv := New(Deps{Version: "test", Store: st, Search: eng, Started: time.Now()})
	c, err := client.NewInProcessClient(srv)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "fetch", Arguments: map[string]any{}},
	})
	if err == nil && (res == nil || !res.IsError) {
		t.Error("fetch без url должен давать ошибку")
	}
}

func TestStatusReportsSearchEngines(t *testing.T) {
	eng, st := testEngineWithStub(t)
	srv := New(Deps{Version: "test", Store: st, Search: eng, Started: time.Now()})
	c, err := client.NewInProcessClient(srv)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "status", Arguments: map[string]any{}},
	})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(textOf(t, res)), &out); err != nil {
		t.Fatal(err)
	}
	sc, ok := out["search"].(map[string]any)
	if !ok {
		t.Fatalf("нет блока search: %v", out["search"])
	}
	if _, ok := sc["onion_engines"]; !ok {
		t.Error("в search нет onion_engines")
	}
}

func TestToolsListIncludesDiscovery(t *testing.T) {
	_, c := newTestServer(t)
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	got := map[string]mcp.Tool{}
	for _, tool := range res.Tools {
		got[tool.Name] = tool
	}
	for _, want := range []string{"discover_onions", "pool_status"} {
		if _, ok := got[want]; !ok {
			names := make([]string, 0, len(got))
			for n := range got {
				names = append(names, n)
			}
			t.Fatalf("инструмент %q не найден, есть: %v", want, names)
		}
	}
	if _, ok := got["discover_onions"].InputSchema.Properties["depth"]; !ok {
		t.Error("у discover_onions нет параметра depth")
	}
	if _, ok := got["discover_onions"].InputSchema.Properties["crawl"]; !ok {
		t.Error("у discover_onions нет параметра crawl")
	}
	// Этап 178 (смоук-H): основной имя параметра фильтра - status_filter, как
	// его зовёт текст описания. Прежняя схема принимала только status, и
	// фильтр по описанному имени молча не применялся.
	if _, ok := got["pool_status"].InputSchema.Properties["status_filter"]; !ok {
		t.Error("у pool_status нет параметра status_filter")
	}
}

func TestDiscoverWithoutPoolErrors(t *testing.T) {
	_, st := testEngineWithStub(t)
	srv := New(Deps{Version: "test", Store: st, Started: time.Now()})
	c, err := client.NewInProcessClient(srv)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "discover_onions", Arguments: map[string]any{}},
	})
	if err != nil {
		t.Fatalf("discover_onions: %v", err)
	}
	if !res.IsError {
		t.Error("без discovery-слоя инструмент должен вернуть ошибку, а не пустой успех")
	}
	if !strings.Contains(textOf(t, res), "discovery") {
		t.Errorf("ошибка не объясняет причину: %s", textOf(t, res))
	}
}

func TestPoolStatusEmpty(t *testing.T) {
	_, st := testEngineWithStub(t)
	srv := New(Deps{Version: "test", Store: st, Started: time.Now()})
	c, err := client.NewInProcessClient(srv)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "pool_status", Arguments: map[string]any{}},
	})
	if err != nil {
		t.Fatalf("pool_status: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(textOf(t, res)), &out); err != nil {
		t.Fatal(err)
	}
	if _, ok := out["total"]; !ok {
		t.Errorf("нет total в отчёте: %v", out)
	}
	if _, ok := out["entries"]; !ok {
		t.Errorf("нет entries в отчёте: %v", out)
	}
}

func TestPoolStatusListsEntries(t *testing.T) {
	_, st := testEngineWithStub(t)
	ctx := context.Background()
	for _, o := range []store.Onion{
		{URL: "abcdefghijklmnop.onion", Status: "live", Category: "market"},
		{URL: "qrstuvwxyz234567.onion", Status: "dead"},
		{URL: "bcdefghijklmnopq.onion", Status: "unknown"},
	} {
		if err := st.UpsertOnion(ctx, o); err != nil {
			t.Fatal(err)
		}
	}

	srv := New(Deps{Version: "test", Store: st, Started: time.Now()})
	c, err := client.NewInProcessClient(srv)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	initClient(t, c)

	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	res, err := c.CallTool(cctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "pool_status", Arguments: map[string]any{}},
	})
	if err != nil {
		t.Fatalf("pool_status: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(textOf(t, res)), &out); err != nil {
		t.Fatal(err)
	}
	if total, _ := out["total"].(float64); int(total) != 3 {
		t.Errorf("total=%v, ожидала 3", out["total"])
	}
	if live, _ := out["live"].(float64); int(live) != 1 {
		t.Errorf("live=%v, ожидала 1", out["live"])
	}
	byStatus, ok := out["by_status"].(map[string]any)
	if !ok {
		t.Fatalf("нет разбивки по статусам: %v", out["by_status"])
	}
	if n, _ := byStatus["live"].(float64); int(n) != 1 {
		t.Errorf("live в разбивке = %v, ожидала 1", byStatus["live"])
	}
	if n, _ := byStatus["dead"].(float64); int(n) != 1 {
		t.Errorf("dead в разбивке = %v, ожидала 1", byStatus["dead"])
	}
	if n, _ := byStatus["unknown"].(float64); int(n) != 1 {
		t.Errorf("unknown в разбивке = %v, ожидала 1", byStatus["unknown"])
	}
	entries, ok := out["entries"].([]any)
	if !ok || len(entries) != 3 {
		t.Errorf("entries=%v, ожидала 3 записи", out["entries"])
	}
}

func TestPoolStatusFiltersByStatus(t *testing.T) {
	_, st := testEngineWithStub(t)
	ctx := context.Background()
	for _, o := range []store.Onion{
		{URL: "abcdefghijklmnop.onion", Status: "live"},
		{URL: "qrstuvwxyz234567.onion", Status: "dead"},
	} {
		if err := st.UpsertOnion(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	srv := New(Deps{Version: "test", Store: st, Started: time.Now()})
	c, err := client.NewInProcessClient(srv)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	initClient(t, c)

	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	res, err := c.CallTool(cctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "pool_status", Arguments: map[string]any{"status": "live"}},
	})
	if err != nil {
		t.Fatalf("pool_status: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(textOf(t, res)), &out); err != nil {
		t.Fatal(err)
	}
	entries, _ := out["entries"].([]any)
	if len(entries) != 1 {
		t.Fatalf("фильтр не сработал: %d записей", len(entries))
	}
	e := entries[0].(map[string]any)
	if e["status"] != "live" {
		t.Errorf("вернулся не тот статус: %v", e["status"])
	}
}

func TestPoolStatusWithoutStoreErrors(t *testing.T) {
	srv := New(Deps{Version: "test", Started: time.Now()})
	c, err := client.NewInProcessClient(srv)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "pool_status", Arguments: map[string]any{}},
	})
	if err != nil {
		t.Fatalf("pool_status: %v", err)
	}
	if !res.IsError {
		t.Error("без базы инструмент должен вернуть ошибку")
	}
}

func TestToolsListIncludesProbe(t *testing.T) {
	_, c := newTestServer(t)
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	var probe *mcp.Tool
	for i := range res.Tools {
		if res.Tools[i].Name == "probe_pool" {
			probe = &res.Tools[i]
		}
	}
	if probe == nil {
		t.Fatal("инструмент probe_pool не найден")
	}
	if _, ok := probe.InputSchema.Properties["limit"]; !ok {
		t.Error("у probe_pool нет параметра limit")
	}
	if _, ok := probe.InputSchema.Properties["addr"]; !ok {
		t.Error("у probe_pool нет параметра addr")
	}
}

func TestProbePoolWithoutProberErrors(t *testing.T) {
	_, st := testEngineWithStub(t)
	srv := New(Deps{Version: "test", Store: st, Started: time.Now()})
	c, err := client.NewInProcessClient(srv)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "probe_pool", Arguments: map[string]any{}},
	})
	if err != nil {
		t.Fatalf("probe_pool: %v", err)
	}
	if !res.IsError {
		t.Error("без проб инструмент должен вернуть ошибку")
	}
}

func TestProbePoolSingleAddress(t *testing.T) {
	_, st := testEngineWithStub(t)
	ctx := context.Background()
	if err := st.UpsertOnion(ctx, store.Onion{URL: "abcdefghijklmnop.onion"}); err != nil {
		t.Fatal(err)
	}
	prober := discover.NewProber(nil, st, nil, discover.ProbeConfig{})
	srv := New(Deps{Version: "test", Store: st, Prober: prober, Started: time.Now()})
	c, err := client.NewInProcessClient(srv)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	initClient(t, c)

	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	res, err := c.CallTool(cctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "probe_pool",
			Arguments: map[string]any{"addr": "abcdefghijklmnop.onion"},
		},
	})
	if err != nil {
		t.Fatalf("probe_pool: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(textOf(t, res)), &out); err != nil {
		t.Fatal(err)
	}
	if total, _ := out["total"].(float64); int(total) != 1 {
		t.Errorf("total=%v, ожидала 1", out["total"])
	}
	if reps, ok := out["results"].([]any); !ok || len(reps) != 1 {
		t.Fatalf("results=%v, ожидала 1 запись", out["results"])
	}
}

// Этап 176: волна строится по очереди охвата, а не только по unknown.
// Прежний контракт (2 неизвестных, «живой в выборку не входит») держал пул
// замороженным: волна не трогала проверенные записи, пока есть
// непроверенные. Теперь live входит в волну после unknown - по старшинству
// last_probe. Живой BEFORE-факт на витрине: limit=5 при 3 unknown вернул
// total=3 - семь записей пула волна не видела.
func TestProbePoolWaveUsesPool(t *testing.T) {
	_, st := testEngineWithStub(t)
	ctx := context.Background()
	for _, u := range []string{"abcdefghijklmnop.onion", "qrstuvwxyz234567.onion"} {
		if err := st.UpsertOnion(ctx, store.Onion{URL: u, Status: "unknown"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.UpsertOnion(ctx, store.Onion{URL: "bcdefghijklmnopq.onion", Status: "live"}); err != nil {
		t.Fatal(err)
	}
	prober := discover.NewProber(nil, st, nil, discover.ProbeConfig{})
	srv := New(Deps{Version: "test", Store: st, Prober: prober, Started: time.Now()})
	c, err := client.NewInProcessClient(srv)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	initClient(t, c)

	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	res, err := c.CallTool(cctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "probe_pool", Arguments: map[string]any{"limit": 10}},
	})
	if err != nil {
		t.Fatalf("probe_pool: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(textOf(t, res)), &out); err != nil {
		t.Fatal(err)
	}
	if total, _ := out["total"].(float64); int(total) != 3 {
		t.Errorf("проверено %v, ожидала 3: очередь охвата несёт и проверенный live после unknown", out["total"])
	}
}

func TestProbePoolEmptyPoolSucceeds(t *testing.T) {
	_, st := testEngineWithStub(t)
	prober := discover.NewProber(nil, st, nil, discover.ProbeConfig{})
	srv := New(Deps{Version: "test", Store: st, Prober: prober, Started: time.Now()})
	c, err := client.NewInProcessClient(srv)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "probe_pool", Arguments: map[string]any{}},
	})
	if err != nil {
		t.Fatalf("probe_pool: %v", err)
	}
	if res.IsError {
		t.Error("пустой пул - не ошибка, а пустая волна")
	}
}

func TestFileSearchTool(t *testing.T) {
	_, st := testEngineWithStub(t)
	ctx := context.Background()
	for _, f := range []store.FileEntry{
		{URL: "http://a.onion/dump.sql", Filename: "dump.sql", Ext: "sql", Size: 9000},
		{URL: "http://a.onion/readme.txt", Filename: "readme.txt", Ext: "txt", Size: 100},
	} {
		if err := st.AddFile(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	srv := New(Deps{Version: "test", Store: st, Started: time.Now()})
	c, err := client.NewInProcessClient(srv)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	initClient(t, c)

	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	res, err := c.CallTool(cctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "file_search", Arguments: map[string]any{"ext": "sql"}},
	})
	if err != nil {
		t.Fatalf("file_search: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(textOf(t, res)), &out); err != nil {
		t.Fatal(err)
	}
	files, ok := out["files"].([]any)
	if !ok || len(files) != 1 {
		t.Fatalf("files=%v, ожидала 1 запись", out["files"])
	}
	if total, _ := out["catalog_total"].(float64); int(total) != 2 {
		t.Errorf("catalog_total=%v, ожидала 2", out["catalog_total"])
	}
	if bytes, _ := out["catalog_bytes"].(float64); int64(bytes) != 9100 {
		t.Errorf("catalog_bytes=%v, ожидала 9100", out["catalog_bytes"])
	}
}

func TestFileSearchWithoutStoreErrors(t *testing.T) {
	srv := New(Deps{Version: "test", Started: time.Now()})
	c, err := client.NewInProcessClient(srv)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "file_search", Arguments: map[string]any{}},
	})
	if err != nil {
		t.Fatalf("file_search: %v", err)
	}
	if !res.IsError {
		t.Error("без базы инструмент должен вернуть ошибку")
	}
}

func TestFileSearchEmptyCatalog(t *testing.T) {
	_, st := testEngineWithStub(t)
	srv := New(Deps{Version: "test", Store: st, Started: time.Now()})
	c, err := client.NewInProcessClient(srv)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "file_search", Arguments: map[string]any{}},
	})
	if err != nil {
		t.Fatalf("file_search: %v", err)
	}
	if res.IsError {
		t.Error("пустой каталог - не ошибка")
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(textOf(t, res)), &out); err != nil {
		t.Fatal(err)
	}
	if returned, _ := out["returned"].(float64); int(returned) != 0 {
		t.Errorf("returned=%v, ожидала 0", out["returned"])
	}
}

func TestCollectFilesWithoutCollectorErrors(t *testing.T) {
	_, st := testEngineWithStub(t)
	srv := New(Deps{Version: "test", Store: st, Started: time.Now()})
	c, err := client.NewInProcessClient(srv)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "collect_files", Arguments: map[string]any{}},
	})
	if err != nil {
		t.Fatalf("collect_files: %v", err)
	}
	if !res.IsError {
		t.Error("без сборщика инструмент должен вернуть ошибку")
	}
}

func TestCollectFilesEmptyPoolErrors(t *testing.T) {
	_, st := testEngineWithStub(t)
	col := catalog.NewCollector(&stubFetcher{}, st, nil, nil, catalog.Config{})
	srv := New(Deps{Version: "test", Store: st, Collector: col, Started: time.Now()})
	c, err := client.NewInProcessClient(srv)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "collect_files", Arguments: map[string]any{}},
	})
	if err != nil {
		t.Fatalf("collect_files: %v", err)
	}
	if !res.IsError {
		t.Error("на пустом пуле инструмент должен сказать про пул, а не молчать")
	}
}

func TestCollectFilesExplicitHost(t *testing.T) {
	_, st := testEngineWithStub(t)
	f := &stubFetcher{body: `<a href="/dump.sql">d</a><a href="/x.zip">z</a>`}
	col := catalog.NewCollector(f, st, nil, nil, catalog.Config{PerHostDelay: time.Millisecond})
	srv := New(Deps{Version: "test", Store: st, Collector: col, Started: time.Now()})
	c, err := client.NewInProcessClient(srv)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	initClient(t, c)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "collect_files",
			Arguments: map[string]any{"hosts": []any{"abcdefghijklmnop.onion"}},
		},
	})
	if err != nil {
		t.Fatalf("collect_files: %v", err)
	}
	if res.IsError {
		t.Fatalf("сбор вернул ошибку: %s", textOf(t, res))
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(textOf(t, res)), &out); err != nil {
		t.Fatal(err)
	}
	if saved, _ := out["saved"].(float64); int(saved) != 2 {
		t.Errorf("saved=%v, ожидала 2", out["saved"])
	}
	if total, _ := out["catalog_total"].(float64); int(total) != 2 {
		t.Errorf("catalog_total=%v, ожидала 2", out["catalog_total"])
	}
	if taskID, _ := out["task_id"].(string); taskID == "" {
		t.Error("task_id не возвращён")
	}
}

// Этап 165, жалоба смоук-агента: второй collect_files отчитал saved и
// task_id, но file_search по этому task_id вернул ноль - файлы уже были в
// каталоге, происхождение не переносится. Ответ обязан нести revisited,
// иначе агент читает saved как свои новые файлы.
func TestCollectFilesReportsRevisited(t *testing.T) {
	_, st := testEngineWithStub(t)
	f := &stubFetcher{body: `<a href="/dump.sql">d</a><a href="/x.zip">z</a>`}
	col := catalog.NewCollector(f, st, nil, nil, catalog.Config{PerHostDelay: time.Millisecond})
	srv := New(Deps{Version: "test", Store: st, Collector: col, Started: time.Now()})
	c, err := client.NewInProcessClient(srv)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	initClient(t, c)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	call := func() map[string]any {
		res, err := c.CallTool(ctx, mcp.CallToolRequest{
			Params: mcp.CallToolParams{
				Name:      "collect_files",
				Arguments: map[string]any{"hosts": []any{"abcdefghijklmnop.onion"}},
			},
		})
		if err != nil {
			t.Fatalf("collect_files: %v", err)
		}
		if res.IsError {
			t.Fatalf("сбор вернул ошибку: %s", textOf(t, res))
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(textOf(t, res)), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	first := call()
	if rev, ok := first["revisited"].(float64); !ok || int(rev) != 0 {
		t.Errorf("первый сбор: revisited=%v, хочу 0", first["revisited"])
	}
	if saved, _ := first["saved"].(float64); int(saved) != 2 {
		t.Fatalf("первый сбор: saved=%v, хочу 2", first["saved"])
	}

	second := call()
	if saved, _ := second["saved"].(float64); int(saved) != 0 {
		t.Errorf("повторный сбор: saved=%v, хочу 0", second["saved"])
	}
	if rev, ok := second["revisited"].(float64); !ok || int(rev) != 2 {
		t.Errorf("повторный сбор: revisited=%v, хочу 2 - файлы известны", second["revisited"])
	}
}

func TestCollectFilesUsesPoolWhenNoHosts(t *testing.T) {
	_, st := testEngineWithStub(t)
	ctx := context.Background()
	if err := st.UpsertOnion(ctx, store.Onion{URL: "abcdefghijklmnop.onion", Status: "live"}); err != nil {
		t.Fatal(err)
	}
	f := &stubFetcher{body: `<a href="/a.txt">a</a>`}
	col := catalog.NewCollector(f, st, nil, nil, catalog.Config{PerHostDelay: time.Millisecond})
	srv := New(Deps{Version: "test", Store: st, Collector: col, Started: time.Now()})
	c, err := client.NewInProcessClient(srv)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	initClient(t, c)

	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	res, err := c.CallTool(cctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "collect_files", Arguments: map[string]any{}},
	})
	if err != nil {
		t.Fatalf("collect_files: %v", err)
	}
	if res.IsError {
		t.Fatalf("сбор по пулу вернул ошибку: %s", textOf(t, res))
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(textOf(t, res)), &out); err != nil {
		t.Fatal(err)
	}
	if hosts, _ := out["hosts"].(float64); int(hosts) != 1 {
		t.Errorf("hosts=%v, ожидала 1 (живой из пула)", out["hosts"])
	}
}

func TestToolsListIncludesCollect(t *testing.T) {
	_, c := newTestServer(t)
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	var found bool
	for _, t2 := range res.Tools {
		if t2.Name == "collect_files" {
			found = true
			if _, ok := t2.InputSchema.Properties["hosts"]; !ok {
				t.Error("у collect_files нет параметра hosts")
			}
		}
	}
	if !found {
		t.Error("инструмент collect_files не найден")
	}
}

// stubFetcher отдаёт заданное тело на GET. Нужен инструментам, которые
// работают поверх сборщика: сети в тестах нет.
type stubFetcher struct {
	body string
}

func (s *stubFetcher) Fetch(ctx context.Context, r httpc.Request) (*httpc.Response, error) {
	if r.Method == http.MethodHead {
		return &httpc.Response{Status: 200, Header: http.Header{}}, nil
	}
	return &httpc.Response{Status: 200, Body: []byte(s.body)}, nil
}

func testEngineWithStub(t *testing.T) (*search.Engine, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/mcp.db")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	eng := &search.Engine{
		Store:    st,
		DDG:      []searchers.Searcher{&stubSearcher{}},
		Log:      nil,
		CacheTTL: time.Hour,
		DefaultN: 20,
	}
	return eng, st
}

type stubSearcher struct{}

func (s *stubSearcher) Name() string { return "stub" }

func (s *stubSearcher) Search(_ context.Context, q string, limit int) ([]searchers.Result, error) {
	res := []searchers.Result{
		{Title: "First " + q, URL: "https://one.example/a"},
		{Title: "Second " + q, URL: "https://two.example/b"},
	}
	if limit > 0 && len(res) > limit {
		res = res[:limit]
	}
	return res, nil
}

var _ netx.Rotator = fakeRotator{}
