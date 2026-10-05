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

// Этап 167. Живой замер ДО (смоук-агент C3, стенд agenthunt, e207a07):
// параллельный discover_onions crawl=true дописал в каталог 51 файл
// (2222jzj4 47, 2b5calm3 4) под task_id crawl-20261001-190804 через 92
// секунды после ответа collect_files - тот честно отчитался своим меткам,
// а дальнейший рост каталога для вызывающего был необъясним: по его
// task_id стабильно 88 из 139 новых, метки crawl-* в ответе discover не
// светились. Путь юзера к файлам один - file_search по task_id, и без
// file_task_id в ответе записи оставались невидимыми.

// crawlFilePage - страница для обхода: файловая ссылка плюс адреса, чтобы
// Finder и Crawler работали на одном подставном клиенте без сети.
const crawlFilePage = `<html><body>` +
	`<a href="/docs/report.pdf">отчёт</a>` +
	`<a href="http://aaaaaaaaaaaaaaaa.onion/">a</a>` +
	`<a href="http://bbbbbbbbbbbbbbbb.onion/">b</a>` +
	`</body></html>`

func crawlPool(t *testing.T) (*discover.Pool, *client.Client) {
	t.Helper()
	pool := discoverPool(t, &stubFetcher{body: crawlFilePage})
	pool.Crawl = discover.NewCrawler(&stubFetcher{body: crawlFilePage}, quietLog{}, discover.CrawlConfig{
		Depth:        1,
		PerHostDelay: time.Millisecond,
	})
	c := newClient(t, Deps{Version: "test", Store: pool.Store, Discover: pool, Started: time.Now()})
	t.Cleanup(func() { c.Close() })
	return pool, c
}

func TestDiscoverResponseCarriesFileTaskID(t *testing.T) {
	_, c := crawlPool(t)

	out := callToolLong(t, c, "discover_onions", map[string]any{
		"crawl":         true,
		"depth":         1,
		"max_hosts":     2,
		"max_addresses": 1,
		"timeout":       30,
	})
	tid, _ := out["file_task_id"].(string)
	if tid == "" {
		t.Fatalf("file_task_id не назван при записанных файлах: %v", out)
	}
	if !strings.HasPrefix(tid, "crawl-") {
		t.Errorf("file_task_id не похож на метку задачи обхода: %q", tid)
	}
	files, _ := out["files"].(float64)
	if files < 1 {
		t.Fatalf("файлы не записаны: files=%v, ответ: %v", out["files"], out)
	}

	// Единственный путь юзера к записям - file_search по метке из ответа.
	fs := callToolLong(t, c, "file_search", map[string]any{"task_id": tid, "limit": 100})
	ret, _ := fs["returned"].(float64)
	if ret != files {
		t.Errorf("file_search по file_task_id вернул %v при files=%v: записи невидимы", ret, files)
	}
}

func TestDiscoverRepeatCrawlReportsRevisited(t *testing.T) {
	// Повторный обход тех же хостов: происхождение первой записи не
	// переписывается, и вторая метка обязана отчитаться повторными, а не
	// «записанными», иначе files лжёт прямо в ответе.
	_, c := crawlPool(t)

	first := callToolLong(t, c, "discover_onions", map[string]any{
		"crawl": true, "depth": 1, "max_hosts": 2, "timeout": 30,
	})
	tid, _ := first["file_task_id"].(string)
	if tid == "" {
		t.Fatalf("первый прогон без file_task_id: %v", first)
	}
	files1, _ := first["files"].(float64)
	if files1 < 1 {
		t.Fatalf("первый прогон ничего не записал: %v", first)
	}

	// Пауза - страховка от грубого системного тика: метки с этапа 168 несут
	// миллисекунды, но мгновенные прогоны теста могут попасть в один тик.
	// Проверяется семантика повтора, а не гранулярность часов.
	time.Sleep(1100 * time.Millisecond)

	second := callToolLong(t, c, "discover_onions", map[string]any{
		"crawl": true, "depth": 1, "max_hosts": 2, "timeout": 30,
	})
	files2, _ := second["files"].(float64)
	rev, _ := second["revisited"].(float64)
	if files2 != 0 {
		t.Errorf("повторный обход отчитал files=%v при известных файлах", files2)
	}
	if rev != files1 {
		t.Errorf("revisited=%v, ожидала %v: повторные находки не посчитаны", rev, files1)
	}
	tid2, _ := second["file_task_id"].(string)
	if tid2 == "" || tid2 == tid {
		t.Errorf("метка повтора %q не выделена или совпала с первой %q", tid2, tid)
	}

	fs := callToolLong(t, c, "file_search", map[string]any{"task_id": tid2, "limit": 100})
	ret, _ := fs["returned"].(float64)
	if ret != 0 {
		t.Errorf("под меткой повтора %v записей: происхождение не должно переписываться", ret)
	}
	fs1 := callToolLong(t, c, "file_search", map[string]any{"task_id": tid, "limit": 100})
	ret1, _ := fs1["returned"].(float64)
	if ret1 != files1 {
		t.Errorf("под меткой первого прогона %v записей, ожидала %v", ret1, files1)
	}
}

func TestDiscoverDescriptionNamesFileTaskID(t *testing.T) {
	// Описание инструмента - подсказка следующему агенту: без фразы про
	// file_task_id путь «файлы обхода → file_search» приходится искать
	// вслепую, что и произошло со смоук-агентом C3.
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
			if !strings.Contains(tool.Description, "file_task_id") {
				t.Errorf("описание discover_onions не называет file_task_id: %q", tool.Description)
			}
			return
		}
	}
	t.Fatal("discover_onions не найден")
}
