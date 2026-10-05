package searchers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"voidsearchswag/internal/httpc"
)

// directClient собирает клиент на прямом транспорте: через httptest-сервер
// проверяется разбор и отчёты без tor и без интернета.
func directClient(t *testing.T) *httpc.Client {
	t.Helper()
	c, err := httpc.NewClient(context.Background(), httpc.Options{
		Transport: "direct",
		Timeout:   10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// stubEngine отдаёт движок, чей Base указывает на стаб-сервер.
func stubEngine(t *testing.T, body string, status int, name string) *OnionEngine {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return &OnionEngine{
		Name_:    name,
		Base:     srv.URL,
		Path:     "/search?q={q}",
		Selector: "a.res",
		Client:   directClient(t),
	}
}

const resBody = `<html><body>
<a class="res" href="http://abcdefabcdefabcd.onion/">Каталог файлов</a>
<a class="res" href="http://bcdefabcdefabcde.onion/">Форум</a>
</body></html>`

func TestNewHealthPoolSeedsEngines(t *testing.T) {
	e1 := &OnionEngine{Name_: "torch", Base: "http://a.onion", Path: "/search?q={q}"}
	e2 := &OnionEngine{Name_: "ahmia", Base: "http://b.onion", Path: "/s"}
	hp := NewHealthPool(nil, []*OnionEngine{e1, e2})

	if live, total := hp.HealthyCount(); live != 0 || total != 2 {
		t.Errorf("live=%d total=%d, ожидала 0/2", live, total)
	}
	st, ok := hp.Get("torch")
	if !ok {
		t.Fatal("движок не зарегистрирован")
	}
	if st.URL != e1.ProbeURL() {
		t.Errorf("URL=%q, ожидала %q", st.URL, e1.ProbeURL())
	}
	if st.Probes != 0 || st.Successes != 0 {
		t.Errorf("счётчики не нулевые: %d/%d", st.Probes, st.Successes)
	}
	if _, ok := hp.Get("нет-такого"); ok {
		t.Error("несуществующий движок найден")
	}
}

func TestNewHealthPoolEmpty(t *testing.T) {
	hp := NewHealthPool(nil, nil)
	if live, total := hp.HealthyCount(); live != 0 || total != 0 {
		t.Errorf("live=%d total=%d", live, total)
	}
	if got := hp.All(); len(got) != 0 {
		t.Errorf("All()=%d записей", len(got))
	}
	if got := hp.Live(); len(got) != 0 {
		t.Errorf("Live()=%d записей", len(got))
	}
}

func TestProbeOneSuccessMarksLive(t *testing.T) {
	e := stubEngine(t, resBody, 200, "torch")
	hp := NewHealthPool(e.Client, []*OnionEngine{e})

	hp.ProbeAll(context.Background(), []*OnionEngine{e})

	st, _ := hp.Get("torch")
	if !st.Live {
		t.Error("движок не отмечен живым")
	}
	if st.Probes != 1 || st.Successes != 1 {
		t.Errorf("счётчики %d/%d", st.Probes, st.Successes)
	}
	if st.SuccessRate != 1 {
		t.Errorf("SuccessRate=%v, ожидала 1", st.SuccessRate)
	}
	if st.FailStreak != 0 {
		t.Errorf("FailStreak=%d", st.FailStreak)
	}
	if st.Disabled {
		t.Error("живой движок отключён")
	}
	if st.LastProbe.IsZero() {
		t.Error("время пробы не записано")
	}
	if st.LatencyAvg < 0 {
		t.Errorf("латентность %d", st.LatencyAvg)
	}
	if live, total := hp.HealthyCount(); live != 1 || total != 1 {
		t.Errorf("live=%d total=%d", live, total)
	}
	if got := hp.Live(); len(got) != 1 || got[0].Name != "torch" {
		t.Errorf("Live()=%+v", got)
	}
}

func TestProbeOneLatencyIsSmoothed(t *testing.T) {
	e := stubEngine(t, resBody, 200, "torch")
	hp := NewHealthPool(e.Client, []*OnionEngine{e})

	for i := 0; i < 4; i++ {
		hp.ProbeAll(context.Background(), []*OnionEngine{e})
	}
	st, _ := hp.Get("torch")
	if st.Probes != 4 || st.Successes != 4 {
		t.Errorf("счётчики %d/%d, ожидала 4/4", st.Probes, st.Successes)
	}
	if st.SuccessRate != 1 {
		t.Errorf("SuccessRate=%v", st.SuccessRate)
	}
}

func TestProbeOneHTTPErrorMarksDead(t *testing.T) {
	e := stubEngine(t, "не найдено", 404, "torch")
	hp := NewHealthPool(e.Client, []*OnionEngine{e})

	hp.ProbeAll(context.Background(), []*OnionEngine{e})

	st, _ := hp.Get("torch")
	if st.Live {
		t.Error("движок с 404 отмечен живым")
	}
	if st.FailStreak != 1 {
		t.Errorf("FailStreak=%d", st.FailStreak)
	}
	if !strings.Contains(st.LastError, "404") {
		t.Errorf("ошибка не называет код: %q", st.LastError)
	}
	if st.Successes != 0 {
		t.Errorf("успехов %d при ошибке", st.Successes)
	}
}

func TestProbeOneEmptyBodyCountsAsFailure(t *testing.T) {
	// Пустое тело означает заглушку или challenge: движок формально ответил
	// 200, но результатов из него нет, поэтому считать его живым нельзя.
	e := stubEngine(t, "", 200, "torch")
	hp := NewHealthPool(e.Client, []*OnionEngine{e})

	hp.ProbeAll(context.Background(), []*OnionEngine{e})

	st, _ := hp.Get("torch")
	if st.Live {
		t.Error("движок с пустым телом отмечен живым")
	}
	if st.FailStreak != 1 {
		t.Errorf("FailStreak=%d", st.FailStreak)
	}
}

func TestProbeThreeFailuresDisablesEngine(t *testing.T) {
	e := stubEngine(t, "ошибка", 503, "torch")
	hp := NewHealthPool(e.Client, []*OnionEngine{e})

	for i := 0; i < healthFailThreshold; i++ {
		hp.ProbeAll(context.Background(), []*OnionEngine{e})
		st, _ := hp.Get("torch")
		if st.FailStreak != i+1 {
			t.Fatalf("после %d проб FailStreak=%d", i+1, st.FailStreak)
		}
		if st.Disabled && i < healthFailThreshold-1 {
			t.Fatalf("движок отключён раньше порога на %d-й пробе", i+1)
		}
	}

	st, _ := hp.Get("torch")
	if !st.Disabled {
		t.Error("движок не отключён после трёх отказов")
	}
	if st.SuccessRate != 0 {
		t.Errorf("SuccessRate=%v", st.SuccessRate)
	}
	if got := hp.Live(); len(got) != 0 {
		t.Errorf("отключённый движок попал в Live(): %+v", got)
	}
	if live, _ := hp.HealthyCount(); live != 0 {
		t.Errorf("live=%d", live)
	}
}

func TestProbeRecoveryClearsDisabled(t *testing.T) {
	// Отключённый движок обязан возвращаться в строй: иначе разовый сбой
	// выключает его до перезапуска процесса. Возврат не мгновенный - доля
	// успехов должна подняться до healthReturnScore, иначе движок,
	// отвечающий через раз, снова попадёт в выдачу.
	e := stubEngine(t, "ошибка", 503, "torch")
	hp := NewHealthPool(e.Client, []*OnionEngine{e})

	for i := 0; i < healthFailThreshold; i++ {
		hp.ProbeAll(context.Background(), []*OnionEngine{e})
	}
	if st, _ := hp.Get("torch"); !st.Disabled {
		t.Fatal("движок не отключился")
	}

	// После трёх отказов пробы 3, успехи 0. Один успех даёт 1/4 = 0.25,
	// что ниже порога возврата: движок жив, но остаётся отключённым.
	hp.MarkResult("torch", true, 50*time.Millisecond)
	st, _ := hp.Get("torch")
	if !st.Live {
		t.Error("после успеха движок не отмечен живым")
	}
	if st.FailStreak != 0 {
		t.Errorf("FailStreak=%d, ожидала 0", st.FailStreak)
	}
	if !st.Disabled {
		t.Error("движок вернулся в строй при доле успехов ниже порога")
	}

	// Ещё два успеха дают 3/6 = 0.5: порог достигнут, движок снова в строю.
	hp.MarkResult("torch", true, 50*time.Millisecond)
	hp.MarkResult("torch", true, 50*time.Millisecond)
	st, _ = hp.Get("torch")
	if st.Disabled {
		t.Errorf("движок не вернулся при доле успехов %v (порог %v)", st.SuccessRate, healthReturnScore)
	}
	if st.SuccessRate < healthReturnScore {
		t.Errorf("SuccessRate=%v ниже порога %v", st.SuccessRate, healthReturnScore)
	}
	if got := hp.Live(); len(got) != 1 {
		t.Errorf("оживший движок не попал в Live(): %+v", got)
	}
}

func TestProbeUnreachableEngine(t *testing.T) {
	e := &OnionEngine{
		Name_:  "dead",
		Base:   "http://127.0.0.1:1",
		Path:   "/search?q={q}",
		Client: directClient(t),
	}
	hp := NewHealthPool(e.Client, []*OnionEngine{e})

	hp.ProbeAll(context.Background(), []*OnionEngine{e})

	st, _ := hp.Get("dead")
	if st.Live {
		t.Error("недоступный движок отмечен живым")
	}
	if st.LastError == "" {
		t.Error("причина отказа не записана")
	}
	if st.Probes != 1 {
		t.Errorf("Probes=%d", st.Probes)
	}
}

func TestProbeOneAddsUnknownEngine(t *testing.T) {
	// Движок, которого нет в пуле, обязан появиться после пробы: иначе
	// отчёт теряет движки, добавленные после сборки ядра.
	e := stubEngine(t, resBody, 200, "новый")
	hp := NewHealthPool(e.Client, nil)

	hp.ProbeAll(context.Background(), []*OnionEngine{e})

	st, ok := hp.Get("новый")
	if !ok {
		t.Fatal("движок не добавлен в пул")
	}
	if !st.Live {
		t.Error("движок не отмечен живым")
	}
}

func TestProbeAllConcurrent(t *testing.T) {
	e1 := stubEngine(t, resBody, 200, "a")
	e2 := stubEngine(t, resBody, 200, "b")
	e3 := stubEngine(t, "ошибка", 500, "c")
	hp := NewHealthPool(e1.Client, []*OnionEngine{e1, e2, e3})

	hp.ProbeAll(context.Background(), []*OnionEngine{e1, e2, e3})

	live, total := hp.HealthyCount()
	if total != 3 {
		t.Errorf("total=%d, ожидала 3", total)
	}
	if live != 2 {
		t.Errorf("live=%d, ожидала 2", live)
	}
	// Отчёт обязан быть упорядочен по качеству, а не по случайному
	// завершению горутин: иначе вывод прыгает между прогонами.
	all := hp.All()
	if len(all) != 3 {
		t.Fatalf("All()=%d", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i].SuccessRate > all[i-1].SuccessRate {
			t.Errorf("порядок нарушен: %s (%v) после %s (%v)",
				all[i].Name, all[i].SuccessRate, all[i-1].Name, all[i-1].SuccessRate)
		}
	}
}

func TestMarkResultUpdatesStats(t *testing.T) {
	e := &OnionEngine{Name_: "torch", Base: "http://a.onion"}
	hp := NewHealthPool(nil, []*OnionEngine{e})

	hp.MarkResult("torch", true, 100*time.Millisecond)
	st, _ := hp.Get("torch")
	if !st.Live || st.LatencyAvg != 100 {
		t.Errorf("live=%v latency=%d", st.Live, st.LatencyAvg)
	}

	hp.MarkResult("torch", true, 300*time.Millisecond)
	st, _ = hp.Get("torch")
	// Скользящее среднее: (100*3+300)/4 = 150.
	if st.LatencyAvg != 150 {
		t.Errorf("сглаживание неверно: %d, ожидала 150", st.LatencyAvg)
	}
	if st.SuccessRate != 1 {
		t.Errorf("SuccessRate=%v", st.SuccessRate)
	}
}

func TestMarkResultFailuresDisable(t *testing.T) {
	e := &OnionEngine{Name_: "torch", Base: "http://a.onion"}
	hp := NewHealthPool(nil, []*OnionEngine{e})

	for i := 0; i < healthFailThreshold; i++ {
		hp.MarkResult("torch", false, 0)
	}
	st, _ := hp.Get("torch")
	if st.Live || !st.Disabled {
		t.Errorf("после %d отказов live=%v disabled=%v", healthFailThreshold, st.Live, st.Disabled)
	}
	if st.FailStreak != healthFailThreshold {
		t.Errorf("FailStreak=%d", st.FailStreak)
	}
}

func TestMarkResultUnknownEngineIgnored(t *testing.T) {
	hp := NewHealthPool(nil, nil)
	// Чужое имя не должно создавать запись: иначе отчёт наполняется
	// движками, которых в ядре нет.
	hp.MarkResult("призрак", true, time.Second)
	if _, ok := hp.Get("призрак"); ok {
		t.Error("неизвестный движок добавлен в пул")
	}
	if _, total := hp.HealthyCount(); total != 0 {
		t.Errorf("total=%d", total)
	}
}

func TestHealthPoolReturnsCopies(t *testing.T) {
	// Get и All обязаны отдавать копию: иначе вызывающий может изменить
	// состояние пула в обход блокировки.
	e := &OnionEngine{Name_: "torch", Base: "http://a.onion"}
	hp := NewHealthPool(nil, []*OnionEngine{e})
	hp.MarkResult("torch", true, time.Second)

	st, _ := hp.Get("torch")
	st.Live = false
	st.Probes = 999

	again, _ := hp.Get("torch")
	if !again.Live {
		t.Error("внешняя правка повлияла на состояние пула")
	}
	if again.Probes == 999 {
		t.Error("счётчик изменён снаружи")
	}
}

func TestSortByScore(t *testing.T) {
	list := []EngineHealth{
		{Name: "bad", SuccessRate: 0.2, LatencyAvg: 10},
		{Name: "fast", SuccessRate: 1.0, LatencyAvg: 10},
		{Name: "slow", SuccessRate: 1.0, LatencyAvg: 900},
		{Name: "mid", SuccessRate: 0.6, LatencyAvg: 5},
	}
	sortByScore(list)

	want := []string{"fast", "slow", "mid", "bad"}
	for i, name := range want {
		if list[i].Name != name {
			t.Errorf("позиция %d: %q, ожидала %q (весь порядок: %v)",
				i, list[i].Name, name, orderOf(list))
		}
	}
}

func orderOf(list []EngineHealth) []string {
	out := make([]string, 0, len(list))
	for _, e := range list {
		out = append(out, e.Name)
	}
	return out
}

func TestSortByScoreEmptyAndSingle(t *testing.T) {
	sortByScore(nil)
	one := []EngineHealth{{Name: "a"}}
	sortByScore(one)
	if len(one) != 1 || one[0].Name != "a" {
		t.Errorf("одиночный список испорчен: %+v", one)
	}
}

func TestTrimErr(t *testing.T) {
	if got := trimErr(nil); got != "" {
		t.Errorf("nil дал %q", got)
	}
	if got := trimErr(errors.New("простая  ошибка\tс пробелами\n")); got != "простая ошибка с пробелами" {
		t.Errorf("пробелы не схлопнуты: %q", got)
	}
	long := strings.Repeat("я", 200)
	if got := trimErr(errors.New(long)); len([]rune(got)) != 120 {
		t.Errorf("длина %d, ожидала 120", len([]rune(got)))
	}
	if got := trimErr(errors.New("короткая")); got != "короткая" {
		t.Errorf("короткая изменена: %q", got)
	}
}

func TestOnionEngineSearchParsesResults(t *testing.T) {
	e := stubEngine(t, resBody, 200, "torch")
	res, err := e.Search(context.Background(), "files", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 {
		t.Fatalf("результатов %d, ожидала 2", len(res))
	}
	if res[0].Title != "Каталог файлов" {
		t.Errorf("заголовок %q", res[0].Title)
	}
	if res[0].Source != "torch" {
		t.Errorf("Source=%q", res[0].Source)
	}
	if !res[0].Onion {
		t.Error("onion-результат не помечен")
	}
}

func TestOnionEngineSearchWithoutClient(t *testing.T) {
	e := &OnionEngine{Name_: "torch", Base: "http://a.onion"}
	if _, err := e.Search(context.Background(), "q", 5); err == nil {
		t.Error("поиск без клиента не вернул ошибку")
	}
}

func TestOnionEngineSearchHTTPError(t *testing.T) {
	e := stubEngine(t, "не найдено", 404, "torch")
	_, err := e.Search(context.Background(), "q", 5)
	if err == nil {
		t.Fatal("404 принят")
	}
	if !strings.Contains(err.Error(), "torch") || !strings.Contains(err.Error(), "404") {
		t.Errorf("ошибка не называет движок и код: %v", err)
	}
}

func TestOnionEngineSearchNoLinks(t *testing.T) {
	// Этап 182: страница без ссылок - пустая выдача, а не отказ движка:
	// репорт ok=true count=0, «по запросу ничего нет».
	e := stubEngine(t, "<html><body>пусто</body></html>", 200, "torch")
	res, err := e.Search(context.Background(), "q", 5)
	if err != nil {
		t.Fatalf("пустая страница - не ошибка движка: %v", err)
	}
	if len(res) != 0 {
		t.Errorf("пустая страница отдала %d результатов", len(res))
	}
}

func TestOnionEngineSearchUnreachable(t *testing.T) {
	e := &OnionEngine{Name_: "dead", Base: "http://127.0.0.1:1", Client: directClient(t)}
	_, err := e.Search(context.Background(), "q", 5)
	if err == nil {
		t.Fatal("недоступный движок не вернул ошибку")
	}
}

func TestOnionCatalogSearchAll(t *testing.T) {
	e1 := stubEngine(t, resBody, 200, "a")
	e2 := stubEngine(t, `<a class="res" href="http://zzzzzzzzzzzzzzzz.onion/">Третий</a>`, 200, "b")
	e3 := stubEngine(t, "ошибка", 500, "c")

	c := &OnionCatalog{Engines: []*OnionEngine{e1, e2, e3}, Log: nil}
	got := c.SearchAll(context.Background(), "files", 10)

	// Два движка ответили, третий упал: каталог обязан отдать результаты
	// живых, а не упасть целиком.
	if len(got) != 3 {
		t.Errorf("результатов %d, ожидала 3: %+v", len(got), got)
	}
}

func TestOnionCatalogSearchAllEmpty(t *testing.T) {
	c := &OnionCatalog{}
	if got := c.SearchAll(context.Background(), "q", 5); len(got) != 0 {
		t.Errorf("пустой каталог дал %d результатов", len(got))
	}
}

func TestOnionCatalogSearchAllDedupes(t *testing.T) {
	e1 := stubEngine(t, resBody, 200, "a")
	e2 := stubEngine(t, resBody, 200, "b")

	c := &OnionCatalog{Engines: []*OnionEngine{e1, e2}}
	got := c.SearchAll(context.Background(), "files", 10)
	if len(got) != 2 {
		t.Errorf("дубликаты не убраны: %d результатов", len(got))
	}
}

func TestOnionCatalogSearchAllRespectsLimit(t *testing.T) {
	body := `<a class="res" href="http://aaaaaaaaaaaaaaaa.onion/">1</a>
<a class="res" href="http://bbbbbbbbbbbbbbbb.onion/">2</a>
<a class="res" href="http://cccccccccccccccc.onion/">3</a>
<a class="res" href="http://dddddddddddddddd.onion/">4</a>`
	e := stubEngine(t, body, 200, "a")
	c := &OnionCatalog{Engines: []*OnionEngine{e}}

	if got := c.SearchAll(context.Background(), "q", 2); len(got) != 2 {
		t.Errorf("лимит не соблюдён: %d", len(got))
	}
}

func TestOnionCatalogLogsWithoutLogger(t *testing.T) {
	// Без логгера вывод не должен паниковать.
	c := &OnionCatalog{}
	c.logf("сообщение %d", 1)
}

func TestOnionEngineName(t *testing.T) {
	if got := (&OnionEngine{Name_: "torch"}).Name(); got != "torch" {
		t.Errorf("Name=%q", got)
	}
}

func TestRestoreLoadsPreviousStats(t *testing.T) {
	e1 := &OnionEngine{Name_: "torch", Base: "http://a.onion", Path: "/search?q={q}"}
	e2 := &OnionEngine{Name_: "tornet", Base: "http://b.onion", Path: "/search?q={q}"}
	hp := NewHealthPool(nil, []*OnionEngine{e1, e2})

	n := hp.Restore([]EngineHealth{
		{Name: "torch", SuccessRate: 0.9, Probes: 10, Successes: 9, Live: true, LatencyAvg: 4000},
		{Name: "нет-такого", Probes: 5},
		{Name: ""},
	})
	if n != 1 {
		t.Errorf("восстановлено %d, ожидала 1 (чужой и пустой отброшены)", n)
	}
	got, ok := hp.Get("torch")
	if !ok {
		t.Fatal("torch пропал")
	}
	if got.Probes != 10 || got.SuccessRate != 0.9 || !got.Live || got.LatencyAvg != 4000 {
		t.Errorf("статистика не подтянулась: %+v", got)
	}
	if live, total := hp.HealthyCount(); live != 1 || total != 2 {
		t.Errorf("live=%d total=%d, ожидала 1/2", live, total)
	}
}

func TestMarkResultPersistsSnapshot(t *testing.T) {
	e1 := &OnionEngine{Name_: "torch", Base: "http://a.onion", Path: "/search?q={q}"}
	hp := NewHealthPool(nil, []*OnionEngine{e1})

	var saved []EngineHealth
	hp.SetPersist(func(e EngineHealth) { saved = append(saved, e) })

	hp.MarkResult("torch", true, 1200*time.Millisecond)
	if len(saved) != 1 {
		t.Fatalf("persist вызван %d раз, ожидала 1", len(saved))
	}
	if saved[0].Name != "torch" || !saved[0].Live || saved[0].Probes != 1 {
		t.Errorf("снимок неверен: %+v", saved[0])
	}
	// Неизвестное имя не должно ни паниковать, ни писать.
	hp.MarkResult("нет-такого", true, time.Second)
	if len(saved) != 1 {
		t.Errorf("persist вызван для чужого имени")
	}
}

func TestRestoreDisabledKeepsEngineOffUntilSuccess(t *testing.T) {
	e1 := &OnionEngine{Name_: "torch", Base: "http://a.onion", Path: "/search?q={q}"}
	hp := NewHealthPool(nil, []*OnionEngine{e1})
	// Был отключён серией провалов, но исторически успешен: один успех
	// возвращает в строй, одиночный успех после 0/5 - нет.
	hp.Restore([]EngineHealth{{Name: "torch", Disabled: true, FailStreak: 3, Probes: 4, Successes: 3}})
	if live, _ := hp.HealthyCount(); live != 0 {
		t.Error("отключённый с прошлого запуска считается живым")
	}
	hp.MarkResult("torch", true, 500*time.Millisecond)
	if live, _ := hp.HealthyCount(); live != 1 {
		t.Error("успех не вернул движок в строй")
	}
}
