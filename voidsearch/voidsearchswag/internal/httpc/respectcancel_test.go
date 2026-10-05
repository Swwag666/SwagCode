package httpc

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// transportRefusal - отказ транспорта, который приходит в момент истечения срока.
// Текст взят из живого падения: socks-рукопожатие tls-client не смотрит на
// контекст запроса и обрывается собственным таймаутом сокета.
var transportRefusal = errors.New("socks connect tcp 127.0.0.1:1->example.com:443: i/o timeout")

// expiredContext возвращает контекст, срок которого гарантированно истёк до
// проверки. Ожидание - активный опрос с потолком, а не разовый сон: на
// Windows квант системного таймера 15.6ms, и разовый Sleep(20ms) просыпался
// в том же кванте, в котором срабатывал 2ms-таймер контекста; порядок внутри
// кванта произволен, и примерно один прогон из десяти под нагрузкой получал
// пустой ctx.Err - helper убивал замер, который собирался проверять.
func expiredContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Millisecond)
	t.Cleanup(cancel)
	giveUp := time.Now().Add(500 * time.Millisecond)
	for ctx.Err() == nil {
		if time.Now().After(giveUp) {
			t.Fatal("контекст не истёк за отведённое ожидание: замер ничего не проверит")
		}
		time.Sleep(time.Millisecond)
	}
	return ctx
}

// Замер до правки на HEAD 497cfd0. Живое падение поймал полный набор тестов:
// TestDoStdReturnsOnContextCancel получил «Get "https://example.com/": socks
// connect tcp 127.0.0.1:52857->example.com:443: read tcp
// 127.0.0.1:52858->127.0.0.1:52857: i/o timeout» вместо context.DeadlineExceeded и
// упал, тогда как в одиночном прогоне тот же тест трижды подряд прошёл.
//
// Причина в порядке готовности каналов: transport отвечает в тот же срок, в
// который истекает контекст, оба канала select готовы, и выбор между ними
// произволен. Ветка отмены возвращала context.DeadlineExceeded, ветка результата -
// сетевую ошибку, поэтому один и тот же прогон давал два разных ответа.
func TestCancelResultPrefersDeadlineErrorOverTransportError(t *testing.T) {
	ctx := expiredContext(t)

	resp, err := cancelResult(ctx, nil, transportRefusal)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("истёкший срок вернул %v вместо context.DeadlineExceeded", err)
	}
	if resp != nil {
		t.Errorf("вместе с ошибкой отмены вернулся ответ %+v", resp)
	}
}

func TestCancelResultPrefersCanceledErrorOverTransportError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := cancelResult(ctx, nil, transportRefusal)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("отменённый контекст вернул %v вместо context.Canceled", err)
	}
}

// Живой контекст не имеет права подменять настоящую причину отказа: иначе каждый
// сетевой сбой выглядел бы как отмена и чинить его искали бы не там.
func TestCancelResultKeepsTransportErrorWhileContextAlive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	_, err := cancelResult(ctx, nil, transportRefusal)
	if !errors.Is(err, transportRefusal) {
		t.Errorf("живой контекст подменил причину отказа: %v", err)
	}
}

// Подмена касается ошибки, а не результата: ответ, полученный до истечения срока,
// вызывающему нужнее, чем сообщение об отмене.
func TestCancelResultKeepsSuccessfulResponseOnExpiredContext(t *testing.T) {
	ctx := expiredContext(t)
	want := &Response{Status: 200, Body: []byte("ok")}

	got, err := cancelResult(ctx, want, nil)
	if err != nil {
		t.Errorf("успешный ответ вернулся с ошибкой %v", err)
	}
	if got != want {
		t.Errorf("вернулся другой ответ: %+v", got)
	}
}

// Ответ, пришедший вместе с ошибкой, не отдаётся: вызывающий проверяет ошибку и не
// станет читать тело, а половина результата оставила бы его в неведении о том, что
// данные просрочены.
func TestCancelResultDropsResponseWhenSubstitutingError(t *testing.T) {
	ctx := expiredContext(t)

	resp, err := cancelResult(ctx, &Response{Status: 200, Body: []byte("поздно")}, transportRefusal)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("истёкший срок вернул %v вместо context.DeadlineExceeded", err)
	}
	if resp != nil {
		t.Errorf("вместе с ошибкой отмены вернулся ответ %+v", resp)
	}
}

