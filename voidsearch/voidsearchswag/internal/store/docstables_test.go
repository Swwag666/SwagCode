package store

import (
	"context"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// readmePath - путь к README из каталога пакета.
const readmePath = "../../README.md"

// tableNamesRe достаёт список имён из предложения «N таблиц: `a`, `b`, ...».
var tableNamesRe = regexp.MustCompile("таблиц:([^.\n]+)\\.")

// docNameRe извлекает имена в обратных кавычках.
var docNameRe = regexp.MustCompile("`([a-z_][a-z0-9_]*)`")

// Список таблиц в README обязан совпадать с тем, что реально создаёт Migrate.
// Расхождение молчаливое: схема растёт миграциями, а абзац в разделе
// «Хранилище» переписывают руками. Замер до правки на HEAD 7b073ff: база после
// миграций содержит двенадцать таблиц, а README перечисляет восемь - без
// engine_health, engines, relevance и hunt_hits, то есть без всей подсистемы
// здоровья движков, релевантности и истории находок охоты.
func TestReadmeListsEveryTable(t *testing.T) {
	fact := liveTableNames(t)

	raw, err := os.ReadFile(readmePath)
	if err != nil {
		t.Fatalf("чтение %s: %v", readmePath, err)
	}
	m := tableNamesRe.FindStringSubmatch(string(raw))
	if m == nil {
		t.Fatalf("в %s нет предложения со списком таблиц", readmePath)
	}

	documented := make(map[string]bool)
	for _, name := range docNameRe.FindAllStringSubmatch(m[1], -1) {
		documented[name[1]] = true
	}
	if len(documented) == 0 {
		t.Fatalf("в %s список таблиц пуст", readmePath)
	}

	for _, name := range fact {
		if !documented[name] {
			t.Errorf("%s: таблица %s не перечислена в разделе «Хранилище»", readmePath, name)
		}
	}
	live := make(map[string]bool, len(fact))
	for _, name := range fact {
		live[name] = true
	}
	for name := range documented {
		if !live[name] {
			t.Errorf("%s: перечислена таблица %s, которой нет в базе после миграций", readmePath, name)
		}
	}
	if len(documented) != len(fact) {
		t.Errorf("%s обещает %d таблиц, база содержит %d", readmePath, len(documented), len(fact))
	}
}

// Число таблиц прописью в том же предложении обязано совпадать с фактом: список
// может быть полным, а счётное слово перед ним - устаревшим.
func TestReadmeTableCountWordMatches(t *testing.T) {
	fact := liveTableNames(t)

	raw, err := os.ReadFile(readmePath)
	if err != nil {
		t.Fatalf("чтение %s: %v", readmePath, err)
	}
	text := string(raw)

	// Берём предложение со списком вместе со словом перед ним.
	idx := strings.Index(text, "таблиц:")
	if idx < 0 {
		t.Fatalf("в %s нет предложения со списком таблиц", readmePath)
	}
	start := strings.LastIndex(text[:idx], "\n")
	head := strings.TrimSpace(text[start:idx])

	want := map[int]string{
		8: "Восемь", 9: "Девять", 10: "Десять", 11: "Одиннадцать",
		12: "Двенадцать", 13: "Тринадцать", 14: "Четырнадцать", 15: "Пятнадцать",
	}
	word, ok := want[len(fact)]
	if !ok {
		t.Fatalf("для %d таблиц нет ожидаемого слова в тесте", len(fact))
	}
	if head != word {
		t.Errorf("в %s написано %q, а база содержит %d таблиц: ждала %q", readmePath, head, len(fact), word)
	}
}

// liveTableNames возвращает имена таблиц базы после миграций, отсортированные.
func liveTableNames(t *testing.T) []string {
	t.Helper()
	st := newStore(t)
	ctx := context.Background()

	rows, err := st.db.QueryContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatalf("чтение sqlite_master: %v", err)
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("обход sqlite_master: %v", err)
	}
	if len(names) == 0 {
		t.Fatal("после миграций в базе нет ни одной таблицы")
	}
	sort.Strings(names)
	return names
}
