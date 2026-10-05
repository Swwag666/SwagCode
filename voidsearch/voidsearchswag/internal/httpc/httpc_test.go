package httpc

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

func TestDecodeGzip(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte("<html>gzip body</html>")); err != nil {
		t.Fatal(err)
	}
	zw.Close()
	got := Decode(buf.Bytes())
	if string(got) != "<html>gzip body</html>" {
		t.Errorf("gzip: %q", got)
	}
}

func TestDecodeZstd(t *testing.T) {
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zw.Write([]byte("<html>zstd body</html>")); err != nil {
		t.Fatal(err)
	}
	zw.Close()
	got := Decode(buf.Bytes())
	if string(got) != "<html>zstd body</html>" {
		t.Errorf("zstd: %q", got)
	}
}

func TestDecodeBrotli(t *testing.T) {
	var buf bytes.Buffer
	bw := brotli.NewWriter(&buf)
	if _, err := bw.Write([]byte("<html>brotli body</html>")); err != nil {
		t.Fatal(err)
	}
	bw.Close()
	got := Decode(buf.Bytes())
	if string(got) != "<html>brotli body</html>" {
		t.Errorf("brotli: %q", got)
	}
}

func TestDecodeDeflate(t *testing.T) {
	var buf bytes.Buffer
	fw, err := flate.NewWriter(&buf, flate.DefaultCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte("<html>deflate body</html>")); err != nil {
		t.Fatal(err)
	}
	fw.Close()
	got := Decode(buf.Bytes())
	if string(got) != "<html>deflate body</html>" {
		t.Errorf("deflate: %q", got)
	}
}

func TestDecodePlainPassthrough(t *testing.T) {
	in := []byte("<html>not compressed at all, plain text</html>")
	got := Decode(in)
	if !bytes.Equal(got, in) {
		t.Errorf("плейнтекст изменён: %q", got)
	}
}

func TestDecodeJSONPassthrough(t *testing.T) {
	in := []byte(`{"status":"ok","items":[1,2,3]}`)
	got := Decode(in)
	if !bytes.Equal(got, in) {
		t.Errorf("JSON изменён: %q", got)
	}
}

func TestDecodeEmpty(t *testing.T) {
	if got := Decode(nil); len(got) != 0 {
		t.Errorf("nil -> %q", got)
	}
	if got := Decode([]byte{}); len(got) != 0 {
		t.Errorf("пустой -> %q", got)
	}
}

func TestDecodeTruncatedGzipReturnsRaw(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write([]byte("some longer content to compress properly here"))
	zw.Close()
	truncated := buf.Bytes()[:4]
	got := Decode(truncated)
	if len(got) == 0 {
		t.Error("битый gzip не должен возвращать пустоту без фоллбэка")
	}
}

func TestClassifyCaptcha(t *testing.T) {
	r := &Response{Status: http.StatusForbidden, Body: []byte("<html>Please solve the captcha</html>")}
	err := Classify(r)
	if err == nil {
		t.Fatal("403 с капчей должен классифицироваться")
	}
	var be *BlockedError
	if !errors.As(err, &be) {
		t.Fatalf("err не BlockedError: %T", err)
	}
	if be.Kind != BlockCaptcha {
		t.Errorf("kind=%v, ожидала BlockCaptcha", be.Kind)
	}
	if !IsBlocked(err) {
		t.Error("IsBlocked не распознал ошибку")
	}
}

func TestClassifyIPBan(t *testing.T) {
	r := &Response{Status: http.StatusForbidden, Body: []byte("Ihr IP-Bereich wurde gesperrt")}
	err := Classify(r)
	var be *BlockedError
	if !errors.As(err, &be) {
		t.Fatalf("err: %v", err)
	}
	if be.Kind != BlockIPBan {
		t.Errorf("kind=%v, ожидала BlockIPBan", be.Kind)
	}
}

func TestClassifyEdgeDeny(t *testing.T) {
	r := &Response{Status: http.StatusForbidden, Body: []byte("<html>Access Denied</html>")}
	err := Classify(r)
	var be *BlockedError
	if !errors.As(err, &be) {
		t.Fatalf("err: %v", err)
	}
	if be.Kind != BlockEdge {
		t.Errorf("kind=%v, ожидала BlockEdge", be.Kind)
	}
}

func TestClassifyRateLimit(t *testing.T) {
	r := &Response{Status: http.StatusTooManyRequests, Body: []byte("slow down")}
	err := Classify(r)
	var be *BlockedError
	if !errors.As(err, &be) {
		t.Fatalf("err: %v", err)
	}
	if be.Kind != BlockRate {
		t.Errorf("kind=%v, ожидала BlockRate", be.Kind)
	}
}

func TestClassifyAuth(t *testing.T) {
	r := &Response{Status: http.StatusUnauthorized, Body: []byte("login")}
	err := Classify(r)
	var be *BlockedError
	if !errors.As(err, &be) {
		t.Fatalf("err: %v", err)
	}
	if be.Kind != BlockAuth {
		t.Errorf("kind=%v, ожидала BlockAuth", be.Kind)
	}
}

func TestClassifySuccessIsNil(t *testing.T) {
	for _, st := range []int{200, 201, 204, 301, 302, 404} {
		if err := Classify(&Response{Status: st, Body: []byte("ok")}); err != nil {
			t.Errorf("статус %d ошибочно классифицирован: %v", st, err)
		}
	}
}

func TestClassifyNilResponse(t *testing.T) {
	if err := Classify(nil); err == nil {
		t.Error("nil-ответ должен давать ошибку")
	}
}

func TestBlockedErrorMessage(t *testing.T) {
	e := &BlockedError{Kind: BlockRate, Status: 429}
	if !strings.Contains(e.Error(), "rate limit") {
		t.Errorf("msg=%q", e.Error())
	}
	if e.Unwrap() != ErrBlocked {
		t.Error("Unwrap не возвращает ErrBlocked")
	}
}

func TestResponseHelpers(t *testing.T) {
	r := &Response{Status: 200, Body: []byte("hello")}
	if !r.OK() {
		t.Error("200 должен быть OK")
	}
	if r.Text() != "hello" {
		t.Errorf("Text=%q", r.Text())
	}
	if (&Response{Status: 500}).OK() {
		t.Error("500 не должен быть OK")
	}
}

func TestXHROrderDiffersByBrowser(t *testing.T) {
	if len(xhrOrder("chrome")) == 0 || len(xhrOrder("firefox")) == 0 || len(xhrOrder("safari")) == 0 {
		t.Fatal("порядок заголовков пуст")
	}
	chrome := strings.Join(xhrOrder("chrome"), ",")
	firefox := strings.Join(xhrOrder("firefox"), ",")
	if chrome == firefox {
		t.Error("порядок заголовков XHR одинаков для разных браузеров")
	}
	if !strings.Contains(firefox, "x-requested-with") {
		t.Error("в firefox-порядке нет x-requested-with")
	}
}

func TestProxyHostPort(t *testing.T) {
	cases := map[string]string{
		"socks5://1.2.3.4:1080":         "1.2.3.4:1080",
		"http://1.2.3.4:8080":           "1.2.3.4:8080",
		"socks5://user:pass@1.2.3.4:90": "1.2.3.4:90",
		"1.2.3.4:3128":                  "1.2.3.4:3128",
		"":                              "",
	}
	for in, want := range cases {
		if got := proxyHostPort(in); got != want {
			t.Errorf("proxyHostPort(%q)=%q, ожидала %q", in, got, want)
		}
	}
}
