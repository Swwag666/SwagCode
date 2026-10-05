package discover

import (
	"context"
	"errors"
	"testing"
	"time"

	"voidsearchswag/internal/httpc"
)

// Этап 178: срез вызова не пишет живость. Смоук E на билде vss178b:
// discover с timeout обрезал crawl, отменённые страницы несли
// Error="context deadline exceeded", recordCrawl писал их провалами, и
// fail_streak хостов рос за чужой счёт - за бюджет вызова, а не за отказ
// сервиса. Два контракта: recordCrawl не трогает срезанные записи (ни
// провал, ни успех, ни skip-счётчик), а отчёт обхода несёт отдельный
// счётчик cut, не смешивая срез с failed.

func TestRecordCrawlSkipsCutPages(t *testing.T) {
	st := newStore(t)
	putOnion(t, st, "cut.onion", "unknown", 0, 0)
	putOnion(t, st, "ok.onion", "live", 2, 100)

	p := &Pool{Store: st, Log: silentLog{}}
	crep := &CrawlReport{PageDetail: []Page{
		// Срез: контекст вызова умер до предел страницы. Ровно такую
		// страницу смоук E видел провалом у живого хоста.
		{Host: "cut.onion", Error: "context deadline exceeded", LatencyMS: 5482, Cut: true},
		// Обычный успех: пишется как всегда.
		{Host: "ok.onion", Status: 200},
	}}
	var res Result
	if n := p.recordCrawl(context.Background(), crep, &res); n != 1 {
		t.Fatalf("записано %d проб, ожидала 1: срез живость не пишет", n)
	}
	if res.ProbeFailed != 0 || res.LastError != "" {
		t.Fatalf("срез записан как отказ базы: %+v", res)
	}
	if res.ProbeSkipped != 0 {
		t.Fatalf("срез записан как нет-транспорта: %+v", res)
	}
	// Хозяйский результат: fail_streak срезанного хоста не вырос -
	// вердикта о сервисе не было.
	o, err := st.GetOnion(context.Background(), "cut.onion")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if o.FailStreak != 0 {
		t.Fatalf("fail_streak = %d: срез вызова поднял полосу неудач", o.FailStreak)
	}
}

// cutFetch отменяет родительский контекст при первом Fetch и возвращает
// ошибку, как это выглядит при отмене внутри запроса: fetch умер от
// родительского бюджета, а не от предел страницы.
type cutFetch struct{ cancel context.CancelFunc }

func (s *cutFetch) Fetch(ctx context.Context, r httpc.Request) (*httpc.Response, error) {
	s.cancel()
	return nil, errors.New("context deadline exceeded")
}

func TestCrawlCountsCutSeparatelyFromFailed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Concurrency=1 делает картину детерминированной: первый хост
	// отменяет контекст из Fetch, остальные заходят в crawlPage уже с
	// мёртвым ctx и режутся на входе.
	cr := NewCrawler(&cutFetch{cancel: cancel}, silentLog{}, fastCrawl(CrawlConfig{Depth: 0, MaxHosts: 3, Concurrency: 1}))

	_, rep := cr.Crawl(ctx, []string{
		"aaaaaaaaaaaaaaab.onion",
		"aaaaaaaaaaaaaaac.onion",
		"aaaaaaaaaaaaaaad.onion",
	})

	if rep.Pages != 3 {
		t.Fatalf("pages = %d, ожидала 3: %+v", rep.Pages, rep)
	}
	if rep.Cut != 3 {
		t.Fatalf("cut = %d, ожидала 3: срез обязан назваться отдельно: %+v", rep.Cut, rep)
	}
	if rep.Failed != 0 {
		t.Fatalf("failed = %d: срез вызова не отказ сервиса: %+v", rep.Failed, rep)
	}
	if rep.Ok != 0 {
		t.Fatalf("ok = %d при отменённых страницах: %+v", rep.Ok, rep)
	}
	if !rep.Cancelled {
		t.Fatal("отмена вызова не названа в crawl.cancelled")
	}
	for i, p := range rep.PageDetail {
		if !p.Cut {
			t.Errorf("страница %d не помечена cut: %+v", i, p)
		}
		if p.Error == "" {
			t.Errorf("страница %d без текста ошибки: %+v", i, p)
		}
	}
}

// Счётчик cut обязан попадать в JSON под своим именем: смоук читает
// именно сериализованный отчёт, и отсутствие ключа означало бы «не
// случилось».
func TestCrawlReportSerializesCut(t *testing.T) {
	m := decodeResult(t, Result{Crawl: &CrawlReport{Cut: 1}})
	crawl, ok := m["crawl"].(map[string]any)
	if !ok {
		t.Fatalf("crawl не сериализуется: %v", m)
	}
	if got, ok := crawl["cut"]; !ok || got != float64(1) {
		t.Fatalf("ключ cut в JSON = %v (есть: %v)", got, ok)
	}
}

// Limiter-ветка среза: отмена, случившаяся между входом в crawlPage и
// паузой вежливости. Wait уважает ctx.Done, поэтому пауза просыпается
// ошибкой, и страница обязана получить Cut, а не «ошибку запроса».
func TestCrawlPageCutByLimiterWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Клиент обязан быть не-nil: выход «клиент не задан» стоит РАНЬШЕ
	// паузы вежливости, и nil-клиент закрыл бы ветку до Wait. До самого
	// Fetch очередь не дойдёт - отмена в паузе будит Wait ошибкой.
	cr := NewCrawler(&stubCrawl{}, silentLog{}, CrawlConfig{
		PerHostDelay: 80 * time.Millisecond,
		PageTimeout:  time.Second,
		Concurrency:  1,
	})

	// Пауза вежливости - между запросами К ОДНОМУ хосту: первый запрос
	// идёт без задержки, поэтому лимитер прогревается заранее, и
	// crawlPage попадает в реальную паузу.
	if err := cr.Limiter.Wait(ctx, "abcdefghijkl.onion"); err != nil {
		t.Fatalf("прогрев лимитера: %v", err)
	}

	done := make(chan Page, 1)
	go func() {
		p, _ := cr.crawlPage(ctx, "abcdefghijkl.onion", 0)
		done <- p
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case p := <-done:
		if !p.Cut {
			t.Fatalf("страница после отмены в паузе не помечена cut: %+v", p)
		}
		if p.Error == "" {
			t.Fatalf("страница без текста ошибки: %+v", p)
		}
		if p.NoTransport {
			t.Fatalf("отмена в паузе не «нет транспорта»: %+v", p)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("crawlPage завис: пауза не проснулась по отмене")
	}
}

func TestPageTitleUnescapesEntities(t *testing.T) {
	// Этап 178: сырые HTML-сущности в title читались как мусор в ответе
	// и мете пула (&amp; в каждом втором заголовке витрины). До этапа
	// строка шла как есть.
	cases := map[string]string{
		"<html><head><title>Foo &amp; Bar</title></head>":                       "Foo & Bar",
		"<html><head><title>&quot;hidden&quot; wiki &#39;s page</title></head>": `"hidden" wiki 's page`,
		"<html><head><title>  Plain   title </title></head>":                    "Plain title",
		"<html><head><title>&lt;no tags&gt; here</title></head>":                "<no tags> here",
	}
	for body, want := range cases {
		if got := pageTitle(body); got != want {
			t.Errorf("pageTitle(%q) = %q, ожидала %q", body, got, want)
		}
	}
}
