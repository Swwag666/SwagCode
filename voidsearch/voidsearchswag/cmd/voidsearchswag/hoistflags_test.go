package main

import (
	"flag"
	"reflect"
	"testing"
)

// searchFlagSet повторяет набор флагов cmdSearch, чтобы тест проверял реальную
// конфигурацию, а не выдуманную.
func searchFlagSet() *flag.FlagSet {
	fs := flag.NewFlagSet("search", flag.ContinueOnError)
	fs.String("mode", "auto", "")
	fs.Int("limit", 20, "")
	fs.Bool("no-cache", false, "")
	fs.Bool("json", false, "")
	fs.Bool("no-tor", false, "")
	return fs
}

func TestHoistFlagsMovesFlagAfterQuery(t *testing.T) {
	// Пакет flag прекращает разбор на первом аргументе без дефиса, поэтому
	// `search -mode fast "запрос" -json` молча искал строку «запрос -json» и
	// печатал текст вместо JSON. Оба независимых пользовательских теста
	// споткнулись именно здесь.
	fs := searchFlagSet()
	got := hoistFlags(fs, []string{"-mode", "fast", "квантовые компьютеры", "-json"})
	want := []string{"-mode", "fast", "-json", "квантовые компьютеры"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("получено %v, ожидала %v", got, want)
	}
	if err := fs.Parse(got); err != nil {
		t.Fatal(err)
	}
	if q := fs.Args(); len(q) != 1 || q[0] != "квантовые компьютеры" {
		t.Errorf("запрос после разбора: %v", q)
	}
	if v := fs.Lookup("json").Value.String(); v != "true" {
		t.Errorf("-json не применился: %s", v)
	}
	if v := fs.Lookup("mode").Value.String(); v != "fast" {
		t.Errorf("-mode не применился: %s", v)
	}
}

func TestHoistFlagsCarriesValueForNonBool(t *testing.T) {
	// Небулев флаг забирает следующий токен как значение. Перенести имя без
	// значения значит сломать и флаг, и запрос.
	fs := searchFlagSet()
	got := hoistFlags(fs, []string{"квантовые компьютеры", "-limit", "5"})
	want := []string{"-limit", "5", "квантовые компьютеры"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("получено %v, ожидала %v", got, want)
	}
	if err := fs.Parse(got); err != nil {
		t.Fatal(err)
	}
	if v := fs.Lookup("limit").Value.String(); v != "5" {
		t.Errorf("-limit не применился: %s", v)
	}
	if q := fs.Args(); len(q) != 1 || q[0] != "квантовые компьютеры" {
		t.Errorf("запрос после разбора: %v", q)
	}
}

func TestHoistFlagsKeepsEqualsForm(t *testing.T) {
	fs := searchFlagSet()
	got := hoistFlags(fs, []string{"запрос", "--limit=7", "--mode=deep"})
	want := []string{"-limit=7", "-mode=deep", "запрос"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("получено %v, ожидала %v", got, want)
	}
	if err := fs.Parse(got); err != nil {
		t.Fatal(err)
	}
	if v := fs.Lookup("limit").Value.String(); v != "7" {
		t.Errorf("--limit=7 не применился: %s", v)
	}
	if v := fs.Lookup("mode").Value.String(); v != "deep" {
		t.Errorf("--mode=deep не применился: %s", v)
	}
}

func TestHoistFlagsLeavesQueryWordsAlone(t *testing.T) {
	// Переносятся только токены, совпавшие с именем известного флага. Часть
	// запроса, начинающаяся с дефиса, обязана остаться на месте, иначе
	// исправление порядка аргументов само исказило бы запрос.
	fs := searchFlagSet()
	args := []string{"-mode", "fast", "-5 градусов мороза", "и ещё -что-то"}
	got := hoistFlags(fs, args)
	if !reflect.DeepEqual(got, args) {
		t.Errorf("порядок изменился: %v, ожидала %v", got, args)
	}
}

