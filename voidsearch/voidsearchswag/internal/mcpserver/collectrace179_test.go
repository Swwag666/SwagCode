package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"voidsearchswag/internal/catalog"
	"voidsearchswag/internal/httpc"
)

// delayedFetcher Ð¾Ñ‚Ð´Ð°Ñ‘Ñ‚ Ð·Ð°Ð´Ð°Ð½Ð½Ð¾Ðµ Ñ‚ÐµÐ»Ð¾ Ð¿Ð¾ÑÐ»Ðµ Ð·Ð°Ð´ÐµÑ€Ð¶ÐºÐ¸: Ð¾Ð±Ñ…Ð¾Ð´ Ð´Ð»Ð¸Ñ‚ÑÑ Ð´Ð¾Ð»ÑŒÑˆÐµ
// ÐºÐ¾Ñ€Ð¾Ñ‚ÐºÐ¾Ð³Ð¾ Ð±ÑŽÐ´Ð¶ÐµÑ‚Ð° Ñ„Ð¸Ð½Ð°Ð»Ð¸Ð·Ð°Ñ†Ð¸Ð¸ - Ð¼Ð¾Ð´ÐµÐ»ÑŒ Ð´Ð¾Ð»Ð³Ð¾Ð³Ð¾ ÑÐ±Ð¾Ñ€Ð° Ð¿Ð¾ tor Ð±ÐµÐ· Ñ€ÐµÐ°Ð»ÑŒÐ½Ñ‹Ñ…
// Ð¼Ð¸Ð½ÑƒÑ‚ Ð² Ñ‚ÐµÑÑ‚Ðµ.
type delayedFetcher struct {
	delay time.Duration
	body  string
}

