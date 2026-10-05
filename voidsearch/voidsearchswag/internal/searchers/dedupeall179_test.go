package searchers

import (
	"fmt"
	"testing"
)

// TestDedupeAllKeepsFullPool - кэтчер этапа 179: дедуп движкового пула не
// режет выдачу. Срез по лимиту - работа вызывающего ПОСЛЕ реранка и фильтра
// шума; прежний Dedupe(all, limit) внутри searchDeep отдавал монополию
// первому движку окна (BEFORE: «market», tornet ok=19, torch ok=25, limit=15,
// выдача - 7, все tornet).
func TestDedupeAllKeepsFullPool(t *testing.T) {
	in := make([]Result, 0, 90)
	for i := 0; i < 60; i++ {
		in = append(in, Result{
			URL:    fmt.Sprintf("https://pool%d.example/%d", i, i),
			Title:  fmt.Sprintf("pool result %d", i),
			Source: "s1",
			Rank:   i + 1,
		})
	}
	// Дубли по ключу NormalizeURL: та же выборка с другим написанием хоста.
	for i := 0; i < 30; i++ {
		in = append(in, Result{
			URL:    fmt.Sprintf("https://www.pool%d.example/%d", i, i),
			Source: "s2",
		})
	}
	out := DedupeAll(in)
	if len(out) != 60 {
		t.Fatalf("DedupeAll вернула %d результатов, ожидала 60 (дедуп без среза)", len(out))
	}
	seen := map[string]bool{}
	for i, r := range out {
		if seen[r.URL] {
			t.Errorf("дубль прошёл дедуп: %s", r.URL)
		}
		seen[r.URL] = true
		if r.Rank != i+1 {
			t.Errorf("rank=%d на позиции %d, ожидала плотный ряд", r.Rank, i)
		}
	}
}

// TestDedupeLimitStillCuts - старый срез не сломан: Dedupe с лимитом режет.
func TestDedupeLimitStillCuts(t *testing.T) {
	in := make([]Result, 0, 60)
	for i := 0; i < 60; i++ {
		in = append(in, Result{URL: fmt.Sprintf("https://cut%d.example/%d", i, i)})
	}
	out := Dedupe(in, 10)
	if len(out) != 10 {
		t.Fatalf("Dedupe(in, 10) вернула %d, ожидала 10", len(out))
	}
}
