package mcpserver

// Этап 178, смоук-раунд 4 (J/K/L на vss178e): юнит-ловцы каждого дефекта
// раунда. Правило прежнее: тест рождается из живого дефекта смока и называет
// его в комментарии, чтобы мутация правки ловилась адресно.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"voidsearchswag/internal/catalog"
	"voidsearchswag/internal/discover"
	"voidsearchswag/internal/promote"
	"voidsearchswag/internal/store"
)

// toolSchemaRequired снимает список обязательных полей схемы инструмента:
// смоук-K (Д2) нашёл расхождение схемы и рантайма, и проверять его нужно
// именно по схеме, которую читает MCP-клиент.
func toolSchemaRequired(t *testing.T, name string) []string {
	t.Helper()
	c := newClient(t, Deps{Version: "test", Started: time.Now()})
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	for _, tool := range res.Tools {
		if tool.Name == name {
			return tool.InputSchema.Required
		}
	}
	t.Fatalf("инструмент %s не найден", name)
	return nil
}

// toolSchemaText сериализует схему целиком: описания параметров живут в
// Properties, а не в общем Description инструмента - фразы контрактов
// параметров проверяются по этому тексту.
func toolSchemaText(t *testing.T, name string) string {
	t.Helper()
	c := newClient(t, Deps{Version: "test", Started: time.Now()})
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	for _, tool := range res.Tools {
		if tool.Name == name {
			b, err := json.Marshal(tool.InputSchema)
			if err != nil {
				t.Fatalf("схема %s не сериализуется: %v", name, err)
			}
			return string(b)
		}
	}
	t.Fatalf("инструмент %s не найден", name)
	return ""
}

func requireSchemaPhrases(t *testing.T, name string, wants []string) {
	t.Helper()
	schema := toolSchemaText(t, name)
	for _, want := range wants {
		if !strings.Contains(schema, want) {
			t.Errorf("схема %s не содержит %q", name, want)
		}
	}
}

// Смоук-K Д2: рантайм parse требует fields («параметр fields обязателен»),
// а inputSchema требовал только url - клиент строил запрос по схеме и ловил
// отказ только на вызове.
func TestParseSchemaRequiresFields(t *testing.T) {
	required := toolSchemaRequired(t, "parse")
	hasURL, hasFields := false, false
	for _, f := range required {
		if f == "url" {
			hasURL = true
		}
		if f == "fields" {
			hasFields = true
		}
	}
	if !hasURL {
		t.Errorf("url выпал из required: %+v", required)
	}
	if !hasFields {
		t.Errorf("fields отсутствует в required схемы parse: %+v - рантайм требует поле, а схема молчит", required)
	}
}

// Смоук-L Д2: limit промоута - потолок ПРОВЕРОК («сколько живых сервисов
// проверить»), а не поднятий. До правки стоп стоял по числу продвинутых
// движков, и вызов без аргументов проверял выборку limit*3=30 при заявленных
// 10. Здесь 6 live-хостов без поисковой формы: поднятий нет вовсе, и только
// новый стоп даёт checked=2 при limit=2.
func TestPromoteStopsAfterLimitChecks(t *testing.T) {
	st, eng, p := promoteDeps(t, noFormHandler())
	ctx := context.Background()
	for i := 0; i < 6; i++ {
		if err := st.UpsertOnion(ctx, store.Onion{
			URL:    fmt.Sprintf("http://host%d.example/", i),
			Status: "live",
		}); err != nil {
			t.Fatal(err)
		}
	}
	promoter := &promote.Promoter{Client: p.Client, MinOnionHits: 2, Timeout: 10 * time.Second}
	mcpSrv := New(Deps{Version: "test", Store: st, Search: eng, Promoter: promoter, Started: time.Now()})
	c := inProcess(t, mcpSrv)
	initClient(t, c)

	out, isErr := callToolJSONErr(t, c, "promote_engines", map[string]any{"limit": 2})
	if isErr {
		t.Fatalf("promote вернул ошибку: %+v", out)
	}
	if checked, _ := out["checked"].(float64); checked != 2 {
		t.Errorf("checked = %v, хочу 2: limit обязан останавливать проверки, а не поднятия (без стопа по проверкам было бы 6)", out["checked"])
	}
}

