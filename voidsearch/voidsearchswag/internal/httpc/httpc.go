package httpc

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	impersonate "github.com/North-web-dev/impersonate-http"
	"github.com/andybalholm/brotli"
	fhttp "github.com/bogdanfinn/fhttp"
	tlsclient "github.com/bogdanfinn/tls-client"
	"github.com/klauspost/compress/zstd"

	"voidsearchswag/internal/netx"
)

const (
	acceptEncoding = "gzip, deflate, br, zstd"
	MaxBody        = 12 << 20
)

var (
	ErrBlocked = errors.New("доступ заблокирован защитой")
	ErrStatus  = errors.New("неожиданный статус")
)

type BlockKind int

const (
	BlockOther BlockKind = iota
	BlockIPBan
	BlockEdge
	BlockRate
	BlockAuth
	BlockCaptcha
)

func (k BlockKind) String() string {
	switch k {
	case BlockIPBan:
		return "ip-range ban"
	case BlockEdge:
		return "edge deny"
	case BlockRate:
		return "rate limit"
	case BlockAuth:
		return "login required"
	case BlockCaptcha:
		return "captcha"
	default:
		return "blocked"
	}
}

type BlockedError struct {
	Kind   BlockKind
	Status int
	Detail string
}

func (e *BlockedError) Error() string {
	if e.Detail != "" {
		return "http: " + e.Kind.String() + " (" + e.Detail + ")"
	}
	return "http: " + e.Kind.String() + " (status " + strconv.Itoa(e.Status) + ")"
}

func (e *BlockedError) Unwrap() error { return ErrBlocked }

func IsBlocked(err error) bool { return errors.Is(err, ErrBlocked) }

type Response struct {
	Status   int
	Proto    string
	Header   http.Header
	Body     []byte
	URL      string
	Duration time.Duration
	Engine   string
}

func (r *Response) Text() string { return string(r.Body) }

func (r *Response) OK() bool { return r.Status >= 200 && r.Status < 300 }

type Session struct {
	tls   tlsclient.HttpClient
	imp   *http.Client
	plain *http.Client
	fp    netx.Fingerprint
	proxy string
	kind  string
	// timeout - потолок одного запроса. Он передан транспортам, но
	// socks-рукопожатие их не слушает, поэтому тот же срок продублирован в
	// контексте запроса: см. sessionContext.
	timeout time.Duration
	mu      sync.RWMutex
	built   time.Time
}

func NewSession(fp netx.Fingerprint, proxy, kind string, timeout time.Duration) (*Session, error) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	tlsProfile, ok := fp.TLSProfile()
	if !ok {
		return nil, fmt.Errorf("нет TLS-профиля %q в tls-client", fp.TLSKey)
	}
	idle := 90 * time.Second
	opts := []tlsclient.HttpClientOption{
		tlsclient.WithClientProfile(tlsProfile),
		tlsclient.WithTimeoutSeconds(int(timeout.Seconds()) + 1),
		tlsclient.WithCookieJar(tlsclient.NewCookieJar()),
		tlsclient.WithDisableHttp3(),
		tlsclient.WithCatchPanics(),
		tlsclient.WithTransportOptions(&tlsclient.TransportOptions{
			DisableCompression:  true,
			MaxIdleConnsPerHost: 4,
			IdleConnTimeout:     &idle,
		}),
	}
	if proxy != "" {
		opts = append(opts, tlsclient.WithProxyUrl(proxy))
	}
	tlsC, err := tlsclient.NewHttpClient(tlsclient.NewNoopLogger(), opts...)
	if err != nil {
		return nil, fmt.Errorf("tls-client (%s): %w", fp.Name, err)
	}

	var impC *http.Client
	if proxy != "" {
		dial, derr := impersonate.ProxyDialer(proxy)
		if derr != nil {
			tlsC.CloseIdleConnections()
			return nil, fmt.Errorf("impersonate dialer (%s): %w", proxy, derr)
		}
		impC = impersonate.New(fp.Profile, impersonate.WithDialer(dial), impersonate.WithTimeout(timeout))
	} else {
		impC = impersonate.New(fp.Profile, impersonate.WithTimeout(timeout))
	}

	return &Session{
		tls:     tlsC,
		imp:     impC,
		plain:   newPlainClient(proxy, timeout),
		fp:      fp,
		proxy:   proxy,
		kind:    kind,
		timeout: timeout,
		built:   time.Now(),
	}, nil
}

