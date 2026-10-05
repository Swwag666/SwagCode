package main

import (
	"encoding/json"
	"strings"
	"testing"

	"voidsearchswag/internal/discover"
)

// Живой замер до правки на HEAD 45c45db, пересобранный бинарник, копия боевой
// базы, тор выключен, транспорт direct:
//
//	probe --limit 3
//	    пробы: живых 0 из 3 (пропущено 3: нужен tor или прокси) за 1ms
//
// Строка называла итог волны, но не называла ни запрошенную выборку, ни
// параллельность, ни таймаут, ни паузу.
func TestPrintProbeReportNamesAppliedLimits(t *testing.T) {
	out := captureStdout(t, func() {
		printProbeReport(discover.ProbeReport{
			Total: 3, Live: 0, Skipped: 3, Elapsed: "1ms",
			RequestedLimit: 3, Concurrency: 8, TimeoutMS: 30000, DelayMS: 0,
		})
	})
	want := "пределы: выборка 3, потоков 8, таймаут 30s, пауза 0s"
	if !strings.Contains(out, want) {
		t.Errorf("отчёт проб не назвал применённые пределы, хочу %q, фактически:\n%s", want, out)
	}
}

// Оговорка о пропусках обязана пережить добавление пределов: она отличает
// «сервис не ответил» от «запрос не ушёл в сеть».
func TestPrintProbeReportKeepsSkippedLineWithLimits(t *testing.T) {
	out := captureStdout(t, func() {
		printProbeReport(discover.ProbeReport{
			Total: 2, Skipped: 2, Elapsed: "0s",
			RequestedLimit: 5, Concurrency: 4, TimeoutMS: 1000, DelayMS: 500,
		})
	})
	for _, want := range []string{
		"пробы: живых 0 из 2 (пропущено 2: нужен tor или прокси) за 0s",
		"пределы: выборка 5, потоков 4, таймаут 1s, пауза 500ms",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("в отчёте нет %q:\n%s", want, out)
		}
	}
}

// Реальный прогон держит связку «флаг доехал до отчёта»: без него печать могла
// бы остаться, а команда перестала бы передавать пределы в структуру.
func TestProbeTextNamesAppliedLimits(t *testing.T) {
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	seedUnknown(t, dir, 10)

	stdout, _, _ := runMainSplit(t, "probe", "--limit", "3")
	if !strings.Contains(stdout, "пределы: выборка 3, потоков") {
		t.Errorf("текстовый отчёт проб не назвал выборку и параллельность, фактически:\n%s",
			firstN(stdout, 400))
	}
}

func TestProbeJSONNamesAppliedLimits(t *testing.T) {
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	seedUnknown(t, dir, 10)

	stdout, _, _ := runMainSplit(t, "probe", "--limit", "3", "--json")
	var rep struct {
		Total          int   `json:"total"`
		RequestedLimit int   `json:"requested_limit"`
		Concurrency    int   `json:"concurrency"`
		TimeoutMS      int64 `json:"timeout_ms"`
		DelayMS        int64 `json:"delay_ms"`
	}
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("отчёт probe не JSON: %v, вывод: %s", err, firstN(stdout, 300))
	}
	if rep.RequestedLimit != 3 {
		t.Errorf("requested_limit=%d, хочу 3 из флага", rep.RequestedLimit)
	}
	if rep.Concurrency <= 0 {
		t.Errorf("concurrency=%d, хочу положительное значение из конфига", rep.Concurrency)
	}
	if rep.TimeoutMS <= 0 {
		t.Errorf("timeout_ms=%d, хочу положительное значение", rep.TimeoutMS)
	}
	if rep.Total != 3 {
		t.Errorf("total=%d, хочу 3: в очереди десять непроверенных адресов", rep.Total)
	}
}
