package config

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// manualVars - переменные, читаемые кодом вне config.go, включая те, что
// спрятаны за константами: VOIDSEARCH_TOR_DIST живёт как EnvTorDist в
// internal/setup/tor.go и grep по литералу os.Getenv("VOIDSEARCH_") её
// не видит. Каждая запись несёт файл-носитель: тест проверяет, что
// литерал реально присутствует в этом файле, поэтому вымывание
// переменной из кода роняет инвариант, а не оставляет док врать.
var manualVars = map[string]string{
	"VOIDSEARCH_VERBOSE":       "../../cmd/voidsearchswag/main.go",
	"VOIDSEARCH_TOR_DIST":      "../setup/tor.go",
	"VOIDSEARCH_CONFIG":        "config.go",
	"VOIDSEARCH_OFFLINE_TESTS": "../../docs/testing.md",
}

// Инвариант справочника docs/env.md: каждая VOIDSEARCH_-переменная кода
// описана в справочнике, и каждая строка справочника существует в коде.
// Смысл инварианта - живой: до этапа 160 из 61 переменной 23 не были
// описаны нигде (22 без TOR_DIST, спрятанной за константой), и передача
// инстанса чужому админу означала чтение исходников вместо дока.
// Регресс в любую сторону - переменная добавлена в код без строки в
// справочнике (админ о ней не узнает) или строка осталась после
// вымывания переменной (док врёт) - роняет пакет здесь, а не в проде у
// того, кто настраивает по документации.
func TestEnvReferenceCoversAllConfigVars(t *testing.T) {
	src, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatalf("не прочитала config.go: %v", err)
	}
	docPath := filepath.Join("..", "..", "docs", "env.md")
	doc, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("не прочитала docs/env.md: %v", err)
	}

	varRe := regexp.MustCompile(`VOIDSEARCH_[A-Z0-9_]+`)
	known := map[string]string{}
	for _, v := range varRe.FindAllString(string(src), -1) {
		known[v] = "internal/config/config.go"
	}
	for name, holder := range manualVars {
		body, err := os.ReadFile(holder)
		if err != nil {
			t.Fatalf("не прочитала файл-носитель %s для %s: %v", holder, name, err)
		}
		if !strings.Contains(string(body), name) {
			t.Errorf("переменная %s приписана файлу %s, но литерала там нет: обнови manualVars", name, holder)
		}
		known[name] = holder
	}

	docVars := map[string]bool{}
	for _, v := range varRe.FindAllString(string(doc), -1) {
		docVars[v] = true
	}

	var missing []string
	for v := range known {
		if !docVars[v] {
			missing = append(missing, v)
		}
	}
	sort.Strings(missing)
	for _, v := range missing {
		t.Errorf("переменная %s (%s) не описана в docs/env.md", v, known[v])
	}

	var ghosts []string
	for v := range docVars {
		if _, ok := known[v]; !ok {
			ghosts = append(ghosts, v)
		}
	}
	sort.Strings(ghosts)
	for _, v := range ghosts {
		t.Errorf("docs/env.md описывает %s, но в коде переменной нет: док врёт", v)
	}

	if len(missing) == 0 && len(ghosts) == 0 {
		t.Logf("справочник покрывает все %d переменных кода", len(known))
	}
}
