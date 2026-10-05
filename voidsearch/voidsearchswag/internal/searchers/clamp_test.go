package searchers

import (
	"testing"
)

// TestClampResults закрывает дефект, из-за которого DuckDuckGo.Search принимал
// параметр limit и полностью его игнорировал.
//
// Формально это работало, потому что вызывающий всё равно обрезал результат в
// Dedupe, но делало параметр бессмысленным именно здесь. Проверяется через
// чистую функцию, потому что эндпоинт DuckDuckGo зашит в код и направить
// Search на тестовый сервер нельзя.
func TestClampResults(t *testing.T) {
	mk := func(n int) []Result {
		out := make([]Result, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, Result{Title: "t", URL: "http://example.com/", Rank: i + 1})
		}
		return out
	}

	cases := []struct {
		name  string
		in    int
		limit int
		want  int
	}{
		{"меньше лимита остаётся как есть", 3, 10, 3},
		{"ровно лимит", 10, 10, 10},
		{"больше лимита обрезается", 30, 10, 10},
		{"limit 1", 5, 1, 1},
		{"ноль означает не ограничивать", 7, 0, 7},
		{"отрицательный не ограничивает", 7, -5, 7},
		{"пустой вход", 0, 10, 0},
		{"пустой вход без ограничения", 0, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := clampResults(mk(c.in), c.limit)
			if len(got) != c.want {
				t.Errorf("clampResults(%d, %d) = %d, ожидала %d", c.in, c.limit, len(got), c.want)
			}
		})
	}
}

func TestClampResultsPreservesEngineOrder(t *testing.T) {
	// Обрезка обязана сохранять порядок движка: ранжированием занимается
	// RerankWithHosts, и локальная сортировка внутри одного движка перечёркивала
	// бы его собственную оценку релевантности.
	in := []Result{
		{URL: "http://first.example/"},
		{URL: "http://second.example/"},
		{URL: "http://third.example/"},
		{URL: "http://fourth.example/"},
	}
	got := clampResults(in, 3)
	if len(got) != 3 {
		t.Fatalf("получено %d, ожидала 3", len(got))
	}
	want := []string{"http://first.example/", "http://second.example/", "http://third.example/"}
	for i, w := range want {
		if got[i].URL != w {
			t.Errorf("[%d] = %s, ожидала %s", i, got[i].URL, w)
		}
	}
	// Отрезан хвост, а не произвольные элементы.
	if got[2].URL != "http://third.example/" {
		t.Errorf("обрезан не хвост: %s", got[2].URL)
	}
}

func TestClampResultsDoesNotMutateInput(t *testing.T) {
	// Обрезка возвращает срез того же массива, поэтому проверяется, что сам
	// вход не испорчен: вызывающий может использовать исходный список дальше.
	in := []Result{{URL: "http://a.example/"}, {URL: "http://b.example/"}, {URL: "http://c.example/"}}
	got := clampResults(in, 1)
	if len(got) != 1 {
		t.Fatalf("получено %d", len(got))
	}
	if len(in) != 3 {
		t.Errorf("вход изменён: len=%d, ожидала 3", len(in))
	}
}

func TestClampResultsNilSafe(t *testing.T) {
	if got := clampResults(nil, 10); len(got) != 0 {
		t.Errorf("nil дал %d записей", len(got))
	}
	if got := clampResults(nil, 0); len(got) != 0 {
		t.Errorf("nil без ограничения дал %d записей", len(got))
	}
}
