package store

import (
	"context"
	"fmt"
	"testing"
)

// Потолок хранилища открыт вызывающим: CLI проверяет --limit до запроса и
// называет предел в отказе, поэтому число не может оставаться приватным. Живой
// замер до правки на HEAD 330ebde, копия боевой базы, tor выключен,
// probe --limit N --json: 3 дало total 3, 20 дало total 20, 0 дало total 50,
// -5 дало total 50, 5001 дало total 5000, 20000 дало total 5000, stderr пуст во
// всех шести прогонах.
func TestMaxStoreLimitIsOpenConstant(t *testing.T) {
	if MaxStoreLimit != 5000 {
		t.Errorf("MaxStoreLimit = %d, хочу 5000: на этом числе снят замер", MaxStoreLimit)
	}
	if got := normLimit(MaxStoreLimit*2, 50); got != MaxStoreLimit {
		t.Errorf("normLimit сверх потолка дал %d, хочу %d", got, MaxStoreLimit)
	}
	if got := normLimit(MaxStoreLimit, 50); got != MaxStoreLimit {
		t.Errorf("normLimit на потолке дал %d, хочу %d", got, MaxStoreLimit)
	}
	if got := normLimit(0, 50); got != 50 {
		t.Errorf("normLimit при нуле дал %d, хочу дефолт 50", got)
	}
}

// Выборка волны не превышает потолок, каким бы большим ни было
// запрошенное значение: именно эту обрезку CLI теперь не пропускает молча.
func TestNextUnprobedNeverExceedsStoreLimit(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		o := Onion{
			URL:    fmt.Sprintf("http://unkn%04dhostaaaa.onion/", i),
			Title:  fmt.Sprintf("unknown host %d", i),
			Status: "unknown",
		}
		if err := st.UpsertOnion(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	for _, limit := range []int{MaxStoreLimit * 2, MaxStoreLimit, 10} {
		got, err := st.NextProbeWave(ctx, limit)
		if err != nil {
			t.Fatalf("NextProbeWave(limit=%d): %v", limit, err)
		}
		if len(got) > MaxStoreLimit {
			t.Errorf("NextProbeWave(limit=%d) вернул %d строк, потолок %d", limit, len(got), MaxStoreLimit)
		}
		if len(got) != 10 {
			t.Errorf("NextProbeWave(limit=%d) вернул %d строк, хочу все 10", limit, len(got))
		}
	}
	partial, err := st.NextProbeWave(ctx, 4)
	if err != nil {
		t.Fatalf("NextProbeWave(limit=4): %v", err)
	}
	if len(partial) != 4 {
		t.Errorf("NextProbeWave(limit=4) вернул %d строк, хочу 4", len(partial))
	}
}
