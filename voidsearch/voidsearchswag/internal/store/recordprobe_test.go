package store

import (
	"context"
	"math"
	"sync"
	"testing"
)

// seedOnion вставляет строку пула напрямую, минуя RecordProbe: тестам нужно
// заданное стартовое состояние, а не результат предыдущих проб.
func seedOnion(t *testing.T, st *Store, url, status string, latency int64, rate float64, streak int) {
	t.Helper()
	_, err := st.db.ExecContext(context.Background(), `
		INSERT INTO onion_pool(url, status, latency_avg, success_rate, fail_streak, last_probe)
		VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`, url, status, latency, rate, streak)
	if err != nil {
		t.Fatalf("посев %s: %v", url, err)
	}
}

func TestRecordProbeConcurrentSameURL(t *testing.T) {
	// Главный дефект: прежняя версия читала строку через GetOnion, считала новые
	// значения в Go и писала обратно. Две конкурентные пробы одного адреса читали
	// одно и то же старое состояние, и вклад одной терялся при последней записи.
	//
	// probe работает с ProbeConcurrency=16, то есть конкурентные пробы - штатный
	// режим. Разные адреса не конфликтовали, поэтому дефект проявлялся редко и
	// выглядел как «статистика немного неточна».
	//
	// Проверка: N одновременных неудачных проб обязаны дать fail_streak ровно N.
	// При потере обновлений счётчик был бы меньше, и адрес не перешёл бы в
	// «dead» после трёх провалов.
	st := newStore(t)
	ctx := context.Background()

	const url = "http://2222222222222222222222222222222222222222222222222222.onion"
	seedOnion(t, st, url, "live", 100, 1.0, 0)

	const n = 16
	var wg sync.WaitGroup
	errs := make([]error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			errs[i] = st.RecordProbe(ctx, url, false, 0)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("проба %d: %v", i, err)
		}
	}

	got, err := st.GetOnion(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if got.FailStreak != n {
		t.Errorf("fail_streak = %d, ожидала %d: конкурентные пробы потеряли обновления", got.FailStreak, n)
	}
	if got.Status != "dead" {
		t.Errorf("status = %q, ожидала dead после %d провалов", got.Status, n)
	}
}

func TestRecordProbeConcurrentSuccessAccumulates(t *testing.T) {
	// Успешные пробы обязаны накапливаться, а не перезаписывать друг друга:
	// success_rate растёт к единице, latency_avg остаётся осмысленной.
	st := newStore(t)
	ctx := context.Background()

	const url = "http://2222222222222222222222222222222222222222222222222222.onion"
	seedOnion(t, st, url, "dead", 0, 0.0, 5)

	const n = 20
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if err := st.RecordProbe(ctx, url, true, 100); err != nil {
				t.Errorf("проба: %v", err)
			}
		}()
	}
	wg.Wait()

	got, err := st.GetOnion(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if got.FailStreak != 0 {
		t.Errorf("fail_streak = %d, ожидала 0 после успеха", got.FailStreak)
	}
	if got.Status != "live" {
		t.Errorf("status = %q, ожидала live", got.Status)
	}
	// 20 применений (rate*9+1)/10 к нулю дают 1 - 0.9^20 ≈ 0.878.
	if got.SuccessRate < 0.8 {
		t.Errorf("success_rate = %f: обновления потерялись, ожидала выше 0.8", got.SuccessRate)
	}
	if got.LatencyAvg != 100 {
		t.Errorf("latency_avg = %d, ожидала 100 при одинаковых пробах", got.LatencyAvg)
	}
}

