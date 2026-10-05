package discover

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"voidsearchswag/internal/httpc"
	"voidsearchswag/internal/store"
)

// brokenWriteSource отдаёт страницу с несколькими валидными v3-адресами. Адреса
// строятся из base32-алфавита и различаются последним символом, чтобы Merge не
// схлопнул их в один.
type brokenWriteSource struct{ n int }

func (s *brokenWriteSource) Fetch(ctx context.Context, r httpc.Request) (*httpc.Response, error) {
	var b strings.Builder
	b.WriteString("<html><body>")
	for i := 0; i < s.n; i++ {
		addr := v3a[:55] + string(rune('a'+i))
		b.WriteString(`<a href="http://` + addr + `.onion/">сервис ` + addr[:8] + `</a>` + "\n")
	}
	b.WriteString("</body></html>")
	return &httpc.Response{Status: 200, Body: []byte(b.String())}, nil
}

// decodeResult разбирает машинный отчёт прогона. Проверка идёт по JSON, а не по
// полям структуры, чтобы один и тот же тест компилировался и до правки, и после
// неё: контракт потребителя - именно JSON.
func decodeResult(t *testing.T, res Result) map[string]any {
	t.Helper()
	blob, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(blob, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// numOf возвращает счётчик из отчёта или -1, если поля нет вовсе.
func numOf(t *testing.T, m map[string]any, key string) int {
	t.Helper()
	v, ok := m[key]
	if !ok {
		return -1
	}
	f, ok := v.(float64)
	if !ok {
		t.Errorf("%s не число: %#v", key, v)
		return -1
	}
	return int(f)
}

func runBrokenPool(t *testing.T, n int, migrate bool) Result {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/pool.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if migrate {
		if err := st.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
	}
	p := &Pool{Store: st, Finder: &Finder{Client: &brokenWriteSource{n: n}, Log: silentLog{}, Attempts: 1}}
	res, err := p.Run(ctx, Options{})
	if err != nil {
		t.Fatalf("Run вернул ошибку: %v", err)
	}
	return res
}

func TestRunReportsFailedPoolWrites(t *testing.T) {
	// Замер: база открыта без миграции, поэтому и чтение, и запись onion_pool
	// падают. Все найденные адреса теряются, а отчёт выглядит как «найдено 3,
	// новых 0» - точно так же, как если бы адреса уже были известны.
	res := runBrokenPool(t, 3, false)

	if res.Found < 3 {
		t.Fatalf("найдено %d адресов, ожидала не меньше 3", res.Found)
	}
	if res.New != 0 || res.Updated != 0 {
		t.Fatalf("новых %d, обновлено %d при мёртвой базе", res.New, res.Updated)
	}
	m := decodeResult(t, res)
	if numOf(t, m, "save_failed") <= 0 {
		t.Errorf("в машинном отчёте нет признака отказавшей записи: %s", mustJSON(t, m))
	}
	if reason, _ := m["last_error"].(string); reason == "" {
		t.Errorf("в отчёте нет причины отказа: %s", mustJSON(t, m))
	}
}

func TestRunCountsSaveFailures(t *testing.T) {
	// Число отказов обязано совпадать с числом потерянных адресов: без этого
	// «отказов 1» при трёх потерянных адресах объяснило бы только треть потери.
	res := runBrokenPool(t, 4, false)
	m := decodeResult(t, res)
	if got := numOf(t, m, "save_failed"); got != res.Found {
		t.Errorf("save_failed = %d при %d найденных адресах: %s", got, res.Found, mustJSON(t, m))
	}
	reason, _ := m["last_error"].(string)
	if !strings.Contains(reason, "onion_pool") && !strings.Contains(reason, "no such table") {
		t.Errorf("причина не объясняет поломку базы: %q", reason)
	}
}

func TestRunHealthyBaseReportsNoFailures(t *testing.T) {
	// Обратная сторона: на живой базе счётчики отказов обязаны оставаться
	// нулями, иначе они перестанут означать поломку.
	res := runBrokenPool(t, 3, true)
	m := decodeResult(t, res)

	if res.New == 0 {
		t.Errorf("новые адреса не записаны при живой базе: found=%d", res.Found)
	}
	for _, key := range []string{"save_failed", "file_save_failed", "probe_failed"} {
		if got := numOf(t, m, key); got != 0 {
			t.Errorf("%s = %d на живой базе: %s", key, got, mustJSON(t, m))
		}
	}
	if reason, _ := m["last_error"].(string); reason != "" {
		t.Errorf("причина отказа заполнена на живой базе: %q", reason)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	blob, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(blob)
}

func TestRecordCrawlCountsProbeFailures(t *testing.T) {
	// Возвращаемое число считает удачные записи, поэтому ноль при мёртвой базе
	// выглядел как «обход не выяснил живости», а не как «пробы не записались».
	st, err := store.Open(t.TempDir() + "/brokenprobe.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	p := &Pool{Store: st, Log: silentLog{}}
	crep := &CrawlReport{PageDetail: []Page{
		{Host: "a.onion", Status: 200},
		{Host: "b.onion", Status: 200},
	}}
	var res Result
	if n := p.recordCrawl(context.Background(), crep, &res); n != 0 {
		t.Errorf("записано %d проб при мёртвой базе", n)
	}
	if res.ProbeFailed != 2 {
		t.Errorf("ProbeFailed = %d, ожидала 2", res.ProbeFailed)
	}
	if res.LastError == "" {
		t.Error("причина отказа записи проб не сохранена")
	}
}

func TestRecordCrawlNilResultSafe(t *testing.T) {
	// Приёмник может быть nil: существующие тесты recordCrawl проверяют саму
	// запись живости, и падать на отсутствующем отчёте они не должны.
	st := newStore(t)
	putOnion(t, st, "ok.onion", "unknown", 0, 0)
	p := &Pool{Store: st, Log: silentLog{}}
	crep := &CrawlReport{PageDetail: []Page{{Host: "ok.onion", Status: 200}}}
	if n := p.recordCrawl(context.Background(), crep, nil); n != 1 {
		t.Errorf("записано %d, ожидала 1", n)
	}
}

func TestResultFailMethodsIgnoreNilError(t *testing.T) {
	var res Result
	res.FailSave(nil)
	res.FailFile(nil)
	res.FailProbe(nil)
	if res.SaveFailed != 0 || res.FileSaveFailed != 0 || res.ProbeFailed != 0 || res.LastError != "" {
		t.Errorf("nil-ошибка учтена как отказ: %+v", res)
	}
}

func TestResultFailMethodsNilReceiverSafe(t *testing.T) {
	var res *Result
	res.FailSave(errors.New("отказ"))
	res.FailFile(errors.New("отказ"))
	res.FailProbe(errors.New("отказ"))
}

func TestResultLastErrorKeepsFirstReason(t *testing.T) {
	// При массовой поломке все отказы одинаковы, и объясняет картину первый:
	// последующие не должны его затирать.
	var res Result
	res.FailSave(errors.New("нет таблицы onion_pool"))
	res.FailFile(errors.New("нет таблицы files"))
	res.FailProbe(errors.New("нет таблицы probes"))

	if res.LastError != "нет таблицы onion_pool" {
		t.Errorf("LastError = %q, ожидала первую причину", res.LastError)
	}
	if res.SaveFailed != 1 || res.FileSaveFailed != 1 || res.ProbeFailed != 1 {
		t.Errorf("счётчики смешаны: %+v", res)
	}
}

func TestRunCountsFileWriteFailures(t *testing.T) {
	// Обход нашёл файлы, база их не приняла. Files считает записанные, поэтому
	// «файлов 0» без парного счётчика означало «файлов не нашлось».
	st, err := store.Open(t.TempDir() + "/brokenfiles.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	page := "http://" + cHost + "/"
	body := `<html><body><a href="/files/dump.zip">дамп</a><a href="/files/keys.7z">ключи</a></body></html>`
	cr := NewCrawler(&stubCrawl{pages: map[string]string{page: body}}, silentLog{}, fastCrawl(CrawlConfig{Depth: 1}))
	p := &Pool{
		Store:  st,
		Finder: &Finder{Client: &brokenWriteSource{n: 2}, Log: silentLog{}, Attempts: 1},
		Crawl:  cr,
		Log:    silentLog{},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res, err := p.Run(ctx, Options{WithCrawl: true, Seeds: []string{cHost}, MaxHosts: 2})
	if err != nil {
		t.Fatalf("Run вернул ошибку: %v", err)
	}
	if res.Crawl == nil {
		t.Fatal("отчёт обхода не заполнен")
	}
	if len(res.Crawl.FileRefs) == 0 {
		t.Fatalf("обход не нашёл файлов, тест ничего не измеряет: %+v", res.Crawl)
	}
	if res.Files != 0 {
		t.Errorf("Files = %d при мёртвой базе", res.Files)
	}
	if res.FileSaveFailed != len(res.Crawl.FileRefs) {
		t.Errorf("FileSaveFailed = %d, ожидала %d", res.FileSaveFailed, len(res.Crawl.FileRefs))
	}
	// Живость по итогам обхода пишется в ту же мёртвую базу, поэтому и пробы
	// обязаны попасть в свой счётчик.
	if res.ProbeFailed == 0 {
		t.Errorf("отказы записи проб не учтены: probed=%d", res.Probed)
	}
	if res.LastError == "" {
		t.Error("причина отказа не сохранена")
	}
}
