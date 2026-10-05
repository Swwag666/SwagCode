package mcpserver

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

func TestServeHTTPRequiresToken(t *testing.T) {
	srv := New(Deps{Version: "test", Started: time.Now()})
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, srv, addr, "s3cret") }()

	var base string
	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		base = "http://" + addr
		if resp, err := http.Get(base + "/health"); err == nil {
			resp.Body.Close()
			break
		}
	}
	base = "http://" + addr

	if resp, err := http.Get(base + "/health"); err != nil || resp.StatusCode != 200 {
		t.Fatalf("health без токена обязан отвечать 200: %v", resp)
	} else {
		resp.Body.Close()
	}

	resp, err := http.Post(base+"/mcp", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("/mcp без токена: %d, ожидала 401", resp.StatusCode)
	}
	if resp.Header.Get("WWW-Authenticate") == "" {
		t.Error("нет WWW-Authenticate на 401")
	}

	req, _ := http.NewRequest("POST", base+"/mcp", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	client := &http.Client{Timeout: 5 * time.Second}
	resp2, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	// Авторизованный запрос доходит до MCP-хендлера: 401 быть не должно.
	// Дальше MCP ответит 4xx/406 по протоколу — это уже не auth.
	if resp2.StatusCode == http.StatusUnauthorized {
		t.Error("валидный Bearer отклонён")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(7 * time.Second):
		t.Error("Serve не остановился по cancel")
	}
	fmt.Println(addr)
}
