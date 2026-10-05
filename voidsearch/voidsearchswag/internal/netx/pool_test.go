package netx

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

type nopLogger struct{}

func (nopLogger) Infof(string, ...any) {}
func (nopLogger) Warnf(string, ...any) {}

func TestParseProxySpecFormats(t *testing.T) {
	cases := []struct {
		in         string
		scheme     string
		host       string
		user, pass string
		wantErr    bool
	}{
		{in: "1.2.3.4:8080", scheme: "socks5", host: "1.2.3.4:8080"},
		{in: "http://1.2.3.4:8080", scheme: "http", host: "1.2.3.4:8080"},
		{in: "https://1.2.3.4:3128", scheme: "https", host: "1.2.3.4:3128"},
		{in: "socks5://1.2.3.4:1080", scheme: "socks5", host: "1.2.3.4:1080"},
		{in: "socks4://1.2.3.4:1080", scheme: "socks4", host: "1.2.3.4:1080"},
		{in: "http://1.2.3.4:8080:user:pass", scheme: "http", host: "1.2.3.4:8080", user: "user", pass: "pass"},
		{in: "socks5://user:pass@1.2.3.4:1080", scheme: "socks5", host: "1.2.3.4:1080", user: "user", pass: "pass"},
		{in: "1.2.3.4:8080:u1:p2", scheme: "socks5", host: "1.2.3.4:8080", user: "u1", pass: "p2"},
		{in: "", wantErr: true},
		{in: "ftp://1.2.3.4:21", wantErr: true},
		{in: "http://no-port", wantErr: true},
		{in: "not-an-address", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			pi, err := parseProxySpec(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("%q: ожидала ошибку, получила %+v", tc.in, pi)
				}
				return
			}
			if err != nil {
				t.Fatalf("%q: %v", tc.in, err)
			}
			if pi.scheme != tc.scheme {
				t.Errorf("%q: scheme=%q, надо %q", tc.in, pi.scheme, tc.scheme)
			}
			if pi.host != tc.host {
				t.Errorf("%q: host=%q, надо %q", tc.in, pi.host, tc.host)
			}
			if pi.user != tc.user || pi.pass != tc.pass {
				t.Errorf("%q: cred=%q/%q, надо %q/%q", tc.in, pi.user, pi.pass, tc.user, tc.pass)
			}
		})
	}
}

func TestSchemeFor(t *testing.T) {
	for proto, want := range map[string]string{
		"http": "http://", "https": "https://", "socks5": "socks5://",
		"socks5h": "socks5://", "socks4": "socks4://", "socks4a": "socks4://", "": "http://",
	} {
		if got := schemeFor(proto); got != want {
			t.Errorf("schemeFor(%q)=%q, надо %q", proto, got, want)
		}
	}
}

func TestExtractIPFromProbes(t *testing.T) {
	cases := map[string]string{
		`{"ip":"8.9.10.11"}`:                 "8.9.10.11",
		`{"ip":"88.7.6.5","asn":123}`:        "88.7.6.5",
		`{"query":"77.88.55.242","as":9002}`: "77.88.55.242",
		`{"ip":""}`:                          "",
		`plain text`:                         "",
		`{"nope":"1.1.1.1"}`:                 "",
	}
	for in, want := range cases {
		if got := extractIP([]byte(in)); got != want {
			t.Errorf("extractIP(%q)=%q, надо %q", in, got, want)
		}
	}
}

