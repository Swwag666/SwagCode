package mcpserver

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"

	"voidsearchswag/internal/discover"
	"voidsearchswag/internal/httpc"
)

// Этап 174: timeout_note называет, ЧТО именно срезал срок, по фактам
// отчёта: три ветки - источники не закончились (обхода не было), обход
// обрезан отменой (crawl.cancelled), источники и обход закончились сами
// (срок ушёл на замер и запись). Прежний единый текст «источники и обход
// могли не закончиться» врал в обе стороны: смоук 172 (агент B,
// timeout=90) - crawl закончился сам за 65с, ответ обвинял обрезку;
// смоук 172 (агент A, timeout=15) - crawl был обрезан, ответ не называл
// виновника.

func noteBranch(t *testing.T, c *client.Client, args map[string]any) string {
	t.Helper()
	out := callToolLong(t, c, "discover_onions", args)
	if hit, _ := out["timeout_hit"].(bool); !hit {
		t.Fatalf("timeout_hit не выставлен, а ветка проверяется по note: %v", out["timeout_hit"])
	}
	note, _ := out["timeout_note"].(string)
	if note == "" {
		t.Fatal("timeout_note пуст")
	}
	return note
}

func TestDiscoverNoteSourcesCutWithoutCrawl(t *testing.T) {
	pool := discoverPool(t, &slowSource{every: 30 * time.Second})
	c := newClient(t, Deps{Version: "test", Store: pool.Store, Discover: pool, Started: time.Now()})
	defer c.Close()

	note := noteBranch(t, c, map[string]any{"crawl": false, "timeout": 5})
	if !strings.Contains(note, "источники могли не закончиться") {
		t.Fatalf("без обхода note обязан винить источники: %q", note)
	}
	if strings.Contains(note, "crawl.cancelled") {
		t.Errorf("note называет crawl-отмену, которой не было: %q", note)
	}
}

func TestDiscoverNoteCrawlCancelled(t *testing.T) {
	c := cancelPool(t)

	note := noteBranch(t, c, map[string]any{"crawl": true, "depth": 3, "timeout": 5})
	if !strings.Contains(note, "обход обрезан отменой (crawl.cancelled=true)") {
		t.Fatalf("crawl.cancelled в отчёте, а note не называет отмену: %q", note)
	}
	if strings.Contains(note, "закончились") {
		t.Errorf("note говорит «закончились», хотя обход обрезан: %q", note)
	}
}

// fastGetSlowHead - расклад ветки «закончились сами»: GET отдаёт страницу
// мгновенно (обход завершается за миллисекунды), а HEAD замера спит
// дольше остатка бюджета - срок умирает в замере размеров, не в обходе.
type fastGetSlowHead struct{}

func (f *fastGetSlowHead) Fetch(ctx context.Context, r httpc.Request) (*httpc.Response, error) {
	if r.Method == http.MethodHead {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(3 * time.Second):
		}
		return &httpc.Response{Status: 200}, nil
	}
	return &httpc.Response{Status: 200, Body: []byte(
		`<a href="/one.zip">1</a><a href="/two.zip">2</a><a href="/three.zip">3</a>` +
			`<a href="/four.zip">4</a><a href="/five.zip">5</a><a href="/six.zip">6</a>`)}, nil
}

func TestDiscoverNoteEverythingFinishedButMeasureAteBudget(t *testing.T) {
	pool := discoverPool(t, &stubFetcher{body: addrPage})
	// Замер сериализован (Concurrency 1): шесть HEAD по 3s держат фазу
	// дольше бюджета вызова 5s, поэтому rctx гарантированно умирает в
	// замере - при любой волне параллелизма суммарной длины > бюджета.
	pool.Crawl = discover.NewCrawler(&fastGetSlowHead{}, quietLog{}, discover.CrawlConfig{
		Depth:        1,
		MaxHosts:     5,
		Concurrency:  1,
		PerHostDelay: time.Millisecond,
	})
	c := newClient(t, Deps{Version: "test", Store: pool.Store, Discover: pool, Started: time.Now()})
	defer c.Close()

	note := noteBranch(t, c, map[string]any{"crawl": true, "seeds": "aaaaaaaaaaaaaaaa.onion", "timeout": 5})
	if !strings.Contains(note, "источники и обход закончились") {
		t.Fatalf("crawl завершился сам, а note винит обрезку: %q", note)
	}
	if strings.Contains(note, "crawl.cancelled") {
		t.Errorf("note называет отмену при самостоятельном завершении: %q", note)
	}
	if !strings.Contains(note, "size=0") {
		t.Errorf("note не объясняет судьбу недомерённых файлов: %q", note)
	}
}
