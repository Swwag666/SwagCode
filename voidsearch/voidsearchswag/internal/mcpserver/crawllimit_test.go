package mcpserver

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"

	"voidsearchswag/internal/discover"
	"voidsearchswag/internal/httpc"
)

// Этап 172. Под-отчёт crawl нёс одно поле limit_hit на два разных
// события: «достигнут потолок хостов» и «контекст отменён», причём
// проверка отмены стояла первой на входе в слой и затирала уже
// поставленный предел. Слепой прогон этапа 171 (агент B, max_hosts=1000)
// получил crawled=1000 из 1000 при limit_hit «контекст отменён»: потолок
// достигнут, а ответ потерял событие. Здесь проверяется, что поле
// cancelled доезжает до MCP-клиента и не смешивается с limit_hit.

// slowCancelPage - тела для медленного обхода: каждый GET спит, а ссылки
// ведут дальше по цепочке, чтобы очередь ссылок оставалась непустой, пока
// бюджет вызова не истечёт.
var slowCancelPage = map[string]string{
	"http://aaaaaaaaaaaaaaaa.onion/": `<a href="http://bbbbbbbbbbbbbbbb.onion/">b</a><a href="http://cccccccccccccccc.onion/">c</a>`,
	"http://bbbbbbbbbbbbbbbb.onion/": `<a href="http://cccccccccccccccc.onion/">c</a>`,
	"http://cccccccccccccccc.onion/": `<a href="http://dddddddddddddddd.onion/">d</a>`,
	"http://dddddddddddddddd.onion/": `<a href="http://eeeeeeeeeeeeeeee.onion/">e</a>`,
}

// slowFetcher держит каждый GET дольше бюджета вызова: слой, начатый до
// истечения, успевает доработать, а вход в следующий слой видит отмену.
type slowFetcher struct {
	body map[string]string
}

func (s *slowFetcher) Fetch(ctx context.Context, r httpc.Request) (*httpc.Response, error) {
	if r.Method == http.MethodHead {
		return &httpc.Response{Status: 200}, nil
	}
	time.Sleep(4 * time.Second)
	body, ok := s.body[r.URL]
	if !ok {
		return &httpc.Response{Status: 404}, nil
	}
	return &httpc.Response{Status: 200, Body: []byte(body)}, nil
}

func cancelPool(t *testing.T) *client.Client {
	t.Helper()
	finder := &stubFetcher{body: `<a href="http://aaaaaaaaaaaaaaaa.onion/">a</a><a href="http://bbbbbbbbbbbbbbbb.onion/">b</a>`}
	pool := discoverPool(t, finder)
	pool.Crawl = discover.NewCrawler(&slowFetcher{body: slowCancelPage}, quietLog{}, discover.CrawlConfig{
		Depth:        3,
		MaxHosts:     10,
		PerHostDelay: time.Millisecond,
	})
	c := newClient(t, Deps{Version: "test", Store: pool.Store, Discover: pool, Started: time.Now()})
	t.Cleanup(func() { c.Close() })
	return c
}

func TestDiscoverCrawlCancelledThroughMCP(t *testing.T) {
	c := cancelPool(t)

	out := callToolLong(t, c, "discover_onions", map[string]any{
		"crawl":   true,
		"depth":   3,
		"timeout": 5,
	})
	crawl, _ := out["crawl"].(map[string]any)
	if crawl == nil {
		t.Fatalf("crawl-отчёта нет: %v", out)
	}
	cancelled, _ := crawl["cancelled"].(bool)
	if !cancelled {
		t.Errorf("обход обрезан таймаутом 5s при страницах по 4s, а crawl.cancelled=%v: отмена не доехала до MCP-клиента", crawl["cancelled"])
	}
	if hit, _ := crawl["limit_hit"].(string); hit != "" {
		t.Errorf("limit_hit=%q при hostsDone < потолка 10: отмена обязана не подменять предел", hit)
	}
	if hit, _ := out["timeout_hit"].(bool); !hit {
		t.Errorf("timeout_hit=%v, хочу true: общий бюджет вызова исчерпан", out["timeout_hit"])
	}
}

func TestDiscoverDescriptionNamesCancelRule(t *testing.T) {
	c := newClient(t, Deps{Version: "test", Started: time.Now()})
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	for _, tool := range res.Tools {
		if tool.Name == "discover_onions" {
			for _, want := range []string{
				"только достигнутый потолок хостов",
				"cancelled",
				"limit_hit и cancelled могут стоять одновременно",
				"seeds_cut",
				"вклад этого вызова",
				"отсутствие поля значит",
			} {
				if !strings.Contains(tool.Description, want) {
					t.Errorf("описание discover_onions не содержит %q: %q", want, tool.Description)
				}
			}
			return
		}
	}
	t.Fatal("discover_onions не найден")
}
