package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"

	"voidsearchswag/internal/catalog"
	"voidsearchswag/internal/discover"
	"voidsearchswag/internal/httpc"
	"voidsearchswag/internal/store"
)

// Этап 163. Живой замер ДО (смоук трёх независимых агентов, v0.1.0 c00d86b):
// discover_onions держал HTTP-вызов 224 секунды и вернул 890KB JSON
// (12151 адресов в поле addresses) - ни потолка времени, ни пагинации;
// collect_files шёл 4m05s без объяснений; у promote_engines трёхминутный
// бюджет не был виден в описании. Клиент curl убит по 180s, сервер
// продолжал работать вслепую.

type quietLog struct{}

func (quietLog) Infof(string, ...any) {}

// addrPage - страница с пятью уникальными v2-адресами: этого хватает,
// чтобы пагинация была видна без сети.
const addrPage = `<html><body>` +
	`<a href="http://aaaaaaaaaaaaaaaa.onion/">a</a>` +
	`<a href="http://bbbbbbbbbbbbbbbb.onion/">b</a>` +
	`<a href="http://cccccccccccccccc.onion/">c</a>` +
	`<a href="http://dddddddddddddddd.onion/">d</a>` +
	`<a href="http://eeeeeeeeeeeeeeee.onion/">e</a>` +
	`</body></html>`

// slowSource не успевает ответить за срок вызова: источник жив, но
// медленный - именно так выглядит живой каталог под нагрузкой.
type slowSource struct{ every time.Duration }

func (s *slowSource) Fetch(ctx context.Context, r httpc.Request) (*httpc.Response, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(s.every):
	}
	return &httpc.Response{Status: 200, Body: []byte(addrPage)}, nil
}

// slowHostFetcher - сборщик файлов, чей хост не успевает ответить.
type slowHostFetcher struct{ every time.Duration }

func (f *slowHostFetcher) Fetch(ctx context.Context, r httpc.Request) (*httpc.Response, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(f.every):
	}
	return &httpc.Response{Status: 200, Body: []byte(`<a href="/x.pdf">x</a>`)}, nil
}

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/mcp.db")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st
}

func discoverPool(t *testing.T, fetcher interface {
	Fetch(context.Context, httpc.Request) (*httpc.Response, error)
}) *discover.Pool {
	t.Helper()
	return &discover.Pool{
		Store:  openTestStore(t),
		Finder: &discover.Finder{Client: fetcher, Log: quietLog{}, Attempts: 1},
	}
}

func newClient(t *testing.T, d Deps) *client.Client {
	t.Helper()
	c, err := client.NewInProcessClient(New(d))
	if err != nil {
		t.Fatalf("клиент: %v", err)
	}
	initClient(t, c)
	return c
}

func TestDiscoverAddressesPaginated(t *testing.T) {
	pool := discoverPool(t, &stubFetcher{body: addrPage})
	c := newClient(t, Deps{Version: "test", Store: pool.Store, Discover: pool, Started: time.Now()})
	defer c.Close()

	out := callToolLong(t, c, "discover_onions", map[string]any{"crawl": false, "max_addresses": 2})
	if got, want := anyLen(out["addresses"]), 2; got != want {
		t.Fatalf("адресов в странице %d, хочу %d", got, want)
	}
	if total, _ := out["addresses_total"].(float64); total != 5 {
		t.Errorf("addresses_total = %v, хочу 5", out["addresses_total"])
	}
	if off, _ := out["addresses_offset"].(float64); off != 0 {
		t.Errorf("addresses_offset = %v, хочу 0", out["addresses_offset"])
	}
	if trunc, _ := out["truncated"].(bool); !trunc {
		t.Error("truncated не выставлен при обрезанной странице")
	}

	out2 := callToolLong(t, c, "discover_onions", map[string]any{"crawl": false, "max_addresses": 2, "offset": 2})
	if got := anyLen(out2["addresses"]); got != 2 {
		t.Fatalf("вторая страница пуста: %d", got)
	}
	first, _ := out["addresses"].([]any)
	second, _ := out2["addresses"].([]any)
	if len(first) > 0 && len(second) > 0 && first[0] == second[0] {
		t.Errorf("offset=2 вернул ту же страницу: %v vs %v", first, second)
	}

	out3 := callToolLong(t, c, "discover_onions", map[string]any{"crawl": false, "offset": 100})
	if got := anyLen(out3["addresses"]); got != 0 {
		t.Errorf("offset за границей вернул адреса: %v", out3["addresses"])
	}
	if trunc, _ := out3["truncated"].(bool); trunc {
		t.Error("offset за границей помечен как обрезка, хотя это пустая страница")
	}
}

func TestDiscoverTimeoutCutsRun(t *testing.T) {
	pool := discoverPool(t, &slowSource{every: 30 * time.Second})
	c := newClient(t, Deps{Version: "test", Store: pool.Store, Discover: pool, Started: time.Now()})
	defer c.Close()

	start := time.Now()
	out := callToolLong(t, c, "discover_onions", map[string]any{"crawl": false, "timeout": 5})
	elapsed := time.Since(start)
	if elapsed > 12*time.Second {
		t.Fatalf("вызов длился %s при таймауте 5s", elapsed)
	}
	if hit, _ := out["timeout_hit"].(bool); !hit {
		t.Errorf("timeout_hit не выставлен: %v", out["timeout_hit"])
	}
	note, _ := out["timeout_note"].(string)
	if !strings.Contains(note, "обрезан") {
		t.Errorf("timeout_note не объясняет обрезку: %q", note)
	}
}

