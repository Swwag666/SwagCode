package mcpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// Скользящее окно механически: три пропуска, четвёртый в то же мгновение
// отказ, пол-окна спустя отказ держится, окно спустя отметка выходит и
// слот освобождается. Часы передаются аргументом - тест не спит.
func TestRateLimiterSlidingWindow(t *testing.T) {
	l := newRateLimiter(3, 100*time.Millisecond)
	t0 := time.Now()

	for i := 1; i <= 3; i++ {
		if ok, _ := l.request("10.0.0.1", t0); !ok {
			t.Fatalf("запрос %d в пустом окне отказан", i)
		}
	}
	if ok, retry := l.request("10.0.0.1", t0); ok {
		t.Fatal("четвёртый запрос в том же окне пропущен")
	} else if retry != 100*time.Millisecond {
		t.Errorf("retry после переполнения = %v, хочу 100мс (вся ширина окна)", retry)
	}
	if ok, _ := l.request("10.0.0.1", t0.Add(50*time.Millisecond)); ok {
		t.Fatal("окно ещё полно через пол-ширины, а запрос прошёл")
	}
	if ok, _ := l.request("10.0.0.1", t0.Add(101*time.Millisecond)); !ok {
		t.Fatal("отметка вышла из окна, слот не освободился")
	}
}

func TestRateLimiterIndependentIPs(t *testing.T) {
	l := newRateLimiter(2, time.Minute)
	t0 := time.Now()
	for i := 0; i < 2; i++ {
		if ok, _ := l.request("10.0.0.1", t0); !ok {
			t.Fatal("квота первого IP отказана")
		}
	}
	if ok, _ := l.request("10.0.0.1", t0); ok {
		t.Fatal("первый IP переполнен, но прошёл")
	}
	if ok, _ := l.request("10.0.0.2", t0); !ok {
		t.Fatal("квота соседнего IP отравлена флудом первого")
	}
}

func TestRateLimiterZeroDisabled(t *testing.T) {
	l := newRateLimiter(0, time.Minute)
	t0 := time.Now()
	for i := 0; i < 100; i++ {
		if ok, _ := l.request("10.0.0.1", t0); !ok {
			t.Fatalf("лимит 0 обязан всё пропускать, отказ на запросе %d", i)
		}
	}
}

func TestRateLimiterMapCeiling(t *testing.T) {
	l := newRateLimiter(1, time.Hour)
	t0 := time.Now()
	for i := 0; i < maxTrackedIPs+50; i++ {
		ip := "10." + strconv.Itoa(i/256) + "." + strconv.Itoa(i%256) + ".1"
		if ok, _ := l.request(ip, t0); !ok {
			t.Fatalf("новый IP %s получил отказ в пустом окне", ip)
		}
	}
	l.mu.Lock()
	n := len(l.events)
	l.mu.Unlock()
	if n > maxTrackedIPs {
		t.Fatalf("карта IP выросла до %d при потолке %d", n, maxTrackedIPs)
	}
}

func TestClientIPFromRemoteAddr(t *testing.T) {
	r := httptest.NewRequest("GET", "/health", nil)
	r.RemoteAddr = "203.0.113.7:41234"
	if got := clientIP(r); got != "203.0.113.7" {
		t.Errorf("clientIP = %q, хочу 203.0.113.7", got)
	}
	r.RemoteAddr = "без-порта"
	if got := clientIP(r); got != "unknown" {
		t.Errorf("нечленимый RemoteAddr должен падать в общий бакет, получил %q", got)
	}
}

func TestRateLimitMiddlewareRejectsFlood(t *testing.T) {
	l := newRateLimiter(2, 1500*time.Millisecond)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := rateLimitMiddleware(l, next)

	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/health", nil)
		req.RemoteAddr = "198.51.100.9:5555"
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("запрос %d внутри квоты: %d, хочу 200", i+1, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/health", nil)
	req.RemoteAddr = "198.51.100.9:5555"
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("флуд за квотой: %d, хочу 429", rec.Code)
	}
	// Окно 1.5с: до свободного слота остаётся ~1.4с, ceil даёт 1.
	if ra := rec.Header().Get("Retry-After"); ra != "1" {
		t.Errorf("Retry-After = %q, хочу 1", ra)
	}
}

func TestRateLimitMiddlewareSeparateIPsDoNotShareQuota(t *testing.T) {
	l := newRateLimiter(1, time.Minute)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := rateLimitMiddleware(l, next)

	for _, addr := range []string{"198.51.100.1:1", "198.51.100.2:2"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/health", nil)
		req.RemoteAddr = addr
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: первый запрос своего IP: %d, хочу 200", addr, rec.Code)
		}
	}
}

func TestRateLimitMiddlewareNilLimiterPasses(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := rateLimitMiddleware(nil, next)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/health", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("nil-лимитер: %d, хочу проход 200", rec.Code)
	}
}

// waitReady ждёт первого успешного ответа сервера и возвращает, сколько
// HTTP-запросов на это ушло. Dial-проба для готовности не годится: принятый
// и брошенный коннект остаётся в StateNew, и Shutdown после отмены ждёт его
// до конца своего таймаута - живой замер дал 50% тестов с пятисекундным
// зависанием на teardown. HTTP-поллинг кончается дочитанным телом и
// соединением в idle.
func waitReady(t *testing.T, base string) int {
	t.Helper()
	// Считаются только ответы, дошедшие до сервера: отказ соединения до
	// старта слушателя квоту не тратит, а тест вычитает именно квоту.
	spent := 0
	for i := 0; i < 100; i++ {
		resp, err := http.Get(base + "/health")
		if err == nil {
			resp.Body.Close()
			spent++
			if resp.StatusCode == http.StatusOK {
				return spent
			}
		} else {
			time.Sleep(50 * time.Millisecond)
		}
	}
	t.Fatal("сервер не ответил на /health за 5 секунд опроса")
	return 0
}

// Живой сервер с малым лимитом: квота исчерпывается нарочно и лишние
// запросы получают 429. Число потраченных на прогрев запросов вычитается,
// чтобы тест не зависел от скорости старта слушателя.
func TestServeWithOptionsRateLimitCutsFlood(t *testing.T) {
	srv := New(Deps{Version: "test", Started: time.Now()})
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- ServeWithOptions(ctx, srv, HTTPOptions{
			Addr: addr, RateLimitPerMin: 8,
		})
	}()

	base := "http://" + addr
	spent := waitReady(t, base)
	for i := 0; i < 8-spent; i++ {
		resp, err := http.Get(base + "/health")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("запрос %d внутри квоты: %d, хочу 200", i+1, resp.StatusCode)
		}
	}
	resp, err := http.Get(base + "/health")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("запрос за квотой: %d, хочу 429", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("429 без Retry-After")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("сервер не завершился после отмены контекста")
	}
}

func TestServeWithOptionsRateLimitZeroUnlimited(t *testing.T) {
	srv := New(Deps{Version: "test", Started: time.Now()})
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- ServeWithOptions(ctx, srv, HTTPOptions{Addr: addr})
	}()

	base := "http://" + addr
	waitReady(t, base)

	for i := 0; i < 40; i++ {
		resp, err := http.Get(base + "/health")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("запрос %d без лимита: %d, хочу 200", i+1, resp.StatusCode)
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("сервер не завершился после отмены контекста")
	}
}
