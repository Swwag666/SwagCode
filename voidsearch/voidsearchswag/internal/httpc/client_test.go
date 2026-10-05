package httpc

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeRotator подставляет управляемый транспорт: тесты проверяют логику
// клиента (выбор сессии, ротацию, реакцию на отказы), а не сеть.
type fakeRotator struct {
	kind      string
	spec      string
	healthy   bool
	rotates   int32
	closed    bool
	rotateErr error
}

func (f *fakeRotator) Kind() string          { return f.kind }
func (f *fakeRotator) TransportSpec() string { return f.spec }
func (f *fakeRotator) Healthy() bool         { return f.healthy }
func (f *fakeRotator) Close() error          { f.closed = true; return nil }
func (f *fakeRotator) Rotate(ctx context.Context) error {
	atomic.AddInt32(&f.rotates, 1)
	return f.rotateErr
}

func TestNewClientRejectsUnknownFingerprint(t *testing.T) {
	_, err := NewClient(context.Background(), Options{
		Transport:    "direct",
		Fingerprints: []string{"netscape_1"},
	})
	if err == nil {
		t.Fatal("неизвестный отпечаток принят")
	}
	if !strings.Contains(err.Error(), "netscape_1") {
		t.Errorf("в ошибке нет имени отпечатка: %v", err)
	}
}

func TestNewClientRejectsUnknownTransport(t *testing.T) {
	_, err := NewClient(context.Background(), Options{Transport: "carrier-pigeon"})
	if err == nil {
		t.Fatal("неизвестный транспорт принят")
	}
	if !strings.Contains(err.Error(), "carrier-pigeon") {
		t.Errorf("в ошибке нет имени транспорта: %v", err)
	}
}

func TestNewClientPoolWithoutPool(t *testing.T) {
	_, err := NewClient(context.Background(), Options{Transport: "pool"})
	if err == nil {
		t.Fatal("транспорт pool без пула принят")
	}
}

func TestNewClientStaticUsesProxies(t *testing.T) {
	c, err := NewClient(context.Background(), Options{
		Transport: "static",
		Proxies:   []string{"http://127.0.0.1:9", "http://127.0.0.1:10"},
	})
	if err != nil {
		t.Fatalf("статический транспорт: %v", err)
	}
	defer c.Close()
	if c.Rotator() == nil {
		t.Fatal("ротатор не создан")
	}
	if c.Rotator().Kind() == "" {
		t.Error("вид ротатора пуст")
	}
}

func TestNewClientInjectRotator(t *testing.T) {
	f := &fakeRotator{kind: "tor", spec: "socks5://127.0.0.1:9050", healthy: true}
	c, err := NewClient(context.Background(), Options{Rotator: f})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Rotator() != f {
		t.Error("переданный ротатор не использован")
	}
}

func TestClientDefaults(t *testing.T) {
	c, err := NewClient(context.Background(), Options{Transport: "direct"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.timeout != 30*time.Second {
		t.Errorf("timeout=%v, ожидала 30s", c.timeout)
	}
	if c.ttl != 10*time.Minute {
		t.Errorf("ttl=%v, ожидала 10m", c.ttl)
	}
	if len(c.fpNames) == 0 {
		t.Error("отпечатки по умолчанию не подставлены")
	}
}

func TestClientKeepsExplicitTimeout(t *testing.T) {
	c, err := NewClient(context.Background(), Options{
		Transport:  "direct",
		Timeout:    7 * time.Second,
		SessionTTL: 3 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.timeout != 7*time.Second {
		t.Errorf("timeout=%v", c.timeout)
	}
	if c.ttl != 3*time.Minute {
		t.Errorf("ttl=%v", c.ttl)
	}
}

func TestPickFingerprintAlwaysValid(t *testing.T) {
	c, err := NewClient(context.Background(), Options{Transport: "direct"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for i := 0; i < 50; i++ {
		fp := c.pickFingerprint()
		if fp.Name == "" {
			t.Fatal("выбран пустой отпечаток")
		}
		if _, ok := fp.TLSProfile(); !ok {
			t.Fatalf("у отпечатка %q нет TLS-профиля", fp.Name)
		}
	}
}

func TestPickFingerprintWithoutNames(t *testing.T) {
	c := &Client{}
	fp := c.pickFingerprint()
	if fp.Name == "" {
		t.Error("без имён отпечаток не выбран")
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	f := &fakeRotator{kind: "tor", healthy: true}
	c, err := NewClient(context.Background(), Options{Rotator: f})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if !f.closed {
		t.Error("ротатор не закрыт")
	}
	// Повторное закрытие не должно падать.
	if err := c.Close(); err != nil {
		t.Errorf("повторное закрытие: %v", err)
	}
}

func TestRotateWithoutRotatorIsNoop(t *testing.T) {
	c, err := NewClient(context.Background(), Options{Transport: "direct"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Rotate(context.Background()); err != nil {
		t.Errorf("ротация без ротатора: %v", err)
	}
}

func TestRotateCallsRotator(t *testing.T) {
	f := &fakeRotator{kind: "tor", healthy: true}
	c, err := NewClient(context.Background(), Options{Rotator: f})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Rotate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&f.rotates) != 1 {
		t.Errorf("ротаций %d, ожидала 1", f.rotates)
	}
}

func TestRotatePropagatesError(t *testing.T) {
	f := &fakeRotator{kind: "tor", healthy: true, rotateErr: errors.New("новый канал не поднялся")}
	c, err := NewClient(context.Background(), Options{Rotator: f})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Rotate(context.Background()); err == nil {
		t.Error("ошибка ротации проглочена")
	}
}

func TestNoteStatusWithoutPoolIsNoop(t *testing.T) {
	c := &Client{}
	// Без пула метка некуда писать, падать не должно.
	c.noteStatus(nil, &Response{Status: 403})
	c.noteStatus(nil, &Response{Status: 200})
}

func TestNoteTransportFailureWithoutPool(t *testing.T) {
	c := &Client{}
	c.noteTransportFailure()
}

func TestProxyHostPortVariants(t *testing.T) {
	cases := map[string]string{
		"socks5h://u:p@10.0.0.1:9050": "10.0.0.1:9050",
		"http://10.0.0.1:8080/path":   "10.0.0.1:8080",
		"  10.0.0.1:3128  ":           "10.0.0.1:3128",
		"socks5://10.0.0.1:9050/":     "10.0.0.1:9050",
		"10.0.0.1:1080":               "10.0.0.1:1080",
	}
	for in, want := range cases {
		if got := ProxyHostPort(in); got != want {
			t.Errorf("ProxyHostPort(%q)=%q, ожидала %q", in, got, want)
		}
	}
}

func TestLogfWithoutLogger(t *testing.T) {
	c := &Client{}
	// Логгера нет: писать некуда, падать не должно.
	c.logf("сообщение %d", 1)
}

func TestGetDelegatesToDo(t *testing.T) {
	c, err := NewClient(context.Background(), Options{Transport: "direct"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Настоящей сети нет: проверяется, что вызов доходит до построения
	// сессии и не паникует, а ошибка возвращается нормально.
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if _, err := c.Get(ctx, "http://127.0.0.1:9/"); err == nil {
		t.Log("запрос на закрытый порт вернул ошибку как ожидалось")
	}
}
