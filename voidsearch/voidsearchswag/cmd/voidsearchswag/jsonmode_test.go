package main

import (
	"encoding/json"
	"io"
	"os"
	"sort"
	"strings"
	"testing"
)

// decodeJSON разбирает тело ответа и роняет тест, если поток не JSON.
func decodeJSON(t *testing.T, body string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("stdout не разбирается как JSON: %v (%.200s)", err, body)
	}
	return out
}

// keysOf возвращает отсортированные имена полей объекта: порядок обхода map в Go
// случайный, а сообщение теста обязано быть воспроизводимым.
func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Пустой каталог в машинном режиме отдаёт JSON, а не человеческую подсказку. До
// правки cmdFiles печатала «каталог пуст» раньше проверки --json, и поток
// оказывался неразбираемым при rc=0 (замер: 132 байта русского текста).
func TestFilesEmptyCatalogJSON(t *testing.T) {
	t.Setenv("VOIDSEARCH_DATA_DIR", t.TempDir())
	stdout, stderr, code := runMainSplit(t, "files", "--json")
	if code != 0 {
		t.Errorf("код возврата %d, хочу 0: пустой каталог - не отказ (stderr %q)", code, firstN(stderr, 200))
	}
	if strings.Contains(stdout, "каталог пуст") {
		t.Errorf("в машинный вывод попала человеческая подсказка: %.200s", stdout)
	}
	body := decodeJSON(t, stdout)
	for _, key := range []string{"returned", "catalog_total", "catalog_bytes", "by_ext", "files"} {
		if _, ok := body[key]; !ok {
			t.Errorf("в теле нет поля %s: %v", key, keysOf(body))
		}
	}
	if n, _ := body["catalog_total"].(float64); n != 0 {
		t.Errorf("catalog_total = %v, хочу 0", body["catalog_total"])
	}
	if n, _ := body["returned"].(float64); n != 0 {
		t.Errorf("returned = %v, хочу 0", body["returned"])
	}
}

// Схема ответа не зависит от наполненности каталога: машина разбирает один и тот
// же набор полей и на свежей базе, и на заполненной.
func TestFilesCatalogJSONSchemaIsStable(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	empty, _, code := runMainSplit(t, "files", "--json")
	if code != 0 {
		t.Fatalf("пустой каталог: код %d", code)
	}
	seedWideCatalog(t, dir, 3)
	full, _, code := runMainSplit(t, "files", "--json")
	if code != 0 {
		t.Fatalf("заполненный каталог: код %d", code)
	}
	a, b := decodeJSON(t, empty), decodeJSON(t, full)
	if len(a) != len(b) {
		t.Errorf("набор полей разошёлся: пустой %v, заполненный %v", keysOf(a), keysOf(b))
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			t.Errorf("поле %s есть в ответе на пустом каталоге и нет на заполненном", k)
		}
	}
	if n, _ := b["catalog_total"].(float64); n != 3 {
		t.Errorf("catalog_total = %v, хочу 3", b["catalog_total"])
	}
	if n, _ := b["returned"].(float64); n != 3 {
		t.Errorf("returned = %v, хочу 3", b["returned"])
	}
}

// Версия в машинном виде: скрипт, собирающий диагностику со всех команд, до
// правки получал здесь текст «voidsearchswag 0.1.0» и падал на разборе.
// С этапа 158 тело несёт и мета сборки (commit, date): скрипты читают по
// ключам, расширение полей их не ломает, а диагностика с прода без
// «какая это сборка» была слепой.
func TestVersionJSON(t *testing.T) {
	stdout, stderr, code := runMainSplit(t, "version", "--json")
	if code != 0 {
		t.Errorf("код возврата %d, хочу 0 (stderr %q)", code, firstN(stderr, 200))
	}
	body := decodeJSON(t, stdout)
	if body["program"] != "voidsearchswag" {
		t.Errorf("program = %v", body["program"])
	}
	if body["version"] != version {
		t.Errorf("version = %v, хочу %s", body["version"], version)
	}
	if body["commit"] != buildCommit {
		t.Errorf("commit = %v, хочу %s", body["commit"], buildCommit)
	}
	if body["date"] != buildDate {
		t.Errorf("date = %v, хочу %s", body["date"], buildDate)
	}
	if len(body) != 4 {
		t.Errorf("в теле %d полей, хочу program/version/commit/date: %v", len(body), keysOf(body))
	}
}

