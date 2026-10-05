package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"voidsearchswag/internal/store"
)

// seedVotes пишет голоса одного запроса на один хост: каждый голос от своего
// автора, потому что порог доверия считается по людям, и число авторов здесь -
// управляемый параметр.
func seedVotes(t *testing.T, dir string, authors int) {
	t.Helper()
	st := openTestStore(t, dir)
	defer closeStore(st)
	ctx := context.Background()
	var votes []store.Vote
	for i := 0; i < authors; i++ {
		votes = append(votes, store.Vote{
			QueryHash: store.QueryHash("seeded judge query", "judge"),
			URL:       fmt.Sprintf("https://judged.example/%d", i),
			Score:     1,
			Author:    fmt.Sprintf("judge%d", i),
		})
	}
	if _, err := st.SubmitVotes(ctx, votes); err != nil {
		t.Fatalf("запись голосов: %v", err)
	}
}

// Судейская база обязана быть видна в сводке: без этого выученное ранжирование
// нельзя было ни проверить, ни объяснить.
func TestStatsShowsJudgeBase(t *testing.T) {
	dir := seedStatsBase(t)
	seedVotes(t, dir, 3)
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	out, code := runMain(t, "stats")
	if code != 0 {
		t.Fatalf("код возврата %d, хочу 0: %s", code, out)
	}
	want := "судьи: 3 голосов, 1 запросов, 1 хостов  бонусы: 1 хостов"
	if !strings.Contains(out, want) {
		t.Errorf("в выводе нет %q: %s", want, out)
	}
}

// Голоса есть, а доверия нет: один судья с пятью оценками не даёт хосту бонус.
// Именно эту разницу и должно быть видно в сводке, иначе оператор принимает
// наличие голосов за выученное ранжирование.
func TestStatsShowsVoicesWithoutTrust(t *testing.T) {
	dir := seedStatsBase(t)
	st := openTestStore(t, dir)
	ctx := context.Background()
	var votes []store.Vote
	for i := 0; i < 5; i++ {
		votes = append(votes, store.Vote{
			QueryHash: store.QueryHash("one judge", "judge"),
			URL:       fmt.Sprintf("https://single.example/%d", i),
			Score:     1,
			Author:    "single",
		})
	}
	if _, err := st.SubmitVotes(ctx, votes); err != nil {
		t.Fatalf("запись голосов: %v", err)
	}
	closeStore(st)
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	jsonOut, code := runMain(t, "stats", "--json")
	if code != 0 {
		t.Fatalf("код возврата %d, хочу 0: %s", code, jsonOut)
	}
	var body struct {
		JudgeVotes   int `json:"judge_votes"`
		JudgeQueries int `json:"judge_queries"`
		JudgeHosts   int `json:"judge_hosts"`
		BoostedHosts int `json:"boosted_hosts"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &body); err != nil {
		t.Fatalf("JSON не разобран: %v\nвывод: %s", err, jsonOut)
	}
	if body.JudgeVotes != 5 {
		t.Errorf("judge_votes = %d, хочу 5", body.JudgeVotes)
	}
	if body.JudgeHosts != 1 {
		t.Errorf("judge_hosts = %d, хочу 1", body.JudgeHosts)
	}
	if body.BoostedHosts != 0 {
		t.Errorf("boosted_hosts = %d, хочу 0: один судья не даёт доверия", body.BoostedHosts)
	}

	// Текстовый режим говорит то же самое.
	textOut, textCode := runMain(t, "stats")
	if textCode != 0 {
		t.Fatalf("код возврата %d: %s", textCode, textOut)
	}
	if !strings.Contains(textOut, "бонусы: 0 хостов") {
		t.Errorf("в тексте нет нулевого числа бонусов: %s", textOut)
	}
	if !strings.Contains(textOut, "судьи: 5 голосов") {
		t.Errorf("в тексте нет числа голосов: %s", textOut)
	}
}

// На пустой судейской базе поля присутствуют нулями: отсутствие поля клиент
// принял бы за «сводка не читалась».
func TestStatsJudgeFieldsAlwaysPresent(t *testing.T) {
	dir := seedStatsBase(t)
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	out, code := runMain(t, "stats", "--json")
	if code != 0 {
		t.Fatalf("код возврата %d, хочу 0: %s", code, out)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(out), &body); err != nil {
		t.Fatalf("JSON не разобран: %v\nвывод: %s", err, out)
	}
	for _, key := range []string{"judge_votes", "judge_queries", "judge_hosts", "boosted_hosts"} {
		v, ok := body[key]
		if !ok {
			t.Errorf("в ответе нет поля %s", key)
			continue
		}
		if n, _ := v.(float64); n != 0 {
			t.Errorf("%s = %v, хочу 0 на пустой базе", key, v)
		}
	}

	textOut, textCode := runMain(t, "stats")
	if textCode != 0 {
		t.Fatalf("код возврата %d: %s", textCode, textOut)
	}
	if !strings.Contains(textOut, "судьи: 0 голосов, 0 запросов, 0 хостов  бонусы: 0 хостов") {
		t.Errorf("в тексте нет строки судей: %s", textOut)
	}
}

// Провал чтения судейской базы обязан быть виден: ноль голосов и «таблицу не
// удалось прочитать» - разные состояния, а код возврата при неполной
// диагностике ненулевой.
func TestStatsReportsJudgeReadFailure(t *testing.T) {
	dir := seedStatsBase(t)
	seedVotes(t, dir, 3)
	dropTable(t, dir, "relevance")
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	stdout, stderr, code := runMainSplit(t, "stats")
	if code == 0 {
		t.Fatalf("код возврата 0 при непрочитанной судейской базе: %s", stdout)
	}
	if !strings.Contains(stderr, "судьи:") {
		t.Errorf("в stderr нет причины провала судей: %s", stderr)
	}
	if !strings.Contains(stderr, "бонусы хостов:") {
		t.Errorf("в stderr нет причины провала бонусов: %s", stderr)
	}
	// Прочие источники прочитаны и напечатаны: частичная поломка не обнуляет
	// остальную диагностику.
	if !strings.Contains(stdout, "пул: ") {
		t.Errorf("в выводе нет прочих счётчиков: %s", stdout)
	}

	jsonOut, jsonErr, jsonCode := runMainSplit(t, "stats", "--json")
	if jsonCode == 0 {
		t.Fatalf("JSON-режим вернул 0 при непрочитанной базе: %s", jsonOut)
	}
	var body struct {
		Errors       []string `json:"errors"`
		JudgeVotes   int      `json:"judge_votes"`
		BoostedHosts int      `json:"boosted_hosts"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &body); err != nil {
		t.Fatalf("JSON не разобран: %v\nstderr: %s\nstdout: %s", err, jsonErr, jsonOut)
	}
	joined := strings.Join(body.Errors, "; ")
	if !strings.Contains(joined, "судьи:") || !strings.Contains(joined, "бонусы хостов:") {
		t.Errorf("в errors нет обеих причин: %+v", body.Errors)
	}
	if body.JudgeVotes != 0 || body.BoostedHosts != 0 {
		t.Errorf("при провале чтения числа %d/%d, хочу 0/0 с причиной в errors", body.JudgeVotes, body.BoostedHosts)
	}
}
