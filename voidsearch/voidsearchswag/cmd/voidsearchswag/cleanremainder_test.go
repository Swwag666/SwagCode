package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"voidsearchswag/internal/store"
)

func remainderStore(t *testing.T, files int) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/clean.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < files; i++ {
		name := string(rune('a'+i)) + ".epub"
		e := store.FileEntry{
			URL:      "http://abcdefghijklmnop.onion/" + name,
			Filename: name,
			Ext:      "epub",
			Size:     1024,
			Verdict:  "ebook",
		}
		if err := st.AddFile(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

func TestCleanRemainderDryRunIsForecast(t *testing.T) {
	// Пробный прогон ничего не удаляет, поэтому остаток обязан быть прогнозом,
	// а фактический счётчик не читается вовсе: в базе три файла, а прогноз
	// говорит два.
	st := remainderStore(t, 3)
	ctx := context.Background()

	after, exact, problems := cleanRemainder(ctx, st, false, 3, 1)
	if after != 2 {
		t.Errorf("after = %d, ожидала прогноз 2", after)
	}
	if exact {
		t.Error("прогноз помечен как фактический подсчёт")
	}
	if len(problems) != 0 {
		t.Errorf("в пробном прогоне проблемы: %v", problems)
	}
}

func TestCleanRemainderApplyUsesRealCount(t *testing.T) {
	// После --apply остаток берётся фактическим счётчиком, даже когда вычитание
	// даёт другое число: в базе два файла, а filesBefore минус filesRemoved
	// утверждает четыре. Расхождение здесь не случайное - именно так выглядит
	// чистка, которая часть строк не удалила.
	st := remainderStore(t, 2)
	ctx := context.Background()

	after, exact, problems := cleanRemainder(ctx, st, true, 5, 1)
	if after != 2 {
		t.Errorf("after = %d, ожидала фактические 2", after)
	}
	if !exact {
		t.Error("фактический подсчёт не помечен как точный")
	}
	if len(problems) != 0 {
		t.Errorf("проблемы при читаемом счётчике: %v", problems)
	}
}

func TestCleanRemainderReportsUnreadableCount(t *testing.T) {
	// Главная проверка: ошибка счётчика после --apply не должна оставлять в
	// отчёте прогноз без пометки. До правки код делал именно это - ветка
	// `if err == nil` подставляла факт, а иначе молча оставляла вычитание, хотя
	// комментарий над ней обещал фактический счётчик.
	st, err := store.Open(t.TempDir() + "/closed.db")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	after, exact, problems := cleanRemainder(ctx, st, true, 5, 1)
	if after != 4 {
		t.Errorf("after = %d, ожидала прогноз 4 как запасное значение", after)
	}
	if exact {
		t.Error("прогноз помечен как фактический подсчёт при непрочитанном счётчике")
	}
	if len(problems) != 1 {
		t.Fatalf("проблем %d, ожидала одну: %v", len(problems), problems)
	}
	if !strings.Contains(problems[0], "остаток каталога не прочитан") {
		t.Errorf("проблема не названа: %q", problems[0])
	}
}

func TestCleanRemainderClampsNegativeForecast(t *testing.T) {
	// Прогноз прижимается к нулю: между подсчётом и обходом в каталог могли
	// добавить строки, и отрицательное число строк в отчёте бессмысленно.
	st := remainderStore(t, 1)
	ctx := context.Background()

	after, exact, problems := cleanRemainder(ctx, st, false, 2, 5)
	if after != 0 {
		t.Errorf("after = %d, ожидала 0", after)
	}
	if exact || len(problems) != 0 {
		t.Errorf("exact = %v, problems = %v", exact, problems)
	}
}

func TestCleanJSONCarriesExactnessFlag(t *testing.T) {
	// Схема JSON обязана нести признак точности: потребитель отличает факт от
	// прогноза по полю, а не по догадке. Проверка статическая, потому что
	// cmdClean требует конфиг и живую базу. После нарезки этапа 183 код clean
	// живёт в clean.go.
	src, err := os.ReadFile("clean.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	if !strings.Contains(text, `"files_after_exact": filesAfterExact`) {
		t.Error("в JSON clean нет поля files_after_exact")
	}
	if !strings.Contains(text, `out["problems"] = problems`) {
		t.Error("в JSON clean нет поля problems")
	}
	if !strings.Contains(text, "cleanRemainder(ctx, st, *apply, filesBefore, filesRemoved)") {
		t.Error("cmdClean не использует cleanRemainder")
	}
	if strings.Contains(text, "if n, err := st.CountFiles(ctx); err == nil {\n\t\t\tfilesAfter = n") {
		t.Error("вернулась прежняя ветка с молчаливым прогнозом")
	}
}
