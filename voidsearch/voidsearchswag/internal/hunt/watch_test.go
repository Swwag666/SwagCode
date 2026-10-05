package hunt

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestWatchDetailedMissingDeps(t *testing.T) {
	if _, err := (&Runner{Search: searchStub(func() []string { return nil })}).
		WatchDetailed(context.Background(), 1, time.Second, time.Second); err == nil {
		t.Error("без хранилища ожидание принято")
	}
	if _, err := (&Runner{Store: openStore(t)}).WatchDetailed(context.Background(), 1, time.Second, time.Second); err == nil {
		t.Error("без поиска ожидание принято")
	}
}

// stableRunner возвращает одну и ту же выдачу: hash не меняется, находок нет.
func stableRunner(t *testing.T, urls []string) *Runner {
	t.Helper()
	now := time.Now()
	return &Runner{
		Store:  openStore(t),
		Now:    func() time.Time { return now },
		Search: searchStub(func() []string { return urls }),
	}
}

func TestWatchDetailedTimeoutReportsPollsAndCount(t *testing.T) {
	// Главный смысл диагностики: пустые hits при таймауте неотличимы от
	// сломанной охоты. Вызов MCP возвращал hits: [], timeout: true, и понять
	// причину было нельзя.
	r := stableRunner(t, []string{"http://a.onion", "http://b.onion"})
	ctx := context.Background()
	id, err := r.Create(ctx, "leak", "deep", 60)
	if err != nil {
		t.Fatal(err)
	}

	rep, err := r.WatchDetailed(ctx, id, 250*time.Millisecond, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Timeout {
		t.Error("таймаут не отмечен")
	}
	if len(rep.Hits) != 0 {
		t.Errorf("находки при стабильной выдаче: %+v", rep.Hits)
	}
	if rep.Polls < 2 {
		t.Errorf("опросов %d, ожидала несколько за 250мс с шагом 50мс", rep.Polls)
	}
	if rep.LastCount != 2 {
		t.Errorf("последняя выдача %d, ожидала 2", rep.LastCount)
	}
	if rep.Elapsed == "" {
		t.Error("длительность не заполнена")
	}
}

func TestWatchDetailedDistinguishesEmptyFromStable(t *testing.T) {
	// Пустая выдача и стабильная выдача дают одинаковый список находок, но
	// это разные ситуации: первая означает, что поиск ничего не находит.
	stable := stableRunner(t, []string{"http://a.onion"})
	ctx := context.Background()
	id, err := stable.Create(ctx, "q", "deep", 60)
	if err != nil {
		t.Fatal(err)
	}
	stableRep, err := stable.WatchDetailed(ctx, id, 150*time.Millisecond, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}

	empty := stableRunner(t, nil)
	eid, err := empty.Create(ctx, "q", "deep", 60)
	if err != nil {
		t.Fatal(err)
	}
	emptyRep, err := empty.WatchDetailed(ctx, eid, 150*time.Millisecond, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}

	if stableRep.LastCount == emptyRep.LastCount {
		t.Errorf("случаи неразличимы: last_count у обоих %d", stableRep.LastCount)
	}
	if stableRep.Note() == emptyRep.Note() {
		t.Errorf("пояснения совпали: %q", stableRep.Note())
	}
	if !strings.Contains(emptyRep.Note(), "пуст") {
		t.Errorf("пояснение пустой охоты не говорит о пустоте: %q", emptyRep.Note())
	}
	if strings.Contains(stableRep.Note(), "пуст") {
		t.Errorf("стабильная охота названа пустой: %q", stableRep.Note())
	}
}

func TestWatchDetailedReturnsHitsOnChange(t *testing.T) {
	urls := []string{"http://a.onion"}
	now := time.Now()
	r := &Runner{Store: openStore(t), Now: func() time.Time { return now },
		Search: searchStub(func() []string { return urls })}
	ctx := context.Background()
	id, err := r.Create(ctx, "leak", "deep", 60)
	if err != nil {
		t.Fatal(err)
	}
	// Первый прогон фиксирует базу.
	if _, err := r.RunOne(ctx, id); err != nil {
		t.Fatal(err)
	}
	urls = []string{"http://a.onion", "http://c.onion"}

	rep, err := r.WatchDetailed(ctx, id, 2*time.Second, 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Timeout {
		t.Error("изменение не замечено, дождался таймаута")
	}
	if len(rep.Hits) != 1 {
		t.Fatalf("находок %d, ожидала 1", len(rep.Hits))
	}
	if rep.Hits[0].Count != 2 {
		t.Errorf("count=%d", rep.Hits[0].Count)
	}
	if rep.Polls < 1 {
		t.Errorf("опросов %d", rep.Polls)
	}
	if got := rep.Note(); got != "выдача изменилась" {
		t.Errorf("пояснение %q", got)
	}
}

func TestWatchDetailedRespectsCancel(t *testing.T) {
	// Отмена обязана прерывать ожидание там, где оно действительно ждёт: в
	// выборе между таймером следующего опроса и каналом контекста.
	//
	// Прежняя версия запускала ожидание в горутине, спала фиксированные 120
	// миллисекунд и отменяла контекст, рассчитывая, что к этому времени случится
	// несколько опросов. Под нагрузкой горутина не успевала стартовать: отмена
	// происходила до первого опроса, RunOne начинался с чтения списка охот и
	// возвращал ошибку отменённого контекста, а WatchDetailed выходил на проверке
	// этой ошибки, вообще не дойдя до выбора. Тест проходил, но проверял другое
	// поведение и переставал защищать целевое.
	//
	// Измерено по счётчикам покрытия. Спокойный прогон: блок case <-ctx.Done()
	// исполнен 1 раз, WatchDetailed 97,6%. Тот же тест под нагрузкой четырьмя
	// параллельными пакетами: блок исполнен 0 раз, WatchDetailed 92,9%, общее
	// покрытие пакета 88,1% вместо 89,6%. Из трёх прогонов под нагрузкой так
	// прошёл один.
	//
	// Теперь отмена привязана к реальным опросам: два сигнала из поиска означают,
	// что цикл уже прошёл через таймер, и только после этого контекст
	// отменяется. Проверка ужесточена до errors.Is(err, context.Canceled),
	// потому что прежнее err != nil принимало любую ошибку, включая отказ базы.
	polled := make(chan struct{}, 8)
	now := time.Now()
	r := &Runner{
		Store: openStore(t),
		Now:   func() time.Time { return now },
		Search: searchStub(func() []string {
			select {
			case polled <- struct{}{}:
			default:
			}
			return []string{"http://a.onion"}
		}),
	}
	ctx := context.Background()
	id, err := r.Create(ctx, "leak", "deep", 60)
	if err != nil {
		t.Fatal(err)
	}

	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		// Интервал намеренно крупный: пять секунд. Если отмена обрабатывается
		// каналом контекста, ожидание выйдет сразу; если нет - только на
		// следующем опросе, то есть через пять секунд. С мелким интервалом эти
		// два случая неразличимы, и тест пропустил бы удаление ветки.
		_, err := r.WatchDetailed(cctx, id, 2*time.Minute, 5*time.Second)
		done <- err
	}()

	// Create поиск не вызывает, поэтому оба сигнала пришли из ожидания.
	for i := 0; i < 2; i++ {
		select {
		case <-polled:
		case <-time.After(60 * time.Second):
			t.Fatal("ожидание не сделало двух опросов")
		}
	}
	cancel()
	cancelled := time.Now()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("отмена вернула %v, ожидала context.Canceled", err)
		}
		// Порог в две секунды при интервале пять секунд разделяет выход по
		// каналу контекста и выход на следующем опросе.
		if took := time.Since(cancelled); took > 2*time.Second {
			t.Errorf("отмена обработана за %v: ожидание прервалось не по каналу контекста, а на следующем опросе", took)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("ожидание не остановилось по отмене")
	}
}

