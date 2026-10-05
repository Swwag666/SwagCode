package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"voidsearchswag/internal/discover"
	"voidsearchswag/internal/store"
)

// Живой замер до правки на HEAD 40a742b, пересобранный бинарник, tor выключен,
// транспорт direct.
//
// Очередь из пяти непроверенных адресов, три из них не являются onion-адресами:
//
//	probe --limit 5 --json  rc=1, total=2, live=0, dead=0, skipped=2,
//	                        results=2, ключи [total,live,dead,skipped,elapsed,results]
//	probe --limit 5         rc=1, «пробы: живых 0 из 2 (пропущено 2: нужен tor
//	                        или прокси) за 1ms» и две строки адресов
//
// Три отброшенных адреса не фигурировали ни в одном поле и ни в одной строке.
//
// Очередь из пяти адресов, где невалидны все:
//
//	probe --limit 5 --json  rc=0, total=0, live=0, dead=0, skipped=0, results=0
//	probe --limit 5         rc=0, «пробы: живых 0 из 0 за 0s»
//
// Команда сообщала «проверять нечего» и возвращала успех при пяти непроверенных
// записях в пуле, хотя собственный комментарий probeExitCode требует отличать
// «пул плох» от «проверка не состоялась».
var rejectedURLs = []string{
	"http://bad0host1aaaaaaa.onion/",
	"http://bad8host9aaaaaaa.onion/",
	"http://short.onion/",
	"http://notonion.example/",
	"http://toolonglabel0123456789abcdef.onion/",
}

var validURLs = []string{
	"http://unknaaaaaaaaaaaa.onion/",
	"http://unknaaaaaaaaaaab.onion/",
}

