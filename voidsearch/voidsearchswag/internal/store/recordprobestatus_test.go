package store

import (
	"context"
	"testing"
)

func TestRecordProbeStatusReturnsLiveOnSuccess(t *testing.T) {
	// Статус возвращается из той же атомарной записи через RETURNING, а не
	// повторным чтением: при ProbeConcurrency=16 и одном соединении к базе
	// чтение после записи могло вернуть состояние, изменённое чужой пробой.
	st := newStore(t)
	ctx := context.Background()

	const url = "http://2222222222222222222222222222222222222222222222222222.onion"
	status, err := st.RecordProbeStatus(ctx, url, true, 120)
	if err != nil {
		t.Fatal(err)
	}
	if status != "live" {
		t.Errorf("status = %q, ожидала live", status)
	}

	// Возвращённый статус обязан совпадать с тем, что лежит в базе.
	got, err := st.GetOnion(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != status {
		t.Errorf("RETURNING дал %q, в базе %q", status, got.Status)
	}
}

func TestRecordProbeStatusFirstFailureIsUnknown(t *testing.T) {
	// Ключевое расхождение, которое прежний вывод стирал: одна неудача нового
	// адреса даёт «unknown», а не «dead». Порог смерти - третья неудача подряд,
	// потому что одна случайная не должна выкидывать живой сервис из пула.
	st := newStore(t)
	ctx := context.Background()

	const url = "http://2222222222222222222222222222222222222222222222222222.onion"
	status, err := st.RecordProbeStatus(ctx, url, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if status != "unknown" {
		t.Errorf("после первой неудачи status = %q, ожидала unknown", status)
	}

	status, err = st.RecordProbeStatus(ctx, url, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if status != "unknown" {
		t.Errorf("после второй неудачи status = %q, ожидала unknown", status)
	}

	status, err = st.RecordProbeStatus(ctx, url, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if status != "dead" {
		t.Errorf("после третьей неудачи status = %q, ожидала dead", status)
	}
}

func TestRecordProbeStatusRevival(t *testing.T) {
	// Успех возвращает мёртвый адрес в «live»: иначе адрес оставался бы
	// исключённым навсегда, даже когда сервис ожил.
	st := newStore(t)
	ctx := context.Background()

	const url = "http://2222222222222222222222222222222222222222222222222222.onion"
	for i := 0; i < 3; i++ {
		if _, err := st.RecordProbeStatus(ctx, url, false, 0); err != nil {
			t.Fatal(err)
		}
	}

	status, err := st.RecordProbeStatus(ctx, url, true, 90)
	if err != nil {
		t.Fatal(err)
	}
	if status != "live" {
		t.Errorf("после успеха status = %q, ожидала live", status)
	}
}

func TestRecordProbeStatusEmptyURL(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	for _, u := range []string{"", "   ", "\t"} {
		status, err := st.RecordProbeStatus(ctx, u, true, 10)
		if err == nil {
			t.Errorf("пустой url %q принят без ошибки", u)
		}
		if status != "" {
			t.Errorf("при ошибке status = %q, ожидала пустую строку", status)
		}
	}
}

func TestRecordProbeWrapperMatchesStatusMethod(t *testing.T) {
	// RecordProbe остался обёрткой: 35 вызывающих довольствуются ошибкой, и
	// менять сигнатуру ради одного потребителя значило бы править все 35 мест.
	// Обёртка обязана вести себя ровно как прежде.
	st := newStore(t)
	ctx := context.Background()

	const url = "http://2222222222222222222222222222222222222222222222222222.onion"
	if err := st.RecordProbe(ctx, url, true, 150); err != nil {
		t.Fatal(err)
	}

	got, err := st.GetOnion(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "live" {
		t.Errorf("status = %q, ожидала live", got.Status)
	}
	if got.LatencyAvg != 150 {
		t.Errorf("latency_avg = %d, ожидала 150", got.LatencyAvg)
	}
}

func TestRecordProbeStatusClampsLikeWrapper(t *testing.T) {
	// Зажим латентности обязан работать в обоих методах одинаково: они делят один
	// запрос, и расхождение означало бы, что часть пути отравляет пул.
	st := newStore(t)
	ctx := context.Background()

	const a = "http://2222222222222222222222222222222222222222222222222222.onion"
	const b = "http://2222222222222222222222222222222222222222222222222223.onion"

	if _, err := st.RecordProbeStatus(ctx, a, true, -500); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordProbe(ctx, b, true, -500); err != nil {
		t.Fatal(err)
	}

	ga, err := st.GetOnion(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	gb, err := st.GetOnion(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	if ga.LatencyAvg != gb.LatencyAvg {
		t.Errorf("зажим разошёлся: %d против %d", ga.LatencyAvg, gb.LatencyAvg)
	}
	if ga.LatencyAvg != 0 {
		t.Errorf("latency_avg = %d, ожидала 0 после зажима отрицательного", ga.LatencyAvg)
	}
}

func TestRecordProbeStatusConcurrentConsistent(t *testing.T) {
	// При конкурентных пробах возвращённый статус обязан соответствовать
	// реальному состоянию строки, а не устаревшему снимку. Проверяю инвариант:
	// если последняя проба была успешной, адрес не может остаться «dead».
	st := newStore(t)
	ctx := context.Background()

	const url = "http://2222222222222222222222222222222222222222222222222222.onion"
	seedOnion(t, st, url, "unknown", 0, 0, 0)

	// Волна провалов, затем гарантированный успех последним.
	for i := 0; i < 5; i++ {
		if _, err := st.RecordProbeStatus(ctx, url, false, 0); err != nil {
			t.Fatal(err)
		}
	}
	status, err := st.RecordProbeStatus(ctx, url, true, 100)
	if err != nil {
		t.Fatal(err)
	}
	if status != "live" {
		t.Errorf("после успеха status = %q, ожидала live", status)
	}

	got, err := st.GetOnion(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != status {
		t.Errorf("RETURNING дал %q, в базе %q", status, got.Status)
	}
	if got.FailStreak != 0 {
		t.Errorf("fail_streak = %d, ожидала 0 после успеха", got.FailStreak)
	}
}
