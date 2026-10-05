package store

import (
	"context"
	"fmt"
	"testing"
)

// Потолок истории обязан быть открытой константой, а не числом внутри запроса:
// CLI отклоняет --limit сверх него до прогона, и если предел здесь сдвинуть,
// проверка в команде начнёт расходиться с фактическим поведением хранилища.
// Замер до правки на HEAD e054dc4, копия боевой базы с историей в 1500 строк:
// «hunt hits --limit 5000 --json» дал rc=0, "limit": 5000 и "count": 1000.
func TestListHuntHitsCeilingIsMaxHuntHitsLimit(t *testing.T) {
	if MaxHuntHitsLimit <= 0 {
		t.Fatalf("MaxHuntHitsLimit = %d, хочу положительное число", MaxHuntHitsLimit)
	}
	st := newStore(t)
	ctx := context.Background()
	id := newHunt(t, st, "потолок истории", "fast")

	total := MaxHuntHitsLimit + 50
	urls := make([]string, 0, total)
	for i := 0; i < total; i++ {
		urls = append(urls, fmt.Sprintf("http://abcdefghijklmnop.onion/ceiling/%d", i))
	}
	saved, err := st.SaveHuntHits(ctx, id, "потолок истории", "fast", urls)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if saved != total {
		t.Fatalf("сохранено %d строк, хочу %d", saved, total)
	}

	got, err := st.ListHuntHits(ctx, id, total*2)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != MaxHuntHitsLimit {
		t.Errorf("запрос сверх потолка отдал %d строк, хочу ровно %d", len(got), MaxHuntHitsLimit)
	}

	exact, err := st.ListHuntHits(ctx, id, MaxHuntHitsLimit)
	if err != nil {
		t.Fatalf("list exact: %v", err)
	}
	if len(exact) != MaxHuntHitsLimit {
		t.Errorf("запрос ровно по потолку отдал %d строк, хочу %d", len(exact), MaxHuntHitsLimit)
	}
}
