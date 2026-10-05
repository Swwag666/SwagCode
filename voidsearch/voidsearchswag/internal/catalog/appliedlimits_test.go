package catalog

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
//	collect --limit 2 --delay 1ms --max-files 7 --concurrency 2 --json
//	    rc=0, ключи отчёта: elapsed, failed, hosts, links, pages, saved, skipped
//
// То есть отчёт показывал только итог. Оператор не мог отличить «нашли семь
// файлов, потому что больше нет» от «нашли семь, потому что потолок семь»: оба
// выглядят как saved=7.
func TestCollectReportNamesAppliedLimits(t *testing.T) {
	st := newStore(t)
	f := &fakeClient{}
	c := NewCollector(f, st, nil, nil, Config{
		MaxHosts: 2, MaxFiles: 7, Concurrency: 3, PerHostDelay: time.Millisecond,
	})
	rep, err := c.Collect(context.Background(), []string{v2a + ".onion"}, "task-applied")
	if err != nil {
		t.Fatal(err)
	}
	if rep.MaxFiles != 7 {
		t.Errorf("отчёт назвал max_files=%d, хочу 7: таков потолок из конфига", rep.MaxFiles)
	}
	if rep.Concurrency != 3 {
		t.Errorf("отчёт назвал concurrency=%d, хочу 3", rep.Concurrency)
	}
	if rep.PerHostDelayMS != 1 {
		t.Errorf("отчёт назвал per_host_delay_ms=%d, хочу 1", rep.PerHostDelayMS)
	}
}

// Отчёт обязан называть применённое значение, а не запрошенное: неположительные
// потолки подменяет withDefaults, и расхождение между флагом и реальностью было
// главным, что отчёт не показывал.
func TestCollectReportNamesSubstitutedDefaults(t *testing.T) {
	st := newStore(t)
	c := NewCollector(&fakeClient{}, st, nil, nil, Config{})
	rep, err := c.Collect(context.Background(), []string{v2a + ".onion"}, "task-defaults")
	if err != nil {
		t.Fatal(err)
	}
	want := Config{}.withDefaults()
	if rep.MaxFiles != want.MaxFiles {
		t.Errorf("отчёт назвал max_files=%d, хочу %d из withDefaults", rep.MaxFiles, want.MaxFiles)
	}
	if rep.Concurrency != want.Concurrency {
		t.Errorf("отчёт назвал concurrency=%d, хочу %d из withDefaults", rep.Concurrency, want.Concurrency)
	}
	if rep.PerHostDelayMS != want.PerHostDelay.Milliseconds() {
		t.Errorf("отчёт назвал per_host_delay_ms=%d, хочу %d из withDefaults",
			rep.PerHostDelayMS, want.PerHostDelay.Milliseconds())
	}
}

// Поля обязаны доезжать до JSON под своими именами: отчёт читают машины, и поле
// без тега или с пустым значением исчезло бы из ответа.
func TestCollectReportJSONCarriesAppliedLimits(t *testing.T) {
	st := newStore(t)
	c := NewCollector(&fakeClient{}, st, nil, nil, Config{MaxFiles: 7, Concurrency: 3, PerHostDelay: time.Millisecond})
	rep, err := c.Collect(context.Background(), []string{v2a + ".onion"}, "task-json")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("отчёт не сериализуется: %v", err)
	}
	body := string(raw)
	for _, want := range []string{`"max_files":7`, `"concurrency":3`, `"per_host_delay_ms":1`} {
		if !strings.Contains(body, want) {
			t.Errorf("в JSON отчёта нет %s, фактически: %s", want, firstNBytes(body, 400))
		}
	}
}

func firstNBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