func (s *delayedFetcher) Fetch(ctx context.Context, _ httpc.Request) (*httpc.Response, error) {
	select {
	case <-time.After(s.delay):
		return &httpc.Response{Status: 200, Body: []byte(s.body)}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// TestCollectTaskFinalizesAfterLongWalk - ÐºÑÑ‚Ñ‡ÐµÑ€ Ð”3 ÑÑ‚Ð°Ð¿Ð° 179: Ð±ÑŽÐ´Ð¶ÐµÑ‚
// Ñ„Ð¸Ð½Ð°Ð»Ð¸Ð·Ð°Ñ†Ð¸Ð¸ Ð¾Ñ‚ÑÑ‡Ð¸Ñ‚Ñ‹Ð²Ð°Ð»ÑÑ Ð¾Ñ‚ ÐÐÐ§ÐÐ›Ð Ð²Ñ‹Ð·Ð¾Ð²Ð°, Ð¸ Ð¿Ð¾ÑÐ»Ðµ Ð¾Ð±Ñ…Ð¾Ð´Ð° Ð´Ð»Ð¸Ð½Ð½ÐµÐµ 30s
// UpdateTask Ð¿Ð°Ð´Ð°Ð» Ð½Ð° Ð¸ÑÑ‚Ñ‘ÐºÑˆÐµÐ¼ ÐºÐ¾Ð½Ñ‚ÐµÐºÑÑ‚Ðµ Ð¼Ð¾Ð»Ñ‡Ð° - tasks_running Ñ€Ð¾Ñ
// Ð¼Ð¾Ð½Ð¾Ñ‚Ð¾Ð½Ð½Ð¾ (ÑÐ¼Ð¾ÑƒÐº-C: 8 running Ð¸Ð· 9 Ð·Ð°Ð´Ð°Ñ‡ ÑÐ¿ÑƒÑÑ‚Ñ Ð¿Ð¾Ð»Ð¼Ð¸Ð½ÑƒÑ‚Ñ‹ Ð¿Ð¾ÑÐ»Ðµ Ð¾Ñ‚Ð²ÐµÑ‚Ð¾Ð²).
// Ð¢ÐµÑÑ‚ ÑÐ¶Ð¸Ð¼Ð°ÐµÑ‚ Ð²Ñ€ÐµÐ¼Ñ: Ð±ÑŽÐ´Ð¶ÐµÑ‚ 60ms, Ð¾Ð±Ñ…Ð¾Ð´ 300ms - Ñ„Ð¸Ð½Ð°Ð»Ð¸Ð·Ð°Ñ†Ð¸Ñ Ð¾Ð±ÑÐ·Ð°Ð½Ð°
// ÑƒÑÐ¿ÐµÑ‚ÑŒ, Ð¿Ð¾Ñ‚Ð¾Ð¼Ñƒ Ñ‡Ñ‚Ð¾ ÐµÑ‘ Ð±ÑŽÐ´Ð¶ÐµÑ‚ ÑÑ‚Ð°Ñ€Ñ‚ÑƒÐµÑ‚ ÐŸÐžÐ¡Ð›Ð• Ð¾Ð±Ñ…Ð¾Ð´Ð°.
func TestCollectTaskFinalizesAfterLongWalk(t *testing.T) {
	dir := t.TempDir()
	st := exportStore(t, dir)
	seedMCPBase(t, st, dir)

	fetcher := &delayedFetcher{delay: 300 * time.Millisecond, body: `<a href="/dump.sql">d</a>`}
	col := catalog.NewCollector(fetcher, st, nil, nil, catalog.Config{PerHostDelay: time.Millisecond, PageTimeout: 5 * time.Second})
	d := Deps{Version: "test", Store: st, Collector: col, Started: time.Now()}

	saved := finalizeBudget
	finalizeBudget = 60 * time.Millisecond
	t.Cleanup(func() { finalizeBudget = saved })

	req := mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "collect_files",
			Arguments: map[string]any{"hosts": []any{"abcdefghijklmnop.onion"}, "timeout": 60},
		},
	}
	if _, err := d.collectFilesHandler(context.Background(), req); err != nil {
		t.Fatalf("collectFilesHandler: %v", err)
	}

	qctx, qcancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer qcancel()
	var status string
	row := mcpDB(t, dir).QueryRowContext(qctx, `SELECT status FROM tasks ORDER BY created_at DESC LIMIT 1`)
	if err := row.Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "done" {
		t.Fatalf("status=%q, Ñ…Ð¾Ñ‡Ñƒ done: Ð±ÑŽÐ´Ð¶ÐµÑ‚ Ñ„Ð¸Ð½Ð°Ð»Ð¸Ð·Ð°Ñ†Ð¸Ð¸ Ð¸ÑÑ‚Ñ‘Ðº Ð´Ð¾ Ð·Ð°Ð¿Ð¸ÑÐ¸ ÑÑ‚Ð°Ñ‚ÑƒÑÐ° (ÑÑ‡Ð¸Ñ‚Ð°ÐµÑ‚ÑÑ Ð¾Ñ‚ Ð½Ð°Ñ‡Ð°Ð»Ð° Ð²Ñ‹Ð·Ð¾Ð²Ð°, Ð° Ð½Ðµ Ð¾Ñ‚ ÐºÐ¾Ð½Ñ†Ð° Ð¾Ð±Ñ…Ð¾Ð´Ð°)", status)
	}
}