// newPlainClient собирает обычный net/http клиент для схемы http://.
//
// Зачем он нужен: impersonate выполняет TLS-рукопожатие для ЛЮБОЙ схемы,
// включая http://. На plain-HTTP адресе это падает с ошибкой «tls: first
// record does not look like a TLS handshake». Почти все onion-сайты
// отдают именно http://, поэтому путь эскалации (DoStd) был мёртв на них
// целиком: вместо повторной попытки после защиты вызывающий получал ошибку
// транспорта. Здесь заголовки и отпечаток User-Agent сохраняются - их
// проставляет stdHeaders, - а TLS не форсится.
func newPlainClient(proxy string, timeout time.Duration) *http.Client {
	tr := &http.Transport{
		DisableCompression:  true,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     90 * time.Second,
	}
	if proxy != "" {
		// Прокси обязан работать и на http:// адресах: иначе эскалация
		// уходит в прямой интернет, минуя tor exit, и выдаёт реальный IP.
		// Схема сохраняется как есть - net/http сам умеет socks5 и socks5h.
		if u, err := url.Parse(proxy); err == nil && u.Host != "" && u.Scheme != "" {
			tr.Proxy = http.ProxyURL(u)
		} else if host := ProxyHostPort(proxy); host != "" {
			// Адрес без схемы нормализуется в socks5 - так же, как это
			// делает netx: один spec обязан вести себя одинаково в обоих
			// клиентах, иначе tls-путь и plain-путь уйдут разными выходами.
			tr.Proxy = http.ProxyURL(&url.URL{Scheme: "socks5", Host: host})
		}
	}
	return &http.Client{Transport: tr, Timeout: timeout}
}

func (s *Session) Fingerprint() netx.Fingerprint { return s.fp }
func (s *Session) Proxy() string                 { return s.proxy }
func (s *Session) Kind() string                  { return s.kind }
func (s *Session) Engine() string                { return s.kind + "/" + s.fp.Name }
func (s *Session) Age() time.Duration            { return time.Since(s.built) }

func (s *Session) Close() {
	if s == nil {
		return
	}
	// Закрытие идёт в отдельной горутине. CloseIdleConnections берёт мьютекс
	// транспорта, а тот может быть занят socks-рукопожатием, которое не слушает
	// контекст и висит до собственного таймаута: синхронное закрытие в таком
	// состоянии блокировало вызывающего навсегда. Живой замер ДО: hunt watch
	// --timeout 5s не выходил, потому что эскалация закрывала сессию и вставала
	// на этом мьютексе.
	go func() {
		if s.tls != nil {
			s.tls.CloseIdleConnections()
		}
		if s.imp != nil {
			s.imp.CloseIdleConnections()
		}
		if s.plain != nil {
			s.plain.CloseIdleConnections()
		}
	}()
}

type Request struct {
	URL     string
	Method  string
	Referer string
	XHR     bool
	Headers map[string]string
	Body    []byte
}

// ErrOnionWithoutTor возвращается при попытке запросить .onion без tor и без
// прокси. Зону .onion знает только резолвер tor, поэтому прямой запрос не
// может succeed в принципе: имя уходит DNS-серверу провайдера (утечка - со
// стороны видно, что ищут именно onion-адрес) и возвращается NXDOMAIN, а
// вызывающий получает невнятное «dial tcp: lookup <адрес>».
//
// Барьер стоит на уровне сессии, а не в командах: через Do и DoStd ходят
// поиск, классификатор, обход и сборщик, и защищать каждый по отдельности
// значит однажды забыть про новый.
var ErrOnionWithoutTor = errors.New("onion-адрес недостижим напрямую: нужен tor или прокси")

