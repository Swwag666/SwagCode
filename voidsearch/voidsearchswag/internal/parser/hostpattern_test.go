package parser

import "testing"

func TestHostPatternIPv6(t *testing.T) {
	// Прежняя версия отсекала хост по последнему двоеточию, предполагая что
	// это всегда порт. Для IPv6-адреса в скобках без порта это давало мусор:
	// Host равен "[2001:db8::1]", а после отсечения оставалось "[:", то есть
	// ключ реестра, по которому сохранённые селекторы не находились никогда.
	cases := []struct {
		url  string
		want string
	}{
		{"http://[2001:db8::1]/path", "2001:db8::1"},
		{"http://[2001:db8::1]:8080/path", "2001:db8::1"},
		{"https://[::1]/", "::1"},
		{"https://Example.COM/Path", "example.com"},
		{"https://example.com:8443/x", "example.com"},
		{"http://plain.onion/search?q=1", "plain.onion"},
		{"http://127.0.0.1:9050/", "127.0.0.1"},
	}
	for _, c := range cases {
		got, err := HostPattern(c.url)
		if err != nil {
			t.Errorf("%s: ошибка %v", c.url, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: получено %q, ожидала %q", c.url, got, c.want)
		}
	}
}

func TestHostPatternRejectsBadURLs(t *testing.T) {
	for _, bad := range []string{"", "   ", "не url", "://x", "/relative/path", "mailto:a@b.c"} {
		if got, err := HostPattern(bad); err == nil {
			t.Errorf("%q принят как %q", bad, got)
		}
	}
}

func TestHostPatternStableAcrossSameHost(t *testing.T) {
	// Ключ обязан быть одинаковым для разных страниц одного хоста: селекторы
	// привязаны к хосту, а не к странице, потому что вёрстка общая.
	a, err := HostPattern("https://example.com/page/one")
	if err != nil {
		t.Fatal(err)
	}
	b, err := HostPattern("https://example.com:443/page/two?x=1")
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Errorf("один хост дал разные ключи: %q и %q", a, b)
	}
}