// TestCollectTaskIDsUniqueUnderBurst - ÐºÑÑ‚Ñ‡ÐµÑ€ Ð”1 ÑÑ‚Ð°Ð¿Ð° 179: Ð¿Ð°Ñ€Ð°Ð»Ð»ÐµÐ»ÑŒÐ½Ñ‹Ðµ
// Ð²Ñ‹Ð·Ð¾Ð²Ñ‹ Ð² Ð¾Ð´Ð½Ñƒ Ð¼Ð¸Ð»Ð»Ð¸ÑÐµÐºÑƒÐ½Ð´Ñƒ Ð¿Ð¾Ð»ÑƒÑ‡Ð°Ð»Ð¸ Ð¾Ð´Ð¸Ð½Ð°ÐºÐ¾Ð²Ñ‹Ð¹ task_id, CreateTask Ð½Ð°
// ÐºÐ¾Ð½Ñ„Ð»Ð¸ÐºÑ‚Ðµ Ð·Ð°Ñ‚Ð¸Ñ€Ð°Ð» Ñ‡ÑƒÐ¶ÑƒÑŽ Ð·Ð°Ð¿Ð¸ÑÑŒ, tasks_total Ñ‚ÐµÑ€ÑÐ» Ð·Ð°Ð´Ð°Ñ‡Ñƒ. ÐŸÐ°Ñ‡ÐºÐ°
// Ð¾Ð´Ð½Ð¾Ð²Ñ€ÐµÐ¼ÐµÐ½Ð½Ñ‹Ñ… Ð²Ñ‹Ð·Ð¾Ð²Ð¾Ð² Ð¾Ð±ÑÐ·Ð°Ð½Ð° Ð´Ð°Ñ‚ÑŒ Ð¿Ð°Ñ‡ÐºÑƒ ÑƒÐ½Ð¸ÐºÐ°Ð»ÑŒÐ½Ñ‹Ñ… id.
func TestCollectTaskIDsUniqueUnderBurst(t *testing.T) {
	dir := t.TempDir()
	st := exportStore(t, dir)
	seedMCPBase(t, st, dir)

	col := catalog.NewCollector(&stubFetcher{body: `<a href="/dump.sql">d</a>`}, st, nil, nil, catalog.Config{PerHostDelay: time.Millisecond})
	d := Deps{Version: "test", Store: st, Collector: col, Started: time.Now()}

	const n = 24
	ids := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := d.collectFilesHandler(context.Background(), mcp.CallToolRequest{
				Params: mcp.CallToolParams{
					Name:      "collect_files",
					Arguments: map[string]any{"hosts": []any{fmt.Sprintf("abcdefghijklmnop%d.onion", i)}, "timeout": 60},
				},
			})
			if err != nil {
				t.Errorf("Ð²Ñ‹Ð·Ð¾Ð² %d: %v", i, err)
				return
			}
			txt := res.Content[0].(mcp.TextContent).Text
			var out map[string]any
			if err := json.Unmarshal([]byte(txt), &out); err != nil {
				t.Errorf("Ð²Ñ‹Ð·Ð¾Ð² %d: json: %v", i, err)
				return
			}
			id, _ := out["task_id"].(string)
			ids[i] = id
		}(i)
	}
	wg.Wait()

	seen := map[string]bool{}
	for i, id := range ids {
		if id == "" {
			t.Fatalf("Ð²Ñ‹Ð·Ð¾Ð² %d: task_id Ð¿ÑƒÑÑ‚ Ð² Ð¾Ñ‚Ð²ÐµÑ‚Ðµ", i)
		}
		if seen[id] {
			t.Fatalf("task_id %q Ð¿Ð¾Ð²Ñ‚Ð¾Ñ€Ð¸Ð»ÑÑ: ÐºÐ¾Ð»Ð»Ð¸Ð·Ð¸Ñ Ð¿Ð°Ñ€Ð°Ð»Ð»ÐµÐ»ÑŒÐ½Ñ‹Ñ… Ð²Ñ‹Ð·Ð¾Ð²Ð¾Ð² (tasks_total Ñ‚ÐµÑ€ÑÐµÑ‚ Ð·Ð°Ð´Ð°Ñ‡Ñƒ, Ð¼ÐµÑ‚ÐºÐ¸ ÐºÐ°Ñ‚Ð°Ð»Ð¾Ð³Ð° Ð½ÐµÑ€Ð°Ð·Ð»Ð¸Ñ‡Ð¸Ð¼Ñ‹)", id)
		}
		seen[id] = true
	}

	qctx, qcancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer qcancel()
	var total int
	if err := mcpDB(t, dir).QueryRowContext(qctx, `SELECT COUNT(*) FROM tasks`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != n {
		t.Fatalf("Ð·Ð°Ð´Ð°Ñ‡ Ð² Ð±Ð°Ð·Ðµ %d, Ñ…Ð¾Ñ‡Ñƒ %d: ÐºÐ¾Ð½Ñ„Ð»Ð¸ÐºÑ‚Ñ‹ id Ð·Ð°Ñ‚Ñ‘Ñ€Ð»Ð¸ Ð·Ð°Ð¿Ð¸ÑÐ¸", total, n)
	}
}

