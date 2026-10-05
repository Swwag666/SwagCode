package searchers

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Реальные onion-адреса движков из рабочей базы: проверка httpc смотрит на
// суффикс .onion, и короткий поддельный адрес прошёл бы мимо части кода.
const (
	torchOnionHost = "torchdeedp3i2jigzjdmfpn5ttjhthh5wbmda2rr3jvqjg5p77c54dqd.onion"
	ahmiaOnionHost = "juhanurmihxlp77nkq76byazcldy2hlmovfu2epvl5ankdibsot4csyd.onion"
)

// onionNoTor собирает движок с onion-адресом и клиентом на прямом транспорте.
// Ровно в такой конфигурации проверка состояться не может: httpc возвращает
// ErrOnionWithoutTor, и о здоровье движка этот исход не говорит ничего.
func onionNoTor(t *testing.T, name, host string) *OnionEngine {
	t.Helper()
	return &OnionEngine{
		Name_:  name,
		Base:   "http://" + host,
		Path:   "/search?q={q}",
		Client: directClient(t),
	}
}

func TestProbeWithoutTorIsSkippedNotFailed(t *testing.T) {
	// До правки probeOne записывал отсутствие tor как отказ движка: росли
	// Probes и FailStreak, падала SuccessRate, Live снимался, а в LastError
	// ложился текст про tor. Счётчики обязаны остаться нетронутыми.
	e := onionNoTor(t, "torch", torchOnionHost)
	hp := NewHealthPool(e.Client, []*OnionEngine{e})

	rep := hp.ProbeAll(context.Background(), []*OnionEngine{e})

	if rep.Total != 1 {
		t.Errorf("Total = %d, ожидала 1", rep.Total)
	}
	if rep.Skipped != 1 {
		t.Errorf("Skipped = %d, ожидала 1: несостоявшаяся проверка не посчитана", rep.Skipped)
	}
	if rep.Probed != 0 || rep.Failed != 0 {
		t.Errorf("Probed = %d, Failed = %d, ожидала 0 и 0", rep.Probed, rep.Failed)
	}

	st, ok := hp.Get("torch")
	if !ok {
		t.Fatal("движок пропал из пула")
	}
	if st.Probes != 0 || st.Successes != 0 || st.FailStreak != 0 {
		t.Errorf("счётчики тронуты несостоявшейся проверкой: проб %d, успехов %d, серия %d",
			st.Probes, st.Successes, st.FailStreak)
	}
	if st.Live {
		t.Error("движок объявлен живым без проверки")
	}
	if st.Disabled {
		t.Error("движок отключен несостоявшейся проверкой")
	}
	if st.LastError != "" {
		t.Errorf("LastError = %q, ожидала пустую строку: отказа движка не было", st.LastError)
	}
	if st.SuccessRate != 0 {
		t.Errorf("SuccessRate = %v, ожидала 0", st.SuccessRate)
	}
}

func TestRepeatedProbesWithoutTorKeepEngineUsable(t *testing.T) {
	// Сценарий живого замера: три вызова onioncheck --no-tor выключали все
	// движки навсегда. Disabled снимается только при Live и SuccessRate >= 0.5,
	// а SuccessRate пожизненная - у torch она падала 38% -> 33% -> 30% -> 27% и
	// вернуться уже не могла.
	e := onionNoTor(t, "torch", torchOnionHost)
	hp := NewHealthPool(e.Client, []*OnionEngine{e})
	for i := 0; i < 4; i++ {
		hp.MarkResult("torch", true, 50*time.Millisecond)
	}
	before, _ := hp.Get("torch")
	if !before.Live || before.SuccessRate != 1 || before.Probes != 4 {
		t.Fatalf("фикстура не собралась: %+v", before)
	}

	for i := 0; i < 5; i++ {
		if rep := hp.ProbeAll(context.Background(), []*OnionEngine{e}); rep.Skipped != 1 {
			t.Fatalf("обход %d: Skipped = %d, ожидала 1", i+1, rep.Skipped)
		}
	}

	st, _ := hp.Get("torch")
	if st.Disabled {
		t.Error("движок отключен пятью несостоявшимися проверками")
	}
	if !st.Live {
		t.Error("движок потерял Live без единой настоящей проверки")
	}
	if st.Probes != before.Probes {
		t.Errorf("Probes = %d, ожидала %d", st.Probes, before.Probes)
	}
	if st.SuccessRate != before.SuccessRate {
		t.Errorf("SuccessRate = %v, ожидала %v", st.SuccessRate, before.SuccessRate)
	}
	if st.FailStreak != 0 {
		t.Errorf("FailStreak = %d, ожидала 0", st.FailStreak)
	}
	if live := hp.Live(); len(live) != 1 {
		t.Errorf("Live() вернул %d движков, ожидала 1: поиск остался бы без onion-источников", len(live))
	}
}

