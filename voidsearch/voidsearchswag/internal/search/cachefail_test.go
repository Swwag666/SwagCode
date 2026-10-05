package search

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"voidsearchswag/internal/router"
	"voidsearchswag/internal/searchers"
)

// killStore закрывает базу под движком: последующие обращения к кэшу
// гарантированно возвращают ошибку, и это детерминированный способ получить
// мёртвый кэш без порчи схемы и без внешнего состояния.
func killStore(t *testing.T, st interface{ Close() error }) {
	t.Helper()
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestSearchReportsDeadCacheInJSON(t *testing.T) {
	// Главная проверка. Ошибка CacheGet уходила в err == nil && ok, ошибка
	// CachePut - в _. Поиск при мёртвом кэше выглядел полностью успешным:
	// Cached=false, Report.Note пуст, и вызывающий не знал, что каждый запрос
	// теперь идёт в движки заново.
	e, st := newEngine(t, &fixedSearcher{name: "s", res: stubResults()})
	killStore(t, st)

	out, err := e.Search(context.Background(), Options{Query: "dead cache", Mode: router.ModeFast, Limit: 20})
	if err != nil {
		t.Fatalf("мёртвый кэш не обязан ронять поиск: %v", err)
	}
	if out.Cached {
		t.Error("выдача помечена кэшированной при мёртвом кэше")
	}
	blob, mErr := json.Marshal(out)
	if mErr != nil {
		t.Fatal(mErr)
	}
	body := string(blob)
	if !strings.Contains(body, "cache_note") {
		t.Fatalf("в машинном исходе нет признака мёртвого кэша: %s", body)
	}
	// Одного наличия поля мало: мёртвая база ломает оба обращения к кэшу, и
	// оговорка обязана назвать оба. Проверка только ключа пропускала случай,
	// когда сообщение о чтении погашено, а о записи осталось.
	if !strings.Contains(out.CacheNote, "кэш не прочитан") {
		t.Errorf("в оговорке нет отказа чтения: %q", out.CacheNote)
	}
	if !strings.Contains(out.CacheNote, "запись в кэш не удалась") {
		t.Errorf("в оговорке нет отказа записи: %q", out.CacheNote)
	}
	if !strings.Contains(out.CacheNote, "; ") {
		t.Errorf("две оговорки не склеены: %q", out.CacheNote)
	}
}

func TestSearchLiveCacheHasNoCacheNote(t *testing.T) {
	// Обратная сторона: живой кэш не должен порождать оговорку, иначе она
	// обесценится и перестанет означать поломку.
	e, _ := newEngine(t, &fixedSearcher{name: "s", res: stubResults()})

	if _, err := e.Search(context.Background(), Options{Query: "healthy cache", Mode: router.ModeFast, Limit: 20}); err != nil {
		t.Fatalf("search: %v", err)
	}
	out, err := e.Search(context.Background(), Options{Query: "healthy cache", Mode: router.ModeFast, Limit: 20})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if !out.Cached {
		t.Fatal("второй запрос не взял выдачу из кэша")
	}
	blob, mErr := json.Marshal(out)
	if mErr != nil {
		t.Fatal(mErr)
	}
	if strings.Contains(string(blob), "cache_note") {
		t.Errorf("оговорка о кэше при живом кэше: %s", string(blob))
	}
}

func TestSearchStillServesResultsWithDeadCache(t *testing.T) {
	// Поломка кэша не отменяет выдачу: движки отработали, результаты на месте,
	// и отказ кэша не должен превращаться в отказ поиска.
	e, st := newEngine(t, &fixedSearcher{name: "s", res: []searchers.Result{
		{URL: "https://a.example/1", Title: "A"},
		{URL: "https://a.example/2", Title: "B"},
	}})
	killStore(t, st)

	out, err := e.Search(context.Background(), Options{Query: "dead cache results", Mode: router.ModeFast, Limit: 20})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(out.Results) != 2 {
		t.Errorf("результатов %d, ожидала 2", len(out.Results))
	}
	if out.Count != 2 {
		t.Errorf("count = %d, ожидала 2", out.Count)
	}
}
