package main

import (
	"strconv"
	"strings"
	"testing"

	"voidsearchswag/internal/config"
)

// Живой замер до правки на HEAD ea8bf4b, пересобранный бинарник, копия боевой
// базы, тор выключен, транспорт direct:
//
//	poolcheck --size -1 --limit 5 --skip-verify --show 3
//	    rc=0, три строки stdout, stderr пуст
//	poolcheck --size 40 --limit -5 --skip-verify --show 3
//	    rc=0, три строки stdout, stderr пуст
//	poolcheck --size 40 --limit 5 --skip-verify --show -1
//	    rc=0, ноль строк stdout, stderr пуст
//	poolcheck --size 40 --limit 5 --skip-verify --show 0
//	    rc=0, ноль строк stdout, stderr пуст
//
// Программный зонд на countingProvider показал, куда девались числа:
//
//	size=-1     limit=-5      -> провайдеру limit=400, применено MaxLive=60
//	size=0      limit=0       -> провайдеру limit=400, применено MaxLive=60
//	size=40     limit=400     -> провайдеру limit=400, применено MaxLive=40
//	size=100000 limit=1000000 -> провайдеру limit=1000000, MaxLive=100000
//
// То есть минус один у размера давал не сорок из дефолта флага, а шестьдесят из
// дефолта пула, а минус пять у лимита отправляли провайдеру запрос на четыреста
// адресов вместо пяти.
// Ноль в списке значений не участвует: с этапа 146 он означает «взять размер пула
// из конфига» и проверен отдельным тестом в poolcheckconfig_test.go.
func TestPoolcheckRejectsNegativeSize(t *testing.T) {
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)

	for _, value := range []int{-1, -40} {
		stdout, stderr, code := runMainSplit(t, "poolcheck", "--size", strconv.Itoa(value), "--skip-verify")
		want := "--size должен быть положительным, получено " + strconv.Itoa(value)
		if !strings.Contains(stderr, want) {
			t.Errorf("poolcheck --size %d: stderr не называет причину, хочу %q, фактически %q", value, want, stderr)
		}
		if code != 1 {
			t.Errorf("poolcheck --size %d: код возврата %d, хочу 1", value, code)
		}
		if strings.Contains(stdout, "exit=") {
			t.Errorf("poolcheck --size %d: пул нагрет несмотря на отказ: %s", value, firstN(stdout, 200))
		}
	}
}

func TestPoolcheckRejectsSizeAboveCeiling(t *testing.T) {
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)

	for _, value := range []int{10001, 100000} {
		stdout, stderr, code := runMainSplit(t, "poolcheck", "--size", strconv.Itoa(value), "--skip-verify")
		want := "--size слишком большой: " + strconv.Itoa(value) + " (предел " + strconv.Itoa(maxFlagPoolSize) + ")"
		if !strings.Contains(stderr, want) {
			t.Errorf("poolcheck --size %d: stderr не называет причину, хочу %q, фактически %q", value, want, stderr)
		}
		if code != 1 {
			t.Errorf("poolcheck --size %d: код возврата %d, хочу 1", value, code)
		}
		if strings.Contains(stdout, "exit=") {
			t.Errorf("poolcheck --size %d: пул нагрет несмотря на отказ: %s", value, firstN(stdout, 200))
		}
	}
}

func TestPoolcheckRejectsNegativeLimit(t *testing.T) {
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)

	for _, value := range []int{-5, -1} {
		stdout, stderr, code := runMainSplit(t, "poolcheck", "--limit", strconv.Itoa(value), "--skip-verify")
		want := "--limit должен быть положительным, получено " + strconv.Itoa(value)
		if !strings.Contains(stderr, want) {
			t.Errorf("poolcheck --limit %d: stderr не называет причину, хочу %q, фактически %q", value, want, stderr)
		}
		if code != 1 {
			t.Errorf("poolcheck --limit %d: код возврата %d, хочу 1", value, code)
		}
		if strings.Contains(stdout, "exit=") {
			t.Errorf("poolcheck --limit %d: пул нагрет несмотря на отказ: %s", value, firstN(stdout, 200))
		}
	}
}

