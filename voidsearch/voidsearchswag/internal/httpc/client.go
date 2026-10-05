package httpc

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"

	"voidsearchswag/internal/netx"
)

type Options struct {
	Transport    string
	Proxies      []string
	Fingerprints []string
	Timeout      time.Duration
	SessionTTL   time.Duration
	Pool         *netx.ProxyPool
	Rotator      netx.Rotator
	Logger       netx.Logger
}

type Client struct {
	mu      sync.Mutex
	rot     netx.Rotator
	pool    *netx.ProxyPool
	sess    *Session
	fpNames []string
	timeout time.Duration
	ttl     time.Duration
	log     netx.Logger
	probe   bool
}

// isolatedRotator оборачивает общий ротатор и подменяет только адрес
// транспорта: к socks-спецификации добавляются учётные данные, по которым tor
// строит отдельную цепь.
//
// Close намеренно ничего не делает. Ротатор общий для всех изолированных
// клиентов, и закрытие его одним из них убило бы tor у остальных. Закрывать
// ресурс обязан владелец, а не временный клиент.
type isolatedRotator struct {
	netx.Rotator
	key string
}

func (r *isolatedRotator) TransportSpec() string {
	return netx.IsolateSpec(r.Rotator.TransportSpec(), r.key)
}

func (r *isolatedRotator) Close() error { return nil }

// Isolated возвращает клиента, который ходит через ОТДЕЛЬНУЮ цепь tor того же
// транспорта. Нужен там, где параллелизм иначе фиктивен: восемь воркеров через
// одну цепь tor сериализуются на самом tor, а не на нашем коде.
//
// Клон разделяет ротатор и настройки исходного клиента, но имеет собственную
// сессию. Пул прокси обнуляется намеренно: падение запроса в изолированной
// цепи говорит о проблеме этой цепи, а не о смерти прокси, и помечать по этому
// событию запись пула мёртвой значило бы терять рабочие адреса.
//
// Клон собирается по полям, а не копированием структуры: в Client есть
// sync.Mutex, и побитовая копия передала бы второму клиенту чужое состояние
// блокировки - это гонка, которую не видно до первого же параллельного
// запроса.
func (c *Client) Isolated(key string) (*Client, error) {
	if strings.TrimSpace(key) == "" {
		return nil, errors.New("httpc: пустой ключ изоляции")
	}
	if c == nil || c.rot == nil {
		return nil, errors.New("httpc: изоляция доступна только с ротатором (tor или пул)")
	}
	// Список отпечатков копируется: общий срез означал бы, что добавление
	// отпечатка одним клиентом молча меняет поведение остальных.
	fps := make([]string, len(c.fpNames))
	copy(fps, c.fpNames)
	return &Client{
		rot:     &isolatedRotator{Rotator: c.rot, key: key},
		pool:    nil,
		sess:    nil,
		fpNames: fps,
		timeout: c.timeout,
		ttl:     c.ttl,
		log:     c.log,
		probe:   c.probe,
	}, nil
}

func NewClient(ctx context.Context, o Options) (*Client, error) {
	if o.Timeout <= 0 {
		o.Timeout = 30 * time.Second
	}
	if o.SessionTTL <= 0 {
		o.SessionTTL = 10 * time.Minute
	}
	if len(o.Fingerprints) == 0 {
		o.Fingerprints = []string{"chrome_131_win", "firefox_133_win", "safari_16_mac"}
	}
	for _, n := range o.Fingerprints {
		if _, ok := netx.FingerprintByName(n); !ok {
			return nil, fmt.Errorf("неизвестный отпечаток %q", n)
		}
	}

	c := &Client{
		pool:    o.Pool,
		fpNames: o.Fingerprints,
		timeout: o.Timeout,
		ttl:     o.SessionTTL,
		log:     o.Logger,
	}
	if o.Rotator != nil {
		c.rot = o.Rotator
		return c, nil
	}
	switch strings.ToLower(strings.TrimSpace(o.Transport)) {
	case "direct", "":
		return c, nil
	case "static":
		r, err := netx.NewProxyRotator(o.Proxies)
		if err != nil {
			return nil, err
		}
		c.rot = r
		return c, nil
	case "pool":
		if c.pool == nil {
			return nil, errors.New("транспорт pool: пул не передан")
		}
		r, err := netx.NewPoolRotator(c.pool)
		if err != nil {
			return nil, err
		}
		c.rot = r
		return c, nil
	default:
		return nil, fmt.Errorf("неизвестный транспорт %q (direct|static|pool|tor)", o.Transport)
	}
}

func (c *Client) Rotator() netx.Rotator { return c.rot }
func (c *Client) Pool() *netx.ProxyPool { return c.pool }

func (c *Client) pickFingerprint() netx.Fingerprint {
	names := c.fpNames
	if len(names) == 0 {
		names = []string{"chrome_131_win"}
	}
	name := names[rand.Intn(len(names))]
	fp, ok := netx.FingerprintByName(name)
	if !ok {
		fp, _ = netx.FingerprintByName("chrome_131_win")
	}
	return fp
}

func (c *Client) ensure(ctx context.Context) (*Session, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sess != nil {
		if c.rot != nil && !c.rot.Healthy() {
			c.sess.Close()
			c.sess = nil
		} else if c.sess.Age() < c.ttl {
			return c.sess, nil
		} else {
			c.sess.Close()
			c.sess = nil
		}
	}
	spec := ""
	kind := "direct"
	if c.rot != nil {
		spec = c.rot.TransportSpec()
		kind = c.rot.Kind()
	}
	s, err := NewSession(c.pickFingerprint(), spec, kind, c.timeout)
	if err != nil {
		return nil, err
	}
	c.sess = s
	c.logf("сессия: %s через %s", s.Engine(), netx.MaskSpec(spec))
	return c.sess, nil
}

