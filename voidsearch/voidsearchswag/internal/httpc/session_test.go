package httpc

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/klauspost/compress/zstd"

	"voidsearchswag/internal/netx"
)

// newTestSession поднимает настоящую сессию на отпечатке chrome_131_win.
// httptest отдаёт plain HTTP, TLS-рукопожатия нет, но построение клиента,
// заголовки, чтение тела и декодирование работают как в бою.
func newTestSession(t *testing.T) *Session {
	t.Helper()
	fp, ok := netx.FingerprintByName("chrome_131_win")
	if !ok {
		t.Fatal("отпечаток chrome_131_win не найден")
	}
	s, err := NewSession(fp, "", "direct", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func TestSessionAccessors(t *testing.T) {
	fp, _ := netx.FingerprintByName("chrome_131_win")
	s, err := NewSession(fp, "socks5://127.0.0.1:9050", "tor", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if s.Proxy() != "socks5://127.0.0.1:9050" {
		t.Errorf("Proxy=%q", s.Proxy())
	}
	if s.Kind() != "tor" {
		t.Errorf("Kind=%q", s.Kind())
	}
	if s.Engine() != "tor/chrome_131_win" {
		t.Errorf("Engine=%q", s.Engine())
	}
	if s.Fingerprint().Name != "chrome_131_win" {
		t.Errorf("Fingerprint=%q", s.Fingerprint().Name)
	}
	if s.Age() < 0 {
		t.Errorf("Age=%v", s.Age())
	}
}

func TestSessionDefaultTimeout(t *testing.T) {
	fp, _ := netx.FingerprintByName("chrome_131_win")
	// Неположительный таймаут обязан заменяться дефолтом: нулевой таймаут
	// у tls-client означает «ждать вечно» и вешает запрос.
	s, err := NewSession(fp, "", "direct", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.Kind() != "direct" {
		t.Errorf("Kind=%q", s.Kind())
	}
}

func TestSessionCloseNil(t *testing.T) {
	// Закрытие nil-сессии не должно паниковать: Close вызывается в defer
	// даже когда NewSession вернул ошибку.
	var s *Session
	s.Close()
}

func TestSessionDoPlainBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("метод %q", r.Method)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte("<html>привет</html>"))
	}))
	defer srv.Close()

	s := newTestSession(t)
	// История этого assertions и две опровергнутые гипотезы.
	//
	// Тест падал мгновенно (0.00s) в полном параллельном прогоне всех пакетов.
	// Первая гипотеза - «нагрузка машины против десятисекундного таймаута
	// сессии» - отброшена: мгновенное падение при таймауте в 10 секунд заняло бы
	// 10 секунд, а не ноль.
	//
	// Вторая гипотеза - «немедленный отказ соединения из-за исчерпания
	// динамических портов Windows» - тоже неверна, и это показал подробный
	// вывод: запрос проходит успешно, тело и заголовки верные, err равен nil.
	// Падало только утверждение о длительности.
	//
	// Измеренный факт: 1 падение на 12 прогонов пакета, в сообщении
	// Duration=0s. Do вычисляет Duration как time.Since(start) вокруг реального
	// круга по loopback, и код здесь корректен - start берётся до запроса,
	// time.Since после. Ноль означает, что интервал оказался короче гранулярности
	// часов, а не что запрос не выполнялся.
	//
	// Поэтому проверяется отсутствие отрицательной длительности, а не строго
	// положительная. Требовать Duration > 0 значило бы требовать от измерения
	// свойства, которого у него нет: sub-миллисекундный loopback при грубой
	// гранулярности часов законно даёт ноль. Отрицательное значение было бы
	// настоящим дефектом - оно означало бы, что start берётся после запроса.
	resp, err := s.Do(context.Background(), Request{URL: srv.URL + "/", Method: http.MethodGet})
	if err != nil {
		t.Fatalf("запрос к локальному серверу %s не удался: %v (тип %T)", srv.URL, err, err)
	}
	if !resp.OK() {
		t.Errorf("статус %d", resp.Status)
	}
	if string(resp.Body) != "<html>привет</html>" {
		t.Errorf("тело %q", resp.Body)
	}
	if resp.Header.Get("Content-Type") == "" {
		t.Error("заголовок Content-Type потерян при конвертации")
	}
	if resp.Duration < 0 {
		t.Errorf("Duration=%v отрицательна: start берётся после запроса, "+
			"а должен браться до", resp.Duration)
	}
	if resp.URL != srv.URL+"/" {
		t.Errorf("URL=%q", resp.URL)
	}
}

