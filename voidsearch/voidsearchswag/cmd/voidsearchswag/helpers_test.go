package main

import (
	"context"
	"sync"
	"testing"

	"voidsearchswag/internal/store"
)

// openTestStore создаёт базу во временном каталоге и накатывает миграции.
// Команды CLI открывают базу по пути из конфига, поэтому каталог данных
// подставляется через переменную окружения в самом тесте.
//
// Закрытие идемпотентно: тест закрывает базу явно через closeStore, чтобы
// команда открыла тот же файл без конкуренции за блокировку, а t.Cleanup
// подстраховывает на случай падения теста посередине.
func openTestStore(t *testing.T, dir string) *store.Store {
	t.Helper()
	st, err := store.Open(dir + "/voidsearchswag.db")
	if err != nil {
		t.Fatalf("база: %v", err)
	}
	if err := st.Migrate(context.Background()); err != nil {
		st.Close()
		t.Fatalf("миграции: %v", err)
	}
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { st.Close() }) })
	return st
}

// closeStore закрывает базу до запуска команды CLI. Закрытая база
// освобождает файл, иначе команда откроет её второй раз поверх первой.
func closeStore(st *store.Store) { st.Close() }

func mustAddFile(t *testing.T, st *store.Store, url, name, ext string, size int64, taskID, sourcePage string) {
	t.Helper()
	f := store.FileEntry{
		TaskID:     taskID,
		URL:        url,
		Filename:   name,
		Ext:        ext,
		Size:       size,
		SourcePage: sourcePage,
	}
	if err := st.AddFile(context.Background(), f); err != nil {
		t.Fatalf("add file %s: %v", url, err)
	}
}

func storeOnion(url, title, status string, rate float64, latency int64) store.Onion {
	return store.Onion{
		URL:         url,
		Title:       title,
		Status:      status,
		SuccessRate: rate,
		LatencyAvg:  latency,
	}
}
