package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"voidsearchswag/internal/config"
	"voidsearchswag/internal/httpc"
	"voidsearchswag/internal/promote"
	"voidsearchswag/internal/search"
	"voidsearchswag/internal/searchers"
	"voidsearchswag/internal/store"
)

func tickEngine(t *testing.T) (*store.Store, *search.Engine, *promote.Promoter) {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/tick.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	cl, err := httpc.NewClient(context.Background(), httpc.Options{Transport: "direct", Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cl.Close() })
	eng := &search.Engine{
		Client: cl,
		Onion:  &searchers.OnionCatalog{},
		Health: searchers.NewHealthPool(cl, nil),
	}
	p := &promote.Promoter{Client: cl, MinOnionHits: 2, Timeout: 10 * time.Second}
	return st, eng, p
}

func TestPromoteTickFindsEngine(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Write([]byte(`<html><body>
<form action="/s" method="get"><input type="text" name="q"></form>
</body></html>`))
			return
		}
		w.Write([]byte(`<html><body>
<a href="http://a1a1a1a1a1a1a1a1.onion/1">one has text here</a>
<a href="http://b2b2b2b2b2b2b2b2.onion/2">two has text here</a>
</body></html>`))
	}))
	defer srv.Close()

	st, eng, p := tickEngine(t)
	ctx := context.Background()
	if err := st.UpsertOnion(ctx, store.Onion{URL: srv.URL + "/", Status: "live"}); err != nil {
		t.Fatal(err)
	}
	if got := promoteTick(ctx, stderrLogger{}, st, eng, p, 5); got != 1 {
		t.Errorf("поднято %d, ожидала 1", got)
	}
	if len(eng.Onion.Engines) != 1 {
		t.Fatalf("каталог не пополнен")
	}
	seeds, err := st.ListEngineSeeds(ctx)
	if err != nil || len(seeds) != 1 {
		t.Errorf("сид не сохранён: %+v %v", seeds, err)
	}
	// Повторный тик дубликата не даёт.
	if got := promoteTick(ctx, stderrLogger{}, st, eng, p, 5); got != 0 {
		t.Errorf("повтор поднял %d", got)
	}
}

func TestPromoteTickNilGuards(t *testing.T) {
	st, eng, p := tickEngine(t)
	ctx := context.Background()
	if promoteTick(ctx, stderrLogger{}, nil, eng, p, 5) != 0 {
		t.Error("nil-стор принят")
	}
	if promoteTick(ctx, stderrLogger{}, st, nil, p, 5) != 0 {
		t.Error("nil-движок принят")
	}
	if promoteTick(ctx, stderrLogger{}, st, eng, nil, 5) != 0 {
		t.Error("nil-промоутер принят")
	}
	if promoteTick(ctx, stderrLogger{}, st, eng, p, 0) != 0 {
		t.Error("нулевой лимит принят")
	}
	eng.Onion = nil
	if promoteTick(ctx, stderrLogger{}, st, eng, p, 5) != 0 {
		t.Error("каталог nil принят")
	}
}

func TestBackupTickCreatesSnapshot(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/tick.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "backups")
	backupTick(ctx, stderrLogger{}, st, dir, 7)
	matches, err := filepath.Glob(filepath.Join(dir, "voidsearchswag-*.db"))
	if err != nil || len(matches) != 1 {
		t.Errorf("снимков %+v, ошибка %v", matches, err)
	}
	// Nil-стор не паникует.
	backupTick(ctx, stderrLogger{}, nil, dir, 7)
}

func TestStartBackgroundDisabled(t *testing.T) {
	// Всё выключено: ни горутин с сетью, ни паники на nil-зависимостях.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := config.Config{}
	startBackground(ctx, stderrLogger{}, cfg, nil, nil, nil, nil, nil, nil, nil)
	time.Sleep(30 * time.Millisecond)
	_ = os.Getenv("PATH")
}
