package main

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"voidsearchswag/internal/config"
)

// Живой замер до правки на HEAD b718028, пересобранный бинарник, копия боевой
// базы, tor выключен, транспорт direct, collect --limit 2 --delay 1ms --json:
//
//	--max-files -1      rc=0, ключи [hosts,pages,links,saved,skipped,failed,elapsed], hosts=2
//	--max-files 0       rc=0, hosts=2
//	--concurrency 0     rc=0, hosts=2
//	--concurrency -5    rc=0, hosts=2
//	--concurrency 100000 rc=0, hosts=2
//
// stderr пуст во всех пяти прогонах, и ни один отчёт не назвал применённое
// значение. Программный зонд catalog.Config.withDefaults показал, куда девались
// числа: MaxFiles -1 и 0 превращались в 500, Concurrency -5 и 0 в 4, а 100000
// проходило без всякого потолка. Оператор, попросивший не сохранять файлы,
// получал потолок в пятьсот, а попросивший один обход за раз - четыре.
func TestCollectRejectsNonPositiveMaxFilesInRealRun(t *testing.T) {
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	seedUnknown(t, dir, 3)

	for _, value := range []string{"0", "-1", "-500"} {
		stdout, stderr, code := runMainSplit(t, "collect", "--limit", "2", "--delay", "1ms", "--max-files", value, "--json")
		want := "--max-files должен быть положительным, получено " + value
		if !strings.Contains(stderr, want) {
			t.Errorf("--max-files %s: stderr не называет причину, хочу %q, фактически %q", value, want, stderr)
		}
		if code != 1 {
			t.Errorf("--max-files %s: код возврата %d, хочу 1", value, code)
		}
		if strings.Contains(stdout, "hosts") {
			t.Errorf("--max-files %s: обход состоялся несмотря на отказ: %s", value, firstN(stdout, 200))
		}
	}
}

func TestCollectRejectsMaxFilesAboveCeilingInRealRun(t *testing.T) {
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	seedUnknown(t, dir, 3)

	for _, value := range []string{"10001", "50000"} {
		stdout, stderr, code := runMainSplit(t, "collect", "--limit", "2", "--delay", "1ms", "--max-files", value, "--json")
		want := "--max-files слишком большой: " + value + " (предел " + strconv.Itoa(maxFlagLimit) + ")"
		if !strings.Contains(stderr, want) {
			t.Errorf("--max-files %s: stderr не называет значение и предел, хочу %q, фактически %q", value, want, stderr)
		}
		if code != 1 {
			t.Errorf("--max-files %s: код возврата %d, хочу 1", value, code)
		}
		if strings.Contains(stdout, "hosts") {
			t.Errorf("--max-files %s: обход состоялся несмотря на отказ: %s", value, firstN(stdout, 200))
		}
	}
}

// Ноль в списке значений не участвует: с этапа 147 он означает «взять
// DiscoverConcurrency из конфига» и проверен в collectconfig_test.go.
func TestCollectRejectsNegativeConcurrencyInRealRun(t *testing.T) {
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	seedUnknown(t, dir, 3)

	for _, value := range []string{"-5", "-1"} {
		stdout, stderr, code := runMainSplit(t, "collect", "--limit", "2", "--delay", "1ms", "--concurrency", value, "--json")
		want := "--concurrency должен быть положительным, получено " + value
		if !strings.Contains(stderr, want) {
			t.Errorf("--concurrency %s: stderr не называет причину, хочу %q, фактически %q", value, want, stderr)
		}
		if code != 1 {
			t.Errorf("--concurrency %s: код возврата %d, хочу 1", value, code)
		}
		if strings.Contains(stdout, "hosts") {
			t.Errorf("--concurrency %s: обход состоялся несмотря на отказ: %s", value, firstN(stdout, 200))
		}
	}
}

func TestCollectRejectsConcurrencyAboveOwnCeilingInRealRun(t *testing.T) {
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	seedUnknown(t, dir, 3)

	for _, value := range []string{"129", "100000"} {
		stdout, stderr, code := runMainSplit(t, "collect", "--limit", "2", "--delay", "1ms", "--concurrency", value, "--json")
		want := "--concurrency слишком большой: " + value + " (предел " + strconv.Itoa(maxCollectConcurrency) + ")"
		if !strings.Contains(stderr, want) {
			t.Errorf("--concurrency %s: stderr не называет значение и предел, хочу %q, фактически %q", value, want, stderr)
		}
		if code != 1 {
			t.Errorf("--concurrency %s: код возврата %d, хочу 1", value, code)
		}
		if strings.Contains(stdout, "hosts") {
			t.Errorf("--concurrency %s: обход состоялся несмотря на отказ: %s", value, firstN(stdout, 200))
		}
	}
}

// Потолок параллельности collect обязан совпадать с тем, что config.Validate
// держит для обхода хостов: обе команды делают одну работу через один транспорт.
func TestCollectConcurrencyCeilingMatchesConfigRule(t *testing.T) {
	if maxCollectConcurrency != 128 {
		t.Errorf("maxCollectConcurrency = %d, хочу 128: таков верхний предел DiscoverConcurrency в конфиге", maxCollectConcurrency)
	}
	t.Setenv("VOIDSEARCH_DISCOVER_CONCURRENCY", "1000")
	t.Setenv("VOIDSEARCH_DATA_DIR", t.TempDir())
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("конфиг не собрался: %v", err)
	}
	if cfg.DiscoverConcurrency != maxCollectConcurrency {
		t.Errorf("конфиг ограничивает обход до %d, а collect держит потолок %d",
			cfg.DiscoverConcurrency, maxCollectConcurrency)
	}
	if err := checkLimitCeiling("concurrency", maxCollectConcurrency, maxCollectConcurrency); err != nil {
		t.Errorf("потолок команды отклонён: %v", err)
	}
	if err := checkLimitCeiling("concurrency", maxCollectConcurrency+1, maxCollectConcurrency); err == nil {
		t.Error("значение на единицу выше потолка принято")
	}
}

// Рабочие значения обоих флагов доходят до обхода без предупреждений и дают
// обычный отчёт.
func TestCollectAcceptsWorkingMaxFilesAndConcurrency(t *testing.T) {
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	seedUnknown(t, dir, 5)

	stdout, stderr, code := runMainSplit(t, "collect", "--limit", "2", "--delay", "1ms",
		"--max-files", "10", "--concurrency", "2", "--json")
	if strings.Contains(stderr, "--max-files") || strings.Contains(stderr, "--concurrency") {
		t.Errorf("рабочие значения вызвали предупреждение %q", stderr)
	}
	if code != 0 {
		t.Errorf("код возврата %d, хочу 0: обход состоялся", code)
	}
	var rep struct {
		Hosts  int `json:"hosts"`
		Failed int `json:"failed"`
	}
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("разбор JSON: %v (%q)", err, firstN(stdout, 300))
	}
	if rep.Hosts != 2 {
		t.Errorf("отчёт несёт hosts=%d, хочу 2", rep.Hosts)
	}
	if rep.Failed != 2 {
		t.Errorf("без tor отказов %d из %d хостов, хочу по одному на адрес", rep.Failed, rep.Hosts)
	}
}