// TestCollectSavedSumMatchesCatalogUnderParallelRace - ÐºÑÑ‚Ñ‡ÐµÑ€ Ð”2 ÑÑ‚Ð°Ð¿Ð°
// 179: Ð´Ð²Ð° Ð¿Ð°Ñ€Ð°Ð»Ð»ÐµÐ»ÑŒÐ½Ñ‹Ñ… collect_files Ð½Ð° Ð¾Ð´Ð½Ð¾Ð¼ Ñ…Ð¾ÑÑ‚Ðµ. ÐšÐ°Ð¶Ð´Ñ‹Ð¹ Ð¿Ñ€Ð¾Ñ…Ð¾Ð´Ð¸Ñ‚
// FileKnown Ð´Ð¾ Ð²ÑÑ‚Ð°Ð²ÐºÐ¸ ÑÐ¾ÑÐµÐ´Ð°, AddFile Ð²Ñ‚Ð¾Ñ€Ð¾Ð³Ð¾ ÑÑ€Ð°Ð±Ð°Ñ‚Ñ‹Ð²Ð°ÐµÑ‚ ÐºÐ°Ðº
// CONFLICT-Ð¾Ð±Ð½Ð¾Ð²Ð»ÐµÐ½Ð¸Ðµ - Ð½Ð¾ Ð¿Ñ€ÐµÐ¶Ð½Ð¸Ð¹ ÐºÐ¾Ð´ ÑÑ‡Ð¸Ñ‚Ð°Ð» ÐµÐ³Ð¾ Ð² saved. Ð˜Ð½Ð²Ð°Ñ€Ð¸Ð°Ð½Ñ‚:
// ÑÑƒÐ¼Ð¼Ð° saved Ð´Ð²ÑƒÑ… Ð¿Ñ€Ð¾Ð³Ð¾Ð½Ð¾Ð² Ñ€Ð°Ð²Ð½Ð° Ñ‡Ð¸ÑÐ»Ñƒ ÑƒÐ½Ð¸ÐºÐ°Ð»ÑŒÐ½Ñ‹Ñ… Ñ„Ð°Ð¹Ð»Ð¾Ð² Ñ…Ð¾ÑÑ‚Ð°, Ð°
// Ñ€Ð°Ð·Ð½Ð¸Ñ†Ð° Ñ‡ÐµÑÑ‚Ð½Ð¾ ÑƒÑ…Ð¾Ð´Ð¸Ñ‚ Ð² revisited.
func TestCollectSavedSumMatchesCatalogUnderParallelRace(t *testing.T) {
	dir := t.TempDir()
	st := exportStore(t, dir)

	const files = 8
	var sb strings.Builder
	sb.WriteString("<html><body>")
	for i := 0; i < files; i++ {
		fmt.Fprintf(&sb, `<a href="/f%d.sql">f%d</a>`, i, i)
	}
	sb.WriteString("</body></html>")

	// Ð—Ð°Ð´ÐµÑ€Ð¶ÐºÐ° Ð² Ð¾Ð±Ñ…Ð¾Ð´Ðµ Ð²Ñ‹Ñ€Ð°Ð²Ð½Ð¸Ð²Ð°ÐµÑ‚ ÑÑ‚Ð°Ñ€Ñ‚Ñ‹ Ð·Ð°Ð¿Ð¸ÑÐµÐ¹: Ð¾Ð±Ð° Ð¿Ñ€Ð¾Ð³Ð¾Ð½Ð° Ð¿Ñ€Ð¸Ñ…Ð¾Ð´ÑÑ‚
	// Ðº AddFile Ð¿Ð¾Ñ‡Ñ‚Ð¸ Ð¾Ð´Ð½Ð¾Ð²Ñ€ÐµÐ¼ÐµÐ½Ð½Ð¾, Ð¸ Ð¾ÐºÐ½Ð¾ FileKnown-Ð´Ð¾-Ð²ÑÑ‚Ð°Ð²ÐºÐ¸-ÑÐ¾ÑÐµÐ´Ð°
	// Ð¾Ñ‚ÐºÑ€Ñ‹Ð²Ð°ÐµÑ‚ÑÑ Ð½Ð°Ð´Ñ‘Ð¶Ð½Ð¾.
	col := catalog.NewCollector(&delayedFetcher{delay: 150 * time.Millisecond, body: sb.String()}, st, nil, nil,
		catalog.Config{PerHostDelay: time.Millisecond, PageTimeout: 5 * time.Second, Concurrency: 1})

	host := "abcdefghijklmnopqrstuv.onion"
	var a, b catalog.Report
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		rep, err := col.Collect(context.Background(), []string{host}, "taskA")
		if err != nil {
			t.Errorf("Ð¿Ñ€Ð¾Ð³Ð¾Ð½ A: %v", err)
			return
		}
		a = rep
	}()
	go func() {
		defer wg.Done()
		rep, err := col.Collect(context.Background(), []string{host}, "taskB")
		if err != nil {
			t.Errorf("Ð¿Ñ€Ð¾Ð³Ð¾Ð½ B: %v", err)
			return
		}
		b = rep
	}()
	wg.Wait()

	sum := a.Saved + b.Saved
	if sum != files {
		t.Fatalf("ÑÑƒÐ¼Ð¼Ð° saved Ð´Ð²ÑƒÑ… Ð¿Ñ€Ð¾Ð³Ð¾Ð½Ð¾Ð² = %d, Ñ…Ð¾Ñ‡Ñƒ %d (ÑƒÐ½Ð¸ÐºÐ°Ð»ÑŒÐ½Ñ‹Ñ… Ñ„Ð°Ð¹Ð»Ð¾Ð²): ÑÑ‡Ñ‘Ñ‚Ñ‡Ð¸Ðº ÑÑ‡Ð¸Ñ‚Ð°ÐµÑ‚ CONFLICT-Ð¾Ð±Ð½Ð¾Ð²Ð»ÐµÐ½Ð¸Ñ Ð²ÑÑ‚Ð°Ð²ÐºÐ°Ð¼Ð¸ - saved Ð²Ñ€Ñ‘Ñ‚ Ð¿Ñ€Ð¸ Ð¿Ð°Ñ€Ð°Ð»Ð»ÐµÐ»ÑŒÐ½Ð¾Ð¼ ÑÐ±Ð¾Ñ€Ðµ (A.saved=%d B.saved=%d A.revisited=%d B.revisited=%d)",
			sum, files, a.Saved, b.Saved, a.Revisited, b.Revisited)
	}

	qctx, qcancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer qcancel()
	var inCatalog int
	if err := mcpDB(t, dir).QueryRowContext(qctx, `SELECT COUNT(*) FROM file_catalog`).Scan(&inCatalog); err != nil {
		t.Fatal(err)
	}
	if inCatalog != files {
		t.Fatalf("Ð² ÐºÐ°Ñ‚Ð°Ð»Ð¾Ð³Ðµ %d Ð·Ð°Ð¿Ð¸ÑÐµÐ¹, Ñ…Ð¾Ñ‡Ñƒ %d: Ð³Ð¾Ð½ÐºÐ° Ð·Ð°Ð´Ð²Ð¾Ð¸Ð»Ð° Ñ„Ð°Ð¹Ð»Ñ‹", inCatalog, files)
	}
}
