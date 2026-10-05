package netx

import (
	impersonate "github.com/North-web-dev/impersonate-http"
	"github.com/bogdanfinn/tls-client/profiles"
)

type Fingerprint struct {
	Name        string
	Kind        string
	TLSKey      string
	Profile     impersonate.Profile
	HeaderOrder []string
	PHeader     []string
	Accept      string
	AcceptLang  string
	SecCH       string
	SecCHMobile string
	SecCHPlat   string
	UserAgent   string
}

const enUS = "en-US,en;q=0.9"

const (
	kindChrome  = "chrome"
	kindFirefox = "firefox"
	kindSafari  = "safari"
	kindEdge    = "edge"
)

const (
	acceptChromium = "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7"
	acceptFirefox  = "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8"
	acceptSafari   = "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"
)

var orderChromium = []string{
	"sec-ch-ua", "sec-ch-ua-mobile", "sec-ch-ua-platform",
	"upgrade-insecure-requests", "user-agent", "accept",
	"sec-fetch-site", "sec-fetch-mode", "sec-fetch-user", "sec-fetch-dest",
	"referer", "accept-encoding", "accept-language", "cookie",
}

var orderFirefox = []string{
	"user-agent", "accept", "accept-language", "accept-encoding",
	"alt-used", "dnt", "upgrade-insecure-requests",
	"sec-fetch-dest", "sec-fetch-mode", "sec-fetch-site", "sec-fetch-user",
	"referer", "cookie",
}

var orderSafari = []string{
	"accept-language", "accept-encoding", "accept", "user-agent",
	"referer", "cookie",
}

var Fingerprints = []Fingerprint{
	{
		Name: "chrome_131_win", Kind: kindChrome, TLSKey: "chrome_131", Profile: impersonate.Chrome,
		HeaderOrder: orderChromium, PHeader: []string{":method", ":authority", ":scheme", ":path"},
		Accept: acceptChromium, AcceptLang: enUS,
		SecCH:       `"Not(A:Brand";v="99", "Google Chrome";v="131", "Chromium";v="131"`,
		SecCHMobile: "?0", SecCHPlat: `"Windows"`,
		UserAgent: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
	},
	{
		Name: "chrome_124_win", Kind: kindChrome, TLSKey: "chrome_124", Profile: impersonate.Chrome,
		HeaderOrder: orderChromium, PHeader: []string{":method", ":authority", ":scheme", ":path"},
		Accept:      "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8",
		AcceptLang:  enUS,
		SecCH:       `"Chromium";v="124", "Google Chrome";v="124", "Not-A.Brand";v="99"`,
		SecCHMobile: "?0", SecCHPlat: `"Windows"`,
		UserAgent: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
	},
	{
		Name: "edge_131_win", Kind: kindEdge, TLSKey: "chrome_131", Profile: impersonate.Edge,
		HeaderOrder: orderChromium, PHeader: []string{":method", ":authority", ":scheme", ":path"},
		Accept: acceptChromium, AcceptLang: enUS,
		SecCH:       `"Not/A)Brand";v="99", "Microsoft Edge";v="131", "Chromium";v="131"`,
		SecCHMobile: "?0", SecCHPlat: `"Windows"`,
		UserAgent: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36 Edg/131.0.0.0",
	},
	{
		Name: "firefox_133_win", Kind: kindFirefox, TLSKey: "firefox_133", Profile: impersonate.Firefox,
		HeaderOrder: orderFirefox, PHeader: []string{":method", ":path", ":authority", ":scheme"},
		Accept: acceptFirefox, AcceptLang: enUS,
		UserAgent: "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:133.0) Gecko/20100101 Firefox/133.0",
	},
	{
		Name: "safari_16_mac", Kind: kindSafari, TLSKey: "safari_16_0", Profile: impersonate.Safari,
		HeaderOrder: orderSafari, PHeader: []string{":method", ":scheme", ":path", ":authority"},
		Accept: acceptSafari, AcceptLang: "en-US,en;q=0.8",
		UserAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/16.6 Safari/605.1.15",
	},
	{
		Name: "chrome_131_android", Kind: kindChrome, TLSKey: "chrome_131", Profile: impersonate.ChromeAndroid,
		HeaderOrder: orderChromium, PHeader: []string{":method", ":authority", ":scheme", ":path"},
		Accept: acceptChromium, AcceptLang: enUS,
		SecCH:       `"Not(A:Brand";v="99", "Google Chrome";v="131", "Chromium";v="131"`,
		SecCHMobile: "?1", SecCHPlat: `"Android"`,
		UserAgent: "Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Mobile Safari/537.36",
	},
	{
		Name: "safari_17_ios", Kind: kindSafari, TLSKey: "safari_ios_17_0", Profile: impersonate.IOS,
		HeaderOrder: orderSafari, PHeader: []string{":method", ":scheme", ":path", ":authority"},
		Accept: acceptSafari, AcceptLang: "en-US,en;q=0.8",
		UserAgent: "Mozilla/5.0 (iPhone; CPU iPhone OS 17_4 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.4 Mobile/15E148 Safari/604.1",
	},
}

func FingerprintByName(name string) (Fingerprint, bool) {
	for _, f := range Fingerprints {
		if f.Name == name {
			return f, true
		}
	}
	return Fingerprint{}, false
}

func (f Fingerprint) TLSProfile() (profiles.ClientProfile, bool) {
	p, ok := profiles.MappedTLSClients[f.TLSKey]
	return p, ok
}
