package store

import (
	"context"
	"fmt"
	"testing"
)

// Порог доверия minVotes считается по разным судьям, а не по строкам. Прежний
// подсчёт строк означал, что один человек с пятью оценками покупал хосту
// максимальный бонус +3 в реранке наравне с тремя независимыми судьями: вес
// 1/sqrt(n) гасил накрутку в среднем, но не в самом факте доверия.
func TestHostQualityThresholdCountsJudgesNotRows(t *testing.T) {
	st := openVotesDB(t)
	ctx := context.Background()

	var solo []Vote
	for i := 0; i < 5; i++ {
		solo = append(solo, Vote{
			QueryHash: "q", URL: fmt.Sprintf("https://one.example/%d", i), Score: 1, Author: "single",
		})
	}
	if _, err := st.SubmitVotes(ctx, solo); err != nil {
		t.Fatal(err)
	}

	got, err := st.HostQuality(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["one.example"]; ok {
		t.Errorf("пять голосов одного судьи прошли порог 3: %+v", got)
	}
	// Порог 1 тот же хост показывает: значит выше его отсекли по числу судей, а
	// не из-за пустого результата или ошибки.
	low, err := st.HostQuality(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := low["one.example"]; !ok {
		t.Errorf("при minVotes=1 хоста нет: %+v", low)
	}

	// Два независимых судьи доводят число людей до трёх, и хост проходит порог.
	if _, err := st.SubmitVotes(ctx, []Vote{
		{QueryHash: "q", URL: "https://one.example/100", Score: 1, Author: "second"},
		{QueryHash: "q", URL: "https://one.example/101", Score: 1, Author: "third"},
	}); err != nil {
		t.Fatal(err)
	}
	got2, err := st.HostQuality(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	if got2["one.example"] != 3 {
		t.Errorf("три судьи не дали бонус: %+v", got2)
	}
}

// Строки без host не дают хосту судей: порог обязан считаться по тем же строкам,
// что участвуют в агрегации, иначе голоса с пустым host поднимали бы доверие
// чужому хосту.
func TestHostQualityThresholdIgnoresHostlessVotes(t *testing.T) {
	st := openVotesDB(t)
	ctx := context.Background()

	if _, err := st.SubmitVotes(ctx, []Vote{
		{QueryHash: "q", URL: "https://a.example/1", Score: 1, Author: "first"},
		{QueryHash: "q", URL: "https://a.example/2", Score: 1, Author: "second"},
	}); err != nil {
		t.Fatal(err)
	}
	// Пять авторов голосуют строками без host: SubmitVotes их отбрасывает, а в
	// таблице они возможны. У каждого при этом есть и настоящий голос, иначе он
	// не попал бы в подзапрос авторских весов (там фильтр host<>'') и внутренний
	// JOIN отбросил бы его строки сам - тогда тест не различал бы отсутствие
	// фильтра в основном запросе.
	for i := 0; i < 5; i++ {
		if _, err := st.db.ExecContext(ctx, `
			INSERT INTO relevance(query_hash, url, host, score, author, updated_at)
			VALUES ('q', ?, '', 1.0, ?, CURRENT_TIMESTAMP)`,
			fmt.Sprintf("nohost%d", i), fmt.Sprintf("ghost%d", i)); err != nil {
			t.Fatal(err)
		}
		// Настоящий голос того же автора на свой хост: порог 3 он не проходит,
		// но в весах участвует.
		if _, err := st.db.ExecContext(ctx, `
			INSERT INTO relevance(query_hash, url, host, score, author, updated_at)
			VALUES ('q', ?, ?, 1.0, ?, CURRENT_TIMESTAMP)`,
			fmt.Sprintf("https://ghost%d.example/1", i), fmt.Sprintf("ghost%d.example", i),
			fmt.Sprintf("ghost%d", i)); err != nil {
			t.Fatal(err)
		}
	}

	got, err := st.HostQuality(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["a.example"]; ok {
		t.Errorf("хост получил судей из строк без host: %+v", got)
	}
	// Пять авторов строк без host дают группу с пустым ключом и проходят порог
	// по людям, если фильтр host<>'' из основного запроса убрать.
	if _, ok := got[""]; ok {
		t.Errorf("пустой host попал в результат: %+v", got)
	}
	// Хосты-призраки в результат не попадают: у каждого один судья.
	for i := 0; i < 5; i++ {
		if _, ok := got[fmt.Sprintf("ghost%d.example", i)]; ok {
			t.Errorf("хост одного судьи прошёл порог: %+v", got)
		}
	}
	if len(got) != 0 {
		t.Errorf("в результате %d хостов, хочу 0: %+v", len(got), got)
	}
}

// Порог применяется к каждому хосту отдельно: сосед с одним судьёй не проходит
// вместе с хостом, у которого судей достаточно.
func TestHostQualityThresholdAppliesPerHost(t *testing.T) {
	st := openVotesDB(t)
	ctx := context.Background()

	votes := []Vote{
		{QueryHash: "q", URL: "https://crowd.example/1", Score: 0.8, Author: "a"},
		{QueryHash: "q", URL: "https://crowd.example/2", Score: 0.8, Author: "b"},
		{QueryHash: "q", URL: "https://crowd.example/3", Score: 0.8, Author: "c"},
	}
	for i := 0; i < 4; i++ {
		votes = append(votes, Vote{
			QueryHash: "q", URL: fmt.Sprintf("https://lonely.example/%d", i), Score: 1, Author: "lonely",
		})
	}
	if _, err := st.SubmitVotes(ctx, votes); err != nil {
		t.Fatal(err)
	}

	got, err := st.HostQuality(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["crowd.example"]; !ok {
		t.Errorf("хост трёх судей не прошёл порог: %+v", got)
	}
	if _, ok := got["lonely.example"]; ok {
		t.Errorf("хост одного судьи прошёл порог вместе с соседом: %+v", got)
	}
	if len(got) != 1 {
		t.Errorf("в результате %d хостов, хочу 1: %+v", len(got), got)
	}
}
