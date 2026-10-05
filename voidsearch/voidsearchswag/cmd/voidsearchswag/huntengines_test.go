package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"voidsearchswag/internal/search"
	"voidsearchswag/internal/searchers"
)

// huntDeadSearcher - движок, который не отвечает. Так ведёт себя каждый движок
// при мёртвом tor или мёртвом прокси.
type huntDeadSearcher struct{ name string }

func (d huntDeadSearcher) Name() string { return d.name }

func (d huntDeadSearcher) Search(context.Context, string, int) ([]searchers.Result, error) {
	return nil, errors.New("соединение разорвано")
}

// huntLiveSearcher - движок, который отвечает выдачей.
type huntLiveSearcher struct {
	name string
	urls []string
}

func (l huntLiveSearcher) Name() string { return l.name }

func (l huntLiveSearcher) Search(context.Context, string, int) ([]searchers.Result, error) {
	res := make([]searchers.Result, 0, len(l.urls))
	for _, u := range l.urls {
		res = append(res, searchers.Result{URL: u, Title: u, Source: l.name})
	}
	return res, nil
}

func TestHuntSearchFreshReportsDeadEngines(t *testing.T) {
	// Адаптер поиска отдавал охоте только список URL и выбрасывал отчёт ядра. При
	// мёртвых движках список пуст и ошибка nil, поэтому охота записывала sha256
	// пустой строки базовым hash и выдавала находку с нулём URL.
	eng := &search.Engine{DDG: []searchers.Searcher{
		huntDeadSearcher{"ddg-html"},
		huntDeadSearcher{"ddg-lite"},
	}}
	out, err := huntSearchFresh(eng)(context.Background(), "leak database", "fast", 5)
	if err != nil {
		t.Fatalf("адаптер вернул ошибку вместо отчёта: %v", err)
	}
	if out.EnginesTotal != 2 {
		t.Errorf("EnginesTotal = %d, ожидала 2", out.EnginesTotal)
	}
	if out.EnginesFailed != 2 {
		t.Errorf("EnginesFailed = %d, ожидала 2", out.EnginesFailed)
	}
	if !out.EnginesDown() {
		t.Error("отказ всех движков не распознан: охота приняла бы пустую выдачу за факт")
	}
	if len(out.URLs) != 0 {
		t.Errorf("URL при мёртвых движках: %v", out.URLs)
	}
	if reason := out.Reason(); !strings.Contains(reason, "все движки поиска отказали (2 из 2)") {
		t.Errorf("Reason() = %q", reason)
	}
}

func TestHuntSearchFreshReportsPartialFailure(t *testing.T) {
	// Частичный отказ обязан остаться частичным: один движок из двух лёг, но
	// выдача есть, и охота должна работать дальше.
	eng := &search.Engine{DDG: []searchers.Searcher{
		huntLiveSearcher{"ddg-html", []string{"http://a.onion/x"}},
		huntDeadSearcher{"ddg-lite"},
	}}
	out, err := huntSearchFresh(eng)(context.Background(), "leak database", "fast", 5)
	if err != nil {
		t.Fatal(err)
	}
	if out.EnginesTotal != 2 || out.EnginesFailed != 1 {
		t.Errorf("движков %d, отказов %d, ожидала 2 и 1", out.EnginesTotal, out.EnginesFailed)
	}
	if out.EnginesDown() {
		t.Error("частичный отказ назван полным: охота остановилась бы при живом движке")
	}
	if len(out.URLs) != 1 || out.URLs[0] != "http://a.onion/x" {
		t.Errorf("выдача %v", out.URLs)
	}
}

// deadTransportEnv ломает транспорт так, чтобы каждый сетевой запрос отказал
// сразу: прокси задан, но слушателя на порту 1 нет.
func deadTransportEnv(t *testing.T) {
	t.Helper()
	t.Setenv("VOIDSEARCH_TRANSPORT", "static")
	t.Setenv("VOIDSEARCH_PROXIES", "127.0.0.1:1")
}

func createFastHunt(t *testing.T, query string) {
	t.Helper()
	out, code := runMain(t, "hunt", "create", "--mode", "fast", "--schedule", "60", query)
	if code != 0 {
		t.Fatalf("hunt create: код %d, вывод %q", code, firstN(out, 300))
	}
}