func TestProbeTargetsAreJSON(t *testing.T) {
	b, err := json.Marshal(LiveProxy{Spec: "http://1.2.3.4:8080", ExitIP: "9.9.9.9", Latency: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if len(b) == 0 {
		t.Fatal("пусто")
	}
	for _, u := range probeTargets {
		if u == "" {
			t.Fatal("пустая цель пробы")
		}
	}
}

func TestProxyScrapeURL(t *testing.T) {
	p := DefaultProxyScrape()
	u := p.url()
	for _, want := range []string{"api.proxyscrape.com/v2/", "request=getproxies", "protocol=http", "country=all", "timeout=8000"} {
		if !strings.Contains(u, want) {
			t.Errorf("url %q не содержит %q", u, want)
		}
	}
	q := ProxyScrape{Protocol: "socks5", Country: "DE", TimeoutMS: 3000, Anonymity: "elite", SSL: "yes"}
	su := q.url()
	for _, want := range []string{"protocol=socks5", "country=de", "timeout=3000", "anonymity=elite", "ssl=yes"} {
		if !strings.Contains(su, want) {
			t.Errorf("url %q не содержит %q", su, want)
		}
	}
}

func TestProxyPoolNextMarksDead(t *testing.T) {
	pool := &ProxyPool{
		cfg:  PoolConfig{Provider: StaticProvider{"http://1.1.1.1:1"}, Verify: true, MaxLive: 5, MinLive: 1, RefillWait: time.Hour},
		log:  nopLogger{},
		dead: map[string]int{},
	}
	pool.add(LiveProxy{Spec: "http://a:1", ExitIP: "1.1.1.1", Latency: 10 * time.Millisecond})
	pool.add(LiveProxy{Spec: "http://b:2", ExitIP: "2.2.2.2", Latency: 20 * time.Millisecond})
	pool.add(LiveProxy{Spec: "http://a:1", ExitIP: "1.1.1.1"})

	lp, ok := pool.Next()
	if !ok || lp.Spec != "http://a:1" {
		t.Fatalf("Next()=%+v ok=%v (ожидала самый свежий-неиспользованный a)", lp, ok)
	}
	if lp2, _ := pool.Next(); lp2.Spec != "http://b:2" {
		t.Fatalf("LRU сломан: второй вызов дал %q", lp2.Spec)
	}
	pool.MarkDead("http://b:2", "тест")
	live, total, killed := pool.Stats()
	if live != 1 || total != 2 || killed != 1 {
		t.Fatalf("после MarkDead: live=%d total=%d killed=%d", live, total, killed)
	}
	if lp3, ok3 := pool.Next(); !ok3 || lp3.Spec != "http://a:1" {
		t.Fatalf("мёртвый не должен выдаваться: %+v ok=%v", lp3, ok3)
	}
	pool.MarkDead("http://b:2", "уже мёртв")
	if _, _, k := pool.Stats(); k != 1 {
		t.Fatalf("повторный MarkDead удвоил счётчик: killed=%d", k)
	}
}

func TestProxyPoolMaxLive(t *testing.T) {
	pool := &ProxyPool{cfg: PoolConfig{Verify: true, MaxLive: 2, MinLive: 1, RefillWait: time.Hour}, dead: map[string]int{}}
	for i := 0; i < 10; i++ {
		pool.add(LiveProxy{Spec: string(rune('a'+i)) + ":1"})
	}
	if live, total, _ := pool.Stats(); live != 2 || total != 2 {
		t.Fatalf("MaxLive не сработал: live=%d total=%d", live, total)
	}
}

func TestPoolStateSkipDead(t *testing.T) {
	ps := LoadPoolState("", nil)
	ps.MarkDead("http://a:1", "boom")
	if why := ps.Skip("http://a:1", ""); why == "" {
		t.Fatal("мёртвый адрес должен скипаться")
	}
	if why := ps.Skip("http://b:2", ""); why != "" {
		t.Fatalf("чистый адрес не должен скипаться: %s", why)
	}
}

func TestPoolStateBanIPAndNet24(t *testing.T) {
	ps := LoadPoolState("", nil)
	ps.MarkBanned("http://b:2", "9.9.9.9", "ban")
	if why := ps.Skip("http://c:3", "9.9.9.9"); why == "" {
		t.Fatal("бан по IP должен скипать чужой адрес с тем же выходом")
	}
	if why := ps.Skip("http://d:4", "9.9.9.10"); why == "" {
		t.Fatal("/24 соседа должен скипаться")
	}
	if why := ps.Skip("http://e:5", "8.8.8.8"); why != "" {
		t.Fatalf("чистый выход не должен скипаться: %s", why)
	}
}

func TestPoolStateSuspectStrikesToBan(t *testing.T) {
	ps := LoadPoolState("", nil)
	ps.MarkSuspect("http://s:1", "1.2.3.4", "fp-несовпадение")
	ps.MarkSuspect("http://s:1", "1.2.3.4", "fp-несовпадение")
	if why := ps.Skip("http://s:1", ""); why != "" {
		t.Fatalf("два страйка - ещё не бан: %s", why)
	}
	ps.MarkSuspect("http://s:1", "1.2.3.4", "fp-несовпадение")
	if why := ps.Skip("http://s:1", ""); why == "" {
		t.Fatal("три страйка должны дать бан")
	}
	if why := ps.Skip("http://x:9", "1.2.3.4"); why == "" {
		t.Fatal("после страйк-бана IP тоже должен быть забанен")
	}
}

func TestPoolStateSaveLoadRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/state.json"
	ps := LoadPoolState(path, nil)
	ps.MarkDead("http://a:1", "boom")
	ps.RememberExit("http://b:2", "7.7.7.7")
	if err := ps.Save(); err != nil {
		t.Fatal(err)
	}
	ps2 := LoadPoolState(path, nil)
	if why := ps2.Skip("http://a:1", ""); why == "" {
		t.Fatal("после reload мёртвый не сохранился")
	}
	if ip := ps2.ExitIP("http://b:2"); ip != "7.7.7.7" {
		t.Fatalf("exit IP не пережил reload: %q", ip)
	}
}
