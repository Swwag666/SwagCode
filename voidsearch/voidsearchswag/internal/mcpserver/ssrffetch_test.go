package mcpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"

	"voidsearchswag/internal/httpc"
	"voidsearchswag/internal/search"
	"voidsearchswag/internal/store"
)

// ssrfServer поднимает локальный стенд с «секретом» в теле и возвращает клиент
// MCP, у которого инструмент fetch ходит через настоящий http-клиент. Именно на
// этом пути живой замер ДО вернул тело локального сервиса внешнему вызывающему.
func ssrfServer(t *testing.T, allowPrivate bool) (*client.Client, string, string) {
	t.Helper()
	const secret = "INTERNAL-ONLY secret=AKIA1234567890"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/plain; charset=utf-8")
		w.Write([]byte(secret))
	}))
	t.Cleanup(srv.Close)

	st, err := store.Open(t.TempDir() + "/ssrf.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}

	cl, err := httpc.NewClient(context.Background(), httpc.Options{Transport: "direct", Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cl.Close() })

	eng := &search.Engine{Store: st, Client: cl, AllowPrivateTarget: allowPrivate}
	c := inProcess(t, New(Deps{Version: "test", Store: st, Search: eng, Started: time.Now()}))
	initClient(t, c)
	return c, srv.URL + "/internal", secret
}

// clip обрезает строку для сообщения теста: ответы инструмента бывают длинными,
// а в диагностике нужна только голова.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func callFetch(t *testing.T, c *client.Client, raw string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "fetch", Arguments: map[string]any{"url": raw}},
	})
	if err != nil {
		t.Fatalf("вызов инструмента: %v", err)
	}
	text := textOf(t, res)
	if res.IsError {
		return "ERROR: " + text
	}
	return text
}

// TestFetchToolRefusesPrivateTarget: инструмент обязан отказать на служебный
// адрес и не вернуть ни байта тела. До правки тот же вызов отдавал 200 и тело.
func TestFetchToolRefusesPrivateTarget(t *testing.T) {
	c, localURL, secret := ssrfServer(t, false)

	targets := []string{
		localURL,
		"http://localhost:1/x",
		"http://[::1]:8080/",
		"http://169.254.169.254/latest/meta-data/",
		"http://192.168.1.1/admin",
		"http://10.0.0.5:9200/_search",
		"http://100.64.0.1/",
	}
	for _, raw := range targets {
		got := callFetch(t, c, raw)
		if !strings.HasPrefix(got, "ERROR: ") {
			t.Errorf("%s: инструмент не отказал, ответ %s", raw, clip(got, 200))
			continue
		}
		if !strings.Contains(got, "служебной или приватной сети") {
			t.Errorf("%s: причина не названа: %s", raw, clip(got, 200))
		}
		if strings.Contains(got, secret) {
			t.Errorf("%s: тело локального сервиса утекло в ответ", raw)
		}
	}
}

// TestFetchToolRefusesForeignScheme: схема, которой нет среди http и https,
// отклоняется барьером, а не транспортом, то есть до попытки запроса.
func TestFetchToolRefusesForeignScheme(t *testing.T) {
	c, _, _ := ssrfServer(t, false)

	targets := []string{
		"file:///C:/Windows/System32/drivers/etc/hosts",
		"gopher://127.0.0.1:70/",
		"ftp://example.com/x",
	}
	for _, raw := range targets {
		got := callFetch(t, c, raw)
		if !strings.HasPrefix(got, "ERROR: ") {
			t.Errorf("%s: схема принята, ответ %s", raw, clip(got, 200))
			continue
		}
		if !strings.Contains(got, "неподдерживаемая схема") {
			t.Errorf("%s: причина не названа: %s", raw, clip(got, 200))
		}
	}
}

// TestFetchToolServesLocalStandWhenAllowed: барьер снимается явно, и локальный
// стенд разработчика продолжает работать. Без этого переключателя отладка
// своего сервиса через fetch стала бы невозможной.
func TestFetchToolServesLocalStandWhenAllowed(t *testing.T) {
	c, localURL, secret := ssrfServer(t, true)

	got := callFetch(t, c, localURL)
	if strings.HasPrefix(got, "ERROR: ") {
		t.Fatalf("локальный стенд отклонён при снятом барьере: %s", clip(got, 300))
	}
	if !strings.Contains(got, secret) {
		t.Errorf("тела нет в ответе: %s", clip(got, 300))
	}
	if !strings.Contains(got, `"status": 200`) {
		t.Errorf("статус не 200: %s", clip(got, 300))
	}
}
