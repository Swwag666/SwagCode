package main

import (
	"strings"
	"testing"
	"time"

	"voidsearchswag/internal/config"
)

func TestCheckDurationRangeAcceptsBoundsAndRejectsOutside(t *testing.T) {
	good := []time.Duration{0, time.Nanosecond, time.Millisecond, 2 * time.Second, maxFlagDelay}
	for _, d := range good {
		if err := checkDurationRange("delay", d, 0, maxFlagDelay); err != nil {
			t.Errorf("checkDurationRange(delay, %s) = %v, хочу nil", d, err)
		}
	}

	bad := map[time.Duration]string{
		-time.Nanosecond:   "--delay должен быть не меньше 0s, получено -1ns",
		-time.Second:       "--delay должен быть не меньше 0s, получено -1s",
		-5 * time.Minute:   "--delay должен быть не меньше 0s, получено -5m0s",
		maxFlagDelay + 1:   "--delay слишком большой: 10m0.000000001s (предел 10m0s)",
		100000 * time.Hour: "--delay слишком большой: 100000h0m0s (предел 10m0s)",
	}
	for d, want := range bad {
		err := checkDurationRange("delay", d, 0, maxFlagDelay)
		if err == nil {
			t.Errorf("checkDurationRange(delay, %s) = nil, хочу ошибку %q", d, want)
			continue
		}
		if err.Error() != want {
			t.Errorf("checkDurationRange(delay, %s) = %q, хочу %q", d, err.Error(), want)
		}
	}
}

// Потолок флага обязан совпадать с потолком той же величины в конфиге: обе
// границы отвечают на один вопрос «какая пауза между запросами к хосту ещё
// осмысленна». До правки флаг допускал сутки, а конфиг отвергал всё, что больше
// десяти минут, и одно и то же значение проходило одним путём и не проходило
// другим. Проверка связи живая: config.Load принимает ровно потолок флага и
// отвергает значение на секунду больше.
func TestMaxFlagDelayMatchesConfigCeiling(t *testing.T) {
	if maxFlagDelay != 10*time.Minute {
		t.Errorf("maxFlagDelay = %s, хочу 10m: таков предел VOIDSEARCH_DISCOVER_DELAY в конфиге", maxFlagDelay)
	}
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	t.Setenv("VOIDSEARCH_BACKUP_DIR", "")

	t.Setenv("VOIDSEARCH_DISCOVER_DELAY", maxFlagDelay.String())
	if _, err := config.Load(); err != nil {
		t.Errorf("конфиг отверг %s, хотя флаг его допускает: %v", maxFlagDelay, err)
	}

	over := maxFlagDelay + time.Second
	t.Setenv("VOIDSEARCH_DISCOVER_DELAY", over.String())
	if _, err := config.Load(); err == nil {
		t.Errorf("конфиг принял %s, хотя предел %s", over, maxFlagDelay)
	}
}

// Живой замер до правки на HEAD 71d085a, пересобранный бинарник, копия боевой
// базы, тор выключен, транспорт direct, collect --limit 2 --json:
//
//	--delay -1s  rc=0, hosts=2, failed=2, stderr пуст
//	--delay -5m  rc=0, hosts=2, failed=2, stderr пуст
//	--delay 0s   rc=0, hosts=2, failed=2, stderr пуст
//
// Программный зонд показал подмену: NewRateLimiter(-1s).Delay() даёт 2s,
// catalog.Config.PerHostDelay=-1s через withDefaults даёт 2s.
func TestCollectRejectsNegativeDelayInRealRun(t *testing.T) {
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	seedPoolURLs(t, dir, []string{"http://" + base32Tail(1, 56) + ".onion/", "http://" + base32Tail(2, 56) + ".onion/"})

	for _, value := range []string{"-1s", "-5m", "-1ns"} {
		stdout, stderr, code := runMainSplit(t, "collect", "--limit", "2", "--delay", value, "--json")
		if !strings.Contains(stderr, "--delay должен быть не меньше 0s, получено "+value) {
			t.Errorf("collect --delay %s: stderr не называет причину, хочу текст про нижнюю границу, фактически %q", value, stderr)
		}
		if code != 1 {
			t.Errorf("collect --delay %s: код возврата %d, хочу 1", value, code)
		}
		if strings.Contains(stdout, "hosts") {
			t.Errorf("collect --delay %s: сбор состоялся несмотря на отказ: %s", value, firstN(stdout, 200))
		}
	}
}

