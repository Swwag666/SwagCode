package mcpserver

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"voidsearchswag/internal/store"
)

// toolCaller - то, что нужно тесту от клиента: один вызов инструмента.
type toolCaller interface {
	CallTool(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error)
}

func judgeServer(t *testing.T) (toolCaller, *store.Store) {
	t.Helper()
	st, eng, _ := promoteDeps(t, engineStub())
	srv := New(Deps{Version: "test", Store: st, Search: eng, Started: time.Now()})
	c := inProcess(t, srv)
	initClient(t, c)
	return c, st
}

func callJudge(t *testing.T, c toolCaller, args map[string]any) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "judge_submit", Arguments: args},
	})
	if err != nil {
		t.Fatalf("judge_submit: %v", err)
	}
	if res.IsError {
		t.Fatalf("judge_submit вернул ошибку: %s", textOf(t, res))
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(textOf(t, res)), &out); err != nil {
		t.Fatalf("ответ не разобран: %v\n%s", err, textOf(t, res))
	}
	return out
}

func numOf(t *testing.T, out map[string]any, key string) float64 {
	t.Helper()
	v, ok := out[key]
	if !ok {
		t.Fatalf("в ответе нет поля %s: %+v", key, out)
	}
	n, ok := v.(float64)
	if !ok {
		t.Fatalf("поле %s не число: %v (%T)", key, v, v)
	}
	return n
}

// Пачка обязана сходиться: accepted плюс skipped равны числу присланных
// элементов. Прежний учёт терял голоса с пустым url - они не попадали ни в
// accepted, ни в skipped, потому что отбрасывались уже в базе.
func TestJudgeSubmitAccountingAddsUp(t *testing.T) {
	c, st := judgeServer(t)
	out := callJudge(t, c, map[string]any{
		"query": "accounting probe",
		"votes": []any{
			map[string]any{"url": "https://a.example/1", "score": 0.9},
			map[string]any{"url": "https://a.example/2", "score": 0.8},
			map[string]any{"url": "https://a.example/3", "score": 0.7},
			map[string]any{"url": "https://a.example/4", "score": 0.6},
			map[string]any{"url": "", "score": 1.0},
			map[string]any{"url": "   ", "score": 0.5},
			map[string]any{"url": "https://b.example/big", "score": 5.0},
			map[string]any{"url": "https://c.example/neg", "score": -2.0},
			map[string]any{"url": "https://d.example/str", "score": "0.42"},
			map[string]any{"url": "https://e.example/bad", "score": "не число"},
			map[string]any{"url": "https://f.example/nil", "score": nil},
			"вообще не объект",
		},
		"author": "accountant",
	})

	total := numOf(t, out, "votes_total")
	if total != 12 {
		t.Errorf("votes_total = %v, хочу 12", total)
	}
	accepted := numOf(t, out, "accepted")
	skipped := numOf(t, out, "skipped")
	if accepted+skipped != total {
		t.Errorf("учёт не сходится: accepted %v + skipped %v != votes_total %v (%+v)",
			accepted, skipped, total, out)
	}
	if accepted != 7 {
		t.Errorf("accepted = %v, хочу 7", accepted)
	}
	if skipped != 5 {
		t.Errorf("skipped = %v, хочу 5", skipped)
	}
	if got := numOf(t, out, "skipped_no_url"); got != 2 {
		t.Errorf("skipped_no_url = %v, хочу 2", got)
	}
	if got := numOf(t, out, "skipped_bad_score"); got != 2 {
		t.Errorf("skipped_bad_score = %v, хочу 2", got)
	}
	if got := numOf(t, out, "skipped_not_object"); got != 1 {
		t.Errorf("skipped_not_object = %v, хочу 1", got)
	}
	if got := numOf(t, out, "new_rows"); got != 7 {
		t.Errorf("new_rows = %v, хочу 7", got)
	}
	if got := numOf(t, out, "stored_rows"); got != 7 {
		t.Errorf("stored_rows = %v, хочу 7", got)
	}
	if got := numOf(t, out, "stored_authors"); got != 1 {
		t.Errorf("stored_authors = %v, хочу 1", got)
	}

	// Ответ не должен расходиться с базой: число строк, которое видит клиент,
	// равно числу строк, которое видит хранилище.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	votes, queries, _, err := st.VoteStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if votes != int(accepted) {
		t.Errorf("в базе %d голосов, accepted = %v", votes, accepted)
	}
	if queries != 1 {
		t.Errorf("запросов в базе %d, хочу 1", queries)
	}
}

