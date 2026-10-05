package mcpserver

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"voidsearchswag/internal/hunt"
	"voidsearchswag/internal/store"
)

// newHitsServer собирает сервер с охотой, выдача которой меняется по раундам на
// каждый запрос. Общий скрипт на несколько охот не годится: охоты съедали бы
// раунды по очереди, и находка случалась бы не у той охоты, которую проверяют.
func newHitsServer(t *testing.T, rounds map[string][][]string) (*store.Store, *hunt.Runner) {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/hits.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	i := map[string]int{}
	h := &hunt.Runner{Store: st, Search: func(_ context.Context, query, _ string, _ int) (hunt.SearchOutcome, error) {
		list := rounds[query]
		n := i[query]
		if n >= len(list) {
			n = len(list) - 1
		}
		i[query]++
		return hunt.SearchOutcome{URLs: list[n]}, nil
	}}
	return st, h
}

// Находка hunt_run обязана быть читаемой отдельным вызовом: до появления
// hunt_hits клиент видел url только внутри ответа того прогона, который их нашёл,
// и после reconnect история была недоступна.
func TestHuntRunSavesHistoryAndHitsToolReadsIt(t *testing.T) {
	st, h := newHitsServer(t, map[string][][]string{
		"leak": {{"http://a.onion/x"}, {"http://b.onion/y", "http://c.onion/z"}},
	})
	srv := New(Deps{Version: "test", Store: st, Hunter: h, Started: time.Now()})
	c := inProcess(t, srv)
	initClient(t, c)

	created := callToolArgs(t, c, "hunt_create", map[string]any{"query": "leak", "mode": "fast"})
	id := int(created["hunt_id"].(float64))
	if id < 1 {
		t.Fatalf("hunt_id=%v", created["hunt_id"])
	}
	base := callToolArgs(t, c, "hunt_run", map[string]any{"id": id})
	if base["changed"] != false {
		t.Fatalf("первый прогон стал находкой: %+v", base)
	}
	hit := callToolArgs(t, c, "hunt_run", map[string]any{"id": id})
	if hit["changed"] != true {
		t.Fatalf("смена выдачи не стала находкой: %+v", hit)
	}
	if saved, _ := hit["saved"].(float64); saved != 2 {
		t.Errorf("saved=%v, хочу 2: история была пуста", hit["saved"])
	}
	if _, ok := hit["save_error"]; ok {
		t.Errorf("save_error при успешной записи: %+v", hit)
	}

	out := callToolArgs(t, c, "hunt_hits", map[string]any{"id": id})
	if n, _ := out["count"].(float64); n != 2 {
		t.Fatalf("count=%v, хочу 2: %+v", out["count"], out)
	}
	rows, ok := out["hits"].([]any)
	if !ok || len(rows) != 2 {
		t.Fatalf("hits=%v", out["hits"])
	}
	urls := map[string]bool{}
	for _, raw := range rows {
		row, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("строка истории не объект: %v", raw)
		}
		// Этап 178: поля истории - snake_case, как объявлено в описании
		// инструмента (json-теги struct Hunt/HuntHit). До правки сериализация
		// шла PascalCase без тегов, и описание врало именами полей.
		u, _ := row["url"].(string)
		urls[u] = true
		if q, _ := row["query"].(string); q != "leak" {
			t.Errorf("строка %s потеряла запрос: %v", u, row["query"])
		}
		if m, _ := row["mode"].(string); m != "fast" {
			t.Errorf("строка %s потеряла режим: %v", u, row["mode"])
		}
		if hid, _ := row["hunt_id"].(float64); int(hid) != id {
			t.Errorf("строка %s привязана к охоте %v", u, row["hunt_id"])
		}
	}
	for _, want := range []string{"http://b.onion/y", "http://c.onion/z"} {
		if !urls[want] {
			t.Errorf("в истории нет %s: %+v", want, urls)
		}
	}
}

func TestHuntHitsToolReportsEmptyHistory(t *testing.T) {
	st, h := newHitsServer(t, map[string][][]string{
		"leak": {{"http://a.onion/x"}},
	})
	srv := New(Deps{Version: "test", Store: st, Hunter: h, Started: time.Now()})
	c := inProcess(t, srv)
	initClient(t, c)
	callToolArgs(t, c, "hunt_create", map[string]any{"query": "leak"})

	out := callToolArgs(t, c, "hunt_hits", map[string]any{})
	if n, _ := out["count"].(float64); n != 0 {
		t.Errorf("count=%v, хочу 0 до первой находки", out["count"])
	}
}

func TestHuntHitsToolRespectsLimit(t *testing.T) {
	st, h := newHitsServer(t, map[string][][]string{
		"leak": {{"http://a.onion/x"}, {"http://a.onion/1", "http://a.onion/2", "http://a.onion/3"}},
	})
	srv := New(Deps{Version: "test", Store: st, Hunter: h, Started: time.Now()})
	c := inProcess(t, srv)
	initClient(t, c)
	created := callToolArgs(t, c, "hunt_create", map[string]any{"query": "leak"})
	id := int(created["hunt_id"].(float64))
	callToolArgs(t, c, "hunt_run", map[string]any{"id": id})
	callToolArgs(t, c, "hunt_run", map[string]any{"id": id})

	two := callToolArgs(t, c, "hunt_hits", map[string]any{"id": id, "limit": 2})
	if n, _ := two["count"].(float64); n != 2 {
		t.Errorf("count=%v при limit=2, хочу 2", two["count"])
	}
	all := callToolArgs(t, c, "hunt_hits", map[string]any{"id": id})
	if n, _ := all["count"].(float64); n != 3 {
		t.Errorf("count=%v без limit, хочу 3", all["count"])
	}
}

// Без Hunter инструмент обязан отвечать внятной ошибкой, а не падать в nil:
// сервер собирается в нескольких конфигурациях, и охота есть не в каждой.
func TestHuntHitsToolWithoutHunter(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/nohunter.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv := New(Deps{Version: "test", Store: st, Started: time.Now()})
	c := inProcess(t, srv)
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "hunt_hits", Arguments: map[string]any{}},
	})
	if err != nil {
		t.Fatalf("вызов: %v", err)
	}
	if !res.IsError {
		t.Errorf("ответ без ошибки: %+v", res)
	}
	if text := textOf(t, res); !strings.Contains(text, "охота не инициализирована") {
		t.Errorf("текст=%q", text)
	}
}
