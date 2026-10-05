package search

import (
	"context"
	"testing"

	"voidsearchswag/internal/router"
	"voidsearchswag/internal/searchers"
	"voidsearchswag/internal/store"
)

func TestRetuneBoostsLiftsJudgedHost(t *testing.T) {
	e, st := newEngine(t,
		&fixedSearcher{name: "a", res: []searchers.Result{{URL: "https://loved.example/leak", Title: "Leak files", Source: "a"}}},
		&fixedSearcher{name: "b", res: []searchers.Result{{URL: "https://meh.example/y", Title: "Leak database", Source: "b"}}},
	)
	ctx := context.Background()
	// Без голосов побеждает точное совпадение токенов (6 против 4).
	out, err := e.Search(ctx, Options{Query: "leak database", Mode: router.ModeFast, NoCache: true})
	if err != nil {
		t.Fatal(err)
	}
	if out.Results[0].URL != "https://meh.example/y" {
		t.Fatalf("базовый топ неверен: %+v", out.Results[0])
	}
	// Судья хвалит loved.example тремя голосами (разные авторы - иначе
	// upsert склеит в один): хост проходит порог и получает бонус.
	for _, author := range []string{"judge1", "judge2", "judge3"} {
		if _, err := st.SubmitVotes(ctx, []store.Vote{{
			QueryHash: store.QueryHash("leak database", "judge"),
			URL:       "https://loved.example/leak",
			Score:     1.0,
			Author:    author,
		}}); err != nil {
			t.Fatal(err)
		}
	}
	if n := e.RetuneBoosts(ctx); n == 0 {
		t.Fatal("пересчёт не нашёл хостов")
	}
	out, err = e.Search(ctx, Options{Query: "leak database", Mode: router.ModeFast, NoCache: true})
	if err != nil {
		t.Fatal(err)
	}
	if out.Results[0].URL != "https://loved.example/leak" {
		t.Errorf("бонус хоста не поднял результат: %+v", out.Results[0])
	}
}
