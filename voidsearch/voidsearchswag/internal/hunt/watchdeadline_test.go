package hunt

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// blockingSearch - заглушка, которая уважает контекст так же, как реальное
// ядро: поиск идёт через net/http, и соединение без ответа прерывается отменой
// контекста, а не висит вечно.
func blockingSearch(ctx context.Context, _, _ string, _ int) (SearchOutcome, error) {
	<-ctx.Done()
	return SearchOutcome{}, ctx.Err()
}

// Живой замер ДО на копии базы %TEMP%\vss\livedata103: hunt watch --id 2
// --timeout 5s при транспорте static с прокси 192.0.2.1:9999 (dial без ответа)
// длился 15190 мс и вернул elapsed=14.854s вместо заявленных пяти секунд.
// Дедлайн ожидания проверялся только между прогонами, а контекст прогона жил до
// внешнего запаса timeout+минута, который CLI даёт намеренно, чтобы ожидание
// выходило красивым путём Timeout=true. В итоге один медленный поиск
// перешагивал --timeout втрое, и скрипт, рассчитывающий на срок ожидания,
// висел вместе с командой.
func TestWatchDetailedStopsAtDeadline(t *testing.T) {
	st := openStore(t)
	r := &Runner{Store: st, Search: blockingSearch}
	base := context.Background()
	id, err := r.Create(base, "leak", "deep", 0)
	if err != nil {
		t.Fatal(err)
	}

	// Внешний запас повторяет формулу CLI (timeout плюс минута), но сжат до
	// секунды, чтобы непроверенная мутация стоила прогону секунду, а не минуту.
	// Смысл тот же: без ограничения прогона дедлайном ожидания заглушка держала
	// бы вызов до внешнего срока.
	const timeout = 200 * time.Millisecond
	wctx, cancel := context.WithTimeout(base, timeout+time.Second)
	defer cancel()

	start := time.Now()
	rep, err := r.WatchDetailed(wctx, id, timeout, 50*time.Millisecond)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("ожидание вернуло ошибку вместо таймаута: %v", err)
	}
	if !rep.Timeout {
		t.Error("таймаут не отмечен, хотя прогон оборван дедлайном ожидания")
	}
	if rep.Polls < 1 {
		t.Errorf("опросов %d, ожидала хотя бы один", rep.Polls)
	}
	if elapsed > 700*time.Millisecond {
		t.Errorf("ожидание длилось %s при timeout 200ms: прогон перешагнул дедлайн", elapsed)
	}
	if rep.Failed < 1 {
		t.Errorf("оборванный прогон не учтён как отказ: %+v", rep)
	}
	if !strings.Contains(rep.LastError, "истёк срок ожидания") {
		t.Errorf("причина %q не объясняет, что прогон оборван по сроку ожидания", rep.LastError)
	}
	if strings.Contains(rep.LastError, "context deadline exceeded") {
		t.Errorf("в отчёт ушёл служебный текст пакета context: %q", rep.LastError)
	}
	if rep.Elapsed == "" {
		t.Error("длительность не заполнена")
	}
}

