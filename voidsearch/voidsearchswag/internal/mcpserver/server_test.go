package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"

	"voidsearchswag/internal/store"
)

func newTestServer(t *testing.T) (*store.Store, *client.Client) {
	t.Helper()
	return newTestServerInDir(t, t.TempDir())
}

// newTestServerInDir поднимает сервер на базе в указанном каталоге.
//
// Каталог передаётся снаружи, а не создаётся внутри, потому что тестам
// неполной сводки нужен путь к файлу базы: сломать схему через публичный API
// store нельзя, поле db не экспортируется, поэтому таблица удаляется отдельным
// соединением.
func newTestServerInDir(t *testing.T, dir string) (*store.Store, *client.Client) {
	t.Helper()
	st, err := store.Open(dir + "/mcp.db")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	srv := New(Deps{Version: "test", Store: st, Started: time.Now()})
	c, err := client.NewInProcessClient(srv)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return st, c
}

func initClient(t *testing.T, c *client.Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := c.Initialize(ctx, mcp.InitializeRequest{
		Params: mcp.InitializeParams{
			ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
			ClientInfo:      mcp.Implementation{Name: "test", Version: "0"},
		},
	})
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
}

func TestInitializeHandshake(t *testing.T) {
	_, c := newTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := c.Initialize(ctx, mcp.InitializeRequest{
		Params: mcp.InitializeParams{
			ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
			ClientInfo:      mcp.Implementation{Name: "test", Version: "0"},
		},
	})
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if res.ServerInfo.Name != "voidsearchswag" {
		t.Errorf("serverInfo.name = %q, ожидала voidsearchswag", res.ServerInfo.Name)
	}
	if res.ServerInfo.Version != "test" {
		t.Errorf("serverInfo.version = %q, ожидала test", res.ServerInfo.Version)
	}
	if res.Capabilities.Tools == nil {
		t.Error("capabilities.tools не объявлены")
	}
	if res.Instructions == "" {
		t.Error("instructions пусты")
	}
}

func TestToolsList(t *testing.T) {
	_, c := newTestServer(t)
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	found := false
	for _, tool := range res.Tools {
		if tool.Name == "status" {
			found = true
			if tool.Description == "" {
				t.Error("у status пустое описание")
			}
		}
	}
	if !found {
		names := make([]string, 0, len(res.Tools))
		for _, tool := range res.Tools {
			names = append(names, tool.Name)
		}
		t.Fatalf("инструмент status не найден, есть: %v", names)
	}
}

func TestStatusToolWithoutTor(t *testing.T) {
	st, c := newTestServer(t)
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := st.UpsertOnion(ctx, store.Onion{URL: "http://a.onion", Status: "live"}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertOnion(ctx, store.Onion{URL: "http://b.onion", Status: "dead"}); err != nil {
		t.Fatal(err)
	}

	res, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "status", Arguments: map[string]any{}},
	})
	if err != nil {
		t.Fatalf("tools/call status: %v", err)
	}
	if res.IsError {
		t.Fatalf("status вернул ошибку: %+v", res.Content)
	}
	text := textOf(t, res)
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("ответ не JSON: %v\n%s", err, text)
	}
	if out["version"] != "test" {
		t.Errorf("version=%v, ожидала test", out["version"])
	}
	if _, ok := out["uptime"]; !ok {
		t.Error("в ответе нет uptime")
	}
	tr, ok := out["transport"].(map[string]any)
	if !ok {
		t.Fatalf("нет блока transport: %v", out["transport"])
	}
	if tr["kind"] != "none" {
		t.Errorf("transport.kind=%v, ожидала none (tor не передан)", tr["kind"])
	}
	db, ok := out["db"].(map[string]any)
	if !ok {
		t.Fatalf("нет блока db: %v", out["db"])
	}
	if db["onion_total"] != float64(2) {
		t.Errorf("onion_total=%v, ожидала 2", db["onion_total"])
	}
	if db["onion_live"] != float64(1) {
		t.Errorf("onion_live=%v, ожидала 1", db["onion_live"])
	}
}

func TestStatusToolWithRotator(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/mcp.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv := New(Deps{Version: "test", Store: st, Rot: fakeRotator{}, Started: time.Now()})
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
	tr := out["transport"].(map[string]any)
	if tr["kind"] != "tor" {
		t.Errorf("kind=%v, ожидала tor", tr["kind"])
	}
	if tr["healthy"] != true {
		t.Errorf("healthy=%v, ожидала true", tr["healthy"])
	}
	if !strings.Contains(tr["spec"].(string), "socks5://") {
		t.Errorf("spec=%v, ожидала socks5-схему", tr["spec"])
	}
}

func TestStatusToolWithoutStore(t *testing.T) {
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
		Params: mcp.CallToolParams{Name: "status", Arguments: map[string]any{}},
	})
	if err != nil {
		t.Fatalf("status без store: %v", err)
	}
	if res.IsError {
		t.Fatalf("status без store вернул ошибку: %+v", res.Content)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(textOf(t, res)), &out); err != nil {
		t.Fatal(err)
	}
	if _, ok := out["db"]; ok {
		t.Error("без store не должно быть блока db")
	}
}

func TestUnknownToolReturnsError(t *testing.T) {
	_, c := newTestServer(t)
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "no_such_tool", Arguments: map[string]any{}},
	})
	if err == nil && (res == nil || !res.IsError) {
		t.Error("неизвестный инструмент не дал ошибку")
	}
}

func textOf(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	var sb strings.Builder
	for _, item := range res.Content {
		if tc, ok := item.(mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	if sb.Len() == 0 {
		t.Fatalf("в ответе нет текста: %+v", res.Content)
	}
	return sb.String()
}

type fakeRotator struct{}

func (fakeRotator) Kind() string                 { return "tor" }
func (fakeRotator) TransportSpec() string        { return "socks5://127.0.0.1:9050" }
func (fakeRotator) Rotate(context.Context) error { return nil }
func (fakeRotator) Healthy() bool                { return true }
func (fakeRotator) Close() error                 { return nil }
