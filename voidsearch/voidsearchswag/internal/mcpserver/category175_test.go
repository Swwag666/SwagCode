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

// Этап 175: два дефекта поля в pool_status, снятые SQL по живой базе:
// category стояла пустой строкой у каждой из 14248 записей (ключ-обещание,
// которое никогда не исполняется - сбор адресов категории не назначает), и
// success_rate/latency_avg читались как «доля успехов»/«средняя латентность»,
// хотя это сглаженные величины. Ключ category теперь появляется только
// присвоенной, описание называет семантику полей.

func TestPoolStatusCategoryPresentOnlyWhenAssigned(t *testing.T) {
	_, st := testEngineWithStub(t)
	ctx := context.Background()
	for _, o := range []store.Onion{
		{URL: "abcdefghijklmnop.onion", Status: "live", Category: "market"},
		{URL: "qrstuvwxyz234567.onion", Status: "unknown"},
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
	entries, _ := out["entries"].([]any)
	if len(entries) != 2 {
		t.Fatalf("entries=%d, ожидала 2", len(entries))
	}

	byURL := map[string]map[string]any{}
	for _, e := range entries {
		m, ok := e.(map[string]any)
		if !ok {
			t.Fatalf("запись не объект: %v", e)
		}
		byURL[m["url"].(string)] = m
	}

	withCat := byURL["abcdefghijklmnop.onion"]
	if withCat["category"] != "market" {
		t.Errorf("присвоенная категория потеряна: %v", withCat["category"])
	}
	withoutCat := byURL["qrstuvwxyz234567.onion"]
	// Отсутствие ключа - договор этапа 175: «не присвоена» читается как
	// absent, а не как пустая строка, которую не отличить от значения.
	if _, ok := withoutCat["category"]; ok {
		t.Errorf("у записи без категории стоит ключ category=%q", withoutCat["category"])
	}
	for _, k := range []string{"url", "status", "latency_avg", "success_rate", "fail_streak"} {
		if _, ok := withoutCat[k]; !ok {
			t.Errorf("обязательное поле %s отсутствует", k)
		}
	}
}

// Описание pool_status обязано называть сглаженную природу статистики:
// слепой агент иначе читает одинаковые success_rate у соседей как копию
// одного числа, а fail_streak у live - как противоречие.
func TestPoolStatusDescriptionNamesPoolSemantics(t *testing.T) {
	c := newClient(t, Deps{Version: "test", Started: time.Now()})
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	for _, tool := range res.Tools {
		if tool.Name != "pool_status" {
			continue
		}
		for _, want := range []string{
			"success_rate - сглаженная живость (EMA",
			"не доля успешных проб",
			"провалы подряд после последнего успеха",
			"latency_avg - сглаженная латентность",
			"четверть веса новой пробы",
			// SQL-факт этапа 175: 0 из 14248 записей с категорией - поле
			// обещано и никогда не исполняется. Описание обязано это назвать.
			"категории не назначает",
			"отсутствие ключа значит «не присвоена»",
		} {
			if !strings.Contains(tool.Description, want) {
				t.Errorf("описание pool_status не содержит %q: %q", want, tool.Description)
			}
		}
		return
	}
	t.Fatal("pool_status не найден")
}

// Смоук-A этапа 174 (хвост): crawl.depth=2 при pages_detail[].depth=0 у всех
// страниц выглядел противоречием - это потолок конфигурации против фактической
// глубины страницы; found=105 против суммы addresses=106 - дедуп хостов.
func TestDiscoverDescriptionNamesDepthAndDedup(t *testing.T) {
	c := newClient(t, Deps{Version: "test", Started: time.Now()})
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	for _, tool := range res.Tools {
		if tool.Name != "discover_onions" {
			continue
		}
		for _, want := range []string{
			"потолок глубины из конфигурации",
			"фактическая глубина этой страницы",
			"уникальные хосты после дедупа",
			// Этап 175: новое поле страницы. Агент обязан знать, что оно
			// появляется только при состоявшемся запросе и что пул его
			// сглаживает - иначе не отличит «не измерялось» от «0ms».
			"latency_ms у страницы появляется при состоявшемся запросе",
			"сглаживается пулом в latency_avg",
			// Смоук-C этапа 175: addresses_total=14160 при total пула
			// 14257 - прежнее описание называло поле «размером пула»,
			// а код считает дедупнутый список этого вызова.
			"addresses_total - размер дедупнутого списка",
			"а не размер пула",
			// Смоук-A этапа 175: elapsed меньше суммы latency_pages при
			// per_host_delay_ms=2000 читался как противоречие - обход
			// параллелен, пауза действует внутри одного хоста.
			"обход идёт хостами параллельно",
		} {
			if !strings.Contains(tool.Description, want) {
				t.Errorf("описание discover_onions не содержит %q: %q", want, tool.Description)
			}
		}
		return
	}
	t.Fatal("discover_onions не найден")
}
