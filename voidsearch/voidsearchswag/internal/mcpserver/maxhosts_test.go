package mcpserver

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"

	"voidsearchswag/internal/discover"
)

// Этап 171. Живой BEFORE (стенд vss171, чистая база): discover с
// max_hosts=15 отвечал crawl.max_hosts=50 и обходил 50 страниц (15
// сидов + 35 ссылок) - параметр доходил только до среза сидов, а сам
// обход жил на серверных 50. Молчаливый вызов на сервере с потолком 7
// брал 50 сидов и лживо отчитывался limit_hit «достигнут потолок
// хостов». Здесь проверяется путь целиком: аргумент инструмента ->
// Options -> копия обходчика -> ответ.

// ceilPage - страница с двумя ссылками на соседние хосты: без потолка
// обход уходит в слой 1, с потолком 1 - останавливается на сиде.
const ceilPage = `<html><body>` +
	`<a href="http://aaaaaaaaaaaaaaaa.onion/">a</a>` +
	`<a href="http://bbbbbbbbbbbbbbbb.onion/">b</a>` +
	`</body></html>`

func ceilPool(t *testing.T) *client.Client {
	t.Helper()
	pool := discoverPool(t, &stubFetcher{body: ceilPage})
	pool.Crawl = discover.NewCrawler(&stubFetcher{body: ceilPage}, quietLog{}, discover.CrawlConfig{
		Depth:        2,
		PerHostDelay: time.Millisecond,
	})
	c := newClient(t, Deps{Version: "test", Store: pool.Store, Discover: pool, Started: time.Now()})
	t.Cleanup(func() { c.Close() })
	return c
}

func TestDiscoverMaxHostsLimitsCrawlThroughMCP(t *testing.T) {
	c := ceilPool(t)

	out := callToolLong(t, c, "discover_onions", map[string]any{
		"crawl":     true,
		"depth":     2,
		"max_hosts": 1,
		"timeout":   30,
	})
	crawl, _ := out["crawl"].(map[string]any)
	if crawl == nil {
		t.Fatalf("crawl-отчёта нет: %v", out)
	}
	mh, _ := crawl["max_hosts"].(float64)
	if mh != 1 {
		t.Errorf("запрошен max_hosts=1, отчёт назвал %v: потолок вызова не дошёл до обхода", crawl["max_hosts"])
	}
	pages, _ := crawl["pages"].(float64)
	if pages != 1 {
		t.Errorf("max_hosts=1 обошёл %v страниц, хочу 1: сид обязан срезаться потолком, а слой ссылок - не начинаться", crawl["pages"])
	}
}

func TestDiscoverDescriptionNamesMaxHostsRule(t *testing.T) {
	// Описание - подсказка следующему агенту: смоук-агенты этапа 170
	// читали «потолок обходимых хостов, по умолчанию 50» и верили, что
	// 15 ограничит обход. Без правила «не шире этого числа» и «0 -
	// конфиг» параметр читается как необязательная декорация.
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
			if tool.InputSchema.Properties == nil {
				t.Fatal("схема discover_onions без параметров")
			}
			raw, ok := tool.InputSchema.Properties["max_hosts"]
			if !ok {
				t.Fatalf("у discover_onions нет параметра max_hosts: %v", tool.InputSchema.Properties)
			}
			prop, _ := raw.(map[string]any)
			desc, _ := prop["description"].(string)
			for _, want := range []string{"не шире этого числа", "потолок из конфига сервера"} {
				if !strings.Contains(desc, want) {
					t.Errorf("описание параметра max_hosts не содержит %q: %q", want, desc)
				}
			}
			return
		}
	}
	t.Fatal("discover_onions не найден")
}
