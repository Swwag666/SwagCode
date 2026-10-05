package httpc

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestPlainClientActuallyUsesSocks5 проверяет, что plain-клиент реально
// ходит через SOCKS5, а не игнорирует прокси. Если net/http.Transport не
// понимает схему socks5, запрос уходит напрямую: для .onion это и падение
// (системный DNS не знает таких зон), и утечка - резолвер провайдера видит,
// что ищут onion-адрес.
func TestPlainClientActuallyUsesSocks5(t *testing.T) {
	// Порт 1 заведомо закрыт. Если прокси используется, ошибка будет про
	// подключение к прокси; если нет - про DNS-резолвинг хоста.
	c := newPlainClient("socks5://127.0.0.1:1", 3*time.Second)
	defer c.CloseIdleConnections()

	_, err := c.Get("http://nonexistent-host-for-test.onion/")
	if err == nil {
		t.Fatal("запрос через закрытый прокси unexpectedly прошёл")
	}
	msg := err.Error()
	t.Logf("ошибка: %s", msg)

	if strings.Contains(msg, "lookup") || strings.Contains(msg, "no such host") {
		t.Fatalf("plain-клиент ушёл НАПРЯМУЮ, проигнорировав socks5-прокси: %v", err)
	}
	if !strings.Contains(msg, "127.0.0.1:1") && !strings.Contains(strings.ToLower(msg), "proxy") &&
		!strings.Contains(strings.ToLower(msg), "socks") {
		t.Fatalf("ошибка не указывает на прокси: %v", err)
	}
}

func TestStdTransportSupportsSocks5Scheme(t *testing.T) {
	// Прямая проверка std-транспорта без нашей обёртки: тот же вопрос.
	tr := &http.Transport{
		Proxy: http.ProxyURL(&url.URL{Scheme: "socks5", Host: "127.0.0.1:1"}),
	}
	c := &http.Client{Transport: tr, Timeout: 3 * time.Second}
	defer c.CloseIdleConnections()

	_, err := c.Get("http://nonexistent-host-for-test.onion/")
	if err == nil {
		t.Fatal("запрос прошёл через закрытый прокси")
	}
	msg := err.Error()
	t.Logf("std transport ошибка: %s", msg)
	if strings.Contains(msg, "lookup") || strings.Contains(msg, "no such host") {
		t.Fatalf("std Transport игнорирует socks5 и уходит напрямую: %v", err)
	}
}
