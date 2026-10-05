package hunt

import (
	"context"
	"testing"
	"time"

	"voidsearchswag/internal/store"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/h.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// searchStub собирает SearchFunc из функции, возвращающей список URL. Отчёт
// движков оставлен нулевым: EnginesTotal == 0 означает «движки не считались»,
// поэтому пустая выдача в таких тестах остаётся пустой выдачей, а не отказом.
func searchStub(fn func() []string) SearchFunc {
	return func(context.Context, string, string, int) (SearchOutcome, error) {
		return SearchOutcome{URLs: fn()}, nil
	}
}

func TestCreateRejectsEmptyQuery(t *testing.T) {
	r := &Runner{Store: openStore(t)}
	if _, err := r.Create(context.Background(), "  ", "auto", 60); err == nil {
		t.Error("пустой запрос принят")
	}
}

func TestFirstRunIsBaseline(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	r := &Runner{Store: st, Search: searchStub(func() []string {
		return []string{"http://a.onion", "http://b.onion"}
	})}
	id, err := r.Create(ctx, "leak", "deep", 60)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := r.RunDue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Checked != 1 || len(rep.Hits) != 0 {
		t.Errorf("первый прогон - база, а не находка: %+v (id=%d)", rep, id)
	}
}

func TestChangedHashIsHit(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	urls := []string{"http://a.onion"}
	now := time.Now()
	r := &Runner{Store: st, Now: func() time.Time { return now },
		Search: searchStub(func() []string { return urls })}
	if _, err := r.Create(ctx, "leak", "deep", 60); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	urls = []string{"http://a.onion", "http://new.onion"}
	now = now.Add(2 * time.Hour)
	rep, err := r.RunDue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Hits) != 1 {
		t.Fatalf("смена выдачи не замечена: %+v", rep)
	}
	if rep.Hits[0].Count != 2 || !rep.Hits[0].Changed {
		t.Errorf("хит неполный: %+v", rep.Hits[0])
	}
}

func TestUnchangedHashNoHit(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	now := time.Now()
	r := &Runner{Store: st, Now: func() time.Time { return now },
		Search: searchStub(func() []string { return []string{"http://a.onion"} })}
	if _, err := r.Create(ctx, "leak", "deep", 60); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Hour)
	rep, err := r.RunDue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Hits) != 0 {
		t.Errorf("ложная находка на той же выдаче: %+v", rep)
	}
}

func TestScheduleSkipsFresh(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	calls := 0
	now := time.Now()
	r := &Runner{Store: st, Now: func() time.Time { return now },
		Search: searchStub(func() []string {
			calls++
			return []string{"http://a.onion"}
		})}
	if _, err := r.Create(ctx, "leak", "deep", 360); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	rep, err := r.RunDue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Skipped != 1 || calls != 1 {
		t.Errorf("свежая охота прогнана повторно: %+v calls=%d", rep, calls)
	}
	now = now.Add(7 * time.Hour)
	rep, err = r.RunDue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("просроченная охота не прогнана: %+v calls=%d", rep, calls)
	}
}

func TestWatchSeesChangeImmediately(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	urls := []string{"http://a.onion"}
	r := &Runner{Store: st, Search: searchStub(func() []string { return urls })}
	id, err := r.Create(ctx, "leak", "deep", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.RunOne(ctx, id); err != nil {
		t.Fatal(err)
	}
	urls = []string{"http://a.onion", "http://new.onion"}
	hits, timedOut, err := r.Watch(ctx, id, 5*time.Second, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if timedOut || len(hits) != 1 {
		t.Errorf("смена не замечена: hits=%v timedOut=%v", hits, timedOut)
	}
}

func TestWatchTimeout(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	r := &Runner{Store: st, Search: searchStub(func() []string { return []string{"http://a.onion"} })}
	id, err := r.Create(ctx, "leak", "deep", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.RunOne(ctx, id); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	hits, timedOut, err := r.Watch(ctx, id, 300*time.Millisecond, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !timedOut || len(hits) != 0 {
		t.Errorf("без смены обязан выйти по таймауту: hits=%v timedOut=%v", hits, timedOut)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("watch висел дольше разумного")
	}
}

func TestWatchUnknownID(t *testing.T) {
	r := &Runner{Store: openStore(t), Search: searchStub(func() []string { return nil })}
	if _, _, err := r.Watch(context.Background(), 999999, time.Second, 100*time.Millisecond); err == nil {
		t.Error("неизвестный id принят")
	}
}

func TestWatchRespectsCancel(t *testing.T) {
	st := openStore(t)
	r := &Runner{Store: st, Search: searchStub(func() []string { return []string{"http://a.onion"} })}
	ctx := context.Background()
	id, err := r.Create(ctx, "leak", "deep", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.RunOne(ctx, id); err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := r.Watch(cctx, id, 10*time.Second, 50*time.Millisecond); err == nil {
		t.Error("отменённый контекст не остановил ожидание")
	}
}

func TestRunOneUnknownID(t *testing.T) {
	r := &Runner{Store: openStore(t), Search: searchStub(func() []string { return nil })}
	if _, err := r.RunOne(context.Background(), 999999); err == nil {
		t.Error("неизвестный id принят")
	}
}

func TestHashIgnoresOrderAndDupes(t *testing.T) {
	a := HashURLs([]string{"http://b.onion", "http://a.onion", "http://a.onion"})
	b := HashURLs([]string{"http://a.onion", "http://b.onion"})
	if a != b {
		t.Error("порядок и дубли влияют на hash")
	}
	if HashURLs(nil) == "" {
		t.Error("пустой hash пуст")
	}
}

func TestRunnerNeedsStoreAndSearch(t *testing.T) {
	r := &Runner{}
	if _, err := r.Create(context.Background(), "x", "auto", 1); err == nil {
		t.Error("создание без стора принято")
	}
	if _, err := r.RunDue(context.Background()); err == nil {
		t.Error("прогон без стора принят")
	}
	r.Store = openStore(t)
	if _, err := r.RunDue(context.Background()); err == nil {
		t.Error("прогон без поиска принят")
	}
}