func TestCollectRejectsHugeDelayInRealRun(t *testing.T) {
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	seedPoolURLs(t, dir, []string{"http://" + base32Tail(1, 56) + ".onion/"})

	for _, value := range []string{"10m1s", "100000h"} {
		stdout, stderr, code := runMainSplit(t, "collect", "--limit", "1", "--delay", value, "--json")
		if !strings.Contains(stderr, "(предел 10m0s)") {
			t.Errorf("collect --delay %s: stderr не называет предел, фактически %q", value, stderr)
		}
		if code != 1 {
			t.Errorf("collect --delay %s: код возврата %d, хочу 1", value, code)
		}
		if strings.Contains(stdout, "hosts") {
			t.Errorf("collect --delay %s: сбор состоялся несмотря на отказ: %s", value, firstN(stdout, 200))
		}
	}
}

// Ноль и рабочая пауза обязаны оставаться рабочими: ноль означает «значение из
// конфига» и у discover, и у collect, и у probe.
func TestCollectAcceptsZeroAndWorkingDelay(t *testing.T) {
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	seedPoolURLs(t, dir, []string{"http://" + base32Tail(1, 56) + ".onion/"})

	for _, value := range []string{"0s", "1ms", "10m"} {
		_, _, code := runMainSplit(t, "collect", "--limit", "1", "--delay", value, "--json")
		if code != 0 {
			t.Errorf("collect --delay %s: код возврата %d, хочу 0", value, code)
		}
	}
}

// Живой замер до правки на HEAD 71d085a: discover --crawl --delay -1s -timeout 8s
// --seeds http://unknaaaaaaaaaaaa.onion/ --json дал rc=0, found=11116, crawled=1 и
// пустой stderr, а --delay 100000h дал то же. Пауза уходила в обход непроверенной.
func TestDiscoverRejectsDelayOutsideRangeInRealRun(t *testing.T) {
	// Офлайн-окружение обязательно: без него сломанная проверка уводит прогон в
	// базу из конфига, то есть в боевую. Так уже случилось на этапе 133.
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)

	for _, value := range []string{"-1s", "-5m", "10m1s", "100000h"} {
		stdout, stderr, code := runMainSplit(t, "discover", "--crawl", "--delay", value, "-timeout", "1s")
		if !strings.Contains(stderr, "--delay") {
			t.Errorf("discover --delay %s: stderr не называет флаг, фактически %q", value, stderr)
		}
		if code != 1 {
			t.Errorf("discover --delay %s: код возврата %d, хочу 1", value, code)
		}
		if strings.Contains(stdout, "crawl") {
			t.Errorf("discover --delay %s: обход состоялся несмотря на отказ: %s", value, firstN(stdout, 200))
		}
	}
}

// Справка обязана называть потолок и смысл нуля у обоих флагов: до правки она
// говорила просто «пауза между запросами к хосту».
func TestDelayHelpNamesCeiling(t *testing.T) {
	_, discoverHelp, _ := runMainSplit(t, "discover", "-h")
	if !strings.Contains(discoverHelp, "не больше 10m") {
		t.Errorf("справка discover не называет потолок паузы, фактически:\n%s", firstN(discoverHelp, 900))
	}
	if !strings.Contains(discoverHelp, "0 - значение из конфига") {
		t.Errorf("справка discover не объясняет ноль у паузы, фактически:\n%s", firstN(discoverHelp, 900))
	}
	_, collectHelp, _ := runMainSplit(t, "collect", "-h")
	if !strings.Contains(collectHelp, "не больше 10m") {
		t.Errorf("справка collect не называет потолок паузы, фактически:\n%s", firstN(collectHelp, 900))
	}
}
