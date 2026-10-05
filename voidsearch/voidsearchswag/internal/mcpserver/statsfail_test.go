package mcpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"

	"voidsearchswag/internal/store"
)

// mcpDB открывает базу теста отдельным соединением.
//
// Прямое соединение нужно, чтобы сломать схему: публичный API store не даёт ни
// удалить таблицу, ни прочитать базу, которую не удаётся прочитать целиком, а
// именно такая ситуация и проверяется.
func mcpDB(t *testing.T, dir string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", dir+"/mcp.db")
	if err != nil {
		t.Fatalf("открытие базы: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// dropMCPTable удаляет таблицу, имитируя повреждение базы.
func dropMCPTable(t *testing.T, dir, table string) {
	t.Helper()
	if _, err := mcpDB(t, dir).ExecContext(context.Background(), `DROP TABLE `+table); err != nil {
		t.Fatalf("удаление таблицы %s: %v", table, err)
	}
}

// seedMCPBase вносит известный состав: два файла, одну живую запись пула и три
// охоты.
func seedMCPBase(t *testing.T, st *store.Store, dir string) {
	t.Helper()
	ctx := context.Background()
	for _, f := range []store.FileEntry{
		{TaskID: "t", URL: "http://a.onion/book.epub", Filename: "book.epub", Ext: "epub", Size: 2048},
		{TaskID: "t", URL: "http://a.onion/photo.jpg", Filename: "photo.jpg", Ext: "jpg", Size: 4096},
	} {
		if err := st.AddFile(ctx, f); err != nil {
			t.Fatalf("add file %s: %v", f.URL, err)
		}
	}
	if err := st.UpsertOnion(ctx, store.Onion{
		URL: "http://a.onion/", Title: "живой", Status: "live",
	}); err != nil {
		t.Fatalf("запись пула: %v", err)
	}
	db := mcpDB(t, dir)
	for i := 0; i < 3; i++ {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO hunts(query, mode, schedule_min) VALUES (?, 'normal', 60)`,
			fmt.Sprintf("query %d", i)); err != nil {
			t.Fatalf("вставка охоты %d: %v", i, err)
		}
	}
}

// callToolJSON вызывает инструмент и разбирает его JSON-ответ.
func callToolJSON(t *testing.T, c *client.Client, name string) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: name, Arguments: map[string]any{}},
	})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("%s вернул ошибку: %s", name, textOf(t, res))
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(textOf(t, res)), &out); err != nil {
		t.Fatalf("ответ %s не разобран: %v\n%s", name, err, textOf(t, res))
	}
	return out
}

// readResourceJSON читает ресурс и разбирает его JSON.
func readResourceJSON(t *testing.T, c *client.Client, uri string) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := c.ReadResource(ctx, mcp.ReadResourceRequest{
		Params: mcp.ReadResourceParams{URI: uri},
	})
	if err != nil {
		t.Fatalf("%s: %v", uri, err)
	}
	tc, ok := res.Contents[0].(mcp.TextResourceContents)
	if !ok {
		t.Fatalf("%s вернул не текст: %T", uri, res.Contents[0])
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(tc.Text), &out); err != nil {
		t.Fatalf("ответ %s не разобран: %v\n%s", uri, err, tc.Text)
	}
	return out
}

// problemsOf возвращает список непрочитанных источников из блока ответа.
func problemsOf(t *testing.T, block map[string]any) []string {
	t.Helper()
	raw, ok := block["errors"]
	if !ok {
		return nil
	}
	list, ok := raw.([]any)
	if !ok {
		t.Fatalf("errors не список: %T (%v)", raw, raw)
	}
	out := make([]string, 0, len(list))
	for _, v := range list {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("элемент errors не строка: %T", v)
		}
		out = append(out, s)
	}
	return out
}

// requireProblem проверяет, что источник назван среди непрочитанных.
func requireProblem(t *testing.T, block map[string]any, source string) {
	t.Helper()
	found := problemsOf(t, block)
	joined := strings.Join(found, "; ")
	if !strings.Contains(joined, source) {
		t.Errorf("источник %q не назван среди непрочитанных: %v", source, found)
	}
}

func TestStatsToolReportsUnreadableSource(t *testing.T) {
	// Инструмент stats обязан отличать «охот нет» от «охоты не удалось
	// прочитать». Прежняя версия при ошибке просто не клала ключ в map, и
	// отсутствие hunts_total было неотличимо от случая, когда поле не
	// предусмотрено: клиент не видел ни нуля, ни ошибки, ни причины.
	dir := t.TempDir()
	st, c := newTestServerInDir(t, dir)
	initClient(t, c)
	seedMCPBase(t, st, dir)
	dropMCPTable(t, dir, "hunts")

	out := callToolJSON(t, c, "stats")
	db, ok := out["db"].(map[string]any)
	if !ok {
		t.Fatalf("в ответе нет блока db: %v", out)
	}
	requireProblem(t, db, "охоты")

	// Источники, которые прочитать удалось, остаются точными: частичная поломка
	// не повод обнулять остальное.
	if n, _ := db["files_total"].(float64); n != 2 {
		t.Errorf("files_total = %v, ожидала 2", db["files_total"])
	}
	if n, _ := db["onion_total"].(float64); n != 1 {
		t.Errorf("onion_total = %v, ожидала 1", db["onion_total"])
	}
}

func TestStatsResourceReportsUnreadableSource(t *testing.T) {
	// Ресурс-двойник инструмента stats повторял ту же схему, поэтому проверяется
	// отдельно: правка одного места без другого оставила бы половину обмана.
	dir := t.TempDir()
	st, c := newTestServerInDir(t, dir)
	initClient(t, c)
	seedMCPBase(t, st, dir)
	dropMCPTable(t, dir, "hunts")

	out := readResourceJSON(t, c, "voidsearch://stats/summary")
	db, ok := out["db"].(map[string]any)
	if !ok {
		t.Fatalf("в ресурсе нет блока db: %v", out)
	}
	requireProblem(t, db, "охоты")
	if n, _ := db["files_total"].(float64); n != 2 {
		t.Errorf("files_total = %v, ожидала 2", db["files_total"])
	}
}

func TestStatusToolReportsUnreadableSource(t *testing.T) {
	dir := t.TempDir()
	st, c := newTestServerInDir(t, dir)
	initClient(t, c)
	seedMCPBase(t, st, dir)
	dropMCPTable(t, dir, "tasks")

	out := callToolJSON(t, c, "status")
	db, ok := out["db"].(map[string]any)
	if !ok {
		t.Fatalf("в ответе нет блока db: %v", out)
	}
	requireProblem(t, db, "задачи")
	if n, _ := db["onion_total"].(float64); n != 1 {
		t.Errorf("onion_total = %v, ожидала 1", db["onion_total"])
	}
}

func TestPoolResourceReportsUnreadableSource(t *testing.T) {
	// Ресурс пула читает два источника, и оба падают на удалённой таблице:
	// список непрочитанных обязан содержать оба, потому что сбор не должен
	// прерываться на первой ошибке.
	dir := t.TempDir()
	st, c := newTestServerInDir(t, dir)
	initClient(t, c)
	seedMCPBase(t, st, dir)
	dropMCPTable(t, dir, "onion_pool")

	out := readResourceJSON(t, c, "voidsearch://pool/status")
	found := problemsOf(t, out)
	if len(found) != 2 {
		t.Errorf("собрано %d проблем, ожидала 2: %v", len(found), found)
	}
	for _, want := range []string{"пул всего", "разбивка"} {
		if !strings.Contains(strings.Join(found, "; "), want) {
			t.Errorf("источник %q не назван: %v", want, found)
		}
	}
}

func TestStatsToolNamesEveryBrokenSource(t *testing.T) {
	// Каждая из пяти таблиц ломается по очереди, и источник обязан быть назван
	// своим именем. Тест нужен не только ради полноты: без него ветки ошибок
	// пула, файлов и селекторов не исполнялись вовсе, и форма сообщения об
	// ошибке в них никем не проверялась.
	cases := []struct{ table, source string }{
		{"onion_pool", "пул"},
		{"file_catalog", "файлы"},
		{"tasks", "задачи"},
		{"hunts", "охоты"},
		{"selectors", "селекторы"},
	}
	for _, c := range cases {
		t.Run(c.table, func(t *testing.T) {
			dir := t.TempDir()
			st, cl := newTestServerInDir(t, dir)
			initClient(t, cl)
			seedMCPBase(t, st, dir)
			dropMCPTable(t, dir, c.table)

			out := callToolJSON(t, cl, "stats")
			db, ok := out["db"].(map[string]any)
			if !ok {
				t.Fatalf("в ответе нет блока db: %v", out)
			}
			requireProblem(t, db, c.source)

			// Ровно одна проблема: поломка одной таблицы не должна валить
			// остальные источники, иначе список перестал бы указывать на причину.
			if found := problemsOf(t, db); len(found) != 1 {
				t.Errorf("собрано %d проблем, ожидала 1: %v", len(found), found)
			}
		})
	}
}

func TestStatsToolWithSearchEngine(t *testing.T) {
	// Ветка с поисковым движком проверяется отдельно: она добавляет в ответ
	// состав onion-движков и их живость, и до этого теста не исполнялась вовсе.
	eng, st := testEngineWithStub(t)
	srv := New(Deps{Version: "test", Store: st, Search: eng, Started: time.Now()})
	c := inProcess(t, srv)
	initClient(t, c)

	out := callToolJSON(t, c, "stats")
	if _, ok := out["onion_engines"]; !ok {
		t.Errorf("с поисковым движком нет состава onion-движков: %v", out)
	}
	db, ok := out["db"].(map[string]any)
	if !ok {
		t.Fatalf("в ответе нет блока db: %v", out)
	}
	if p := problemsOf(t, db); len(p) != 0 {
		t.Errorf("на здоровой базе есть непрочитанные источники: %v", p)
	}

	// Ресурс-двойник проверяется на тех же Deps: он обязан показывать состав
	// движков так же, как инструмент, иначе клиент получил бы две разные картины
	// одного состояния.
	res := readResourceJSON(t, c, "voidsearch://stats/summary")
	if _, ok := res["onion_engines"]; !ok {
		t.Errorf("в ресурсе с поисковым движком нет состава onion-движков: %v", res)
	}
	if _, ok := res["db"].(map[string]any); !ok {
		t.Errorf("в ресурсе нет блока db: %v", res)
	}
}
func TestHealthySummariesHaveNoErrors(t *testing.T) {
	// На здоровой базе поле errors отсутствовать обязано: иначе клиент начнёт
	// видеть шум там, где всё прочитано.
	dir := t.TempDir()
	st, c := newTestServerInDir(t, dir)
	initClient(t, c)
	seedMCPBase(t, st, dir)

	stats := callToolJSON(t, c, "stats")
	db, ok := stats["db"].(map[string]any)
	if !ok {
		t.Fatalf("в ответе нет блока db: %v", stats)
	}
	if p := problemsOf(t, db); len(p) != 0 {
		t.Errorf("на здоровой базе есть непрочитанные источники: %v", p)
	}
	if n, _ := db["hunts_total"].(float64); n != 3 {
		t.Errorf("hunts_total = %v, ожидала 3", db["hunts_total"])
	}
	if n, _ := db["files_bytes"].(float64); n != 6144 {
		t.Errorf("files_bytes = %v, ожидала 6144", db["files_bytes"])
	}

	status := callToolJSON(t, c, "status")
	if sdb, ok := status["db"].(map[string]any); ok {
		if p := problemsOf(t, sdb); len(p) != 0 {
			t.Errorf("status на здоровой базе сообщил о проблемах: %v", p)
		}
	}

	pool := readResourceJSON(t, c, "voidsearch://pool/status")
	if p := problemsOf(t, pool); len(p) != 0 {
		t.Errorf("ресурс пула на здоровой базе сообщил о проблемах: %v", p)
	}
	if n, _ := pool["total"].(float64); n != 1 {
		t.Errorf("total = %v, ожидала 1", pool["total"])
	}
}
