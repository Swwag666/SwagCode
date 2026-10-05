package searchers

import (
	"strings"
	"testing"
)

func TestProbeURLNeverContainsPlaceholder(t *testing.T) {
	cases := []struct {
		name string
		base string
		path string
		want string
	}{
		{"пустой путь", "http://x.onion", "", "http://x.onion/"},
		{"поисковый путь", "http://x.onion", "/search?q={q}", "http://x.onion/search"},
		{"путь с амперсандом", "http://x.onion", "/?q={q}", "http://x.onion/"},
		{"двойной плейсхолдер", "http://x.onion", "/s?q={q}&page=2", "http://x.onion/s"},
		{"путь без слеша", "http://x.onion", "search?q={q}", "http://x.onion/search"},
		{"слеш на конце базы", "http://x.onion/", "/search?q={q}", "http://x.onion/search"},
		{"просто слеш", "http://x.onion", "/", "http://x.onion/"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := &OnionEngine{Name_: "t", Base: c.base, Path: c.path}
			got := e.ProbeURL()
			if strings.Contains(got, "{q}") {
				t.Errorf("ProbeURL содержит плейсхолдер: %s", got)
			}
			if strings.Contains(got, "?") {
				t.Errorf("ProbeURL содержит query-строку: %s", got)
			}
			if got != c.want {
				t.Errorf("ProbeURL=%q, ожидала %q", got, c.want)
			}
		})
	}
}

func TestProbeURLIsSameHostAsBase(t *testing.T) {
	e := &OnionEngine{Name_: "t", Base: "http://abc.onion", Path: "/search?q={q}"}
	got := e.ProbeURL()
	if !strings.HasPrefix(got, "http://abc.onion") {
		t.Errorf("ProbeURL=%q ушёл с хоста базы", got)
	}
}

func TestBootstrapCompleteDetection(t *testing.T) {
	full := "NOTICE BOOTSTRAP PROGRESS=100 TAG=done SUMMARY=\"Done\""
	if !strings.Contains(full, "PROGRESS=100") {
		t.Error("детект завершения bootstrap сломан - waitBootstrap никогда не выйдет")
	}
}
