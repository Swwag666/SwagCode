package netx

import (
	"strings"
	"testing"
)

func TestIsolateSpecAddsCredentials(t *testing.T) {
	got := IsolateSpec("socks5://127.0.0.1:9050", "probe0")
	if got == "socks5://127.0.0.1:9050" {
		t.Fatal("учётные данные не добавлены")
	}
	if !strings.Contains(got, "probe0") {
		t.Errorf("ключ не попал в спецификацию: %q", got)
	}
	if !strings.Contains(got, "127.0.0.1:9050") {
		t.Errorf("адрес потерян: %q", got)
	}
}

func TestIsolateSpecDistinctKeysDiffer(t *testing.T) {
	// Смысл изоляции в том, что разные ключи дают разные цепи. Если tor
	// получит одинаковую пару логин/пароль, все воркеры снова встанут в
	// очередь к одной цепи, и параллелизм останется фиктивным.
	a := IsolateSpec("socks5://127.0.0.1:9050", "probe0")
	b := IsolateSpec("socks5://127.0.0.1:9050", "probe1")
	if a == b {
		t.Errorf("разные ключи дали одинаковую спецификацию: %q", a)
	}
}

func TestIsolateSpecSameKeyStable(t *testing.T) {
	spec := "socks5://127.0.0.1:9050"
	if IsolateSpec(spec, "k") != IsolateSpec(spec, "k") {
		t.Error("одинаковый ключ даёт разные результаты")
	}
}

func TestIsolateSpecSocks5h(t *testing.T) {
	got := IsolateSpec("socks5h://127.0.0.1:9050", "w1")
	if !strings.HasPrefix(got, "socks5h://") {
		t.Errorf("схема изменена: %q", got)
	}
	if !strings.Contains(got, "w1") {
		t.Errorf("ключ не добавлен: %q", got)
	}
}

func TestIsolateSpecLeavesHTTPProxyAlone(t *testing.T) {
	// У чужого http-прокси учётные данные - это авторизация, а не маркер
	// изоляции. Подставить туда наш ключ значит сломать доступ к прокси.
	for _, spec := range []string{
		"http://1.2.3.4:8080",
		"http://user:pass@1.2.3.4:8080",
		"https://1.2.3.4:8080",
	} {
		if got := IsolateSpec(spec, "probe0"); got != spec {
			t.Errorf("http-спецификация изменена: %q -> %q", spec, got)
		}
	}
}

func TestIsolateSpecPreservesExistingCredentialsOnHTTP(t *testing.T) {
	spec := "http://user:pass@1.2.3.4:8080"
	got := IsolateSpec(spec, "x")
	if !strings.Contains(got, "user:pass") {
		t.Errorf("авторизация прокси потеряна: %q", got)
	}
}

func TestIsolateSpecEmptyInputs(t *testing.T) {
	cases := []struct{ spec, key string }{
		{"", "k"},
		{"socks5://127.0.0.1:9050", ""},
		{"   ", "k"},
		{"socks5://127.0.0.1:9050", "   "},
	}
	for _, c := range cases {
		got := IsolateSpec(c.spec, c.key)
		if strings.TrimSpace(c.spec) == "" {
			if strings.TrimSpace(got) != "" {
				t.Errorf("из пустой спецификации сделано %q", got)
			}
			continue
		}
		if got != strings.TrimSpace(c.spec) {
			t.Errorf("пустой ключ изменил спецификацию: %q -> %q", c.spec, got)
		}
	}
}

func TestIsolateSpecBadURL(t *testing.T) {
	// Неразбираемая спецификация возвращается как есть: лучше честная попытка
	// с исходным значением, чем паника или молчаливая подмена адреса.
	for _, spec := range []string{"::не url::", "socks5://"} {
		got := IsolateSpec(spec, "k")
		if strings.Contains(got, "k") && !strings.Contains(spec, "k") {
			t.Errorf("битая спецификация %q изменена в %q", spec, got)
		}
	}
}

func TestIsolateSpecTrimsInput(t *testing.T) {
	got := IsolateSpec("  socks5://127.0.0.1:9050  ", "  probe0  ")
	if strings.Contains(got, " ") {
		t.Errorf("пробелы не убраны: %q", got)
	}
	if !strings.Contains(got, "probe0") {
		t.Errorf("ключ не добавлен: %q", got)
	}
}
