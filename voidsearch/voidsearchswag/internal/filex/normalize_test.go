package filex

import (
	"strings"
	"testing"
)

func TestNormalizeBase(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"   ", ""},
		{"abc.onion", "http://abc.onion"},
		{"  abc.onion  ", "http://abc.onion"},
		{"http://abc.onion", "http://abc.onion"},
		{"https://example.com", "https://example.com"},
		{"http://abc.onion/", "http://abc.onion"},
		{"http://abc.onion///", "http://abc.onion"},
		{"HTTP://ABC.ONION", "HTTP://ABC.ONION"},
		{"socks5://127.0.0.1:9050", "socks5://127.0.0.1:9050"},
		{"http://abc.onion/search", "http://abc.onion/search"},
		{"127.0.0.1:8080", "http://127.0.0.1:8080"},
	}
	for _, c := range cases {
		if got := NormalizeBase(c.in); got != c.want {
			t.Errorf("NormalizeBase(%q) = %q, ожидала %q", c.in, got, c.want)
		}
	}
}

func TestNormalizeBaseIdempotent(t *testing.T) {
	// Двойное применение не должно менять результат: вызывающие нормализуют
	// адрес в разных слоях, и накопление «http://http://» сломало бы запрос.
	for _, in := range []string{"abc.onion", "http://abc.onion", "https://x.example/y", ""} {
		once := NormalizeBase(in)
		twice := NormalizeBase(once)
		if once != twice {
			t.Errorf("не идемпотентно: %q -> %q -> %q", in, once, twice)
		}
	}
}

func TestNormalizeBaseKeepsExistingScheme(t *testing.T) {
	// Схема не должна подменяться: https-источник, приведённый к http,
	// потерял бы доступ к своему onion-адресу.
	got := NormalizeBase("https://ahmia.fi/search")
	if !strings.HasPrefix(got, "https://") {
		t.Errorf("схема заменена: %q", got)
	}
}

func TestNormalizeBaseBareHostIsURLParseable(t *testing.T) {
	// Смысл нормализации в том, чтобы голый хост из пула перестал быть
	// «invalid URL scheme: []» для url.Parse и http-клиента.
	got := NormalizeBase("metagerv65pwclop2rsfzg4jwowpavpwd6grhhlvdgsswvo6ii4akgyd.onion")
	if !strings.HasPrefix(got, "http://") {
		t.Fatalf("голый onion не получил схему: %q", got)
	}
	if strings.Contains(got, "://http") {
		t.Errorf("схема удвоена: %q", got)
	}
}
