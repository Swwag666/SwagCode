package store

import (
	"context"
	"testing"
)

func openVotesDB(t *testing.T) *Store {
	t.Helper()
	st, err := Open(t.TempDir() + "/rel.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestSubmitVotesClampsAndSkips(t *testing.T) {
	st := openVotesDB(t)
	ctx := context.Background()
	n, err := st.SubmitVotes(ctx, []Vote{
		{QueryHash: "q1", URL: "https://a.example/1", Score: 2.5, Author: "j"},
		{QueryHash: "q1", URL: "https://b.example/2", Score: -1, Author: "j"},
		{QueryHash: "q1", URL: "  ", Score: 1, Author: "j"},
		{QueryHash: "", URL: "https://c.example/3", Score: 1, Author: "j"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("принято %d, ожидала 2", n)
	}
	got, err := st.HostQuality(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	// 2.5 кламп в 1.0 -> бонус +3; -1 кламп в 0 -> бонус -3.
	if got["a.example"] != 3 {
		t.Errorf("бонус любимого хоста %v", got["a.example"])
	}
	if got["b.example"] != -3 {
		t.Errorf("бонус мусора %v", got["b.example"])
	}
}

func TestSubmitVotesClampedScoreChangesAverage(t *testing.T) {
	// Прежний тест на клампинг проверял бонус одиночного голоса: score 2.5 даёт
	// (2.5-0.5)*6 = 12, а после зажима бонуса в +3 получается ровно тот же +3,
	// что и при приведении оценки к 1.0. Мутация «убрать клампинг оценок»
	// проходила весь пакет незамеченной - это измерено, а не предположено.
	// Различие видно только на среднем: вне шкалы уходит сама оценка, а зажим
	// бонуса скрывает её, пока голос один.
	st := openVotesDB(t)
	ctx := context.Background()
	if _, err := st.SubmitVotes(ctx, []Vote{
		{QueryHash: "q-avg", URL: "https://mixed.example/1", Score: 5, Author: "first"},
		{QueryHash: "q-avg", URL: "https://mixed.example/2", Score: 0.5, Author: "second"},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := st.HostQuality(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	// 5 приводится к 1.0, веса авторов равны, среднее (1.0+0.5)/2 = 0.75, бонус
	// (0.75-0.5)*6 = 1.5. Без приведения среднее было бы 2.75, и бонус упёрся бы
	// в потолок +3.
	if diff := got["mixed.example"] - 1.5; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("бонус %v, хочу 1.5: оценка вне шкалы не приведена к границе", got["mixed.example"])
	}
}

func TestHostQualityNeedsMinVotes(t *testing.T) {
	st := openVotesDB(t)
	ctx := context.Background()
	if _, err := st.SubmitVotes(ctx, []Vote{
		{QueryHash: "q1", URL: "https://solo.example/1", Score: 1, Author: "j"},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := st.HostQuality(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("одиночный голос дал бонус: %+v", got)
	}
}

func TestHostQualityDownweightsSpammer(t *testing.T) {
	st := openVotesDB(t)
	ctx := context.Background()
	// Спамер ставит 100 единиц одному хосту, честный - одну единицу другому.
	var votes []Vote
	for i := 0; i < 100; i++ {
		votes = append(votes, Vote{QueryHash: "q", URL: "https://spam.example/x", Score: 1, Author: "spammer"})
	}
	// Один автор - upsert склеит в одну строку! Разные URL нужны.
	votes = votes[:0]
	for i := 0; i < 10; i++ {
		votes = append(votes, Vote{
			QueryHash: "q",
			URL:       "https://spam.example/page" + string(rune('a'+i)),
			Score:     1,
			Author:    "spammer",
		})
	}
	votes = append(votes,
		Vote{QueryHash: "q", URL: "https://honest.example/1", Score: 1, Author: "honest1"},
		Vote{QueryHash: "q", URL: "https://honest.example/2", Score: 1, Author: "honest2"},
		Vote{QueryHash: "q", URL: "https://honest.example/3", Score: 1, Author: "honest3"},
	)
	if _, err := st.SubmitVotes(ctx, votes); err != nil {
		t.Fatal(err)
	}
	got, err := st.HostQuality(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	// Честный хост набрал три мнения и получает +3. Спамерский набрал десять
	// строк, но все от одного автора: порог считается по людям, поэтому доверия
	// он не получает вовсе. Прежняя версия теста утверждала здесь +3 для обоих
	// хостов, потому что порог считался по строкам, и накрутка одним автором
	// проходила наравне с тремя независимыми судьями.
	if got["honest.example"] != 3 {
		t.Errorf("честный хост: %+v", got)
	}
	if _, ok := got["spam.example"]; ok {
		t.Errorf("спамерский хост получил бонус по голосам одного автора: %+v", got)
	}
	// Тот же набор с порогом 1 показывает спамера: значит выше его отсёк именно
	// порог по числу судей, а не пустой результат.
	low, err := st.HostQuality(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := low["spam.example"]; !ok {
		t.Errorf("при minVotes=1 спамерского хоста нет: %+v", low)
	}
	votesN, queries, hosts, err := st.VoteStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if votesN != 13 || queries != 1 || hosts != 2 {
		t.Errorf("статистика %d/%d/%d", votesN, queries, hosts)
	}
}

func TestVoteUpsertSameKey(t *testing.T) {
	st := openVotesDB(t)
	ctx := context.Background()
	v := Vote{QueryHash: "q", URL: "https://a.example/1", Score: 0.2, Author: "j"}
	if _, err := st.SubmitVotes(ctx, []Vote{v}); err != nil {
		t.Fatal(err)
	}
	v.Score = 0.9
	if _, err := st.SubmitVotes(ctx, []Vote{v}); err != nil {
		t.Fatal(err)
	}
	// Переоценка перезаписывает, а не плодит строки.
	n, _, _, err := st.VoteStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("голосов %d, ожидала 1", n)
	}
	got, err := st.HostQuality(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if b := got["a.example"]; b < 2.39 || b > 2.41 {
		t.Errorf("бонус %v, ожидала ~2.4 ((0.9-0.5)*6)", b)
	}
}
