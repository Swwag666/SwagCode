package search

import (
	"context"
	"testing"
	"time"

	"voidsearchswag/internal/metrics"
	"voidsearchswag/internal/searchers"
)

// ignoringSearcher не смотрит на контекст и отвечает только по своему таймеру.
// Именно так ведёт себя настоящий транспорт на socks-рукопожатии: прокси принял
// соединение и молчит, а запрос висит до собственного срока. blockingSearcher из
// engine_test.go для этих замеров не годится: он слушает ctx.Done() и успевает
// ответить одновременно с обрывом опроса, из-за чего исход зависел бы от выбора
// select.
type ignoringSearcher struct {
	name  string
	delay time.Duration
	res   []searchers.Result
}

func (s *ignoringSearcher) Name() string { return s.name }

func (s *ignoringSearcher) Search(context.Context, string, int) ([]searchers.Result, error) {
	time.Sleep(s.delay)
	return s.res, nil
}

func quickResults(n int) []searchers.Result {
	out := make([]searchers.Result, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, searchers.Result{URL: "https://q" + string(rune('a'+i)) + ".example/"})
	}
	return out
}

// Замер до правки на HEAD c429301. Опрос выходил из select по ctx.Done() через
// goto done и формировал отчёт только из тех движков, которые успели отправить
// результат:
//
//	результатов 1, движков в отчёте 1 из 2: [fast()]
//	результатов 4, движков в отчёте 1 из 2: [fast()]
//
// В живом прогоне search против молчащего прокси при VOIDSEARCH_REQUEST_TIMEOUT=3s
// это дало отчёт {"duration":"3m0s","report":{"engines":null,"live_engines":0,
// "total_engines":2}} - ни имени, ни elapsed, ни причины. Оператор видел «нет
// результатов» и шёл проверять запрос вместо выхода.
func TestUnansweredEngineReportedOnDeadline(t *testing.T) {
	fast := &blockingSearcher{name: "fast", res: quickResults(1)}
	hung := &ignoringSearcher{name: "hung", delay: time.Second}
	e, _ := newEngine(t, fast)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	all, reports := e.querySearchersParallel(ctx, []searchers.Searcher{fast, hung}, "q", 10)
	if len(reports) != 2 {
		t.Fatalf("в отчёте %d движков, хочу 2: %+v", len(reports), reports)
	}
	if reports[1].Name != "hung" {
		t.Errorf("вторая запись отчёта: %+v, хочу hung", reports[1])
	}
	want := "срок поиска истёк, движок не ответил"
	if reports[1].Error != want {
		t.Errorf("причина у неответившего движка %q, хочу %q", reports[1].Error, want)
	}
	if reports[1].Elapsed == "" {
		t.Error("у неответившего движка нет elapsed")
	}
	if reports[1].OK || reports[1].Count != 0 {
		t.Errorf("неответивший движок отмечен ответившим: %+v", reports[1])
	}
	if len(all) != 1 {
		t.Errorf("выдача %d результатов, хочу 1: неответивший движок не имеет права её пополнять", len(all))
	}
}

func TestUnansweredEngineReportedOnCancel(t *testing.T) {
	fast := &blockingSearcher{name: "fast", res: quickResults(1)}
	hung := &ignoringSearcher{name: "hung", delay: time.Second}
	e, _ := newEngine(t, fast)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()

	_, reports := e.querySearchersParallel(ctx, []searchers.Searcher{fast, hung}, "q", 10)
	if len(reports) != 2 {
		t.Fatalf("в отчёте %d движков, хочу 2: %+v", len(reports), reports)
	}
	want := "поиск отменён, движок не ответил"
	if reports[1].Error != want {
		t.Errorf("причина у неответившего движка %q, хочу %q", reports[1].Error, want)
	}
}

// Отчёт остаётся в порядке списка движков: запись о неответившем встаёт на своё
// место, а не дописывается в конец.
func TestUnansweredEngineKeepsSearcherOrder(t *testing.T) {
	first := &blockingSearcher{name: "alpha", res: quickResults(1)}
	hung := &ignoringSearcher{name: "bravo", delay: time.Second}
	last := &blockingSearcher{name: "charlie", res: quickResults(1)}
	e, _ := newEngine(t, first, last)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	_, reports := e.querySearchersParallel(ctx, []searchers.Searcher{first, hung, last}, "q", 100)
	want := []string{"alpha", "bravo", "charlie"}
	if len(reports) != len(want) {
		t.Fatalf("в отчёте %d движков, хочу %d: %+v", len(reports), len(want), reports)
	}
	for i, name := range want {
		if reports[i].Name != name {
			t.Errorf("позиция %d = %s, хочу %s", i, reports[i].Name, name)
		}
	}
	if reports[1].Error == "" {
		t.Error("неответивший движок в середине списка остался без причины")
	}
	if !reports[0].OK || !reports[2].OK {
		t.Errorf("ответившие движки отмечены как упавшие: %+v / %+v", reports[0], reports[2])
	}
}

// Неответивший движок не виноват: срок выставил вызывающий. Считать это отказом
// значило бы растить engine_fail и FailStreak на чужом таймауте.
func TestUnansweredEngineNotCountedAsFailure(t *testing.T) {
	fast := &blockingSearcher{name: "fast", res: quickResults(1)}
	hung := &ignoringSearcher{name: "hung", delay: time.Second}
	e, _ := newEngine(t, fast)
	e.Metrics = metrics.New()

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	e.queryParallel(ctx, []searchers.Searcher{fast, hung}, "q", 10, true)

	snap := e.Metrics.Snapshot()
	if n := snap[metrics.Key("engine_fail", "hung")]; n != 0 {
		t.Errorf("engine_fail для неответившего движка = %d, хочу 0", n)
	}
	if n := snap[metrics.Key("engine_ok", "fast")]; n != 1 {
		t.Errorf("engine_ok для ответившего движка = %d, хочу 1", n)
	}
}

func TestStopReasonTextClassifiesStop(t *testing.T) {
	cases := []struct {
		grace  bool
		err    error
		reason string
		want   string
	}{
		{grace: true, reason: "дозор без ошибки контекста", want: "окно дозора истекло, движок не успел ответить"},
		{grace: true, err: context.DeadlineExceeded, reason: "дозор важнее истёкшего срока", want: "окно дозора истекло, движок не успел ответить"},
		{err: context.DeadlineExceeded, reason: "истёкший срок", want: "срок поиска истёк, движок не ответил"},
		{err: context.Canceled, reason: "отмена", want: "поиск отменён, движок не ответил"},
		{reason: "обрыв без причины", want: "движок не ответил"},
	}
	for i, c := range cases {
		if got := stopReasonText(c.grace, c.err); got != c.want {
			t.Errorf("случай %d (%s): %q, хочу %q", i+1, c.reason, got, c.want)
		}
	}
}