// onionBlocked проверяет, что onion-цель не пытаются запросить мимо tor.
// Любой непустой proxy-spec считается подходящим: tor, socks-прокси из пула и
// статический прокси одинаково умеют резолвить .onion, если за ними tor.
func onionBlocked(rawURL, proxy string) error {
	if strings.TrimSpace(proxy) != "" {
		return nil
	}
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u == nil {
		return nil
	}
	host := strings.ToLower(u.Hostname())
	if strings.HasSuffix(host, ".onion") {
		return fmt.Errorf("%w: %s", ErrOnionWithoutTor, host)
	}
	return nil
}

// respectCancel возвращает управление вызывающему по отмене контекста, даже
// если транспорт отмену игнорирует.
//
// Замер ДО: запрос через молчащий socks-прокси (silentProxy в тесте) висел
// дольше 20 секунд при отмене контекста через 300 миллисекунд. Стек tls-client
// выполняет socks-рукопожатие собственным диалером: контекст из запроса он не
// смотрит, а WithTimeoutSeconds на рукопожатие не действует. Отсюда рос весь
// перерасход заявленных сроков: hunt watch --timeout 5s длился 12147 мс и вернул
// elapsed=8.935s, одиночный search - 17184 мс при REQUEST_TIMEOUT=12s.
//
// Горутина запроса не убивается - убивать горутины в Go нечем, - но вызывающий
// уходит сразу. Результат, который придёт позже, выбрасывается: канал
// буферизован, поэтому горутина не остаётся заблокированной навечно. Тело
// ответа к этому моменту уже прочитано и закрыто внутри do/doStd, так что
// соединение держит только транспорт, и закроет он его по своему таймауту.
func respectCancel(ctx context.Context, run func() (*Response, error)) (*Response, error) {
	type result struct {
		resp *Response
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		resp, err := run()
		ch <- result{resp, err}
	}()
	select {
	case res := <-ch:
		return cancelResult(ctx, res.resp, res.err)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// cancelGrace - срок, в течение которого ошибка транспорта считается обрывом по
// дедлайну. Выбрана по замеру: расхождение между отказом сокса и истечением срока
// измерялось десятыми долями миллисекунды, пять миллисекунд покрывают его с
// запасом и не успевают стать заметной задержкой для вызывающего.
const cancelGrace = 5 * time.Millisecond

// cancelTries - сколько коротких окон ожидания допускается, пока таймер контекста
// не сработает. Четыре окна по cancelGrace дают потолок в двадцать миллисекунд:
// этого хватает планировщику под нагрузкой и не становится заметной задержкой для
// вызывающего, который и так уже получил отказ.
const cancelTries = 4

// cancelResult решает, что отдать вызывающему, когда транспорт ответил в тот же
// срок, в который истёк контекст.
//
// Подменяется только ошибка и только на истёкшем или вот-вот истекающем контексте:
// успешный ответ остаётся у вызывающего, а живой контекст с далёким дедлайном не
// трогает настоящую причину отказа.
//
// Обе ветки select обязаны расходиться одинаково, иначе исход зависел бы от
// порядка готовности каналов: transport умеет вернуть свою ошибку одновременно с
// дедлайном, select выбирает готовый канал произвольно, и до выноса этого правила в
// отдельную функцию вызывающий получал то context.DeadlineExceeded, то «socks
// connect tcp ... i/o timeout». Полный набор тестов поймал второй исход на живом
// транспорте в TestDoStdReturnsOnContextCancel, а в одиночном прогоне тот же тест
// трижды подряд прошёл.
//
// Последствие не косметическое: internal/hunt/hunt.go отличает служебный текст
// пакета context от настоящей причины отказа по подстроке, и сетевая ошибка вместо
// контекстной уходила в LastError охоты как поломка источника.
//
// Подмена касается и случая, когда дедлайн уже наступил или наступает вот-вот.
// Сокс-рукопожатие tls-client обрывается по сроку контекста, но оформляет обрыв как
// сетевую ошибку сокета. Замер на HEAD f5fca08, молчащий прокси, дедлайн 50ms,
// двести повторов doStd без обёртки: все двести раз вернулась сетевая ошибка, и к
// моменту возврата дедлайн уже истёк - запас составил от минус шестидесяти
// микросекунд до минус тринадцати с половиной миллисекунд. При этом ctx.Err умеет
// оставаться пустым ещё сотни микросекунд после дедлайна, пока не сработает
// собственный таймер контекста: замер показал пустой ctx.Err спустя 353.6
// микросекунды после срока.
//
// Отсюда два исхода в respectCancel: select выбирает готовый канал произвольно, и
// ветка результата отдавала сетевую ошибку вместо контекстной. Полный набор тестов
// падал из-за этого примерно в одном прогоне из двадцати, а в одиночном прогоне тот
// же тест проходил.
//
// Ждать дольше cancelGrace за один подход смысла нет, но и одной попытки мало: под
// нагрузкой планировщик задерживает и таймер контекста, и select. Замер на HEAD
// 49e5da6 с параллельно идущим набором cmd/voidsearchswag дал три падения из десяти
// прогонов TestDoStdReturnsOnContextCancel по двадцать повторов в каждом, и в
// одиночном прогоне те же триста повторов не дали ни одного. Поэтому окон ожидания
// несколько, и каждое короткое.
func cancelResult(ctx context.Context, resp *Response, err error) (*Response, error) {
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return nil, cerr
		}
		if dl, ok := ctx.Deadline(); ok && time.Until(dl) <= cancelGrace {
			for attempt := 0; attempt < cancelTries; attempt++ {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(cancelGrace):
					if cerr := ctx.Err(); cerr != nil {
						return nil, cerr
					}
				}
			}
		}
	}
	return resp, err
}

