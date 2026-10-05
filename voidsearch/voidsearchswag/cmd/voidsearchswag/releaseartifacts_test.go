package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Makefile и ci.yml - боевые артефакты сборки, и их свойства обязаны
// держаться против правки «на скорую руку»: релиз без залитой версии
// неотличим от самосбора, а «версия 0.1.0» на проде из чужого коммита -
// это ложь в диагностике. Замер этапа 158 ДО: бинарь 627107d печатал
// «voidsearchswag 0.1.0» независимо от коммита, даты и ветки.
func TestMakefileStampsVersionIntoBinary(t *testing.T) {
	mk := readRepoFile(t, "Makefile")

	// Ключи -X живут в определении LDFLAGS (шапка Makefile): одно
	// определение, общее для всех целей.
	if !strings.Contains(mk, "LDFLAGS := -X main.version=$(VERSION) -X main.buildCommit=$(COMMIT) -X main.buildDate=$(DATE)") {
		t.Error("LDFLAGS не заливает все три ключа версии: релиз уйдёт без штампа")
	}

	// build и cross обязаны собирать с -ldflags "$(LDFLAGS)".
	for _, target := range []string{"build:", "cross:"} {
		block, err := extractMakeTarget(mk, target)
		if err != nil {
			t.Fatalf("цель %s не найдена: %v", target, err)
		}
		if !strings.Contains(block, `-ldflags "$(LDFLAGS)"`) {
			t.Errorf("цель %s собирается без залитой версии", target)
		}
	}

	// VERSION по умолчанию берётся из git describe с честным суффиксом
	// dirty, а не из захардкоженной константы.
	if !strings.Contains(mk, "VERSION ?= $(shell git describe --tags --always --dirty") {
		t.Error("VERSION не берётся из git describe: теги перестанут попадать в версию")
	}

	// release гоняет тесты до сборки и считает суммы бинарей.
	rel, err := extractMakeTarget(mk, "release:")
	if err != nil {
		t.Fatalf("цель release не найдена: %v", err)
	}
	if !strings.Contains(rel, "test cross") && !strings.Contains(rel, "cross") {
		t.Error("release не зависит от тестов и cross: релиз собирается без проверки")
	}
	if !strings.Contains(rel, "sha256sum") {
		t.Error("release не считает суммы бинарей: подмена релиза неотслежуема")
	}
}

func TestCIStampsAndUploadsRelease(t *testing.T) {
	ci := readRepoFile(t, filepath.Join(".github", "workflows", "ci.yml"))

	for _, key := range []string{"-X main.version=", "-X main.buildCommit=", "-X main.buildDate="} {
		if !strings.Contains(ci, key) {
			t.Errorf("CI не заливает %q в кросс-сборку", key)
		}
	}
	// Штамп проверяется исполнением бинаря, а не глазомером.
	if !strings.Contains(ci, "name: verify version stamp on release binary") {
		t.Error("CI не проверяет штамп версии исполнением бинаря")
	}
	if !strings.Contains(ci, "sha256sum dist/") {
		t.Error("CI не считает суммы релизных бинарей")
	}
	if !strings.Contains(ci, "name: binaries") {
		t.Error("CI не выкладывает релизные бинари артефактом")
	}
}

// Локальная сборка честно называет себя dev-сборкой: пользователь, у
// которого «ничего не работает», обязан с первого взгляда отличать
// самосбор от релиза.
func TestDefaultVersionIsDevBuild(t *testing.T) {
	if !strings.HasSuffix(version, "-dev") {
		t.Errorf("незалитая версия %q не помечена как dev-сборка", version)
	}
}

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatalf("прочитать %s: %v", rel, err)
	}
	return string(b)
}

// extractMakeTarget вырезает блок цели (включая строку с зависимостями)
// до следующей цели или конца файла.
func extractMakeTarget(makefile, name string) (string, error) {
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `.*$`)
	loc := re.FindStringIndex(makefile)
	if loc == nil {
		return "", os.ErrNotExist
	}
	rest := makefile[loc[0]:]
	next := regexp.MustCompile(`(?m)^[a-zA-Z_-]+:`).FindStringIndex(rest[len(name):])
	if next != nil {
		rest = rest[:len(name)+next[0]]
	}
	return rest, nil
}
