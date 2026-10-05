package mcpserver

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"

	"voidsearchswag/internal/store"
)

// Этап 166, жалоба смоук-агента C2: 7 файлов с size=0 в каталоге, но
// file_search с max_size=100 возвращал пустоту без единого слова - и достать
// эти записи не было никаким фильтром. Проверяются обе половины правки:
// unknown_size=true достаёт записи, а пустой размерный фильтр объясняет,
// куда они делись.
func TestFileSearchUnknownSize(t *testing.T) {
	_, st := testEngineWithStub(t)
	ctx := context.Background()
	for _, f := range []store.FileEntry{
		{TaskID: "t1", URL: "http://a.onion/known.zip", Filename: "known.zip", Ext: "zip", Size: 500},
		{TaskID: "t1", URL: "http://a.onion/unknown1.zip", Filename: "unknown1.zip", Ext: "zip"},
		{TaskID: "t1", URL: "http://a.onion/unknown2.zip", Filename: "unknown2.zip", Ext: "zip"},
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

	out := callToolArgs(t, c, "file_search", map[string]any{"unknown_size": true})
	files, ok := out["files"].([]any)
	if !ok {
		t.Fatalf("files не список: %v", out["files"])
	}
	if len(files) != 2 {
		t.Fatalf("unknown_size=true вернул %d файлов, хочу 2", len(files))
	}
	for _, raw := range files {
		e, _ := raw.(map[string]any)
		name, _ := e["filename"].(string)
		if name == "known.zip" {
			t.Error("в выдачу unknown_size попал файл с известным размером")
		}
	}
	if v, _ := out["unknown_size_only"].(bool); !v {
		t.Errorf("unknown_size_only=%v, хочу true", out["unknown_size_only"])
	}

	// Пустой размерный фильтр обязан объяснить пустоту и назвать число:
	// до правки агент получал ноль записей и ноль слов.
	empty := callToolArgs(t, c, "file_search", map[string]any{"max_size": 100})
	files, _ = empty["files"].([]any)
	if len(files) != 0 {
		t.Fatalf("max_size=100 вернул %d файлов, хочу 0 (неизвестные исключены)", len(files))
	}
	n, _ := empty["unknown_size_files"].(float64)
	if int(n) != 2 {
		t.Errorf("unknown_size_files=%v, хочу 2", empty["unknown_size_files"])
	}
	note, _ := empty["note"].(string)
	if !strings.Contains(note, "неизвестного размера") || !strings.Contains(note, "unknown_size=true") {
		t.Errorf("note не объясняет пустоту: %q", note)
	}

	// Схема инструмента обязана нести параметр: агент читает описания
	// tools/list, а не исходники.
	tctx, tcancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer tcancel()
	tools, err := c.ListTools(tctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	for _, tool := range tools.Tools {
		if tool.Name != "file_search" {
			continue
		}
		if _, found := tool.InputSchema.Properties["unknown_size"]; !found {
			t.Errorf("в схеме file_search нет параметра unknown_size: %+v", tool.InputSchema.Properties)
		}
		if !strings.Contains(tool.Description, "не удалось снять") || !strings.Contains(tool.Description, "unknown_size") {
			t.Errorf("описание file_search не объясняет семантику неизвестного размера: %q", tool.Description)
		}
		return
	}
	t.Fatal("инструмент file_search не объявлен")
}
