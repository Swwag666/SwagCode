package search

import (
	"context"
	"testing"

	"voidsearchswag/internal/router"
	"voidsearchswag/internal/searchers"
	"voidsearchswag/internal/store"
)

// Оценка судьи отвечает на вопрос «насколько этот URL отвечает ЭТОМУ запросу»,
// а не «хорош ли хост вообще». Замер ДО: три голоса по запросу «leak database»
// поднимали loved.example на первое место в выдаче по чужому запросу «borsch
// recipe», где его никто не оценивал, - бонус хоста считался по всей таблице
// relevance без фильтра по query_hash.
//
// Счета подобраны так, чтобы перелом был однозначным, а не ничьей: без голосов
// meh.example набирает 8 (оба токена в заголовке по 3 плюс оба в url по 1),
// loved.example - 6 (один токен в заголовке 3, один в url 1, один в сниппете 1
// и 1 за непустой сниппет). Бонус +3 делает 9 против 8.
func TestJudgedHostIsNotLiftedInForeignQuery(t *testing.T) {
	e, st := newEngine(t,
		&fixedSearcher{name: "a", res: []searchers.Result{{
			URL:     "https://loved.example/borsch",
			Title:   "Borsch",
			Snippet: "recipe for soup",
			Source:  "a",
		}}},
		&fixedSearcher{name: "b", res: []searchers.Result{{
			URL:    "https://meh.example/borsch-recipe",
			Title:  "Borsch recipe",
			Source: "b",
		}}},
	)
	ctx := context.Background()

	out, err := e.Search(ctx, Options{Query: "borsch recipe", Mode: router.ModeFast, NoCache: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Results) < 2 {
		t.Fatalf("выдача короче двух результатов: %+v", out.Results)
	}
	if out.Results[0].URL != "https://meh.example/borsch-recipe" {
		t.Fatalf("базовый топ неверен: %+v", out.Results[0])
	}

	// Судья хвалит loved.example тремя голосами разных авторов - но по другому
	// запросу. Порог доверия проходим, бонус начисляется.
	for _, author := range []string{"judge1", "judge2", "judge3"} {
		if _, err := st.SubmitVotes(ctx, []store.Vote{{
			QueryHash: store.QueryHash("leak database", "judge"),
			URL:       "https://loved.example/borsch",
			Score:     1.0,
			Author:    author,
		}}); err != nil {
			t.Fatal(err)
		}
	}
	if n := e.RetuneBoosts(ctx); n == 0 {
		t.Fatal("пересчёт не нашёл хостов с бонусом")
	}

	out, err = e.Search(ctx, Options{Query: "borsch recipe", Mode: router.ModeFast, NoCache: true})
	if err != nil {
		t.Fatal(err)
	}
	if out.Results[0].URL != "https://meh.example/borsch-recipe" {
		t.Errorf("голоса по запросу «leak database» подняли чужой результат в выдаче «borsch recipe»: первым идёт %+v", out.Results[0])
	}
}

// Обратная сторона: в том запросе, где судья голосовал, бонус обязан работать.
// Без этой проверки правку «не применять бонусы вовсе» нельзя было бы отличить
// от правильной привязки к запросу.
func TestJudgedHostIsLiftedInItsOwnQuery(t *testing.T) {
	e, st := newEngine(t,
		&fixedSearcher{name: "a", res: []searchers.Result{{
			URL:     "https://loved.example/borsch",
			Title:   "Borsch",
			Snippet: "recipe for soup",
			Source:  "a",
		}}},
		&fixedSearcher{name: "b", res: []searchers.Result{{
			URL:    "https://meh.example/borsch-recipe",
			Title:  "Borsch recipe",
			Source: "b",
		}}},
	)
	ctx := context.Background()

	for _, author := range []string{"judge1", "judge2", "judge3"} {
		if _, err := st.SubmitVotes(ctx, []store.Vote{{
			QueryHash: store.QueryHash("borsch recipe", "judge"),
			URL:       "https://loved.example/borsch",
			Score:     1.0,
			Author:    author,
		}}); err != nil {
			t.Fatal(err)
		}
	}
	if n := e.RetuneBoosts(ctx); n == 0 {
		t.Fatal("пересчёт не нашёл хостов с бонусом")
	}

	out, err := e.Search(ctx, Options{Query: "borsch recipe", Mode: router.ModeFast, NoCache: true})
	if err != nil {
		t.Fatal(err)
	}
	if out.Results[0].URL != "https://loved.example/borsch" {
		t.Errorf("бонус не поднял хост в том запросе, где его оценили: первым идёт %+v", out.Results[0])
	}
}
