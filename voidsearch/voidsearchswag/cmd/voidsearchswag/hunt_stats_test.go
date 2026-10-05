package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Охота и статистика не требуют сети на create/list/stats: движок для них
// не поднимается, поэтому тесты детерминированы.

func TestCmdHuntCreateAndList(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	t.Setenv("VOIDSEARCH_TOR", "off")
	t.Setenv("VOIDSEARCH_TRANSPORT", "direct")

	out := captureStdout(t, func() { cmdHunt([]string{"create", "leak database"}) })
	if !strings.Contains(out, "заведена") {
		t.Errorf("создание не подтверждено: %q", out)
	}
	out = captureStdout(t, func() { cmdHunt([]string{"list"}) })
	if !strings.Contains(out, "leak database") {
		t.Errorf("охота не в списке: %q", out)
	}
}

func TestCmdHuntListEmpty(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	t.Setenv("VOIDSEARCH_TOR", "off")

	out := captureStdout(t, func() { cmdHunt([]string{"list"}) })
	if !strings.Contains(out, "охот нет") {
		t.Errorf("пустой список не объяснён: %q", out)
	}
}

func TestCmdStatsEmpty(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	t.Setenv("VOIDSEARCH_TOR", "off")

	out := captureStdout(t, func() { cmdStats(nil) })
	if !strings.Contains(out, "пул:") {
		t.Errorf("сводка не напечатана: %q", out)
	}
	if !strings.Contains(out, "селекторы:") {
		t.Errorf("строки селекторов нет: %q", out)
	}
}

func TestCmdStatsJSON(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	out := captureStdout(t, func() { cmdStats([]string{"--json"}) })
	if !strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Errorf("JSON-сводка не объект: %q", out)
	}
	for _, key := range []string{`"onion_total"`, `"files_total"`, `"hunts_total"`, `"selectors_total"`} {
		if !strings.Contains(out, key) {
			t.Errorf("в сводке нет %s: %q", key, out)
		}
	}
}

func TestEngineFetchNil(t *testing.T) {
	if _, err := (engineFetch{}).FetchURL(context.Background(), "http://example.com/"); err == nil {
		t.Error("fetch без ядра принят")
	}
}

func TestEngineFetchBadURL(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	cfg := directConfig()
	st := openTestStore(t, dir)
	defer closeStore(st)
	eng, cleanup := buildEngine(cfg, stderrLogger{}, st)
	defer cleanup()

	if _, err := (engineFetch{eng}).FetchURL(context.Background(), "://мусор"); err == nil {
		t.Error("битый URL принят")
	}
}

func TestHuntSearchFreshEmptyQueryErrors(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	cfg := directConfig()
	st := openTestStore(t, dir)
	defer closeStore(st)
	eng, cleanup := buildEngine(cfg, stderrLogger{}, st)
	defer cleanup()

	// Пустой запрос отклоняется ядром до сети: заодно проверяется фолбэк
	// неверного режима на auto.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := huntSearchFresh(eng)(ctx, "  ", "не-режим", 5); err == nil {
		t.Error("пустой запрос принят")
	}
}

func TestPrintMCPSnippet(t *testing.T) {
	// Сниппет обязан содержать готовый JSON-блок с запуском через run:
	// его копируют в конфиг клиента без правок.
	out := captureStdout(t, func() { printMCPSnippet() })
	for _, want := range []string{`"mcpServers"`, `"voidsearchswag"`, `"run"`, `"command"`} {
		if !strings.Contains(out, want) {
			t.Errorf("в сниппете нет %s:\n%s", want, out)
		}
	}
}

func TestOpenStoreAndEngine(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	cfg := directConfig()
	st, eng, cleanup := openStoreAndEngine(cfg, stderrLogger{})
	defer cleanup()
	if st == nil || eng == nil {
		t.Fatal("стора или движок не собраны")
	}
	if eng.Store != st {
		t.Error("база не проброшена в движок")
	}
}
