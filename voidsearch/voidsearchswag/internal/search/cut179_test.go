package search

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"voidsearchswag/internal/metrics"
	"voidsearchswag/internal/router"
	"voidsearchswag/internal/searchers"
	"voidsearchswag/internal/store"
)

// switcherSearcher отдаёт первую выдачу в первом вызове, дальше вторую:
// имитация движка, чья выдача изменилась между прогонами (страница движка
// перелистнулась, даун-статус поднялся).
type switcherSearcher struct {
	name   string
	first  []searchers.Result
	second []searchers.Result
	calls  int
}

func (s *switcherSearcher) Name() string { return s.name }

func (s *switcherSearcher) Search(_ context.Context, _ string, _ int) ([]searchers.Result, error) {
	s.calls++
	if s.calls <= 1 {
		return s.first, nil
	}
	return s.second, nil
}

func noisyResults(n int) []searchers.Result {
	out := make([]searchers.Result, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, searchers.Result{
			Title:   fmt.Sprintf("витрина витрина %d", i),
			URL:     fmt.Sprintf("https://noise%d.example/%d", i, i),
			Snippet: "витрина витрина витрина",
			Source:  "s1",
		})
	}
	return out
}

func relevantResults(n int) []searchers.Result {
	out := make([]searchers.Result, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, searchers.Result{
			Title:   fmt.Sprintf("market shop %d", i),
			URL:     fmt.Sprintf("https://market%d.example/%d", i, i),
			Snippet: "market market market",
			Source:  "s2",
		})
	}
	return out
}

