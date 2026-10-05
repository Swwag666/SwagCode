package discover

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"voidsearchswag/internal/httpc"
)

// noTorClient собирает клиент без ротатора. На таком любой onion-запрос падает
// с httpc.ErrOnionWithoutTor ещё до обращения к сети - ровно так, как на машине
// без tor и без прокси. Клиент нужен, чтобы отличить «сервис не ответил» от
// «запрос физически не мог уйти».
func noTorClient(t *testing.T) *httpc.Client {
	t.Helper()
	cl, err := httpc.NewClient(context.Background(), httpc.Options{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("клиент без tor: %v", err)
	}
	return cl
}

// fastProbe укорачивает паузу между запросами: беречь в тесте нечего, а
// задержка ротатора по умолчанию в две секунды растянула бы три пробы на
// четыре секунды ожидания.
func fastProbe() ProbeConfig {
	return ProbeConfig{Concurrency: 1, Timeout: 2 * time.Second, Delay: time.Millisecond}
}

func TestProbeWithoutTorKeepsHostLive(t *testing.T) {
	// Живой адрес и три «пробы» на машине без tor. Запрос ни разу не ушёл в
	// сеть, но каждая ошибка транспорта записывалась как отказ сервиса: на
	// третьей срабатывал порог fail_streak и адрес становился мёртвым.
	st := newStore(t)
	ctx := context.Background()
	putOnion(t, st, cHost, "live", 4, 1800)

	p := NewProber(noTorClient(t), st, silentLog{}, fastProbe())
	for i := 0; i < 3; i++ {
		if rep := p.Probe(ctx, []string{cHost}); rep.Total != 1 {
			t.Fatalf("total=%d, ожидала 1", rep.Total)
		}
	}

	got, err := st.GetOnion(ctx, cHost)
	if err != nil {
		t.Fatalf("чтение адреса: %v", err)
	}
	if got.Status != "live" {
		t.Errorf("статус %q, ожидала live: отсутствие tor записано как смерть сервиса", got.Status)
	}
	if got.FailStreak != 0 {
		t.Errorf("fail_streak=%d, ожидала 0: запросы не уходили в сеть", got.FailStreak)
	}
	if got.SuccessRate == 0 {
		t.Error("доля успехов обнулена пробой, которой не было")
	}
}

func TestProbeWithoutTorDoesNotCreatePoolRow(t *testing.T) {
	// Адреса нет в пуле. Проба без tor не выяснила о нём ничего, поэтому и
	// строки в пуле появиться не должно: иначе пул наполняется записями,
	// единственным фактом о которых является «у автора не было tor».
	st := newStore(t)
	p := NewProber(noTorClient(t), st, silentLog{}, fastProbe())

	if rep := p.Probe(context.Background(), []string{cHost}); rep.Total != 1 {
		t.Fatalf("total=%d, ожидала 1", rep.Total)
	}
	if _, err := st.GetOnion(context.Background(), cHost); err == nil {
		t.Error("строка в пуле создана пробой, которая не состоялась")
	}
}

func TestProbeReportCountsSkippedSeparately(t *testing.T) {
	// Отчёт проверяется через JSON: контракт потребителя именно он, и через
	// json.Marshal тест собирается и до правки, и после неё.
	st := newStore(t)
	p := NewProber(noTorClient(t), st, silentLog{}, fastProbe())
	rep := p.Probe(context.Background(), []string{cHost})

	blob := mustJSON(t, rep)
	var m map[string]any
	if err := json.Unmarshal([]byte(blob), &m); err != nil {
		t.Fatal(err)
	}
	if got := numOf(t, m, "skipped"); got != 1 {
		t.Errorf("skipped=%d, ожидала 1: %s", got, blob)
	}
	if dead, _ := m["dead"].(float64); dead != 0 {
		t.Errorf("dead=%v, ожидала 0: проба без tor не делает адрес мёртвым (%s)", m["dead"], blob)
	}
	res, _ := m["results"].([]any)
	if len(res) != 1 {
		t.Fatalf("результатов %d, ожидала 1: %s", len(res), blob)
	}
	one, _ := res[0].(map[string]any)
	if nt, _ := one["no_transport"].(bool); !nt {
		t.Errorf("в результате нет признака отсутствия транспорта: %s", blob)
	}
	if reason, _ := one["error"].(string); reason == "" {
		t.Errorf("причина пропуска не названа: %s", blob)
	}
}

// deadProxyClient собирает клиент с транспортом через заведомо мёртвый прокси.
// Onion-барьер такой клиент проходит (прокси задан), а запрос падает на
// соединении: это ошибка сети, а не отсутствие транспорта, и она обязана
// считаться отказом сервиса.
func deadProxyClient(t *testing.T) *httpc.Client {
	t.Helper()
	cl, err := httpc.NewClient(context.Background(), httpc.Options{
		Transport: "static",
		Proxies:   []string{"127.0.0.1:1"},
		Timeout:   2 * time.Second,
	})
	if err != nil {
		t.Fatalf("клиент с мёртвым прокси: %v", err)
	}
	return cl
}

func TestProbeRealRefusalStillCounted(t *testing.T) {
	// Обратная сторона: настоящий отказ обязан учитываться как раньше. Прокси
	// задан, поэтому onion-барьер пройден и ошибка приходит от сети.
	st := newStore(t)
	ctx := context.Background()
	putOnion(t, st, cHost, "live", 4, 1800)
	p := NewProber(deadProxyClient(t), st, silentLog{}, fastProbe())

	rep := p.Probe(ctx, []string{cHost})
	blob := mustJSON(t, rep)
	if strings.Contains(blob, "недостижим напрямую") {
		t.Fatalf("замер испорчен: клиент ответил отказом транспорта: %s", blob)
	}
	if rep.Total != 1 {
		t.Errorf("total=%d, ожидала 1", rep.Total)
	}
	if rep.Dead != 1 {
		t.Errorf("dead=%d, ожидала 1: отказ перестал считаться (%s)", rep.Dead, blob)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(blob), &m); err != nil {
		t.Fatal(err)
	}
	if got := numOf(t, m, "skipped"); got > 0 {
		t.Errorf("skipped=%d, ожидала 0: настоящий отказ записан в пропуски (%s)", got, blob)
	}
	got, err := st.GetOnion(ctx, cHost)
	if err != nil {
		t.Fatalf("отказ не записан в пул: %v", err)
	}
	if got.FailStreak != 1 {
		t.Errorf("fail_streak=%d, ожидала 1", got.FailStreak)
	}
}

func TestProbeWaveWithoutTorSparesWholePool(t *testing.T) {
	// Волна проб без tor на живом пуле: именно так пользователь без tor одним
	// запуском `probe --limit N` переводил N живых адресов в мёртвые.
	st := newStore(t)
	ctx := context.Background()
	second := v3a[:55] + "z.onion"
	putOnion(t, st, cHost, "live", 4, 1800)
	putOnion(t, st, second, "live", 4, 1800)
	p := NewProber(noTorClient(t), st, silentLog{}, fastProbe())

	rep := p.Probe(ctx, []string{cHost, second})
	if rep.Total != 2 {
		t.Fatalf("total=%d, ожидала 2", rep.Total)
	}
	if rep.Dead != 0 {
		t.Errorf("dead=%d, ожидала 0: %s", rep.Dead, mustJSON(t, rep))
	}
	blob := mustJSON(t, rep)
	var m map[string]any
	if err := json.Unmarshal([]byte(blob), &m); err != nil {
		t.Fatal(err)
	}
	if got := numOf(t, m, "skipped"); got != 2 {
		t.Errorf("skipped=%d, ожидала 2: %s", got, blob)
	}
	for _, host := range []string{cHost, second} {
		got, err := st.GetOnion(ctx, host)
		if err != nil {
			t.Fatalf("%s: %v", host, err)
		}
		if got.Status != "live" || got.FailStreak != 0 {
			t.Errorf("%s испорчен пробой без tor: status=%q streak=%d", host, got.Status, got.FailStreak)
		}
	}
}

func TestCrawlWithoutTorDoesNotKillHost(t *testing.T) {
	// Обход в той же ситуации: клиент возвращает ошибку транспорта для каждой
	// страницы. Отчёт при этом печатал «живость: обновлено по обходу 1 хостов»,
	// то есть подавал отсутствующий tor как полезную работу.
	st := newStore(t)
	ctx := context.Background()
	putOnion(t, st, cHost, "live", 4, 1800)

	cr := NewCrawler(&stubCrawl{err: fmt.Errorf("%w: %s", httpc.ErrOnionWithoutTor, cHost)}, silentLog{}, fastCrawl(CrawlConfig{Depth: 1}))
	_, crep := cr.Crawl(ctx, []string{cHost})
	if len(crep.PageDetail) != 1 {
		t.Fatalf("страниц в отчёте %d, ожидала 1", len(crep.PageDetail))
	}

	p := &Pool{Store: st, Log: silentLog{}}
	var res Result
	if n := p.recordCrawl(ctx, &crep, &res); n != 0 {
		t.Errorf("записано %d проб, ожидала 0: обход без tor живость не выяснял", n)
	}
	m := decodeResult(t, res)
	blob := mustJSON(t, m)
	if got := numOf(t, m, "probe_skipped"); got != 1 {
		t.Errorf("probe_skipped=%d, ожидала 1: %s", got, blob)
	}
	if got := numOf(t, m, "probed"); got != 0 {
		t.Errorf("probed=%d, ожидала 0: %s", got, blob)
	}
	got, err := st.GetOnion(ctx, cHost)
	if err != nil {
		t.Fatal(err)
	}
	if got.FailStreak != 0 {
		t.Errorf("fail_streak=%d, ожидала 0: живой хост испорчен обходом без tor", got.FailStreak)
	}
}

func TestRecordCrawlStillRecordsRealFailures(t *testing.T) {
	// Обычный отказ страницы (таймаут, HTTP 500) остаётся вердиктом живости:
	// запрос ушёл, сервис не ответил, это факт о сервисе. Пропуском такая
	// страница считаться не должна - проверка через «больше нуля», чтобы тест
	// собирался и до появления поля.
	st := newStore(t)
	ctx := context.Background()
	putOnion(t, st, "slow.onion", "unknown", 0, 0)

	p := &Pool{Store: st, Log: silentLog{}}
	crep := &CrawlReport{PageDetail: []Page{{Host: "slow.onion", Error: "таймаут"}}}
	var res Result
	if n := p.recordCrawl(ctx, crep, &res); n != 1 {
		t.Errorf("записано %d, ожидала 1: настоящий отказ перестал учитываться", n)
	}
	m := decodeResult(t, res)
	if got := numOf(t, m, "probe_skipped"); got > 0 {
		t.Errorf("probe_skipped=%d, ожидала 0: %s", got, mustJSON(t, m))
	}
	got, err := st.GetOnion(ctx, "slow.onion")
	if err != nil {
		t.Fatal(err)
	}
	if got.FailStreak != 1 {
		t.Errorf("fail_streak=%d, ожидала 1", got.FailStreak)
	}
}

// recLog собирает служебные сообщения вместо того, чтобы их глушить: строка
// итога волны видна только в --verbose, и она обязана говорить то же, что отчёт.
type recLog struct{ lines []string }

func (l *recLog) Infof(format string, args ...any) {
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *recLog) Warnf(format string, args ...any) {
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func TestProbeLogNamesSkipped(t *testing.T) {
	st := newStore(t)
	log := &recLog{}
	p := NewProber(noTorClient(t), st, log, fastProbe())
	p.Probe(context.Background(), []string{cHost})

	joined := strings.Join(log.lines, "\n")
	if !strings.Contains(joined, "пропущено 1") {
		t.Errorf("служебный лог не назвал пропуск, «живых 0 из 1» звучит как вердикт:\n%s", joined)
	}
	if !strings.Contains(joined, "нужен tor или прокси") {
		t.Errorf("причина пропуска не названа:\n%s", joined)
	}
}

func TestProbeLogUnchangedWhenTransportPresent(t *testing.T) {
	// Обратная сторона: при живом транспорте строка лога остаётся прежней, без
	// оговорки о tor, которая шумела бы в каждом нормальном прогоне.
	st := newStore(t)
	log := &recLog{}
	p := NewProber(deadProxyClient(t), st, log, fastProbe())
	p.Probe(context.Background(), []string{cHost})

	joined := strings.Join(log.lines, "\n")
	if strings.Contains(joined, "пропущено") {
		t.Errorf("оговорка о пропусках появилась при живом транспорте:\n%s", joined)
	}
	if !strings.Contains(joined, "probe: живых") {
		t.Errorf("строка итога волны потеряна:\n%s", joined)
	}
}

func TestSkipProbeToleratesNilResult(t *testing.T) {
	// Приёмник проверяется на nil, как у остальных счётчиков Result: recordCrawl
	// вызывают и без приёмника результата, и паника там уронила бы фоновый тик.
	var res *Result
	res.SkipProbe()
}
