package main

import (
	"encoding/json"
	"strings"
	"testing"

	"voidsearchswag/internal/catalog"
)

// Прогон без работы в машинном режиме отдаёт тот же отчёт, что и настоящий
// сбор: нули в счётчиках и причина в note. До правки collect --json на пустом
// пуле печатал в stdout русский текст при rc=0, и разбор потока падал.
func TestCollectEmptyPoolJSON(t *testing.T) {
	t.Setenv("VOIDSEARCH_DATA_DIR", t.TempDir())
	t.Setenv("VOIDSEARCH_TOR", "off")

	stdout, stderr, code := runMainSplit(t, "collect", "--json")
	if code != 0 {
		t.Fatalf("код возврата %d, хочу 0: пустой пул - не отказ (stderr %q)", code, firstN(stderr, 200))
	}
	var rep catalog.Report
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("stdout не разбирается как отчёт сбора: %v (%.200s)", err, stdout)
	}
	if !strings.Contains(rep.Note, "пул пуст") {
		t.Errorf("note = %q, хочу причину про пустой пул", rep.Note)
	}
	if rep.Elapsed != "0s" {
		t.Errorf("elapsed = %q, хочу 0s: обхода не было", rep.Elapsed)
	}
	for name, got := range map[string]int{
		"hosts": rep.Hosts, "pages": rep.Pages, "links": rep.Links,
		"saved": rep.Saved, "skipped": rep.Skipped, "failed": rep.Failed,
	} {
		if got != 0 {
			t.Errorf("%s = %d, хочу 0", name, got)
		}
	}
	// Схема обязана совпадать с настоящим отчётом: потребитель разбирает один
	// формат и не должен узнавать о пустом прогоне по отсутствующим полям.
	var raw map[string]any
	if err := json.Unmarshal([]byte(stdout), &raw); err != nil {
		t.Fatalf("stdout не JSON-объект: %v", err)
	}
	for _, key := range []string{"hosts", "pages", "links", "saved", "skipped", "failed", "elapsed", "note"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("в теле нет поля %s: %v", key, keysOf(raw))
		}
	}
}

// Текстовый режим сохраняет подсказку и не содержит JSON.
func TestCollectEmptyPoolTextKeeps(t *testing.T) {
	t.Setenv("VOIDSEARCH_DATA_DIR", t.TempDir())
	t.Setenv("VOIDSEARCH_TOR", "off")

	stdout, _, code := runMainSplit(t, "collect")
	if code != 0 {
		t.Fatalf("код возврата %d, хочу 0", code)
	}
	if strings.TrimSpace(stdout) != "пул пуст: сначала соберите адреса через discover" {
		t.Errorf("подсказка изменилась: %q", stdout)
	}
	if strings.Contains(stdout, "{") {
		t.Errorf("без --json появился JSON: %q", stdout)
	}
}

// Обе ветки пустого прогона - пустой пул и отсутствие файловых хостов - идут
// через одну функцию, поэтому формат у них общий.
func TestFinishEmptyCollectBothModes(t *testing.T) {
	text := captureStdout(t, func() { finishEmptyCollect(false, "пул пуст") })
	if strings.TrimSpace(text) != "пул пуст" {
		t.Errorf("текстовый режим: %q", text)
	}

	body := captureStdout(t, func() { finishEmptyCollect(true, "файловые хосты не найдены") })
	var rep catalog.Report
	if err := json.Unmarshal([]byte(body), &rep); err != nil {
		t.Fatalf("машинный режим не отдаёт отчёт: %v (%.200s)", err, body)
	}
	if rep.Note != "файловые хосты не найдены" {
		t.Errorf("note = %q", rep.Note)
	}
	if rep.Hosts != 0 || rep.Saved != 0 || rep.Elapsed != "0s" {
		t.Errorf("отчёт о несостоявшемся прогоне: %+v", rep)
	}
}

// Пустой отчёт не теряет ни одного поля настоящего: единственное добавление -
// note, и оно появляется только при пустом прогоне.
func TestCollectEmptyReportKeepsSchema(t *testing.T) {
	real, err := json.Marshal(catalog.Report{Hosts: 2, Pages: 3, Elapsed: "4s"})
	if err != nil {
		t.Fatalf("сериализация настоящего отчёта: %v", err)
	}
	empty, err := json.Marshal(collectEmptyReport("причина"))
	if err != nil {
		t.Fatalf("сериализация пустого отчёта: %v", err)
	}
	var a, b map[string]any
	if err := json.Unmarshal(real, &a); err != nil {
		t.Fatalf("настоящий отчёт не JSON: %v", err)
	}
	if err := json.Unmarshal(empty, &b); err != nil {
		t.Fatalf("пустой отчёт не JSON: %v", err)
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			t.Errorf("поле %s есть в настоящем отчёте и нет в пустом: %v", k, keysOf(b))
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok && k != "note" {
			t.Errorf("в пустом отчёте появилось лишнее поле %s", k)
		}
	}
	if _, ok := a["note"]; ok {
		t.Errorf("у настоящего отчёта появилось поле note: оно должно оставаться признаком несостоявшегося прогона, %v", keysOf(a))
	}
	if b["note"] != "причина" {
		t.Errorf("note = %v", b["note"])
	}
}
