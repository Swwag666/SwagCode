package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"voidsearchswag/internal/config"
)

// Живой замер до правки на HEAD 17502a8, пересобранный бинарник, копия боевой
// базы, тор выключен, транспорт direct:
//
//	probe -h
//	    флаги команды probe:
//	      -addr string
//	      -json
//	      -limit int
//	      -timeout duration
//	probe --limit 3 --delay 1s --json
//	    rc=2, stderr: ERROR: неизвестный флаг -delay команды probe
//
// Флаг --delay есть у collect и discover, а probe брал паузу только из конфига,
// поэтому замедлить одну волну без правки файла конфига было нельзя.
func TestProbeDelayFlagRule(t *testing.T) {
	cases := []struct {
		flag time.Duration
		cfg  time.Duration
		want time.Duration
	}{
		{0, 2 * time.Second, 2 * time.Second},
		{time.Millisecond, 2 * time.Second, time.Millisecond},
		{5 * time.Second, 2 * time.Second, 5 * time.Second},
		{-time.Second, 2 * time.Second, 2 * time.Second},
	}
	for _, c := range cases {
		if got := probeDelay(c.flag, c.cfg); got != c.want {
			t.Errorf("probeDelay(%v, %v) = %v, хочу %v", c.flag, c.cfg, got, c.want)
		}
	}
}

func TestBuildProberTakesDelayFromFlag(t *testing.T) {
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	st := openTestStore(t, dir)
	defer closeStore(st)

	eng, cleanup := buildEngine(cfg, stderrLogger{}, st)
	defer cleanup()

	p := buildProber(cfg, stderrLogger{}, st, eng, 5*time.Second)
	if p == nil {
		t.Fatal("пробер не собрался")
	}
	if p.Cfg.Delay != 5*time.Second {
		t.Errorf("пауза %v, хочу 5s из флага", p.Cfg.Delay)
	}
	if p.Limiter == nil {
		t.Fatal("ограничитель паузы не собран: пробы пойдут без задержки")
	}
	if p.Limiter.Delay() != 5*time.Second {
		t.Errorf("ограничитель держит %v, хочу 5s: конфиг пробера и ограничитель разошлись",
			p.Limiter.Delay())
	}
}

func TestProbeRejectsNegativeDelay(t *testing.T) {
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	seedUnknown(t, dir, 5)

	stdout, stderr, code := runMainSplit(t, "probe", "--limit", "3", "--delay", "-1s")
	if code != 1 {
		t.Errorf("код возврата %d, хочу 1", code)
	}
	want := "--delay должен быть не меньше 0s, получено -1s"
	if !strings.Contains(stderr, want) {
		t.Errorf("stderr не называет причину, хочу %q, фактически %q", want, stderr)
	}
	if strings.Contains(stdout, "total") {
		t.Errorf("волна состоялась несмотря на отказ: %s", firstN(stdout, 200))
	}
}

func TestProbeRejectsDelayAboveCeiling(t *testing.T) {
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	seedUnknown(t, dir, 5)

	stdout, stderr, code := runMainSplit(t, "probe", "--limit", "3", "--delay", "25h")
	if code != 1 {
		t.Errorf("код возврата %d, хочу 1", code)
	}
	want := "--delay слишком большой: 25h0m0s (предел 10m0s)"
	if !strings.Contains(stderr, want) {
		t.Errorf("stderr не называет причину, хочу %q, фактически %q", want, stderr)
	}
	if strings.Contains(stdout, "total") {
		t.Errorf("волна состоялась несмотря на отказ: %s", firstN(stdout, 200))
	}
}

// Флаг обязан доезжать до волны, а не только до проверки: отчёт проб после этапа
// 140 несёт delay_ms, и по нему видно, что именно применил пробер.
func TestProbeDelayFlagReachesWaveReport(t *testing.T) {
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	seedUnknown(t, dir, 5)

	stdout, _, _ := runMainSplit(t, "probe", "--limit", "3", "--delay", "1s", "--json")
	var rep struct {
		DelayMS int64 `json:"delay_ms"`
	}
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("отчёт probe не JSON: %v, вывод: %s", err, firstN(stdout, 300))
	}
	if rep.DelayMS != 1000 {
		t.Errorf("delay_ms=%d, хочу 1000 из флага", rep.DelayMS)
	}
}

// Ноль у флага обязан оставить значение конфига, иначе флаг сломал бы штатный
// режим: по умолчанию пауза берётся из DiscoverPerHostDelay.
func TestProbeDelayZeroKeepsConfigValue(t *testing.T) {
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	seedUnknown(t, dir, 5)
	t.Setenv("VOIDSEARCH_DISCOVER_DELAY", "3s")

	stdout, _, _ := runMainSplit(t, "probe", "--limit", "3", "--json")
	var rep struct {
		DelayMS int64 `json:"delay_ms"`
	}
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("отчёт probe не JSON: %v, вывод: %s", err, firstN(stdout, 300))
	}
	if rep.DelayMS != 3000 {
		t.Errorf("delay_ms=%d, хочу 3000 из конфига", rep.DelayMS)
	}
}

func TestProbeHelpNamesDelayFlag(t *testing.T) {
	_, stderr, _ := runMainSplit(t, "probe", "-h")
	for _, want := range []string{
		"-delay duration",
		"пауза между пробами одного хоста, не больше 10m (0 - значение из конфига)",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("справка probe не содержит %q, фактически:\n%s", want, firstN(stderr, 900))
		}
	}
}
