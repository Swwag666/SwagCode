package store

import (
	"context"
	"testing"
)

// TestSearchOnionsRanksTitleAboveAddress закрывает дефект, из-за которого
// poolsearch book находил сервисы без книг в названии.
//
// Адрес onion-сервиса - 56 случайных символов base32, поэтому любая короткая
// подстрока регулярно встречается внутри него просто по случайности. Без ранга
// выдача сортировалась только по success_rate, и случайное совпадение в адресе
// оказывалось выше настоящего совпадения в названии, если у него была лучше
// статистика.
func TestSearchOnionsRanksTitleAboveAddress(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	// Сервис с совпадением только в адресе получает заведомо лучшую
	// статистику: если ранг не работает, он всплывёт первым.
	randomAddr := "http://bookzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz.onion"
	if err := st.UpsertOnion(ctx, Onion{
		URL: randomAddr, Status: "live", Title: "Совершенно иное",
		SuccessRate: 1.0, LatencyAvg: 100,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertOnion(ctx, Onion{
		URL: "http://real-library.onion", Status: "live", Title: "Book archive",
		SuccessRate: 0.2, LatencyAvg: 5000,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := st.SearchOnions(ctx, "book", "", 10, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("найдено %d, ожидала 2", len(got))
	}
	if got[0].Title != "Book archive" {
		t.Errorf("первым идёт %q: совпадение в адресе победило совпадение в заголовке", got[0].Title)
	}
}

func TestSearchOnionsRanksDescriptionAboveAddress(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	if err := st.UpsertOnion(ctx, Onion{
		URL:    "http://bookzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz.onion",
		Status: "live", Title: "Не то", Description: "не связано",
		SuccessRate: 1.0, LatencyAvg: 100,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertOnion(ctx, Onion{
		URL: "http://other.onion", Status: "live", Title: "Не то",
		Description: "библиотека book collection",
		SuccessRate: 0.1, LatencyAvg: 9000,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := st.SearchOnions(ctx, "book", "", 10, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("найдено %d, ожидала 2", len(got))
	}
	if got[0].Description == "" || got[0].URL != "http://other.onion" {
		t.Errorf("описание не победило адрес: %+v", got[0])
	}
}

func TestSearchOnionsAddressMatchStillFound(t *testing.T) {
	// Совпадение по адресу не убрано, а только опущено в конец: искать сервис
	// по фрагменту адреса - законный сценарий, когда название неизвестно.
	st := newStore(t)
	ctx := context.Background()
	target := "http://zlibrary24tuxziyiyfr7zd46ytefdqbqd2axkmxm4o5374ptpc52fad.onion"
	if err := st.UpsertOnion(ctx, Onion{URL: target, Status: "live"}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertOnion(ctx, Onion{
		URL: "http://unrelated.onion", Status: "live", Title: "zlibrary mirror",
	}); err != nil {
		t.Fatal(err)
	}

	got, err := st.SearchOnions(ctx, "zlibrary", "", 10, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("найдено %d, ожидала 2: адрес перестал учитываться", len(got))
	}
	// Заголовок раньше адреса.
	if got[0].Title != "zlibrary mirror" {
		t.Errorf("первым идёт %+v", got[0])
	}
	if got[1].URL != target {
		t.Errorf("вторым должен быть сервис с совпадением в адресе, получено %+v", got[1])
	}
}

func TestSearchOnionsRankDoesNotBreakStatusFilter(t *testing.T) {
	// Регрессия на порядок связывания параметров: аргументы ранга из ORDER BY
	// обязаны идти после аргументов WHERE. При перепутанном порядке фильтр по
	// статусу получал строку шаблона и не отбирал ничего.
	st := newStore(t)
	ctx := context.Background()
	for _, o := range []Onion{
		{URL: "http://a-book.onion", Status: "live", Title: "book one"},
		{URL: "http://b-book.onion", Status: "dead", Title: "book two"},
		{URL: "http://c-book.onion", Status: "unknown", Title: "book three"},
	} {
		if err := st.UpsertOnion(ctx, o); err != nil {
			t.Fatal(err)
		}
	}

	live, err := st.SearchOnions(ctx, "book", "live", 10, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || live[0].Status != "live" {
		t.Errorf("фильтр live+текст дал %+v", live)
	}

	dead, err := st.SearchOnions(ctx, "book", "dead", 10, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(dead) != 1 || dead[0].Status != "dead" {
		t.Errorf("фильтр dead+текст дал %+v", dead)
	}

	// Без явного статуса мёртвые исключаются.
	all, err := st.SearchOnions(ctx, "book", "", 10, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Errorf("ожидала 2 записи без мёртвых, получено %d", len(all))
	}
	for _, o := range all {
		if o.Status == "dead" {
			t.Errorf("мёртвый сервис в выдаче: %+v", o)
		}
	}
}

func TestSearchOnionsNoTextKeepsStatisticsOrder(t *testing.T) {
	// Без текстового запроса ранг постоянен и порядок обязан остаться прежним:
	// по success_rate, затем по latency.
	st := newStore(t)
	ctx := context.Background()
	for _, o := range []Onion{
		{URL: "http://slow-good.onion", Status: "live", SuccessRate: 0.9, LatencyAvg: 5000},
		{URL: "http://fast-good.onion", Status: "live", SuccessRate: 0.9, LatencyAvg: 500},
		{URL: "http://bad.onion", Status: "live", SuccessRate: 0.1, LatencyAvg: 100},
	} {
		if err := st.UpsertOnion(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.SearchOnions(ctx, "", "", 10, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("найдено %d", len(got))
	}
	if got[0].URL != "http://fast-good.onion" {
		t.Errorf("первым должен быть самый быстрый при равном success_rate, получено %s", got[0].URL)
	}
	if got[2].URL != "http://bad.onion" {
		t.Errorf("последним должен быть худший по success_rate, получено %s", got[2].URL)
	}
}

func TestSearchOnionsRankStableWithLimit(t *testing.T) {
	// Ранг обязан применяться до LIMIT, а не после: иначе релевантная запись
	// отсекается вместе с нерелевантными.
	st := newStore(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if err := st.UpsertOnion(ctx, Onion{
			URL:    "http://zzzbookzzz" + string(rune('a'+i)) + ".onion",
			Status: "live", Title: "случайный", SuccessRate: 1.0, LatencyAvg: 100,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.UpsertOnion(ctx, Onion{
		URL: "http://real.onion", Status: "live", Title: "book archive",
		SuccessRate: 0.01, LatencyAvg: 20000,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := st.SearchOnions(ctx, "book", "", 2, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("найдено %d, ожидала 2", len(got))
	}
	if got[0].Title != "book archive" {
		t.Errorf("при limit=2 релевантная запись отсекалась: первой идёт %+v", got[0])
	}
}
