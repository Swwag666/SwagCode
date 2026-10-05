package store

import (
	"context"
	"testing"
)

// Этап 180. Рестарт или жёсткий kill сервера посреди обхода оставлял
// задачу running навсегда: финализацию никто не делал заново, а status
// честно считал вечный running. Смоук этапа 179 оставил в базе стенда
// 8 таких записей. Здесь проверяется ядро recovery: метод переводит
// все running в failed, не трогая завершённые записи.
func TestFailRunningTasks(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir + "/s.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()

	seed := []struct {
		id, status string
	}{
		{"mcp-a", "running"},
		{"mcp-b", "running"},
		{"mcp-c", "done"},
		{"mcp-d", "failed"},
	}
	for _, s := range seed {
		if err := st.CreateTask(ctx, Task{ID: s.id, Kind: "collect_files", Status: s.status, Message: "хостов 1"}); err != nil {
			t.Fatalf("seed %s: %v", s.id, err)
		}
	}

	n, err := st.FailRunningTasks(ctx, "прерван рестартом сервера")
	if err != nil {
		t.Fatalf("fail running: %v", err)
	}
	if n != 2 {
		t.Fatalf("переведено %d записей, хочу 2: счётчик recovery врёт", n)
	}

	for _, s := range seed {
		got, err := st.GetTask(ctx, s.id)
		if err != nil {
			t.Fatalf("get %s: %v", s.id, err)
		}
		if s.status == "running" {
			if got.Status != "failed" {
				t.Fatalf("%s: статус %q, хочу failed - running пережил recovery", s.id, got.Status)
			}
			if got.Message != "прерван рестартом сервера" {
				t.Fatalf("%s: сообщение %q, хочу пометку о причине", s.id, got.Message)
			}
			continue
		}
		if got.Status != s.status {
			t.Fatalf("%s: статус %q, хочу прежний %q - recovery затронул завершённую задачу", s.id, got.Status, s.status)
		}
		if got.Message != "хостов 1" {
			t.Fatalf("%s: сообщение %q, хочу прежнее - recovery переписал чужую историю", s.id, got.Message)
		}
	}
}
