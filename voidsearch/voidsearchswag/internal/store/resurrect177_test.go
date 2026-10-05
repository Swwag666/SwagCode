package store

import (
	"context"
	"testing"
)

// Этап 177, D1. Живой BEFORE (стенд vss177b, витрина 9348844): адрес
// 2b4smu пришёл из unknown с latency_avg=0, успешная проба 6906 - и
// latency_avg стал 6906 ровно, без сглаживания. Смоук-агенты 176-го
// (A: 3539, C: 3110) читали raw-число как «EMA обойдена». Формула верна
// (EMA от нуля занизила бы первую меру вчетверо) - не был назван сам
// договор. Контракт: воскрешение из нулевой латентности меряется целиком,
// сглаживание включается со второй точки.
func TestRecordProbeResurrectionLatencyWhole(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	const url = "http://2222222222222222222222222222222222222222222222222222.onion"
	seedOnion(t, st, url, "dead", 0, 0, 3)

	if err := st.RecordProbe(ctx, url, true, 3000); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetOnion(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "live" {
		t.Fatalf("status = %q, ожидала воскрешение в live", got.Status)
	}
	if got.LatencyAvg != 3000 {
		t.Errorf("latency_avg = %d, ожидала 3000 целиком: первая мера не сглаживается", got.LatencyAvg)
	}

	// Вторая точка включает вес четверти: (3000*3 + 1000)/4 = 2500.
	if err := st.RecordProbe(ctx, url, true, 1000); err != nil {
		t.Fatal(err)
	}
	got, err = st.GetOnion(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if got.LatencyAvg != 2500 {
		t.Errorf("latency_avg = %d, ожидала 2500 со сглаживанием со второй точки", got.LatencyAvg)
	}
}
