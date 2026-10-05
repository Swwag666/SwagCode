package main

import (
	"context"
	"strings"
	"testing"

	"voidsearchswag/internal/catalog"
)

// Живой замер до правки на HEAD 58a8751, пересобранный бинарник, копия боевой
// базы, тор выключен, транспорт direct:
//
//	collect --limit 2 --max-files 7 --concurrency 3 --delay 1ms
//	    сбор: хостов 2, страниц 0, ссылок 0, файлов 0 за 0s
//	    отказавших хостов: 2 из 2
//	    задача: collect-20260929-132128
//	    каталог всего: 19 файлов, 27.9 MiB
//	collect --limit 2
//	    сбор: хостов 2, страниц 0, ссылок 0, файлов 0 за 1ms
//	    отказавших хостов: 2 из 2
//	    задача: collect-20260929-132129
//	    каталог всего: 19 файлов, 27.9 MiB
//
// Два прогона с потолком семь и с потолком пятьсот, с паузой миллисекунда и с
// паузой две секунды, напечатали один и тот же текст: JSON после этапа 137 уже
// называл все три числа, а текстовая ветка - нет.
func TestPrintCollectReportNamesAppliedLimits(t *testing.T) {
	dir := t.TempDir()
	st := openTestStore(t, dir)
	defer closeStore(st)

	out := captureStdout(t, func() {
		printCollectReport(context.Background(), st, catalog.Report{
			Hosts: 2, Pages: 1, Saved: 7, Elapsed: "3s",
			MaxFiles: 7, Concurrency: 3, PerHostDelayMS: 1,
		}, "collect-limits")
	})
	want := "пределы: файлов 7, потоков 3, пауза 1ms"
	if !strings.Contains(out, want) {
		t.Errorf("отчёт не назвал применённые пределы, хочу %q, фактически:\n%s", want, out)
	}
}

// Пределы печатаются и тогда, когда прогон взял их не из флагов, а из дефолтов
// ядра: именно этот случай оператор не может восстановить по памяти.
func TestPrintCollectReportNamesSubstitutedDefaults(t *testing.T) {
	dir := t.TempDir()
	st := openTestStore(t, dir)
	defer closeStore(st)

	out := captureStdout(t, func() {
		printCollectReport(context.Background(), st, catalog.Report{
			Hosts: 1, Elapsed: "1s",
			MaxFiles: 500, Concurrency: 4, PerHostDelayMS: 2000,
		}, "collect-defaults")
	})
	want := "пределы: файлов 500, потоков 4, пауза 2s"
	if !strings.Contains(out, want) {
		t.Errorf("отчёт не назвал пределы из дефолтов, хочу %q, фактически:\n%s", want, out)
	}
}

// Число сохранённых файлов обязано стоять рядом с потолком, иначе строка пределов
// уедет в конец отчёта и её перестанут читать.
func TestPrintCollectReportPutsLimitsNextToSavedCount(t *testing.T) {
	dir := t.TempDir()
	st := openTestStore(t, dir)
	defer closeStore(st)

	out := captureStdout(t, func() {
		printCollectReport(context.Background(), st, catalog.Report{
			Hosts: 1, Saved: 7, Elapsed: "1s",
			MaxFiles: 7, Concurrency: 2, PerHostDelayMS: 1,
		}, "collect-order")
	})
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		t.Fatalf("отчёт короче двух строк: %q", out)
	}
	if !strings.Contains(lines[0], "файлов 7") {
		t.Errorf("первая строка не называет число файлов: %q", lines[0])
	}
	if !strings.Contains(lines[1], "пределы: файлов 7") {
		t.Errorf("вторая строка не называет потолок файлов: %q", lines[1])
	}
}

func TestAppliedDelayFormatsMilliseconds(t *testing.T) {
	cases := map[int64]string{
		0:     "0s",
		1:     "1ms",
		500:   "500ms",
		1500:  "1.5s",
		2000:  "2s",
		60000: "1m0s",
	}
	for ms, want := range cases {
		if got := appliedDelay(ms); got != want {
			t.Errorf("appliedDelay(%d) = %q, хочу %q", ms, got, want)
		}
	}
}
