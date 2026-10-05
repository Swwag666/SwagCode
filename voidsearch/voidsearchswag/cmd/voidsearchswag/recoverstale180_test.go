package main

import (
	"context"
	"testing"

	"voidsearchswag/internal/store"
)

// Этап 180. Кэтcher вызова recovery из run-пути: run обязан финализировать
// чужие running при старте - жёсткий kill посреди обхода оставлял задачу
// running навсегда, tasks_running в status врал вечно (смоук 179: 8 вечных
// running в базе стенда, рестарт на старом бинаре их не трогал - живой
// BEFORE r_before_status.json). Здесь прогоняется функция пути запуска.
func TestRecoverStaleTasksOnStartup(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir + "/s.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()

	for _, id := range []string{"mcp-old1", "mcp-old2", "mcp-old3"} {
		if err := st.CreateTask(ctx, store.Task{ID: id, Kind: "collect_files", Status: "running", Message: "хостов 1"}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	if err := recoverStaleTasks(ctx, st, quietCmdLog{}); err != nil {
		t.Fatalf("recoverStaleTasks: %v", err)
	}

	rest, err := st.ListTasks(ctx, "running", 100)
	if err != nil {
		t.Fatalf("list running: %v", err)
	}
	if len(rest) != 0 {
		t.Fatalf("после recovery осталось %d running: рестарт сервера не финализирует чужие задачи - tasks_running врёт вечно", len(rest))
	}
	for _, id := range []string{"mcp-old1", "mcp-old2", "mcp-old3"} {
		got, err := st.GetTask(ctx, id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if got.Status != "failed" {
			t.Fatalf("%s: статус %q, хочу failed", id, got.Status)
		}
	}
}

type quietCmdLog struct{}

func (quietCmdLog) Debugf(string, ...any) {}
func (quietCmdLog) Infof(string, ...any)  {}
func (quietCmdLog) Warnf(string, ...any)  {}
func (quietCmdLog) Errorf(string, ...any) {}
