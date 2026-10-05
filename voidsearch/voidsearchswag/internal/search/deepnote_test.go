package search

import (
	"context"
	"errors"
	"strings"
	"testing"

	"voidsearchswag/internal/router"
	"voidsearchswag/internal/searchers"
)

func TestDeepNoteAdmitsFailedFallback(t *testing.T) {
	// Пояснение деградации ставилось до фоллбэка на clearnet и переживало его
	// провал: при живых движках 0 из N отчёт продолжал утверждать «отработал
	// clearnet». Охота подставляет Note в причину отказа, и такой текст уводил
	// оператора проверять запрос вместо починки сети.
	e, _ := newEngine(t,
		&fixedSearcher{name: "ddg-html", err: errors.New("socks failure")},
		&fixedSearcher{name: "ddg-lite", err: errors.New("socks failure")},
	)
	out, err := e.Search(context.Background(), Options{Query: "onion leak dump", Mode: router.ModeDeep})
	if err != nil {
		t.Fatal(err)
	}
	if out.Report.Live != 0 {
		t.Fatalf("Live = %d, ожидала 0: фикстура перестала быть мёртвой", out.Report.Live)
	}
	if out.Report.Total == 0 {
		t.Fatal("Total = 0: отчёт не собрал движки, EnginesDown не сработает")
	}
	if strings.Contains(out.Report.Note, "отработал clearnet") {
		t.Errorf("пояснение утверждает успешный фоллбэк при Live=0: %q", out.Report.Note)
	}
	if !strings.Contains(out.Report.Note, "тоже не ответил") {
		t.Errorf("Note = %q, ожидала признание неудачного фоллбэка", out.Report.Note)
	}
	if !out.Report.Degraded {
		t.Error("деградация не отмечена")
	}
}

func TestDeepNoteKeepsSuccessfulFallback(t *testing.T) {
	// Обратная сторона: когда clearnet действительно ответил, пояснение обязано
	// остаться прежним - оно точное, и менять его значило бы потерять смысл.
	// Результат фикстуры несёт токен запроса в адресе: фильтр шума этапа 164
	// не должен вмешиваться, иначе тест проверял бы два механизма разом.
	e, _ := newEngine(t,
		&fixedSearcher{name: "ddg-html", res: []searchers.Result{{URL: "https://clear.example/leak-dump"}}},
		&fixedSearcher{name: "ddg-lite", err: errors.New("socks failure")},
	)
	out, err := e.Search(context.Background(), Options{Query: "onion leak dump", Mode: router.ModeDeep})
	if err != nil {
		t.Fatal(err)
	}
	if out.Report.Live != 1 {
		t.Fatalf("Live = %d, ожидала 1: фикстура не отвечает", out.Report.Live)
	}
	if out.Report.Note != "onion-выдача пуста, отработал clearnet" {
		t.Errorf("Note = %q, ожидала прежний текст при успешном фоллбэке", out.Report.Note)
	}
	if out.Count == 0 {
		t.Error("выдача потеряна при живом clearnet")
	}
}
