package discover

import (
	"context"
	"net/http"
	"testing"
	"time"

	"voidsearchswag/internal/httpc"
)

// slowGetCrawl держит GET заявленное время: обход обязан записать этот
// интервал в LatencyMS страницы.
type slowGetCrawl struct {
	delay time.Duration
}

func (s *slowGetCrawl) Fetch(ctx context.Context, r httpc.Request) (*httpc.Response, error) {
	if r.Method == http.MethodHead {
		return &httpc.Response{Status: 200}, nil
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(s.delay):
	}
	return &httpc.Response{Status: 200, Body: []byte("")}, nil
}

func TestCrawlPageMeasuresFetchLatency(t *testing.T) {
	f := &slowGetCrawl{delay: 40 * time.Millisecond}
	cr := NewCrawler(f, silentLog{}, fastCrawl(CrawlConfig{Depth: 1, PageTimeout: 5 * time.Second}))
	_, rep := cr.Crawl(context.Background(), []string{cHost})

	if len(rep.PageDetail) != 1 {
		t.Fatalf("страниц в отчёте %d, ожидала 1", len(rep.PageDetail))
	}
	p := rep.PageDetail[0]
	if p.Error != "" {
		t.Fatalf("страница с ошибкой: %q", p.Error)
	}
	// Окно широкое сверху и жёсткое снизу: sleep меньше 40ms не отдаёт
	// раньше срока, квант таймера Windows может добавить 15ms.
	if p.LatencyMS < 25 {
		t.Errorf("замер латентности потерян: %dms при паузе клиента 40ms", p.LatencyMS)
	}
	if p.LatencyMS > 5000 {
		t.Errorf("замер латентности завышен: %dms", p.LatencyMS)
	}
}

// Пауза вежливости - не время сервиса: замер обязан обнимать только Fetch.
// Wait отсчитывает слот per-host: второй запрос к тому же хосту ждёт
// PerHostDelay целиком, и мутация «замер вокруг Wait» приписала бы эти
// сотни миллисекунд латентностью сервиса.
func TestCrawlLatencyExcludesRateLimitWait(t *testing.T) {
	f := &slowGetCrawl{delay: time.Millisecond}
	cr := NewCrawler(f, silentLog{}, CrawlConfig{
		Depth:        1,
		PerHostDelay: 200 * time.Millisecond,
		PageTimeout:  5 * time.Second,
	})
	ctx := context.Background()

	p1, _ := cr.crawlPage(ctx, cHost, 0)
	if p1.Error != "" {
		t.Fatalf("первая страница с ошибкой: %q", p1.Error)
	}
	p2, _ := cr.crawlPage(ctx, cHost, 0)
	if p2.Error != "" {
		t.Fatalf("вторая страница с ошибкой: %q", p2.Error)
	}
	// 100ms барьер: пауза в 200ms в замере дала бы 200+ у второго запроса.
	if p2.LatencyMS >= 100 {
		t.Errorf("пауза вежливости попала в замер второго запроса: %dms (PerHostDelay 200ms)", p2.LatencyMS)
	}
}

// Этап 175: живость по обходу пишется с фактическим временем страницы, а не
// с заглушкой 1000ms. BEFORE-факт по живой базе: latency_avg=1000 у всех
// live-записей обхода подряд.
func TestRecordCrawlWritesMeasuredLatency(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	putOnion(t, st, "ok.onion", "unknown", 0, 0)

	p := &Pool{Store: st, Log: silentLog{}}
	crep := &CrawlReport{PageDetail: []Page{
		{Host: "ok.onion", Status: 200, LatencyMS: 500},
	}}
	if n := p.recordCrawl(ctx, crep, nil); n != 1 {
		t.Fatalf("записано хостов %d, ожидала 1", n)
	}

	o, err := st.GetOnion(ctx, "ok.onion")
	if err != nil {
		t.Fatal(err)
	}
	if o.LatencyAvg != 500 {
		t.Errorf("латентность записи = %d, ожидала фактическую 500 (заглушка 1000 - дефект этапа до 175)", o.LatencyAvg)
	}
}

func TestRecordCrawlSmoothsMeasuredLatency(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	putOnion(t, st, "ok.onion", "unknown", 0, 0)

	p := &Pool{Store: st, Log: silentLog{}}
	p.recordCrawl(ctx, &CrawlReport{PageDetail: []Page{
		{Host: "ok.onion", Status: 200, LatencyMS: 500},
	}}, nil)
	p.recordCrawl(ctx, &CrawlReport{PageDetail: []Page{
		{Host: "ok.onion", Status: 200, LatencyMS: 100},
	}}, nil)

	o, err := st.GetOnion(ctx, "ok.onion")
	if err != nil {
		t.Fatal(err)
	}
	// Сглаживание пробы: (500*3 + 100)/4 = 400.
	if o.LatencyAvg != 400 {
		t.Errorf("сглаженная латентность = %d, ожидала 400", o.LatencyAvg)
	}
}

// Контракт записи: неуспешная страница не переписывает латентность живости -
// время до отказа остаётся в pages_detail, но не в среднем пула.
func TestRecordCrawlKeepsPriorLatencyOnFail(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	putOnion(t, st, "ok.onion", "unknown", 0, 0)

	p := &Pool{Store: st, Log: silentLog{}}
	p.recordCrawl(ctx, &CrawlReport{PageDetail: []Page{
		{Host: "ok.onion", Status: 200, LatencyMS: 500},
	}}, nil)
	p.recordCrawl(ctx, &CrawlReport{PageDetail: []Page{
		{Host: "ok.onion", Error: "таймаут", LatencyMS: 9999},
	}}, nil)

	o, err := st.GetOnion(ctx, "ok.onion")
	if err != nil {
		t.Fatal(err)
	}
	if o.LatencyAvg != 500 {
		t.Errorf("провал переписал латентность: %d, ожидала прежние 500", o.LatencyAvg)
	}
}
