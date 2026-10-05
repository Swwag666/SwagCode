package store

import (
	"context"
	"testing"
	"time"
)

func TestSaveLoadEngineHealth(t *testing.T) {
	st, err := Open(t.TempDir() + "/eh.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	when := time.Now().Truncate(time.Second)
	in := EngineHealth{
		Name: "torch", URL: "http://t.onion/search", Live: true,
		LatencyAvg: 1500, SuccessRate: 0.8, FailStreak: 0,
		Probes: 10, Successes: 8, LastProbe: when, Disabled: false,
	}
	if err := st.SaveEngineHealth(ctx, in); err != nil {
		t.Fatal(err)
	}
	got, err := st.LoadEngineHealth(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("записей %d, ожидала 1", len(got))
	}
	h := got[0]
	if h.Name != "torch" || !h.Live || h.LatencyAvg != 1500 || h.Probes != 10 || h.Successes != 8 {
		t.Errorf("снимок искажён: %+v", h)
	}
	if h.SuccessRate != 0.8 {
		t.Errorf("rate=%v", h.SuccessRate)
	}
}

func TestSaveEngineHealthUpsert(t *testing.T) {
	st, err := Open(t.TempDir() + "/eh.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	mk := func(probes int, live bool) EngineHealth {
		return EngineHealth{Name: "tornet", Probes: probes, Live: live}
	}
	if err := st.SaveEngineHealth(ctx, mk(1, true)); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveEngineHealth(ctx, mk(5, false)); err != nil {
		t.Fatal(err)
	}
	got, err := st.LoadEngineHealth(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("upsert дал %d строк вместо 1", len(got))
	}
	if got[0].Probes != 5 || got[0].Live {
		t.Errorf("вторая запись не перезаписала первую: %+v", got[0])
	}
}

func TestSaveEngineHealthRejectsEmptyName(t *testing.T) {
	st, err := Open(t.TempDir() + "/eh.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveEngineHealth(ctx, EngineHealth{}); err == nil {
		t.Error("пустое имя принято")
	}
}