// TestLateCutKeepsSecondEngineResults - кэтчер дефекта этапа 179: срез по
// лимиту до реранка. Первый движок отдаёт 25 витринных результатов без
// единого токена запроса, второй - 19 релевантных. Прежний Dedupe(all,
// limit) забирал в выдачу первые 15 (все - движок №1), фильтр шума выкидывал
// их все, фильтр снимался и оператор получал витрину вместо выдачи. Правильный
// порядок: фильтр видит весь пул, срез получает отфильтрованный топ.
func TestLateCutKeepsSecondEngineResults(t *testing.T) {
	e, _ := newEngine(t,
		&fixedSearcher{name: "s1", res: noisyResults(25)},
		&fixedSearcher{name: "s2", res: relevantResults(19)},
	)
	out, err := e.Search(context.Background(), Options{
		Query: "market", Mode: router.ModeFast, Limit: 15, NoCache: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Count != 15 {
		t.Fatalf("count=%d, ожидала 15 (лимит выбран до конца)", out.Count)
	}
	for _, r := range out.Results {
		if r.Source != "s2" {
			t.Fatalf("в выдаче витрина движка s1: %+v", r)
		}
		if !strings.Contains(r.Title, "market") {
			t.Fatalf("нерелевантный результат прошёл фильтр: %+v", r)
		}
	}
	if !strings.Contains(out.Report.Note, "выкинуто 25 результатов") {
		t.Errorf("note не называет витрину: %q", out.Report.Note)
	}
}

// TestSearchCutAppliesAfterFiltersAndReportsBeyond - лимит режет выдачу
// последним шагом и докладывает в note, сколько релевантных результатов
// осталось за пределами топа: прежде срез проходил молча, и смоук-агент
// ловил выдачу короче лимита без единого пояснения.
func TestSearchCutAppliesAfterFiltersAndReportsBeyond(t *testing.T) {
	e, _ := newEngine(t, &fixedSearcher{name: "s2", res: relevantResults(40)})
	out, err := e.Search(context.Background(), Options{
		Query: "market", Mode: router.ModeFast, Limit: 15, NoCache: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Count != 15 {
		t.Fatalf("count=%d, ожидала 15", out.Count)
	}
	if len(out.Results) != 15 {
		t.Fatalf("len(Results)=%d, ожидала 15", len(out.Results))
	}
	for i, r := range out.Results {
		if r.Rank != i+1 {
			t.Errorf("rank=%d на позиции %d, ожидала плотный ряд 1..15", r.Rank, i)
		}
	}
	if !strings.Contains(out.Report.Note, "срез по лимиту: 25 результатов за пределами топ-15") {
		t.Errorf("note не называет потери за пределами лимита: %q", out.Report.Note)
	}
}

// TestNoCacheRefreshesStaleCacheEntry - кэтчер дефекта этапа 179: no_cache не
// обновлял устаревшую запись, и повтор без флага получал из кэша выдачу,
// которую свежий прогон уже опроверг. Флаг отключает чтение, а не запись.
func TestNoCacheRefreshesStaleCacheEntry(t *testing.T) {
	sw := &switcherSearcher{
		name:   "sw",
		first:  []searchers.Result{{Title: "market old", URL: "https://old.example/1", Source: "sw"}},
		second: []searchers.Result{{Title: "market new", URL: "https://new.example/1", Source: "sw"}},
	}
	e, _ := newEngine(t, sw)
	ctx := context.Background()

	// Первый вызов ложит в кэш старую выдачу.
	if _, err := e.Search(ctx, Options{Query: "market", Mode: router.ModeFast, Limit: 20}); err != nil {
		t.Fatal(err)
	}
	// Свежий прогон с no_cache должен не только обойти чтение, но и
	// обновить запись: выдача движка изменилась.
	fresh, err := e.Search(ctx, Options{Query: "market", Mode: router.ModeFast, Limit: 20, NoCache: true})
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Cached {
		t.Fatal("no_cache не должен читать кэш")
	}
	if fresh.Results[0].URL != "https://new.example/1" {
		t.Fatalf("свежий прогон отдал старьё: %+v", fresh.Results[0])
	}
	// Повтор без флага обязан получить из кэша обновлённую запись.
	cached, err := e.Search(ctx, Options{Query: "market", Mode: router.ModeFast, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if !cached.Cached {
		t.Fatal("повтор без no_cache должен прийти из кэша")
	}
	if cached.Results[0].URL != "https://new.example/1" {
		t.Fatalf("кэш не обновился свежим прогоном: %+v", cached.Results[0])
	}
}

// TestDeepLateCutKeepsSecondOnionEngine - та же детонация, но в deep-режиме:
// первый onion-движок отдаёт витрину без токенов, второй - релевантные.
// Срез по лимиту обязан пройти ПОСЛЕ фильтра шума, иначе движок №1
// монополизирует окно (BEFORE этапа 179: «market», tornet ok=19, torch
// ok=25, limit=15, выдача - 7, все tornet).
func TestDeepLateCutKeepsSecondOnionEngine(t *testing.T) {
	srv1 := newAhmiaStub(t, onionPage(20, "noise", "витрина"))
	srv2 := newAhmiaStub(t, onionPage(19, "market", "market"))
	eng1 := &searchers.OnionEngine{Name_: "eng1", Base: srv1, Client: testDirectClient(t)}
	eng2 := &searchers.OnionEngine{Name_: "eng2", Base: srv2, Client: testDirectClient(t)}
	e, _ := newEngine(t)
	e.Onion = &searchers.OnionCatalog{Engines: []*searchers.OnionEngine{eng1, eng2}}

	out, err := e.Search(context.Background(), Options{
		Query: "market", Mode: router.ModeDeep, Limit: 15, NoCache: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Count != 15 {
		t.Fatalf("count=%d, ожидала 15", out.Count)
	}
	for _, r := range out.Results {
		if r.Source != "eng2" {
			t.Fatalf("в выдаче витрина движка eng1: %+v", r)
		}
		if !strings.Contains(r.Title, "market") {
			t.Fatalf("нерелевантный результат прошёл фильтр: %+v", r)
		}
	}
}

func onionPage(n int, prefix, word string) string {
	var sb strings.Builder
	sb.WriteString("<html><body>")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&sb, `<a href="https://%s%d.example/%d">%s %d</a>`, prefix, i, i, word, i)
	}
	sb.WriteString("</body></html>")
	return sb.String()
}

// TestMetricsQueryMissCountsResults - счётчик search_query_miss_dropped
// считает выброшенные результаты, а не прогоны: смоук этапа 179 поймал 7 при
// минимум 8 отсечённых в одном запросе, потому что Inc отмечал сам факт
// фильтрации.
func TestMetricsQueryMissCountsResults(t *testing.T) {
	e, _ := newEngine(t,
		&fixedSearcher{name: "s1", res: noisyResults(3)},
		&fixedSearcher{name: "s2", res: relevantResults(2)},
	)
	m := metrics.New()
	e.Metrics = m
	if _, err := e.Search(context.Background(), Options{
		Query: "market", Mode: router.ModeFast, NoCache: true,
	}); err != nil {
		t.Fatal(err)
	}
	snap := m.Snapshot()
	if snap["search_query_miss_dropped"] != 3 {
		t.Errorf("search_query_miss_dropped=%d, ожидала 3 (по одному на выкинутый результат)", snap["search_query_miss_dropped"])
	}
}

// TestOutcomeNoteMirrorsReportNote - кэтчер дефекта этапа 179 (финальный
// смоук): канал потерь лежал в report.note, а слепой агент читает верхний
// уровень ответа - count, cached, duration - и не находит пояснений.
// Outcome.Note обязан зеркалить финальный Report.Note: срез по лимиту и
// токен-дропы видны без знания вложенности схемы.
func TestOutcomeNoteMirrorsReportNote(t *testing.T) {
	e, _ := newEngine(t, &fixedSearcher{name: "s2", res: relevantResults(40)})
	out, err := e.Search(context.Background(), Options{
		Query: "market", Mode: router.ModeFast, Limit: 15, NoCache: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Note == "" {
		t.Fatal("верхний note пуст: канал потерь невиден без знания схемы")
	}
	if out.Note != out.Report.Note {
		t.Fatalf("note=%q расходится с report.note=%q", out.Note, out.Report.Note)
	}
	if !strings.Contains(out.Note, "срез по лимиту") {
		t.Errorf("note не называет срез: %q", out.Note)
	}
}

// TestCachedOutcomeNoteFromLegacyRecord - кэш-хит обязан поднять зеркало из
// Report.Note даже для записи, сделанной до появления поля Note (записи со
// старой схемой в рабочей базе обязаны читаться с пояснениями).
func TestCachedOutcomeNoteFromLegacyRecord(t *testing.T) {
	e, st := newEngine(t, &fixedSearcher{name: "s2", res: relevantResults(40)})
	ctx := context.Background()
	// Запись старого формата: Note пуст, пояснение - только в Report.Note.
	legacy := &Outcome{
		Query: "market", Mode: router.ModeFast, UsedMode: router.ModeFast,
		Results: relevantResults(20), Count: 20, Limit: 20,
		Report: Report{Note: "срез по лимиту: 20 результатов за пределами топ-20"},
	}
	payload, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	key := store.CacheKey("market", string(router.ModeFast))
	if err := st.CachePut(ctx, key, string(router.ModeFast), string(payload), time.Hour); err != nil {
		t.Fatal(err)
	}
	out, err := e.Search(ctx, Options{Query: "market", Mode: router.ModeFast, Limit: 15})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Cached {
		t.Fatal("ожидала кэш-хит по legacy-записи")
	}
	if out.Note != legacy.Report.Note {
		t.Fatalf("кэш-хит не поднял зеркало из report.note: note=%q", out.Note)
	}
}
