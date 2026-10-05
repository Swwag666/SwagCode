package mcpserver

import (
	"net/http/httptest"
	"testing"
)

func TestCheckBearerAllowsEmptyToken(t *testing.T) {
	r := httptest.NewRequest("POST", "/mcp", nil)
	if !checkBearer("", r) {
		t.Error("пустой токен обязан пропускать всех")
	}
}

func TestCheckBearerHeader(t *testing.T) {
	r := httptest.NewRequest("POST", "/mcp", nil)
	r.Header.Set("Authorization", "Bearer s3cret")
	if !checkBearer("s3cret", r) {
		t.Error("валидный Bearer отклонён")
	}
	r2 := httptest.NewRequest("POST", "/mcp", nil)
	r2.Header.Set("Authorization", "Bearer чужой")
	if checkBearer("s3cret", r2) {
		t.Error("чужой Bearer принят")
	}
}

func TestCheckBearerAltHeader(t *testing.T) {
	r := httptest.NewRequest("POST", "/mcp", nil)
	r.Header.Set("X-API-Token", "s3cret")
	if !checkBearer("s3cret", r) {
		t.Error("X-API-Token отклонён")
	}
}

func TestCheckBearerMissing(t *testing.T) {
	r := httptest.NewRequest("POST", "/mcp", nil)
	if checkBearer("s3cret", r) {
		t.Error("запрос без токена принят")
	}
}