// Обёрнутая ошибка транспорта тоже обязана уступить контекстной: вызывающий
// проверяет errors.Is, а не текст.
func TestCancelResultPrefersContextErrorOverWrappedTransportError(t *testing.T) {
	ctx := expiredContext(t)
	wrapped := fmt.Errorf("запрос к каталогу: %w", transportRefusal)

	_, err := cancelResult(ctx, nil, wrapped)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("истёкший срок вернул обёртку %v вместо context.DeadlineExceeded", err)
	}
}

// Пара (nil, nil) проходит насквозь: подменять нечего, а придумывать ошибку на
// пустом месте значило бы ломать вызывающего, который считает nil успехом.
func TestCancelResultKeepsEmptyResult(t *testing.T) {
	ctx := expiredContext(t)

	resp, err := cancelResult(ctx, nil, nil)
	if resp != nil || err != nil {
		t.Errorf("пустой результат превратился в (%+v, %v)", resp, err)
	}
}

// Интеграция: обе ветки respectCancel обязаны расходиться одинаково.
func TestRespectCancelReturnsDeadlineErrorAfterExpiry(t *testing.T) {
	for i := 0; i < 30; i++ {
		ctx := expiredContext(t)
		resp, err := respectCancel(ctx, func() (*Response, error) { return nil, transportRefusal })
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("прогон %d: истёкший срок вернул %v вместо context.DeadlineExceeded", i+1, err)
		}
		if resp != nil {
			t.Fatalf("прогон %d: вместе с ошибкой отмены вернулся ответ %+v", i+1, resp)
		}
	}
}

func TestRespectCancelReturnsCanceledOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	_, err := respectCancel(ctx, func() (*Response, error) {
		time.Sleep(400 * time.Millisecond)
		return nil, transportRefusal
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("отмена вернула %v, хочу context.Canceled", err)
	}
}

func TestRespectCancelKeepsTransportErrorWhileContextAlive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	_, err := respectCancel(ctx, func() (*Response, error) { return nil, transportRefusal })
	if !errors.Is(err, transportRefusal) {
		t.Errorf("живой контекст подменил причину отказа: %v", err)
	}
}

func TestRespectCancelKeepsSuccessfulResponse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	want := &Response{Status: 200, Body: []byte("ok")}
	got, err := respectCancel(ctx, func() (*Response, error) { return want, nil })
	if err != nil {
		t.Fatalf("успешный запрос вернулся с ошибкой: %v", err)
	}
	if got != want {
		t.Errorf("вернулся другой ответ: %+v", got)
	}
}

// Замер на HEAD f5fca08, молчащий прокси, дедлайн 50ms, двести повторов: сто
// девяносто девять раз DoStd вернул контекстную ошибку и один раз - «socks connect
// tcp 127.0.0.1:51145->example.com:443: read tcp ...: i/o timeout» спустя
// 49.6213ms, за четырнадцать сотых миллисекунды до дедлайна. Транспорт обрывает
// рукопожатие по сроку контекста, но оформляет обрыв как сетевую ошибку сокета, и
// полный набор тестов падал из-за этого примерно в одном прогоне из двадцати.
func TestCancelResultWaitsOutNearDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), cancelGrace/2)
	defer cancel()

	resp, err := cancelResult(ctx, nil, transportRefusal)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("дедлайн наступил через половину окна, вернулось %v вместо context.DeadlineExceeded", err)
	}
	if resp != nil {
		t.Errorf("вместе с ошибкой отмены вернулся ответ %+v", resp)
	}
}

// Дедлайн уже наступил, а собственный таймер контекста ещё не сработал: замер на
// живом транспорте показал, что doStd возвращает сетевую ошибку спустя шестьдесят
// микросекунд после срока, и в этот момент ctx.Err ещё пуст. Подмена обязана
// сработать и здесь, иначе исход снова зависит от планирования.
func TestCancelResultSubstitutesWhenDeadlineJustPassed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Millisecond)
	defer cancel()
	time.Sleep(3 * time.Millisecond)

	resp, err := cancelResult(ctx, nil, transportRefusal)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("дедлайн наступил, вернулось %v вместо context.DeadlineExceeded", err)
	}
	if resp != nil {
		t.Errorf("вместе с ошибкой отмены вернулся ответ %+v", resp)
	}
}

