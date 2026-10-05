package store

import (
	"context"
	"fmt"
	"testing"
)

func seedPool(t *testing.T, st *Store, n int, status, titleFmt string) {
	t.Helper()
	ctx := context.Background()
	// openTest не мигрирует, а миграция идемпотентна: без неё onion_pool не
	// существует и первая же вставка падает.
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for i := 0; i < n; i++ {
		// Адрес включает статус: UpsertOnion обновляет запись по URL, и без
		// уникального префикса второй вызов seedPool перезаписал бы строки
		// первого вместо того, чтобы добавить свои.
		o := Onion{
			URL:    fmt.Sprintf("http://%s%04dpoolhostaaaa.onion/", status, i),
			Title:  fmt.Sprintf(titleFmt, i),
			Status: status,
		}
		if err := st.UpsertOnion(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCountOnionSearchReportsBeyondLimit(t *testing.T) {
	// Главная проверка. Выдача поиска ограничена потолком, и её размер не равен
	// числу совпадений: команда, которая печатала len(выдачи) под словом
	// «найдено», рапортовала 500 при шести сотнях совпадений, и отличить полный
	// ответ от усечённого было нельзя.
	st := openTest(t)
	ctx := context.Background()
	seedPool(t, st, 600, "live", "test host %d")

	onions, err := st.SearchOnions(ctx, "test host", "", 1000, false)
	if err != nil {
		t.Fatalf("SearchOnions: %v", err)
	}
	if len(onions) != OnionSearchLimit() {
		t.Fatalf("выдача %d, ожидала ровно потолок %d", len(onions), OnionSearchLimit())
	}
	matched, err := st.CountOnionSearch(ctx, "test host", "", false)
	if err != nil {
		t.Fatalf("CountOnionSearch: %v", err)
	}
	if matched != 600 {
		t.Errorf("matched = %d, ожидала 600", matched)
	}
	if matched <= len(onions) {
		t.Errorf("счётчик не превысил выдачу: matched %d, len %d", matched, len(onions))
	}
}

func TestCountOnionSearchAgreesWithSearchBelowLimit(t *testing.T) {
	// Пока совпадений меньше потолка, счётчик обязан совпадать с размером
	// выдачи: иначе шапка «найдено N, показано M» врала бы на каждом запросе.
	st := openTest(t)
	ctx := context.Background()
	seedPool(t, st, 40, "live", "agree host %d")

	for _, text := range []string{"agree host", "agree", "host 7", "нет-такого-слова"} {
		onions, err := st.SearchOnions(ctx, text, "", 100, false)
		if err != nil {
			t.Fatalf("SearchOnions(%q): %v", text, err)
		}
		matched, err := st.CountOnionSearch(ctx, text, "", false)
		if err != nil {
			t.Fatalf("CountOnionSearch(%q): %v", text, err)
		}
		if matched != len(onions) {
			t.Errorf("запрос %q: matched %d, выдача %d", text, matched, len(onions))
		}
	}
}

func TestCountOnionSearchRespectsStatusFilter(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	seedPool(t, st, 3, "live", "status host %d")
	seedPool(t, st, 2, "dead", "status dead %d")

	cases := []struct {
		name        string
		text        string
		status      string
		includeDead bool
		want        int
	}{
		{"без текста, мёртвые исключены", "", "", false, 3},
		{"без текста, мёртвые включены", "", "", true, 5},
		{"только мёртвые", "", "dead", true, 2},
		{"только живые", "", "live", false, 3},
		{"текст и статус", "status host", "live", false, 3},
		{"текст без совпадений", "чего-тут-нет", "", true, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := st.CountOnionSearch(ctx, c.text, c.status, c.includeDead)
			if err != nil {
				t.Fatalf("CountOnionSearch: %v", err)
			}
			if got != c.want {
				t.Errorf("got %d, want %d", got, c.want)
			}
			// Счётчик обязан совпадать с выдачей, когда потолок не достигнут.
			onions, err := st.SearchOnions(ctx, c.text, c.status, 500, c.includeDead)
			if err != nil {
				t.Fatalf("SearchOnions: %v", err)
			}
			if len(onions) != got {
				t.Errorf("расхождение с выдачей: matched %d, len %d", got, len(onions))
			}
		})
	}
}

func TestOnionSearchLimitMatchesClamp(t *testing.T) {
	// Порог, который отдаёт пакет, обязан быть тем самым порогом, до которого
	// SearchOnions урезает limit: иначе вызывающий будет предупреждать об
	// усечении по одному числу, а выдача урежется по другому.
	st := openTest(t)
	ctx := context.Background()
	seedPool(t, st, OnionSearchLimit()+5, "live", "clamp host %d")

	onions, err := st.SearchOnions(ctx, "clamp host", "", 100000, false)
	if err != nil {
		t.Fatalf("SearchOnions: %v", err)
	}
	if len(onions) != OnionSearchLimit() {
		t.Errorf("выдача %d, ожидала потолок %d", len(onions), OnionSearchLimit())
	}
}
