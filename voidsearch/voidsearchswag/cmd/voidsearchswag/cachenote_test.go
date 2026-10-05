package main

import (
	"strings"
	"testing"
	"time"

	"voidsearchswag/internal/router"
	"voidsearchswag/internal/search"
	"voidsearchswag/internal/searchers"
)

func cacheNoteOutcome(note string, cached bool) *search.Outcome {
	return &search.Outcome{
		Query:     "leak database",
		Mode:      router.ModeFast,
		UsedMode:  router.ModeFast,
		Count:     1,
		Limit:     20,
		Results:   []searchers.Result{{URL: "https://a.example/1", Title: "A"}},
		Cached:    cached,
		CacheNote: note,
		Duration:  "1s",
	}
}

func TestPrintSearchOutcomeMarksCacheNote(t *testing.T) {
	// Поломка кэша обязана быть видна в тексте. До правки движок молчал, и
	// мёртвый кэш выглядел как обычный прогон: пользователь не знал, что
	// каждый запрос идёт в движки заново.
	out := cacheNoteOutcome("кэш не прочитан, запрос пошёл в движки: sql: database is closed", false)
	text := captureStdout(t, func() { printSearchOutcome(out, 20, time.Hour) })

	if !strings.Contains(text, "кэш: кэш не прочитан") {
		t.Errorf("оговорка о поломке кэша не напечатана:\n%s", text)
	}
	if !strings.Contains(text, "database is closed") {
		t.Errorf("в оговорке нет причины:\n%s", text)
	}
	// Противоположный факт печататься не должен: выдача не из кэша.
	if strings.Contains(text, "выдача из кэша") {
		t.Errorf("строка о кэшированной выдаче рядом с поломкой кэша:\n%s", text)
	}
}

func TestPrintSearchOutcomeCachedHasNoNote(t *testing.T) {
	// Кэшированная выдача и оговорка о поломке - взаимоисключающие состояния:
	// движок чистит cache_note, когда запись отдалась.
	out := cacheNoteOutcome("", true)
	text := captureStdout(t, func() { printSearchOutcome(out, 20, time.Hour) })

	if !strings.Contains(text, "выдача из кэша") {
		t.Errorf("строка о кэшированной выдаче потеряна:\n%s", text)
	}
	if strings.Contains(text, "кэш: ") {
		t.Errorf("оговорка о поломке при выдаче из кэша:\n%s", text)
	}
}

func TestPrintSearchOutcomeHealthyCacheIsSilent(t *testing.T) {
	// Живой кэш не порождает оговорки: иначе она обесценится и перестанет
	// означать поломку.
	out := cacheNoteOutcome("", false)
	text := captureStdout(t, func() { printSearchOutcome(out, 20, time.Hour) })

	if strings.Contains(text, "кэш: ") {
		t.Errorf("оговорка о кэше при здоровом кэше:\n%s", text)
	}
	if strings.Contains(text, "выдача из кэша") {
		t.Errorf("свежий прогон выдан за кэшированный:\n%s", text)
	}
}

func TestSearchOutcomeJSONCarriesCacheNote(t *testing.T) {
	// Машинный потребитель получает оговорку тем же полем: CLI и MCP отдают
	// один и тот же Outcome.
	out := cacheNoteOutcome("запись в кэш не удалась: sql: database is closed", false)
	if out.CacheNote == "" {
		t.Fatal("оговорка потеряна до сериализации")
	}
	if !strings.Contains(out.CacheNote, "запись в кэш не удалась") {
		t.Errorf("оговорка не о записи: %q", out.CacheNote)
	}
}
