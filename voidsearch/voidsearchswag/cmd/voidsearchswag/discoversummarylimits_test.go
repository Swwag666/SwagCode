package main

import (
	"strings"
	"testing"

	"voidsearchswag/internal/discover"
)

// Живой замер до правки на HEAD e22e51e, пересобранный бинарник, свежая копия
// боевой базы на каждый прогон, тор выключен, транспорт direct:
//
//	discover --crawl -depth 2 -max-hosts 3 --delay 1ms -timeout 8s --seeds ...
//	    обход: страниц 1 (успешно 0, ошибок 0, без tor 1), глубина 2, за 1ms
//	discover --crawl -timeout 8s --seeds ...
//	    обход: страниц 1 (успешно 0, ошибок 0, без tor 1), глубина 2, за 0s
//
// Потолок три и потолок пятьдесят, пауза миллисекунда и пауза две секунды дали
// неразличимую строку, хотя JSON того же прогона называл все три числа после
// этапа 137.
func TestPrintDiscoverSummaryNamesCrawlLimits(t *testing.T) {
	res := discover.Result{
		Found: 1,
		Crawl: &discover.CrawlReport{
			Pages: 1, NoTransport: 1, Depth: 2, Elapsed: "1ms",
			MaxHosts: 3, PerHostDelayMS: 1,
		},
	}

	out := captureStdout(t, func() { printDiscoverSummary(res) })
	want := "глубина 2, потолок хостов 3, пауза 1ms, за 1ms"
	if !strings.Contains(out, want) {
		t.Errorf("строка обхода не называет применённые пределы, хочу %q, фактически:\n%s", want, out)
	}
}

// Пределы печатаются и тогда, когда прогон взял их не из флагов, а из дефолтов
// ядра: этот случай оператор не может восстановить по памяти.
func TestPrintDiscoverSummaryNamesCrawlDefaults(t *testing.T) {
	res := discover.Result{
		Found: 1,
		Crawl: &discover.CrawlReport{
			Pages: 5, Ok: 4, Failed: 1, Depth: 2, Elapsed: "3s",
			MaxHosts: 50, PerHostDelayMS: 2000,
		},
	}

	out := captureStdout(t, func() { printDiscoverSummary(res) })
	want := "глубина 2, потолок хостов 50, пауза 2s, за 3s"
	if !strings.Contains(out, want) {
		t.Errorf("строка обхода не называет пределы из дефолтов, хочу %q, фактически:\n%s", want, out)
	}
}

// Оговорка о страницах без транспорта обязана пережить добавление пределов: она
// отличает «сервис отказал» от «сервис не спросили».
func TestPrintDiscoverSummaryKeepsNoTransportWithLimits(t *testing.T) {
	res := discover.Result{
		Found: 1,
		Crawl: &discover.CrawlReport{
			Pages: 2, NoTransport: 2, Depth: 1, Elapsed: "0s",
			MaxHosts: 7, PerHostDelayMS: 500,
		},
	}

	out := captureStdout(t, func() { printDiscoverSummary(res) })
	want := "обход: страниц 2 (успешно 0, ошибок 0, без tor 2), глубина 1, потолок хостов 7, пауза 500ms, за 0s"
	if !strings.Contains(out, want) {
		t.Errorf("нет строки %q:\n%s", want, out)
	}
}

// Реальный прогон держит связку «флаг доехал до текста»: без него функция могла
// бы печатать пределы, а команда перестала бы их передавать в отчёт.
func TestDiscoverTextNamesAppliedCrawlLimits(t *testing.T) {
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	t.Setenv("VOIDSEARCH_REQUEST_TIMEOUT", "2s")

	stdout, stderr, code := runMainSplit(t, "discover", "--crawl",
		"-depth", "2", "-max-hosts", "3", "--delay", "1ms",
		"-timeout", "6s", "--seeds", "http://unknaaaaaaaaaaaa.onion/")
	if code != 0 {
		t.Fatalf("discover завершился с кодом %d, stderr: %s", code, stderr)
	}
	want := "глубина 2, потолок хостов 3, пауза 1ms"
	if !strings.Contains(stdout, want) {
		t.Errorf("текстовый отчёт не называет применённые пределы, хочу %q, фактически:\n%s",
			want, firstN(stdout, 500))
	}
}
