package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"voidsearchswag/internal/store"
)

func callStats(t *testing.T, c toolCaller) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "stats"},
	})
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if res.IsError {
		t.Fatalf("stats вернул ошибку: %s", textOf(t, res))
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(textOf(t, res)), &out); err != nil {
		t.Fatalf("ответ stats не разобран: %v\n%s", err, textOf(t, res))
	}
	return out
}

func dbBlock(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	db, ok := out["db"].(map[string]any)
	if !ok {
		t.Fatalf("в ответе stats нет блока db: %+v", out)
	}
	return db
}

// Судейская база видна в MCP-сводке: объём голосов и число хостов, которым бонус
// реально выдаётся. Одно число голосов вводило бы клиента в заблуждение, потому
// что порог доверия считается по разным судьям.
func TestStatsCarriesJudgeBase(t *testing.T) {
	st, eng, _ := promoteDeps(t, engineStub())
	ctx := context.Background()

	var solo []store.Vote
	for i := 0; i < 5; i++ {
		solo = append(solo, store.Vote{
			QueryHash: "q", URL: fmt.Sprintf("https://single.example/%d", i), Score: 1, Author: "single",
		})
	}
	if _, err := st.SubmitVotes(ctx, solo); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SubmitVotes(ctx, []store.Vote{
		{QueryHash: "q", URL: "https://crowd.example/1", Score: 1, Author: "a"},
		{QueryHash: "q", URL: "https://crowd.example/2", Score: 1, Author: "b"},
		{QueryHash: "q", URL: "https://crowd.example/3", Score: 1, Author: "c"},
	}); err != nil {
		t.Fatal(err)
	}

	srv := New(Deps{Version: "test", Store: st, Search: eng, Started: time.Now()})
	c := inProcess(t, srv)
	initClient(t, c)
	db := dbBlock(t, callStats(t, c))

	if got := numOf(t, db, "judge_votes"); got != 8 {
		t.Errorf("judge_votes = %v, хочу 8", got)
	}
	if got := numOf(t, db, "judge_hosts"); got != 2 {
		t.Errorf("judge_hosts = %v, хочу 2", got)
	}
	if got := numOf(t, db, "judge_queries"); got != 1 {
		t.Errorf("judge_queries = %v, хочу 1", got)
	}
	// Бонус только у хоста трёх судей: хост одного автора с пятью голосами
	// порог не проходит.
	if got := numOf(t, db, "boosted_hosts"); got != 1 {
		t.Errorf("boosted_hosts = %v, хочу 1", got)
	}
}

// На пустой судейской базе поля присутствуют нулями, а не исчезают: исчезнувший
// ключ клиент принял бы за «сводку не удалось прочитать».
func TestStatsJudgeFieldsPresentOnEmptyBase(t *testing.T) {
	st, eng, _ := promoteDeps(t, engineStub())
	srv := New(Deps{Version: "test", Store: st, Search: eng, Started: time.Now()})
	c := inProcess(t, srv)
	initClient(t, c)
	db := dbBlock(t, callStats(t, c))

	for _, key := range []string{"judge_votes", "judge_queries", "judge_hosts", "boosted_hosts"} {
		if got := numOf(t, db, key); got != 0 {
			t.Errorf("%s = %v, хочу 0", key, got)
		}
	}
	if _, ok := db["errors"]; ok {
		t.Errorf("на здоровой базе появились ошибки: %+v", db)
	}
}

// Провал чтения судейской базы обязан быть назван, а не раствориться в нуле: ноль
// голосов и «таблицу не удалось прочитать» - разные состояния, и молчание о
// втором превращает диагностику в обман. Обе причины перечисляются, потому что
// сбор не прерывается на первой ошибке.
func TestStatsToolReportsJudgeReadFailure(t *testing.T) {
	dir := t.TempDir()
	st, c := newTestServerInDir(t, dir)
	initClient(t, c)
	seedMCPBase(t, st, dir)
	dropMCPTable(t, dir, "relevance")

	out := callToolJSON(t, c, "stats")
	db, ok := out["db"].(map[string]any)
	if !ok {
		t.Fatalf("в ответе нет блока db: %v", out)
	}
	requireProblem(t, db, "судьи")
	requireProblem(t, db, "бонусы хостов")
	if found := problemsOf(t, db); len(found) != 2 {
		t.Errorf("собрано %d проблем, ожидала 2: %v", len(found), found)
	}
	// Остальные источники остаются точными: частичная поломка не повод обнулять
	// всё остальное.
	if n, _ := db["files_total"].(float64); n != 2 {
		t.Errorf("files_total = %v, ожидала 2", db["files_total"])
	}

	// Ресурс-двойник обязан показывать то же: он собирается тем же dbSummary, и
	// расхождение означало бы две разные картины одного состояния.
	res := readResourceJSON(t, c, "voidsearch://stats/summary")
	resDB, ok := res["db"].(map[string]any)
	if !ok {
		t.Fatalf("в ресурсе нет блока db: %v", res)
	}
	requireProblem(t, resDB, "судьи")
	requireProblem(t, resDB, "бонусы хостов")
}