func (s *Session) Do(ctx context.Context, r Request) (*Response, error) {
	ctx, cancel := sessionContext(ctx, s.timeout)
	defer cancel()
	return respectCancel(ctx, func() (*Response, error) { return s.do(ctx, r) })
}

func (s *Session) do(ctx context.Context, r Request) (*Response, error) {
	if err := onionBlocked(r.URL, s.proxy); err != nil {
		return nil, err
	}
	req, err := buildFHTTP(ctx, s.fp, r)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	resp, err := s.tls.Do(req)
	if err != nil {
		return nil, err
	}
	body, err := readRawEnc(resp.Body, resp.Header.Get("content-encoding"))
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	return &Response{
		Status:   resp.StatusCode,
		Proto:    resp.Proto,
		Header:   toStdHeader(resp.Header),
		Body:     body,
		URL:      r.URL,
		Duration: time.Since(start),
	}, nil
}

func (s *Session) Get(ctx context.Context, rawURL string) (*Response, error) {
	return s.Do(ctx, Request{URL: rawURL, Method: http.MethodGet})
}

func (s *Session) GetWith(ctx context.Context, rawURL, referer string, xhr bool) (*Response, error) {
	return s.Do(ctx, Request{URL: rawURL, Method: http.MethodGet, Referer: referer, XHR: xhr})
}

func (s *Session) Post(ctx context.Context, rawURL string, body []byte, contentType string) (*Response, error) {
	headers := map[string]string{}
	if contentType != "" {
		headers["content-type"] = contentType
	}
	return s.Do(ctx, Request{URL: rawURL, Method: http.MethodPost, Headers: headers, Body: body})
}

func (s *Session) DoStd(ctx context.Context, r Request) (*Response, error) {
	ctx, cancel := sessionContext(ctx, s.timeout)
	defer cancel()
	return respectCancel(ctx, func() (*Response, error) { return s.doStd(ctx, r) })
}

