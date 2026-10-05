package search

import (
	"context"
	"strings"
	"testing"
	"time"

	"voidsearchswag/internal/metrics"
	"voidsearchswag/internal/searchers"
)

const torchOnionBase = "http://torchdeedp3i2jigzjdmfpn5ttjhthh5wbmda2rr3jvqjg5p77c54dqd.onion"

// onionEngineNoTor собирает onion-движок на прямом клиенте: httpc отвечает
// ErrOnionWithoutTor, то есть запрос даже не уходит в сеть.
func onionEngineNoTor(t *testing.T, name string) (*searchers.OnionEngine, *searchers.HealthPool) {
	t.Helper()
	client := testDirectClient(t)
	e := &searchers.OnionEngine{
		Name_:  name,
		Base:   torchOnionBase,
		Path:   "/search?q={q}",
		Client: client,
	}
	return e, searchers.NewHealthPool(client, []*searchers.OnionEngine{e})
}

func TestQueryEnginesWithoutTorKeepsHealth(t *testing.T) {
	// Поиск в deep опрашивает onion-движки и пишет исход в пул здоровья.
	// Отсутствие tor - не отказ движка: до правки MarkResult(false) валил
	// FailStreak, и живой замер показал, как один поиск без tor плюс одна
	// проверка превратили tornet из «успех 90%, проб 30» в «отключён, успех 84%,
	// проб 32». Отчёт движка при этом обязан остаться честным: выдачи нет.
	e, hp := onionEngineNoTor(t, "torch")
	hp.MarkResult("torch", true, 50*time.Millisecond)

	eng := &Engine{Health: hp, Metrics: metrics.New()}
	res, reports := eng.queryEnginesParallel(context.Background(), []*searchers.OnionEngine{e}, "leak database", 5)

	if len(res) != 0 {
		t.Errorf("выдача без tor: %v", res)
	}
	if len(reports) != 1 {
		t.Fatalf("отчётов %d, ожидала 1", len(reports))
	}
	if reports[0].OK {
		t.Error("движок помечен ответившим")
	}
	if !strings.Contains(reports[0].Error, "tor") {
		t.Errorf("в отчёте нет причины: %q", reports[0].Error)
	}

	st, _ := hp.Get("torch")
	if st.Probes != 1 || st.FailStreak != 0 {
		t.Errorf("счётчики испорчены: проб %d, серия %d, ожидала 1 и 0", st.Probes, st.FailStreak)
	}
	if !st.Live || st.Disabled {
		t.Errorf("состояние %+v, ожидала Live=true и Disabled=false", st)
	}
	if st.SuccessRate != 1 {
		t.Errorf("SuccessRate = %v, ожидала 1: несостоявшийся запрос не должен её снижать", st.SuccessRate)
	}
}

func TestQueryEnginesRealFailureStillMarksHealth(t *testing.T) {
	// Настоящий отказ обязан по-прежнему валить FailStreak: различение исходов
	// не должно превратиться в безнаказанность мёртвых движков.
	client := testDirectClient(t)
	e := &searchers.OnionEngine{
		Name_:  "dead",
		Base:   "http://127.0.0.1:1",
		Path:   "/search?q={q}",
		Client: client,
	}
	hp := searchers.NewHealthPool(client, []*searchers.OnionEngine{e})

	eng := &Engine{Health: hp, Metrics: metrics.New()}
	_, reports := eng.queryEnginesParallel(context.Background(), []*searchers.OnionEngine{e}, "leak database", 5)

	if len(reports) != 1 || reports[0].Error == "" {
		t.Fatalf("отчёты %+v", reports)
	}
	st, _ := hp.Get("dead")
	if st.Probes != 1 || st.FailStreak != 1 {
		t.Errorf("настоящий отказ не учтён: проб %d, серия %d", st.Probes, st.FailStreak)
	}
	if st.Live {
		t.Error("отказавший движок остался живым")
	}
}

func TestProbeOnionReportsSkipped(t *testing.T) {
	// ProbeOnion обязан вернуть число пропусков третьим значением: без него
	// onioncheck и onion_health печатали «живых 0 из 7» при выключенном tor, и
	// это читалось как массовая смерть движков.
	e, hp := onionEngineNoTor(t, "torch")
	eng := &Engine{Onion: &searchers.OnionCatalog{Engines: []*searchers.OnionEngine{e}}, Health: hp}

	live, total, skipped := eng.ProbeOnion(context.Background())

	if live != 0 || total != 1 {
		t.Errorf("live=%d total=%d, ожидала 0/1", live, total)
	}
	if skipped != 1 {
		t.Errorf("skipped = %d, ожидала 1", skipped)
	}
	if st, _ := hp.Get("torch"); st.Probes != 0 || st.FailStreak != 0 {
		t.Errorf("счётчики тронуты: %+v", st)
	}
}

func TestProbeOnionWithoutCatalogReportsNothing(t *testing.T) {
	// Граница: без каталога и пула все три числа нулевые, паники нет.
	eng := &Engine{}
	live, total, skipped := eng.ProbeOnion(context.Background())
	if live != 0 || total != 0 || skipped != 0 {
		t.Errorf("live=%d total=%d skipped=%d", live, total, skipped)
	}
}
