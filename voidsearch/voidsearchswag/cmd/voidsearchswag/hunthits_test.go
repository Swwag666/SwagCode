package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"voidsearchswag/internal/config"
	"voidsearchswag/internal/store"
)

// seedHuntHistory заводит охоту и пишет в её историю находки тем же путём, каким
// это делает прогон: команда hunt hits читает стор, поэтому заполнять базу нужно
// через SaveHuntHits, а не вставками в обход боевого кода.
func seedHuntHistory(t *testing.T, query, mode string, rounds [][]string) int64 {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("конфиг: %v", err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatalf("база: %v", err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("миграции: %v", err)
	}
	id, err := st.CreateHunt(ctx, store.Hunt{Query: query, Mode: mode, ScheduleMin: 60})
	if err != nil {
		t.Fatalf("охота: %v", err)
	}
	for _, urls := range rounds {
		if _, err := st.SaveHuntHits(ctx, id, query, mode, urls); err != nil {
			t.Fatalf("история: %v", err)
		}
	}
	return id
}

func TestCmdHuntHitsPrintsHistory(t *testing.T) {
	t.Setenv("VOIDSEARCH_DATA_DIR", t.TempDir())
	id := seedHuntHistory(t, "leak database", "fast", [][]string{
		{"http://a.onion/x", "http://b.onion/y"},
		{"http://b.onion/y", "http://c.onion/z"},
	})
	out, code := runMain(t, "hunt", "hits", "--id", fmt.Sprint(id))
	if code != 0 {
		t.Fatalf("rc=%d, хочу 0: %s", code, out)
	}
	for _, want := range []string{"http://a.onion/x", "http://b.onion/y", "http://c.onion/z",
		"история находок: 3 строк", "[leak database/fast]", "охота " + fmt.Sprint(id)} {
		if !strings.Contains(out, want) {
			t.Errorf("нет %q в выводе:\n%s", want, out)
		}
	}
	// Счётчик повторов обязан быть виден: по нему отличают адрес, который
	// кочует из выдачи в выдачу, от одноразовой находки.
	if !strings.Contains(out, "x2") {
		t.Errorf("нет счётчика повторов x2 в выводе:\n%s", out)
	}
}

func TestCmdHuntHitsReadsAllHunts(t *testing.T) {
	t.Setenv("VOIDSEARCH_DATA_DIR", t.TempDir())
	seedHuntHistory(t, "leak", "fast", [][]string{{"http://a.onion/x"}})
	seedHuntHistory(t, "breach", "stealth", [][]string{{"http://b.onion/y"}})
	out, code := runMain(t, "hunt", "hits")
	if code != 0 {
		t.Fatalf("rc=%d, хочу 0: %s", code, out)
	}
	for _, want := range []string{"http://a.onion/x", "http://b.onion/y", "[breach/stealth]", "история находок: 2 строк"} {
		if !strings.Contains(out, want) {
			t.Errorf("нет %q в выводе:\n%s", want, out)
		}
	}
}

func TestCmdHuntHitsExplainsEmptyHistory(t *testing.T) {
	t.Setenv("VOIDSEARCH_DATA_DIR", t.TempDir())
	id := seedHuntHistory(t, "leak", "fast", nil)
	out, code := runMain(t, "hunt", "hits", "--id", fmt.Sprint(id))
	if code != 0 {
		t.Fatalf("rc=%d, хочу 0: %s", code, out)
	}
	// Пустой ответ обязан говорить, что находок не было, а не выглядеть как
	// «история не читается».
	if !strings.Contains(out, fmt.Sprintf("находок у охоты %d нет", id)) ||
		!strings.Contains(out, "выдача ещё не менялась") {
		t.Errorf("пустая история не объяснена:\n%s", out)
	}

	all, code := runMain(t, "hunt", "hits")
	if code != 0 {
		t.Fatalf("rc=%d, хочу 0: %s", code, all)
	}
	if !strings.Contains(all, "находок нет ни у одной охоты") {
		t.Errorf("пустая история по всем охотам не объяснена:\n%s", all)
	}
}

func TestCmdHuntHitsJSON(t *testing.T) {
	t.Setenv("VOIDSEARCH_DATA_DIR", t.TempDir())
	id := seedHuntHistory(t, "leak", "fast", [][]string{
		{"http://a.onion/x"},
		{"http://b.onion/y"},
	})
	out, code := runMain(t, "hunt", "hits", "--id", fmt.Sprint(id), "--json")
	if code != 0 {
		t.Fatalf("rc=%d, хочу 0: %s", code, out)
	}
	var payload struct {
		HuntID int64 `json:"hunt_id"`
		Count  int   `json:"count"`
		Limit  int   `json:"limit"`
		Hits   []struct {
			URL       string `json:"url"`
			HuntID    int64  `json:"hunt_id"`
			Query     string `json:"query"`
			Mode      string `json:"mode"`
			TimesSeen int    `json:"times_seen"`
		} `json:"hits"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("не JSON: %v\n%s", err, out)
	}
	if payload.HuntID != id || payload.Count != 2 || len(payload.Hits) != 2 {
		t.Fatalf("payload=%+v", payload)
	}
	if payload.Limit != 50 {
		t.Errorf("limit=%d, хочу дефолтные 50", payload.Limit)
	}
	if payload.Hits[0].URL == "" || payload.Hits[0].Query != "leak" || payload.Hits[0].Mode != "fast" {
		t.Errorf("строка истории потеряла поля: %+v", payload.Hits[0])
	}
	if payload.Hits[0].TimesSeen < 1 {
		t.Errorf("times_seen=%d", payload.Hits[0].TimesSeen)
	}
}

func TestCmdHuntHitsLimitWarnsAboutTruncation(t *testing.T) {
	t.Setenv("VOIDSEARCH_DATA_DIR", t.TempDir())
	id := seedHuntHistory(t, "leak", "fast", [][]string{
		{"http://a.onion/1", "http://a.onion/2", "http://a.onion/3",
			"http://a.onion/4", "http://a.onion/5"},
	})
	out, code := runMain(t, "hunt", "hits", "--id", fmt.Sprint(id), "--limit", "2")
	if code != 0 {
		t.Fatalf("rc=%d, хочу 0: %s", code, out)
	}
	if !strings.Contains(out, "история находок: 2 строк") {
		t.Errorf("limit не применён:\n%s", out)
	}
	// Обрезанный список без предупреждения читается как «находок всего две».
	if !strings.Contains(out, "подними --limit") {
		t.Errorf("нет предупреждения об обрезке:\n%s", out)
	}
	if strings.Contains(out, "http://a.onion/3") {
		t.Errorf("limit=2 отдал больше строк:\n%s", out)
	}
	full, code := runMain(t, "hunt", "hits", "--id", fmt.Sprint(id), "--limit", "5")
	if code != 0 {
		t.Fatalf("rc=%d, хочу 0: %s", code, full)
	}
	if !strings.Contains(full, "http://a.onion/5") {
		t.Errorf("полный список не отдан:\n%s", full)
	}
}

func TestCmdHuntHitsClear(t *testing.T) {
	t.Setenv("VOIDSEARCH_DATA_DIR", t.TempDir())
	id := seedHuntHistory(t, "leak", "fast", [][]string{
		{"http://a.onion/x", "http://b.onion/y"},
	})
	out, code := runMain(t, "hunt", "hits", "--clear", "--id", fmt.Sprint(id))
	if code != 0 {
		t.Fatalf("rc=%d, хочу 0: %s", code, out)
	}
	if !strings.Contains(out, fmt.Sprintf("история охоты %d очищена", id)) ||
		!strings.Contains(out, "удалено 2 строк") {
		t.Errorf("чистка не отчиталась:\n%s", out)
	}
	after, code := runMain(t, "hunt", "hits", "--id", fmt.Sprint(id))
	if code != 0 {
		t.Fatalf("rc=%d, хочу 0: %s", code, after)
	}
	if strings.Contains(after, "http://a.onion/x") {
		t.Errorf("история не стёрта:\n%s", after)
	}
}

func TestCmdHuntHitsClearJSON(t *testing.T) {
	t.Setenv("VOIDSEARCH_DATA_DIR", t.TempDir())
	id := seedHuntHistory(t, "leak", "fast", [][]string{{"http://a.onion/x"}})
	out, code := runMain(t, "hunt", "hits", "--clear", "--id", fmt.Sprint(id), "--json")
	if code != 0 {
		t.Fatalf("rc=%d, хочу 0: %s", code, out)
	}
	var payload struct {
		HuntID  int64 `json:"hunt_id"`
		Deleted int   `json:"deleted"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("не JSON: %v\n%s", err, out)
	}
	if payload.HuntID != id || payload.Deleted != 1 {
		t.Errorf("payload=%+v", payload)
	}
}

// Чистка без id стёрла бы историю всех охот одной командой: слишком дорогой
// побочный эффект для вызова по памяти, поэтому он обязан быть отказом.
func TestCmdHuntHitsClearRequiresID(t *testing.T) {
	t.Setenv("VOIDSEARCH_DATA_DIR", t.TempDir())
	seedHuntHistory(t, "leak", "fast", [][]string{{"http://a.onion/x"}})
	out, code := runMain(t, "hunt", "hits", "--clear")
	if code == 0 {
		t.Fatalf("rc=0 при --clear без --id:\n%s", out)
	}
	if !strings.Contains(out, "нужен --id") {
		t.Errorf("отказ не объяснён:\n%s", out)
	}
	after, _ := runMain(t, "hunt", "hits")
	if !strings.Contains(after, "http://a.onion/x") {
		t.Errorf("история стёрта отказом:\n%s", after)
	}
}

func TestCmdHuntHitsClearUnknownHunt(t *testing.T) {
	t.Setenv("VOIDSEARCH_DATA_DIR", t.TempDir())
	seedHuntHistory(t, "leak", "fast", [][]string{{"http://a.onion/x"}})
	out, code := runMain(t, "hunt", "hits", "--clear", "--id", "999")
	if code == 0 {
		t.Fatalf("rc=0 при чистке несуществующей охоты:\n%s", out)
	}
	// Молчаливый ноль при опечатке в id неотличим от пустой истории.
	if !strings.Contains(out, "запись не найдена") {
		t.Errorf("нет ErrNotFound в сообщении:\n%s", out)
	}
	after, _ := runMain(t, "hunt", "hits")
	if !strings.Contains(after, "http://a.onion/x") {
		t.Errorf("чужая история пострадала:\n%s", after)
	}
}

func TestCmdHuntSubcommandHelpMentionsHits(t *testing.T) {
	t.Setenv("VOIDSEARCH_DATA_DIR", t.TempDir())
	out, code := runMain(t, "hunt", "nosuch")
	if code == 0 {
		t.Fatalf("rc=0 при неизвестной подкоманде:\n%s", out)
	}
	if !strings.Contains(out, "create|list|run|watch|hits") {
		t.Errorf("список подкоманд не обновлён:\n%s", out)
	}
	bare, code := runMain(t, "hunt")
	if code == 0 {
		t.Fatalf("rc=0 без подкоманды:\n%s", bare)
	}
	if !strings.Contains(bare, "create|list|run|watch|hits") {
		t.Errorf("подсказка без подкоманды не обновлена:\n%s", bare)
	}
	usage, _ := runMain(t, "-h")
	if !strings.Contains(usage, "create|list|run|watch|hits") {
		t.Errorf("справка не упоминает hits:\n%s", usage)
	}
}
