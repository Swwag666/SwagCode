package discover

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Живой замер до правки на HEAD 28f4541, пересобранный бинарник, копия боевой
// базы, тор выключен, транспорт direct:
//
//	discover --crawl -depth 2 -max-hosts 3 --delay 1ms -timeout 8s --seeds ... --json
//	    rc=0, ключи crawl: depth, elapsed, failed, files, found, no_transport,
//	    ok, pages, pages_detail
//
// Глубина в отчёте была, а потолок хостов и пауза - нет, поэтому оператор не мог
// отличить «нашли три хоста, потому что больше нет» от «нашли три, потому что
// потолок три».
func TestCrawlReportNamesAppliedLimits(t *testing.T) {
	cr := NewCrawler(nil, nil, CrawlConfig{Depth: 1, MaxHosts: 3, PerHostDelay: time.Millisecond})
	_, rep := cr.Crawl(context.Background(), nil)
	if rep.Depth != 1 {
		t.Errorf("отчёт назвал depth=%d, хочу 1", rep.Depth)
	}
	if rep.MaxHosts != 3 {
		t.Errorf("отчёт назвал max_hosts=%d, хочу 3", rep.MaxHosts)
	}
	if rep.PerHostDelayMS != 1 {
		t.Errorf("отчёт назвал per_host_delay_ms=%d, хочу 1", rep.PerHostDelayMS)
	}
}

// Отчёт обязан называть применённое значение, а не запрошенное: withDefaults
// подменяет неположительные глубину, потолок и паузу, и именно эту подмену
// оператор не видел.
func TestCrawlReportNamesSubstitutedDefaults(t *testing.T) {
	cr := NewCrawler(nil, nil, CrawlConfig{})
	_, rep := cr.Crawl(context.Background(), nil)
	want := CrawlConfig{}.withDefaults()
	if rep.Depth != want.Depth {
		t.Errorf("отчёт назвал depth=%d, хочу %d из withDefaults", rep.Depth, want.Depth)
	}
	if rep.MaxHosts != want.MaxHosts {
		t.Errorf("отчёт назвал max_hosts=%d, хочу %d из withDefaults", rep.MaxHosts, want.MaxHosts)
	}
	if rep.PerHostDelayMS != want.PerHostDelay.Milliseconds() {
		t.Errorf("отчёт назвал per_host_delay_ms=%d, хочу %d из withDefaults",
			rep.PerHostDelayMS, want.PerHostDelay.Milliseconds())
	}
}

// Поля обязаны доезжать до JSON под своими именами: ключ crawl печатает CLI как
// есть, и поле без тега исчезло бы из ответа.
func TestCrawlReportJSONCarriesAppliedLimits(t *testing.T) {
	cr := NewCrawler(nil, nil, CrawlConfig{Depth: 2, MaxHosts: 3, PerHostDelay: time.Millisecond})
	_, rep := cr.Crawl(context.Background(), nil)
	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("отчёт не сериализуется: %v", err)
	}
	body := string(raw)
	for _, want := range []string{`"depth":2`, `"max_hosts":3`, `"per_host_delay_ms":1`} {
		if !strings.Contains(body, want) {
			t.Errorf("в JSON отчёта нет %s, фактически: %s", want, firstNRunes(body, 400))
		}
	}
}

func firstNRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
