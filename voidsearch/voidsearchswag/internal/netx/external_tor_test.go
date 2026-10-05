package netx

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

func TestNormalizeHostPort(t *testing.T) {
	cases := []struct{ in, want string }{
		{"127.0.0.1:9050", "127.0.0.1:9050"},
		{"  127.0.0.1:9050  ", "127.0.0.1:9050"},
		{"socks5://127.0.0.1:9050", "127.0.0.1:9050"},
		{"socks5h://127.0.0.1:9050", "127.0.0.1:9050"},
		{"http://127.0.0.1:9050/", "127.0.0.1:9050"},
		{"localhost:9150", "localhost:9150"},
		{"", ""},
		{"   ", ""},
	}
	for _, c := range cases {
		if got := normalizeHostPort(c.in); got != c.want {
			t.Errorf("normalizeHostPort(%q) = %q, ожидала %q", c.in, got, c.want)
		}
	}
}

func TestProbeTCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	if err := probeTCP(ln.Addr().String(), 2*time.Second); err != nil {
		t.Errorf("живой порт не пройден: %v", err)
	}
	if err := probeTCP("127.0.0.1:1", 500*time.Millisecond); err == nil {
		t.Error("закрытый порт принят")
	}
}

// liveSock поднимает TCP-слушатель, который изображает socks-порт tor.
func liveSock(t *testing.T) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

func TestAttachTorRequiresLiveSocks(t *testing.T) {
	// Мёртвый socks-порт обязан быть ошибкой, а не молчаливым откатом к
	// прямому соединению: для onion откат означает и падение, и утечку
	// запроса в системный DNS.
	_, err := attachTor(Config{TorSocksAddr: "127.0.0.1:1", Logger: nopLogger{}})
	if err == nil {
		t.Fatal("недоступный socks принят")
	}
	if !strings.Contains(err.Error(), "недоступен") {
		t.Errorf("ошибка не объясняет причину: %v", err)
	}
}

func TestAttachTorRejectsEmptyAddr(t *testing.T) {
	for _, in := range []string{"", "   "} {
		if _, err := attachTor(Config{TorSocksAddr: in, Logger: nopLogger{}}); err == nil {
			t.Errorf("пустой адрес %q принят", in)
		}
	}
}

func TestAttachTorWithoutControl(t *testing.T) {
	addr, stop := liveSock(t)
	defer stop()

	r, err := attachTor(Config{TorSocksAddr: addr, Logger: nopLogger{}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	if r.Kind() != "tor" {
		t.Errorf("Kind=%q, ожидала tor", r.Kind())
	}
	if got, want := r.TransportSpec(), "socks5://"+addr; got != want {
		t.Errorf("TransportSpec=%q, ожидала %q", got, want)
	}
	if !r.Healthy() {
		t.Error("живой socks-порт признан нездоровым")
	}
	// Без control-порта NEWNYM невозможен, но это не ошибка транспорта:
	// запросы продолжают ходить. Ошибка здесь заставила бы вызывающего
	// считать сессию сломанной и пересоздавать её в цикле.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := r.Rotate(ctx); err != nil {
		t.Errorf("Rotate без control вернул ошибку: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	// Повторный Close не должен паниковать.
	if err := r.Close(); err != nil {
		t.Errorf("второй Close: %v", err)
	}
}

func TestExternalRotatorUnhealthyAfterSockDies(t *testing.T) {
	addr, stop := liveSock(t)

	r, err := attachTor(Config{TorSocksAddr: addr, Logger: nopLogger{}})
	if err != nil {
		stop()
		t.Fatal(err)
	}
	defer r.Close()

	if !r.Healthy() {
		t.Fatal("здоровье не определено на живом порту")
	}
	stop()
	if r.Healthy() {
		t.Error("ротатор считает транспорт живым после падения демона")
	}
}

func TestExternalRotatorNeverKillsProcess(t *testing.T) {
	// Ротатор не владеет демоном. Проверка структурная: у externalRotator нет
	// поля процесса, поэтому Close физически не может остановить чужой tor.
	addr, stop := liveSock(t)
	defer stop()

	r, err := attachTor(Config{TorSocksAddr: addr, Logger: nopLogger{}})
	if err != nil {
		t.Fatal(err)
	}
	er, ok := r.(*externalRotator)
	if !ok {
		t.Fatalf("тип %T, ожидала *externalRotator", r)
	}
	if err := er.Close(); err != nil {
		t.Fatal(err)
	}
	// Порт обязан остаться живым: чужой процесс мы не трогали.
	if err := probeTCP(addr, 2*time.Second); err != nil {
		t.Errorf("внешний tor пострадал от Close: %v", err)
	}
}

func TestStartTorPrefersExternalOverSpawn(t *testing.T) {
	// Задан socks-адрес: StartTor обязан подключиться к нему и не требовать
	// бинарь. Иначе пользователи системного tor без установленного tor.exe
	// остались бы без транспорта.
	addr, stop := liveSock(t)
	defer stop()

	r, err := StartTor(Config{TorSocksAddr: addr, TorBinary: "", Logger: nopLogger{}})
	if err != nil {
		t.Fatalf("StartTor с внешним tor: %v", err)
	}
	defer r.Close()
	if r.Kind() != "tor" || r.TransportSpec() != "socks5://"+addr {
		t.Errorf("Kind=%q spec=%q", r.Kind(), r.TransportSpec())
	}
}

func TestStartTorStillRequiresBinaryWithoutExternal(t *testing.T) {
	// Без внешнего адреса прежнее поведение сохраняется: нет бинаря - ошибка.
	// TorNoReuse отсекает живой демон, который может быть опубликован в
	// системе командой tord: тест проверяет spawn-путь, а не переиспользование.
	if _, err := StartTor(Config{TorBinary: "   ", TorNoReuse: true, Logger: nopLogger{}}); err == nil {
		t.Error("пустой бинарь принят")
	}
}

func TestExternalRotatorImplementsInterface(t *testing.T) {
	var _ Rotator = &externalRotator{}
}