func TestCmdHuntRunReportsDeadEngines(t *testing.T) {
	// Сквозной прогон в условиях живого замера: transport=static через мёртвый
	// прокси. До правки вывод был «проверено 2, пропущено 0, находок 1» без
	// единого слова об отказе, а hunt list показывал hash e3b0c44298fc... -
	// sha256 пустой строки вместо настоящей базы.
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	deadTransportEnv(t)
	createFastHunt(t, "probe schedule zero")

	out, code := runMain(t, "hunt", "run", "--no-tor")
	if code != 0 {
		t.Fatalf("hunt run: код %d, вывод %q", code, firstN(out, 400))
	}
	if !strings.Contains(out, "отказов поиска: 1") {
		t.Errorf("в выводе нет счётчика отказов: %q", out)
	}
	if !strings.Contains(out, "все движки поиска отказали") {
		t.Errorf("в выводе нет причины отказа: %q", out)
	}
	if strings.Contains(out, "находок 1") {
		t.Errorf("прогон без поиска выдал находку: %q", out)
	}

	list, code := runMain(t, "hunt", "list")
	if code != 0 {
		t.Fatalf("hunt list: код %d", code)
	}
	if strings.Contains(list, "e3b0c44298fc") {
		t.Errorf("базовым hash записан sha256 пустой строки: %q", list)
	}
}

func TestCmdHuntRunByIDNamesReason(t *testing.T) {
	// Прогон конкретной охоты обязан вернуть причину и ненулевой код: молчаливый
	// Hit с changed=false оставил бы в базе затёртый hash.
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	deadTransportEnv(t)
	createFastHunt(t, "probe schedule zero")

	out, code := runMain(t, "hunt", "run", "--id", "1", "--no-tor")
	if code != 1 {
		t.Errorf("код возврата %d, ожидала 1: вывод %q", code, firstN(out, 300))
	}
	if !strings.Contains(out, "все движки поиска отказали") {
		t.Errorf("в выводе нет причины: %q", firstN(out, 300))
	}
}

func TestCmdHuntWatchCountsFailedSearches(t *testing.T) {
	// Ожидание по конкретной охоте при мёртвых движках: роль получила
	// failed=0 и note «охота не сломана по таймауту, она пуста», что уводило
	// править запрос вместо починки tor.
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	deadTransportEnv(t)
	createFastHunt(t, "probe schedule zero")

	out, code := runMain(t, "hunt", "watch", "--id", "1", "--timeout", "2s",
		"--interval", "1s", "--no-tor", "--json")
	if code != 0 {
		t.Fatalf("hunt watch: код %d, вывод %q", code, firstN(out, 400))
	}
	var rep map[string]any
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("вывод не JSON: %v (%q)", err, firstN(out, 300))
	}
	failed, _ := rep["failed"].(float64)
	if failed < 1 {
		t.Errorf("failed = %v, ожидала не меньше 1: %+v", rep["failed"], rep)
	}
	le, _ := rep["last_error"].(string)
	if !strings.Contains(le, "все движки поиска отказали") {
		t.Errorf("last_error = %q", le)
	}
	note, _ := rep["note"].(string)
	if strings.Contains(note, "не сломана") {
		t.Errorf("note утверждает, что охота не сломана: %q", note)
	}
	if !strings.Contains(note, "отказ") {
		t.Errorf("note не называет отказы: %q", note)
	}
	if rep["timeout"] != true {
		t.Errorf("ожидание не закончилось таймаутом: %+v", rep)
	}
}

// huntLog пишет строки лога в срез: ветка отказа движков в findFileHosts
// объясняет пустой список кандидатов именно через лог.
type huntLog struct{ lines []string }

func (l *huntLog) Infof(format string, args ...any) {
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *huntLog) Warnf(format string, args ...any) {
	l.Infof(format, args...)
}

func TestFindFileHostsExplainsDeadEngines(t *testing.T) {
	// Подбор хостов для обхода идёт через тот же адаптер. Без ветки отказа
	// пустой список выглядел как «кандидатов в выдаче нет», хотя на самом деле
	// спрашивать было некого, и лог не объяснял причину.
	eng := &search.Engine{DDG: []searchers.Searcher{
		huntDeadSearcher{"ddg-html"},
		huntDeadSearcher{"ddg-lite"},
	}}
	log := &huntLog{}
	hosts := findFileHosts(context.Background(), eng, log, 5, 5)
	if len(hosts) != 0 {
		t.Errorf("хосты из выдачи мёртвых движков: %v", hosts)
	}
	joined := strings.Join(log.lines, "\n")
	if !strings.Contains(joined, "все движки поиска отказали") {
		t.Errorf("в логе нет причины пустого списка: %q", joined)
	}
}