// Оценка вне шкалы приводится к границе, и клиент обязан об этом узнать: 5.0
// превращается в голос «лучший результат» с бонусом +3 в реранке, а -2.0 - в
// штраф -3. Молчаливое приведение выглядело как мнение судьи, которого он не
// высказывал.
func TestJudgeSubmitReportsClamping(t *testing.T) {
	c, _ := judgeServer(t)
	out := callJudge(t, c, map[string]any{
		"query": "clamping probe",
		"votes": []any{
			map[string]any{"url": "https://high.example/1", "score": 5.0},
			map[string]any{"url": "https://low.example/1", "score": -2.0},
			map[string]any{"url": "https://ok.example/1", "score": 0.5},
		},
		"author": "clamp-judge",
	})
	if got := numOf(t, out, "clamped"); got != 2 {
		t.Errorf("clamped = %v, хочу 2", got)
	}
	if _, ok := out["clamped_reason"]; !ok {
		t.Errorf("в ответе нет причины приведения: %+v", out)
	}
	if got := numOf(t, out, "accepted"); got != 3 {
		t.Errorf("приведённая оценка не должна теряться: accepted = %v, хочу 3", got)
	}

	// Этап 178 (смоук-K/J): на чистой пачке счётчиков событий НЕТ - контракт
	// описания «присутствуют только при событии: при чистой пачке их нет,
	// а не нули». До правки handler клал clamped и skipped_* всегда, и
	// чистая пачка приходила с нулевыми ключами против собственного
	// описания инструмента.
	clean := callJudge(t, c, map[string]any{
		"query": "clean probe",
		"votes": []any{
			map[string]any{"url": "https://clean.example/1", "score": 0.4},
		},
		"author": "clamp-judge",
	})
	for _, key := range []string{"clamped", "clamped_reason", "skipped_no_url", "skipped_bad_score", "skipped_not_object", "skipped", "skipped_reason"} {
		if _, ok := clean[key]; ok {
			t.Errorf("ключ %q присутствует на чистой пачке: события не было - ключа быть не должно (%+v)", key, clean)
		}
	}
}

// Повторная отправка той же пачки тем же автором обновляет строки, а не
// добавляет их. Без new_rows «accepted: 2» было неотличимо от «добавлено два
// голоса», и клиент, повторяющий отправку после сбоя, считал базу вдвое объёмнее.
func TestJudgeSubmitRepeatUpdatesInsteadOfGrowing(t *testing.T) {
	c, _ := judgeServer(t)
	args := map[string]any{
		"query": "repeat probe",
		"votes": []any{
			map[string]any{"url": "https://a.example/1", "score": 0.9},
			map[string]any{"url": "https://a.example/2", "score": 0.2},
		},
		"author": "repeat-judge",
	}

	first := callJudge(t, c, args)
	if got := numOf(t, first, "new_rows"); got != 2 {
		t.Errorf("первая отправка: new_rows = %v, хочу 2", got)
	}
	if got := numOf(t, first, "stored_rows"); got != 2 {
		t.Errorf("первая отправка: stored_rows = %v, хочу 2", got)
	}

	second := callJudge(t, c, args)
	if got := numOf(t, second, "accepted"); got != 2 {
		t.Errorf("повторная отправка: accepted = %v, хочу 2", got)
	}
	if got := numOf(t, second, "new_rows"); got != 0 {
		t.Errorf("повторная отправка: new_rows = %v, хочу 0", got)
	}
	if got := numOf(t, second, "updated_rows"); got != 2 {
		t.Errorf("повторная отправка: updated_rows = %v, хочу 2", got)
	}
	if got := numOf(t, second, "stored_rows"); got != 2 {
		t.Errorf("повторная отправка: stored_rows = %v, хочу 2", got)
	}
}

// stored_authors показывает, сколькими людьми набраны голоса: одна строка на
// автора при том же url, и объём базы без этого числа выглядит весомее, чем он
// есть по числу судей.
func TestJudgeSubmitShowsStoredAuthors(t *testing.T) {
	c, _ := judgeServer(t)
	votes := []any{map[string]any{"url": "https://shared.example/1", "score": 0.9}}

	callJudge(t, c, map[string]any{"query": "authors probe", "votes": votes, "author": "first"})
	out := callJudge(t, c, map[string]any{"query": "authors probe", "votes": votes, "author": "second"})

	if got := numOf(t, out, "stored_rows"); got != 2 {
		t.Errorf("stored_rows = %v, хочу 2: голоса разных авторов не схлопываются", got)
	}
	if got := numOf(t, out, "stored_authors"); got != 2 {
		t.Errorf("stored_authors = %v, хочу 2", got)
	}

	// Третий голос от первого автора добавляет строку, но не автора: без этого
	// случая строки и авторы совпадают по числам, и подмена COUNT(DISTINCT
	// author) на COUNT(*) осталась бы незамеченной.
	more := callJudge(t, c, map[string]any{
		"query":  "authors probe",
		"votes":  []any{map[string]any{"url": "https://shared.example/2", "score": 0.3}},
		"author": "first",
	})
	if got := numOf(t, more, "stored_rows"); got != 3 {
		t.Errorf("stored_rows = %v, хочу 3", got)
	}
	if got := numOf(t, more, "stored_authors"); got != 2 {
		t.Errorf("stored_authors = %v, хочу 2: число авторов не равно числу строк", got)
	}
}

// Голоса одного запроса не должны утекать в другой: stored_rows считается по
// query_hash, а не по всей таблице.
func TestJudgeSubmitStoredIsScopedToQuery(t *testing.T) {
	c, _ := judgeServer(t)
	callJudge(t, c, map[string]any{
		"query":  "first query",
		"votes":  []any{map[string]any{"url": "https://a.example/1", "score": 0.9}},
		"author": "scope-judge",
	})
	out := callJudge(t, c, map[string]any{
		"query":  "second query",
		"votes":  []any{map[string]any{"url": "https://a.example/1", "score": 0.1}},
		"author": "scope-judge",
	})
	if got := numOf(t, out, "stored_rows"); got != 1 {
		t.Errorf("stored_rows = %v, хочу 1: в счёт попал чужой запрос", got)
	}
	if got := numOf(t, out, "new_rows"); got != 1 {
		t.Errorf("new_rows = %v, хочу 1", got)
	}
}
