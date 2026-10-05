package mcpserver

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestCheckListenPolicyTable(t *testing.T) {
	type row struct {
		addr      string
		token     string
		allowOpen bool
		refuse    bool
		why       string
	}
	rows := []row{
		{"", "", false, false, "пустой адрес означает stdio"},
		{"", "s3cret", false, false, "stdio с токеном"},
		{"127.0.0.1:8080", "", false, false, "петлевой адрес без токена"},
		{"127.0.0.2:8080", "", false, false, "любой адрес 127/8 петлевой"},
		{"[::1]:8080", "", false, false, "петлевой IPv6 без токена"},
		{"localhost:8080", "", false, false, "имя localhost"},
		{"  127.0.0.1:8080  ", "", false, false, "пробелы вокруг петлевого адреса"},
		{":8080", "", false, true, "пустой хост слушает все интерфейсы"},
		{"0.0.0.0:8080", "", false, true, "явный ноль слушает все интерфейсы"},
		{"[::]:8080", "", false, true, "IPv6-ноль слушает все интерфейсы"},
		{"192.168.0.18:8080", "", false, true, "адрес локальной сети"},
		{"93.184.216.34:8080", "", false, true, "публичный адрес"},
		{"myhost.local:8080", "", false, true, "имя машины в сети, не петля"},
		{":8080", "   ", false, true, "пробелы вместо токена"},
		{":8080", "s3cret", false, false, "открытый адрес с токеном"},
		{"0.0.0.0:8080", "", true, false, "осознанный открытый стенд"},
	}
	for _, r := range rows {
		err := CheckListenPolicy(r.addr, r.token, r.allowOpen)
		if r.refuse {
			if err == nil {
				t.Errorf("%s: адрес %q токен %q allowOpen=%v - отказ не возвращён",
					r.why, r.addr, r.token, r.allowOpen)
				continue
			}
			if !errors.Is(err, ErrOpenAddressWithoutToken) {
				t.Errorf("%s: ошибка %v не распознаётся через errors.Is", r.why, err)
			}
			if !strings.Contains(err.Error(), r.addr) {
				t.Errorf("%s: в ошибке нет адреса %q: %v", r.why, r.addr, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: адрес %q токен %q allowOpen=%v - неожиданный отказ %v",
				r.why, r.addr, r.token, r.allowOpen, err)
		}
	}
}

// freeAddrAny возвращает адрес слушателя на всех интерфейсах и адрес для
// проверок через петлю: обращаться к 0.0.0.0 как к цели в Windows можно не
// всегда, а порт у обоих один.
func freeAddrAny(t *testing.T) (string, string) {
	t.Helper()
	l, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	return addr, "http://127.0.0.1:" + port
}

func waitHealth(base string) bool {
	for i := 0; i < 100; i++ {
		resp, err := http.Get(base + "/health")
		if err == nil {
			resp.Body.Close()
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

func TestServeRefusesOpenAddressWithoutToken(t *testing.T) {
	srv := New(Deps{Version: "test", Started: time.Now()})
	addr, base := freeAddrAny(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// ServeWithOptions блокируется до конца контекста, если сервер поднялся,
	// поэтому отказ ждём в горутине с коротким пределом: зависший здесь тест
	// означал бы, что политика не сработала вовсе.
	res := make(chan error, 1)
	go func() { res <- ServeWithOptions(ctx, srv, HTTPOptions{Addr: addr}) }()
	select {
	case err := <-res:
		if err == nil {
			t.Fatalf("сервер поднялся на %s без токена и без AllowOpen и сразу завершился", addr)
		}
		if !errors.Is(err, ErrOpenAddressWithoutToken) {
			t.Errorf("ошибка %v не ErrOpenAddressWithoutToken", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("сервер на %s не отказал, а поднялся и слушает", addr)
	}
	cancel()

	// Отказ обязан случиться до bind, иначе за процессом останется чужой порт.
	hostPort := strings.TrimPrefix(base, "http://")
	if c, dialErr := net.DialTimeout("tcp", hostPort, 500*time.Millisecond); dialErr == nil {
		c.Close()
		t.Errorf("на %s кто-то слушает, хотя сервер обязан был отказать", hostPort)
	}
}

func TestServeAllowsOpenAddressWithAllowOpen(t *testing.T) {
	srv := New(Deps{Version: "test", Started: time.Now()})
	addr, base := freeAddrAny(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ServeWithOptions(ctx, srv, HTTPOptions{Addr: addr, AllowOpen: true}) }()

	if !waitHealth(base) {
		cancel()
		<-done
		t.Fatalf("сервер с AllowOpen на %s не поднялся за 5 с", addr)
	}
	resp, err := http.Get(base + "/health")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/health на открытом стенде: статус %d, хочу 200", resp.StatusCode)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(7 * time.Second):
		t.Error("сервер не остановился по cancel")
	}
}

func TestServeAllowsOpenAddressWithToken(t *testing.T) {
	srv := New(Deps{Version: "test", Started: time.Now()})
	addr, base := freeAddrAny(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ServeWithOptions(ctx, srv, HTTPOptions{Addr: addr, Token: "s3cret"}) }()

	if !waitHealth(base) {
		cancel()
		<-done
		t.Fatalf("сервер с токеном на %s не поднялся за 5 с", addr)
	}

	resp, err := http.Post(base+"/mcp", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("/mcp без Bearer на открытом адресе с токеном: статус %d, хочу 401", resp.StatusCode)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(7 * time.Second):
		t.Error("сервер не остановился по cancel")
	}
}

func TestServeStdioIgnoresPolicy(t *testing.T) {
	// Пустой адрес означает stdio-транспорт: политика слушателя его не
	// касается, иначе локальный агент без токена перестал бы работать.
	srv := New(Deps{Version: "test", Started: time.Now()})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ServeWithOptions(ctx, srv, HTTPOptions{}) }()
	cancel()
	select {
	case err := <-done:
		if errors.Is(err, ErrOpenAddressWithoutToken) {
			t.Errorf("stdio-транспорт отклонён политикой слушателя: %v", err)
		}
	case <-time.After(7 * time.Second):
		t.Error("stdio-сервер не остановился по cancel")
	}
}
