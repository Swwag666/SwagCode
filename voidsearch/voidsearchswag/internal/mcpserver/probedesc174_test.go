package mcpserver

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// Этап 174: описания инструментов обязаны называть новые поля событий,
// иначе слепой агент не знает, чем «cancelled» отличается от «limit_hit»
// и читает отсутствие поля как false. probe_pool получил волю отмены
// волны (probe.cancelled), discover_onions - составной limit_hit.

func TestProbePoolDescriptionNamesCancelSemantics(t *testing.T) {
	c := newClient(t, Deps{Version: "test", Started: time.Now()})
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	for _, tool := range res.Tools {
		if tool.Name != "probe_pool" {
			continue
		}
		for _, want := range []string{
			"cancelled",
			"волна обрезана отменой",
			"limit_hit у пробы не событие",
			// Смоук-C этапа 174: агент увидел dead=3 при pool_status
			// "unknown" и latency_ms == timeout_ms без поля cancelled -
			// и не смог вывести, обрезана волна или пробы падали сами.
			// Оба факта штатные, их обязан называть контракт инструмента.
			"не дождалась своего предела, а не отмена волны",
			"накопительный вердикт порогами",
		} {
			if !strings.Contains(tool.Description, want) {
				t.Errorf("описание probe_pool не содержит %q: %q", want, tool.Description)
			}
		}
		return
	}
	t.Fatal("probe_pool не найден")
}

// Смоук-C этапа 174: при max_files=3, links=6, saved=1 поле limit_hit
// отсутствовало - слепой агент не знал, лимит не достигнут или событие
// потеряно. Описание обязано назвать, что потолок считается по saved и
// повторные находки его не съедают.
func TestCollectFilesDescriptionNamesCeilingBySaved(t *testing.T) {
	c := newClient(t, Deps{Version: "test", Started: time.Now()})
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	for _, tool := range res.Tools {
		if tool.Name != "collect_files" {
			continue
		}
		for _, want := range []string{
			"Потолок max_files считается по saved",
			"не съедают",
			"отсутствует, если лимит не достигнут",
		} {
			if !strings.Contains(tool.Description, want) {
				t.Errorf("описание collect_files не содержит %q: %q", want, tool.Description)
			}
		}
		return
	}
	t.Fatal("collect_files не найден")
}

// Смоук-A/B этапа 174: new_hosts=105 при new=0 корня читался как потеря
// записей. new_hosts - новые для обхода, вклад в пул - по new/updated
// корня; и размер в pages_detail всегда 0 (замер после обхода), что
// выглядело противоречием с агрегатом.
func TestDiscoverDescriptionNamesNewHostsAndPageSizes(t *testing.T) {
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
			"новые для этого обхода, а не для пула",
			"Размер файла в pages_detail всегда 0",
			"агрегат file_refs несёт снятые размеры",
		} {
			if !strings.Contains(tool.Description, want) {
				t.Errorf("описание discover_onions не содержит %q: %q", want, tool.Description)
			}
		}
		return
	}
	t.Fatal("discover_onions не найден")
}
