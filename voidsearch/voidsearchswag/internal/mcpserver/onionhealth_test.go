package mcpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"voidsearchswag/internal/httpc"
	"voidsearchswag/internal/search"
	"voidsearchswag/internal/searchers"
	"voidsearchswag/internal/store"
)

const torchOnionBase = "http://torchdeedp3i2jigzjdmfpn5ttjhthh5wbmda2rr3jvqjg5p77c54dqd.onion"

func directHealthClient(t *testing.T) *httpc.Client {
	t.Helper()
	c, err := httpc.NewClient(context.Background(), httpc.Options{
		Transport: "direct",
		Timeout:   5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// noTorOnionEngine собирает ядро с одним onion-движком на прямом клиенте:
// проверка не может состояться, потому что tor не поднят.
func noTorOnionEngine(t *testing.T, name string) (*search.Engine, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/s.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	client := directHealthClient(t)
	e := &searchers.OnionEngine{
		Name_:  name,
		Base:   torchOnionBase,
		Path:   "/search?q={q}",
		Client: client,
	}
	return &search.Engine{
		Store:  st,
		Onion:  &searchers.OnionCatalog{Engines: []*searchers.OnionEngine{e}},
		Health: searchers.NewHealthPool(client, []*searchers.OnionEngine{e}),
	}, st
}

func TestOnionHealthProbeReportsSkipped(t *testing.T) {
	// Клиент получал live=0 total=1 и делал вывод, что движок умер. Пропуск
	// обязан быть виден отдельным полем, а состояние движка - остаться прежним.
	eng, st := noTorOnionEngine(t, "torch")
	srv := New(Deps{Version: "test", Store: st, Search: eng, Started: time.Now()})
	c := inProcess(t, srv)
	initClient(t, c)

	out := callToolArgs(t, c, "onion_health", map[string]any{"probe": true})

	if out["probed"] != true {
		t.Errorf("probed = %v, ожидала true", out["probed"])
	}
	if skipped, _ := out["skipped"].(float64); skipped != 1 {
		t.Errorf("skipped = %v, ожидала 1 (%+v)", out["skipped"], out)
	}
	if total, _ := out["total"].(float64); total != 1 {
		t.Errorf("total = %v, ожидала 1", out["total"])
	}
	note, _ := out["note"].(string)
	if !strings.Contains(note, "нужен tor или прокси") {
		t.Errorf("note = %q, ожидала причину пропуска", note)
	}
	if !strings.Contains(note, "состояние движков не менялось") {
		t.Errorf("note = %q, ожидала заверение о сохранности состояния", note)
	}
	if st, ok := eng.Health.Get("torch"); !ok {
		t.Fatal("движок пропал из пула")
	} else if st.Probes != 0 || st.FailStreak != 0 || st.Disabled {
		t.Errorf("состояние испорчено несостоявшейся проверкой: %+v", st)
	}
}

func TestOnionHealthProbeWithoutSkipsHasNoNote(t *testing.T) {
	// Обратная сторона: когда проверка состоялась, поля skipped и note не
	// появляются - иначе клиент перестанет отличать пропуск от отказа.
	st, err := store.Open(t.TempDir() + "/s.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	srvHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`<html><body><a class="res" href="http://abcdefabcdefabcd.onion/">файлы</a></body></html>`))
	}))
	t.Cleanup(srvHTTP.Close)

	client := directHealthClient(t)
	e := &searchers.OnionEngine{
		Name_:    "torch",
		Base:     srvHTTP.URL,
		Path:     "/search?q={q}",
		Selector: "a.res",
		Client:   client,
	}
	eng := &search.Engine{
		Store:  st,
		Onion:  &searchers.OnionCatalog{Engines: []*searchers.OnionEngine{e}},
		Health: searchers.NewHealthPool(client, []*searchers.OnionEngine{e}),
	}

	srv := New(Deps{Version: "test", Store: st, Search: eng, Started: time.Now()})
	c := inProcess(t, srv)
	initClient(t, c)

	out := callToolArgs(t, c, "onion_health", map[string]any{"probe": true})

	if _, ok := out["skipped"]; ok {
		t.Errorf("skipped появился без пропусков: %+v", out)
	}
	if _, ok := out["note"]; ok {
		t.Errorf("note появился без пропусков: %+v", out)
	}
	if live, _ := out["live"].(float64); live != 1 {
		t.Errorf("live = %v, ожидала 1: живой движок не учтён", out["live"])
	}
	if st, _ := eng.Health.Get("torch"); st.Probes != 1 || !st.Live {
		t.Errorf("состояние %+v, ожидала одну пробу и Live=true", st)
	}
}