// sessionContext сужает срок запроса до таймаута сессии.
//
// Таймаут передаётся транспортам - tlsclient.WithTimeoutSeconds,
// impersonate.WithTimeout, http.Client.Timeout, - но на socks-рукопожатии он не
// работает: прокси, который принял соединение и молчит, держит запрос до срока
// внешнего контекста. Замер ДО на HEAD a73f53e: сессия с таймаутом 1s против
// молчащего слушателя на 127.0.0.1 держала запрос полные 20s, весь срок
// контекста, и вернула context deadline exceeded только по его истечении. В
// живом прогоне то же самое стоило трёх минут: search с
// VOIDSEARCH_REQUEST_TIMEOUT=3s и VOIDSEARCH_PROXIES=socks5://127.0.0.1:18999
// длился 3m0s, 3m0s и 3m0s в трёх повторениях, упёршись в потолок команды из
// main.go, и вернул отчёт с engines:null и duration 3m0s - оператор не увидел ни
// одной причины отказа.
//
// Срок вызывающего не удлиняется: если его дедлайн ближе, остаётся он.
func sessionContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return ctx, func() {}
	}
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) <= timeout {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}

func (s *Session) doStd(ctx context.Context, r Request) (*Response, error) {
	if err := onionBlocked(r.URL, s.proxy); err != nil {
		return nil, err
	}
	method := r.Method
	if method == "" {
		method = http.MethodGet
	}
	var body io.Reader
	if len(r.Body) > 0 {
		body = bytes.NewReader(r.Body)
	}
	req, err := http.NewRequestWithContext(ctx, method, r.URL, body)
	if err != nil {
		return nil, err
	}
	req.Header = stdHeaders(s.fp, r.Referer, r.XHR)
	for k, v := range r.Headers {
		req.Header.Set(k, v)
	}
	start := time.Now()
	resp, err := s.stdClient(req.URL).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := readRawEnc(resp.Body, resp.Header.Get("Content-Encoding"))
	if err != nil {
		return nil, err
	}
	return &Response{
		Status: resp.StatusCode, Proto: resp.Proto, Header: resp.Header,
		Body: raw, URL: r.URL, Duration: time.Since(start),
	}, nil
}

// stdClient выбирает клиент под схему адреса. impersonate выполняет
// TLS-рукопожатие даже для http://, поэтому на plain-HTTP адресе он
// падает с «tls: first record does not look like a TLS handshake».
// Для http:// берётся обычный клиент: заголовки отпечатка всё равно
// проставлены в stdHeaders, а TLS здесь физически нет.
func (s *Session) stdClient(u *url.URL) *http.Client {
	if u != nil && strings.EqualFold(u.Scheme, "http") && s.plain != nil {
		return s.plain
	}
	return s.imp
}

func buildFHTTP(ctx context.Context, fp netx.Fingerprint, r Request) (*fhttp.Request, error) {
	method := r.Method
	if method == "" {
		method = http.MethodGet
	}
	var body io.Reader
	if len(r.Body) > 0 {
		body = bytes.NewReader(r.Body)
	}
	req, err := fhttp.NewRequestWithContext(ctx, method, r.URL, body)
	if err != nil {
		return nil, err
	}
	h := fhttp.Header{}
	set := func(k, v string) {
		if v != "" {
			h.Set(k, v)
		}
	}
	if r.XHR {
		h["accept"] = []string{"application/json, text/javascript, */*; q=0.01"}
		h["x-requested-with"] = []string{"XMLHttpRequest"}
		set("referer", r.Referer)
	} else {
		set("accept", fp.Accept)
		set("upgrade-insecure-requests", "1")
		set("sec-fetch-dest", "document")
		set("sec-fetch-mode", "navigate")
		set("sec-fetch-user", "?1")
		if r.Referer != "" {
			set("referer", r.Referer)
			h["sec-fetch-site"] = []string{"same-origin"}
		} else {
			h["sec-fetch-site"] = []string{"none"}
		}
	}
	if fp.Kind == "chrome" || fp.Kind == "edge" {
		set("sec-ch-ua", fp.SecCH)
		set("sec-ch-ua-mobile", fp.SecCHMobile)
		set("sec-ch-ua-platform", fp.SecCHPlat)
	}
	h["user-agent"] = []string{fp.UserAgent}
	h["accept-language"] = []string{fp.AcceptLang}
	h["accept-encoding"] = []string{acceptEncoding}

	order := fp.HeaderOrder
	if r.XHR {
		order = xhrOrder(fp.Kind)
	}
	h[fhttp.HeaderOrderKey] = order
	h[fhttp.PHeaderOrderKey] = fp.PHeader
	for k, v := range r.Headers {
		h.Set(k, v)
	}
	req.Header = h
	return req, nil
}