func TestCollectTimeoutCutsRun(t *testing.T) {
	st := openTestStore(t)
	collector := catalog.NewCollector(&slowHostFetcher{every: 30 * time.Second}, st, nil, nil, catalog.Config{})
	c := newClient(t, Deps{Version: "test", Store: st, Collector: collector, Started: time.Now()})
	defer c.Close()

	start := time.Now()
	out := callToolLong(t, c, "collect_files", map[string]any{"hosts": []any{"abcdefghijklmnop.onion"}, "timeout": 5})
	elapsed := time.Since(start)
	if elapsed > 12*time.Second {
		t.Fatalf("вызов длился %s при таймауте 5s", elapsed)
	}
	if hit, _ := out["timeout_hit"].(bool); !hit {
		t.Errorf("timeout_hit не выставлен: %v", out["timeout_hit"])
	}
	if failed, _ := out["failed"].(float64); failed < 1 {
		t.Errorf("обрезанный хост не посчитан отказом: %v", out["failed"])
	}
}

// dualHostFetcher отвечает мгновенно на быстрый хост и висит до отмены на
// медленном: сбор обрезается по сроку, но файлы быстрого хоста уже
// собраны и обязаны доехать до каталога.
type dualHostFetcher struct{}

func (f *dualHostFetcher) Fetch(ctx context.Context, r httpc.Request) (*httpc.Response, error) {
	if strings.Contains(r.URL, "aaaaaaaaaaaaaaaa") {
		return &httpc.Response{Status: 200, Body: []byte(`<a href="/fast.pdf">x</a>`)}, nil
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(30 * time.Second):
	}
	return &httpc.Response{Status: 200, Body: []byte(`<a href="/slow.pdf">x</a>`)}, nil
}

func TestCollectSavesWhatWasCollectedBeforeCut(t *testing.T) {
	st := openTestStore(t)
	collector := catalog.NewCollector(&dualHostFetcher{}, st, nil, nil, catalog.Config{})
	c := newClient(t, Deps{Version: "test", Store: st, Collector: collector, Started: time.Now()})
	defer c.Close()

	out := callToolLong(t, c, "collect_files", map[string]any{
		"hosts":   []any{"aaaaaaaaaaaaaaaa.onion", "bbbbbbbbbbbbbbbb.onion"},
		"timeout": 5,
	})
	if hit, _ := out["timeout_hit"].(bool); !hit {
		t.Fatalf("сбор не обрезан по сроку: timeout_hit=%v", out["timeout_hit"])
	}
	if saved, _ := out["saved"].(float64); saved < 1 {
		t.Fatalf("собранное до обрезки не доехало до базы: saved=%v, весь отчёт: %v", out["saved"], out)
	}
	if failed, _ := out["failed"].(float64); failed < 1 {
		t.Errorf("медленный хост не посчитан отказом: failed=%v", out["failed"])
	}
}
func TestLongCallToolsDescribeBudgets(t *testing.T) {
	c := newClient(t, Deps{Version: "test", Started: time.Now()})
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	tools := map[string]mcp.Tool{}
	for _, t2 := range res.Tools {
		tools[t2.Name] = t2
	}

	disc := tools["discover_onions"]
	if disc.Name == "" {
		t.Fatal("discover_onions не найден")
	}
	for _, p := range []string{"timeout", "offset", "max_addresses"} {
		if _, ok := disc.InputSchema.Properties[p]; !ok {
			t.Errorf("у discover_onions нет параметра %s", p)
		}
	}
	if !strings.Contains(disc.Description, "timeout") {
		t.Errorf("описание discover_onions не объясняет бюджет времени: %q", disc.Description)
	}
	coll := tools["collect_files"]
	if coll.Name == "" {
		t.Fatal("collect_files не найден")
	}
	if _, ok := coll.InputSchema.Properties["timeout"]; !ok {
		t.Error("у collect_files нет параметра timeout")
	}
	if !strings.Contains(coll.Description, "timeout") {
		t.Errorf("описание collect_files не объясняет бюджет времени: %q", coll.Description)
	}
	prom := tools["promote_engines"]
	if prom.Name == "" {
		t.Fatal("promote_engines не найден")
	}
	if !strings.Contains(strings.ToLower(prom.Description), "бюджет") {
		t.Errorf("описание promote_engines не объясняет бюджет: %q", prom.Description)
	}
}

// callToolLong - как callToolArgs, но с запасом под таймаут-тесты: вызов
// с бюджетом 5s укладывается и в сетевые повторы, и в медленный тестовый
// CI, а 30s у callToolArgs режут легитимные ожидания этого этапа.
func anyLen(v any) int {
	if v == nil {
		return 0
	}
	if s, ok := v.([]any); ok {
		return len(s)
	}
	return -1
}

func callToolLong(t *testing.T, c *client.Client, name string, args map[string]any) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
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