// seedPoolURLs пишет в пул адреса как есть, без проверки корректности: именно
// невалидные записи и есть предмет этих тестов.
func seedPoolURLs(t *testing.T, dir string, urls []string) {
	t.Helper()
	st, err := store.Open(filepath.Join(dir, "voidsearchswag.db"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for i, u := range urls {
		o := store.Onion{URL: u, Title: fmt.Sprintf("seed host %d", i), Status: "unknown"}
		if err := st.UpsertOnion(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	queue, err := st.NextProbeWave(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(queue) != len(urls) {
		t.Fatalf("в очереди %d адресов, хочу %d", len(queue), len(urls))
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
}

type probeReportJSON struct {
	Total         int      `json:"total"`
	Live          int      `json:"live"`
	Dead          int      `json:"dead"`
	Skipped       int      `json:"skipped"`
	Rejected      int      `json:"rejected"`
	RejectedAddrs []string `json:"rejected_addrs"`
}

// emptyProviderServer поднимает локального провайдера, отвечающего одним
// валидным, но мёртвым адресом. Живой замер ДО этапа 159: poolcheck-тесты
// уходили fetch'ем на публичный api.proxyscrape.com и падали от его
// настроения - «ERROR: пул: proxyscrape/http: Get
// "https://api.proxyscrape.com/v2/...": EOF» в прогоне
// TestPoolcheckFlagsOverrideConfigValues, а соседний прогон того же кода
// при живом api проходил за 2.5с.
//
// Пустой ответ не годится: по контракту Fetch «пустой ответ» - ошибка, и
// poolcheck завершается rc=1. Один мёртвый адрес даёт провайдеру право
// ответить «вот список», список проходит парсер, а с --skip-verify живость
// не проверяется: полный локальный контракт «провайдер доступен, пул
// собран», без единого байта наружу.
func emptyProviderServer(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("127.0.0.1:1\n"))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// setOfflineProbeEnv собирает окружение для offline-прогона команды:
// данные в temp, tor и транспорт выключены, пул адресов пуст, а провайдер
// прокси подменён локальным. Инвариант локальности провайдера держит
// отдельный тест - см. TestSetOfflineProbeEnvKeepsProviderLocal.
func setOfflineProbeEnv(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	t.Setenv("VOIDSEARCH_BACKUP_DIR", "")
	t.Setenv("VOIDSEARCH_TOR", "off")
	t.Setenv("VOIDSEARCH_TRANSPORT", "direct")
	t.Setenv("VOIDSEARCH_PROXIES", "")
	t.Setenv("VOIDSEARCH_PROXY_PROVIDER_URL", emptyProviderServer(t))
}

func TestProbeExitCodeFailsWhenWholeQueueRejected(t *testing.T) {
	cases := []struct {
		name string
		rep  discover.ProbeReport
		want int
	}{
		{"пустая очередь", discover.ProbeReport{}, 0},
		{"вся очередь отброшена", discover.ProbeReport{Rejected: 5}, 1},
		{"все пробы пропущены", discover.ProbeReport{Total: 2, Skipped: 2}, 1},
		{"волна состоялась", discover.ProbeReport{Total: 2, Live: 1, Dead: 1}, 0},
		{"часть адресов отброшена, волна состоялась", discover.ProbeReport{Total: 2, Live: 2, Rejected: 3}, 0},
	}
	for _, c := range cases {
		if got := probeExitCode(c.rep); got != c.want {
			t.Errorf("%s: probeExitCode = %d, хочу %d", c.name, got, c.want)
		}
	}
}

func TestPrintProbeReportNamesRejectedHosts(t *testing.T) {
	rep := discover.ProbeReport{
		Total:         2,
		Skipped:       2,
		Elapsed:       "1ms",
		Rejected:      3,
		RejectedAddrs: rejectedURLs[:3],
	}
	out := captureStdout(t, func() { printProbeReport(rep) })
	for _, want := range []string{
		"пробы: живых 0 из 2",
		"отброшено 3",
		"не onion-адрес или повтор в выборке",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("в выводе нет %q, фактически:\n%s", want, out)
		}
	}
	for _, addr := range rejectedURLs[:3] {
		if !strings.Contains(out, strings.TrimSuffix(strings.TrimPrefix(addr, "http://"), "/")) {
			t.Errorf("в выводе нет отброшенного адреса %q, фактически:\n%s", addr, out)
		}
	}
}

// Нулевой счётчик отброшенных не должен появляться в выводе: строка «отброшено 0»
// на чистой выборке читалась бы как предупреждение.
func TestPrintProbeReportSilentWithoutRejected(t *testing.T) {
	rep := discover.ProbeReport{Total: 1, Skipped: 1, Elapsed: "1ms"}
	out := captureStdout(t, func() { printProbeReport(rep) })
	if strings.Contains(out, "отброшено") {
		t.Errorf("на чистой выборке напечатано «отброшено»: %s", out)
	}
}

func TestProbeCLIReportsRejectedHostsInRealRun(t *testing.T) {
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	seedPoolURLs(t, dir, append(append([]string{}, rejectedURLs[:3]...), validURLs...))

	stdout, stderr, code := runMainSplit(t, "probe", "--limit", "5", "--json")
	if code != 1 {
		t.Errorf("код возврата %d, хочу 1: без tor ни одна проба не состоялась", code)
	}
	if stderr != "" {
		t.Errorf("stderr не пуст: %q", stderr)
	}
	var rep probeReportJSON
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("разбор JSON: %v (%q)", err, firstN(stdout, 300))
	}
	if rep.Total != 2 {
		t.Errorf("total = %d, хочу 2", rep.Total)
	}
	if rep.Rejected != 3 {
		t.Errorf("rejected = %d, хочу 3", rep.Rejected)
	}
	if len(rep.RejectedAddrs) != 3 {
		t.Fatalf("rejected_addrs несёт %d строк, хочу 3: %v", len(rep.RejectedAddrs), rep.RejectedAddrs)
	}
	for i, want := range rejectedURLs[:3] {
		if rep.RejectedAddrs[i] != want {
			t.Errorf("rejected_addrs[%d] = %q, хочу %q", i, rep.RejectedAddrs[i], want)
		}
	}
}

func TestProbeCLIFailsWhenWholeQueueRejected(t *testing.T) {
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	seedPoolURLs(t, dir, rejectedURLs)

	stdout, _, code := runMainSplit(t, "probe", "--limit", "5", "--json")
	if code != 1 {
		t.Errorf("код возврата %d, хочу 1: очередь полна, но ни один адрес не дошёл до сети", code)
	}
	var rep probeReportJSON
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("разбор JSON: %v (%q)", err, firstN(stdout, 300))
	}
	if rep.Total != 0 {
		t.Errorf("total = %d, хочу 0", rep.Total)
	}
	if rep.Rejected != len(rejectedURLs) {
		t.Errorf("rejected = %d, хочу %d", rep.Rejected, len(rejectedURLs))
	}
}

func TestProbeCLITextNamesRejectedHosts(t *testing.T) {
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	seedPoolURLs(t, dir, rejectedURLs)

	stdout, _, _ := runMainSplit(t, "probe", "--limit", "5")
	for _, want := range []string{"пробы: живых 0 из 0", "отброшено 5", "bad0host1aaaaaaa.onion"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("в выводе нет %q, фактически:\n%s", want, stdout)
		}
	}
}

// Пустая очередь остаётся нулевым исходом: код возврата обязан отличать «нечего
// проверять» от «всё отброшено».
func TestProbeCLIEmptyQueueStillSucceeds(t *testing.T) {
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	stdout, _, code := runMainSplit(t, "probe", "--limit", "5", "--json")
	if code != 0 {
		t.Errorf("код возврата %d на пустой очереди, хочу 0", code)
	}
	var rep probeReportJSON
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("разбор JSON: %v (%q)", err, firstN(stdout, 300))
	}
	if rep.Total != 0 || rep.Rejected != 0 {
		t.Errorf("на пустой очереди total=%d rejected=%d, хочу нули", rep.Total, rep.Rejected)
	}
}