func TestSessionDoGzipEncoded(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write([]byte("<html>gzip через сессию</html>"))
	zw.Close()
	enc := buf.Bytes()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Сервер отдаёт сжатое тело и объявляет encoding: клиент обязан
		// распаковать его сам, иначе разбор HTML получит бинарный мусор.
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(enc)
	}))
	defer srv.Close()

	s := newTestSession(t)
	resp, err := s.Do(context.Background(), Request{URL: srv.URL + "/", Method: http.MethodGet})
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.Body) != "<html>gzip через сессию</html>" {
		t.Errorf("тело не распаковано: %q", resp.Body)
	}
}

func TestSessionDoDeflateEncoded(t *testing.T) {
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	zw.Write([]byte("<html>deflate через сессию</html>"))
	zw.Close()
	enc := buf.Bytes()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "deflate")
		w.Write(enc)
	}))
	defer srv.Close()

	s := newTestSession(t)
	resp, err := s.Do(context.Background(), Request{URL: srv.URL + "/", Method: http.MethodGet})
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.Body) != "<html>deflate через сессию</html>" {
		t.Errorf("тело не распаковано: %q", resp.Body)
	}
}

func TestSessionDoErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("не найдено"))
	}))
	defer srv.Close()

	s := newTestSession(t)
	resp, err := s.Do(context.Background(), Request{URL: srv.URL + "/нет", Method: http.MethodGet})
	if err != nil {
		t.Fatal(err)
	}
	if resp.OK() {
		t.Error("404 отмечен как успех")
	}
	if resp.Status != 404 {
		t.Errorf("статус %d", resp.Status)
	}
}

func TestSessionDoUnreachable(t *testing.T) {
	s := newTestSession(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := s.Do(ctx, Request{URL: "http://127.0.0.1:1/", Method: http.MethodGet}); err == nil {
		t.Error("недоступный адрес не вернул ошибку")
	}
}

func TestSessionDoBadURL(t *testing.T) {
	s := newTestSession(t)
	// URL без схемы: ошибка обязана вернуться из построения запроса, а не
	// паникой из tls-client.
	if _, err := s.Do(context.Background(), Request{URL: "://мусор", Method: http.MethodGet}); err == nil {
		t.Error("битый URL принят")
	}
}

func TestSessionGet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	s := newTestSession(t)
	resp, err := s.Get(context.Background(), srv.URL+"/")
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.Body) != "ok" {
		t.Errorf("тело %q", resp.Body)
	}
}

func TestSessionGetWithSetsRefererAndXHR(t *testing.T) {
	var gotReferer, gotXHR, gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotReferer = r.Header.Get("Referer")
		gotXHR = r.Header.Get("X-Requested-With")
		gotAccept = r.Header.Get("Accept")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	s := newTestSession(t)
	resp, err := s.GetWith(context.Background(), srv.URL+"/api", "http://example.test/page", true)
	if err != nil {
		t.Fatal(err)
	}
	if !resp.OK() {
		t.Errorf("статус %d", resp.Status)
	}
	if gotReferer != "http://example.test/page" {
		t.Errorf("Referer=%q", gotReferer)
	}
	if gotXHR != "XMLHttpRequest" {
		t.Errorf("X-Requested-With=%q", gotXHR)
	}
	if !strings.Contains(gotAccept, "application/json") {
		t.Errorf("Accept для XHR=%q", gotAccept)
	}
}

