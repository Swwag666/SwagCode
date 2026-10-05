package catalog

import (
	"context"
	"testing"

	"voidsearchswag/internal/store"
)

// Этап 165, жалоба смоук-агента: collect_files отчитался saved=2 и task_id,
// но file_search по этому task_id вернул ноль записей - файлы уже лежали в
// каталоге от прежнего прогона, а происхождение (task_id первой находки)
// не перезаписывается. Задача обязана посчитать повторные находки отдельно
// и не выдавать их за свои новые.
func TestCollectCountsRevisitedFiles(t *testing.T) {
	st := newStore(t)
	f := &fakeClient{body: `<a href="/a.zip">a</a><a href="/b.pdf">b</a>`}
	c := NewCollector(f, st, nil, nil, fastCfg(Config{}))

	first, err := c.Collect(context.Background(), []string{v2a + ".onion"}, "task-first")
	if err != nil {
		t.Fatalf("первый сбор: %v", err)
	}
	if first.Saved != 2 {
		t.Fatalf("первый сбор: saved=%d, хочу 2", first.Saved)
	}
	if first.Revisited != 0 {
		t.Fatalf("первый сбор: revisited=%d, хочу 0", first.Revisited)
	}

	second, err := c.Collect(context.Background(), []string{v2a + ".onion"}, "task-second")
	if err != nil {
		t.Fatalf("повторный сбор: %v", err)
	}
	if second.Saved != 0 {
		t.Fatalf("повторный сбор: saved=%d, хочу 0 - файлы уже известны", second.Saved)
	}
	if second.Revisited != 2 {
		t.Fatalf("повторный сбор: revisited=%d, хочу 2", second.Revisited)
	}

	// Происхождение первой находки живёт: file_search по первой задаче
	// по-прежнему находит оба файла.
	files, err := st.SearchFiles(context.Background(), store.FileQuery{TaskID: "task-first"})
	if err != nil {
		t.Fatalf("поиск по первой задаче: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("у первой задачи %d файлов, хочу 2", len(files))
	}
	// И вторая задача честна в отчёте: её файлы - revisited, не saved.
	if second.Revisited != 2 || second.Saved != 0 {
		t.Fatalf("отчёт повторной задачи: saved=%d revisited=%d, хочу 0 и 2", second.Saved, second.Revisited)
	}
}

// Revisited не ест потолок файлов: повторные находки не новые записи, и
// MaxFiles режет только то, что ляжет в базу впервые.
func TestCollectRevisitedDoesNotConsumeMaxFiles(t *testing.T) {
	st := newStore(t)
	f := &fakeClient{body: `<a href="/a.zip">a</a><a href="/b.pdf">b</a>`}
	full := NewCollector(f, st, nil, nil, fastCfg(Config{}))
	if _, err := full.Collect(context.Background(), []string{v2a + ".onion"}, "t1"); err != nil {
		t.Fatal(err)
	}

	// Потолок 1: оба файла - повторные, ни один не ляжет в базу заново,
	// пересчёт обязан пройти оба, а не остановиться на первом.
	c := NewCollector(f, st, nil, nil, fastCfg(Config{MaxFiles: 1}))
	rep, err := c.Collect(context.Background(), []string{v2a + ".onion"}, "t2")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Revisited != 2 {
		t.Fatalf("revisited=%d, хочу 2: повторные находки не едят MaxFiles", rep.Revisited)
	}
	if rep.Saved != 0 {
		t.Fatalf("saved=%d, хочу 0", rep.Saved)
	}
	if rep.LimitHit != "" {
		t.Fatalf("LimitHit=%q: потолок не должен сработать на повторных", rep.LimitHit)
	}
}
