package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"voidsearchswag/internal/catalog"
	"voidsearchswag/internal/httpc"
)

// failOnHostFetcher отказывает одному хосту и отдаёт тело остальным.
// Этап 182 (смоук-181): мёртвый хост в collect_files давал failed=1 без
// причины - битый адрес и молчащий хост различались только по elapsed.
type failOnHostFetcher struct {
	body    string
	deadFor string
}

func (f *failOnHostFetcher) Fetch(ctx context.Context, r httpc.Request) (*httpc.Response, error) {
	if f.deadFor != "" && r.URL == f.deadFor {
		return nil, errors.New("socks connect: соединение отклонено")
	}
	if r.Method == http.MethodHead {
		return &httpc.Response{Status: 200, Header: http.Header{}}, nil
	}
	return &httpc.Response{Status: 200, Body: []byte(f.body)}, nil
}

// Кэтчер этапа 182: failed_hosts обязан появиться в ответе collect_files при
// отказе хоста и нести текст причины. Счётчик failed и карта согласованы:
// по каждому отказавшему хосту - ровно одна запись.
func TestCollectFailedHostsInResponse(t *testing.T) {
	dir := t.TempDir()
	st := exportStore(t, dir)
	seedMCPBase(t, st, dir)

	col := catalog.NewCollector(&failOnHostFetcher{
		body:    `<html><body><a href="/dump.sql">d</a></body></html>`,
		deadFor: "http://deadhost.onion/",
	}, st, nil, nil, catalog.Config{PerHostDelay: time.Millisecond})
	d := Deps{Version: "test", Store: st, Collector: col, Started: time.Now()}

	req := mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "collect_files",
			Arguments: map[string]any{
				"hosts":   []any{"deadhost.onion", "livehost.onion"},
				"timeout": 60,
			},
		},
	}
	res, err := d.collectFilesHandler(context.Background(), req)
	if err != nil {
		t.Fatalf("collectFilesHandler: %v", err)
	}
	tc, ok := res.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("content[0] = %T, хочу TextContent", res.Content[0])
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(tc.Text), &out); err != nil {
		t.Fatalf("разбор ответа: %v\n%s", err, tc.Text)
	}

	if failed, _ := out["failed"].(float64); failed != 1 {
		t.Fatalf("failed = %v, хочу 1: один хост из двух мёртв", out["failed"])
	}
	v, ok := out["failed_hosts"]
	if !ok {
		t.Fatalf("failed_hosts отсутствует в ответе с отказом: %v", out)
	}
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("failed_hosts = %T, хочу объект", v)
	}
	if len(m) != 1 {
		t.Fatalf("failed_hosts несёт %d записей, хочу 1: карта обязана совпадать со счётчиком failed", len(m))
	}
	reason, _ := m["deadhost.onion"].(string)
	if reason == "" {
		t.Fatalf("причина отказа пустая: %v", m)
	}
	if !jsonSafe(m) {
		t.Error("карта причин не сериализуется")
	}

	// Живой хост обязан отработать на фоне отказа соседа: сбор не валится
	// целиком из-за одного хоста.
	if saved, _ := out["saved"].(float64); saved != 1 {
		t.Fatalf("saved = %v, хочу 1: живой хост обязан записать файл", out["saved"])
	}
}

// jsonSafe - контроль сериализации карты: нестроковые ключи или значения
// упали бы уже на Unmarshal, здесь дублируем явной проверкой.
func jsonSafe(m map[string]any) bool {
	_, err := json.Marshal(m)
	return err == nil
}

// Обратный контроль: без отказов поле не появляется - пустая карта не
// засоряет ответ (omitempty), как и limit_hit при недостигнутом лимите.
func TestCollectFailedHostsAbsentWhenAllAlive(t *testing.T) {
	dir := t.TempDir()
	st := exportStore(t, dir)
	seedMCPBase(t, st, dir)

	col := catalog.NewCollector(&failOnHostFetcher{
		body: `<html><body><a href="/notes.txt">n</a></body></html>`,
	}, st, nil, nil, catalog.Config{PerHostDelay: time.Millisecond})
	d := Deps{Version: "test", Store: st, Collector: col, Started: time.Now()}

	req := mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "collect_files",
			Arguments: map[string]any{"hosts": []any{"okhost.onion"}, "timeout": 60},
		},
	}
	res, err := d.collectFilesHandler(context.Background(), req)
	if err != nil {
		t.Fatalf("collectFilesHandler: %v", err)
	}
	tc, ok := res.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("content[0] = %T, хочу TextContent", res.Content[0])
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(tc.Text), &out); err != nil {
		t.Fatalf("разбор ответа: %v\n%s", err, tc.Text)
	}
	if _, ok := out["failed_hosts"]; ok {
		t.Fatalf("failed_hosts присутствует без отказов: %v", out["failed_hosts"])
	}
}