// Смоук-J Д1: задача collect_files обязана финализироваться при отменённом
// контексте вызова. HTTP-транспорт mcp-go отменяет ctx обработчика при уходе
// клиента, и записи задач на отменённом контексте отказывали молча -
// tasks_running висел 17+ минут после остановки прогресса. Вызов хендлера с
// уже отменённым контекстом - точная модель ушедшего клиента: обход
// мгновенно срезается, но судьба записи (done/failed) обязана доехать до
// базы на контексте финализации.
func TestCollectFilesTaskFinalizesAfterClientGone(t *testing.T) {
	dir := t.TempDir()
	st := exportStore(t, dir)
	seedMCPBase(t, st, dir)

	col := catalog.NewCollector(&stubFetcher{body: `<a href="/dump.sql">d</a>`}, st, nil, nil,
		catalog.Config{PerHostDelay: time.Millisecond})
	d := Deps{Version: "test", Store: st, Collector: col, Started: time.Now()}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "collect_files",
			Arguments: map[string]any{"hosts": []any{"abcdefghijklmnop.onion"}},
		},
	}
	if _, err := d.collectFilesHandler(ctx, req); err != nil {
		t.Fatalf("collectFilesHandler: %v", err)
	}

	qctx, qcancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer qcancel()
	tasks, _, err := st.TaskStats(qctx)
	if err != nil {
		t.Fatal(err)
	}
	if tasks != 1 {
		t.Fatalf("задач в базе %d, хочу 1: задача не зарегистрирована на отменённом контексте", tasks)
	}
	var status string
	var msg string
	row := mcpDB(t, dir).QueryRowContext(qctx, `SELECT status, message FROM tasks`)
	if err := row.Scan(&status, &msg); err != nil {
		t.Fatal(err)
	}
	if status == "running" {
		t.Errorf("задача осталась running при отменённом контексте вызова: финализация не пережила уход клиента (status=%q msg=%q)", status, msg)
	}
	if status != "done" && status != "failed" {
		t.Errorf("status = %q, хочу done или failed", status)
	}
}

// Смоук-K флаг 1: пустая волна probe_pool обязана нести применённые пределы
// и elapsed, как и отброшенная выборка: «живых 0 из 0» с concurrency=0 и
// elapsed="" выглядело незаполненным ответом.
func TestProbeWaveEmptyPoolReportsAppliedLimits(t *testing.T) {
	st, eng, _ := promoteDeps(t, noFormHandler())
	prober := discover.NewProber(nil, st, nil, discover.ProbeConfig{
		Concurrency: 3, Timeout: 7 * time.Second, Delay: time.Millisecond,
	})
	srv := New(Deps{Version: "test", Store: st, Search: eng, Prober: prober, Started: time.Now()})
	c := inProcess(t, srv)
	initClient(t, c)

	out, isErr := callToolJSONErr(t, c, "probe_pool", map[string]any{"limit": 5})
	if isErr {
		t.Fatalf("probe_pool на пустом пуле вернул ошибку: %+v", out)
	}
	if n, _ := out["total"].(float64); n != 0 {
		t.Fatalf("пул пуст, а total = %v", out["total"])
	}
	if n, _ := out["concurrency"].(float64); n != 3 {
		t.Errorf("concurrency = %v, хочу 3: применённые пределы не названы на пустой волне", out["concurrency"])
	}
	if n, _ := out["timeout_ms"].(float64); n != 7000 {
		t.Errorf("timeout_ms = %v, хочу 7000: применённые пределы не названы на пустой волне", out["timeout_ms"])
	}
	if n, _ := out["delay_ms"].(float64); n != 1 {
		t.Errorf("delay_ms = %v, хочу 1: применённые пределы не названы на пустой волне", out["delay_ms"])
	}
	if s, _ := out["elapsed"].(string); s == "" {
		t.Error("elapsed пуст на пустой волне: ответ выглядит незаполненным")
	}
}

// noFormHandler отдаёт страницу без поисковой формы: каждый кандидат
// промоута честно проваливает проверку.
func noFormHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html><body><p>никаких форм здесь нет</p></body></html>`))
	}
}
