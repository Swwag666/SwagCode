package discover

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"voidsearchswag/internal/httpc"
	"voidsearchswag/internal/store"
)

// slowHeadCrawl отдаёт страницу мгновенно, а HEAD замера держит столько,
// сколько просят, и считает вызовы. Так видно, ходит ли замер в сеть после
// того, как бюджет вызова уже истёк: ответ клиенту ждёт именно этот хвост.
type slowHeadCrawl struct {
	inner *stubCrawl
	delay time.Duration
	heads int32
}

func (s *slowHeadCrawl) Fetch(ctx context.Context, r httpc.Request) (*httpc.Response, error) {
	if r.Method == http.MethodHead {
		// Реальный httpc-клиент строит запрос через
		// http.NewRequestWithContext и отклоняет его мгновенно при
		// мёртвом контексте - фейк обязан вести себя так же, иначе тест
		// меряет фейк, а не систему.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		atomic.AddInt32(&s.heads, 1)
		time.Sleep(s.delay)
	}
	return s.inner.Fetch(ctx, r)
}

// cancelOnGet отменяет бюджет прогона на первом GET-запросе страницы и
// пропускает всё через inner: страница возвращается с файловыми ссылками,
// а запись и замер стартуют уже «после обрезки», как в живом прогоне.
type cancelOnGet struct {
	inner  Fetcher
	cancel context.CancelFunc
	fired  bool
}

func (c *cancelOnGet) Fetch(ctx context.Context, r httpc.Request) (*httpc.Response, error) {
	if r.Method != http.MethodHead && !c.fired {
		c.fired = true
		c.cancel()
	}
	return c.inner.Fetch(ctx, r)
}

func TestDiscoverMeasureDiesWithBudget(t *testing.T) {
	// Этап 173. Живой смоук B этапа 172 (стенд after172, прогретая база):
	// ответ discover приходил на 2с / 22.6с / 38.5с позже заявленного
	// timeout (5/30/90с), и перебор рос вместе с бюджетом. HTTP-клиент,
	// поставивший таймаут равным бюджету вызова, терял валидные ответы.
	// Причина: замер размеров (сеть, HEAD через tor) жил под собственным
	// measureCtx без отмены родителя - до 60с после обрезки, поверх
	// мёртвого rctx, тогда как запись пула и файлов - локальные секунды.
	// Замер - не запись: размер не входит в контракт «собранное доезжает»
	// (адреса, мета, живость, файлы). Бюджет вызова обязан резать и его.
	st := newStore(t)
	page := "http://" + cHost + "/"
	body := `<html><body><a href="/files/dump.zip">дамп</a><a href="/files/keys.7z">ключи</a></body></html>`
	head := &slowHeadCrawl{
		inner: &stubCrawl{pages: map[string]string{page: body}, head: http.Header{"Content-Length": []string{"12345"}}},
		delay: 150 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cr := NewCrawler(&cancelOnGet{inner: head, cancel: cancel}, silentLog{}, fastCrawl(CrawlConfig{Depth: 1}))
	p := &Pool{
		Store:       st,
		Finder:      &Finder{Client: &brokenWriteSource{n: 1}, Log: silentLog{}, Attempts: 1},
		Crawl:       cr,
		Log:         silentLog{},
		sizesBudget: 400 * time.Millisecond,
	}

	t0 := time.Now()
	res, err := p.Run(ctx, Options{WithCrawl: true, Seeds: []string{cHost}, MaxHosts: 2})
	wall := time.Since(t0)
	if err != nil {
		t.Fatalf("Run вернул ошибку: %v", err)
	}
	if ctx.Err() == nil {
		t.Fatal("тест не воспроизвёл обрезку: контекст жив")
	}
	if got := atomic.LoadInt32(&head.heads); got != 0 {
		t.Fatalf("после обрезки budget замер сходил в сеть %d раз: хвост ответа тянет не запись, а размеры", got)
	}
	if wall > 150*time.Millisecond {
		t.Fatalf("wall = %v при мёртвом бюджете вызова: замер размеров жил дольше обхода (sizesBudget 400ms)", wall)
	}
	if res.Files != 2 {
		t.Fatalf("files = %d: смерть замера не имеет права рвать доставку файлов", res.Files)
	}
	for _, f := range res.Crawl.FileRefs {
		if f.Size != 0 {
			t.Errorf("ref %s: size = %d при мёртвом бюджете: замер обязан молчать, размер - довеска, не заявка", f.URL, f.Size)
		}
	}
	got, err := st.SearchFiles(context.Background(), store.FileQuery{TaskID: res.FileTaskID, Limit: 100})
	if err != nil {
		t.Fatalf("SearchFiles: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("по метке из ответа найдено %d записей при files=%d", len(got), res.Files)
	}
}

func TestDiscoverMeasureLivesInsideBudget(t *testing.T) {
	// Обратная сторона этапа 173: замер обязан вымирать вместе с бюджетом
	// вызова, но в живом бюджете он обязан работать - иначе правка «лечит»
	// хвост, вырезав размеры совсем, и каталог навсегда остаётся без
	// размеров под предлогом экономии времени.
	st := newStore(t)
	page := "http://" + cHost + "/"
	body := `<html><body><a href="/files/dump.zip">дамп</a><a href="/files/keys.7z">ключи</a></body></html>`
	head := &slowHeadCrawl{
		inner: &stubCrawl{pages: map[string]string{page: body}, head: http.Header{"Content-Length": []string{"7777"}}},
		delay: 120 * time.Millisecond,
	}
	cr := NewCrawler(head, silentLog{}, fastCrawl(CrawlConfig{Depth: 1}))
	p := &Pool{
		Store:       st,
		Finder:      &Finder{Client: &brokenWriteSource{n: 1}, Log: silentLog{}, Attempts: 1},
		Crawl:       cr,
		Log:         silentLog{},
		sizesBudget: 5 * time.Second,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	t0 := time.Now()
	res, err := p.Run(ctx, Options{WithCrawl: true, Seeds: []string{cHost}, MaxHosts: 2})
	wall := time.Since(t0)
	if err != nil {
		t.Fatalf("Run вернул ошибку: %v", err)
	}
	if got := atomic.LoadInt32(&head.heads); got == 0 {
		t.Fatal("замер не сходил в сеть при живом бюджете: правка хвоста убила размеры целиком")
	}
	if res.Files != 2 {
		t.Fatalf("files = %d, ожидала 2", res.Files)
	}
	for i, f := range res.Crawl.FileRefs {
		if f.Size != 7777 {
			t.Errorf("ref[%d] %s: size = %d, ожидала 7777 - живой бюджет обязан давать размеры", i, f.URL, f.Size)
		}
	}
	if wall > 900*time.Millisecond {
		t.Fatalf("wall = %v при живом бюджете 2s: замер обязан вписываться в бюджет вызова, а не ехать поверх", wall)
	}
}