// Обратная сторона: если вызывающий дал срок короче --timeout, прогон
// обрывается по внешнему дедлайну, и ожидание возвращает ошибку контекста, а не
// Timeout=true. Так работают MCP-вызовы со своим общим сроком.
func TestWatchDetailedKeepsShorterExternalDeadline(t *testing.T) {
	st := openStore(t)
	r := &Runner{Store: st, Search: blockingSearch}
	base := context.Background()
	id, err := r.Create(base, "leak", "deep", 0)
	if err != nil {
		t.Fatal(err)
	}

	wctx, cancel := context.WithTimeout(base, 150*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err = r.WatchDetailed(wctx, id, 5*time.Second, 50*time.Millisecond)
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("внешний дедлайн вернул %v, ожидала context.DeadlineExceeded", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("ожидание длилось %s при внешнем сроке 150ms", elapsed)
	}
}

// Последний шаг ожидания обрезается остатком до дедлайна: без обрезки сон
// длился бы полный интервал и выводил команду за заявленный срок даже тогда,
// когда поиск отвечает исправно.
//
// Прогон намеренно занимает заметную часть срока - 120 мс из 200 мс. Остаток до
// дедлайна тогда 80 мс, и шаг обязан обрезаться до него. С мгновенным прогоном
// мутация «не обрезать шаг» была бы неразличима: остаток и интервал совпадали, и
// обе редакции заканчивали ожидание за те же 200 мс.
func TestWatchDetailedLastStepDoesNotOverrunDeadline(t *testing.T) {
	slow := func(ctx context.Context, _, _ string, _ int) (SearchOutcome, error) {
		select {
		case <-ctx.Done():
			return SearchOutcome{}, ctx.Err()
		case <-time.After(120 * time.Millisecond):
			return SearchOutcome{URLs: []string{"http://a.onion"}, EnginesTotal: 1}, nil
		}
	}
	r := &Runner{Store: openStore(t), Search: slow}
	base := context.Background()
	id, err := r.Create(base, "leak", "deep", 60)
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	rep, err := r.WatchDetailed(base, id, 200*time.Millisecond, 200*time.Millisecond)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Timeout {
		t.Error("таймаут не отмечен при стабильной выдаче")
	}
	if rep.Polls < 1 {
		t.Errorf("опросов %d, ожидала хотя бы один", rep.Polls)
	}
	if elapsed > 280*time.Millisecond {
		t.Errorf("ожидание длилось %s при timeout 200ms и шаге 200ms: шаг не обрезан по остатку", elapsed)
	}
}

// Обрыв по сроку не затирает настоящую причину отказа движков: счётчик «2 из 2»
// говорит оператору, что опрашивать было некого, а обрыв объясняет, почему они не
// ответили. Живой замер ПОСЛЕ на копии базы %TEMP%\vss\livedata103 показал при
// elapsed=5s текст «все движки поиска отказали (2 из 2)» - то есть движки
// винились за то, что окно ожидания закрыл сам вызывающий.
func TestWatchDetailedKeepsEngineReasonWhenCutByDeadline(t *testing.T) {
	st := openStore(t)
	r := &Runner{Store: st, Search: func(ctx context.Context, _, _ string, _ int) (SearchOutcome, error) {
		<-ctx.Done()
		// Ядро в таком положении возвращает не ошибку контекста, а отчёт о
		// мёртвых движках: так выглядит живой отказ через прокси.
		return SearchOutcome{EnginesTotal: 2, EnginesFailed: 2}, nil
	}}
	base := context.Background()
	id, err := r.Create(base, "leak", "deep", 0)
	if err != nil {
		t.Fatal(err)
	}

	wctx, cancel := context.WithTimeout(base, time.Second)
	defer cancel()

	rep, err := r.WatchDetailed(wctx, id, 200*time.Millisecond, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("ожидание вернуло ошибку: %v", err)
	}
	if !rep.Timeout {
		t.Error("таймаут не отмечен")
	}
	if !strings.Contains(rep.LastError, "истёк срок ожидания") {
		t.Errorf("причина %q не называет обрыв по сроку ожидания", rep.LastError)
	}
	if !strings.Contains(rep.LastError, "2 из 2") {
		t.Errorf("причина %q потеряла счётчик движков", rep.LastError)
	}
	if strings.Count(rep.LastError, "истёк срок ожидания") != 1 {
		t.Errorf("причина %q дублирует обрыв на каждом опросе", rep.LastError)
	}
}

// runDeadline разбирается напрямую: ветка «внешний дедлайн короче» возвращает
// сам контекст, иначе проверка мутацией «всегда создавать дочерний» проходила бы
// незамеченной, ведь поведение по срокам от этого не меняется.
func TestRunDeadline(t *testing.T) {
	base := context.Background()

	// Без внешнего дедлайна создаётся дочерний контекст с нашим сроком.
	want := time.Now().Add(time.Second)
	rc, cancel := runDeadline(base, want)
	got, ok := rc.Deadline()
	cancel()
	if !ok || !got.Equal(want) {
		t.Errorf("дедлайн прогона = %v (ok=%v), хочу %v", got, ok, want)
	}

	// Внешний дедлайн короче - возвращается сам контекст, без лишнего потомка.
	short, cancelShort := context.WithTimeout(base, 100*time.Millisecond)
	defer cancelShort()
	rc2, cancel2 := runDeadline(short, time.Now().Add(time.Hour))
	if rc2 != short {
		t.Error("короткий внешний дедлайн подменён новым контекстом")
	}
	cancel2()

	// Внешний дедлайн длиннее - действует срок ожидания.
	long, cancelLong := context.WithTimeout(base, time.Hour)
	defer cancelLong()
	wantShort := time.Now().Add(200 * time.Millisecond)
	rc3, cancel3 := runDeadline(long, wantShort)
	got3, ok3 := rc3.Deadline()
	cancel3()
	if !ok3 || !got3.Equal(wantShort) {
		t.Errorf("дедлайн прогона = %v (ok=%v), хочу %v", got3, ok3, wantShort)
	}
}
