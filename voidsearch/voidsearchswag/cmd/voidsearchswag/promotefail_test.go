package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"voidsearchswag/internal/httpc"
	"voidsearchswag/internal/promote"
	"voidsearchswag/internal/search"
	"voidsearchswag/internal/searchers"
	"voidsearchswag/internal/store"
)

// promoteBase создаёт базу с live-кандидатами, которые заведомо не проходят
// проверку: onion-адреса недостижимы без tor, а команда вызывается с --no-tor.
func promoteBase(t *testing.T, dir string, n int) {
	t.Helper()
	st, err := store.Open(filepath.Join(dir, "voidsearchswag.db"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		o := store.Onion{
			URL:    fmt.Sprintf("http://fail%04dpromotecandidate.onion/", i),
			Title:  fmt.Sprintf("кандидат %d", i),
			Status: "live",
		}
		if err := st.UpsertOnion(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPromoteCLIReportsFailedChecks(t *testing.T) {
	// До правки прогон печатал «проверено 6, поднято 0» и выглядел выводом о
	// пуле - движков не нашлось, - хотя ни одна проверка не состоялась.
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	promoteBase(t, dir, 3)

	out, code := runMain(t, "promote", "--no-tor", "-limit", "3")
	// Ни одна проверка не состоялась, поэтому прогон завершается единицей:
	// обвязка отличает провал по коду, а текст отчёта объясняет причину.
	if code != 1 {
		t.Fatalf("код возврата %d, хочу 1: %s", code, firstN(out, 300))
	}
	if !strings.Contains(out, "проверено 3, поднято 0") {
		t.Errorf("итоговая строка потеряна:\n%s", firstN(out, 300))
	}
	if !strings.Contains(out, "отказов 3 (проверок 3, сохранений 0) из 3 проверенных") {
		t.Errorf("отказы не названы:\n%s", firstN(out, 400))
	}
	if !strings.Contains(out, "tor") {
		t.Errorf("в причине нет объяснения про tor:\n%s", firstN(out, 400))
	}
}

func TestPromoteCLIJSONCarriesFailures(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	promoteBase(t, dir, 2)

	out, code := runMain(t, "promote", "--no-tor", "-limit", "2", "--json")
	// Машинный режим подчиняется тому же правилу: JSON печатается целиком, и
	// только потом процесс завершается единицей.
	if code != 1 {
		t.Fatalf("код возврата %d, хочу 1: %s", code, firstN(out, 300))
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("JSON не разобран: %v (%s)", err, firstN(out, 200))
	}
	if res["failed"] != float64(2) {
		t.Errorf("failed = %+v, ожидала 2: %+v", res["failed"], res)
	}
	if res["checked"] != float64(2) {
		t.Errorf("checked = %+v, ожидала 2", res["checked"])
	}
	if reason, _ := res["last_error"].(string); reason == "" {
		t.Errorf("в JSON нет причины отказа: %+v", res)
	}
	// promoted обязан быть пустым списком, а не null: MCP-инструмент отдаёт [],
	// и два формата одной команды не должны расходиться в типе поля.
	promoted, ok := res["promoted"].([]any)
	if !ok {
		t.Fatalf("promoted не список: %#v", res["promoted"])
	}
	if len(promoted) != 0 {
		t.Errorf("promoted = %d элементов при недоступных кандидатах", len(promoted))
	}
}

func TestPromoteCLISilentWhenNothingChecked(t *testing.T) {
	// Обратная сторона: оговорка об отказах при нуле проверок превратила бы
	// пустой пул в поломку.
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	promoteBase(t, dir, 0)

	out, code := runMain(t, "promote", "--no-tor", "-limit", "3")
	if code != 0 {
		t.Fatalf("код возврата %d: %s", code, firstN(out, 300))
	}
	if !strings.Contains(out, "проверено 0, поднято 0") {
		t.Errorf("итоговая строка потеряна:\n%s", firstN(out, 300))
	}
	if strings.Contains(out, "отказов") {
		t.Errorf("оговорка об отказах при пустом пуле:\n%s", firstN(out, 300))
	}
}

func TestPromoteTickWarnsAboutFailedChecks(t *testing.T) {
	// Фоновый тик не печатает итогов, поэтому отказ проверок терялся в нём
	// проще всего: регулярные падения выглядели как «движки просто не
	// находятся».
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "bg.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertOnion(ctx, store.Onion{URL: "http://failtickcandidate.onion/", Title: "тик", Status: "live"}); err != nil {
		t.Fatal(err)
	}
	cl, err := httpc.NewClient(ctx, httpc.Options{Transport: "direct", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	eng := &search.Engine{
		Client: cl,
		Onion:  &searchers.OnionCatalog{},
		Health: searchers.NewHealthPool(cl, nil),
	}
	p := &promote.Promoter{Client: cl, MinOnionHits: 2, Timeout: 5 * time.Second, Budget: time.Minute}

	text := captureStderr(t, func() {
		promoteTick(ctx, stderrLogger{}, st, eng, p, 3)
	})
	if !strings.Contains(text, "фон: промоут: отказов 1") {
		t.Errorf("фон промолчал об отказе проверки: %q", text)
	}
	if !strings.Contains(text, "из 1 проверенных") {
		t.Errorf("в предупреждении нет доли отказов: %q", text)
	}
}

func TestPromoteTickSilentWhenPoolEmpty(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "bgempty.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	cl, err := httpc.NewClient(ctx, httpc.Options{Transport: "direct", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	eng := &search.Engine{
		Client: cl,
		Onion:  &searchers.OnionCatalog{},
		Health: searchers.NewHealthPool(cl, nil),
	}
	p := &promote.Promoter{Client: cl, MinOnionHits: 2, Timeout: 5 * time.Second, Budget: time.Minute}

	text := captureStderr(t, func() {
		promoteTick(ctx, stderrLogger{}, st, eng, p, 3)
	})
	if strings.Contains(text, "отказов") {
		t.Errorf("фон сообщил об отказах при пустом пуле: %q", text)
	}
}
