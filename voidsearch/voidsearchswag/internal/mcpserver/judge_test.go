package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

func TestJudgeSubmitAcceptsVotes(t *testing.T) {
	st, eng, _ := promoteDeps(t, engineStub())
	srv := New(Deps{Version: "test", Store: st, Search: eng, Started: time.Now()})
	c := inProcess(t, srv)
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "judge_submit", Arguments: map[string]any{
			"query": "leak database",
			"votes": []any{
				map[string]any{"url": "https://good.example/leak", "score": 1.0},
				map[string]any{"url": "https://junk.example/x", "score": 0.0},
				map[string]any{"url": "", "score": 1.0},
			},
			"author": "test-judge",
		}},
	})
	if err != nil || res.IsError {
		t.Fatalf("judge_submit: %v %+v", err, res)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(textOf(t, res)), &out); err != nil {
		t.Fatal(err)
	}
	// Пустой url отброшен, два голоса приняты.
	if out["accepted"] != float64(2) {
		t.Errorf("принято %+v", out)
	}
	votes, queries, hosts, err := st.VoteStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if votes != 2 || queries != 1 || hosts != 2 {
		t.Errorf("статистика голосов: %d/%d/%d", votes, queries, hosts)
	}
}

func TestJudgeSubmitRequiresVotes(t *testing.T) {
	st, eng, _ := promoteDeps(t, engineStub())
	srv := New(Deps{Version: "test", Store: st, Search: eng, Started: time.Now()})
	c := inProcess(t, srv)
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "judge_submit", Arguments: map[string]any{"query": "x"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Error("submit без голосов обязан вернуть ошибку")
	}
}

func TestJudgeSubmitAcceptsStringScore(t *testing.T) {
	// Числовая оценка в виде строки - обычное дело для JS-клиентов MCP и
	// вызовов из шаблонов. Прежний switch не обрабатывал string, поэтому
	// {"score": "0.9"} не попадал ни в одну ветку, score оставался нулевым, и
	// голос записывался как 0.0 - наихудшая возможная оценка. HostQuality
	// задвигал такой хост штрафом до -3, и порча сохранялась в SQLite между
	// перезапусками, тихо ухудшая каждый последующий поиск.
	st, eng, _ := promoteDeps(t, engineStub())
	srv := New(Deps{Version: "test", Store: st, Search: eng, Started: time.Now()})
	c := inProcess(t, srv)
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "judge_submit", Arguments: map[string]any{
			"query": "string score probe",
			"votes": []any{
				map[string]any{"url": "https://a.example/1", "score": "0.9"},
				map[string]any{"url": "https://a.example/2", "score": "  0.25 "},
			},
			"author": "string-judge",
		}},
	})
	if err != nil || res.IsError {
		t.Fatalf("judge_submit: %v %+v", err, res)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(textOf(t, res)), &out); err != nil {
		t.Fatal(err)
	}
	if out["accepted"] != float64(2) {
		t.Errorf("строковые оценки не приняты: %+v", out)
	}
	if out["skipped"] != nil {
		t.Errorf("строковые оценки помечены пропущенными: %+v", out)
	}

	// Главное: оценка не должна была превратиться в ноль.
	q, err := st.HostQuality(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(q) == 0 {
		t.Fatal("голоса не дошли до таблицы relevance")
	}
	for host, bonus := range q {
		// Среднее 0.575 даёт положительный бонус; ноль дал бы отрицательный.
		if bonus <= 0 {
			t.Errorf("хост %s получил бонус %v: строковая оценка превратилась в ноль", host, bonus)
		}
	}
}

func TestJudgeSubmitSkipsUnparseableScore(t *testing.T) {
	// Неразобранная оценка пропускается, а не пишется нулём. Запись нуля была
	// бы голосом «худший результат» от имени судьи, который ничего подобного
	// не утверждал.
	st, eng, _ := promoteDeps(t, engineStub())
	srv := New(Deps{Version: "test", Store: st, Search: eng, Started: time.Now()})
	c := inProcess(t, srv)
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "judge_submit", Arguments: map[string]any{
			"query": "junk score probe",
			"votes": []any{
				map[string]any{"url": "https://ok.example/1", "score": 1.0},
				map[string]any{"url": "https://bad.example/1", "score": "не число"},
				map[string]any{"url": "https://bad.example/2", "score": nil},
				map[string]any{"url": "https://bad.example/3"},
				"вообще не объект",
			},
			"author": "junk-judge",
		}},
	})
	if err != nil || res.IsError {
		t.Fatalf("judge_submit: %v %+v", err, res)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(textOf(t, res)), &out); err != nil {
		t.Fatal(err)
	}
	if out["accepted"] != float64(1) {
		t.Errorf("принято %+v, ожидала 1", out["accepted"])
	}
	if out["skipped"] != float64(4) {
		t.Errorf("пропущено %+v, ожидала 4: молчаливый пропуск неотличим от отказа", out["skipped"])
	}
	votes, _, _, err := st.VoteStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if votes != 1 {
		t.Errorf("в базе %d голосов, ожидала 1: мусор записался", votes)
	}
}

func TestJudgeSubmitRejectsOversizedBatch(t *testing.T) {
	// Пачка больше потолка отклоняется целиком. Без границы массив из ста
	// тысяч элементов блокировал бы единственное соединение базы надолго.
	st, eng, _ := promoteDeps(t, engineStub())
	srv := New(Deps{Version: "test", Store: st, Search: eng, Started: time.Now()})
	c := inProcess(t, srv)
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	votes := make([]any, 0, maxVotes+1)
	for i := 0; i <= maxVotes; i++ {
		votes = append(votes, map[string]any{"url": "https://x.example/1", "score": 1.0})
	}
	res, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "judge_submit", Arguments: map[string]any{
			"query": "oversized probe", "votes": votes, "author": "flood",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("пачка сверх потолка принята")
	}
	if !strings.Contains(textOf(t, res), "максимум") {
		t.Errorf("ошибка не объясняет ограничение: %s", textOf(t, res))
	}
	n, _, _, err := st.VoteStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("отклонённая пачка частично записалась: %d голосов", n)
	}
}

func TestJudgePromptSubstitutes(t *testing.T) {
	_, c := newTestServer(t)
	initClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := c.GetPrompt(ctx, mcp.GetPromptRequest{
		Params: mcp.GetPromptParams{
			Name:      "judge",
			Arguments: map[string]string{"query": "leak database", "mode": "deep"},
		},
	})
	if err != nil {
		t.Fatalf("prompts/get judge: %v", err)
	}
	body := promptText(t, res)
	for _, want := range []string{"leak database", "режим deep", "judge_submit", "score 0..1"} {
		if !strings.Contains(body, want) {
			t.Errorf("в промпте нет %q", want)
		}
	}
}