func TestWatchParamsDefaultsAndClamps(t *testing.T) {
	// Проверяем подстановку напрямую: реальный прогон с нулевыми параметрами
	// ждал бы две минуты ради тех же утверждений.
	cases := []struct {
		inTO, inInt   time.Duration
		wantTO, wantI time.Duration
	}{
		{0, 0, 2 * time.Minute, 30 * time.Second},
		{-time.Second, -time.Second, 2 * time.Minute, 30 * time.Second},
		{time.Minute, 0, time.Minute, 30 * time.Second},
		{time.Minute, time.Hour, time.Minute, time.Minute},
		{5 * time.Second, 10 * time.Second, 5 * time.Second, 5 * time.Second},
		{30 * time.Second, 10 * time.Second, 30 * time.Second, 10 * time.Second},
	}
	for _, c := range cases {
		to, iv := watchParams(c.inTO, c.inInt)
		if to != c.wantTO || iv != c.wantI {
			t.Errorf("watchParams(%v,%v) = (%v,%v), ожидала (%v,%v)",
				c.inTO, c.inInt, to, iv, c.wantTO, c.wantI)
		}
	}
}

func TestWatchDetailedClampedIntervalTimesOut(t *testing.T) {
	r := stableRunner(t, []string{"http://a.onion"})
	ctx := context.Background()
	id, err := r.Create(ctx, "leak", "deep", 60)
	if err != nil {
		t.Fatal(err)
	}
	// Интервал больше таймаута прижимается к нему, поэтому ровно один опрос
	// и сразу таймаут - ждать бесконечно вызывающий не должен.
	rep, err := r.WatchDetailed(ctx, id, 120*time.Millisecond, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Timeout {
		t.Error("интервал больше таймаута не прижат")
	}
	if rep.Polls < 1 {
		t.Errorf("опросов %d", rep.Polls)
	}
}

func TestWatchDetailedRunDueMode(t *testing.T) {
	// id <= 0 включает режим «все охоты». Расписание обязано игнорироваться:
	// до исправления RunDue пропускал охоты по плановому интервалу (360 минут
	// по умолчанию), поэтому ожидание опрашивало пул один раз и дальше до
	// самого таймаута получало «пропущено» - заметить изменение было нельзя.
	r := stableRunner(t, []string{"http://a.onion"})
	ctx := context.Background()
	if _, err := r.Create(ctx, "leak", "deep", 60); err != nil {
		t.Fatal(err)
	}
	rep, err := r.WatchDetailed(ctx, 0, 250*time.Millisecond, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Polls < 2 {
		t.Fatalf("опросов %d, ожидала несколько", rep.Polls)
	}
	if rep.Checked != rep.Polls {
		t.Errorf("проверок %d при %d опросах: расписание всё ещё блокирует ожидание: %+v",
			rep.Checked, rep.Polls, rep)
	}
	if !rep.Timeout {
		t.Error("стабильная выдача должна дать таймаут")
	}
}

func TestWatchDetailedRunAllIgnoresSchedule(t *testing.T) {
	// RunDue обязан сохранять прежнее поведение для плановых прогонов, иначе
	// фоновый тик начнёт дёргать поиск на каждом вызове вместо расписания.
	r := stableRunner(t, []string{"http://a.onion"})
	ctx := context.Background()
	if _, err := r.Create(ctx, "leak", "deep", 60); err != nil {
		t.Fatal(err)
	}
	first, err := r.RunDue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first.Checked != 1 {
		t.Fatalf("первый плановый прогон: %+v", first)
	}
	second, err := r.RunDue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if second.Checked != 0 || second.Skipped != 1 {
		t.Errorf("расписание не сработало в плановом режиме: %+v", second)
	}
	all, err := r.RunAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if all.Checked != 1 {
		t.Errorf("RunAll не проигнорировал расписание: %+v", all)
	}
}

func TestWatchStillWorksAsBefore(t *testing.T) {
	// Обёртка обязана сохранять прежний контракт: находки и флаг таймаута.
	r := stableRunner(t, []string{"http://a.onion"})
	ctx := context.Background()
	id, err := r.Create(ctx, "leak", "deep", 60)
	if err != nil {
		t.Fatal(err)
	}
	hits, timedOut, err := r.Watch(ctx, id, 150*time.Millisecond, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !timedOut {
		t.Error("таймаут не передан через обёртку")
	}
	if len(hits) != 0 {
		t.Errorf("находки %+v", hits)
	}
}

func TestWatchNoteNoPolls(t *testing.T) {
	rep := WatchReport{}
	if got := rep.Note(); got != "охота ни разу не прогнана" {
		t.Errorf("пояснение %q", got)
	}
}

func TestWatchNoteStable(t *testing.T) {
	rep := WatchReport{Polls: 3, LastCount: 5, Timeout: true}
	got := rep.Note()
	if !strings.Contains(got, "стабильн") {
		t.Errorf("пояснение %q", got)
	}
}
