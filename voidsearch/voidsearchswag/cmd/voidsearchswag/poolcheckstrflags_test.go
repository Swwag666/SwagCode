package main

import (
	"flag"
	"io"
	"strings"
	"testing"
)

// Живой замер до правки на HEAD 7342776, пересобранный бинарник, пустой каталог
// данных, тор выключен, транспорт direct:
//
//	VOIDSEARCH_PROXYSCRAPE_PROTOCOL=socks5
//	VOIDSEARCH_PROXYSCRAPE_COUNTRY=ru
//	VOIDSEARCH_PROXY_SKIP_VERIFY=true
//	VOIDSEARCH_PROXY_POOL_SIZE=7 VOIDSEARCH_PROXY_FETCH_LIMIT=9
//	    poolcheck --show 0
//	    rc=0, «пределы: размер пула 7, выборка 9, протокол http,
//	                   страна all, проверка живости да»
//	poolcheck -h
//	    -country string
//	        ISO-код страны или all (default "all")
//	    -protocol string
//	        http|https|socks4|socks5 (default "http")
//	    -skip-verify
//	        не проверять живость
//
// Числа конфиг читал с прошлого этапа, поэтому семь и девять в той строке были
// настоящими, а протокол, страна и проверка живости - чужими: запрос уходил на
// публичный API за http-адресами всех стран и грел каждый адрес живой проверкой,
// хотя конфиг просил socks5 по России и проверку не делать. Справка показывала
// дефолты флагов как истину, то есть прочитать правило из вывода было нельзя.
func TestPoolcheckTakesProtocolAndCountryFromConfig(t *testing.T) {
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	t.Setenv("VOIDSEARCH_PROXYSCRAPE_PROTOCOL", "socks5")
	t.Setenv("VOIDSEARCH_PROXYSCRAPE_COUNTRY", "ru")
	t.Setenv("VOIDSEARCH_PROXY_POOL_SIZE", "4")
	t.Setenv("VOIDSEARCH_PROXY_FETCH_LIMIT", "6")

	stdout, stderr, code := runMainSplit(t, "poolcheck", "--skip-verify", "--show", "0")
	if code != 0 {
		t.Fatalf("код возврата %d, stderr %q", code, stderr)
	}
	want := "пределы: размер пула 4, выборка 6, протокол socks5, страна ru"
	if !strings.Contains(stdout, want) {
		t.Errorf("строка пределов не назвала протокол и страну из конфига, хочу %q, stdout: %s",
			want, firstN(stdout, 400))
	}
}