func TestProbeWithoutTorDoesNotPersist(t *testing.T) {
	// Пул пишет снимок в базу через колбэк. Несостоявшаяся проверка не должна
	// оставлять след и там: иначе испорченные счётчики пережили бы перезапуск.
	e := onionNoTor(t, "ahmia", ahmiaOnionHost)
	hp := NewHealthPool(e.Client, []*OnionEngine{e})
	var saved []EngineHealth
	hp.SetPersist(func(st EngineHealth) { saved = append(saved, st) })

	hp.ProbeAll(context.Background(), []*OnionEngine{e})

	if len(saved) != 0 {
		t.Errorf("несостоявшаяся проверка записана в базу: %+v", saved)
	}
}

func TestRealFailureStillCountsAfterSkipFix(t *testing.T) {
	// Обратная сторона: настоящий отказ движка обязан считаться как раньше.
	e := stubEngine(t, "internal error", 500, "torch")
	hp := NewHealthPool(e.Client, []*OnionEngine{e})

	rep := hp.ProbeAll(context.Background(), []*OnionEngine{e})

	if rep.Failed != 1 || rep.Probed != 1 || rep.Skipped != 0 {
		t.Errorf("отчёт %+v, ожидала Failed=1 Probed=1 Skipped=0", rep)
	}
	st, _ := hp.Get("torch")
	if st.FailStreak != 1 || st.Live || st.Probes != 1 {
		t.Errorf("состояние %+v, ожидала серию 1, Live=false, проб 1", st)
	}
	if st.LastError != "HTTP 500" {
		t.Errorf("LastError = %q", st.LastError)
	}
}

func TestThreeRealFailuresStillDisable(t *testing.T) {
	// Семантика отключения сохраняется: три настоящих отказа по-прежнему
	// выключают движок.
	e := stubEngine(t, "gone", 503, "torch")
	hp := NewHealthPool(e.Client, []*OnionEngine{e})
	for i := 0; i < healthFailThreshold; i++ {
		if rep := hp.ProbeAll(context.Background(), []*OnionEngine{e}); rep.Failed != 1 {
			t.Fatalf("обход %d: Failed = %d", i+1, rep.Failed)
		}
	}
	st, _ := hp.Get("torch")
	if !st.Disabled {
		t.Error("движок не отключен после трёх настоящих отказов")
	}
	if st.FailStreak != healthFailThreshold {
		t.Errorf("FailStreak = %d, ожидала %d", st.FailStreak, healthFailThreshold)
	}
}

func TestMixedProbeReportCountsBothKinds(t *testing.T) {
	// Обход из двух движков: один недоступен без tor, второй отвечает. Отчёт
	// обязан разделить исходы, а не свалить их в один счётчик.
	skipped := onionNoTor(t, "torch", torchOnionHost)
	live := stubEngine(t, resBody, 200, "ahmia")
	hp := NewHealthPool(live.Client, []*OnionEngine{skipped, live})

	rep := hp.ProbeAll(context.Background(), []*OnionEngine{skipped, live})

	if rep.Total != 2 {
		t.Errorf("Total = %d, ожидала 2", rep.Total)
	}
	if rep.Skipped != 1 || rep.Probed != 1 || rep.Failed != 0 {
		t.Errorf("отчёт %+v, ожидала Skipped=1 Probed=1 Failed=0", rep)
	}
	if st, _ := hp.Get("ahmia"); !st.Live || st.Probes != 1 {
		t.Errorf("живой движок %+v", st)
	}
	if st, _ := hp.Get("torch"); st.Probes != 0 {
		t.Errorf("пропущенный движок получил пробу: %+v", st)
	}
}

func TestProbeReportNote(t *testing.T) {
	if note := (ProbeReport{Total: 3, Probed: 3}).Note(); note != "" {
		t.Errorf("Note() при состоявшихся проверках = %q, ожидала пустую строку", note)
	}
	note := (ProbeReport{Total: 7, Skipped: 7}).Note()
	if !strings.Contains(note, "пропущено 7 из 7") {
		t.Errorf("Note() = %q, ожидала счётчик пропусков", note)
	}
	if !strings.Contains(note, "нужен tor или прокси") {
		t.Errorf("Note() = %q, ожидала причину", note)
	}
	if !strings.Contains(note, "состояние движков не менялось") {
		t.Errorf("Note() = %q, ожидала заверение о сохранности состояния", note)
	}
}