func xhrOrder(kind string) []string {
	switch kind {
	case "firefox":
		return []string{"user-agent", "accept", "accept-language", "accept-encoding",
			"sec-fetch-dest", "sec-fetch-mode", "sec-fetch-site", "sec-fetch-user",
			"referer", "x-requested-with", "cookie"}
	case "safari":
		return []string{"referer", "accept-language", "accept-encoding", "x-requested-with",
			"accept", "user-agent", "cookie"}
	default:
		return []string{"sec-ch-ua", "sec-ch-ua-mobile", "sec-ch-ua-platform",
			"user-agent", "accept", "x-requested-with", "sec-fetch-site", "sec-fetch-mode",
			"sec-fetch-dest", "referer", "accept-encoding", "accept-language", "cookie"}
	}
}

func stdHeaders(fp netx.Fingerprint, referer string, xhr bool) http.Header {
	h := http.Header{}
	if xhr {
		h.Set("Accept", "application/json, text/javascript, */*; q=0.01")
		h.Set("X-Requested-With", "XMLHttpRequest")
	} else {
		h.Set("Accept", fp.Accept)
		h.Set("Upgrade-Insecure-Requests", "1")
		h.Set("Sec-Fetch-Dest", "document")
		h.Set("Sec-Fetch-Mode", "navigate")
		h.Set("Sec-Fetch-User", "?1")
		h.Set("Sec-Fetch-Site", boolStr(referer != "", "same-origin", "none"))
	}
	if referer != "" {
		h.Set("Referer", referer)
	}
	if fp.Kind == "chrome" || fp.Kind == "edge" {
		h.Set("Sec-Ch-Ua", fp.SecCH)
		h.Set("Sec-Ch-Ua-Mobile", fp.SecCHMobile)
		h.Set("Sec-Ch-Ua-Platform", fp.SecCHPlat)
	}
	h.Set("User-Agent", fp.UserAgent)
	h.Set("Accept-Language", fp.AcceptLang)
	h.Set("Accept-Encoding", acceptEncoding)
	return h
}

func boolStr(b bool, t, f string) string {
	if b {
		return t
	}
	return f
}

func toStdHeader(h fhttp.Header) http.Header {
	out := make(http.Header, len(h))
	for k, v := range h {
		if k == fhttp.HeaderOrderKey || k == fhttp.PHeaderOrderKey {
			continue
		}
		out[http.CanonicalHeaderKey(k)] = v
	}
	return out
}

func readRaw(r io.Reader) ([]byte, error) {
	return readRawEnc(r, "")
}

func readRawEnc(r io.Reader, encoding string) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r, MaxBody))
	if err != nil {
		return nil, err
	}
	if encoding == "" {
		return Decode(raw), nil
	}
	return DecodeWith(raw, encoding), nil
}

func DecodeWith(raw []byte, encoding string) []byte {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "gzip", "x-gzip":
		if out := gunzip(raw); out != nil {
			return out
		}
	case "zstd":
		if out := unzstd(raw); out != nil {
			return out
		}
	case "br":
		if out := unbrotli(raw); out != nil {
			return out
		}
	case "deflate":
		if out := inflate(raw); out != nil {
			return out
		}
	}
	return Decode(raw)
}

