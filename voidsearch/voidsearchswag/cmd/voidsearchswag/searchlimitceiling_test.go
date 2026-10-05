package main

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"voidsearchswag/internal/config"
)

// Живой замер до правки на HEAD 719b6db, пересобранный бинарник, копия боевой
// базы, тор выключен, транспорт direct:
//
//	search --limit 5000 --no-tor --json test
//	    rc=0, ключи: query, mode, used_mode, decision, results, count, cached,
//	    report, duration, limit; limit=5000, results=10
//	search --limit 10001 --no-tor --json test
//	    rc=1, ERROR: --limit слишком большой: 10001 (предел 10000)
//	VOIDSEARCH_RESULT_LIMIT=5000 search --no-tor --json test
//	    rc=0, stderr пуст
//
// Флаг допускал пять тысяч и печатал их в отчёте, а конфиг ту же величину молча
// урезал до тысячи.
func TestMaxFlagResultLimitMatchesConfigCeiling(t *testing.T) {
	if maxFlagResultLimit != 1000 {
		t.Errorf("maxFlagResultLimit = %d, хочу 1000: такова верхняя граница ResultLimit в config.Validate",
			maxFlagResultLimit)
	}
	if maxFlagResultLimit >= maxFlagLimit {
		t.Errorf("потолок результата %d не ниже общего потолка %d", maxFlagResultLimit, maxFlagLimit)
	}

	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	t.Setenv("VOIDSEARCH_BACKUP_DIR", "")

	t.Setenv("VOIDSEARCH_RESULT_LIMIT", strconv.Itoa(maxFlagResultLimit))
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("конфиг отверг %d, хотя флаг его допускает: %v", maxFlagResultLimit, err)
	}
	if cfg.ResultLimit != maxFlagResultLimit {
		t.Errorf("конфиг дал ResultLimit=%d при заданном %d", cfg.ResultLimit, maxFlagResultLimit)
	}

	t.Setenv("VOIDSEARCH_RESULT_LIMIT", strconv.Itoa(maxFlagResultLimit*5))
	cfg, err = config.Load()
	if err != nil {
		t.Fatalf("конфиг не загрузился: %v", err)
	}
	if cfg.ResultLimit != maxFlagResultLimit {
		t.Errorf("конфиг урезал %d до %d, а не до потолка флага %d",
			maxFlagResultLimit*5, cfg.ResultLimit, maxFlagResultLimit)
	}
}

func TestSearchRejectsLimitAboveConfigCeiling(t *testing.T) {
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	t.Setenv("VOIDSEARCH_REQUEST_TIMEOUT", "2s")

	for _, value := range []string{"1001", "5000", "10001"} {
		stdout, stderr, code := runMainSplit(t, "search", "--limit", value, "--no-tor", "--json", "test")
		want := "--limit слишком большой: " + value + " (предел 1000)"
		if !strings.Contains(stderr, want) {
			t.Errorf("search --limit %s: stderr не называет причину, хочу %q, фактически %q",
				value, want, stderr)
		}
		if code != 1 {
			t.Errorf("search --limit %s: код возврата %d, хочу 1", value, code)
		}
		if strings.Contains(stdout, "used_mode") {
			t.Errorf("search --limit %s: поиск состоялся несмотря на отказ: %s",
				value, firstN(stdout, 200))
		}
	}
}

func TestSearchAcceptsLimitAtConfigCeiling(t *testing.T) {
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	t.Setenv("VOIDSEARCH_REQUEST_TIMEOUT", "2s")

	stdout, stderr, code := runMainSplit(t, "search", "--limit", "1000", "--no-tor", "--json", "test")
	if code != 0 {
		t.Fatalf("search --limit 1000: код возврата %d, stderr %q", code, stderr)
	}
	var rep struct {
		Limit int `json:"limit"`
	}
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("отчёт search не JSON: %v, вывод: %s", err, firstN(stdout, 300))
	}
	if rep.Limit != 1000 {
		t.Errorf("limit=%d, хочу 1000: граница включённая", rep.Limit)
	}
}

func TestSearchHelpNamesLimitCeiling(t *testing.T) {
	_, stderr, _ := runMainSplit(t, "search", "-h")
	want := "сколько результатов, не больше 1000 (0 - значение из конфига)"
	if !strings.Contains(stderr, want) {
		t.Errorf("справка search не называет потолок количества и смысл нуля, хочу %q, фактически:\n%s",
			want, firstN(stderr, 900))
	}
}
