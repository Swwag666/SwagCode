package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// Живой замер до правки на HEAD ac85bf8, пересобранный бинарник, копия боевой
// базы, тор выключен, транспорт direct. Пауза пятнадцать часов проходила через
// флаг всех трёх команд и уходила в волну:
//
//	probe --limit 3 --delay 15h --json        rc=1, delay_ms=54000000
//	collect --limit 1 --delay 15h --json      rc=0, per_host_delay_ms=54000000
//	discover --crawl --depth 1 --delay 15h --json  rc=0, crawl.per_host_delay_ms=54000000
//
// а то же значение в конфиге отвергалось:
//
//	VOIDSEARCH_DISCOVER_DELAY=15h probe --limit 3 --json
//	    rc=1, ERROR: конфиг: config: VOIDSEARCH_DISCOVER_DELAY слишком большой:
//	    15h0m0s (предел 10m0s)
//
// После правки потолок флага равен потолку конфига, и все три команды называют
// его в отказе.
func TestAllCommandsRejectDelayAboveConfigCeiling(t *testing.T) {
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	// Одна и та же очередь нужна обеим командам: probe берёт из неё
	// непроверенные адреса, collect остаётся с пустой выборкой и всё равно
	// обязан отказать по флагу раньше, чем полезет за хостами. Отдельный посев
	// под collect конфликтует с проверкой seedPoolURLs на размер очереди.
	seedUnknown(t, dir, 5)

	want := "--delay слишком большой: 15h0m0s (предел 10m0s)"
	runs := []struct {
		name string
		args []string
	}{
		{"probe", []string{"probe", "--limit", "3", "--delay", "15h", "--json"}},
		{"collect", []string{"collect", "--limit", "1", "--delay", "15h", "--json"}},
		{"discover", []string{"discover", "--crawl", "--delay", "15h", "-timeout", "1s"}},
	}
	for _, r := range runs {
		stdout, stderr, code := runMainSplit(t, r.args...)
		if code != 1 {
			t.Errorf("%s --delay 15h: код возврата %d, хочу 1", r.name, code)
		}
		if !strings.Contains(stderr, want) {
			t.Errorf("%s --delay 15h: stderr не называет причину, хочу %q, фактически %q",
				r.name, want, stderr)
		}
		if strings.Contains(stdout, "delay_ms") || strings.Contains(stdout, "per_host_delay_ms") {
			t.Errorf("%s --delay 15h: волна состоялась несмотря на отказ: %s",
				r.name, firstN(stdout, 200))
		}
	}
}

// Граница обязана быть включённой: ровно десять минут - законная пауза, и команда
// с ней работает, а не отказывает.
func TestAllCommandsAcceptDelayAtConfigCeiling(t *testing.T) {
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	seedUnknown(t, dir, 5)

	stdout, stderr, code := runMainSplit(t, "probe", "--limit", "3", "--delay", "10m", "--json")
	if code == 1 && strings.Contains(stderr, "--delay") {
		t.Errorf("probe --delay 10m отклонён: %q", stderr)
	}
	var rep struct {
		DelayMS int64 `json:"delay_ms"`
	}
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("отчёт probe не JSON: %v, вывод: %s", err, firstN(stdout, 300))
	}
	if rep.DelayMS != 600000 {
		t.Errorf("probe --delay 10m не донёс паузу до отчёта: delay_ms=%d, хочу 600000, вывод:\n%s",
			rep.DelayMS, firstN(stdout, 300))
	}

	if _, stderr, code := runMainSplit(t, "collect", "--limit", "1", "--delay", "10m", "--json"); code != 0 {
		t.Errorf("collect --delay 10m: код возврата %d, stderr %q", code, stderr)
	}
}
