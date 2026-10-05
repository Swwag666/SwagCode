package search

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"voidsearchswag/internal/router"
	"voidsearchswag/internal/searchers"
)

// Этап 165, жалоба смоук-агента: fetch ронял живой onion-адрес с «имя не
// разрешается» за 50 мс - DNS-барьер резолвил .onion прямым транспортом,
// хотя зона живёт только у резолвера тора. Проверяется, что onion уходит
// мимо барьера и доходит до транспорта: с direct-клиентом честной ошибкой
// будет «onion-адрес недостижим напрямую», а не DNS-отказ.
func TestFetchURLSkipsDNSBarrierForOnion(t *testing.T) {
	eng, _, err := Build(DeepConfig{Transport: "direct"})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	// AllowPrivateTarget выключен: onion не может указать в приватную сеть,
	// и барьер для него не нужен. Прежняя версия падала здесь DNS-ошибкой.
	_, ferr := eng.FetchURL(context.Background(), "http://abcdefghijklmnop.onion/x")
	if ferr == nil {
		t.Fatal("direct-транспорт обязан отказаться от onion - тор не поднят")
	}
	msg := ferr.Error()
	if strings.Contains(msg, "не разрешается") || strings.Contains(msg, "no such host") {
		t.Errorf("ошибка из DNS-барьера, а не транспорта: %q", msg)
	}
	if !strings.Contains(msg, "onion") {
		t.Errorf("ошибка не про onion-доступность: %q", msg)
	}
}

// Проверка быстрого отказа: onion не должен тратить время на DNS-резолв
// (прямой резолв .onion у провайдера висит или падает по-разному). Барьерный
// путь занимал бы сетевой запрос; транспортный отказ локален.
func TestFetchURLOnionFailsFastWithoutDNS(t *testing.T) {
	eng, _, err := Build(DeepConfig{Transport: "direct"})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	start := time.Now()
	_, _ = eng.FetchURL(context.Background(), "http://zzzzzzzzzzzzzzzz.onion/")
	if time.Since(start) > 2*time.Second {
		t.Errorf("отказ от onion шёл %v - похоже, полез в DNS", time.Since(start))
	}
}

// Этап 165, жалоба смоук-агента: duration в ответе не включал работу
// validate - заявлено 21.766s при реальных 113.86s. Стенд спит 600 мс на
// запрос: Duration обязан учесть проверку.
func TestSearchValidateTimeCountsIntoDuration(t *testing.T) {
	e, _ := newEngine(t)
	e.AllowPrivateTarget = true
	e.Direct = directClientFor(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(600 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	e.DDG = []searchers.Searcher{&fixedSearcher{name: "s", res: []searchers.Result{
		{URL: srv.URL + "/ok", Title: "acme slow", Snippet: "acme"},
	}}}
	out, err := e.Search(context.Background(), Options{Query: "acme", Mode: router.ModeFast, Validate: true, NoCache: true})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	dur, pErr := time.ParseDuration(out.Duration)
	if pErr != nil {
		t.Fatalf("duration не парсится: %q", out.Duration)
	}
	if dur < 500*time.Millisecond {
		t.Errorf("duration=%s не включает 600 мс validate", out.Duration)
	}
}

// Этап 166: время выдачи пишется в единственной точке - финал Search и финал
// кэш-ветки. Промежуточные присваивания (после движков, после фоллбэков,
// после реранка) убраны: любое из них могло остаться последним и отрезать
// секунды validate. Здесь проверяется кэш-хит: чтение записи мгновенно, но
// validate гоняет те же 400 мс живых запросов, и Duration обязан их показать.
func TestCacheHitDurationCountsValidate(t *testing.T) {
	e, _ := newEngine(t)
	e.AllowPrivateTarget = true
	e.Direct = directClientFor(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	e.DDG = []searchers.Searcher{&fixedSearcher{name: "s", res: []searchers.Result{
		{URL: srv.URL + "/ok", Title: "cache acme", Snippet: "acme"},
	}}}
	// Первый прогон - без validate и с рабочим кэшем: в запись уходит время
	// одних движков (миллисекунды). Второй - кэш-хит с validate: правильный
	// код меряет заново и покажет ~400 мс; мутант вернёт чужое время из
	// записи.
	first, err := e.Search(context.Background(), Options{Query: "cache acme", Mode: router.ModeFast, Validate: false})
	if err != nil {
		t.Fatalf("первый search: %v", err)
	}
	second, err := e.Search(context.Background(), Options{Query: "cache acme", Mode: router.ModeFast, Validate: true})
	if err != nil {
		t.Fatalf("кэш-хит search: %v", err)
	}
	dur, pErr := time.ParseDuration(second.Duration)
	if pErr != nil {
		t.Fatalf("duration кэш-хита не парсится: %q", second.Duration)
	}
	if dur < 300*time.Millisecond {
		t.Errorf("кэш-хит duration=%s не включает 400 мс validate (первый прогон: %s)", second.Duration, first.Duration)
	}
}
