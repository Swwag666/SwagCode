package netx

import "testing"

func TestAddrIsLoopback(t *testing.T) {
	loopback := []string{
		"127.0.0.1:8080",
		"127.0.0.2:1",
		"[::1]:8080",
		"[::1]",
		"localhost:8080",
		"LOCALHOST:1",
		"localhost",
		"127.0.0.1",
		"  127.0.0.1:9000  ",
	}
	for _, addr := range loopback {
		if !AddrIsLoopback(addr) {
			t.Errorf("AddrIsLoopback(%q) = false, хочу true", addr)
		}
	}

	open := []string{
		"",
		"   ",
		":8080",
		":0",
		"0.0.0.0:8080",
		"[::]:8080",
		"192.168.0.18:8080",
		"8.8.8.8:80",
		"[fe80::1]:80",
		"example.com:8080",
		"host.invalid",
		"0.0.0.0",
	}
	for _, addr := range open {
		if AddrIsLoopback(addr) {
			t.Errorf("AddrIsLoopback(%q) = true, хочу false: такой адрес слушает не только петлю", addr)
		}
	}
}
