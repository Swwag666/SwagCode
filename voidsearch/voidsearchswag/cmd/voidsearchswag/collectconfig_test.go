package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// Живой замер до правки на HEAD bb08193, пересобранный бинарник, копия боевой
// базы, тор выключен, транспорт direct:
//
//	VOIDSEARCH_DISCOVER_CONCURRENCY=9 VOIDSEARCH_DISCOVER_DELAY=7s
//	    collect --limit 1 --json
//	    rc=0, concurrency=4, per_host_delay_ms=2000, max_files=500, hosts=1, failed=1
//	collect --limit 1 --json (без переменных окружения)
//	    rc=0, concurrency=4, per_host_delay_ms=2000
//	collect --limit 1 --json --concurrency 0 --delay 0s
//	    rc=1, ERROR: --concurrency должен быть положительным, получено 0
//	collect -h
//	    -concurrency int
//	        одновременных обходов (default 4)
//	    -delay duration
//	        пауза между запросами к хосту, не больше 10m (default 2s)
//
// Дефолты флагов четыре и две секунды совпадали с envDefault полей
// DiscoverConcurrency и DiscoverPerHostDelay, поэтому обе переменные окружения не
// действовали, а ноль, которым discover и probe говорят «возьми значение из
// конфига», здесь отвергался.
func collectReport(t *testing.T, args ...string) (concurrency int, delayMS int64) {
	t.Helper()
	stdout, stderr, code := runMainSplit(t, args...)
	if code != 0 {
		t.Fatalf("%s: код возврата %d, stderr: %s", strings.Join(args, " "), code, stderr)
	}
	var rep struct {
		Concurrency    int   `json:"concurrency"`
		PerHostDelayMS int64 `json:"per_host_delay_ms"`
	}
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("%s: отчёт collect не JSON: %v, вывод: %s",
			strings.Join(args, " "), err, firstN(stdout, 400))
	}
	return rep.Concurrency, rep.PerHostDelayMS
}

func collectConfigEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	t.Setenv("VOIDSEARCH_REQUEST_TIMEOUT", "2s")
	seedUnknown(t, dir, 3)
	return dir
}

func TestCollectConcurrencyTakesConfigValue(t *testing.T) {
	collectConfigEnv(t)
	t.Setenv("VOIDSEARCH_DISCOVER_CONCURRENCY", "9")

	got, _ := collectReport(t, "collect", "--limit", "1", "--delay", "1ms", "--json")
	if got != 9 {
		t.Errorf("concurrency=%d, хочу 9 из VOIDSEARCH_DISCOVER_CONCURRENCY", got)
	}
}

func TestCollectDelayTakesConfigValue(t *testing.T) {
	collectConfigEnv(t)
	t.Setenv("VOIDSEARCH_DISCOVER_DELAY", "3s")

	_, got := collectReport(t, "collect", "--limit", "1", "--concurrency", "2", "--json")
	if got != 3000 {
		t.Errorf("per_host_delay_ms=%d, хочу 3000 из VOIDSEARCH_DISCOVER_DELAY", got)
	}
}

func TestCollectZeroFlagsTakeConfigValues(t *testing.T) {
	collectConfigEnv(t)
	t.Setenv("VOIDSEARCH_DISCOVER_CONCURRENCY", "11")
	t.Setenv("VOIDSEARCH_DISCOVER_DELAY", "4s")

	got, delayMS := collectReport(t, "collect", "--limit", "1", "--concurrency", "0", "--delay", "0s", "--json")
	if got != 11 {
		t.Errorf("concurrency=%d, хочу 11 из конфига при явном нуле флага", got)
	}
	if delayMS != 4000 {
		t.Errorf("per_host_delay_ms=%d, хочу 4000 из конфига при явном нуле флага", delayMS)
	}
}

func TestCollectFlagsOverrideConfigValues(t *testing.T) {
	collectConfigEnv(t)
	t.Setenv("VOIDSEARCH_DISCOVER_CONCURRENCY", "9")
	t.Setenv("VOIDSEARCH_DISCOVER_DELAY", "7s")

	got, delayMS := collectReport(t, "collect", "--limit", "1", "--concurrency", "3", "--delay", "1ms", "--json")
	if got != 3 {
		t.Errorf("concurrency=%d, хочу 3 из флага, а не 9 из конфига", got)
	}
	if delayMS != 1 {
		t.Errorf("per_host_delay_ms=%d, хочу 1 из флага, а не 7000 из конфига", delayMS)
	}
}

func TestCollectHelpNamesConfigRule(t *testing.T) {
	_, stderr, _ := runMainSplit(t, "collect", "-h")
	for _, want := range []string{
		"одновременных обходов, не больше 128 (0 - значение из конфига)",
		"пауза между запросами к хосту, не больше 10m (0 - значение из конфига)",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("справка collect не описывает правило нуля целиком, хочу %q, фактически:\n%s",
				want, firstN(stderr, 1200))
		}
	}
	for _, gone := range []string{"(default 4)", "(default 2s)"} {
		if strings.Contains(stderr, gone) {
			t.Errorf("справка collect всё ещё печатает дефолт %s:\n%s", gone, firstN(stderr, 1200))
		}
	}
}