func (c *Client) Rotate(ctx context.Context) error {
	if c.rot == nil {
		return nil
	}
	c.mu.Lock()
	if c.sess != nil {
		c.sess.Close()
		c.sess = nil
	}
	c.mu.Unlock()
	err := c.rot.Rotate(ctx)
	if c.pool != nil {
		c.pool.SaveState()
	}
	return err
}

func (c *Client) Close() error {
	c.mu.Lock()
	s := c.sess
	c.sess = nil
	c.mu.Unlock()
	if s != nil {
		s.Close()
	}
	if c.rot != nil {
		return c.rot.Close()
	}
	return nil
}

func (c *Client) Do(ctx context.Context, r Request) (*Response, error) {
	sess, err := c.ensure(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := sess.Do(ctx, r)
	if err != nil {
		c.noteTransportFailure()
		return nil, err
	}
	if resp.Status >= 400 {
		c.noteStatus(sess, resp)
		return resp, nil
	}
	return resp, nil
}

func (c *Client) Get(ctx context.Context, rawURL string) (*Response, error) {
	return c.Do(ctx, Request{URL: rawURL, Method: "GET"})
}

func (c *Client) GetWith(ctx context.Context, rawURL, referer string, xhr bool) (*Response, error) {
	return c.Do(ctx, Request{URL: rawURL, Method: "GET", Referer: referer, XHR: xhr})
}

func (c *Client) Escalate(ctx context.Context, r Request) (*Response, error) {
	sess, err := c.ensure(ctx)
	if err != nil {
		return nil, err
	}
	if sess.kind == "tor" {
		imp, ierr := sess.DoStd(ctx, r)
		if ierr == nil && imp.Status < 400 {
			return imp, nil
		}
	}
	if err := c.Rotate(ctx); err != nil {
		c.logf("ротация не удалась: %v", err)
	}
	sess, err = c.ensure(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := sess.DoStd(ctx, r)
	if err != nil {
		c.noteTransportFailure()
		return nil, err
	}
	if resp.Status >= 400 {
		c.noteStatus(sess, resp)
	}
	return resp, nil
}

func (c *Client) Fetch(ctx context.Context, r Request) (*Response, error) {
	resp, err := c.Do(ctx, r)
	if err != nil {
		// Истёкший контекст эскалировать некуда: срок вызывающего вышел, а
		// попытка стоила бы новой сессии и закрытия прежней. Отказ возвращается
		// как есть, и вызывающий сам решает, что делать со своим дедлайном.
		if ctx.Err() != nil {
			return nil, err
		}
		// Транспортная ошибка - такой же повод для эскалации, как блокировка.
		// Стек fhttp/tls-client падает на части адресов: onion-сайт, который
		// редиректит http->https, отдаёт «protocol negotiated» (в tls-client
		// протекает внутренний сентинел согласования ALPN), хотя тот же запрос
		// через impersonate-путь проходит. Без эскалации на ошибке такие адреса
		// теряются навсегда, а ответ у нас уже никто не спросит.
		c.logf("транспорт: %v, эскалация", err)
		if esc, eerr := c.Escalate(ctx, r); eerr == nil && esc != nil {
			return esc, nil
		}
		c.noteTransportFailure()
		return nil, err
	}
	if err := Classify(resp); err != nil {
		// Проверка истёкшего контекста здесь намеренно не дублируется: ответ уже
		// на руках, и окно между его получением и Classify измеряется
		// микросекундами. Если срок всё же вышел, эскалация упрётся в ctx.Err()
		// на первом же запросе и вернёт этот ответ как есть. Ветка была бы
		// непроверяемой на практике, а непроверенный код защищает только на
		// словах.
		c.logf("защита: %v, эскалация", err)
		esc, eerr := c.Escalate(ctx, r)
		if eerr != nil {
			return resp, nil
		}
		return esc, nil
	}
	return resp, nil
}

func (c *Client) GetString(ctx context.Context, rawURL string) (string, error) {
	resp, err := c.Fetch(ctx, Request{URL: rawURL, Method: "GET"})
	if err != nil {
		return "", err
	}
	if err := Classify(resp); err != nil {
		return "", err
	}
	return resp.Text(), nil
}

func (c *Client) noteStatus(sess *Session, resp *Response) {
	if c.pool == nil {
		return
	}
	switch resp.Status {
	case 403, 429, 451:
		c.pool.MarkSuspect(sess.Proxy(), "status "+fmt.Sprint(resp.Status))
	}
}

func (c *Client) noteTransportFailure() {
	if c.pool == nil {
		return
	}
	c.mu.Lock()
	spec := ""
	if c.sess != nil {
		spec = c.sess.Proxy()
	}
	c.mu.Unlock()
	if spec != "" {
		c.pool.MarkSuspect(spec, "transport error")
	}
}

func (c *Client) logf(format string, args ...any) {
	if c.log != nil {
		c.log.Infof(format, args...)
	}
}

func proxyHostPort(spec string) string { return ProxyHostPort(spec) }

func ProxyHostPort(spec string) string {
	s := strings.TrimSpace(spec)
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if at := strings.LastIndex(s, "@"); at >= 0 {
		s = s[at+1:]
	}
	if slash := strings.IndexByte(s, '/'); slash >= 0 {
		s = s[:slash]
	}
	return strings.TrimSpace(s)
}
