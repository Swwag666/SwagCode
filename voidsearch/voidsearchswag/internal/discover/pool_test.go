package discover

import (
	"context"
	"fmt"
	"testing"

	"voidsearchswag/internal/store"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/pool.db")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// putOnion заводит адрес и задаёт ему живость. Живость задаётся пробами:
// каждая успешная поднимает долю успехов и обнуляет счётчик отказов, каждая
// неудачная его растит. Три отказа подряд переводят адрес в dead, поэтому
// мёртвый адрес помечается тремя пробами.
func putOnion(t *testing.T, st *store.Store, url, status string, rate, latency int) {
	t.Helper()
	ctx := context.Background()
	if err := st.UpsertOnion(ctx, store.Onion{URL: url, Status: status}); err != nil {
		t.Fatalf("upsert %s: %v", url, err)
	}
	switch status {
	case "live":
		for i := 0; i < rate; i++ {
			if err := st.RecordProbe(ctx, url, true, int64(latency)); err != nil {
				t.Fatalf("проба %s: %v", url, err)
			}
		}
	case "dead":
		for i := 0; i < 3; i++ {
			if err := st.RecordProbe(ctx, url, false, 0); err != nil {
				t.Fatalf("проба %s: %v", url, err)
			}
		}
	}
}

func rateOf(t *testing.T, st *store.Store, url string) float64 {
	t.Helper()
	o, err := st.GetOnion(context.Background(), url)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	return o.SuccessRate
}

func TestSeedHostsFreshBeforeDead(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	putOnion(t, st, "dead.onion", "dead", 0, 0)

	p := &Pool{Store: st, Log: silentLog{}}
	cands := []Candidate{
		{Address: "dead.onion"},
		{Address: "fresh.onion"},
	}

	seeds, _ := p.seedHosts(ctx, cands, 2)
	if len(seeds) != 2 {
		t.Fatalf("семян %d, ожидала 2: %+v", len(seeds), seeds)
	}
	// Свежий адрес стоит попытки, мёртвый - только остатка бюджета.
	if seeds[0] != "fresh.onion" {
		t.Errorf("мёртвый впереди свежего: %+v", seeds)
	}
}

func TestSeedHostsLiveThenFreshThenDead(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	putOnion(t, st, "live.onion", "live", 5, 300)
	putOnion(t, st, "dead.onion", "dead", 0, 0)

	p := &Pool{Store: st, Log: silentLog{}}
	cands := []Candidate{
		{Address: "dead.onion"},
		{Address: "live.onion"},
		{Address: "fresh.onion"},
	}

	seeds, _ := p.seedHosts(ctx, cands, 3)
	want := []string{"live.onion", "fresh.onion", "dead.onion"}
	for i, w := range want {
		if i >= len(seeds) || seeds[i] != w {
			t.Fatalf("порядок %+v, ожидала %+v", seeds, want)
		}
	}
}

func TestSeedHostsDeadOnlyWithLeftoverBudget(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	for i, u := range []string{"dead1.onion", "dead2.onion", "dead3.onion"} {
		putOnion(t, st, u, "dead", 0, 0)
		_ = i
	}

	p := &Pool{Store: st, Log: silentLog{}}
	cands := []Candidate{
		{Address: "dead1.onion"},
		{Address: "dead2.onion"},
		{Address: "dead3.onion"},
		{Address: "fresh.onion"},
	}

	// Потолок 2: свежий обязан попасть, из мёртвых - только один.
	seeds, _ := p.seedHosts(ctx, cands, 2)
	if len(seeds) != 2 {
		t.Fatalf("семян %d, ожидала 2: %+v", len(seeds), seeds)
	}
	if seeds[0] != "fresh.onion" {
		t.Errorf("свежий не впереди: %+v", seeds)
	}
}

func TestSeedHostsExcludesKnownLiveDuplicate(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	putOnion(t, st, "live.onion", "live", 5, 300)

	p := &Pool{Store: st, Log: silentLog{}}
	cands := []Candidate{{Address: "live.onion"}, {Address: "fresh.onion"}}

	seeds, _ := p.seedHosts(ctx, cands, 10)
	seen := map[string]int{}
	for _, s := range seeds {
		seen[s]++
	}
	if seen["live.onion"] != 1 {
		t.Errorf("живой адрес дублирован: %+v", seeds)
	}
	if len(seeds) != 2 {
		t.Errorf("семян %d, ожидала 2: %+v", len(seeds), seeds)
	}
}

func TestKnownStatuses(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	putOnion(t, st, "live.onion", "live", 5, 300)
	putOnion(t, st, "dead.onion", "dead", 0, 0)
	putOnion(t, st, "fresh.onion", "unknown", 0, 0)

	got, err := st.KnownStatuses(ctx, []string{"live.onion", "dead.onion", "fresh.onion", "absent.onion"})
	if err != nil {
		t.Fatal(err)
	}
	if got["live.onion"] != "live" || got["dead.onion"] != "dead" || got["fresh.onion"] != "unknown" {
		t.Errorf("статусы неверны: %+v", got)
	}
	if _, ok := got["absent.onion"]; ok {
		t.Errorf("отсутствующий адрес попал в ответ: %+v", got)
	}
}

func TestKnownStatusesEmpty(t *testing.T) {
	st := newStore(t)
	got, err := st.KnownStatuses(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("на пустом списке %+v", got)
	}
}

func TestKnownStatusesChunked(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	// Больше порции в 500: запрос должен пройти целиком, без ошибки.
	urls := make([]string, 0, 1200)
	for i := 0; i < 1200; i++ {
		u := fmt.Sprintf("h%04d.onion", i)
		urls = append(urls, u)
	}
	for i := 0; i < 1200; i += 100 {
		putOnion(t, st, urls[i], "unknown", 0, 0)
	}

	got, err := st.KnownStatuses(ctx, urls)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 12 {
		t.Errorf("статусов %d, ожидала 12", len(got))
	}
}

func TestRecordCrawlMarksLiveAndDead(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	putOnion(t, st, "ok.onion", "unknown", 0, 0)
	putOnion(t, st, "bad.onion", "unknown", 0, 0)

	p := &Pool{Store: st, Log: silentLog{}}
	crep := &CrawlReport{
		PageDetail: []Page{
			{Host: "ok.onion", Status: 200},
			{Host: "bad.onion", Error: "таймаут"},
		},
	}

	if n := p.recordCrawl(ctx, crep, nil); n != 2 {
		t.Fatalf("записано %d хостов, ожидала 2", n)
	}

	ok, err := st.GetOnion(ctx, "ok.onion")
	if err != nil {
		t.Fatal(err)
	}
	if ok.Status != "live" {
		t.Errorf("ответивший хост не стал live: %q", ok.Status)
	}

	bad, err := st.GetOnion(ctx, "bad.onion")
	if err != nil {
		t.Fatal(err)
	}
	if bad.FailStreak != 1 {
		t.Errorf("отказ не учтён: streak=%d", bad.FailStreak)
	}
	// Одного отказа мало для dead: адрес может ответить со второго раза.
	if bad.Status == "dead" {
		t.Error("адрес помечен мёртвым после одного отказа")
	}
}

func TestRecordCrawlThreeFailuresMarksDead(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	putOnion(t, st, "bad.onion", "unknown", 0, 0)

	p := &Pool{Store: st, Log: silentLog{}}
	crep := &CrawlReport{PageDetail: []Page{{Host: "bad.onion", Error: "таймаут"}}}

	for i := 0; i < 3; i++ {
		p.recordCrawl(ctx, crep, nil)
	}

	o, err := st.GetOnion(ctx, "bad.onion")
	if err != nil {
		t.Fatal(err)
	}
	if o.Status != "dead" {
		t.Errorf("три отказа не пометили dead: %q (streak=%d)", o.Status, o.FailStreak)
	}
}

func TestRecordCrawlEmpty(t *testing.T) {
	st := newStore(t)
	p := &Pool{Store: st, Log: silentLog{}}
	if n := p.recordCrawl(context.Background(), &CrawlReport{}, nil); n != 0 {
		t.Errorf("на пустом отчёте записано %d", n)
	}
}

func TestRecordCrawlWithoutStore(t *testing.T) {
	p := &Pool{Log: silentLog{}}
	crep := &CrawlReport{PageDetail: []Page{{Host: "a.onion"}}}
	if n := p.recordCrawl(context.Background(), crep, nil); n != 0 {
		t.Errorf("без базы записано %d", n)
	}
}

func TestRecordCrawlSkipsEmptyHost(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	putOnion(t, st, "ok.onion", "unknown", 0, 0)

	p := &Pool{Store: st, Log: silentLog{}}
	crep := &CrawlReport{PageDetail: []Page{{Host: ""}, {Host: "ok.onion", Status: 200}}}
	if n := p.recordCrawl(ctx, crep, nil); n != 1 {
		t.Errorf("пустой хост не пропущен: записано %d", n)
	}
}

func TestRecordCrawlMakesSeedsNextRunLive(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	putOnion(t, st, "dead1.onion", "unknown", 0, 0)
	putOnion(t, st, "live1.onion", "unknown", 0, 0)

	p := &Pool{Store: st, Log: silentLog{}}
	// Первый прогон: live1 ответил, dead1 - нет.
	crep := &CrawlReport{PageDetail: []Page{
		{Host: "live1.onion", Status: 200},
		{Host: "dead1.onion", Error: "таймаут"},
	}}
	p.recordCrawl(ctx, crep, nil)

	// Второй прогон должен поставить живого впереди мёртвого.
	seeds, _ := p.seedHosts(ctx, []Candidate{{Address: "dead1.onion"}}, 10)
	if len(seeds) == 0 || seeds[0] != "live1.onion" {
		t.Errorf("живой хост не поднялся вперёд: %+v", seeds)
	}
}

func TestKnownKeepsRanking(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	// Живость набирается пробами: больше успешных - выше доля успехов.
	// Латентность сглаживается, поэтому у быстрого адреса она ниже.
	putOnion(t, st, "aaa.onion", "live", 3, 900)
	putOnion(t, st, "bbb.onion", "live", 1, 300)
	putOnion(t, st, "ccc.onion", "live", 5, 200)

	got, err := Known(ctx, st, "live", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("адресов %d, ожидала 3", len(got))
	}
	// Первым идёт самый успешный, а не первый по алфавиту.
	if got[0] != "ccc.onion" {
		t.Errorf("первым %q, ожидала ccc.onion (лучший по живости)", got[0])
	}
	if got[1] != "aaa.onion" {
		t.Errorf("вторым %q, ожидала aaa.onion", got[1])
	}
	if got[2] != "bbb.onion" {
		t.Errorf("третьим %q, ожидала bbb.onion", got[2])
	}
	if rateOf(t, st, "ccc.onion") <= rateOf(t, st, "bbb.onion") {
		t.Fatalf("пробы не дали разброса живости: ccc=%.2f bbb=%.2f",
			rateOf(t, st, "ccc.onion"), rateOf(t, st, "bbb.onion"))
	}
}

func TestKnownFiltersByStatus(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	putOnion(t, st, "live.onion", "live", 9, 100)
	putOnion(t, st, "dead.onion", "dead", 0, 0)
	putOnion(t, st, "fresh.onion", "unknown", 0, 0)

	live, err := Known(ctx, st, "live", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || live[0] != "live.onion" {
		t.Errorf("фильтр live не сработал: %+v", live)
	}

	fresh, err := Known(ctx, st, "unknown", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh) != 1 || fresh[0] != "fresh.onion" {
		t.Errorf("фильтр unknown не сработал: %+v", fresh)
	}
}

func TestKnownNilStore(t *testing.T) {
	if _, err := Known(context.Background(), nil, "live", 5); err == nil {
		t.Error("без базы ошибки нет")
	}
}

func TestKnownEmpty(t *testing.T) {
	st := newStore(t)
	got, err := Known(context.Background(), st, "live", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("на пустом пуле %d адресов", len(got))
	}
}

func TestSeedHostsPrefersLive(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	putOnion(t, st, "live1.onion", "live", 9, 200)
	putOnion(t, st, "live2.onion", "live", 8, 400)

	p := &Pool{Store: st, Log: silentLog{}}
	cands := []Candidate{
		{Address: "fresh1.onion"},
		{Address: "fresh2.onion"},
	}

	seeds, _ := p.seedHosts(ctx, cands, 10)
	if len(seeds) != 4 {
		t.Fatalf("семян %d, ожидала 4: %+v", len(seeds), seeds)
	}
	if seeds[0] != "live1.onion" || seeds[1] != "live2.onion" {
		t.Errorf("живые не впереди: %+v", seeds)
	}
}

func TestSeedHostsRespectsLimit(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	putOnion(t, st, "live1.onion", "live", 9, 200)
	putOnion(t, st, "live2.onion", "live", 8, 300)
	putOnion(t, st, "live3.onion", "live", 7, 400)

	p := &Pool{Store: st, Log: silentLog{}}
	cands := []Candidate{{Address: "fresh1.onion"}}

	seeds, _ := p.seedHosts(ctx, cands, 2)
	if len(seeds) != 2 {
		t.Fatalf("потолок не сработал: %d семян", len(seeds))
	}
	if seeds[0] != "live1.onion" || seeds[1] != "live2.onion" {
		t.Errorf("отбор неверен: %+v", seeds)
	}
}

func TestSeedHostsDeduplicates(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	putOnion(t, st, "live1.onion", "live", 9, 200)

	p := &Pool{Store: st, Log: silentLog{}}
	cands := []Candidate{
		{Address: "live1.onion"},
		{Address: "fresh1.onion"},
		{Address: "fresh1.onion"},
	}

	seeds, _ := p.seedHosts(ctx, cands, 10)
	if len(seeds) != 2 {
		t.Fatalf("дубли не схлопнуты: %+v", seeds)
	}
}

func TestSeedHostsWithoutStore(t *testing.T) {
	p := &Pool{Log: silentLog{}}
	cands := []Candidate{{Address: "a.onion"}, {Address: "b.onion"}}
	seeds, _ := p.seedHosts(context.Background(), cands, 10)
	if len(seeds) != 2 {
		t.Fatalf("без базы семена потеряны: %+v", seeds)
	}
}

func TestSeedHostsDefaultLimit(t *testing.T) {
	p := &Pool{Log: silentLog{}}
	cands := make([]Candidate, 0, 60)
	for i := 0; i < 60; i++ {
		cands = append(cands, Candidate{Address: string(rune('a'+i%26)) + string(rune('a'+i/26)) + ".onion"})
	}
	seeds, _ := p.seedHosts(context.Background(), cands, 0)
	if len(seeds) != 50 {
		t.Errorf("дефолтный потолок дал %d семян, ожидала 50", len(seeds))
	}
}

func TestSeedHostsFreshWhenNoLive(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	putOnion(t, st, "dead.onion", "dead", 0, 0)

	p := &Pool{Store: st, Log: silentLog{}}
	cands := []Candidate{{Address: "fresh1.onion"}, {Address: "fresh2.onion"}}

	seeds, _ := p.seedHosts(ctx, cands, 10)
	if len(seeds) != 2 {
		t.Fatalf("свежие не взяты при отсутствии живых: %+v", seeds)
	}
	if seeds[0] != "fresh1.onion" {
		t.Errorf("порядок свежих не сохранён: %+v", seeds)
	}
}
