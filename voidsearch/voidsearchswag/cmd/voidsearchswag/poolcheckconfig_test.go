package main

import (
	"strings"
	"testing"
)

// Живой замер до правки на HEAD 360ca56, пересобранный бинарник, копия боевой
// базы, тор выключен, транспорт direct, провайдеры недоступны:
//
//	VOIDSEARCH_PROXY_POOL_SIZE=7 VOIDSEARCH_PROXY_FETCH_LIMIT=9
//	    poolcheck --skip-verify --show 0
//	    rc=0, stdout пустой, stderr пустой
//	poolcheck --skip-verify --show 0 --size 0 --limit 0
//	    rc=1, ERROR: --size должен быть положительным, получено 0
//	poolcheck -h
//	    -size int
//	        целевой размер пула, не больше 10000 (default 40)
//	    -limit int
//	        сколько адресов брать у провайдера, не больше 100000 (default 400)
//
// Дефолты флагов сорок и четыреста совпадали с envDefault полей ProxyPoolSize и
// ProxyFetchLimit, команда не читала конфиг вовсе, и удачный прогон не печатал ни
// одного слова: увидеть применённый размер было негде.
func TestPoolcheckZeroSizeTakesConfigValue(t *testing.T) {
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	t.Setenv("VOIDSEARCH_PROXY_POOL_SIZE", "7")
	t.Setenv("VOIDSEARCH_PROXY_FETCH_LIMIT", "9")

	stdout, stderr, code := runMainSplit(t, "poolcheck", "--skip-verify", "--show", "0")
	if code != 0 {
		t.Fatalf("код возврата %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "пределы: размер пула 7, выборка 9") {
		t.Errorf("строка пределов не назвала значения конфига, stdout: %s", firstN(stdout, 400))
	}
}

func TestPoolcheckZeroSizeExplicitTakesConfigValue(t *testing.T) {
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	t.Setenv("VOIDSEARCH_PROXY_POOL_SIZE", "13")
	t.Setenv("VOIDSEARCH_PROXY_FETCH_LIMIT", "17")

	stdout, stderr, code := runMainSplit(t, "poolcheck", "--skip-verify", "--show", "0",
		"--size", "0", "--limit", "0")
	if code != 0 {
		t.Fatalf("явный ноль отклонён: код возврата %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "пределы: размер пула 13, выборка 17") {
		t.Errorf("строка пределов не назвала значения конфига, stdout: %s", firstN(stdout, 400))
	}
}

func TestPoolcheckFlagsOverrideConfigValues(t *testing.T) {
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	t.Setenv("VOIDSEARCH_PROXY_POOL_SIZE", "500")
	t.Setenv("VOIDSEARCH_PROXY_FETCH_LIMIT", "900")

	stdout, stderr, code := runMainSplit(t, "poolcheck", "--skip-verify", "--show", "0",
		"--size", "11", "--limit", "23")
	if code != 0 {
		t.Fatalf("код возврата %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "пределы: размер пула 11, выборка 23") {
		t.Errorf("флаги не перебили конфиг, stdout: %s", firstN(stdout, 400))
	}
}

// Без переменных окружения команда обязана назвать дефолты конфига, а не молчать:
// до правки удачный прогон не печатал ничего, и отличить «пул пустой» от «пул не
// грели» было нельзя.
func TestPoolcheckPrintsAppliedLimitsWithoutEnv(t *testing.T) {
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)

	stdout, stderr, code := runMainSplit(t, "poolcheck", "--skip-verify", "--show", "0")
	if code != 0 {
		t.Fatalf("код возврата %d, stderr %q", code, stderr)
	}
	want := "пределы: размер пула 40, выборка 400, протокол http, страна all, проверка живости нет"
	if !strings.Contains(stdout, want) {
		t.Errorf("строка пределов не совпала, хочу %q, stdout: %s", want, firstN(stdout, 400))
	}
}

func TestPoolcheckNamesLiveCheckWhenNotSkipped(t *testing.T) {
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	t.Setenv("VOIDSEARCH_PROXY_POOL_SIZE", "5")
	t.Setenv("VOIDSEARCH_PROXY_FETCH_LIMIT", "6")
	// Прогон без --skip-verify греет пул по-настоящему и упирается в таймаут
	// запроса, поэтому он укорочен: строка пределов печатается до пула, и ждать
	// сеть здесь незачем.
	t.Setenv("VOIDSEARCH_REQUEST_TIMEOUT", "2s")
	t.Setenv("VOIDSEARCH_PROXY_PROBE_TIMEOUT", "1s")

	stdout, _, _ := runMainSplit(t, "poolcheck", "--show", "0", "--size", "5", "--limit", "6")
	if !strings.Contains(stdout, "проверка живости да") {
		t.Errorf("без --skip-verify строка пределов не назвала проверку живости, stdout: %s", firstN(stdout, 400))
	}
}

func TestPoolcheckHelpNamesConfigRule(t *testing.T) {
	_, stderr, _ := runMainSplit(t, "poolcheck", "-h")
	for _, want := range []string{
		"целевой размер пула, не больше 10000 (0 - значение из конфига)",
		"сколько адресов брать у провайдера, не больше 100000 (0 - значение из конфига)",
		"http|https|socks4|socks5 (пусто - значение из конфига)",
		"ISO-код страны или all (пусто - значение из конфига)",
		"не проверять живость (без флага - значение из конфига)",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("справка poolcheck не описывает правило целиком, хочу %q, фактически:\n%s",
				want, firstN(stderr, 1200))
		}
	}
	// Дефолт флага в справке читается как значение по умолчанию для прогона.
	// Пока флаги не читали конфиг, «(default "http")» и «(default "all")»
	// обещали оператору, что пустой флаг и переменная окружения дают одно и то
	// же, хотя окружение команда не читала вовсе.
	for _, gone := range []string{"(default 40)", "(default 400)", `(default "http")`, `(default "all")`} {
		if strings.Contains(stderr, gone) {
			t.Errorf("справка poolcheck всё ещё печатает дефолт %s:\n%s", gone, firstN(stderr, 1200))
		}
	}
}
