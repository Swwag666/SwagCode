package store

import (
	"context"
	"math"
	"testing"
)

// Оценка судьи - это ответ на вопрос «насколько URL подходит ЭТОМУ запросу».
// Живой замер ДО: три голоса, записанные через MCP judge_submit по запросу
// «leak database», поднимали loved.example с седьмого места на пятое в выдаче
// «borsch recipe» и опускали meh.example с пятого на седьмое. HostQuality
// агрегировала всю таблицу relevance без фильтра по query_hash.

func TestHostQualityForQueryIsolatesVotes(t *testing.T) {
	st := openVotesDB(t)
	ctx := context.Background()
	own := QueryHash("leak database", "judge")
	foreign := QueryHash("borsch recipe", "judge")

	for _, author := range []string{"j1", "j2", "j3"} {
		if _, err := st.SubmitVotes(ctx, []Vote{{
			QueryHash: own, URL: "https://loved.example/1", Score: 1, Author: author,
		}}); err != nil {
			t.Fatal(err)
		}
	}

	got, err := st.HostQualityForQuery(ctx, own, 3)
	if err != nil {
		t.Fatal(err)
	}
	if b := got["loved.example"]; b != 3 {
		t.Errorf("бонус своего запроса %v, хочу 3", b)
	}

	other, err := st.HostQualityForQuery(ctx, foreign, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 0 {
		t.Errorf("чужой запрос получил бонусы %v: голоса не привязаны к query_hash", other)
	}

	all, err := st.HostQuality(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all["loved.example"] != 3 {
		t.Errorf("глобальная сводка изменилась: %v", all)
	}
}

// Порог доверия считается по людям, и люди эти обязаны относиться к тому же
// запросу: иначе два голоса здесь плюс один там давали бы хосту доверие,
// которого ни один из запросов не заработал.
func TestHostQualityForQueryThresholdIsPerQuery(t *testing.T) {
	st := openVotesDB(t)
	ctx := context.Background()
	a := QueryHash("query a", "judge")
	b := QueryHash("query b", "judge")

	if _, err := st.SubmitVotes(ctx, []Vote{
		{QueryHash: a, URL: "https://h.example/1", Score: 1, Author: "first"},
		{QueryHash: a, URL: "https://h.example/2", Score: 1, Author: "second"},
		{QueryHash: b, URL: "https://h.example/3", Score: 1, Author: "third"},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := st.HostQualityForQuery(ctx, a, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("порог доверия посчитан по чужим голосам: %v", got)
	}

	all, err := st.HostQuality(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Errorf("глобальная сводка потеряла хост, у которого три автора: %v", all)
	}
}

// Вес автора 1/sqrt(n) гасит накрутку, и n обязано считаться в пределах
// запроса. Автор с тысячей оценок по чужим запросам не должен терять вес здесь,
// а его голоса там не должны разбавлять этот запрос.
//
// Числа подобраны так, чтобы глобальные веса давали другой ответ:
//   - запрос A: heavy оставил четыре голоса (вес 0.5), first и second по одному
//     (вес 1). Среднее (4*1*0.5 + 0 + 1)/4 = 0.75, бонус (0.75-0.5)*6 = 1.5.
//   - запрос B: у heavy один голос (вес 1). Среднее (1+0+1)/3 = 0.6667, бонус 1.
//   - при глобальном подсчёте у heavy пять голосов, вес 1/sqrt(5)=0.447, и
//     бонус запроса A вышел бы около 1.676.
func TestHostQualityForQueryWeightsAuthorWithinQuery(t *testing.T) {
	st := openVotesDB(t)
	ctx := context.Background()
	a := QueryHash("query a", "judge")
	b := QueryHash("query b", "judge")

	votes := []Vote{
		{QueryHash: a, URL: "https://h.example/1", Score: 1, Author: "heavy"},
		{QueryHash: a, URL: "https://h.example/2", Score: 1, Author: "heavy"},
		{QueryHash: a, URL: "https://h.example/3", Score: 1, Author: "heavy"},
		{QueryHash: a, URL: "https://h.example/4", Score: 1, Author: "heavy"},
		{QueryHash: a, URL: "https://h.example/5", Score: 0, Author: "first"},
		{QueryHash: a, URL: "https://h.example/6", Score: 1, Author: "second"},
		{QueryHash: b, URL: "https://h.example/7", Score: 1, Author: "heavy"},
		{QueryHash: b, URL: "https://h.example/8", Score: 0, Author: "first"},
		{QueryHash: b, URL: "https://h.example/9", Score: 1, Author: "second"},
	}
	if _, err := st.SubmitVotes(ctx, votes); err != nil {
		t.Fatal(err)
	}

	gotA, err := st.HostQualityForQuery(ctx, a, 3)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(gotA["h.example"]-1.5) > 1e-9 {
		t.Errorf("бонус запроса A = %v, хочу 1.5: вес автора посчитан не в пределах запроса", gotA["h.example"])
	}

	gotB, err := st.HostQualityForQuery(ctx, b, 3)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(gotB["h.example"]-1.0) > 1e-9 {
		t.Errorf("бонус запроса B = %v, хочу 1.0: вес автора посчитан не в пределах запроса", gotB["h.example"])
	}
}

// Пустой hash не должен молча превращаться в сводку по всей базе: именно так
// чужие голоса и попали бы в чужую выдачу. Для глобальной картины есть
// HostQuality.
func TestHostQualityForQueryEmptyHashReturnsEmpty(t *testing.T) {
	st := openVotesDB(t)
	ctx := context.Background()
	for _, author := range []string{"j1", "j2", "j3"} {
		if _, err := st.SubmitVotes(ctx, []Vote{{
			QueryHash: QueryHash("real query", "judge"), URL: "https://h.example/1", Score: 1, Author: author,
		}}); err != nil {
			t.Fatal(err)
		}
	}

	for _, hash := range []string{"", "   "} {
		got, err := st.HostQualityForQuery(ctx, hash, 3)
		if err != nil {
			t.Fatalf("hash %q: %v", hash, err)
		}
		if len(got) != 0 {
			t.Errorf("hash %q вернул бонусы %v вместо пустой карты", hash, got)
		}
	}
}