func TestSessionPost(t *testing.T) {
	var gotMethod, gotCT string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotCT = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.Write([]byte("принято"))
	}))
	defer srv.Close()

	s := newTestSession(t)
	resp, err := s.Post(context.Background(), srv.URL+"/submit", []byte("тело=1"), "application/x-www-form-urlencoded")
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("метод %q", gotMethod)
	}
	if gotCT != "application/x-www-form-urlencoded" {
		t.Errorf("Content-Type=%q", gotCT)
	}
	if string(gotBody) != "тело=1" {
		t.Errorf("тело запроса %q", gotBody)
	}
	if string(resp.Body) != "принято" {
		t.Errorf("тело ответа %q", resp.Body)
	}
}

func TestSessionPostWithoutContentType(t *testing.T) {
	var gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	s := newTestSession(t)
	if _, err := s.Post(context.Background(), srv.URL+"/", []byte("x"), ""); err != nil {
		t.Fatal(err)
	}
	// Пустой content-type не должен подставлять заголовок.
	if gotCT != "" {
		t.Errorf("Content-Type=%q при пустом значении", gotCT)
	}
}

func TestSessionDoStdPlain(t *testing.T) {
	var gotUA, gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		gotAccept = r.Header.Get("Accept")
		w.Write([]byte("std ok"))
	}))
	defer srv.Close()

	s := newTestSession(t)
	resp, err := s.DoStd(context.Background(), Request{URL: srv.URL + "/", Method: http.MethodGet})
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.Body) != "std ok" {
		t.Errorf("тело %q", resp.Body)
	}
	// DoStd обязан проставить тот же отпечаток, что и tls-путь: иначе
	// эскалация выдаёт себя голой Go-библиотекой.
	if !strings.Contains(gotUA, "Chrome/131") {
		t.Errorf("User-Agent=%q", gotUA)
	}
	if gotAccept == "" {
		t.Error("Accept не проставлен")
	}
}

func TestSessionDoStdDefaultMethod(t *testing.T) {
	var gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	s := newTestSession(t)
	if _, err := s.DoStd(context.Background(), Request{URL: srv.URL + "/"}); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("метод по умолчанию %q, ожидала GET", gotMethod)
	}
}

func TestSessionDoStdWithBodyAndHeaders(t *testing.T) {
	var gotBody []byte
	var gotCustom string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotCustom = r.Header.Get("X-Custom")
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	s := newTestSession(t)
	_, err := s.DoStd(context.Background(), Request{
		URL:     srv.URL + "/",
		Method:  http.MethodPost,
		Body:    []byte("payload"),
		Headers: map[string]string{"X-Custom": "значение"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(gotBody) != "payload" {
		t.Errorf("тело %q", gotBody)
	}
	if gotCustom != "значение" {
		t.Errorf("X-Custom=%q", gotCustom)
	}
}

func TestSessionDoStdBadURL(t *testing.T) {
	s := newTestSession(t)
	if _, err := s.DoStd(context.Background(), Request{URL: "://мусор"}); err == nil {
		t.Error("битый URL принят")
	}
}

func TestSessionDoStdGzipEncoded(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write([]byte("std gzip"))
	zw.Close()
	enc := buf.Bytes()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(enc)
	}))
	defer srv.Close()

	s := newTestSession(t)
	resp, err := s.DoStd(context.Background(), Request{URL: srv.URL + "/"})
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.Body) != "std gzip" {
		t.Errorf("тело не распаковано: %q", resp.Body)
	}
}

