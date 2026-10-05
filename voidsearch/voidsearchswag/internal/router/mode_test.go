package router

import (
	"strings"
	"testing"
)

func TestRouteDeepSignals(t *testing.T) {
	cases := []string{
		"leak database 2024",
		"скачать базу слив",
		"onion marketplace",
		"any .onion site list",
		"CVE-2024-3094 exploit",
		"combo list telegram",
		"даркнет форум",
	}
	for _, q := range cases {
		d := Route(q)
		if d.Mode != ModeDeep {
			t.Errorf("Route(%q)=%s, ожидала deep (reason: %s)", q, d.Mode, d.Reason)
		}
		if len(d.Signals) == 0 {
			t.Errorf("Route(%q): сигналы не указаны", q)
		}
	}
}

func TestRouteStealthSignals(t *testing.T) {
	cases := []string{
		"book hotel booking.com",
		"avito объявления",
		"instagram profile",
		"cloudflare protected site",
		"капча solver",
	}
	for _, q := range cases {
		d := Route(q)
		if d.Mode != ModeStealth {
			t.Errorf("Route(%q)=%s, ожидала stealth", q, d.Mode)
		}
	}
}

func TestRouteOperatorsToFast(t *testing.T) {
	d := Route("site:example.com market")
	if d.Mode != ModeFast {
		t.Errorf("оператор site: -> %s, ожидала fast", d.Mode)
	}
	if !strings.Contains(d.Reason, "site:") {
		t.Errorf("reason=%q, оператор не отражён", d.Reason)
	}
}

func TestRouteFastSignals(t *testing.T) {
	for _, q := range []string{"погода москва", "news today", "что такое http"} {
		if d := Route(q); d.Mode != ModeFast {
			t.Errorf("Route(%q)=%s, ожидала fast", q, d.Mode)
		}
	}
}

func TestRouteDefaultIsFast(t *testing.T) {
	d := Route("купить кофе зерно оптом")
	if d.Mode != ModeFast {
		t.Errorf("нейтральный запрос -> %s, ожидала fast по умолчанию", d.Mode)
	}
	if len(d.Fallback) == 0 {
		t.Error("для режима по умолчанию должен быть фоллбэк")
	}
}

func TestRouteEmpty(t *testing.T) {
	d := Route("   ")
	if d.Mode != ModeFast {
		t.Errorf("пустой запрос -> %s, ожидала fast", d.Mode)
	}
}

func TestRouteCaseInsensitive(t *testing.T) {
	lower := Route("darknet market")
	upper := Route("DARKNET MARKET")
	if lower.Mode != upper.Mode {
		t.Errorf("регистр меняет режим: %s vs %s", lower.Mode, upper.Mode)
	}
}

func TestRouteDeepHasFallbackChain(t *testing.T) {
	d := Route("leak dump")
	if len(d.Fallback) < 2 {
		t.Errorf("у deep должна быть цепочка фоллбэков, есть %v", d.Fallback)
	}
	if d.Fallback[0] == ModeDeep {
		t.Error("первый фоллбэк не должен повторять основной режим")
	}
}

func TestParseModes(t *testing.T) {
	cases := map[string]Mode{
		"":        ModeAuto,
		"auto":    ModeAuto,
		"fast":    ModeFast,
		"stealth": ModeStealth,
		"deep":    ModeDeep,
		"tor":     ModeDeep,
		"onion":   ModeDeep,
		"DEEP":    ModeDeep,
		" deep ":  ModeDeep,
	}
	for in, want := range cases {
		got, ok := Parse(in)
		if !ok {
			t.Errorf("Parse(%q) не распознан", in)
			continue
		}
		if got != want {
			t.Errorf("Parse(%q)=%s, ожидала %s", in, got, want)
		}
	}
	if _, ok := Parse("nonsense"); ok {
		t.Error("мусорный режим не должен распознаваться")
	}
}

func TestFallbacksForEveryMode(t *testing.T) {
	for _, m := range []Mode{ModeFast, ModeStealth, ModeDeep} {
		fb := FallbacksFor(m)
		if len(fb) != 2 {
			t.Errorf("FallbacksFor(%s)=%v, ожидала 2 варианта", m, fb)
		}
		for _, f := range fb {
			if f == m {
				t.Errorf("FallbacksFor(%s) содержит сам себя", m)
			}
		}
	}
	if len(FallbacksFor(ModeAuto)) != 3 {
		t.Errorf("для auto ожидала 3 фоллбэка, есть %v", FallbacksFor(ModeAuto))
	}
}

func TestSignalsCapped(t *testing.T) {
	d := Route("onion leak dump combo paste creds shodan cve- exploit даркнет")
	if len(d.Signals) > 4 {
		t.Errorf("сигналов %d, ожидала не больше 4", len(d.Signals))
	}
}

func TestDeepBeatsStealth(t *testing.T) {
	d := Route("onion market amazon")
	if d.Mode != ModeDeep {
		t.Errorf("deep должен приоритетнее stealth, получено %s", d.Mode)
	}
}
