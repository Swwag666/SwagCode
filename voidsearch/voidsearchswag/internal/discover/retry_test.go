package discover

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"voidsearchswag/internal/httpc"
)

// flakySource падает первые N запросов, а затем отдаёт тело. Так проверяется
// повтор: без него один таймаут выкидывал источник из прогона целиком.
type flakySource struct {
	body       string
	failures   int
	calls      int
	failStatus int
	okStatus   int
}

func (f *flakySource) Fetch(ctx context.Context, r httpc.Request) (*httpc.Response, error) {
	f.calls++
	if f.calls <= f.failures {
		if f.failStatus != 0 {
			return &httpc.Response{Status: f.failStatus, Body: []byte(f.body)}, nil
		}
		return nil, errors.New("таймаут соединения")
	}
	status := f.okStatus
	if status == 0 {
		status = 200
	}
	return &httpc.Response{Status: status, Body: []byte(f.body)}, nil
}

// retryBody содержит два адреса намеренно: порог живой выдачи равен двум, и
// фикстура с одним адресом проверяла бы не ретраи, а отсечение редких
// источников. Тест обязан изолировать то поведение, ради которого написан.
const retryBody = `<a href="http://` + v2a + `.onion/">Catalog Entry</a>` +
	`<a href="http://` + v2b + `.onion/">Second Entry</a>`

func TestFetchSourceRetriesTransientFailure(t *testing.T) {
	src := &flakySource{body: retryBody, failures: 1}
	f := &Finder{Client: src, Log: silentLog{}, Attempts: 3}

	got, err := f.fetchSource(context.Background(), source{name: "s", url: "https://s.example/"})
	if err != nil {
		t.Fatalf("повтор не спас источник: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("адресов %d, ожидала 2 (в фикстуре retryBody два адреса)", len(got))
	}
	if src.calls != 2 {
		t.Errorf("запросов %d, ожидала 2 (отказ + успешный повтор)", src.calls)
	}
}

func TestFetchSourceRetriesServerError(t *testing.T) {
	src := &flakySource{body: retryBody, failures: 1, failStatus: 503}
	f := &Finder{Client: src, Log: silentLog{}, Attempts: 3}

	// Первая попытка вернёт 503, вторая - 200 с телом.
	if _, err := f.fetchSource(context.Background(), source{name: "s", url: "https://s.example/"}); err != nil {
		t.Fatalf("5xx не повторён: %v", err)
	}
	if src.calls != 2 {
		t.Errorf("запросов %d, ожидала 2", src.calls)
	}
}

func TestFetchSourceGivesUpAfterAttempts(t *testing.T) {
	src := &flakySource{body: retryBody, failures: 99}
	f := &Finder{Client: src, Log: silentLog{}, Attempts: 3}

	if _, err := f.fetchSource(context.Background(), source{name: "s", url: "https://s.example/"}); err == nil {
		t.Fatal("вечный сбой принят как успех")
	}
	if src.calls != 3 {
		t.Errorf("попыток %d, ожидала ровно 3", src.calls)
	}
}

func TestFetchSourceNoRetryOnClientError(t *testing.T) {
	// 404 - осознанный отказ источника. Повтор получит то же самое и
	// потратит вдвое больше времени на прогон.
	src := &flakySource{body: retryBody, okStatus: 404}
	f := &Finder{Client: src, Log: silentLog{}, Attempts: 3}

	if _, err := f.fetchSource(context.Background(), source{name: "s", url: "https://s.example/"}); err == nil {
		t.Fatal("404 принят как успех")
	}
	if src.calls != 1 {
		t.Errorf("попыток %d, ожидала 1: 4xx не повторяется", src.calls)
	}
}

func TestFetchSourceNoRetryOnEmptyResult(t *testing.T) {
	// «Адресов не найдено» - это ответ, а не сбой: страница отдалась, но
	// разметка не та. Повтор даст тот же пустой результат.
	src := &flakySource{body: `<html>нет ссылок</html>`}
	f := &Finder{Client: src, Log: silentLog{}, Attempts: 3}

	_, err := f.fetchSource(context.Background(), source{name: "s", url: "https://s.example/"})
	if err == nil {
		t.Fatal("пустой результат принят")
	}
	if !strings.Contains(err.Error(), "адресов не найдено") {
		t.Errorf("не та ошибка: %v", err)
	}
	if src.calls != 1 {
		t.Errorf("попыток %d, ожидала 1", src.calls)
	}
}

func TestFetchSourceWithoutClient(t *testing.T) {
	f := &Finder{Log: silentLog{}}
	if _, err := f.fetchSource(context.Background(), source{name: "s", url: "https://s.example/"}); err == nil {
		t.Error("запрос без клиента не отмечен ошибкой")
	}
}

func TestFetchSourceCancelledContextStopsRetries(t *testing.T) {
	src := &flakySource{body: retryBody, failures: 99}
	f := &Finder{Client: src, Log: silentLog{}, Attempts: 5}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.fetchSource(ctx, source{name: "s", url: "https://s.example/"}); err == nil {
		t.Error("отменённый контекст не остановил запросы")
	}
	if src.calls != 0 {
		t.Errorf("на отменённом контексте сделано %d запросов", src.calls)
	}
}

func TestAttemptsDefault(t *testing.T) {
	f := &Finder{}
	if got := f.attempts(); got != sourceAttempts {
		t.Errorf("дефолт %d, ожидала %d", got, sourceAttempts)
	}
	f = &Finder{Attempts: -1}
	if got := f.attempts(); got != sourceAttempts {
		t.Errorf("отрицательное значение не заменено дефолтом: %d", got)
	}
	f = &Finder{Attempts: 4}
	if got := f.attempts(); got != 4 {
		t.Errorf("явное значение затёрто: %d", got)
	}
}

func TestRetryable(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("HTTP 404"), false},
		{errors.New("HTTP 403"), false},
		{errors.New("HTTP 503"), true},
		{errors.New("HTTP 500"), true},
		{errors.New("адресов не найдено"), false},
		{errors.New("таймаут соединения"), true},
		{errors.New("connection reset by peer"), true},
		{context.DeadlineExceeded, true},
	}
	for _, c := range cases {
		if got := retryable(c.err); got != c.want {
			t.Errorf("retryable(%v)=%v, ожидала %v", c.err, got, c.want)
		}
	}
}

func TestNewFinderAcceptsInterface(t *testing.T) {
	src := &flakySource{body: retryBody}
	f := NewFinder(src, silentLog{})
	if f.Client != src {
		t.Error("клиент не проброшен")
	}
	if f.attempts() != sourceAttempts {
		t.Errorf("попыток %d", f.attempts())
	}
}

func TestDiscoverClearnetRetriesInsideRun(t *testing.T) {
	// Отчёт прогона должен показать источник живым, если повтор вытянул
	// его после транзиентного сбоя.
	src := &flakySource{body: retryBody, failures: 1}
	f := &Finder{Client: src, Log: silentLog{}, Attempts: 3}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cands, reports := f.DiscoverClearnet(ctx)

	if len(cands) == 0 {
		t.Fatal("адреса не собраны")
	}
	var okCount int
	for _, r := range reports {
		if r.OK {
			okCount++
		}
	}
	if okCount == 0 {
		t.Errorf("ни один источник не отмечен живым: %+v", reports)
	}
	if cands[0].Via != "clearnet" {
		t.Errorf("Via=%q", cands[0].Via)
	}
}
