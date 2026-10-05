package mcpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Ответ MCP несёт UTF-8 (note с кириллицей, limit_hit, errors), а mcp-go
// ставит Content-Type: application/json без параметров - смоук-агент этапа 168
// получил mojibake в PowerShell 5.1, которая без charset читает ISO-8859-1.
func TestCharsetMiddlewareAugmentsJSON(t *testing.T) {
	body := []byte(`{"note":"под меткой нет записей"}`)
	h := charsetMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", nil))

	if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("charset не дотянут: Content-Type = %q, хочу application/json; charset=utf-8", got)
	}
	if string(rec.Body.Bytes()) != string(body) {
		t.Fatalf("тело искажено: %q", rec.Body.String())
	}
}

// SSE и текстовые ответы не трогаем: замена только для точного application/json.
func TestCharsetMiddlewareLeavesOtherTypesAlone(t *testing.T) {
	for _, ct := range []string{"text/event-stream", "text/plain; charset=utf-8"} {
		h := charsetMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", ct)
			_, _ = w.Write([]byte("data: ok\n\n"))
		}))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/mcp", nil))
		if got := rec.Header().Get("Content-Type"); got != ct {
			t.Fatalf("Content-Type %q заменён на %q - не должен", ct, got)
		}
	}
}

// Write без явного WriteHeader - паттерн /metrics: заголовок стоит,
// тело пишет json.NewEncoder. Обёртка обязана дотянуть charset и тут:
// неявный код ответа подставляется внутри Write.
func TestCharsetMiddlewareAugmentsImplicitHeader(t *testing.T) {
	h := charsetMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			Note string `json:"note"`
		}{"записей нет"})
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", nil))
	if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("неявный заголовок без charset: %q", got)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("код ответа %d, хочу 200", rec.Code)
	}
}

// Без проброса Flush SSE-поток streamable HTTP завис бы в буфере обёртки.
func TestCharsetMiddlewareForwardsFlush(t *testing.T) {
	h := charsetMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("http.Flusher не проброшен через обёртку")
		}
		f.Flush()
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/mcp", nil))
	if !rec.Flushed {
		t.Fatal("Flush не дошёл до рекордера")
	}
}
