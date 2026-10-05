package mcpserver

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// initBody - корректный JSON-RPC initialize: его прежняя версия сервера
// принимала на любом пути.
const initBody = `{"jsonrpc":"2.0","id":1,"method":"initialize",` +
	`"params":{"protocolVersion":"2024-11-05","capabilities":{},` +
	`"clientInfo":{"name":"routing-test","version":"1"}}}`

// serveHTTP поднимает настоящий HTTP-транспорт на свободном петлевом порту и
// возвращает базовый адрес вместе с остановкой. Токен "" означает сервер без
// аутентификации.
func serveHTTP(t *testing.T, token string) (string, func()) {
	t.Helper()
	srv := New(Deps{Version: "test", Started: time.Now()})
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, srv, addr, token) }()
	base := "http://" + addr
	for i := 0; i < 100; i++ {
		resp, err := http.Get(base + "/health")
		if err == nil {
			resp.Body.Close()
			return base, func() {
				cancel()
				select {
				case <-done:
				case <-time.After(7 * time.Second):
					t.Error("сервер не остановился по cancel")
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	<-done
	t.Fatalf("сервер на %s не поднялся за 5 с", addr)
	return "", nil
}

// doReq выполняет запрос и возвращает статус вместе с началом тела. Статус 0
// означает, что ответа не было вовсе - именно так вела себя прежняя версия на
// GET к неизвестному пути: соединение открывалось в SSE-поток и висело.
func doReq(t *testing.T, method, url, body string, timeout time.Duration) (int, string) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatalf("запрос %s %s: %v", method, url, err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, string(data)
}

func TestUnknownPathsAnswer404(t *testing.T) {
	base, stop := serveHTTP(t, "")
	defer stop()

	for _, path := range []string{
		"/foobar", "/admin/panel/deep", "/mcp2", "/health2", "/metrics2",
		"/peer/export2", "/long/unknown/path",
	} {
		code, body := doReq(t, http.MethodGet, base+path, "", 3*time.Second)
		if code != http.StatusNotFound {
			t.Errorf("GET %s: статус %d, хочу 404 (тело %.80q)", path, code, body)
		}
	}
	// Путь с «..» здесь не проверяется намеренно: net/http.ServeMux чистит его
	// до сопоставления с шаблоном и отдаёт 301 на очищенный адрес, поэтому
	// /mcp/../metrics попадает в /metrics и возвращается с 200. Это поведение
	// стандартной библиотеки, а не маршрутизации MCP; факт проверен живым
	// прогоном и зафиксирован здесь, чтобы его не приняли за дыру в 404.

	for _, path := range []string{"/foobar", "/admin/panel/deep"} {
		code, body := doReq(t, http.MethodPost, base+path, initBody, 3*time.Second)
		if code != http.StatusNotFound {
			t.Errorf("POST %s: статус %d, хочу 404", path, code)
		}
		if strings.Contains(body, "protocolVersion") {
			t.Errorf("POST %s: мусорный путь исполняет JSON-RPC: %.160q", path, body)
		}
	}

	code, body := doReq(t, http.MethodDelete, base+"/whatever", "", 3*time.Second)
	if code != http.StatusNotFound {
		t.Errorf("DELETE /whatever: статус %d, хочу 404 (тело %.80q)", code, body)
	}
}

func TestMCPStaysOnItsOwnPaths(t *testing.T) {
	base, stop := serveHTTP(t, "")
	defer stop()

	// Точный корень оставлен за MCP сознательно: клиенты, настроенные на url
	// без пути, не должны сломаться от правки маршрутизации.
	for _, path := range []string{"/mcp", "/"} {
		code, body := doReq(t, http.MethodPost, base+path, initBody, 5*time.Second)
		if code != http.StatusOK {
			t.Fatalf("POST %s: статус %d, хочу 200 (тело %.160q)", path, code, body)
		}
		if !strings.Contains(body, `"protocolVersion"`) {
			t.Errorf("POST %s: ответ не похож на initialize: %.160q", path, body)
		}
	}

	code, body := doReq(t, http.MethodGet, base+"/health", "", 3*time.Second)
	if code != http.StatusOK || strings.TrimSpace(body) != "ok" {
		t.Errorf("GET /health: статус %d тело %q, хочу 200 и «ok»", code, body)
	}
	code, _ = doReq(t, http.MethodGet, base+"/healthz", "", 3*time.Second)
	if code != http.StatusOK {
		t.Errorf("GET /healthz: статус %d, хочу 200", code)
	}
	code, _ = doReq(t, http.MethodGet, base+"/metrics", "", 3*time.Second)
	if code != http.StatusOK {
		t.Errorf("GET /metrics на сервере без токена: статус %d, хочу 200", code)
	}
}

func TestUnknownPathDoesNotHangTheConnection(t *testing.T) {
	base, stop := serveHTTP(t, "")
	defer stop()

	start := time.Now()
	code, body := doReq(t, http.MethodGet, base+"/long/unknown/path", "", 2*time.Second)
	if code == 0 {
		t.Fatalf("GET на неизвестном пути не ответил за 2 с: %s "+
			"(прежняя версия открывала бесконечный SSE-поток без статуса)", body)
	}
	if code != http.StatusNotFound {
		t.Errorf("GET /long/unknown/path: статус %d, хочу 404", code)
	}
	if took := time.Since(start); took > 1500*time.Millisecond {
		t.Errorf("404 на неизвестном пути занял %v", took)
	}
}

func TestNotFoundComesBeforeAuth(t *testing.T) {
	base, stop := serveHTTP(t, "s3cret")
	defer stop()

	code, body := doReq(t, http.MethodGet, base+"/foobar", "", 3*time.Second)
	if code != http.StatusNotFound {
		t.Errorf("GET /foobar на сервере с токеном: статус %d, хочу 404", code)
	}
	if strings.Contains(body, "unauthorized") {
		t.Error("мусорный путь сообщает, что на сервере включён токен")
	}

	for _, path := range []string{"/mcp", "/"} {
		code, _ := doReq(t, http.MethodPost, base+path, initBody, 3*time.Second)
		if code != http.StatusUnauthorized {
			t.Errorf("POST %s без Bearer: статус %d, хочу 401", path, code)
		}
	}
	code, _ = doReq(t, http.MethodGet, base+"/metrics", "", 3*time.Second)
	if code != http.StatusUnauthorized {
		t.Errorf("GET /metrics без Bearer: статус %d, хочу 401", code)
	}

	req, err := http.NewRequest(http.MethodPost, base+"/mcp", strings.NewReader(initBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer s3cret")
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		t.Errorf("POST /mcp с верным Bearer: статус %d, хочу 200 (тело %.160q)", resp.StatusCode, data)
	}
}
