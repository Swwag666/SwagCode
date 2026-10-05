package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"voidsearchswag/internal/hunt"
	"voidsearchswag/internal/parser"
	"voidsearchswag/internal/store"
)

// Инструмент parse отдавал healed=true и note с советом перегенерировать
// селекторы, когда сама база селекторов была недоступна. Совет уводил от
// причины: селекторы в порядке, не читается база. Замер на фикстуре с
// сохранённым .price показывает разницу числами: на открытой базе value берётся
// стратегией css со confidence 1, после закрытия той же базы - structural со
// confidence 0.5, и до правки ответ не содержал ни одного признака деградации.
func TestParseToolWarnsWhenSelectorsUnreadable(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/warn.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	body := `<html><body><div class="price">100 ₽</div></body></html>`
	p := &parser.Parser{Store: st, Fetch: stubParserFetch{body: body}}
	if err := p.Save(ctx, "example.com", "price", ".price", "css"); err != nil {
		t.Fatalf("Save: %v", err)
	}

	h := &hunt.Runner{Store: st, Search: func(context.Context, string, string, int) (hunt.SearchOutcome, error) {
		return hunt.SearchOutcome{URLs: []string{"http://a.onion/x"}}, nil
	}}
	srv := New(Deps{Version: "test", Store: st, Parser: p, Hunter: h, Started: time.Now()})
	c := inProcess(t, srv)
	initClient(t, c)

	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	out := callParse(t, ctx, c)
	warn, ok := out["warning"].(string)
	if !ok || warn == "" {
		t.Fatalf("в ответе нет предупреждения о недоступных селекторах: %+v", out)
	}
	if !strings.Contains(warn, "example.com") {
		t.Errorf("в предупреждении нет хоста: %q", warn)
	}
	if !strings.Contains(warn, "разбор без них") {
		t.Errorf("предупреждение не объясняет последствие: %q", warn)
	}
	fields, ok := out["fields"].(map[string]any)
	if !ok {
		t.Fatalf("поля потеряны: %+v", out)
	}
	price, ok := fields["price"].(map[string]any)
	if !ok {
		t.Fatalf("нет поля price: %+v", fields)
	}
	if price["value"] != "100 ₽" {
		t.Errorf("значение потеряно вместе с деградацией: %+v", price)
	}
	if price["strategy"] == "css" {
		t.Errorf("точный селектор применён на закрытой базе: %+v", price)
	}
}

// Обратная сторона: на живой базе ключа warning нет, иначе потребитель перестанет
// отличать настоящую потерю селекторов от штатного разбора.
func TestParseToolHasNoWarningWhenSelectorsReadable(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/clean.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	body := `<html><body><div class="price">100 ₽</div></body></html>`
	p := &parser.Parser{Store: st, Fetch: stubParserFetch{body: body}}
	if err := p.Save(ctx, "example.com", "price", ".price", "css"); err != nil {
		t.Fatalf("Save: %v", err)
	}

	h := &hunt.Runner{Store: st, Search: func(context.Context, string, string, int) (hunt.SearchOutcome, error) {
		return hunt.SearchOutcome{URLs: []string{"http://a.onion/x"}}, nil
	}}
	srv := New(Deps{Version: "test", Store: st, Parser: p, Hunter: h, Started: time.Now()})
	c := inProcess(t, srv)
	initClient(t, c)

	out := callParse(t, ctx, c)
	if warn, ok := out["warning"]; ok {
		t.Errorf("ключ warning присутствует при живой базе: %v", warn)
	}
	fields := out["fields"].(map[string]any)
	price := fields["price"].(map[string]any)
	if price["strategy"] != "css" {
		t.Errorf("точный разбор не состоялся на живой базе: %+v", price)
	}
}

// callParse вызывает инструмент parse и разбирает его JSON-ответ.
func callParse(t *testing.T, ctx context.Context, c interface {
	CallTool(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error)
}) map[string]any {
	t.Helper()
	res, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "parse", Arguments: map[string]any{
			"url": "http://example.com/x", "fields": []any{"price"},
		}},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("parse вернул ошибку: %+v", res.Content)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(textOf(t, res)), &out); err != nil {
		t.Fatalf("разбор ответа: %v", err)
	}
	return out
}