func TestHoistFlagsLeavesUnknownFlagsForFlagPackage(t *testing.T) {
	// Неизвестный флаг не переносим. package flag до него не добирается -
	// разбор уже остановлен на позиционном аргументе, - поэтому токен остаётся
	// частью запроса, и это надо не молча глотать, а предупреждать.
	fs := searchFlagSet()
	got := hoistFlags(fs, []string{"запрос", "-turbo"})
	if !reflect.DeepEqual(got, []string{"запрос", "-turbo"}) {
		t.Errorf("получено %v", got)
	}
	if err := fs.Parse(got); err != nil {
		t.Fatalf("разбор упал: %v", err)
	}
	if q := fs.Args(); len(q) != 2 || q[1] != "-turbo" {
		t.Errorf("неизвестный токен не остался в запросе: %v", q)
	}
	if stray := strayFlags(fs.Args()); !reflect.DeepEqual(stray, []string{"-turbo"}) {
		t.Errorf("strayFlags вернул %v, ожидала [-turbo]", stray)
	}
}

func TestStrayFlagsIgnoresSeparatorsAndWords(t *testing.T) {
	// Одиночный дефис и разделитель «--» флагами не считаются: это
	// общепринятые маркеры, и предупреждение о них было бы шумом.
	if got := strayFlags([]string{"-", "--", "текст", "-5", "градусов"}); len(got) != 1 || got[0] != "-5" {
		t.Errorf("получено %v, ожидала только [-5]", got)
	}
	if got := strayFlags([]string{"обычный запрос без дефисов"}); len(got) != 0 {
		t.Errorf("чистый запрос дал предупреждение: %v", got)
	}
	if got := strayFlags(nil); len(got) != 0 {
		t.Errorf("пустой ввод дал %v", got)
	}
}

func TestWarnStrayFlagsDoesNotFireOnKnownFlags(t *testing.T) {
	// После hoistFlags известных флагов среди позиционных аргументов быть не
	// должно, иначе предупреждение сработает на каждый обычный запуск.
	fs := searchFlagSet()
	if err := fs.Parse(hoistFlags(fs, []string{"-mode", "fast", "запрос", "-json"})); err != nil {
		t.Fatal(err)
	}
	if n := warnStrayFlags(fs, []string{"-mode", "fast", "запрос", "-json"}); n != 0 {
		t.Errorf("предупреждение сработало на корректном запуске: %d токенов", n)
	}
}

func TestHoistFlagsNoopWhenAlreadyOrdered(t *testing.T) {
	fs := searchFlagSet()
	args := []string{"-mode", "fast", "-json", "-limit", "5", "квантовые компьютеры"}
	if got := hoistFlags(fs, args); !reflect.DeepEqual(got, args) {
		t.Errorf("порядок изменился без причины: %v", got)
	}
}

func TestHoistFlagsEmptyAndEdgeTokens(t *testing.T) {
	fs := searchFlagSet()
	if got := hoistFlags(fs, nil); len(got) != 0 {
		t.Errorf("пустой ввод дал %v", got)
	}
	// Одиночный дефис и двойной - это общепринятые разделители, а не флаги.
	args := []string{"-", "--", "текст"}
	if got := hoistFlags(fs, args); !reflect.DeepEqual(got, args) {
		t.Errorf("разделители изменены: %v", got)
	}
	// Небулев флаг в самом конце без значения: не паникуем, пусть flag решит.
	got := hoistFlags(fs, []string{"запрос", "-limit"})
	if err := fs.Parse(got); err == nil {
		t.Error("пропущенное значение флага не вызвало ошибку")
	}
}

func TestHoistFlagsPreservesMultiWordQuery(t *testing.T) {
	// Запрос из нескольких слов собирается через strings.Join(fs.Args(), " ").
	// Перенос флагов не должен ни потерять слово, ни переставить их местами.
	fs := searchFlagSet()
	got := hoistFlags(fs, []string{"-mode", "fast", "квантовые", "компьютеры", "с", "чего", "начать", "-json"})
	if err := fs.Parse(got); err != nil {
		t.Fatal(err)
	}
	q := fs.Args()
	want := []string{"квантовые", "компьютеры", "с", "чего", "начать"}
	if !reflect.DeepEqual(q, want) {
		t.Errorf("запрос после разбора %v, ожидала %v", q, want)
	}
}
