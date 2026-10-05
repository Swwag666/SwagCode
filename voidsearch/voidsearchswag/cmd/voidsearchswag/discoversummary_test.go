package main

import (
	"strings"
	"testing"

	"voidsearchswag/internal/discover"
)

func TestPrintDiscoverSummaryMarksSaveFailures(t *testing.T) {
	// До правки прогон с мёртвой базой печатал «найдено адресов: 3 (новых 0,
	// обновлено 0)» и выглядел как «адреса уже известны», хотя база не приняла
	// ни одной записи.
	res := discover.Result{
		Found: 3, New: 0, Updated: 0, Elapsed: "4ms",
		SaveFailed: 3,
		LastError:  "нет таблицы onion_pool",
	}
	text := captureStdout(t, func() { printDiscoverSummary(res) })

	if !strings.Contains(text, "найдено адресов: 3 (новых 0, обновлено 0)") {
		t.Errorf("итоговая строка изменилась:\n%s", text)
	}
	if !strings.Contains(text, "отказов записи 3 (адресов 3, файлов 0, проб 0)") {
		t.Errorf("отказы записи не названы:\n%s", text)
	}
	if !strings.Contains(text, "нет таблицы onion_pool") {
		t.Errorf("в строке нет причины:\n%s", text)
	}
}

func TestPrintDiscoverSummarySplitsFailureKinds(t *testing.T) {
	// Отказ записи адреса, файла и пробы - три разных факта: смешанные в одном
	// числе они не объясняют, что именно потеряно.
	res := discover.Result{
		Found: 9, New: 0, Elapsed: "2s",
		SaveFailed: 2, FileSaveFailed: 3, ProbeFailed: 4,
		LastError: "база заблокирована",
	}
	text := captureStdout(t, func() { printDiscoverSummary(res) })

	if !strings.Contains(text, "отказов записи 9 (адресов 2, файлов 3, проб 4): база заблокирована") {
		t.Errorf("разбивка по видам записи потеряна:\n%s", text)
	}
}

func TestPrintDiscoverSummarySilentWhenWritesSucceeded(t *testing.T) {
	// Обратная сторона: при удачных записях строки об отказах быть не должно,
	// иначе она обесценится и перестанет означать поломку.
	res := discover.Result{Found: 3, New: 2, Updated: 1, Elapsed: "4ms"}
	text := captureStdout(t, func() { printDiscoverSummary(res) })

	if strings.Contains(text, "отказов записи") {
		t.Errorf("строка об отказах при удачных записях:\n%s", text)
	}
	if !strings.Contains(text, "найдено адресов: 3 (новых 2, обновлено 1)") {
		t.Errorf("итоговая строка потеряна:\n%s", text)
	}
}

func TestPrintDiscoverSummaryNoteWithoutReason(t *testing.T) {
	// Счётчики есть, а причина пуста: строка всё равно обязана сообщить число,
	// а не промолчать.
	res := discover.Result{Found: 1, SaveFailed: 1}
	text := captureStdout(t, func() { printDiscoverSummary(res) })

	if !strings.Contains(text, "отказов записи 1 (адресов 1, файлов 0, проб 0)\n") {
		t.Errorf("строка об отказах без причины потеряна или повреждена:\n%q", text)
	}
}

func TestPrintDiscoverSummaryKeepsCrawlAndSourceLines(t *testing.T) {
	// Сохранность семантики: вынос печати в функцию не должен потерять ни одну
	// существующую строку отчёта.
	res := discover.Result{
		Found: 2, New: 1, Updated: 1, Files: 4, Probed: 2, Elapsed: "1s",
		Crawl: &discover.CrawlReport{
			Pages: 5, Ok: 4, Failed: 1, Depth: 2, Elapsed: "2s",
			MaxHosts: 50, PerHostDelayMS: 2000,
			LimitHit: "потолок хостов 50",
		},
		Sources: []discover.SourceReport{
			{Name: "ahmia-address", OK: true, Found: 2, Elapsed: "1s"},
			{Name: "darkfail", OK: false, Found: 0, Elapsed: "3s", Error: "таймаут источника"},
		},
	}
	text := captureStdout(t, func() { printDiscoverSummary(res) })

	want := []string{
		"источники: 2, найдено адресов: 2 (новых 1, обновлено 1) за 1s",
		"обход: страниц 5 (успешно 4, ошибок 1), глубина 2, потолок хостов 50, пауза 2s, за 2s",
		"предел: потолок хостов 50",
		"каталог: добавлено файлов 4",
		"живость: обновлено по обходу 2 хостов",
		"ahmia-address",
		"darkfail",
		"ошибка",
		"таймаут источника",
	}
	for _, w := range want {
		if !strings.Contains(text, w) {
			t.Errorf("в отчёте нет %q:\n%s", w, text)
		}
	}
	if strings.Contains(text, "отказов записи") {
		t.Errorf("строка об отказах при нулевых счётчиках:\n%s", text)
	}
}

func TestPrintDiscoverSummaryOmitsEmptyOptionalLines(t *testing.T) {
	// Нулевые файлы, живость и обход не должны порождать своих строк: отчёт без
	// обхода обязан остаться коротким.
	res := discover.Result{Found: 1, New: 1, Elapsed: "1s"}
	text := captureStdout(t, func() { printDiscoverSummary(res) })

	for _, gone := range []string{"обход:", "каталог:", "живость:", "предел:"} {
		if strings.Contains(text, gone) {
			t.Errorf("строка %q напечатана при пустых данных:\n%s", gone, text)
		}
	}
}
