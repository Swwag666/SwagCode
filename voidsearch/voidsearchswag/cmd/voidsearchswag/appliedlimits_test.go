package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// Живой замер до правки на HEAD 28f4541, пересобранный бинарник, копия боевой
// базы, тор выключен, транспорт direct:
//
//	collect --limit 2 --delay 1ms --max-files 7 --concurrency 2 --json
//	    rc=0, ключи отчёта: elapsed, failed, hosts, links, pages, saved, skipped
//	discover --crawl -depth 2 -max-hosts 3 --delay 1ms --json
//	    rc=0, ключи crawl: depth, elapsed, failed, files, found, no_transport,
//	    ok, pages, pages_detail
//
// Ни один из двух отчётов не называл применённые потолки, поэтому по JSON нельзя
// было понять, уперся прогон в потолок или в реальное отсутствие данных.
func TestCollectJSONNamesAppliedLimits(t *testing.T) {
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	t.Setenv("VOIDSEARCH_REQUEST_TIMEOUT", "2s")
	seedPoolURLs(t, dir, []string{"http://" + base32Tail(1, 56) + ".onion/"})

	stdout, stderr, code := runMainSplit(t, "collect", "--limit", "1",
		"--max-files", "7", "--concurrency", "3", "--delay", "1ms", "--json")
	if code != 0 {
		t.Fatalf("collect завершился с кодом %d, stderr: %s", code, stderr)
	}
	var rep struct {
		MaxFiles       int   `json:"max_files"`
		Concurrency    int   `json:"concurrency"`
		PerHostDelayMS int64 `json:"per_host_delay_ms"`
	}
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("отчёт collect не JSON: %v, вывод: %s", err, firstN(stdout, 300))
	}
	if rep.MaxFiles != 7 {
		t.Errorf("отчёт назвал max_files=%d, хочу 7 из флага", rep.MaxFiles)
	}
	if rep.Concurrency != 3 {
		t.Errorf("отчёт назвал concurrency=%d, хочу 3 из флага", rep.Concurrency)
	}
	if rep.PerHostDelayMS != 1 {
		t.Errorf("отчёт назвал per_host_delay_ms=%d, хочу 1 из флага", rep.PerHostDelayMS)
	}
}

func TestDiscoverJSONNamesAppliedCrawlLimits(t *testing.T) {
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	t.Setenv("VOIDSEARCH_REQUEST_TIMEOUT", "2s")

	stdout, stderr, code := runMainSplit(t, "discover", "--crawl",
		"-depth", "2", "-max-hosts", "3", "--delay", "1ms",
		"-timeout", "6s", "--seeds", "http://unknaaaaaaaaaaaa.onion/", "--json")
	if code != 0 {
		t.Fatalf("discover завершился с кодом %d, stderr: %s", code, stderr)
	}
	var rep struct {
		Crawl struct {
			Depth          int   `json:"depth"`
			MaxHosts       int   `json:"max_hosts"`
			PerHostDelayMS int64 `json:"per_host_delay_ms"`
		} `json:"crawl"`
	}
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("отчёт discover не JSON: %v, вывод: %s", err, firstN(stdout, 300))
	}
	if rep.Crawl.Depth != 2 {
		t.Errorf("отчёт назвал crawl.depth=%d, хочу 2 из флага", rep.Crawl.Depth)
	}
	if rep.Crawl.MaxHosts != 3 {
		t.Errorf("отчёт назвал crawl.max_hosts=%d, хочу 3 из флага", rep.Crawl.MaxHosts)
	}
	if rep.Crawl.PerHostDelayMS != 1 {
		t.Errorf("отчёт назвал crawl.per_host_delay_ms=%d, хочу 1 из флага", rep.Crawl.PerHostDelayMS)
	}
	if !strings.Contains(stdout, "max_hosts") {
		t.Errorf("в JSON нет ключа max_hosts, фактически: %s", firstN(stdout, 400))
	}
}
