package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// Этап 166, жалоба смоук-агента: файлы без размера были в каталоге, но
// files --max-size их вырезал, а отдельного входа не существовало. Флаг
// --unknown-size обязан достать их и в текстовом, и в JSON-режиме, а пустой
// размерный фильтр - объяснить, куда записи делись.
func TestCmdFilesUnknownSizeFlag(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	st := openTestStore(t, dir)
	mustAddFile(t, st, "http://abc.onion/f/known.bin", "known.bin", "bin", 2048, "task-1", "http://abc.onion/leaks/")
	// Размер 0 - сервер не снял его с заголовков: единственный вход к записи.
	mustAddFile(t, st, "http://abc.onion/f/nosize1.bin", "nosize1.bin", "bin", 0, "task-1", "http://abc.onion/leaks/")
	mustAddFile(t, st, "http://abc.onion/f/nosize2.bin", "nosize2.bin", "bin", 0, "task-1", "http://abc.onion/leaks/")
	closeStore(st)

	out := captureStdout(t, func() { cmdFiles([]string{"--unknown-size"}) })
	if !strings.Contains(out, "nosize1.bin") || !strings.Contains(out, "nosize2.bin") {
		t.Errorf("--unknown-size не достал файлы без размера: %q", out)
	}
	if strings.Contains(out, "known.bin") {
		t.Errorf("в выдачу --unknown-size попал файл с известным размером: %q", out)
	}

	// Пустой размерный фильтр обязан объяснить пустоту, а не молчать:
	// агент видит 0 записей и не знает, что каталог не пуст.
	empty := captureStdout(t, func() { cmdFiles([]string{"--max-size", "100"}) })
	if !strings.Contains(empty, "неизвестного размера") {
		t.Errorf("пустой --max-size не объяснил пустоту: %q", empty)
	}
	if !strings.Contains(empty, "--unknown-size") {
		t.Errorf("подсказка не называет команду входа к записям: %q", empty)
	}

	var js struct {
		Files            []map[string]any `json:"files"`
		UnknownSizeFiles int              `json:"unknown_size_files"`
		Note             string           `json:"note"`
		UnknownSizeOnly  bool             `json:"unknown_size_only"`
	}
	raw := captureStdout(t, func() { cmdFiles([]string{"--unknown-size", "--json"}) })
	if err := json.Unmarshal([]byte(raw), &js); err != nil {
		t.Fatalf("JSON не парсится: %v\n%s", err, raw)
	}
	if len(js.Files) != 2 {
		t.Fatalf("JSON --unknown-size вернул %d файлов, хочу 2: %s", len(js.Files), raw)
	}
	if !js.UnknownSizeOnly {
		t.Errorf("JSON не пометил unknown_size_only: %s", raw)
	}
	rawEmpty := captureStdout(t, func() { cmdFiles([]string{"--max-size", "100", "--json"}) })
	var jsEmpty struct {
		Files            []map[string]any `json:"files"`
		UnknownSizeFiles int              `json:"unknown_size_files"`
		Note             string           `json:"note"`
	}
	if err := json.Unmarshal([]byte(rawEmpty), &jsEmpty); err != nil {
		t.Fatalf("JSON пустого фильтра не парсится: %v\n%s", err, rawEmpty)
	}
	if len(jsEmpty.Files) != 0 {
		t.Fatalf("JSON --max-size=100 вернул %d файлов, хочу 0", len(jsEmpty.Files))
	}
	if jsEmpty.UnknownSizeFiles != 2 {
		t.Errorf("JSON unknown_size_files=%d, хочу 2", jsEmpty.UnknownSizeFiles)
	}
	if jsEmpty.Note == "" {
		t.Errorf("JSON пустого фильтра без note: %s", rawEmpty)
	}
}
