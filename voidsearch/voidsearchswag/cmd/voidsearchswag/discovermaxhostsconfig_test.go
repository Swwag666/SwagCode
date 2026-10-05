package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// Живой замер до правки на HEAD 8345ec3, пересобранный бинарник, копия боевой
// базы, тор выключен, транспорт direct:
//
//	VOIDSEARCH_DISCOVER_MAX_HOSTS=7   discover --crawl --depth 1 --json --seeds ...
//	    rc=0, crawl.max_hosts=50, crawl.depth=1
//	VOIDSEARCH_DISCOVER_MAX_HOSTS=500 discover --crawl --depth 1 --json --seeds ...
//	    rc=0, crawl.max_hosts=50, crawl.depth=1
//	VOIDSEARCH_DISCOVER_MAX_HOSTS=7   discover --crawl --depth 1 --json --max-hosts 0 --seeds ...
//	    rc=0, crawl.max_hosts=7
//
// Дефолт флага пятьдесят совпадал с envDefault DiscoverMaxHosts, поэтому команда
// не могла отличить «флаг не задан» от «флаг задан пятьюдесятью», и переменная
// окружения не действовала вовсе. Справка при этом обещала «0 - значение из
// конфига», то есть описывала правило, которое работало только для явного нуля.
func crawlMaxHosts(t *testing.T, args ...string) int {
	t.Helper()
	stdout, stderr, code := runMainSplit(t, args...)
	if code != 0 {
		t.Fatalf("%s: код возврата %d, stderr: %s", strings.Join(args, " "), code, stderr)
	}
	var rep struct {
		Crawl struct {
			MaxHosts int `json:"max_hosts"`
		} `json:"crawl"`
	}
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("%s: отчёт discover не JSON: %v, вывод: %s",
			strings.Join(args, " "), err, firstN(stdout, 300))
	}
	return rep.Crawl.MaxHosts
}

func discoverCrawlArgs(extra ...string) []string {
	args := []string{"discover", "--crawl", "-depth", "1", "-timeout", "6s",
		"--seeds", "http://unknaaaaaaaaaaaa.onion/", "--json"}
	return append(args, extra...)
}

func TestDiscoverMaxHostsTakesConfigValue(t *testing.T) {
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	t.Setenv("VOIDSEARCH_REQUEST_TIMEOUT", "2s")
	t.Setenv("VOIDSEARCH_DISCOVER_MAX_HOSTS", "7")

	if got := crawlMaxHosts(t, discoverCrawlArgs()...); got != 7 {
		t.Errorf("crawl.max_hosts=%d, хочу 7 из VOIDSEARCH_DISCOVER_MAX_HOSTS", got)
	}
}

func TestDiscoverMaxHostsZeroTakesConfigValue(t *testing.T) {
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	t.Setenv("VOIDSEARCH_REQUEST_TIMEOUT", "2s")
	t.Setenv("VOIDSEARCH_DISCOVER_MAX_HOSTS", "9")

	if got := crawlMaxHosts(t, discoverCrawlArgs("-max-hosts", "0")...); got != 9 {
		t.Errorf("crawl.max_hosts=%d, хочу 9 из конфига при явном нуле флага", got)
	}
}

func TestDiscoverMaxHostsFlagOverridesConfigValue(t *testing.T) {
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	t.Setenv("VOIDSEARCH_REQUEST_TIMEOUT", "2s")
	t.Setenv("VOIDSEARCH_DISCOVER_MAX_HOSTS", "500")

	if got := crawlMaxHosts(t, discoverCrawlArgs("-max-hosts", "11")...); got != 11 {
		t.Errorf("crawl.max_hosts=%d, хочу 11 из флага, а не 500 из конфига", got)
	}
}

func TestDiscoverHelpNamesMaxHostsConfigRule(t *testing.T) {
	stdout, stderr, _ := runMainSplit(t, "discover", "-h")
	help := stdout + stderr
	want := "потолок обходимых хостов от 1 до 5000 (0 - значение из конфига)"
	if !strings.Contains(help, want) {
		t.Errorf("справка discover не описывает правило нуля целиком, хочу %q, фактически:\n%s",
			want, firstN(help, 1200))
	}
	if strings.Contains(help, "(default 50)") {
		t.Errorf("справка discover всё ещё печатает дефолт 50 у --max-hosts:\n%s", firstN(help, 1200))
	}
}
