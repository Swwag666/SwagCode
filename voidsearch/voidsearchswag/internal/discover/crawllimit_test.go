package discover

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"

	"voidsearchswag/internal/httpc"
)

// dHostE - четвёртый хост цепочки: срезанный сид уже visited, поэтому
// очередь слоя 1 держит только ссылка на хост, не входивший в сиды.
const dHostE = "eeeeeeeeeeee5555.onion"

// cancelCrawl - фетчер, который отменяет контекст обхода после заданного
// числа GET. Так тест останавливает обход между слоями, не трогая сам
// Crawler: слой, уже начатый, дорабатывает, а вход в следующий видит
// отмену - ровно конкурентная ситуация «потолок уже достигнут, отмена
// пришла следом», на которой слепой прогон этапа 171 поймал limit_hit,
// потерявший событие предела.
type cancelCrawl struct {
	pages  map[string]string
	before int32
	calls  int32
	cancel func()
}

func (s *cancelCrawl) Fetch(ctx context.Context, r httpc.Request) (*httpc.Response, error) {
	if r.Method == http.MethodHead {
		return &httpc.Response{Status: 200}, nil
	}
	if n := atomic.AddInt32(&s.calls, 1); int(n) <= int(s.before) {
		s.cancel()
	}
	body, ok := s.pages[r.URL]
	if !ok {
		return &httpc.Response{Status: 404}, nil
	}
	return &httpc.Response{Status: 200, Body: []byte(body)}, nil
}

func TestCrawlCancelledBeforeStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := &cancelCrawl{pages: map[string]string{
		"http://" + dHostA + "/": `<a href="http://` + dHostB + `/">b</a>`,
	}, before: 0, cancel: func() {}}
	cr := NewCrawler(f, silentLog{}, fastCrawl(CrawlConfig{Depth: 1, MaxHosts: 5}))
	_, rep := cr.Crawl(ctx, []string{dHostA})
	if !rep.Cancelled {
		t.Error("отмена до старта не отмечена: cancelled=false, а ни один слой не должен был начаться")
	}
	if rep.LimitHit != "" {
		t.Errorf("отмена - не предел, а limit_hit=%q: поле обязано молчать", rep.LimitHit)
	}
	if rep.Pages != 0 {
		t.Errorf("при отменённом контексте пройдено страниц %d, ожидаю 0", rep.Pages)
	}
}

func TestCrawlCancelKeepsCeiling(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Четвёртый хост нужен потому, что срезанный сид уже помечен visited
	// при построении очереди: ссылка на него не создаст слой 1. Ссылка
	// B -> dHostE (не сид) держит очередь непустой, и отмена встречает её
	// на входе в следующий слой.
	f := &cancelCrawl{pages: map[string]string{
		"http://" + dHostA + "/": `<a href="http://` + dHostB + `/">b</a>`,
		"http://" + dHostB + "/": `<a href="http://` + dHostE + `/">e</a>`,
	}, before: 2, cancel: cancel}
	// Потолок 2 при трёх сидах: срез ставит limit_hit на входе в слой 0.
	// Отмена срабатывает после второго GET: слой дорабатывает, а вход в
	// слой 1 видит отмену. До этапа 172 отмена ЗАТИРАЛА уже стоявший
	// предел - отчёт называл «контекст отменён» и терял событие потолка.
	cr := NewCrawler(f, silentLog{}, fastCrawl(CrawlConfig{Depth: 1, MaxHosts: 2}))
	_, rep := cr.Crawl(ctx, []string{dHostA, dHostB, dHostC})
	if rep.LimitHit != "достигнут потолок хостов" {
		t.Errorf("limit_hit=%q, хочу «достигнут потолок хостов»: отмена не имеет права затирать предел", rep.LimitHit)
	}
	if !rep.Cancelled {
		t.Error("отмена не отмечена: cancelled=false, хотя слой 1 не начался из-за неё")
	}
	if rep.Pages != 2 {
		t.Errorf("страниц %d, ожидаю 2: слой обязан доработать после отмены", rep.Pages)
	}
}

func TestCrawlCancelSetsCeilingWhenExact(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &cancelCrawl{pages: map[string]string{
		"http://" + dHostA + "/": `<a href="http://` + dHostB + `/">b</a>`,
		"http://" + dHostB + "/": `<a href="http://` + dHostC + `/">c</a>`,
	}, before: 2, cancel: cancel}
	// Ровно потолок сидов: срез на входе в слой 0 НЕ ставит limit_hit
	// (очередь не шире потолка). Отмена после второго GET - и обход
	// остановился на 2 из 2 при непустой очереди ссылок. Развязка этапа
	// 172 обязана назвать оба события: отмену - полем, предел - строкой.
	cr := NewCrawler(f, silentLog{}, fastCrawl(CrawlConfig{Depth: 1, MaxHosts: 2}))
	_, rep := cr.Crawl(ctx, []string{dHostA, dHostB})
	if !rep.Cancelled {
		t.Error("отмена не отмечена: cancelled=false")
	}
	if rep.LimitHit != "достигнут потолок хостов" {
		t.Errorf("limit_hit=%q, хочу «достигнут потолок хостов»: обошли ровно потолок, ссылки в очереди не тронуты", rep.LimitHit)
	}
	if rep.Pages != 2 {
		t.Errorf("страниц %d, ожидаю 2", rep.Pages)
	}
}

func TestCrawlCancelBelowCeilingKeepsLimitSilent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &cancelCrawl{pages: map[string]string{
		"http://" + dHostA + "/": `<a href="http://` + dHostB + `/">b</a>`,
		"http://" + dHostB + "/": `<a href="http://` + dHostC + `/">c</a>`,
	}, before: 1, cancel: cancel}
	// Потолок 10, обошли 2: отмена - единственное событие, limit_hit
	// обязан молчать, чтобы «потолок не достигнут» не подменялось
	// «обход обрезан».
	cr := NewCrawler(f, silentLog{}, fastCrawl(CrawlConfig{Depth: 1, MaxHosts: 10}))
	_, rep := cr.Crawl(ctx, []string{dHostA, dHostB})
	if !rep.Cancelled {
		t.Error("отмена не отмечена: cancelled=false")
	}
	if rep.LimitHit != "" {
		t.Errorf("limit_hit=%q при 2 из 10: предел не достигнут, поле обязано молчать", rep.LimitHit)
	}
}

func TestCrawlWithoutCancelHasNoCancelledField(t *testing.T) {
	f := &stubCrawl{pages: map[string]string{
		"http://" + cHost + "/": `<a href="http://` + dHostA + `/">a</a>`,
	}}
	cr := NewCrawler(f, silentLog{}, fastCrawl(CrawlConfig{Depth: 0, MaxHosts: 5}))
	_, rep := cr.Crawl(context.Background(), []string{cHost})
	if rep.Cancelled {
		t.Error("без отмены поле cancelled стоит: ложное событие")
	}
	if rep.LimitHit != "" {
		t.Errorf("limit_hit=%q при исчерпанной очереди: поле обязано молчать", rep.LimitHit)
	}
}
