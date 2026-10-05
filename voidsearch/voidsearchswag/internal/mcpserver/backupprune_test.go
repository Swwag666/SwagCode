package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// pruneFixture наполняет каталог бэкапов n файлами с именами снимков. Содержимое
// не важно: ротация выбирает жертвы по имени, а снимок создаёт VACUUM INTO из
// базы, которую готовит promoteDeps.
func pruneFixture(t *testing.T, dir string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("voidsearchswag-202609%02d-080000.000.db", i+1)
		if err := os.WriteFile(filepath.Join(dir, name), []byte("fixture"), 0o644); err != nil {
			t.Fatalf("фикстура %s: %v", name, err)
		}
	}
}

func countSnaps(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("чтение каталога: %v", err)
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "voidsearchswag-") && strings.HasSuffix(e.Name(), ".db") {
			n++
		}
	}
	return n
}

// Пустой каталог бэкапов - первый запуск: подтверждения не требуется даже при
// keep=1, потому что стирать нечего.
func TestBackupCreateFirstRunNeedsNoConfirm(t *testing.T) {
	st, eng, _ := promoteDeps(t, engineStub())
	dir := t.TempDir()
	srv := New(Deps{Version: "test", Store: st, Search: eng, BackupDir: dir, BackupKeep: 7, Started: time.Now()})
	c := inProcess(t, srv)
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "backup_create", Arguments: map[string]any{"keep": float64(1)}},
	})
	if err != nil {
		t.Fatalf("backup_create: %v", err)
	}
	if res.IsError {
		t.Fatalf("первый снимок отказал: %s", textOf(t, res))
	}
	if got := countSnaps(t, dir); got != 1 {
		t.Errorf("снимков %d, хочу 1", got)
	}
}

// Инструмент доступен LLM-агенту: до правки один вызов с keep=1 стирал всю
// историю резервных копий, а ответ об этом молчал.
func TestBackupCreateRefusesMassPrune(t *testing.T) {
	st, eng, _ := promoteDeps(t, engineStub())
	dir := t.TempDir()
	pruneFixture(t, dir, 4)

	srv := New(Deps{Version: "test", Store: st, Search: eng, BackupDir: dir, BackupKeep: 7, Started: time.Now()})
	c := inProcess(t, srv)
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "backup_create", Arguments: map[string]any{"keep": float64(1)}},
	})
	if err != nil {
		t.Fatalf("backup_create: %v", err)
	}
	if !res.IsError {
		t.Fatal("агент стёр историю без подтверждения")
	}
	if got := countSnaps(t, dir); got != 4 {
		t.Errorf("снимков %d, хочу 4: отказ обязан оставить историю нетронутой", got)
	}
	body := textOf(t, res)
	if !strings.Contains(body, "confirm_prune") {
		t.Errorf("в ответе нет способа подтвердить: %q", body)
	}
	if !strings.Contains(body, "4 снимков") {
		t.Errorf("в ответе нет числа жертв: %q", body)
	}
}

func TestBackupCreateConfirmPrune(t *testing.T) {
	st, eng, _ := promoteDeps(t, engineStub())
	dir := t.TempDir()
	pruneFixture(t, dir, 4)

	srv := New(Deps{Version: "test", Store: st, Search: eng, BackupDir: dir, BackupKeep: 7, Started: time.Now()})
	c := inProcess(t, srv)
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "backup_create", Arguments: map[string]any{
			"keep": float64(1), "confirm_prune": true,
		}},
	})
	if err != nil {
		t.Fatalf("backup_create: %v", err)
	}
	if res.IsError {
		t.Fatalf("подтверждённая ротация отказала: %s", textOf(t, res))
	}
	if got := countSnaps(t, dir); got != 1 {
		t.Errorf("снимков %d, хочу 1", got)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(textOf(t, res)), &out); err != nil {
		t.Fatalf("ответ не JSON: %v (%.200s)", err, textOf(t, res))
	}
	if out["pruned"] != float64(4) {
		t.Errorf("pruned = %v, хочу 4: ответ обязан называть число стёртых", out["pruned"])
	}
	if out["snapshot"] == nil || out["snapshot"] == "" {
		t.Errorf("новый снимок не назван: %+v", out)
	}
}

// Штатная ротация подтверждением не облагается: иначе агент упирался бы в отказ
// на каждом плановом снимке.
func TestBackupCreateUsualRotationNeedsNoConfirm(t *testing.T) {
	st, eng, _ := promoteDeps(t, engineStub())
	dir := t.TempDir()
	pruneFixture(t, dir, 3)

	srv := New(Deps{Version: "test", Store: st, Search: eng, BackupDir: dir, BackupKeep: 3, Started: time.Now()})
	c := inProcess(t, srv)
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "backup_create", Arguments: map[string]any{}},
	})
	if err != nil {
		t.Fatalf("backup_create: %v", err)
	}
	if res.IsError {
		t.Fatalf("штатная ротация отказала: %s", textOf(t, res))
	}
	if got := countSnaps(t, dir); got != 3 {
		t.Errorf("снимков %d, хочу 3", got)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(textOf(t, res)), &out); err != nil {
		t.Fatalf("ответ не JSON: %v (%.200s)", err, textOf(t, res))
	}
	if out["pruned"] != float64(1) {
		t.Errorf("pruned = %v, хочу 1", out["pruned"])
	}
}

// Параметр подтверждения обязан быть виден в схеме инструмента: агент, который
// его не знает, получит отказ без способа продолжить.
func TestBackupToolDeclaresConfirmPrune(t *testing.T) {
	st, eng, _ := promoteDeps(t, engineStub())
	srv := New(Deps{Version: "test", Store: st, Search: eng, BackupDir: t.TempDir(), BackupKeep: 7, Started: time.Now()})
	c := inProcess(t, srv)
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	tools, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	for _, tool := range tools.Tools {
		if tool.Name != "backup_create" {
			continue
		}
		if _, ok := tool.InputSchema.Properties["confirm_prune"]; !ok {
			t.Fatalf("в схеме backup_create нет confirm_prune: %+v", tool.InputSchema.Properties)
		}
		return
	}
	t.Fatal("backup_create не найден в tools/list")
}
