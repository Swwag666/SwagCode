package discover

import (
	"context"
	"testing"
	"time"
)

// Хосты цепочки для тестов глубины: страница каждого уровня ссылается на
// следующий. Depth=1 обязан остановиться на втором уровне, Depth=2 - дойти
// до третьего.
const (
	dHostA = "aaaaaaaaaaaa2222.onion"
	dHostB = "bbbbbbbbbbbb3333.onion"
	dHostC = "cccccccccccc4444.onion"
)

func chainCrawler(t *testing.T) *Crawler {
	t.Helper()
	pages := map[string]string{
		"http://" + dHostA + "/": `<html><body><a href="http://` + dHostB + `/">b</a></body></html>`,
		"http://" + dHostB + "/": `<html><body><a href="http://` + dHostC + `/">c</a></body></html>`,
		"http://" + dHostC + "/": `<html><body><a href="http://` + dHostA + `/">a</a></body></html>`,
	}
	return NewCrawler(&stubCrawl{pages: pages}, silentLog{}, fastCrawl(CrawlConfig{Depth: 2, MaxHosts: 10}))
}

// TestWithDepthOverridesOnlyDepth: метод обязан менять глубину, не трогая
// ни исходного обходчика (его конфигурация - серверная, её подмена ломала
// бы фоновые прогоны), ни остальных участников копии: клиент и лимитер
// общие, потому что пауза между хостами остаётся серверной настройкой.
// Неположительная глубина и совпадающая возвращают исходного обходчика:
// это договорённость «0 = глубина из конфига», та же, что у CLI-флага.
func TestWithDepthOverridesOnlyDepth(t *testing.T) {
	base := chainCrawler(t)
	limiter := base.Limiter

	one := base.WithDepth(1)
	if one == base {
		t.Fatal("глубина 2 -> 1 обязана дать другого обходчика")
	}
	if one.Cfg.Depth != 1 {
		t.Errorf("копия несёт depth=%d, хочу 1", one.Cfg.Depth)
	}
	if base.Cfg.Depth != 2 {
		t.Errorf("исходный обходчик переписан: depth=%d, хочу 2", base.Cfg.Depth)
	}
	if one.Limiter != limiter || one.Client != base.Client {
		t.Error("клиент и лимитер обязаны переиспользоваться, а не заводиться заново")
	}

	if zero := base.WithDepth(0); zero != base {
		t.Error("нулевая глубина - «как в конфиге»: возвращаться обязан исходный обходчик")
	}
	if neg := base.WithDepth(-3); neg != base {
		t.Error("отрицательная глубина - та же договорённость, что ноль")
	}
	if same := base.WithDepth(2); same != base {
		t.Error("совпавшая глубина не обязана плодить копию")
	}
}

// TestRunAppliesRequestedDepth: живой BEFORE этапа 170 (стенд vss170):
// discover с depth=1 отвечал crawl.depth=2 и обходил лишний слой -
// Options.Depth молча не доходил до Crawler.Crawl, и единственной
// глубиной оставалась серверная из конфига. Тест ставит цепочку из трёх
// хостов и серверную глубину 2: запрос depth=1 обязан обойти два уровня
// (A-сид, B-ссылка) и не ходить на C, а depth=0 - оставить серверные
// два уровня, как фоновый прогон.
func TestRunAppliesRequestedDepth(t *testing.T) {
	st := newStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	mkPool := func() *Pool {
		return &Pool{
			Store:  st,
			Finder: &Finder{Client: &brokenWriteSource{n: 1}, Log: silentLog{}, Attempts: 1},
			Crawl:  chainCrawler(t),
			Log:    silentLog{},
		}
	}

	res, err := mkPool().Run(ctx, Options{WithCrawl: true, Seeds: []string{dHostA}, Depth: 1, MaxHosts: 10})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Crawl == nil {
		t.Fatal("crawl-отчёта нет")
	}
	if res.Crawl.Depth != 1 {
		t.Errorf("запрошен depth=1, отчёт назвал %d: глубина вызова до обхода не доехала", res.Crawl.Depth)
	}
	if res.Crawled != 2 {
		t.Errorf("depth=1 обошёл %d страниц, хочу 2 (сид и его ссылка): слой за пределами глубины жив", res.Crawled)
	}

	res2, err := mkPool().Run(ctx, Options{WithCrawl: true, Seeds: []string{dHostA}, Depth: 0, MaxHosts: 10})
	if err != nil {
		t.Fatalf("Run с depth=0: %v", err)
	}
	if res2.Crawl == nil {
		t.Fatal("crawl-отчёта нет при depth=0")
	}
	if res2.Crawl.Depth != 2 {
		t.Errorf("depth=0 - серверная глубина: отчёт назвал %d, хочу 2", res2.Crawl.Depth)
	}
	if res2.Crawled != 3 {
		t.Errorf("серверная глубина 2 обошла %d страниц, хочу 3: договорённость «0 = конфиг» сломана", res2.Crawled)
	}
}

// TestRunNegativeDepthTreatedAsConfig: обработчик MCP приводит
// отрицательный depth к нулю; пул обязан относиться к нулю и минусу
// одинаково - как к серверной глубине, а не как к «обойди всё».
func TestRunNegativeDepthTreatedAsConfig(t *testing.T) {
	st := newStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	p := &Pool{
		Store:  st,
		Finder: &Finder{Client: &brokenWriteSource{n: 1}, Log: silentLog{}, Attempts: 1},
		Crawl:  chainCrawler(t),
		Log:    silentLog{},
	}
	res, err := p.Run(ctx, Options{WithCrawl: true, Seeds: []string{dHostA}, Depth: -1, MaxHosts: 10})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Crawl == nil || res.Crawl.Depth != 2 {
		t.Fatalf("отрицательная глубина обязана читаться как серверная: depth=%v", res.Crawl)
	}
	if res.Crawled != 3 {
		t.Errorf("серверная глубина 2 обошла %d страниц, хочу 3", res.Crawled)
	}
}
