package discover

import (
	"context"
	"testing"
	"time"
)

// TestWithMaxHostsOverridesOnlyMaxHosts: метод обязан менять потолок, не
// трогая ни исходного обходчика (его конфигурация - серверная), ни
// остальных участников копии. Неположительный потолок и совпавший
// возвращают исходного: договорённость «0 = потолок из конфига», та же,
// что у CLI-флага.
func TestWithMaxHostsOverridesOnlyMaxHosts(t *testing.T) {
	base := chainCrawler(t)
	limiter := base.Limiter

	seven := base.WithMaxHosts(7)
	if seven == base {
		t.Fatal("потолок 10 -> 7 обязан дать другого обходчика")
	}
	if seven.Cfg.MaxHosts != 7 {
		t.Errorf("копия несёт max_hosts=%d, хочу 7", seven.Cfg.MaxHosts)
	}
	if base.Cfg.MaxHosts != 10 {
		t.Errorf("исходный обходчик переписан: max_hosts=%d, хочу 10", base.Cfg.MaxHosts)
	}
	if seven.Cfg.Depth != base.Cfg.Depth {
		t.Errorf("копия зачем-то меняет глубину: %d вместо %d", seven.Cfg.Depth, base.Cfg.Depth)
	}
	if seven.Limiter != limiter || seven.Client != base.Client {
		t.Error("клиент и лимитер обязаны переиспользоваться, а не заводиться заново")
	}

	if zero := base.WithMaxHosts(0); zero != base {
		t.Error("нулевой потолок - «как в конфиге»: возвращаться обязан исходный обходчик")
	}
	if neg := base.WithMaxHosts(-3); neg != base {
		t.Error("отрицательный потолок - та же договорённость, что ноль")
	}
	if same := base.WithMaxHosts(10); same != base {
		t.Error("совпавший потолок не обязан плодить копию")
	}
}

// TestRunAppliesRequestedMaxHosts: живой BEFORE этапа 171 (стенд vss171,
// чистая база): discover с max_hosts=15 отвечал crawl.max_hosts=50 и
// обходил 50 страниц (15 сидов + 35 ссылок) - потолок запроса резал
// только срез сидов, а сам обход жил на серверных 50. Тест ставит
// цепочку из трёх хостов и серверный потолок 10: запрос max_hosts=2
// обязан обойти две страницы и упереться в «достигнут потолок хостов»,
// а max_hosts=0 - оставить серверские 10 и обойти все три. Явные сиды
// сверх потолка срезаются тем же числом: потолок один для среза и обхода.
func TestRunAppliesRequestedMaxHosts(t *testing.T) {
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

	res, err := mkPool().Run(ctx, Options{WithCrawl: true, Seeds: []string{dHostA, dHostB, dHostC}, Depth: 1, MaxHosts: 2})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Crawl == nil {
		t.Fatal("crawl-отчёта нет")
	}
	if res.Crawl.MaxHosts != 2 {
		t.Errorf("запрошен max_hosts=2, отчёт назвал %d: потолок вызова до обхода не доехал", res.Crawl.MaxHosts)
	}
	if res.Crawled != 2 {
		t.Errorf("max_hosts=2 обошёл %d страниц, хочу 2: три явных сида обязаны срезаться потолком, а не обходитваться целиком", res.Crawled)
	}
	if res.Crawl.LimitHit != "достигнут потолок хостов" {
		t.Errorf("limit_hit=%q, хочу «достигнут потолок хостов»: срез сидов потолком - видимое событие, а не молчаливое", res.Crawl.LimitHit)
	}

	res2, err := mkPool().Run(ctx, Options{WithCrawl: true, Seeds: []string{dHostA}, Depth: 1, MaxHosts: 0})
	if err != nil {
		t.Fatalf("Run с max_hosts=0: %v", err)
	}
	if res2.Crawl == nil {
		t.Fatal("crawl-отчёта нет при max_hosts=0")
	}
	if res2.Crawl.MaxHosts != 10 {
		t.Errorf("max_hosts=0 - серверный потолок: отчёт назвал %d, хочу 10", res2.Crawl.MaxHosts)
	}
	if res2.Crawled != 2 {
		t.Errorf("серверный потолок 10 при depth=1 обошёл %d страниц, хочу 2 (A и его ссылку)", res2.Crawled)
	}
	if res2.Crawl.LimitHit != "" {
		t.Errorf("потолок не достигнут (2 из 10), а limit_hit=%q: ложное событие", res2.Crawl.LimitHit)
	}
}

// TestRunMaxHostsZeroLimitsSeedsByConfig: молчание по потолку - это
// серверная настройка, а не жёсткие 50. Живой BEFORE факта 2: на сервере
// с потолком 7 молчаливый вызов брал 50 сидов и отчитывался limit_hit
// «достигнут потолок хостов» при обходе семи - потолок был чужим.
// Тест: серверный потолок 2, источник даёт 5 адресов, вызов без потолка:
// срез сидов обязан взять ровно 2, и limit_hit обязан молчать, потому
// что очередь исчерпана, а не срезана.
func TestRunMaxHostsZeroLimitsSeedsByConfig(t *testing.T) {
	st := newStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cr := NewCrawler(&stubCrawl{pages: map[string]string{}}, silentLog{}, fastCrawl(CrawlConfig{Depth: 2, MaxHosts: 2}))
	p := &Pool{
		Store:  st,
		Finder: &Finder{Client: &brokenWriteSource{n: 5}, Log: silentLog{}, Attempts: 1},
		Crawl:  cr,
		Log:    silentLog{},
	}
	res, err := p.Run(ctx, Options{WithCrawl: true, Depth: 1})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Crawl == nil {
		t.Fatal("crawl-отчёта нет")
	}
	if res.Crawled != 2 {
		t.Errorf("молчаливый вызов обошёл %d страниц, хочу 2: срез сидов обязан брать потолок конфига, а не дефолт 50", res.Crawled)
	}
	if res.Crawl.MaxHosts != 2 {
		t.Errorf("отчёт назвал max_hosts=%d, хочу 2 из конфига обходчика", res.Crawl.MaxHosts)
	}
	if res.Crawl.LimitHit != "" {
		t.Errorf("очередь исчерпана (2 из 2), а limit_hit=%q: срез сидов молчаливым потолком лжёт о событии", res.Crawl.LimitHit)
	}
	if res.Crawl.Ok+res.Crawl.Failed != 2 {
		t.Errorf("страниц посчитано %d, хочу 2: каждая обойдённая обязана попасть в отчёт", res.Crawl.Ok+res.Crawl.Failed)
	}
}