func TestPoolcheckRejectsLimitAboveCeiling(t *testing.T) {
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)

	stdout, stderr, code := runMainSplit(t, "poolcheck", "--limit", "100001", "--skip-verify")
	want := "--limit слишком большой: 100001 (предел " + strconv.Itoa(maxFlagFetchLimit) + ")"
	if !strings.Contains(stderr, want) {
		t.Errorf("poolcheck --limit 100001: stderr не называет причину, хочу %q, фактически %q", want, stderr)
	}
	if code != 1 {
		t.Errorf("poolcheck --limit 100001: код возврата %d, хочу 1", code)
	}
	if strings.Contains(stdout, "exit=") {
		t.Errorf("poolcheck --limit 100001: пул нагрет несмотря на отказ: %s", firstN(stdout, 200))
	}
}

// Ноль у --show законен и означает «не показывать адреса», поэтому правило здесь
// диапазон, а не положительность. Отрицательное значение до правки давало тот же
// пустой вывод молча, а огромное печатало дубли: pool.Next ходит по кругу и не
// заканчивается вместе с пулом.
func TestPoolcheckRejectsShowOutsideRange(t *testing.T) {
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)

	for _, value := range []int{-1, -10, maxFlagLimit + 1} {
		stdout, stderr, code := runMainSplit(t, "poolcheck", "--show", strconv.Itoa(value), "--skip-verify")
		want := "--show должен быть в диапазоне от 0 до " + strconv.Itoa(maxFlagLimit) + ", получено " + strconv.Itoa(value)
		if !strings.Contains(stderr, want) {
			t.Errorf("poolcheck --show %d: stderr не называет причину, хочу %q, фактически %q", value, want, stderr)
		}
		if code != 1 {
			t.Errorf("poolcheck --show %d: код возврата %d, хочу 1", value, code)
		}
		if strings.Contains(stdout, "exit=") {
			t.Errorf("poolcheck --show %d: пул нагрет несмотря на отказ: %s", value, firstN(stdout, 200))
		}
	}
}

// Потолки команды обязаны совпадать с границами конфига: иначе флаг либо запрещал
// бы рабочий размер пула, либо пропускал значение, которое конфиг всё равно
// обрезал бы для переменной окружения.
func TestPoolcheckCeilingsMatchConfigRules(t *testing.T) {
	if maxFlagPoolSize != 10000 {
		t.Errorf("maxFlagPoolSize = %d, хочу 10000: таков верхний предел ProxyPoolSize в конфиге", maxFlagPoolSize)
	}
	if maxFlagFetchLimit != 100000 {
		t.Errorf("maxFlagFetchLimit = %d, хочу 100000: таков верхний предел ProxyFetchLimit в конфиге", maxFlagFetchLimit)
	}

	t.Setenv("VOIDSEARCH_DATA_DIR", t.TempDir())
	t.Setenv("VOIDSEARCH_PROXY_POOL_SIZE", "1000000")
	t.Setenv("VOIDSEARCH_PROXY_FETCH_LIMIT", "10000000")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("конфиг не собрался: %v", err)
	}
	if cfg.ProxyPoolSize != maxFlagPoolSize {
		t.Errorf("конфиг ограничивает размер пула до %d, а команда держит %d",
			cfg.ProxyPoolSize, maxFlagPoolSize)
	}
	if cfg.ProxyFetchLimit != maxFlagFetchLimit {
		t.Errorf("конфиг ограничивает выборку провайдера до %d, а команда держит %d",
			cfg.ProxyFetchLimit, maxFlagFetchLimit)
	}
}

// Справка обязана называть потолки и смысл нуля у --show: до правки она говорила
// «целевой размер пула», «сколько адресов брать у провайдера» и «сколько живых
// показать», не называя ни одной границы. Строки сравниваются целиком: поиск
// короткого «не больше 10000» находил его внутри «не больше 100000» у соседнего
// флага, и справка без потолка размера проходила проверку.
func TestPoolcheckHelpNamesCeilings(t *testing.T) {
	_, stderr, _ := runMainSplit(t, "poolcheck", "-h")
	for _, want := range []string{
		"целевой размер пула, не больше 10000",
		"сколько адресов брать у провайдера, не больше 100000",
		"сколько живых показать (0 - не показывать)",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("справка poolcheck не содержит %q, фактически:\n%s", want, firstN(stderr, 900))
		}
	}
}
