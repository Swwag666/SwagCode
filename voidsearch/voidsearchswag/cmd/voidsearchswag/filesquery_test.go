package main

import (
	"strings"
	"testing"
)

// Команда files печатает в каждой записи строку «страница: <адрес>», но
// отобрать по этому адресу не давала: -query сравнивался только с именем и URL
// файла. Имена в собранных каталогах машинные, так что для пользователя выдача
// выглядела противоречиво - поле показано, а найти по нему нельзя.
func TestCmdFilesFindsBySourcePage(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	st := openTestStore(t, dir)
	mustAddFile(t, st, "http://abc.onion/f/0a1b.bin", "0a1b.bin", "bin", 2048, "task-1", "http://abc.onion/leaks/db-dump/")
	mustAddFile(t, st, "http://abc.onion/f/photo.jpg", "photo.jpg", "jpg", 4096, "task-1", "http://abc.onion/gallery/")
	closeStore(st)

	out := captureStdout(t, func() { cmdFiles([]string{"--query", "leaks"}) })
	if !strings.Contains(out, "0a1b.bin") {
		t.Errorf("поиск по странице-источнику не нашёл файл: %q", out)
	}
	if strings.Contains(out, "photo.jpg") {
		t.Errorf("фильтр не отсёк запись с чужой страницей: %q", out)
	}
	if !strings.Contains(out, "показано 1") {
		t.Errorf("в выдаче нет счётчика отобранных записей: %q", out)
	}
}

// JSON-режим фильтрует тем же путём: параметр query в MCP-инструменте
// приходит в то же поле FileQuery.Text, и расхождение двух входов было бы
// невидимым до первого же спора о выдаче.
func TestCmdFilesJSONFindsBySourcePage(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	st := openTestStore(t, dir)
	mustAddFile(t, st, "http://abc.onion/f/0a1b.bin", "0a1b.bin", "bin", 2048, "task-1", "http://abc.onion/leaks/db-dump/")
	mustAddFile(t, st, "http://abc.onion/f/photo.jpg", "photo.jpg", "jpg", 4096, "task-1", "http://abc.onion/gallery/")
	closeStore(st)

	out := captureStdout(t, func() { cmdFiles([]string{"--query", "db-dump", "--json"}) })
	if !strings.Contains(out, "0a1b.bin") {
		t.Errorf("в JSON нет файла, найденного по странице: %q", out)
	}
	if strings.Contains(out, "photo.jpg") {
		t.Errorf("в JSON попала запись с чужой страницей: %q", out)
	}
}
