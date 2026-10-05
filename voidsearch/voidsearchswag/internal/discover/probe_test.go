package discover

import (
	"context"
	"testing"
	"time"
)

func TestProbeConfigDefaults(t *testing.T) {
	c := ProbeConfig{}.withDefaults()
	if c.Concurrency != 8 {
		t.Errorf("concurrency=%d, ожидала 8", c.Concurrency)
	}
	if c.Timeout != 30*time.Second {
		t.Errorf("timeout=%v, ожидала 30s", c.Timeout)
	}
}

func TestProberNilClientMarksDead(t *testing.T) {
	p := NewProber(nil, nil, nil, ProbeConfig{})
	rep := p.Probe(context.Background(), []string{v2a + ".onion", v2b + ".onion"})
	if rep.Total != 2 {
		t.Errorf("total=%d, ожидала 2", rep.Total)
	}
	if rep.Dead != 2 {
		t.Errorf("dead=%d, ожидала 2 (клиента нет)", rep.Dead)
	}
	if rep.Live != 0 {
		t.Errorf("live=%d, ожидала 0", rep.Live)
	}
	for _, r := range rep.Results {
		if r.Error == "" {
			t.Errorf("%s без причины ошибки", r.URL)
		}
	}
}

func TestProberDropsInvalidAddresses(t *testing.T) {
	p := NewProber(nil, nil, nil, ProbeConfig{})
	rep := p.Probe(context.Background(), []string{
		v2a + ".onion", "", "example.com", "aaa111bbb222cccc.onion",
	})
	if rep.Total != 1 {
		t.Errorf("total=%d, ожидала 1 (только валидный адрес)", rep.Total)
	}
}

func TestProberDedupesAddresses(t *testing.T) {
	p := NewProber(nil, nil, nil, ProbeConfig{})
	rep := p.Probe(context.Background(), []string{v2a + ".onion", v2a + ".onion"})
	if rep.Total != 1 {
		t.Errorf("total=%d, ожидала 1 (дубликат схлопнут)", rep.Total)
	}
}

func TestProberEmptyInput(t *testing.T) {
	p := NewProber(nil, nil, nil, ProbeConfig{})
	rep := p.Probe(context.Background(), nil)
	if rep.Total != 0 || rep.Live != 0 || rep.Dead != 0 {
		t.Errorf("на пустом входе что-то насчиталось: %+v", rep)
	}
}

func TestProberCancelledContext(t *testing.T) {
	p := NewProber(nil, nil, nil, ProbeConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rep := p.Probe(ctx, []string{v2a + ".onion"})
	// Этап 174: отмена живёт в Cancelled, а не строкой в LimitHit -
	// словарь пробы выровнен с crawl-отчётом (этап 172).
	if !rep.Cancelled {
		t.Error("отмена контекста не отражена в cancelled")
	}
	if rep.LimitHit != "" {
		t.Errorf("отмена - не предел, а limit_hit=%q: поле обязано молчать", rep.LimitHit)
	}
}

func TestProberResultsSortedByInput(t *testing.T) {
	p := NewProber(nil, nil, nil, ProbeConfig{Concurrency: 4})
	rep := p.Probe(context.Background(), []string{v2c + ".onion", v2a + ".onion", v2b + ".onion"})
	if len(rep.Results) != 3 {
		t.Fatalf("результатов %d, ожидала 3", len(rep.Results))
	}
	for i := 1; i < len(rep.Results); i++ {
		if rep.Results[i-1].URL > rep.Results[i].URL {
			t.Errorf("результаты не отсортированы: %v", rep.Results)
			break
		}
	}
}

func TestProbeWaveWithoutStoreErrors(t *testing.T) {
	p := NewProber(nil, nil, nil, ProbeConfig{})
	if _, err := p.ProbeWave(context.Background(), 5); err == nil {
		t.Error("без базы ProbeWave должен вернуть ошибку")
	}
}

func TestRateLimiterProbeDelay(t *testing.T) {
	rl := NewRateLimiter(0)
	if rl.Delay() != 2*time.Second {
		t.Errorf("нулевая задержка не заменена дефолтом: %v", rl.Delay())
	}
}
