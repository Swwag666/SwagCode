package searchers

import (
	"testing"
	"time"
)

func TestRegisterAddsEngine(t *testing.T) {
	hp := NewHealthPool(nil, nil)
	e := &OnionEngine{Name_: "fresh", Base: "http://fresh.onion", Path: "/s?q={q}"}
	if !hp.Register(e) {
		t.Fatal("движок не зарегистрирован")
	}
	got, ok := hp.Get("fresh")
	if !ok {
		t.Fatal("движка нет в пуле")
	}
	if got.URL != "http://fresh.onion/s" {
		t.Errorf("probe URL %q", got.URL)
	}
	// Повтор - дубликат.
	if hp.Register(e) {
		t.Error("дубликат зарегистрирован")
	}
	if hp.Register(nil) || hp.Register(&OnionEngine{}) {
		t.Error("мусор зарегистрирован")
	}
}

func TestRegisterSurvivesMarkCycle(t *testing.T) {
	hp := NewHealthPool(nil, nil)
	hp.Register(&OnionEngine{Name_: "fresh", Base: "http://fresh.onion"})
	hp.MarkResult("fresh", true, 800*time.Millisecond)
	if live, total := hp.HealthyCount(); live != 1 || total != 1 {
		t.Errorf("live=%d total=%d", live, total)
	}
}