// Залитая линкером мета печатается в текстовом выводе, а локальная сборка
// (unknown) не приписывает себе чужой коммит. Прямой вызов с подменой
// stdout: подпроцесс не увидит присвоенные в тесте переменные, а мета
// обязана читаться именно из них.
func TestVersionTextCarriesBuildMeta(t *testing.T) {
	savedC, savedD := buildCommit, buildDate
	defer func() { buildCommit, buildDate = savedC, savedD }()

	buildCommit, buildDate = "80d6e00", "2026-10-01T13:54:00Z"
	out := captureVersionStdout(t, nil)
	want := "voidsearchswag " + version + " (80d6e00, 2026-10-01T13:54:00Z)\n"
	if out != want {
		t.Errorf("вывод %q, хочу %q", out, want)
	}

	buildCommit, buildDate = "unknown", "unknown"
	out = captureVersionStdout(t, nil)
	if strings.Contains(out, "unknown") {
		t.Errorf("локальная сборка тащит мету unknown в вывод: %q", out)
	}
}

// captureVersionStdout зовёт cmdVersion в текущем процессе и снимает stdout.
func captureVersionStdout(t *testing.T, rest []string) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = w
	cmdVersion(rest)
	os.Stdout = saved
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Текстовая версия и короткие формы сохраняются: JSON появляется только там, где
// его попросили.
func TestVersionTextKeeps(t *testing.T) {
	for _, arg := range []string{"version", "--version", "-v"} {
		stdout, _, code := runMainSplit(t, arg)
		if code != 0 {
			t.Errorf("%s: код %d, хочу 0", arg, code)
		}
		if !strings.Contains(stdout, "voidsearchswag "+version) {
			t.Errorf("%s: вывод %q", arg, stdout)
		}
		if strings.Contains(stdout, "{") {
			t.Errorf("%s: без --json появился JSON: %q", arg, stdout)
		}
	}
}

// Создание охоты в машинном виде отдаёт id: до правки --json у create не
// объявлялся, и FlagSet с ExitOnError завершал процесс кодом 2 - команда, у
// которой попросили машинный ответ, не выполнялась вообще.
func TestHuntCreateJSON(t *testing.T) {
	t.Setenv("VOIDSEARCH_DATA_DIR", t.TempDir())
	stdout, stderr, code := runMainSplit(t, "hunt", "create", "--json",
		"--mode", "fast", "--schedule", "90", "leak", "database")
	if code != 0 {
		t.Fatalf("код возврата %d, stderr %q", code, firstN(stderr, 200))
	}
	body := decodeJSON(t, stdout)
	if id, _ := body["id"].(float64); id <= 0 {
		t.Errorf("id = %v, хочу положительный", body["id"])
	}
	if body["query"] != "leak database" {
		t.Errorf("query = %v, хочу leak database", body["query"])
	}
	if body["mode"] != "fast" {
		t.Errorf("mode = %v, хочу fast", body["mode"])
	}
	if n, _ := body["schedule_min"].(float64); n != 90 {
		t.Errorf("schedule_min = %v, хочу 90", body["schedule_min"])
	}
}

func TestHuntCreateTextKeeps(t *testing.T) {
	t.Setenv("VOIDSEARCH_DATA_DIR", t.TempDir())
	stdout, stderr, code := runMainSplit(t, "hunt", "create", "--mode", "fast", "leak", "database")
	if code != 0 {
		t.Fatalf("код возврата %d, stderr %q", code, firstN(stderr, 200))
	}
	if !strings.Contains(stdout, "охота ") || !strings.Contains(stdout, "заведена") {
		t.Errorf("текстовый ответ изменился: %q", stdout)
	}
	if strings.Contains(stdout, "{") {
		t.Errorf("без --json появился JSON: %q", stdout)
	}
}

