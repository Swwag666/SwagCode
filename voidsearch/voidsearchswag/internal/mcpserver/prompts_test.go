package mcpserver

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

func TestPromptsListed(t *testing.T) {
	_, c := newTestServer(t)
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := c.ListPrompts(ctx, mcp.ListPromptsRequest{})
	if err != nil {
		t.Fatalf("prompts/list: %v", err)
	}
	want := map[string]bool{
		"recon": false, "investigate": false, "harvest": false,
		"compare": false, "health": false, "extract": false, "watch": false,
		"judge": false,
	}
	for _, p := range res.Prompts {
		if _, ok := want[p.Name]; ok {
			want[p.Name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("промпт %q не зарегистрирован", name)
		}
	}
}

func TestPromptReconSubstitutes(t *testing.T) {
	_, c := newTestServer(t)
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := c.GetPrompt(ctx, mcp.GetPromptRequest{
		Params: mcp.GetPromptParams{
			Name:      "recon",
			Arguments: map[string]string{"topic": "leak database", "depth": "2"},
		},
	})
	if err != nil {
		t.Fatalf("prompts/get: %v", err)
	}
	body := promptText(t, res)
	for _, want := range []string{"leak database", "depth=2", "discover_onions", "probe_pool", "onion_search"} {
		if !strings.Contains(body, want) {
			t.Errorf("в промпте нет %q:\n%s", want, body)
		}
	}
}

func TestPromptReconRequiresTopic(t *testing.T) {
	_, c := newTestServer(t)
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := c.GetPrompt(ctx, mcp.GetPromptRequest{
		Params: mcp.GetPromptParams{Name: "recon", Arguments: map[string]string{}},
	})
	if err == nil {
		t.Error("без topic промпт должен вернуть ошибку")
	}
}

func TestPromptReconDefaultDepth(t *testing.T) {
	_, c := newTestServer(t)
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := c.GetPrompt(ctx, mcp.GetPromptRequest{
		Params: mcp.GetPromptParams{
			Name:      "recon",
			Arguments: map[string]string{"topic": "x"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(promptText(t, res), "depth=1") {
		t.Error("глубина по умолчанию не подставлена")
	}
}

func TestPromptInvestigateNormalisesHost(t *testing.T) {
	_, c := newTestServer(t)
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := c.GetPrompt(ctx, mcp.GetPromptRequest{
		Params: mcp.GetPromptParams{
			Name:      "investigate",
			Arguments: map[string]string{"host": "abcdefghijklmnop.onion"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	body := promptText(t, res)
	if !strings.Contains(body, "http://abcdefghijklmnop.onion") {
		t.Errorf("схема не подставлена:\n%s", body)
	}
	if !strings.Contains(body, "collect_files") {
		t.Error("в промпте нет шага сбора файлов")
	}
}

func TestPromptInvestigateKeepsScheme(t *testing.T) {
	_, c := newTestServer(t)
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := c.GetPrompt(ctx, mcp.GetPromptRequest{
		Params: mcp.GetPromptParams{
			Name:      "investigate",
			Arguments: map[string]string{"host": "http://example.com"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(promptText(t, res), "http://http://") {
		t.Error("схема продублирована")
	}
}

func TestPromptHarvestWithExt(t *testing.T) {
	_, c := newTestServer(t)
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := c.GetPrompt(ctx, mcp.GetPromptRequest{
		Params: mcp.GetPromptParams{
			Name:      "harvest",
			Arguments: map[string]string{"ext": "zip", "limit": "5"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	body := promptText(t, res)
	if !strings.Contains(body, `ext="zip"`) {
		t.Errorf("фильтр по расширению не подставлен:\n%s", body)
	}
	if !strings.Contains(body, "limit=5") {
		t.Error("лимит не подставлен")
	}
}

func TestPromptHarvestWithoutExt(t *testing.T) {
	_, c := newTestServer(t)
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := c.GetPrompt(ctx, mcp.GetPromptRequest{
		Params: mcp.GetPromptParams{Name: "harvest", Arguments: map[string]string{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	body := promptText(t, res)
	if !strings.Contains(body, "file_search без фильтра") {
		t.Errorf("без ext должен быть шаг без фильтра:\n%s", body)
	}
	if !strings.Contains(body, "limit=10") {
		t.Error("лимит по умолчанию не подставлен")
	}
}

func TestPromptCompareSubstitutes(t *testing.T) {
	_, c := newTestServer(t)
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := c.GetPrompt(ctx, mcp.GetPromptRequest{
		Params: mcp.GetPromptParams{
			Name:      "compare",
			Arguments: map[string]string{"query": "tor market"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	body := promptText(t, res)
	for _, want := range []string{"tor market", "mode=fast", "mode=deep", "mode=stealth"} {
		if !strings.Contains(body, want) {
			t.Errorf("в промпте нет %q", want)
		}
	}
}

func TestPromptCompareRequiresQuery(t *testing.T) {
	_, c := newTestServer(t)
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := c.GetPrompt(ctx, mcp.GetPromptRequest{
		Params: mcp.GetPromptParams{Name: "compare", Arguments: map[string]string{}},
	})
	if err == nil {
		t.Error("без query промпт должен вернуть ошибку")
	}
}

func TestPromptHealth(t *testing.T) {
	_, c := newTestServer(t)
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := c.GetPrompt(ctx, mcp.GetPromptRequest{
		Params: mcp.GetPromptParams{Name: "health", Arguments: map[string]string{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	body := promptText(t, res)
	for _, want := range []string{"status", "onion_health", "pool_status", "file_search"} {
		if !strings.Contains(body, want) {
			t.Errorf("в промпте нет %q", want)
		}
	}
}

func TestPromptUnknownName(t *testing.T) {
	_, c := newTestServer(t)
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := c.GetPrompt(ctx, mcp.GetPromptRequest{
		Params: mcp.GetPromptParams{Name: "nope"},
	})
	if err == nil {
		t.Error("неизвестный промпт должен вернуть ошибку")
	}
}

func TestPromptNamesCalledTools(t *testing.T) {
	_, c := newTestServer(t)
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tools, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	known := map[string]bool{}
	for _, tl := range tools.Tools {
		known[tl.Name] = true
	}

	prompts, err := c.ListPrompts(ctx, mcp.ListPromptsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range prompts.Prompts {
		args := map[string]string{}
		for _, a := range p.Arguments {
			if a.Required {
				args[a.Name] = "x"
			}
		}
		res, err := c.GetPrompt(ctx, mcp.GetPromptRequest{
			Params: mcp.GetPromptParams{Name: p.Name, Arguments: args},
		})
		if err != nil {
			t.Fatalf("промпт %q: %v", p.Name, err)
		}
		body := promptText(t, res)
		for _, name := range promptToolMentions(body) {
			if !known[name] {
				t.Errorf("промпт %q ссылается на несуществующий инструмент %q", p.Name, name)
			}
		}
	}
}

// promptToolMentions вытаскивает из текста промпта упоминания инструментов.
// Промпт, зовущий инструмент, которого нет, ломает сценарий на первом шаге,
// поэтому соответствие имён проверяется тестом.
func promptToolMentions(body string) []string {
	names := []string{
		"status", "search", "route", "onion_health", "fetch",
		"discover_onions", "pool_status", "probe_pool", "onion_search",
		"file_search", "collect_files",
		"parse", "build_parser", "save_selector", "classify_source",
		"hunt_create", "hunt_list", "hunt_run", "hunt_watch", "hunt_hits", "stats",
		"judge_submit", "promote_engines", "metrics", "backup_create",
		"peer_list", "peer_sync",
	}
	var out []string
	for _, n := range names {
		if strings.Contains(body, n) {
			out = append(out, n)
		}
	}
	return out
}

func promptText(t *testing.T, res *mcp.GetPromptResult) string {
	t.Helper()
	var b strings.Builder
	for _, m := range res.Messages {
		if tc, ok := m.Content.(mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

var _ = client.NewInProcessClient
