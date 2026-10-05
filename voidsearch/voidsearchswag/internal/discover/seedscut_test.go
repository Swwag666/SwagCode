package discover

import (
	"context"
	"testing"
	"time"
)

// Этап 174: срез входа и отмена с пустой очередью - два события, которые
// до этого исчезали из отчёта молча.
//
// Срез сидов: seedHosts выбрасывал кандидатов сверх потолка без следа
// (живой BEFORE-факт смоука 172, агент C: 14217 кандидатов, потолок 3,
// сидов 3 - «обошли 3 из 14217» без объяснения). Число отрезанного теперь
// едет вторым значением в SeedsCut, а НЕ в limit_hit: срез входа - не
// событие обхода, и контракт maxhosts («limit_hit молчит, когда очередь
// исчерпана») обязан пережить правку.
//
// Отмена с пустой очередью: цикл Crawl умел выходить сам, и единственная
// проверка отмены стояла на входе в следующий слой - живой BEFORE-факт
// (смоук 172, агент B, timeout=5): 4 страницы несли «отменено», очередь
// ссылок пуста, цикл вышел сам, crawl.cancelled исчез.

func TestRunSeedCutCountsCandidatesBeyondCeiling(t *testing.T) {
	st := newStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cr := NewCrawler(&stubCrawl{pages: map[string]string{}}, silentLog{}, fastCrawl(CrawlConfig{Depth: 1, MaxHosts: 2}))
	p := &Pool{
		Store:  st,
		Finder: &Finder{Client: &brokenWriteSource{n: 5}, Log: silentLog{}, Attempts: 1},
		Crawl:  cr,
		Log:    silentLog{},
	}
	res, err := p.Run(ctx, Options{WithCrawl: true, MaxHosts: 2})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Crawl == nil {
		t.Fatal("crawl-отчёта нет")
	}
	if res.Crawl.SeedsCut != 3 {
		t.Errorf("seeds_cut=%d, хочу 3: пять кандидатов при потолке 2 - трое не стали сидами, и это обязано быть видно", res.Crawl.SeedsCut)
	}
	if res.Crawled != 2 {
		t.Errorf("обошли %d страниц, хочу 2: потолок 2", res.Crawled)
	}
	if res.Crawl.LimitHit != "" {
		t.Errorf("limit_hit=%q при исчерпанной очереди: срез сидов - не событие обхода, контракт maxhosts нарушен", res.Crawl.LimitHit)
	}
}

func TestRunSeedCutSilentWhenEverythingFits(t *testing.T) {
	st := newStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cr := NewCrawler(&stubCrawl{pages: map[string]string{}}, silentLog{}, fastCrawl(CrawlConfig{Depth: 1, MaxHosts: 5}))
	p := &Pool{
		Store:  st,
		Finder: &Finder{Client: &brokenWriteSource{n: 2}, Log: silentLog{}, Attempts: 1},
		Crawl:  cr,
		Log:    silentLog{},
	}
	res, err := p.Run(ctx, Options{WithCrawl: true, MaxHosts: 5})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Crawl == nil {
		t.Fatal("crawl-отчёта нет")
	}
	if res.Crawl.SeedsCut != 0 {
		t.Errorf("seeds_cut=%d без среза: поле обязано молчать, когда всё поместилось", res.Crawl.SeedsCut)
	}
	// omitempty: отсутствие события - отсутствие поля, а не ноль в JSON.
	if _, has := decodeResult(t, res)["crawl"].(map[string]any)["seeds_cut"]; has {
		t.Error("seeds_cut=0 сериализуется как поле: omitempty не работает, «не случилось» неотличимо от нуля")
	}
}

// cancelEmptyQueueCrawl отменяет контекст внутри единственного слоя, но
// возвращает страницу без ссылок: очередь next пуста, цикл выходит сам -
// расклад живого BEFORE-факта B172-T5, где отмена терялась. Сама обёртка
// не нужна: cancelingCrawl уже дёргает cancel на первом GET, а тело без
// ссылок оставляет next пустым.

func TestCrawlCancelledWithEmptyNextQueueIsReported(t *testing.T) {
	st := newStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	page := "http://" + cHost + "/"
	body := `<html><head><title>Пусто</title></head><body></body></html>`
	// Отмена дёргается на первом же GET, страница возвращается нормальной
	// и ссылок не содержит: слой дорабатывает, next пуст, цикл выходит.
	wrapped := &cancelingCrawl{inner: &stubCrawl{pages: map[string]string{page: body}}, cancel: cancel}
	cr := NewCrawler(wrapped, silentLog{}, fastCrawl(CrawlConfig{Depth: 2, MaxHosts: 10}))
	p := &Pool{
		Store:  st,
		Finder: &Finder{Client: &brokenWriteSource{n: 1}, Log: silentLog{}, Attempts: 1},
		Crawl:  cr,
		Log:    silentLog{},
	}
	res, err := p.Run(ctx, Options{WithCrawl: true, Seeds: []string{cHost}, MaxHosts: 10})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Crawl == nil {
		t.Fatal("crawl-отчёта нет")
	}
	if !res.Crawl.Cancelled {
		t.Fatal("отмена внутри слоя с пустой очередью потеряна: crawl.cancelled=false - расклад B172-T5 не закрыт")
	}
	if res.Crawl.LimitHit != "" {
		t.Errorf("limit_hit=%q: потолок не при делах, событие только одно - отмена", res.Crawl.LimitHit)
	}
}

func TestCrawlNotCancelledWhenContextDiesAfterReturn(t *testing.T) {
	st := newStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	page := "http://" + cHost + "/"
	body := `<html><head><title>Мгновенно</title></head><body></body></html>`
	cr := NewCrawler(&stubCrawl{pages: map[string]string{page: body}}, silentLog{}, fastCrawl(CrawlConfig{Depth: 1, MaxHosts: 10}))
	p := &Pool{
		Store:  st,
		Finder: &Finder{Client: &brokenWriteSource{n: 1}, Log: silentLog{}, Attempts: 1},
		Crawl:  cr,
		Log:    silentLog{},
	}
	res, err := p.Run(ctx, Options{WithCrawl: true, Seeds: []string{cHost}, MaxHosts: 10})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	cancel()
	if res.Crawl == nil {
		t.Fatal("crawl-отчёта нет")
	}
	if res.Crawl.Cancelled {
		t.Error("отмена ПОСЛЕ возврата Crawl ставит crawl.cancelled ретроактивно: флаг описывает обход, а не жизнь вызывающего")
	}
}

func TestSeedHostsCutCountsFreshAndDeadBeyondLimit(t *testing.T) {
	st := newStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Мёртвые в пуле не съедают мест в сиде, но кандидаты сверх потолка
	// считаются отрезанными в обеих очередях: число - про вход, не про
	// приоритет.
	cands := make([]Candidate, 0, 7)
	for i := 0; i < 7; i++ {
		addr := v3a[:55] + string(rune('a'+i))
		cands = append(cands, Candidate{Address: addr + ".onion"})
	}
	p := &Pool{Store: st, Log: silentLog{}}
	seeds, cut := p.seedHosts(ctx, cands, 3)
	if len(seeds) != 3 {
		t.Fatalf("сидов %d, хочу 3: потолок обязан работать", len(seeds))
	}
	if cut != 4 {
		t.Fatalf("отрезано %d, хочу 4: семь кандидатов при потолке 3", cut)
	}
}
