package httpc

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"voidsearchswag/internal/netx"
)

const onionHost = "metagerv65pwclop2rsfzg4jwowpavpwd6grhhlvdgsswvo6ii4akgyd.onion"

func TestOnionBlockedWithoutProxy(t *testing.T) {
	err := onionBlocked("http://"+onionHost+"/", "")
	if err == nil {
		t.Fatal("onion без прокси пропущен")
	}
	if !errors.Is(err, ErrOnionWithoutTor) {
		t.Errorf("ошибка не распознаётся через errors.Is: %v", err)
	}
	if !strings.Contains(err.Error(), onionHost) {
		t.Errorf("в ошибке нет адреса: %v", err)
	}
}

func TestOnionBlockedTable(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		proxy   string
		blocked bool
	}{
		{"onion без прокси", "http://" + onionHost + "/", "", true},
		{"onion https без прокси", "https://" + onionHost + "/", "", true},
		{"onion в верхнем регистре", "http://" + strings.ToUpper(onionHost) + "/", "", true},
		{"onion с пробелами", "  http://" + onionHost + "/  ", "", true},
		{"onion через tor", "http://" + onionHost + "/", "socks5://127.0.0.1:9050", false},
		{"onion через пул-прокси", "http://" + onionHost + "/", "1.2.3.4:1080", false},
		{"clearnet без прокси", "https://duckduckgo.com/", "", false},
		{"clearnet с прокси", "https://duckduckgo.com/", "socks5://127.0.0.1:9050", false},
		{"пустой URL", "", "", false},
		{"битый URL", "://мусор", "", false},
		{"похожий на onion хост", "http://notonion.com/x.onion", "", false},
		{"onion как путь, не хост", "http://example.com/" + onionHost, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := onionBlocked(c.url, c.proxy)
			if c.blocked && err == nil {
				t.Errorf("ожидала блокировку, получила пропуск")
			}
			if !c.blocked && err != nil {
				t.Errorf("ожидала пропуск, получила %v", err)
			}
		})
	}
}

func TestSessionDoBlocksOnionWithoutProxy(t *testing.T) {
	// Прямая сессия (spec пустой) не должна даже пытаться резолвить .onion:
	// это и падение, и утечка запроса в DNS провайдера.
	s, err := NewSession(mustFP(t), "", "direct", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if _, err := s.Do(context.Background(), Request{URL: "http://" + onionHost + "/"}); !errors.Is(err, ErrOnionWithoutTor) {
		t.Errorf("Do: err=%v", err)
	}
	if _, err := s.DoStd(context.Background(), Request{URL: "http://" + onionHost + "/"}); !errors.Is(err, ErrOnionWithoutTor) {
		t.Errorf("DoStd: err=%v", err)
	}
	// Clearnet на той же сессии обязан работать: барьер точечный.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ок"))
	}))
	defer srv.Close()
	if _, err := s.Do(context.Background(), Request{URL: srv.URL + "/"}); err != nil {
		t.Errorf("clearnet через прямую сессию: %v", err)
	}
}

func TestSessionWithProxyAllowsOnion(t *testing.T) {
	// С tor-сессией onion обязан проходить барьер; сам запрос здесь упадёт на
	// соединении с закрытым портом, но НЕ с ErrOnionWithoutTor.
	s, err := NewSession(mustFP(t), "socks5://127.0.0.1:1", "tor", 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	_, err = s.Do(context.Background(), Request{URL: "http://" + onionHost + "/"})
	if errors.Is(err, ErrOnionWithoutTor) {
		t.Error("tor-сессия заблокирована барьером")
	}
}

// flipRotator отдаёт нерабочий spec до ротации и прямой после. Так первый
// (fhttp) запрос гарантированно падает на транспорте, а эскалация получает
// рабочую сессию: проверяется именно контракт Fetch, а не сам Escalate.
type flipRotator struct {
	first   string
	rotated bool
}

func (f *flipRotator) Kind() string { return "tor" }
func (f *flipRotator) TransportSpec() string {
	if f.rotated {
		return ""
	}
	return f.first
}
func (f *flipRotator) Healthy() bool { return true }
func (f *flipRotator) Close() error  { return nil }
func (f *flipRotator) Rotate(ctx context.Context) error {
	f.rotated = true
	return nil
}

func TestClientFetchEscalatesOnTransportError(t *testing.T) {
	var stdCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&stdCalls, 1)
		w.Write([]byte("контент после эскалации"))
	}))
	defer srv.Close()

	c, err := NewClient(context.Background(), Options{
		Rotator:      &flipRotator{first: "socks5://127.0.0.1:1"},
		Fingerprints: []string{"chrome_131_win"},
		Timeout:      5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// До исправления Fetch возвращал транспортную ошибку вызывающему, даже
	// когда запасной путь работал: onion-сайт с редиректом на https терялся
	// целиком вместо того, чтобы доехать через impersonate.
	resp, err := c.Fetch(context.Background(), Request{URL: srv.URL + "/", Method: http.MethodGet})
	if err != nil {
		t.Fatalf("Fetch не эскалировал транспортную ошибку: %v", err)
	}
	if resp == nil || resp.Status != http.StatusOK {
		t.Fatalf("ответ эскалации: %+v", resp)
	}
	if !strings.Contains(string(resp.Body), "эскалации") {
		t.Errorf("тело: %q", resp.Body)
	}
	if atomic.LoadInt32(&stdCalls) == 0 {
		t.Error("запасной путь не использован")
	}
}

func TestClientFetchReturnsTransportErrorWhenEscalationFailsToo(t *testing.T) {
	// Когда и эскалация падает, вызывающий обязан получить исходную ошибку:
	// молча проглатывать её нельзя, иначе сбой транспорта выглядит как пустой
	// ответ.
	c, err := NewClient(context.Background(), Options{
		Rotator:      &fakeRotator{kind: "tor", spec: "socks5://127.0.0.1:1", healthy: true},
		Fingerprints: []string{"chrome_131_win"},
		Timeout:      3 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if _, err := c.Fetch(context.Background(), Request{URL: "http://127.0.0.1:1/x", Method: http.MethodGet}); err == nil {
		t.Error("Fetch скрыл транспортную ошибку")
	}
}

func TestOnionErrorIsActionable(t *testing.T) {
	// Текст ошибки обязан объяснять, что делать: без tor onion не открыть,
	// и «no such host» эту причину не показывает.
	err := onionBlocked("http://"+onionHost+"/", "")
	if err == nil {
		t.Fatal("нет ошибки")
	}
	msg := err.Error()
	for _, want := range []string{"tor", onionHost} {
		if !strings.Contains(strings.ToLower(msg), strings.ToLower(want)) {
			t.Errorf("в ошибке нет %q: %s", want, msg)
		}
	}
}

func mustFP(t *testing.T) netx.Fingerprint {
	t.Helper()
	fp, ok := netx.FingerprintByName("chrome_131_win")
	if !ok {
		t.Fatal("отпечаток chrome_131_win не найден")
	}
	return fp
}
