package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"voidsearchswag/internal/netx"
	"voidsearchswag/internal/promote"
	"voidsearchswag/internal/store"
)

// stubRotator изображает ротатор tor с известными адресами.
type stubRotator struct {
	spec    string
	control string
	closed  bool
}

func (s *stubRotator) Kind() string                 { return "tor" }
func (s *stubRotator) TransportSpec() string        { return s.spec }
func (s *stubRotator) Healthy() bool                { return true }
func (s *stubRotator) Close() error                 { s.closed = true; return nil }
func (s *stubRotator) ControlAddr() string          { return s.control }
func (s *stubRotator) Rotate(context.Context) error { return nil }

// plainRotator не реализует ControlAddrer: внешний tor часто доступен только
// по socks, и хелпер обязан это переживать.
type plainRotator struct{ spec string }

func (p *plainRotator) Kind() string                 { return "tor" }
func (p *plainRotator) TransportSpec() string        { return p.spec }
func (p *plainRotator) Healthy() bool                { return true }
func (p *plainRotator) Close() error                 { return nil }
func (p *plainRotator) Rotate(context.Context) error { return nil }

func TestTorEndpointFromStripsScheme(t *testing.T) {
	cases := []struct{ spec, want string }{
		{"socks5://127.0.0.1:9050", "127.0.0.1:9050"},
		{"socks5h://127.0.0.1:9050", "127.0.0.1:9050"},
		{"127.0.0.1:9050", "127.0.0.1:9050"},
		{"socks5://127.0.0.1:9050/", "127.0.0.1:9050"},
		{"  socks5://127.0.0.1:9050  ", "127.0.0.1:9050"},
	}
	for _, c := range cases {
		socks, _ := torEndpointFrom(&stubRotator{spec: c.spec, control: "127.0.0.1:9051"})
		if socks != c.want {
			t.Errorf("spec %q -> socks %q, ожидала %q", c.spec, socks, c.want)
		}
	}
}

func TestTorEndpointFromReturnsControl(t *testing.T) {
	socks, control := torEndpointFrom(&stubRotator{
		spec: "socks5://127.0.0.1:9050", control: "127.0.0.1:9051",
	})
	if socks != "127.0.0.1:9050" || control != "127.0.0.1:9051" {
		t.Errorf("socks=%q control=%q", socks, control)
	}
}

func TestTorEndpointFromWithoutControlAddrer(t *testing.T) {
	// Ротатор без control-порта не ошибка: публикация socks всё равно полезна.
	socks, control := torEndpointFrom(&plainRotator{spec: "socks5://127.0.0.1:9050"})
	if socks != "127.0.0.1:9050" {
		t.Errorf("socks=%q", socks)
	}
	if control != "" {
		t.Errorf("control=%q, ожидала пусто", control)
	}
}

func TestTorEndpointFromNil(t *testing.T) {
	// Нил не должен ронять команду: лучше опубликовать пустой эндпоинт
	// (его отсеет живая проверка порта), чем паниковать на старте.
	socks, control := torEndpointFrom(nil)
	if socks != "" || control != "" {
		t.Errorf("socks=%q control=%q", socks, control)
	}
}

func TestTorEndpointFromPublishedRoundTrip(t *testing.T) {
	// Публикация обязана сохраняться в том виде, в котором её прочитает
	// другой процесс: socks без схемы, потому что attachTor добавляет её сам.
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	socks, control := torEndpointFrom(&stubRotator{
		spec: "socks5://127.0.0.1:4711", control: "127.0.0.1:4712",
	})
	if _, err := netx.SaveTorEndpoint(socks, control); err != nil {
		t.Fatal(err)
	}
	ep, err := netx.LoadTorEndpoint()
	if err != nil {
		t.Fatal(err)
	}
	if ep.Socks != "127.0.0.1:4711" || ep.Control != "127.0.0.1:4712" {
		t.Errorf("эндпоинт %+v", ep)
	}
}

func TestPromoteTickStopsOnBudget(t *testing.T) {
	// Фоновый тик обязан уважать бюджет: он работает внутри общего
	// планировщика, и зависший промоут задержал бы охоты и бэкапы.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Медленный ответ: каждая проверка съедает больше бюджета, чем
		// отведено на весь прогон.
		time.Sleep(120 * time.Millisecond)
		w.Write([]byte(`<html><body>
<form action="/s" method="get"><input type="text" name="q"></form>
<a href="http://a1a1a1a1a1a1a1a1.onion/1">one has text here</a>
<a href="http://b2b2b2b2b2b2b2b2.onion/2">two has text here</a>
</body></html>`))
	}))
	defer slow.Close()

	st, eng, p := tickEngine(t)
	p.Budget = 10 * time.Millisecond
	ctx := context.Background()
	for i := 0; i < 6; i++ {
		if err := st.UpsertOnion(ctx, store.Onion{
			URL: slow.URL + "/host" + string(rune('a'+i)) + "/", Status: "live",
		}); err != nil {
			t.Fatal(err)
		}
	}

	done := make(chan int, 1)
	go func() { done <- promoteTick(ctx, stderrLogger{}, st, eng, p, 6) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("промоут не остановился по бюджету")
	}
}

func TestPromoteTickDefaultBudgetApplies(t *testing.T) {
	// Незаданный бюджет не должен означать «без ограничения»: иначе тик может
	// работать вечно.
	st, eng, p := tickEngine(t)
	if p.Budget != 0 {
		t.Fatalf("бюджет уже задан: %v", p.Budget)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Пустой пул: прогон завершается штатно и быстро.
	if got := promoteTick(ctx, stderrLogger{}, st, eng, p, 5); got != 0 {
		t.Errorf("поднято %d на пустом пуле", got)
	}
	if promote.DefaultBudget <= 0 {
		t.Errorf("дефолтный бюджет %v", promote.DefaultBudget)
	}
}
