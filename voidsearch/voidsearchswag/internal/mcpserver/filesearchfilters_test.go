package mcpserver

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"

	"voidsearchswag/internal/store"
)

// file_search печатает verdict в каждой записи ответа, но отобрать по нему не
// давал: CLI-команда files принимает --verdict, --risk и --task, а инструмент
// ограничивался query, ext, размером и limit. Агент видел метку в ответе и не
// мог ею воспользоваться. Это то же расхождение двух входов, из-за которого
// консоль и MCP со временем начинают отвечать по-разному: разбор списка меток
// живёт в хранилище, поэтому инструменту оставалось только передать параметр.
func TestFileSearchToolFiltersByVerdictRiskAndTask(t *testing.T) {
	_, st := testEngineWithStub(t)
	ctx := context.Background()
	for _, f := range []store.FileEntry{
		{TaskID: "t1", URL: "http://a.onion/book.epub", Filename: "book.epub", Ext: "epub", Size: 100, Verdict: "ebook"},
		{TaskID: "t1", URL: "http://a.onion/setup.exe", Filename: "setup.exe", Ext: "exe", Size: 200, Verdict: "executable"},
		{TaskID: "t1", URL: "http://a.onion/keys.kdbx", Filename: "keys.kdbx", Ext: "kdbx", Size: 300, Verdict: "secret"},
		{TaskID: "t2", URL: "http://a.onion/notes.txt", Filename: "notes.txt", Ext: "txt", Size: 50, Verdict: "document"},
	} {
		if err := st.AddFile(ctx, f); err != nil {
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

	names := func(out map[string]any) []string {
		t.Helper()
		files, ok := out["files"].([]any)
		if !ok {
			t.Fatalf("files не список: %v", out["files"])
		}
		got := make([]string, 0, len(files))
		for _, raw := range files {
			e, ok := raw.(map[string]any)
			if !ok {
				t.Fatalf("запись не объект: %v", raw)
			}
			name, _ := e["filename"].(string)
			got = append(got, name)
		}
		sort.Strings(got)
		return got
	}
	same := func(label string, got, want []string) {
		t.Helper()
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s: получено %v, хочу %v", label, got, want)
		}
	}

	same("verdict=ebook",
		names(callToolArgs(t, c, "file_search", map[string]any{"verdict": "ebook"})),
		[]string{"book.epub"})
	same("verdict=ebook,document списком",
		names(callToolArgs(t, c, "file_search", map[string]any{"verdict": "ebook,document"})),
		[]string{"book.epub", "notes.txt"})
	same("risk_only",
		names(callToolArgs(t, c, "file_search", map[string]any{"risk_only": true})),
		[]string{"keys.kdbx", "setup.exe"})
	same("task_id=t2",
		names(callToolArgs(t, c, "file_search", map[string]any{"task_id": "t2"})),
		[]string{"notes.txt"})
	same("verdict вместе с task_id",
		names(callToolArgs(t, c, "file_search", map[string]any{"verdict": "executable", "task_id": "t1"})),
		[]string{"setup.exe"})
}

// Схема инструмента обязана объявлять новые параметры: клиент узнаёт о них из
// tools/list и без объявления просто не предложит их модели.
func TestFileSearchToolSchemaDeclaresFilters(t *testing.T) {
	srv := New(Deps{Version: "test", Started: time.Now()})
	c, err := client.NewInProcessClient(srv)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	initClient(t, c)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tools, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	for _, tool := range tools.Tools {
		if tool.Name != "file_search" {
			continue
		}
		for _, want := range []string{"verdict", "risk_only", "task_id"} {
			if _, found := tool.InputSchema.Properties[want]; !found {
				t.Errorf("в схеме file_search нет параметра %s: %+v", want, tool.InputSchema.Properties)
			}
		}
		return
	}
	t.Fatal("инструмент file_search не объявлен")
}
