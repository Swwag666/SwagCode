package main

import (
	"context"
	"strings"
	"testing"

	"voidsearchswag/internal/search"
)

func TestHuntSearchFreshRejectsUnknownMode(t *testing.T) {
	// Адаптер поиска подменял недопустимый режим на auto. Проверка стоит не только
	// в Create: охоты, заведённые до неё, уже лежат в базе с мусорным режимом,
	// и такой прогон обязан закончиться отказом с понятной причиной, а не
	// тихой работой в другом режиме.
	fn := huntSearchFresh(&search.Engine{})
	if _, err := fn(context.Background(), "leak database", "телепорт", 10); err == nil {
		t.Error("недопустимый режим принят: поиск пошёл бы в auto вместо отказа")
	} else if !strings.Contains(err.Error(), "телепорт") {
		t.Errorf("в ошибке не назван режим: %v", err)
	} else if !strings.Contains(err.Error(), "auto|fast|stealth|deep") {
		t.Errorf("в ошибке нет списка допустимых режимов: %v", err)
	}
}

func TestHuntCreateRejectsUnknownModeInCLI(t *testing.T) {
	// CLI принимал любое значение флага --mode, хотя справка перечисляет
	// auto|fast|stealth|deep. Охота создавалась, и дальше работала не в том
	// режиме, который заказал пользователь.
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	out, code := runMain(t, "hunt", "create", "--mode", "depp", "leak database")
	if code != 1 {
		t.Errorf("код возврата %d, ожидала 1: вывод %q", code, firstN(out, 200))
	}
	if !strings.Contains(out, "недопустимый режим") {
		t.Errorf("в выводе нет причины отказа: %q", firstN(out, 200))
	}
	if !strings.Contains(out, "depp") {
		t.Errorf("в выводе не названо отвергнутое значение: %q", firstN(out, 200))
	}

	// Охота не должна остаться в базе.
	list, code := runMain(t, "hunt", "list")
	if code != 0 {
		t.Fatalf("hunt list: код %d, вывод %q", code, firstN(list, 200))
	}
	if strings.Contains(list, "leak database") {
		t.Errorf("отвергнутая охота осталась в базе: %q", firstN(list, 200))
	}
}

func TestHuntCreateAcceptsModesInCLI(t *testing.T) {
	// Строгость не должна отрезать законные значения, включая синоним.
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	for _, mode := range []string{"auto", "fast", "stealth", "deep", "tor"} {
		out, code := runMain(t, "hunt", "create", "--mode", mode, "запрос "+mode)
		if code != 0 {
			t.Errorf("режим %s отвергнут: код %d, вывод %q", mode, code, firstN(out, 200))
		}
	}
	// tor приводится к deep, поэтому в списке каноническое значение.
	list, code := runMain(t, "hunt", "list")
	if code != 0 {
		t.Fatalf("hunt list: код %d", code)
	}
	if !strings.Contains(list, "запрос tor/deep") {
		t.Errorf("синоним tor не приведён к deep: %q", firstN(list, 400))
	}
}