func TestPoolcheckFlagsOverrideProtocolAndCountry(t *testing.T) {
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	t.Setenv("VOIDSEARCH_PROXYSCRAPE_PROTOCOL", "socks5")
	t.Setenv("VOIDSEARCH_PROXYSCRAPE_COUNTRY", "ru")

	stdout, stderr, code := runMainSplit(t, "poolcheck", "--skip-verify", "--show", "0",
		"--protocol", "https", "--country", "de")
	if code != 0 {
		t.Fatalf("код возврата %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "протокол https, страна de") {
		t.Errorf("флаги не перебили конфиг, stdout: %s", firstN(stdout, 400))
	}
}

// Конфиг просит проверку не делать, флаг не назван вовсе: пул обязан греться без
// живой проверки. До правки здесь всегда стояло «проверка живости да», и прогон
// без --skip-verify тратил минуты на пробы адресов, которые оператор выключил.
func TestPoolcheckSkipVerifyTakesConfigValue(t *testing.T) {
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	t.Setenv("VOIDSEARCH_PROXY_SKIP_VERIFY", "true")
	t.Setenv("VOIDSEARCH_PROXY_POOL_SIZE", "4")
	t.Setenv("VOIDSEARCH_PROXY_FETCH_LIMIT", "6")

	stdout, stderr, code := runMainSplit(t, "poolcheck", "--show", "0")
	if code != 0 {
		t.Fatalf("код возврата %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "проверка живости нет") {
		t.Errorf("конфиг не выключил проверку живости, stdout: %s", firstN(stdout, 400))
	}
}

// Обратная сторона того же правила: false у булева флага - рабочее значение, а не
// «флаг не задан». Без разбора по списку флагов конфиг со включённым skip-verify
// нельзя было бы перебить обратно, и --skip-verify=false молча означал бы «как в
// конфиге». Прогон греет пул по-настоящему и упирается в сеть, поэтому код
// возврата здесь не предмет проверки: строка пределов печатается до пула.
func TestPoolcheckSkipVerifyFlagOverridesConfigTrue(t *testing.T) {
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	t.Setenv("VOIDSEARCH_PROXY_SKIP_VERIFY", "true")
	t.Setenv("VOIDSEARCH_REQUEST_TIMEOUT", "2s")
	t.Setenv("VOIDSEARCH_PROXY_PROBE_TIMEOUT", "1s")

	stdout, _, _ := runMainSplit(t, "poolcheck", "--show", "0", "--size", "3", "--limit", "5",
		"--skip-verify=false")
	if !strings.Contains(stdout, "проверка живости да") {
		t.Errorf("--skip-verify=false не перебил конфиг, stdout: %s", firstN(stdout, 400))
	}
}

func TestPoolcheckSkipVerifyFlagOverridesConfigFalse(t *testing.T) {
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	t.Setenv("VOIDSEARCH_PROXY_SKIP_VERIFY", "false")

	stdout, stderr, code := runMainSplit(t, "poolcheck", "--show", "0", "--skip-verify")
	if code != 0 {
		t.Fatalf("код возврата %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "проверка живости нет") {
		t.Errorf("--skip-verify не перебил конфиг, stdout: %s", firstN(stdout, 400))
	}
}

// Пустая строка в конфиге законна: netx.ProxyScrape подставляет «http» и «all»
// сам через orStr. Строка пределов обязана называть то же значение, которое
// действительно уйдёт в запрос, поэтому третий слой - дефолт провайдера, а не
// пустое место.
func TestPoolcheckEmptyConfigProtocolFallsBackToProviderDefault(t *testing.T) {
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	t.Setenv("VOIDSEARCH_PROXYSCRAPE_PROTOCOL", "")
	t.Setenv("VOIDSEARCH_PROXYSCRAPE_COUNTRY", "")

	stdout, stderr, code := runMainSplit(t, "poolcheck", "--skip-verify", "--show", "0")
	if code != 0 {
		t.Fatalf("код возврата %d, stderr %q", code, stderr)
	}
	want := "протокол http, страна all"
	if !strings.Contains(stdout, want) {
		t.Errorf("пустой конфиг не дал дефолт провайдера, хочу %q, stdout: %s", want, firstN(stdout, 400))
	}
}

// Пробелы вокруг значения флага попали бы в query-параметр запроса как есть, и
// провайдер ответил бы пустым списком вместо адресов нужной страны.
func TestPoolcheckTrimsWhitespaceInStringFlags(t *testing.T) {
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)

	stdout, stderr, code := runMainSplit(t, "poolcheck", "--skip-verify", "--show", "0",
		"--protocol", "  socks5 ", "--country", " ru ")
	if code != 0 {
		t.Fatalf("код возврата %d, stderr %q", code, stderr)
	}
	want := "протокол socks5, страна ru"
	if !strings.Contains(stdout, want) {
		t.Errorf("значение флага не обрезано, хочу %q, stdout: %s", want, firstN(stdout, 400))
	}
}

func TestStrFlagOrConfigLayers(t *testing.T) {
	cases := []struct {
		name                          string
		flagValue, cfgValue, defValue string
		want                          string
	}{
		{"флаг важнее конфига", "socks5", "http", "http", "socks5"},
		{"пустой флаг отдаёт конфиг", "", "socks5", "http", "socks5"},
		{"пробелы вместо значения флага", "   ", "socks5", "http", "socks5"},
		{"пустой конфиг отдаёт дефолт", "", "  ", "http", "http"},
		{"пусто везде", "", "", "", ""},
		{"значение флага обрезается", " socks5 ", "http", "http", "socks5"},
		{"значение конфига обрезается", "", " ru ", "all", "ru"},
	}
	for _, c := range cases {
		if got := strFlagOrConfig(c.flagValue, c.cfgValue, c.defValue); got != c.want {
			t.Errorf("%s: strFlagOrConfig(%q, %q, %q) = %q, хочу %q",
				c.name, c.flagValue, c.cfgValue, c.defValue, got, c.want)
		}
	}
}

// Проверка идёт на настоящем flag.FlagSet: правило «флаг назван - значит его
// значение и применяем» держится на fs.Visit, а не на сравнении со значением по
// умолчанию, и подменить его сравнением с false было бы легко.
func TestBoolFlagOrConfigReadsPassedFlagsOnly(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		cfgValue bool
		want     bool
	}{
		{"флаг не назван, конфиг false", nil, false, false},
		{"флаг не назван, конфиг true", nil, true, true},
		{"явный false перебивает true из конфига", []string{"--skip-verify=false"}, true, false},
		{"флаг перебивает false из конфига", []string{"--skip-verify"}, false, true},
		{"явный true перебивает false из конфига", []string{"--skip-verify=true"}, false, true},
	}
	for _, c := range cases {
		fs := flag.NewFlagSet("probe", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		value := fs.Bool("skip-verify", false, "")
		if err := fs.Parse(c.args); err != nil {
			t.Fatalf("%s: разбор %v: %v", c.name, c.args, err)
		}
		if got := boolFlagOrConfig(fs, "skip-verify", *value, c.cfgValue); got != c.want {
			t.Errorf("%s: boolFlagOrConfig = %v, хочу %v", c.name, got, c.want)
		}
		if got := flagPassed(fs, "skip-verify"); got != (len(c.args) > 0) {
			t.Errorf("%s: flagPassed = %v, хочу %v", c.name, got, len(c.args) > 0)
		}
	}
}
