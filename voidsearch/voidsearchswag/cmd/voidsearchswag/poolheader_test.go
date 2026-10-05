package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"voidsearchswag/internal/store"
)

func TestPrintPoolHeaderTruncated(t *testing.T) {
	// Усечённая выдача обязана быть видна: «найдено 500» при 1200 совпадениях
	// читалось как полный ответ.
	out := captureStdout(t, func() {
		printPoolHeader(1500, 900, nil, 1200, nil, 500, 1000)
	})
	if !strings.Contains(out, "пул: всего 1500, живых 900") {
		t.Errorf("строка пула потеряна:\n%s", out)
	}
	if !strings.Contains(out, "найдено 1200, показано 500") {
		t.Errorf("усечение не названо:\n%s", out)
	}
	if !strings.Contains(out, fmt.Sprintf("потолок выдачи %d", store.OnionSearchLimit())) {
		t.Errorf("потолок не назван:\n%s", out)
	}
}

func TestPrintPoolHeaderFull(t *testing.T) {
	// Когда выдача полная, о потолке говорить нечего: лишняя оговорка
	// обесценила бы настоящую.
	out := captureStdout(t, func() {
		printPoolHeader(40, 30, nil, 12, nil, 12, 30)
	})
	if !strings.Contains(out, "найдено 12\n") {
		t.Errorf("строка совпадений потеряна:\n%s", out)
	}
	if strings.Contains(out, "потолок") || strings.Contains(out, "показано") {
		t.Errorf("оговорка об усечении при полной выдаче:\n%s", out)
	}
}

func TestPrintPoolHeaderStatsError(t *testing.T) {
	// Нули вместо непрочитанной статистики печатались как факт, потому что
	// ошибка OnionStats уходила в _.
	var problems []string
	out := captureStdout(t, func() {
		problems = printPoolHeader(0, 0, errors.New("база заблокирована"), 7, nil, 7, 30)
	})
	if strings.Contains(out, "всего 0, живых 0") {
		t.Errorf("нули напечатаны как факт при непрочитанной статистике:\n%s", out)
	}
	if !strings.Contains(out, "статистика не прочитана") {
		t.Errorf("в шапке нет признания:\n%s", out)
	}
	if len(problems) != 1 {
		t.Fatalf("проблем %d, ожидала одну: %v", len(problems), problems)
	}
	if !strings.Contains(problems[0], "база заблокирована") {
		t.Errorf("в проблеме нет причины: %q", problems[0])
	}
	// Совпадения при этом остаются: статистика пула и число совпадений - разные
	// источники, и потеря одного не отменяет другой.
	if !strings.Contains(out, "найдено 7") {
		t.Errorf("число совпадений потеряно вместе со статистикой:\n%s", out)
	}
}

func TestPrintPoolHeaderMatchError(t *testing.T) {
	// Ошибка счётчика совпадений не должна подменять точное число размером
	// выдачи: показывается то, что есть, и прямо говорится, чего нет.
	var problems []string
	out := captureStdout(t, func() {
		problems = printPoolHeader(100, 80, nil, 0, errors.New("нет таблицы"), 12, 30)
	})
	if strings.Contains(out, "найдено") {
		t.Errorf("число совпадений напечатано, хотя счётчик не прочитан:\n%s", out)
	}
	if !strings.Contains(out, "показано 12") {
		t.Errorf("размер выдачи потерян:\n%s", out)
	}
	if !strings.Contains(out, "точное число совпадений не прочитано") {
		t.Errorf("в шапке нет признания:\n%s", out)
	}
	if len(problems) != 1 || !strings.Contains(problems[0], "нет таблицы") {
		t.Errorf("проблемы неверны: %v", problems)
	}
}

func TestPoolSearchCLIReportsTruncation(t *testing.T) {
	// Сквозная проверка на живой базе: команда с -limit 1000 обязана сказать,
	// что совпадений больше, чем показано.
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	st, err := store.Open(filepath.Join(dir, "voidsearchswag.db"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	want := store.OnionSearchLimit() + 100
	for i := 0; i < want; i++ {
		o := store.Onion{
			URL:    fmt.Sprintf("http://clip%04dhostaaaa.onion/", i),
			Title:  fmt.Sprintf("clip host %d", i),
			Status: "live",
		}
		if err := st.UpsertOnion(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	out, code := runMain(t, "poolsearch", "-limit", "1000", "clip host")
	if code != 0 {
		t.Fatalf("код возврата %d: %s", code, firstN(out, 300))
	}
	if !strings.Contains(out, fmt.Sprintf("найдено %d, показано %d", want, store.OnionSearchLimit())) {
		t.Errorf("усечение не названо в живом прогоне:\n%s", firstN(out, 300))
	}
}

func TestPoolSearchCLIFullResultHasNoTruncationNote(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	st, err := store.Open(filepath.Join(dir, "voidsearchswag.db"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		o := store.Onion{
			URL:    fmt.Sprintf("http://full%04dhostaaaa.onion/", i),
			Title:  fmt.Sprintf("full host %d", i),
			Status: "live",
		}
		if err := st.UpsertOnion(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	out, code := runMain(t, "poolsearch", "full host")
	if code != 0 {
		t.Fatalf("код возврата %d: %s", code, firstN(out, 300))
	}
	if !strings.Contains(out, "найдено 5") {
		t.Errorf("точное число совпадений не напечатано:\n%s", firstN(out, 300))
	}
	if strings.Contains(out, "потолок") {
		t.Errorf("оговорка об усечении при полной выдаче:\n%s", firstN(out, 300))
	}
}