func Decode(raw []byte) []byte {
	if len(raw) == 0 {
		return raw
	}
	switch {
	case len(raw) >= 2 && raw[0] == 0x1f && raw[1] == 0x8b:
		if out := gunzip(raw); out != nil {
			return out
		}
		return raw
	case len(raw) >= 4 && raw[0] == 0x28 && raw[1] == 0xb5 && raw[2] == 0x2f && raw[3] == 0xfd:
		if out := unzstd(raw); out != nil {
			return out
		}
		return raw
	case len(raw) >= 2 && raw[0] == 0x78 && (raw[1] == 0x01 || raw[1] == 0x9c || raw[1] == 0xda):
		if out := inflate(raw); out != nil {
			return out
		}
		return raw
	}
	if textish(raw) {
		return raw
	}
	if out := tryRawDeflate(raw); out != nil && textish(out) {
		return out
	}
	if out := unbrotli(raw); out != nil && textish(out) {
		return out
	}
	return raw
}

func textish(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	sample := b
	if len(sample) > 4096 {
		sample = sample[:4096]
	}
	ctrl := 0
	for _, c := range sample {
		switch {
		case c == '\t' || c == '\n' || c == '\r':
		case c < 0x20:
			ctrl++
		}
	}
	return ctrl*20 <= len(sample)
}

func gunzip(raw []byte) []byte {
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil
	}
	out, rerr := io.ReadAll(io.LimitReader(zr, MaxBody))
	zr.Close()
	if rerr != nil {
		return nil
	}
	return out
}

func unzstd(raw []byte) []byte {
	dec, err := zstd.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil
	}
	defer dec.Close()
	out, rerr := io.ReadAll(io.LimitReader(dec, MaxBody))
	if rerr != nil {
		return nil
	}
	return out
}

func unbrotli(raw []byte) []byte {
	out, rerr := io.ReadAll(io.LimitReader(brotli.NewReader(bytes.NewReader(raw)), MaxBody))
	if rerr != nil || len(out) == 0 {
		return nil
	}
	return out
}

func inflate(raw []byte) []byte {
	if out := tryZlib(raw); out != nil {
		return out
	}
	if out := tryRawDeflate(raw); out != nil {
		return out
	}
	return nil
}

func tryZlib(raw []byte) []byte {
	fr, err := zlib.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil
	}
	out, rerr := io.ReadAll(io.LimitReader(fr, MaxBody))
	fr.Close()
	if rerr != nil {
		return nil
	}
	return out
}

func tryRawDeflate(raw []byte) []byte {
	fr := flate.NewReader(bytes.NewReader(raw))
	out, rerr := io.ReadAll(io.LimitReader(fr, MaxBody))
	fr.Close()
	if rerr != nil {
		return nil
	}
	return out
}

func Classify(r *Response) error {
	if r == nil {
		return errors.New("пустой ответ")
	}
	switch r.Status {
	case http.StatusForbidden, http.StatusTooManyRequests, http.StatusUnavailableForLegalReasons:
		head := r.Body
		if len(head) > 8192 {
			head = head[:8192]
		}
		s := strings.ToLower(string(head))
		switch {
		case strings.Contains(s, "captcha"), strings.Contains(s, "unusual traffic"),
			strings.Contains(s, "are you a robot"):
			return &BlockedError{Kind: BlockCaptcha, Status: r.Status}
		case strings.Contains(s, "gesperrt"), strings.Contains(s, "ip-bereich"),
			strings.Contains(s, "your ip"), strings.Contains(s, "blocked"):
			return &BlockedError{Kind: BlockIPBan, Status: r.Status}
		case strings.Contains(s, "access denied"), strings.Contains(s, "errors.edgesuite.net"):
			return &BlockedError{Kind: BlockEdge, Status: r.Status}
		case r.Status == http.StatusTooManyRequests || r.Status == http.StatusUnavailableForLegalReasons:
			return &BlockedError{Kind: BlockRate, Status: r.Status}
		default:
			return &BlockedError{Kind: BlockOther, Status: r.Status}
		}
	case http.StatusUnauthorized:
		return &BlockedError{Kind: BlockAuth, Status: r.Status}
	}
	return nil
}
