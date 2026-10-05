package main

import (
	"strings"
	"testing"

	"voidsearchswag/internal/search"
	"voidsearchswag/internal/searchers"
)

// Живой замер до правки на HEAD d4c9f03: молчащий TCP-слушатель на 127.0.0.1:18999
// в роли socks-прокси, VOIDSEARCH_REQUEST_TIMEOUT=2s, транспорт static, копия
// боевой базы, search test --limit 2 --no-cache длился 16184 мс и печатал
//
//	запрос: test
//	режим: fast  |  0 результатов  |  16.011s
//	запрошено 2, получено 0: больше не дали сами движки, а не фильтр
//	ничего не найдено: все движки отвалились
//	  ddg-html: ddg: context deadline exceeded
//	  ddg-lite: ddg: context deadline exceeded
//	решение: явных признаков нет -> быстрый режим по умолчанию
//	движки: ddg-html:fail, ddg-lite:fail
//
// Двенадцать секунд из шестнадцати не объясняла ни одна строка: stealth и deep
// отработали и исчезли из отчёта вместе со своей длительностью.
func TestPrintSearchFallbacksNamesModesAndDurations(t *testing.T) {
	out := captureStdout(t, func() {
		printSearchFallbacks([]search.FallbackReport{
			{Mode: "stealth", Duration: "4.002s", Engines: 2, Live: 0},
			{Mode: "deep", Duration: "8.005s", Engines: 3, Live: 1},
		})
	})
	want := "запасные режимы после пустой выдачи: stealth 4.002s (движков 2, живых 0), " +
		"deep 8.005s (движков 3, живых 1)\n"
	if out != want {
		t.Errorf("строка запасных режимов разошлась\nхочу: %q\nфакт: %q", want, out)
	}
}

// Пустой список не печатается вовсе: строка без продолжения выглядела бы
// оборванной, а обычный прогон без запасных режимов обязан остаться прежним.
func TestPrintSearchFallbacksSilentWhenNone(t *testing.T) {
	for _, list := range [][]search.FallbackReport{nil, {}} {
		out := captureStdout(t, func() { printSearchFallbacks(list) })
		if out != "" {
			t.Errorf("пустой список запасных режимов напечатал %q", out)
		}
	}
}

// Строка обязана появляться в выводе команды, а не только в функции: иначе она
// осталась бы мёртвым кодом, который никто не вызывает.
func TestSearchTextOutputNamesFallbacks(t *testing.T) {
	out := &search.Outcome{
		Query:    "test",
		Mode:     "fast",
		UsedMode: "fast",
		Count:    0,
		Duration: "16.011s",
		Report: search.Report{
			Engines: []search.EngineReport{
				{Name: "ddg-html", Error: "ddg: context deadline exceeded", Elapsed: "4.002s"},
			},
			Live:  0,
			Total: 2,
			Fallbacks: []search.FallbackReport{
				{Mode: "stealth", Duration: "4.003s", Engines: 2, Live: 0},
				{Mode: "deep", Duration: "8.004s", Engines: 1, Live: 0},
			},
		},
	}
	text := captureStdout(t, func() { printSearchOutcome(out, 2, 0) })
	if !strings.Contains(text, "запасные режимы после пустой выдачи: stealth 4.003s") {
		t.Errorf("вывод команды не назвал запасные режимы:\n%s", firstN(text, 800))
	}
	if !strings.Contains(text, "deep 8.004s (движков 1, живых 0)") {
		t.Errorf("вывод команды не назвал второй запасной режим:\n%s", firstN(text, 800))
	}
	if !strings.Contains(text, "16.011s") {
		t.Errorf("вывод команды потерял длительность:\n%s", firstN(text, 800))
	}
}

// Примечание отчёта обязано печататься: там лежит причина пропущенных запасных
// режимов и объяснение деградации в deep. До правки оно уходило только в JSON и в
// ответ MCP, и оператор в терминале видел «ничего не найдено» без объяснения, что
// поиск даже не пытался уйти в другой режим.
func TestSearchTextOutputPrintsReportNote(t *testing.T) {
	out := &search.Outcome{
		Query:    "test",
		Mode:     "fast",
		UsedMode: "fast",
		Count:    0,
		Duration: "4.004s",
		Report: search.Report{
			Engines: []search.EngineReport{
				{Name: "ddg-html", Error: "ddg: context deadline exceeded", Elapsed: "4.004s", Timeout: true},
			},
			Live:     0,
			Total:    1,
			Degraded: true,
			Note:     "запасные режимы не запускались: все движки легли по сроку, транспорт не отвечает",
		},
	}
	text := captureStdout(t, func() { printSearchOutcome(out, 2, 0) })
	want := "примечание: запасные режимы не запускались: все движки легли по сроку, транспорт не отвечает\n"
	if !strings.Contains(text, want) {
		t.Errorf("вывод команды не напечатал примечание %q:\n%s", want, firstN(text, 800))
	}
	if strings.Contains(text, "запасные режимы после пустой выдачи") {
		t.Errorf("вывод команды объявил запасные режимы, которых не было:\n%s", firstN(text, 800))
	}
}

// Без примечания строка не печатается вовсе: обычный прогон обязан остаться
// прежним.
func TestSearchTextOutputOmitsEmptyReportNote(t *testing.T) {
	out := &search.Outcome{
		Query:    "test",
		Mode:     "fast",
		UsedMode: "fast",
		Count:    1,
		Duration: "1.2s",
		Results:  []searchers.Result{{URL: "https://f1.example/1", Title: "первый", Rank: 1}},
		Report: search.Report{
			Engines: []search.EngineReport{{Name: "ddg-html", OK: true, Count: 1, Elapsed: "1.2s"}},
			Live:    1,
			Total:   1,
		},
	}
	text := captureStdout(t, func() { printSearchOutcome(out, 2, 0) })
	if strings.Contains(text, "примечание:") {
		t.Errorf("пустое примечание попало в вывод:\n%s", firstN(text, 800))
	}
}
