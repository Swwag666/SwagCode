package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"voidsearchswag/internal/config"
	"voidsearchswag/internal/hunt"
	"voidsearchswag/internal/metrics"
	"voidsearchswag/internal/store"
)

func openHuntRunner(t *testing.T, urls []string) *hunt.Runner {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/bg.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return &hunt.Runner{Store: st, Search: func(context.Context, string, string, int) (hunt.SearchOutcome, error) {
		return hunt.SearchOutcome{URLs: urls}, nil
	}}
}

func TestHuntTickNotifiesOnHits(t *testing.T) {
	current := []string{"http://a.onion"}
	st, err := store.Open(t.TempDir() + "/bg.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := &hunt.Runner{Store: st, Search: func(context.Context, string, string, int) (hunt.SearchOutcome, error) {
		return hunt.SearchOutcome{URLs: current}, nil
	}}
	// Время под контролем: расписание вышло - тик обязан прогнать.
	now := time.Now()
	r.Now = func() time.Time { return now }
	ctx := context.Background()
	if _, err := r.Create(ctx, "leak", "deep", 60); err != nil {
		t.Fatal(err)
	}
	// База фиксируется, время уходит за расписание, выдача меняется -
	// тик обязан позвать notify.
	if _, err := r.RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Hour)
	current = []string{"http://a.onion", "http://new.onion/x"}

	var notified [][]hunt.Hit
	huntTick(ctx, stderrLogger{}, r, func(h []hunt.Hit) { notified = append(notified, h) })
	if len(notified) != 1 || len(notified[0]) != 1 {
		t.Errorf("notify вызван неверно: %+v", notified)
	}
}

func TestHuntTickSilentWithoutHits(t *testing.T) {
	r := openHuntRunner(t, []string{"http://a.onion"})
	ctx := context.Background()
	if _, err := r.Create(ctx, "leak", "deep", 1); err != nil {
		t.Fatal(err)
	}
	called := false
	huntTick(ctx, stderrLogger{}, r, func([]hunt.Hit) { called = true })
	if called {
		t.Error("notify на базовом прогоне без смены")
	}
}

func TestHuntTickNilRunner(t *testing.T) {
	// Фон не должен паниковать на несобранной охоте.
	huntTick(context.Background(), stderrLogger{}, nil, nil)
}

// Этап 178 (смоук-I): тик обязан писать счётчики фоновых прогонов - описание
// metrics обещает bg_hunt_runs/bg_hunt_hits, а до правки фон не писал в
// счётчики ничего: ответ metrics был пуст даже после часа живых тиков.
func TestHuntTickCountsBgRuns(t *testing.T) {
	r := openHuntRunner(t, []string{"http://a.onion"})
	ctx := context.Background()
	if _, err := r.Create(ctx, "leak", "deep", 1); err != nil {
		t.Fatal(err)
	}
	before := metrics.Default.Snapshot()["bg_hunt_runs"]
	huntTick(ctx, stderrLogger{}, r, nil)
	runs, hits := metrics.Default.Snapshot()["bg_hunt_runs"], metrics.Default.Snapshot()["bg_hunt_hits"]
	if runs != before+1 {
		t.Errorf("bg_hunt_runs=%d, хочу %d: тик не посчитан в счётчик фоновых прогонов", runs, before+1)
	}
	if hits < 0 {
		t.Errorf("bg_hunt_hits=%d отрицателен", hits)
	}
}

func TestEveryStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var n atomic.Int32
	every(ctx, 10*time.Millisecond, 10*time.Millisecond, func(context.Context) { n.Add(1) })
	time.Sleep(100 * time.Millisecond)
	if n.Load() == 0 {
		t.Fatal("тик ни разу не сработал")
	}
	cancel()
	// Замер после отмены: начавшийся тик успевает досчитаться, новых нет.
	time.Sleep(50 * time.Millisecond)
	got := n.Load()
	time.Sleep(50 * time.Millisecond)
	if n.Load() != got {
		t.Error("тик продолжает работать после отмены")
	}
}

func TestEveryZeroIntervalDoesNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	called := false
	every(ctx, 0, 0, func(context.Context) { called = true })
	time.Sleep(30 * time.Millisecond)
	if called {
		t.Error("нулевой интервал сработал")
	}
}

func TestDiscoverTickNilPool(t *testing.T) {
	// Без discovery-слоя тик молча пропускается, а не падает.
	discoverTick(context.Background(), stderrLogger{}, config.Config{}, nil, nil)
}
