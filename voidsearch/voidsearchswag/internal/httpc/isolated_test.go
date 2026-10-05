package httpc

import (
	"context"
	"strings"
	"testing"
	"time"

	"voidsearchswag/internal/netx"
)

func torClientForIsolation(t *testing.T) (*Client, *fakeRotator) {
	t.Helper()
	fr := &fakeRotator{kind: "tor", spec: "socks5://127.0.0.1:9050", healthy: true}
	c, err := NewClient(context.Background(), Options{
		Transport:    "tor",
		Rotator:      fr,
		Fingerprints: []string{"chrome_131_win"},
		Timeout:      5 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c, fr
}

func TestIsolatedRejectsEmptyKey(t *testing.T) {
	c, _ := torClientForIsolation(t)
	defer c.Close()
	for _, key := range []string{"", "   "} {
		if _, err := c.Isolated(key); err == nil {
			t.Errorf("пустой ключ %q принят", key)
		}
	}
}

func TestIsolatedRequiresRotator(t *testing.T) {
	// Direct-транспорт не имеет ротатора: изолировать нечего, и молча вернуть
	// тот же клиент значило бы обещать отдельную цепь, которой нет.
	c, err := NewClient(context.Background(), Options{Transport: "direct", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Close()
	if _, err := c.Isolated("probe0"); err == nil {
		t.Error("изоляция принята без ротатора")
	}
}

func TestIsolatedChangesTransportSpec(t *testing.T) {
	c, _ := torClientForIsolation(t)
	defer c.Close()

	cl, err := c.Isolated("probe0")
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()

	spec := cl.rot.TransportSpec()
	if spec == "socks5://127.0.0.1:9050" {
		t.Fatal("спецификация не изменилась, отдельной цепи не будет")
	}
	if !strings.Contains(spec, "probe0") {
		t.Errorf("ключ не попал в спецификацию: %q", spec)
	}
	if !strings.Contains(spec, "127.0.0.1:9050") {
		t.Errorf("адрес потерян: %q", spec)
	}
}

func TestIsolatedClientsGetDistinctSpecs(t *testing.T) {
	// Каждый воркер обязан получить свою цепь. Одинаковые спецификации
	// означают, что восемь горутин снова упираются в одну цепь tor.
	c, _ := torClientForIsolation(t)
	defer c.Close()

	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		cl, err := c.Isolated("probe" + string(rune('0'+i)))
		if err != nil {
			t.Fatal(err)
		}
		spec := cl.rot.TransportSpec()
		if seen[spec] {
			t.Fatalf("дублирующаяся спецификация у воркера %d: %q", i, spec)
		}
		seen[spec] = true
		cl.Close()
	}
}

func TestIsolatedClientDoesNotOwnRotator(t *testing.T) {
	// Клон не владеет tor. Закрытие временного клиента не должно убивать
	// демон, которым пользуются остальные.
	c, fr := torClientForIsolation(t)
	defer c.Close()

	cl, err := c.Isolated("probe0")
	if err != nil {
		t.Fatal(err)
	}
	if err := cl.Close(); err != nil {
		t.Errorf("Close клона: %v", err)
	}
	if fr.closed {
		t.Error("клон закрыл общий ротатор")
	}
	// Исходный клиент продолжает работать.
	if c.rot == nil || !c.rot.Healthy() {
		t.Error("исходный клиент сломан после закрытия клона")
	}
}

func TestOriginalClientStillClosesRotator(t *testing.T) {
	c, fr := torClientForIsolation(t)
	if err := c.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if !fr.closed {
		t.Error("владелец не закрыл ротатор")
	}
}

func TestIsolatedClientHasOwnSession(t *testing.T) {
	c, _ := torClientForIsolation(t)
	defer c.Close()

	cl, err := c.Isolated("probe0")
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()

	if cl.sess != nil {
		t.Error("клон унаследовал чужую сессию")
	}
	if cl == c {
		t.Error("Isolated вернул тот же клиент")
	}
	// Настройки копируются: без таймаута клон ходил бы с нулевым.
	if cl.timeout != c.timeout {
		t.Errorf("таймаут %v, ожидала %v", cl.timeout, c.timeout)
	}
	if len(cl.fpNames) != len(c.fpNames) {
		t.Errorf("отпечатки не скопированы: %v", cl.fpNames)
	}
}

func TestIsolatedClientDoesNotSharePool(t *testing.T) {
	// Падение в изолированной цепи говорит о проблеме этой цепи, а не о смерти
	// прокси. Клон с общим пулом помечал бы рабочие адреса мёртвыми.
	c, _ := torClientForIsolation(t)
	defer c.Close()

	// Пул подставляется напрямую: тест живёт в пакете httpc, а конструктор
	// требует живого провайдера, который для этой проверки не нужен.
	c.pool = &netx.ProxyPool{}

	cl, err := c.Isolated("probe0")
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()

	if cl.pool != nil {
		t.Error("клон получил общий пул прокси")
	}
	if c.pool == nil {
		t.Error("исходный клиент потерял пул")
	}
}

func TestIsolatedFingerprintListIsCopied(t *testing.T) {
	c, _ := torClientForIsolation(t)
	defer c.Close()

	cl, err := c.Isolated("probe0")
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()

	if len(cl.fpNames) == 0 {
		t.Skip("список отпечатков пуст")
	}
	cl.fpNames[0] = "изменён"
	if c.fpNames[0] == "изменён" {
		t.Error("клон изменил список отпечатков исходного клиента")
	}
}

func TestIsolatedRotatorDelegatesRotateAndHealthy(t *testing.T) {
	fr := &fakeRotator{kind: "tor", spec: "socks5://127.0.0.1:9050", healthy: true}
	ir := &isolatedRotator{Rotator: fr, key: "k"}

	if ir.Kind() != "tor" {
		t.Errorf("Kind=%q", ir.Kind())
	}
	if !ir.Healthy() {
		t.Error("Healthy не делегируется")
	}
	if err := ir.Rotate(context.Background()); err != nil {
		t.Errorf("Rotate: %v", err)
	}
	if err := ir.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if fr.closed {
		t.Error("обёртка закрыла общий ротатор")
	}
}