// Список охот в машинном виде несёт те же данные, что печатает текст, а у
// небегавшей охоты last_run равен null: нулевое время сериализовалось бы как
// 0001-01-01T00:00:00Z и читалось бы как дата, а не как «не бегала».
func TestHuntListJSON(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	created, stderr, code := runMainSplit(t, "hunt", "create", "--json",
		"--mode", "fast", "--schedule", "90", "leak", "database")
	if code != 0 {
		t.Fatalf("создание: код %d, stderr %q", code, firstN(stderr, 200))
	}
	wantID, _ := decodeJSON(t, created)["id"].(float64)

	stdout, stderr, code := runMainSplit(t, "hunt", "list", "--json")
	if code != 0 {
		t.Fatalf("список: код %d, stderr %q", code, firstN(stderr, 200))
	}
	var list struct {
		Count int `json:"count"`
		Hunts []struct {
			ID          float64 `json:"id"`
			Query       string  `json:"query"`
			Mode        string  `json:"mode"`
			ScheduleMin int     `json:"schedule_min"`
			LastHash    string  `json:"last_hash"`
			LastRun     *string `json:"last_run"`
		} `json:"hunts"`
	}
	if err := json.Unmarshal([]byte(stdout), &list); err != nil {
		t.Fatalf("список не JSON: %v (%.200s)", err, stdout)
	}
	if list.Count != 1 || len(list.Hunts) != 1 {
		t.Fatalf("count=%d hunts=%d, хочу 1 и 1: %.200s", list.Count, len(list.Hunts), stdout)
	}
	h := list.Hunts[0]
	if h.ID != wantID {
		t.Errorf("id в списке %v, созданная охота %v", h.ID, wantID)
	}
	if h.Query != "leak database" || h.Mode != "fast" || h.ScheduleMin != 90 {
		t.Errorf("поля охоты разошлись с заданными: %+v", h)
	}
	if h.LastRun != nil {
		t.Errorf("last_run у небегавшей охоты = %s, хочу null", *h.LastRun)
	}
	if h.LastHash != "" {
		t.Errorf("last_hash у небегавшей охоты = %q, хочу пустую строку", h.LastHash)
	}
}

// Пустой список отдаётся массивом, а не null: машина отличает «охот нет»
// (count 0) от «список не читается» (отказ с кодом 1).
func TestHuntListEmptyJSON(t *testing.T) {
	t.Setenv("VOIDSEARCH_DATA_DIR", t.TempDir())
	stdout, stderr, code := runMainSplit(t, "hunt", "list", "--json")
	if code != 0 {
		t.Fatalf("код возврата %d, stderr %q", code, firstN(stderr, 200))
	}
	body := decodeJSON(t, stdout)
	if n, _ := body["count"].(float64); n != 0 {
		t.Errorf("count = %v, хочу 0", body["count"])
	}
	if body["hunts"] == nil {
		t.Errorf("hunts = null, хочу пустой массив: %.160s", stdout)
	}
}

// Текстовый список и подсказка на пустой базе сохраняются.
func TestHuntListTextKeeps(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	if _, stderr, code := runMainSplit(t, "hunt", "create", "--mode", "deep", "leak", "database"); code != 0 {
		t.Fatalf("создание: код %d, stderr %q", code, firstN(stderr, 200))
	}
	stdout, _, code := runMainSplit(t, "hunt", "list")
	if code != 0 {
		t.Fatalf("список: код %d", code)
	}
	if !strings.Contains(stdout, "[leak database/deep]") {
		t.Errorf("текстовый список изменился: %q", stdout)
	}
	if strings.Contains(stdout, "schedule_min") {
		t.Errorf("без --json в списке появился JSON: %q", stdout)
	}

	t.Setenv("VOIDSEARCH_DATA_DIR", t.TempDir())
	empty, _, code := runMainSplit(t, "hunt", "list")
	if code != 0 {
		t.Fatalf("пустой список: код %d", code)
	}
	if !strings.Contains(empty, "охот нет") {
		t.Errorf("подсказка на пустой базе пропала: %q", empty)
	}
}
