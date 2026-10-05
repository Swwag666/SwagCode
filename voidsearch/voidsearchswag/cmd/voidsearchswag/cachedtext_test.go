package main

import (
	"strings"
	"testing"
	"time"

	"voidsearchswag/internal/router"
	"voidsearchswag/internal/search"
	"voidsearchswag/internal/searchers"
)

func cachedOutcome(cached bool) *search.Outcome {
	return &search.Outcome{
		Query:    "duckduckgo test",
		Mode:     router.ModeFast,
		UsedMode: router.ModeFast,
		Count:    2,
		Cached:   cached,
		Duration: "1ms",
		Limit:    5,
		Decision: router.Decision{Mode: router.ModeFast, Reason: "явных признаков нет"},
		Report: search.Report{Engines: []search.EngineReport{
			{Name: "ddg-html", OK: true, Count: 2},
			{Name: "ddg-lite", OK: false, Error: "таймаут"},
		}},
		Results: []searchers.Result{
			{Rank: 1, Title: "первый", URL: "https://one.example/1", Snippet: "коротко"},
			{Rank: 2, Title: "второй", URL: "https://two.example/2"},
		},
	}
}

func TestPrintSearchOutcomeMarksCached(t *testing.T) {
	// Ответ из кэша печатался теми же словами, что и живой прогон. Строка
	// «движки: ddg-html:2, ddg-lite:fail» утверждала, что ddg-lite лёг сейчас,
	// хотя запись сделана раньше и ни один движок не запрашивался.
	out := cachedOutcome(true)
	text := captureStdout(t, func() { printSearchOutcome(out, 5, time.Hour) })

	for _, want := range []string{
		"выдача из кэша",
		"движки в этом прогоне не запрашивались",
		"движки (запись кэша)",
		"1h0m0s",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("в тексте нет %q:\n%s", want, text)
		}
	}
}

func TestPrintSearchOutcomeFreshHasNoCacheMarks(t *testing.T) {
	// Обратная сторона: живой прогон не должен обрастать оговорками, иначе
	// отметка потеряет смысл.
	out := cachedOutcome(false)
	text := captureStdout(t, func() { printSearchOutcome(out, 5, time.Hour) })

	if strings.Contains(text, "кэш") {
		t.Errorf("в живом прогоне появилась отметка о кэше:\n%s", text)
	}
	if !strings.Contains(text, "движки: ") {
		t.Errorf("строка движков потеряна:\n%s", text)
	}
}

func TestPrintSearchOutcomeCacheShortfall(t *testing.T) {
	// Нехватка против -limit объяснялась отказом движков и в кэшированном
	// ответе, где движки не участвовали.
	out := cachedOutcome(true)
	out.Count = 2
	text := captureStdout(t, func() { printSearchOutcome(out, 30, time.Hour) })

	if !strings.Contains(text, "столько лежит в записи кэша") {
		t.Errorf("нехватка в кэше объяснена не тем:\n%s", text)
	}
	if strings.Contains(text, "сами движки") {
		t.Errorf("в кэшированном ответе снова ссылка на движки:\n%s", text)
	}
}

func TestPrintSearchOutcomeFreshShortfall(t *testing.T) {
	out := cachedOutcome(false)
	out.Count = 2
	text := captureStdout(t, func() { printSearchOutcome(out, 30, time.Hour) })

	if !strings.Contains(text, "больше не дали сами движки") {
		t.Errorf("объяснение нехватки потеряно:\n%s", text)
	}
}

func TestPrintSearchOutcomeEmptyFromCache(t *testing.T) {
	// Пустая выдача из кэша: причина берётся из отчёта прошлого прогона, и это
	// должно быть видно.
	out := cachedOutcome(true)
	out.Count = 0
	out.Results = nil
	out.Report.Engines = []search.EngineReport{{Name: "ddg-lite", OK: false, Error: "таймаут"}}
	text := captureStdout(t, func() { printSearchOutcome(out, 5, 0) })

	if !strings.Contains(text, "все движки отвалились (по записи кэша)") {
		t.Errorf("причина пустоты не помечена как взятая из записи:\n%s", text)
	}
	// cacheTTL равен нулю - строка про срок жизни записи не печатается, но сам
	// факт кэша остаётся.
	if !strings.Contains(text, "выдача из кэша") {
		t.Errorf("при нулевом TTL отметка о кэше пропала:\n%s", text)
	}
	if strings.Contains(text, "1h0m0s") || strings.Contains(text, "0s") {
		t.Errorf("при нулевом TTL напечатан срок жизни записи:\n%s", text)
	}
}

func TestPrintSearchOutcomeEmptyFreshKeepsWording(t *testing.T) {
	// Семантика живого прогона сохранена: три прежних формулировки без оговорок.
	out := cachedOutcome(false)
	out.Count = 0
	out.Results = nil
	out.Report.Engines = []search.EngineReport{{Name: "ddg-lite", OK: false, Error: "таймаут"}}
	text := captureStdout(t, func() { printSearchOutcome(out, 5, time.Hour) })

	if !strings.Contains(text, "ничего не найдено: все движки отвалились\n") {
		t.Errorf("формулировка живого прогона изменена:\n%s", text)
	}
	if strings.Contains(text, "по записи кэша") {
		t.Errorf("в живом прогоне появилась оговорка о кэше:\n%s", text)
	}
}
