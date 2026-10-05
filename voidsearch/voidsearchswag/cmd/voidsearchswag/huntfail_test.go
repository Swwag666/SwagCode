package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"voidsearchswag/internal/hunt"
	"voidsearchswag/internal/store"
)

func TestHuntTickWarnsAboutSearchFailures(t *testing.T) {
	// Фоновый тик молчал при отказавшем поиске: RunDue возвращал nil, находок
	// нет, и huntTick выходил, не сказав ни слова. Охоты переставали работать,
	// а лог оставался пустым - то есть фон выглядел здоровым ровно тогда, когда
	// tor лёг. Предупреждение обязано печататься и без находок.
	st, err := store.Open(t.TempDir() + "/bg.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := &hunt.Runner{Store: st, Search: func(context.Context, string, string, int) (hunt.SearchOutcome, error) {
		return hunt.SearchOutcome{}, errors.New("tor мёртв")
	}}
	ctx := context.Background()
	if _, err := r.Create(ctx, "leak", "deep", 0); err != nil {
		t.Fatal(err)
	}

	notified := false
	out := captureStderr(t, func() {
		huntTick(ctx, stderrLogger{}, r, func([]hunt.Hit) { notified = true })
	})
	if notified {
		t.Error("notify вызван при отказавшем поиске: находок не было")
	}
	if !strings.Contains(out, "отказов поиска") {
		t.Errorf("в логе нет предупреждения об отказах: %q", out)
	}
	if !strings.Contains(out, "tor мёртв") {
		t.Errorf("в логе нет причины отказа: %q", out)
	}
}

func TestHuntTickStaysSilentWhenSearchWorks(t *testing.T) {
	// Обратная сторона: предупреждение не должно появляться, когда поиск
	// отвечает. Иначе каждый штатный тик шумел бы в лог.
	r := openHuntRunner(t, []string{"http://a.onion"})
	ctx := context.Background()
	if _, err := r.Create(ctx, "leak", "deep", 1); err != nil {
		t.Fatal(err)
	}
	out := captureStderr(t, func() {
		huntTick(ctx, stderrLogger{}, r, nil)
	})
	if strings.Contains(out, "отказов поиска") {
		t.Errorf("предупреждение при работающем поиске: %q", out)
	}
}