func TestSessionDoStdUnreachable(t *testing.T) {
	s := newTestSession(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := s.DoStd(ctx, Request{URL: "http://127.0.0.1:1/"}); err == nil {
		t.Error("недоступный адрес не вернул ошибку")
	}
}

func TestStdHeadersDocument(t *testing.T) {
	fp, _ := netx.FingerprintByName("chrome_131_win")
	h := stdHeaders(fp, "", false)

	if h.Get("Sec-Fetch-Dest") != "document" || h.Get("Sec-Fetch-Mode") != "navigate" {
		t.Errorf("sec-fetch: %q / %q", h.Get("Sec-Fetch-Dest"), h.Get("Sec-Fetch-Mode"))
	}
	// Без реферера сайт помечается как none: иначе антифрод видит переход
	// «из ниоткуда» с заголовком same-origin.
	if h.Get("Sec-Fetch-Site") != "none" {
		t.Errorf("Sec-Fetch-Site=%q", h.Get("Sec-Fetch-Site"))
	}
	if h.Get("Referer") != "" {
		t.Errorf("пустой Referer проставлен: %q", h.Get("Referer"))
	}
	if h.Get("User-Agent") != fp.UserAgent {
		t.Errorf("User-Agent=%q", h.Get("User-Agent"))
	}
	if h.Get("Accept-Encoding") != acceptEncoding {
		t.Errorf("Accept-Encoding=%q", h.Get("Accept-Encoding"))
	}
	if h.Get("Sec-Ch-Ua") != fp.SecCH {
		t.Errorf("Sec-Ch-Ua не проставлен для chrome: %q", h.Get("Sec-Ch-Ua"))
	}
}

func TestStdHeadersWithReferer(t *testing.T) {
	fp, _ := netx.FingerprintByName("chrome_131_win")
	h := stdHeaders(fp, "http://example.test/page", false)

	if h.Get("Referer") != "http://example.test/page" {
		t.Errorf("Referer=%q", h.Get("Referer"))
	}
	if h.Get("Sec-Fetch-Site") != "same-origin" {
		t.Errorf("Sec-Fetch-Site=%q", h.Get("Sec-Fetch-Site"))
	}
}

func TestStdHeadersXHR(t *testing.T) {
	fp, _ := netx.FingerprintByName("chrome_131_win")
	h := stdHeaders(fp, "http://example.test/", true)

	if h.Get("X-Requested-With") != "XMLHttpRequest" {
		t.Errorf("X-Requested-With=%q", h.Get("X-Requested-With"))
	}
	if !strings.Contains(h.Get("Accept"), "application/json") {
		t.Errorf("Accept=%q", h.Get("Accept"))
	}
	// XHR не должен нести навигационные заголовки: браузер их не шлёт.
	if h.Get("Sec-Fetch-Dest") != "" {
		t.Errorf("Sec-Fetch-Dest в XHR=%q", h.Get("Sec-Fetch-Dest"))
	}
	if h.Get("Upgrade-Insecure-Requests") != "" {
		t.Errorf("Upgrade-Insecure-Requests в XHR=%q", h.Get("Upgrade-Insecure-Requests"))
	}
}

func TestStdHeadersFirefoxSkipsClientHints(t *testing.T) {
	fp, _ := netx.FingerprintByName("firefox_133_win")
	h := stdHeaders(fp, "", false)
	// Firefox не шлёт Sec-Ch-Ua: заголовок выдаёт подделку отпечатка.
	if h.Get("Sec-Ch-Ua") != "" {
		t.Errorf("Sec-Ch-Ua проставлен для firefox: %q", h.Get("Sec-Ch-Ua"))
	}
	if h.Get("User-Agent") != fp.UserAgent {
		t.Errorf("User-Agent=%q", h.Get("User-Agent"))
	}
}

func TestBoolStr(t *testing.T) {
	if got := boolStr(true, "да", "нет"); got != "да" {
		t.Errorf("true -> %q", got)
	}
	if got := boolStr(false, "да", "нет"); got != "нет" {
		t.Errorf("false -> %q", got)
	}
}

func TestToStdHeaderSkipsOrderKeys(t *testing.T) {
	// fhttp добавляет служебные ключи порядка заголовков: в стандартный
	// http.Header их переносить нельзя, иначе они уедут на сервер.
	in := fhttp.Header{}
	in["Accept"] = []string{"text/html"}
	in[fhttp.HeaderOrderKey] = []string{"accept", "user-agent"}
	in[fhttp.PHeaderOrderKey] = []string{":method", ":path"}

	out := toStdHeader(in)
	if out.Get("Accept") != "text/html" {
		t.Errorf("Accept=%q", out.Get("Accept"))
	}
	if len(out) != 1 {
		t.Errorf("служебные ключи перенесены: %v", out)
	}
}

func TestToStdHeaderEmpty(t *testing.T) {
	if got := toStdHeader(fhttp.Header{}); len(got) != 0 {
		t.Errorf("пустой заголовок дал %v", got)
	}
}

// brotliEncode и zstdEncode сжимают тело так, как это делает сервер.
func brotliEncode(t *testing.T, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	bw := brotli.NewWriter(&buf)
	if _, err := bw.Write(body); err != nil {
		t.Fatal(err)
	}
	bw.Close()
	return buf.Bytes()
}

func zstdEncode(t *testing.T, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zw.Write(body); err != nil {
		t.Fatal(err)
	}
	zw.Close()
	return buf.Bytes()
}

// newTestPool собирает пул прокси без проверки живости и без сети.
func newTestPool(t *testing.T) *netx.ProxyPool {
	t.Helper()
	pool, err := netx.NewProxyPool(context.Background(), netx.PoolConfig{
		Provider:   netx.StaticProvider{"1.2.3.4:1080"},
		Verify:     false,
		MaxLive:    2,
		MinLive:    1,
		RefillWait: time.Hour,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return pool
}

func TestReadRawPlain(t *testing.T) {
	got, err := readRaw(strings.NewReader("простой текст"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "простой текст" {
		t.Errorf("тело %q", got)
	}
}

func TestReadRawGzipByMagic(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write([]byte("gzip без заголовка"))
	zw.Close()

	got, err := readRaw(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "gzip без заголовка" {
		t.Errorf("магические байты не распознаны: %q", got)
	}
}

func TestReadRawZlibByMagic(t *testing.T) {
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	zw.Write([]byte("zlib без заголовка"))
	zw.Close()

	got, err := readRaw(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "zlib без заголовка" {
		t.Errorf("тело %q", got)
	}
}

func TestReadRawEmpty(t *testing.T) {
	got, err := readRaw(strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("пустой вход дал %d байт", len(got))
	}
}

func TestReadRawEncHonoursHeader(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write([]byte("по заголовку"))
	zw.Close()

	for _, enc := range []string{"gzip", "GZIP", " x-gzip "} {
		got, err := readRawEnc(bytes.NewReader(buf.Bytes()), enc)
		if err != nil {
			t.Fatalf("%q: %v", enc, err)
		}
		if string(got) != "по заголовку" {
			t.Errorf("%q: тело %q", enc, got)
		}
	}
}

func TestReadRawEncUnknownFallsBack(t *testing.T) {
	// Неизвестный encoding не должен ронять ответ: тело отдаётся как есть,
	// иначе одна экзотическая кодировка ломает весь поиск.
	got, err := readRawEnc(strings.NewReader("обычный текст"), "brotli-x-неведомый")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "обычный текст" {
		t.Errorf("тело %q", got)
	}
}

func TestDecodeWithAllEncodings(t *testing.T) {
	plain := []byte("одно и то же тело")

	var gz bytes.Buffer
	gzw := gzip.NewWriter(&gz)
	gzw.Write(plain)
	gzw.Close()

	var zl bytes.Buffer
	zlw := zlib.NewWriter(&zl)
	zlw.Write(plain)
	zlw.Close()

	var fl bytes.Buffer
	flw, _ := flate.NewWriter(&fl, flate.DefaultCompression)
	flw.Write(plain)
	flw.Close()

	br := brotliEncode(t, plain)
	zs := zstdEncode(t, plain)

	cases := []struct {
		enc string
		raw []byte
	}{
		{"gzip", gz.Bytes()},
		{"x-gzip", gz.Bytes()},
		{"deflate", zl.Bytes()},
		{"deflate", fl.Bytes()},
		{"br", br},
		{"zstd", zs},
		{"", plain},
		{"identity", plain},
	}
	for _, c := range cases {
		got := DecodeWith(c.raw, c.enc)
		if string(got) != string(plain) {
			t.Errorf("encoding %q: тело %q", c.enc, got)
		}
	}
}

func TestDecodeWithBrokenPayloadReturnsRaw(t *testing.T) {
	// Битый gzip не должен ни падать, ни отдавать пустоту: вызывающий
	// получает исходные байты и сам решает, что с ними делать.
	broken := []byte{0x1f, 0x8b, 0x08, 0x00, 0xff, 0xff}
	got := DecodeWith(broken, "gzip")
	if len(got) == 0 {
		t.Error("битый payload дал пустой результат")
	}
}

func TestInflatePrefersZlibThenRawDeflate(t *testing.T) {
	plain := []byte("тело для inflate")

	var zl bytes.Buffer
	zlw := zlib.NewWriter(&zl)
	zlw.Write(plain)
	zlw.Close()
	if got := inflate(zl.Bytes()); string(got) != string(plain) {
		t.Errorf("zlib-ветка: %q", got)
	}

	var fl bytes.Buffer
	flw, _ := flate.NewWriter(&fl, flate.DefaultCompression)
	flw.Write(plain)
	flw.Close()
	if got := inflate(fl.Bytes()); string(got) != string(plain) {
		t.Errorf("raw deflate ветка: %q", got)
	}

	if got := inflate([]byte("это не сжатые данные")); got != nil {
		t.Errorf("мусор дал %q", got)
	}
}

func TestTryZlibRejectsGarbage(t *testing.T) {
	if got := tryZlib([]byte("мусор без заголовка zlib")); got != nil {
		t.Errorf("мусор принят: %q", got)
	}
	// Обрезанный zlib-поток: заголовок валиден, данных нет.
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	zw.Write([]byte("длинное тело которое обрежут"))
	zw.Close()
	full := buf.Bytes()
	if got := tryZlib(full[:3]); got != nil {
		t.Errorf("обрезанный поток дал %q", got)
	}
}

func TestClientPoolAccessor(t *testing.T) {
	c, err := NewClient(context.Background(), Options{Transport: "direct"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Pool() != nil {
		t.Error("пул появился без транспорта pool")
	}
	// direct намеренно без ротатора: ensure() берёт spec из rot, а nil
	// означает прямое соединение. Лишний слой здесь не нужен.
	if c.Rotator() != nil {
		t.Error("ротатор создан для direct")
	}
}

func TestClientPoolAccessorWithPool(t *testing.T) {
	pool := newTestPool(t)
	c, err := NewClient(context.Background(), Options{
		Transport:    "pool",
		Pool:         pool,
		Fingerprints: []string{"chrome_131_win"},
		SessionTTL:   time.Minute,
		Timeout:      5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Pool() != pool {
		t.Error("Pool() вернул чужой пул")
	}
}

func TestClientGetStringAgainstStub(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("строка ответа"))
	}))
	defer srv.Close()

	c, err := NewClient(context.Background(), Options{Transport: "direct", Fingerprints: []string{"chrome_131_win"}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	got, err := c.GetString(context.Background(), srv.URL+"/")
	if err != nil {
		t.Fatal(err)
	}
	if got != "строка ответа" {
		t.Errorf("строка %q", got)
	}
}

func TestClientGetStringBlocked(t *testing.T) {
	// Защита отвечает 403 с текстом капчи: GetString обязан вернуть ошибку,
	// а не пустую строку, иначе вызывающий примет заглушку за результат.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte("please solve the captcha to continue"))
	}))
	defer srv.Close()

	c, err := NewClient(context.Background(), Options{Transport: "direct", Fingerprints: []string{"chrome_131_win"}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if _, err := c.GetString(context.Background(), srv.URL+"/"); err == nil {
		t.Error("заблокированный ответ принят как текст")
	} else if !IsBlocked(err) {
		t.Errorf("ошибка не помечена как блокировка: %v", err)
	}
}

func TestClientFetchEscalatesOnProtection(t *testing.T) {
	// Первый ответ - заглушка защиты, второй (после эскалации) - нормальный.
	// Без эскалации поиск теряет страницы, которые отдают challenge на
	// первом запросе и работают на повторном.
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte("access denied by edge"))
			return
		}
		w.Write([]byte("<html>настоящий контент</html>"))
	}))
	defer srv.Close()

	c, err := NewClient(context.Background(), Options{Transport: "direct", Fingerprints: []string{"chrome_131_win"}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	resp, err := c.Fetch(context.Background(), Request{URL: srv.URL + "/", Method: http.MethodGet})
	if err != nil {
		t.Fatal(err)
	}
	if calls < 2 {
		t.Errorf("эскалации не было: запросов %d", calls)
	}
	if !strings.Contains(resp.Text(), "настоящий контент") {
		t.Errorf("тело после эскалации %q", resp.Text())
	}
}

func TestClientFetchReturnsStubWhenEscalationFails(t *testing.T) {
	// Сервер всегда отдаёт защиту: эскалация не помогает, и Fetch обязан
	// вернуть исходный ответ без ошибки - вызывающий сам его классифицирует.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte("access denied"))
	}))
	defer srv.Close()

	c, err := NewClient(context.Background(), Options{Transport: "direct", Fingerprints: []string{"chrome_131_win"}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	resp, err := c.Fetch(context.Background(), Request{URL: srv.URL + "/", Method: http.MethodGet})
	if err != nil {
		t.Fatalf("Fetch вернул ошибку на заглушке: %v", err)
	}
	if resp == nil {
		t.Fatal("ответ nil")
	}
	if err := Classify(resp); err == nil {
		t.Error("заглушка не классифицируется как блокировка")
	}
}

func TestClientGetWithDelegates(t *testing.T) {
	var gotReferer, gotXHR string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotReferer = r.Header.Get("Referer")
		gotXHR = r.Header.Get("X-Requested-With")
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	c, err := NewClient(context.Background(), Options{Transport: "direct", Fingerprints: []string{"chrome_131_win"}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	resp, err := c.GetWith(context.Background(), srv.URL+"/api", "http://example.test/", true)
	if err != nil {
		t.Fatal(err)
	}
	if !resp.OK() {
		t.Errorf("статус %d", resp.Status)
	}
	if gotReferer != "http://example.test/" {
		t.Errorf("Referer=%q", gotReferer)
	}
	if gotXHR != "XMLHttpRequest" {
		t.Errorf("X-Requested-With=%q", gotXHR)
	}
}

func TestClientEscalateAgainstStub(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("после эскалации"))
	}))
	defer srv.Close()

	c, err := NewClient(context.Background(), Options{Transport: "direct", Fingerprints: []string{"chrome_131_win"}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	resp, err := c.Escalate(context.Background(), Request{URL: srv.URL + "/", Method: http.MethodGet})
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.Body) != "после эскалации" {
		t.Errorf("тело %q", resp.Body)
	}
}

func TestClientEscalateServerError(t *testing.T) {
	// 5xx не считается блокировкой, но Escalate обязан вернуть ответ с
	// кодом, а не ошибку: вызывающий различает их по resp.Status.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte("плохой шлюз"))
	}))
	defer srv.Close()

	c, err := NewClient(context.Background(), Options{Transport: "direct", Fingerprints: []string{"chrome_131_win"}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	resp, err := c.Escalate(context.Background(), Request{URL: srv.URL + "/", Method: http.MethodGet})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != http.StatusBadGateway {
		t.Errorf("статус %d", resp.Status)
	}
}

func TestClientEscalateUnreachable(t *testing.T) {
	c, err := NewClient(context.Background(), Options{Transport: "direct", Fingerprints: []string{"chrome_131_win"}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := c.Escalate(ctx, Request{URL: "http://127.0.0.1:1/", Method: http.MethodGet}); err == nil {
		t.Error("недоступный адрес не вернул ошибку")
	}
}

func TestClientSessionReusedWithinTTL(t *testing.T) {
	// Сессия обязана переиспользоваться: пересоздание на каждый запрос
	// означает новое TLS-рукопожатие и новый отпечаток, что выглядит как
	// бот и убивает живучесть.
	c, err := NewClient(context.Background(), Options{
		Transport: "direct", Fingerprints: []string{"chrome_131_win"}, SessionTTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	s1, err := c.ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s2, err := c.ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s1 != s2 {
		t.Error("сессия пересоздана внутри TTL")
	}
}

func TestClientSessionRecreatedAfterTTL(t *testing.T) {
	c, err := NewClient(context.Background(), Options{
		Transport: "direct", Fingerprints: []string{"chrome_131_win"}, SessionTTL: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	s1, err := c.ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	s2, err := c.ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s1 == s2 {
		t.Error("просроченная сессия не заменена")
	}
}

func TestClientSessionRecreatedWhenUnhealthy(t *testing.T) {
	f := &fakeRotator{kind: "tor", spec: "", healthy: true}
	c, err := NewClient(context.Background(), Options{
		Rotator: f, Fingerprints: []string{"chrome_131_win"}, SessionTTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	s1, err := c.ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Транспорт потерял здоровье: сессия обязана пересоздаться, иначе запрос
	// уйдёт через мёртвый канал и упадёт по таймауту.
	f.healthy = false
	s2, err := c.ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s1 == s2 {
		t.Error("сессия не пересоздана при нездоровом ротаторе")
	}
}

func TestStdClientPicksByScheme(t *testing.T) {
	// Ядро исправления: для http:// обязан выбираться plain-клиент, иначе
	// impersonate форсит TLS-рукопожатие и эскалация падает на каждом
	// onion-адресе. Для https:// остаётся impersonate.
	s := newTestSession(t)

	httpURL, _ := url.Parse("http://abcdefghijklmnop.onion/")
	if got := s.stdClient(httpURL); got != s.plain {
		t.Error("для http:// не выбран plain-клиент")
	}

	httpsURL, _ := url.Parse("https://example.test/")
	if got := s.stdClient(httpsURL); got != s.imp {
		t.Error("для https:// не выбран impersonate-клиент")
	}

	// Регистр схемы не должен влиять: HTTP:// встречается в дрянных
	// объявлениях onion-ссылок.
	upper, _ := url.Parse("HTTP://abcdefghijklmnop.onion/")
	if got := s.stdClient(upper); got != s.plain {
		t.Error("HTTP:// в верхнем регистре не выбрал plain-клиент")
	}

	// nil-URL (случай битого адреса) не должен паниковать.
	if got := s.stdClient(nil); got != s.imp {
		t.Error("nil-URL не откатился к impersonate")
	}
}

func TestStdClientPlainAlwaysSet(t *testing.T) {
	// plain обязан быть создан всегда, даже без прокси: иначе stdClient
	// вернёт nil на http:// и DoStd запаникует.
	s := newTestSession(t)
	if s.plain == nil {
		t.Fatal("plain-клиент не создан")
	}
}

func TestNewPlainClientRoutesProxy(t *testing.T) {
	// Прокси обязан попасть в plain-клиент: без него эскалация по http://
	// уйдёт напрямую, минуя tor exit, и выдаст реальный IP.
	for _, spec := range []string{
		"socks5://127.0.0.1:9050",
		"http://127.0.0.1:8080",
		"127.0.0.1:1080",
	} {
		c := newPlainClient(spec, 5*time.Second)
		tr, ok := c.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("%q: транспорт %T", spec, c.Transport)
		}
		if tr.Proxy == nil {
			t.Errorf("%q: прокси не подставлен", spec)
			continue
		}
		got, err := tr.Proxy(&http.Request{URL: &url.URL{Scheme: "http", Host: "x.onion"}})
		if err != nil {
			t.Errorf("%q: %v", spec, err)
			continue
		}
		if got == nil || got.Host != "127.0.0.1:"+portOf(spec) {
			t.Errorf("%q: прокси-URL %+v", spec, got)
		}
	}
}

func TestNewPlainClientDirectWithoutProxy(t *testing.T) {
	c := newPlainClient("", 5*time.Second)
	tr := c.Transport.(*http.Transport)
	if tr.Proxy != nil {
		t.Error("без прокси Proxy всё равно подставлен")
	}
	if c.Timeout != 5*time.Second {
		t.Errorf("таймаут %v", c.Timeout)
	}
}

// portOf достаёт порт из spec, дополняя его значением по умолчанию, когда
// порт не задан: 1080 для socks, 8080 для http.
func portOf(spec string) string {
	if i := strings.LastIndex(spec, ":"); i >= 0 {
		return spec[i+1:]
	}
	return ""
}