// Окно ожидания обязано быть коротким и заведомо меньше дедлайна, с которым живут
// настоящие запросы: иначе сетевой сбой начал бы выглядеть как отмена у всех, а не
// только у тех, кто и так не успевал.
func TestCancelResultKeepsTransportErrorWhenDeadlineBeyondGrace(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), cancelGrace*20)
	defer cancel()

	start := time.Now()
	_, err := cancelResult(ctx, nil, transportRefusal)
	elapsed := time.Since(start)
	if !errors.Is(err, transportRefusal) {
		t.Errorf("далёкий дедлайн подменил причину отказа: %v", err)
	}
	// Живой контекст с далёким сроком не имеет права платить за ожидание: иначе
	// каждый сетевой сбой стоил бы вызывающему полного набора окон.
	if elapsed > cancelGrace {
		t.Errorf("далёкий дедлайн заставил ждать %s", elapsed)
	}
}

// Контекст без дедлайна ждать нечего: подмене взяться неоткуда.
func TestCancelResultKeepsTransportErrorWithoutDeadline(t *testing.T) {
	_, err := cancelResult(context.Background(), nil, transportRefusal)
	if !errors.Is(err, transportRefusal) {
		t.Errorf("контекст без срока подменил причину отказа: %v", err)
	}
}

// Успешный ответ не ждёт дедлайна и не подменяется: данные, полученные в срок,
// нужнее сообщения об отмене.
func TestCancelResultKeepsSuccessfulResponseNearDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), cancelGrace/2)
	defer cancel()

	want := &Response{Status: 200, Body: []byte("ok")}
	got, err := cancelResult(ctx, want, nil)
	if err != nil {
		t.Errorf("успешный ответ вернулся с ошибкой %v", err)
	}
	if got != want {
		t.Errorf("вернулся другой ответ: %+v", got)
	}
}

// Окно задано числом, а не на глаз: оно должно превышать замеренное расхождение в
// четырнадцать сотых миллисекунды и оставаться незаметным для вызывающего.
func TestCancelGraceCoversMeasuredGap(t *testing.T) {
	if cancelGrace != 5*time.Millisecond {
		t.Errorf("cancelGrace = %s, хочу 5ms", cancelGrace)
	}
	if cancelGrace < 200*time.Microsecond {
		t.Errorf("cancelGrace = %s меньше замеренного расхождения 140 микросекунд", cancelGrace)
	}
	if cancelGrace > 50*time.Millisecond {
		t.Errorf("cancelGrace = %s: ждать дольше уже заметно вызывающему", cancelGrace)
	}
}

// Под нагрузкой планировщик задерживает и таймер контекста, и select: замер на HEAD
// 49e5da6 с параллельно идущим набором cmd/voidsearchswag дал три падения из десяти
// прогонов по двадцать повторов, тогда как в одиночном прогоне триста повторов не
// дали ни одного. Одной попытки ожидания не хватило, поэтому их несколько, и их
// число обязано оставаться в пределах заметной для вызывающего задержки.
func TestCancelTriesBoundsWaiting(t *testing.T) {
	if cancelTries != 4 {
		t.Errorf("cancelTries = %d, хочу 4", cancelTries)
	}
	if cancelTries < 2 {
		t.Errorf("cancelTries = %d: одна попытка не переживает задержку планировщика", cancelTries)
	}
	if total := time.Duration(cancelTries) * cancelGrace; total > 50*time.Millisecond {
		t.Errorf("потолок ожидания %s: вызывающий заметит такую задержку на отказе", total)
	}
}

// Потолок ожидания обязан соблюдаться на деле, а не только в константах: прогон с
// истёкшим дедлайном не имеет права держать вызывающего дольше суммы окон.
func TestCancelResultReturnsWithinBoundedWait(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), cancelGrace/2)
	defer cancel()

	start := time.Now()
	_, err := cancelResult(ctx, nil, transportRefusal)
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("вернулось %v вместо context.DeadlineExceeded", err)
	}
	limit := time.Duration(cancelTries)*cancelGrace + 50*time.Millisecond
	if elapsed > limit {
		t.Errorf("cancelResult держал вызывающего %s при потолке %s", elapsed, limit)
	}
}

// Интеграция: обёртка обязана дождаться дедлайна и отдать контекстную ошибку, а не
// сетевую, когда транспорт поторопился.
func TestRespectCancelWaitsOutNearDeadline(t *testing.T) {
	for i := 0; i < 20; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), cancelGrace/2)
		resp, err := respectCancel(ctx, func() (*Response, error) { return nil, transportRefusal })
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("прогон %d: вернулось %v вместо context.DeadlineExceeded", i+1, err)
		}
		if resp != nil {
			t.Fatalf("прогон %d: вместе с ошибкой отмены вернулся ответ %+v", i+1, resp)
		}
	}
}