func TestRecordProbeConcurrentMixedURLs(t *testing.T) {
	// Смешанная нагрузка: разные адреса пишутся параллельно, и строки не должны
	// влиять друг на друга.
	st := newStore(t)
	ctx := context.Background()

	const urls = 8
	const perURL = 5
	var wg sync.WaitGroup
	for u := 0; u < urls; u++ {
		url := "http://222222222222222222222222222222222222222222222222222" + string(rune('a'+u)) + ".onion"
		seedOnion(t, st, url, "unknown", 0, 0, 0)
		for i := 0; i < perURL; i++ {
			wg.Add(1)
			go func(url string, ok bool) {
				defer wg.Done()
				if err := st.RecordProbe(ctx, url, ok, 50); err != nil {
					t.Errorf("проба %s: %v", url, err)
				}
			}(url, i%2 == 0)
		}
	}
	wg.Wait()

	pool, err := st.ListOnions(ctx, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(pool) != urls {
		t.Fatalf("в пуле %d адресов, ожидала %d", len(pool), urls)
	}
	for _, o := range pool {
		// Последняя проба каждого адреса была неудачной при чётном i=4, но
		// порядок конкурентных записей не определён, поэтому проверяем только
		// инварианты: счётчик не превышает числа проб и статус согласован.
		if o.FailStreak < 0 || o.FailStreak > perURL {
			t.Errorf("%s: fail_streak = %d вне диапазона", o.URL, o.FailStreak)
		}
		if o.SuccessRate < 0 || o.SuccessRate > 1 {
			t.Errorf("%s: success_rate = %f вне [0,1]", o.URL, o.SuccessRate)
		}
		if o.LatencyAvg < 0 {
			t.Errorf("%s: latency_avg = %d отрицательная", o.URL, o.LatencyAvg)
		}
	}
}

func TestRecordProbeLatencyClamped(t *testing.T) {
	// Переполнение: (старое*3 + новое)/4 заворачивает int64 в отрицательное
	// число при значении выше 3.07e18 мс. Отрицательная латентность не проходит
	// условие latency_avg > 0, поэтому ветвь сглаживания отключалась и колонка
	// залипала на сыром значении - пул отравлялся навсегда.
	st := newStore(t)
	ctx := context.Background()

	const url = "http://2222222222222222222222222222222222222222222222222222.onion"
	// Стартовое значение заведомо выше порога, как испорченная строка.
	seedOnion(t, st, url, "live", math.MaxInt64/2, 1.0, 0)

	if err := st.RecordProbe(ctx, url, true, 50); err != nil {
		t.Fatal(err)
	}

	got, err := st.GetOnion(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if got.LatencyAvg < 0 {
		t.Errorf("latency_avg = %d: переполнение дало отрицательное значение", got.LatencyAvg)
	}
	if got.LatencyAvg > maxLatencyMS {
		t.Errorf("latency_avg = %d выше порога %d", got.LatencyAvg, maxLatencyMS)
	}
}

func TestRecordProbeHugeIncomingLatencyClamped(t *testing.T) {
	// Огромное входящее значение тоже обязано зажиматься, иначе первая же
	// испорченная проба отравила бы строку.
	st := newStore(t)
	ctx := context.Background()

	const url = "http://2222222222222222222222222222222222222222222222222222.onion"
	if err := st.RecordProbe(ctx, url, true, math.MaxInt64); err != nil {
		t.Fatal(err)
	}

	got, err := st.GetOnion(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if got.LatencyAvg != maxLatencyMS {
		t.Errorf("latency_avg = %d, ожидала зажим до %d", got.LatencyAvg, maxLatencyMS)
	}
}

func TestRecordProbeNegativeLatencyClamped(t *testing.T) {
	// Отрицательная латентность возможна при сбое таймера или переводе часов;
	// без зажима она отравила бы сглаживание так же, как переполнение.
	st := newStore(t)
	ctx := context.Background()

	const url = "http://2222222222222222222222222222222222222222222222222222.onion"
	if err := st.RecordProbe(ctx, url, true, -5000); err != nil {
		t.Fatal(err)
	}

	got, err := st.GetOnion(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if got.LatencyAvg != 0 {
		t.Errorf("latency_avg = %d, ожидала 0 после зажима отрицательного", got.LatencyAvg)
	}
}

func TestRecordProbeSmoothingUnchanged(t *testing.T) {
	// Перенос арифметики в SQL не должен менять формулу: (старое*3 + новое)/4.
	st := newStore(t)
	ctx := context.Background()

	const url = "http://2222222222222222222222222222222222222222222222222222.onion"
	seedOnion(t, st, url, "live", 100, 1.0, 0)

	if err := st.RecordProbe(ctx, url, true, 200); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetOnion(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	var want int64 = (100*3 + 200) / 4 // 125
	if got.LatencyAvg != want {
		t.Errorf("latency_avg = %d, ожидала %d по формуле сглаживания", got.LatencyAvg, want)
	}
}

func TestRecordProbeFirstLatencyNotSmoothed(t *testing.T) {
	// При нулевой старой латентности сглаживание неприменимо: иначе первый
	// ответ дал бы latency/4 и адрес выглядел бы вчетверо быстрее реального.
	st := newStore(t)
	ctx := context.Background()

	const url = "http://2222222222222222222222222222222222222222222222222222.onion"
	seedOnion(t, st, url, "unknown", 0, 0, 0)

	if err := st.RecordProbe(ctx, url, true, 300); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetOnion(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if got.LatencyAvg != 300 {
		t.Errorf("latency_avg = %d, ожидала 300 без сглаживания", got.LatencyAvg)
	}
}

func TestRecordProbeFailureKeepsLatency(t *testing.T) {
	// Провал не измеряет латентность ответа, поэтому портить накопленное
	// значение он не должен: иначе три неудачи подряд обнулили бы статистику
	// скорости живого адреса.
	st := newStore(t)
	ctx := context.Background()

	const url = "http://2222222222222222222222222222222222222222222222222222.onion"
	seedOnion(t, st, url, "live", 250, 1.0, 0)

	if err := st.RecordProbe(ctx, url, false, 0); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetOnion(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if got.LatencyAvg != 250 {
		t.Errorf("latency_avg = %d, ожидала сохранённые 250", got.LatencyAvg)
	}
	if got.FailStreak != 1 {
		t.Errorf("fail_streak = %d, ожидала 1", got.FailStreak)
	}
	if got.Status != "live" {
		t.Errorf("status = %q: одна неудача не должна убивать адрес", got.Status)
	}
}

func TestRecordProbeDeadAfterThreeFailures(t *testing.T) {
	// Порог «dead» обязан срабатывать ровно на третьей неудаче подряд, и
	// вычисляться от состояния строки до обновления: SQLite вычисляет все
	// присваивания SET против старых значений, поэтому проверка идёт по
	// fail_streak + 1.
	st := newStore(t)
	ctx := context.Background()

	const url = "http://2222222222222222222222222222222222222222222222222222.onion"
	seedOnion(t, st, url, "live", 100, 1.0, 0)

	for i := 1; i <= 3; i++ {
		if err := st.RecordProbe(ctx, url, false, 0); err != nil {
			t.Fatal(err)
		}
		got, err := st.GetOnion(ctx, url)
		if err != nil {
			t.Fatal(err)
		}
		if got.FailStreak != i {
			t.Errorf("после %d провалов fail_streak = %d", i, got.FailStreak)
		}
		wantStatus := "live"
		if i >= 3 {
			wantStatus = "dead"
		}
		if got.Status != wantStatus {
			t.Errorf("после %d провалов status = %q, ожидала %q", i, got.Status, wantStatus)
		}
	}
}

func TestRecordProbeSuccessRevivesDeadAddress(t *testing.T) {
	// Успех после серии провалов обнуляет счётчик и возвращает адрес в «live»:
	// иначе адрес оставался бы мёртвым навсегда.
	st := newStore(t)
	ctx := context.Background()

	const url = "http://2222222222222222222222222222222222222222222222222222.onion"
	seedOnion(t, st, url, "dead", 0, 0.1, 7)

	if err := st.RecordProbe(ctx, url, true, 80); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetOnion(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if got.FailStreak != 0 {
		t.Errorf("fail_streak = %d, ожидала 0", got.FailStreak)
	}
	if got.Status != "live" {
		t.Errorf("status = %q, ожидала live", got.Status)
	}
}

func TestRecordProbeNewAddressInserted(t *testing.T) {
	// probe --addr принимает любой валидный onion, а не только ранее собранный.
	// Терять результат нельзя: иначе адрес так и не появился бы в пуле, и
	// следующий прогон проверил бы его заново с нуля.
	st := newStore(t)
	ctx := context.Background()

	const url = "http://2222222222222222222222222222222222222222222222222222.onion"

	if err := st.RecordProbe(ctx, url, true, 120); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetOnion(ctx, url)
	if err != nil {
		t.Fatalf("новый адрес не появился в пуле: %v", err)
	}
	if got.Status != "live" {
		t.Errorf("status = %q, ожидала live", got.Status)
	}
	if got.LatencyAvg != 120 {
		t.Errorf("latency_avg = %d, ожидала 120", got.LatencyAvg)
	}
	if got.FailStreak != 0 {
		t.Errorf("fail_streak = %d, ожидала 0", got.FailStreak)
	}
	// (0*9 + 1)/10 = 0.1
	if got.SuccessRate != 0.1 {
		t.Errorf("success_rate = %f, ожидала 0.1", got.SuccessRate)
	}
}

func TestRecordProbeNewAddressFailure(t *testing.T) {
	// Первый же провал нового адреса: streak=1, статус остаётся «unknown»,
	// потому что одной неудачи мало, чтобы объявить адрес мёртвым.
	st := newStore(t)
	ctx := context.Background()

	const url = "http://2222222222222222222222222222222222222222222222222222.onion"

	if err := st.RecordProbe(ctx, url, false, 0); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetOnion(ctx, url)
	if err != nil {
		t.Fatalf("новый адрес не появился в пуле: %v", err)
	}
	if got.Status != "unknown" {
		t.Errorf("status = %q, ожидала unknown", got.Status)
	}
	if got.FailStreak != 1 {
		t.Errorf("fail_streak = %d, ожидала 1", got.FailStreak)
	}
	if got.SuccessRate != 0 {
		t.Errorf("success_rate = %f, ожидала 0", got.SuccessRate)
	}
}

func TestRecordProbeSuccessRateDecays(t *testing.T) {
	// Доля успехов обязана затухать при провалах: иначе адрес с одной удачной
	// пробой из ста оставался бы в пуле как надёжный.
	st := newStore(t)
	ctx := context.Background()

	const url = "http://2222222222222222222222222222222222222222222222222222.onion"
	seedOnion(t, st, url, "live", 100, 1.0, 0)

	for i := 0; i < 10; i++ {
		if err := st.RecordProbe(ctx, url, false, 0); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.GetOnion(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	// 0.9^10 ≈ 0.349
	if got.SuccessRate > 0.4 {
		t.Errorf("success_rate = %f: затухание не применилось", got.SuccessRate)
	}
	if got.SuccessRate < 0.3 {
		t.Errorf("success_rate = %f: затухание слишком агрессивное", got.SuccessRate)
	}
}

func TestRecordProbeEmptyURL(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	for _, u := range []string{"", "   ", "\t"} {
		if err := st.RecordProbe(ctx, u, true, 10); err == nil {
			t.Errorf("пустой url %q принят без ошибки", u)
		}
	}
}

func TestRecordProbeTrimsURL(t *testing.T) {
	// Адрес с пробелами обязан попасть в ту же строку пула, что и чистый: иначе
	// один адрес дал бы две записи с раздельной статистикой.
	st := newStore(t)
	ctx := context.Background()

	const url = "http://2222222222222222222222222222222222222222222222222222.onion"
	if err := st.RecordProbe(ctx, "  "+url+"  ", true, 100); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordProbe(ctx, url, true, 100); err != nil {
		t.Fatal(err)
	}

	pool, err := st.ListOnions(ctx, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pool) != 1 {
		t.Errorf("в пуле %d записей, ожидала 1 (пробелы создали дубль)", len(pool))
	}
}

func TestRecordProbeDoesNotReturnNotFound(t *testing.T) {
	// Прежняя версия возвращала ErrNotFound из GetOnion при ошибке чтения.
	// Теперь чтения нет, и запись нового адреса не должна сообщать «не найдено»:
	// вызывающий трактует это как отсутствие адреса и теряет результат пробы.
	st := newStore(t)
	ctx := context.Background()

	err := st.RecordProbe(ctx, "http://2222222222222222222222222222222222222222222222222222.onion", true, 10)
	if err != nil {
		t.Errorf("запись нового адреса вернула ошибку: %v", err)
	}
}
