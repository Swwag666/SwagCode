package metrics

import (
	"testing"
)

func TestIncAndSnapshot(t *testing.T) {
	c := New()
	c.Inc("a")
	c.Inc("a")
	c.Add("a", 3)
	c.Add("b", 2)
	if got := c.Snapshot()["a"]; got != 5 {
		t.Errorf("a=%d, ожидала 5", got)
	}
	// Копия изолирована: правка снимка не трогает реестр.
	snap := c.Snapshot()
	snap["a"] = 999
	if got := c.Snapshot()["a"]; got != 5 {
		t.Errorf("реестр испорчен через снимок: %d", got)
	}
}

func TestNilSafe(t *testing.T) {
	var c *Counters
	c.Inc("x")
	c.Add("x", 1)
	if got := c.Snapshot(); len(got) != 0 {
		t.Errorf("nil-реестр отдал %+v", got)
	}
	if names := c.Names(); len(names) != 0 {
		t.Errorf("nil-имена: %v", names)
	}
}

func TestIgnoresGarbage(t *testing.T) {
	c := New()
	c.Inc("")
	c.Add("", 5)
	c.Add("x", 0)
	c.Add("x", -3)
	if len(c.Snapshot()) != 0 {
		t.Errorf("мусор попал в реестр: %+v", c.Snapshot())
	}
}

func TestKeySanitizes(t *testing.T) {
	if got := Key("engine_ok", "rod-browser"); got != "engine_ok_rod_browser" {
		t.Errorf("key=%q", got)
	}
	if got := Key("a/../b", "  ", "c.d"); got != "a__b_c_d" {
		t.Errorf("key=%q", got)
	}
	if got := Key("", "  "); got != "" {
		t.Errorf("пустой key=%q", got)
	}
}

func TestNamesSorted(t *testing.T) {
	c := New()
	c.Inc("z")
	c.Inc("a")
	c.Inc("m")
	names := c.Names()
	want := []string{"a", "m", "z"}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("имена %v, ожидала %v", names, want)
		}
	}
}

func TestGlobalDefault(t *testing.T) {
	if Default == nil {
		t.Fatal("Default не инициализирован")
	}
	before := Default.Snapshot()["test_probe_key"]
	Default.Inc("test_probe_key")
	if got := Default.Snapshot()["test_probe_key"]; got != before+1 {
		t.Error("глобальный реестр не считает")
	}
}
