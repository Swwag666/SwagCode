package netx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

type Provider interface {
	Name() string
	Fetch(ctx context.Context, limit int) ([]string, error)
}

type ProxyScrape struct {
	Protocol  string
	Country   string
	TimeoutMS int
	Anonymity string
	SSL       string
	Endpoint  string
}

func DefaultProxyScrape() ProxyScrape {
	return ProxyScrape{
		Protocol:  "http",
		Country:   "all",
		TimeoutMS: 8000,
		Anonymity: "all",
		SSL:       "all",
		Endpoint:  "https://api.proxyscrape.com/v2/",
	}
}

// Effective возвращает копию поставщика с подставленными значениями по
// умолчанию. Пустое поле законно и означает «как у DefaultProxyScrape»: url и
// Name подставляли те же дефолты через orStr и orInt, но наружу подстановка
// видна не была, поэтому вызывающий не мог напечатать те параметры, которые
// действительно уйдут в запрос. Прогрев пула из CLI печатает строку поставщика
// именно отсюда: дефолты провайдера живут в одном месте, а не копируются в
// команду.
func (p ProxyScrape) Effective() ProxyScrape {
	d := DefaultProxyScrape()
	return ProxyScrape{
		Protocol:  orStr(p.Protocol, d.Protocol),
		Country:   orStr(p.Country, d.Country),
		TimeoutMS: orInt(p.TimeoutMS, d.TimeoutMS),
		Anonymity: orStr(p.Anonymity, d.Anonymity),
		SSL:       orStr(p.SSL, d.SSL),
		Endpoint:  orStr(p.Endpoint, d.Endpoint),
	}
}

func (p ProxyScrape) Name() string {
	return "proxyscrape/" + strings.ToLower(p.Effective().Protocol)
}

func (p ProxyScrape) url() string {
	e := p.Effective()
	ep := e.Endpoint
	q := fmt.Sprintf("request=getproxies&protocol=%s&timeout=%d&country=%s&ssl=%s&anonymity=%s",
		strings.ToLower(e.Protocol), e.TimeoutMS,
		strings.ToLower(e.Country), strings.ToLower(e.SSL),
		strings.ToLower(e.Anonymity))
	// Порядок веток важен: адрес, уже заканчивающийся на "?", содержит этот
	// символ, поэтому проверка суффикса обязана идти первой. Иначе получится
	// "?&request=..." с пустым первым параметром, а ветка суффикса останется
	// недостижимой.
	switch {
	case strings.HasSuffix(ep, "?"):
		return ep + q
	case strings.Contains(ep, "?"):
		return ep + "&" + q
	default:
		return ep + "?" + q
	}
}

func (p ProxyScrape) Fetch(ctx context.Context, limit int) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.url(), nil)
	if err != nil {
		return nil, err
	}
	c := &http.Client{Timeout: 30 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", p.Name(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", p.Name(), resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("%s: чтение: %w", p.Name(), err)
	}
	scheme := schemeFor(p.Protocol)
	seen := map[string]bool{}
	out := make([]string, 0, 64)
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		spec := line
		if !strings.Contains(spec, "://") {
			spec = scheme + spec
		}
		if _, perr := parseProxySpec(spec); perr != nil {
			continue
		}
		if seen[spec] {
			continue
		}
		seen[spec] = true
		out = append(out, spec)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: пустой ответ", p.Name())
	}
	return out, nil
}

func schemeFor(protocol string) string {
	switch strings.ToLower(protocol) {
	case "socks5", "socks5h":
		return "socks5://"
	case "socks4", "socks4a":
		return "socks4://"
	case "https":
		return "https://"
	default:
		return "http://"
	}
}

type StaticProvider []string

func (s StaticProvider) Name() string { return "static" }

func (s StaticProvider) Fetch(_ context.Context, limit int) ([]string, error) {
	out := make([]string, 0, len(s))
	for _, spec := range s {
		spec = strings.TrimSpace(spec)
		if spec == "" || strings.HasPrefix(spec, "#") {
			continue
		}
		if !strings.Contains(spec, "://") {
			spec = "socks5://" + spec
		}
		if _, err := parseProxySpec(spec); err != nil {
			continue
		}
		out = append(out, spec)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	if len(out) == 0 {
		return nil, errors.New("static: пустой список")
	}
	return out, nil
}

var probeTargets = []string{
	"https://api.ipify.org?format=json",
	"https://ifconfig.co/json",
}

var ErrProbeTarget = errors.New("проб-сервис недоступен")

func ProbeProxy(ctx context.Context, spec string, timeout time.Duration) (string, time.Duration, error) {
	timeout = orDur(timeout, 7*time.Second)
	pi, err := parseProxySpec(spec)
	if err != nil {
		return "", 0, err
	}
	start := time.Now()
	tr := &http.Transport{
		MaxIdleConns:          2,
		IdleConnTimeout:       10 * time.Second,
		TLSHandshakeTimeout:   timeout,
		ExpectContinueTimeout: time.Second,
		DialContext: (&net.Dialer{
			Timeout:   timeout,
			KeepAlive: 15 * time.Second,
		}).DialContext,
	}
	if pi.isSocks() {
		tr.DialContext = socksDial(pi, timeout)
	} else {
		u, uerr := parseProxyURL(spec)
		if uerr != nil {
			return "", 0, uerr
		}
		tr.Proxy = http.ProxyURL(u)
	}
	cl := &http.Client{Timeout: timeout, Transport: tr}
	defer cl.CloseIdleConnections()

	var lastErr error
	tunnelOK := false
	for _, target := range probeTargets {
		rctx, cancel := context.WithTimeout(ctx, timeout)
		req, err := http.NewRequestWithContext(rctx, http.MethodGet, target, nil)
		if err != nil {
			cancel()
			lastErr = err
			continue
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
		req.Header.Set("Accept", "application/json")
		resp, err := cl.Do(req)
		if err != nil {
			cancel()
			lastErr = err
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		gotStatus := resp.StatusCode
		resp.Body.Close()
		cancel()
		tunnelOK = true
		if gotStatus != http.StatusOK {
			lastErr = fmt.Errorf("probe: HTTP %d", gotStatus)
			continue
		}
		ip := extractIP(body)
		if ip == "" {
			lastErr = errors.New("probe: в ответе нет IP")
			continue
		}
		return ip, time.Since(start), nil
	}
	if tunnelOK {
		return "", time.Since(start), fmt.Errorf("%w: %v", ErrProbeTarget, lastErr)
	}
	return "", time.Since(start), lastErr
}

func extractIP(b []byte) string {
	s := string(b)
	for _, key := range []string{`"ip":"`, `"query":"`} {
		i := strings.Index(s, key)
		if i < 0 {
			continue
		}
		rest := s[i+len(key):]
		if j := strings.IndexByte(rest, '"'); j > 0 {
			return rest[:j]
		}
	}
	return ""
}

type LiveProxy struct {
	Spec    string
	ExitIP  string
	Latency time.Duration
}

func FilterLive(ctx context.Context, list []string, concurrency int, timeout time.Duration, log Logger, st *PoolState) []LiveProxy {
	concurrency = orInt(concurrency, 48)
	if concurrency > 64 {
		concurrency = 64
	}
	timeout = orDur(timeout, 7*time.Second)
	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		live    []LiveProxy
		sem     = make(chan struct{}, concurrency)
		probed  int
		skipped int
		blind   int
	)
	for _, spec := range list {
		if ctx.Err() != nil {
			break
		}
		if st != nil {
			if why := st.Skip(spec, st.ExitIP(spec)); why != "" {
				skipped++
				continue
			}
		}
		wg.Add(1)
		go func(spec string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			ip, lat, err := ProbeProxy(ctx, spec, timeout)
			noVerdict := errors.Is(err, ErrProbeTarget)

			mu.Lock()
			probed++
			done := probed
			switch {
			case err == nil:
				live = append(live, LiveProxy{Spec: spec, ExitIP: ip, Latency: lat})
			case noVerdict:
				live = append(live, LiveProxy{Spec: spec, Latency: lat})
				blind++
			}
			alive := len(live)
			mu.Unlock()

			if st != nil && err == nil {
				st.RememberExit(spec, ip)
			}
			if st != nil && err != nil && !noVerdict {
				st.MarkDead(spec, err.Error())
			}
			if log != nil && done%50 == 0 {
				log.Infof("проверка прокси: %d/%d пройдено, живых %d", done, len(list), alive)
			}
		}(spec)
	}
	wg.Wait()
	sort.SliceStable(live, func(a, b int) bool {
		if (live[a].ExitIP == "") != (live[b].ExitIP == "") {
			return live[a].ExitIP != ""
		}
		return live[a].Latency < live[b].Latency
	})
	if log != nil {
		pct := 0.0
		if checked := len(list) - skipped; checked > 0 {
			pct = 100 * float64(len(live)) / float64(checked)
		}
		if skipped > 0 || blind > 0 {
			log.Infof("прокси: %d в списке, %d пропущено по состоянию, проверено %d, живых %d (%.0f%%), из них без exit IP %d",
				len(list), skipped, probed, len(live), pct, blind)
		} else {
			log.Infof("прокси: проверено %d, живых %d (%.0f%%)", probed, len(live), pct)
		}
	}
	if st != nil {
		_ = st.Save()
	}
	return live
}

type PoolConfig struct {
	Provider     Provider
	Verify       bool
	ProbeTimeout time.Duration
	Concurrency  int
	FetchLimit   int
	MinLive      int
	MaxLive      int
	RefillWait   time.Duration
	StatePath    string
}

func DefaultPoolConfig(p Provider) PoolConfig {
	return PoolConfig{
		Provider:     p,
		Verify:       true,
		ProbeTimeout: 7 * time.Second,
		Concurrency:  48,
		FetchLimit:   400,
		MinLive:      3,
		MaxLive:      60,
		RefillWait:   20 * time.Second,
	}
}

type poolEntry struct {
	lp    LiveProxy
	used  time.Time
	bad   int
	alive bool
}

type ProxyPool struct {
	cfg PoolConfig
	log Logger
	st  *PoolState

	mu       sync.Mutex
	entries  []*poolEntry
	dead     map[string]int
	lastRef  time.Time
	lastSave time.Time
	killed   int
}

func NewProxyPool(ctx context.Context, cfg PoolConfig, log Logger) (*ProxyPool, error) {
	if cfg.Provider == nil {
		return nil, errors.New("pool: не задан поставщик")
	}
	cfg.MaxLive = orInt(cfg.MaxLive, 60)
	cfg.FetchLimit = orInt(cfg.FetchLimit, 400)
	cfg.ProbeTimeout = orDur(cfg.ProbeTimeout, 7*time.Second)
	cfg.Concurrency = orInt(cfg.Concurrency, 48)
	cfg.MinLive = orInt(cfg.MinLive, 3)
	if cfg.RefillWait <= 0 {
		cfg.RefillWait = 20 * time.Second
	}
	p := &ProxyPool{cfg: cfg, log: log, dead: map[string]int{}}
	if cfg.StatePath != "" {
		p.st = LoadPoolState(cfg.StatePath, log)
	}
	if err := p.fill(ctx); err != nil {
		return nil, err
	}
	if p.countLive() == 0 {
		return nil, fmt.Errorf("pool: %s не дал ни одного живого прокси", cfg.Provider.Name())
	}
	return p, nil
}

func (p *ProxyPool) State() *PoolState { return p.st }

func (p *ProxyPool) countLive() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, e := range p.entries {
		if e.alive {
			n++
		}
	}
	return n
}

func (p *ProxyPool) fill(ctx context.Context) error {
	list, err := p.cfg.Provider.Fetch(ctx, p.cfg.FetchLimit)
	if err != nil {
		return err
	}
	if !p.cfg.Verify {
		for _, s := range list {
			if p.st != nil && p.st.Skip(s, "") != "" {
				continue
			}
			p.add(LiveProxy{Spec: s})
		}
		return nil
	}
	for _, lp := range FilterLive(ctx, list, p.cfg.Concurrency, p.cfg.ProbeTimeout, p.log, p.st) {
		p.add(lp)
	}
	return nil
}

func (p *ProxyPool) refillAsync() {
	p.mu.Lock()
	if time.Since(p.lastRef) < p.cfg.RefillWait {
		p.mu.Unlock()
		return
	}
	p.lastRef = time.Now()
	p.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := p.fill(ctx); err != nil && p.log != nil {
			p.log.Warnf("дозаправка пула %s: %v", p.cfg.Provider.Name(), err)
		}
	}()
}

func (p *ProxyPool) add(lp LiveProxy) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.entries {
		if e.lp.Spec == lp.Spec {
			if !e.alive {
				e.alive = true
				e.used = time.Time{}
			}
			return
		}
	}
	n := 0
	for _, e := range p.entries {
		if e.alive {
			n++
		}
	}
	if n >= p.cfg.MaxLive {
		return
	}
	p.entries = append(p.entries, &poolEntry{lp: lp, alive: true})
}

func (p *ProxyPool) Next() (LiveProxy, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var best *poolEntry
	for _, e := range p.entries {
		if !e.alive {
			continue
		}
		if best == nil || e.used.Before(best.used) {
			best = e
		}
	}
	if best == nil {
		return LiveProxy{}, false
	}
	best.used = time.Now()
	return best.lp, true
}

func (p *ProxyPool) verdictOf(spec string) (exitIP string, found bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.entries {
		if e.lp.Spec == spec {
			return e.lp.ExitIP, true
		}
	}
	return "", false
}

func (p *ProxyPool) drop(spec, reason string) int {
	p.mu.Lock()
	var hit *poolEntry
	for _, e := range p.entries {
		if e.lp.Spec == spec && e.alive {
			hit = e
			break
		}
	}
	if hit == nil {
		p.mu.Unlock()
		return -1
	}
	hit.alive = false
	hit.bad++
	p.dead[spec]++
	p.killed++
	live := 0
	for _, e := range p.entries {
		if e.alive {
			live++
		}
	}
	p.mu.Unlock()

	if p.log != nil {
		p.log.Warnf("прокси %s выброшен (%s), живых в пуле %d", maskProxy(spec), clipReason(reason), live)
	}
	if live < p.cfg.MinLive {
		p.refillAsync()
	}
	return live
}

func (p *ProxyPool) MarkDead(spec, reason string) {
	if p.drop(spec, "мёртв: "+reason) < 0 {
		return
	}
	if p.st != nil {
		p.st.MarkDead(spec, reason)
		p.maybeSave()
	}
}

func (p *ProxyPool) MarkBanned(spec, reason string) {
	exitIP, _ := p.verdictOf(spec)
	if exitIP == "" && p.st != nil {
		exitIP = p.st.ExitIP(spec)
	}
	p.drop(spec, "бан по IP: "+reason)
	if p.st != nil {
		p.st.MarkBanned(spec, exitIP, reason)
		p.maybeSave()
	}
}

func (p *ProxyPool) MarkSuspect(spec, reason string) {
	if p.st == nil {
		return
	}
	exitIP, _ := p.verdictOf(spec)
	if exitIP == "" {
		exitIP = p.st.ExitIP(spec)
	}
	p.st.MarkSuspect(spec, exitIP, reason)
	p.maybeSave()
}

func (p *ProxyPool) maybeSave() {
	if p.st == nil {
		return
	}
	p.mu.Lock()
	if time.Since(p.lastSave) < 15*time.Second {
		p.mu.Unlock()
		return
	}
	p.lastSave = time.Now()
	p.mu.Unlock()
	if err := p.st.Save(); err != nil && p.log != nil {
		p.log.Warnf("сохранение состояния пула: %v", err)
	}
}

func (p *ProxyPool) SaveState() error {
	if p.st == nil {
		return nil
	}
	p.mu.Lock()
	p.lastSave = time.Now()
	p.mu.Unlock()
	return p.st.Save()
}

func (p *ProxyPool) Stats() (live, total, killed int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.entries {
		total++
		if e.alive {
			live++
		}
	}
	return live, total, p.killed
}

func (p *ProxyPool) ProviderName() string { return p.cfg.Provider.Name() }

func (p *ProxyPool) ExitIP(spec string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	want := maskProxy(spec)
	for _, e := range p.entries {
		if maskProxy(e.lp.Spec) == want {
			return e.lp.ExitIP
		}
	}
	return ""
}

type poolRotator struct {
	pool *ProxyPool

	mu   sync.Mutex
	spec string
}

func NewPoolRotator(pool *ProxyPool) (Rotator, error) {
	lp, ok := pool.Next()
	if !ok {
		return nil, errors.New("pool: пустой пул")
	}
	return &poolRotator{pool: pool, spec: lp.Spec}, nil
}

func (r *poolRotator) Kind() string { return "proxy" }

func (r *poolRotator) TransportSpec() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.spec
}

func (r *poolRotator) Rotate(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if lp, ok := r.pool.Next(); ok {
		r.spec = lp.Spec
		return nil
	}
	r.mu.Unlock()
	err := r.pool.fill(context.Background())
	r.mu.Lock()
	if lp, ok := r.pool.Next(); ok {
		r.spec = lp.Spec
		return nil
	}
	if err == nil {
		err = errors.New("pool: ни одного живого прокси")
	}
	return err
}

func (r *poolRotator) MarkFailed(spec, reason string) { r.pool.MarkDead(spec, reason) }

func (r *poolRotator) MarkBanned(spec, reason string) { r.pool.MarkBanned(spec, reason) }

func (r *poolRotator) MarkSuspect(spec, reason string) { r.pool.MarkSuspect(spec, reason) }

func (r *poolRotator) Healthy() bool {
	live, _, _ := r.pool.Stats()
	return live > 0
}

func (r *poolRotator) Close() error { return r.pool.SaveState() }

func (r *poolRotator) Pool() *ProxyPool { return r.pool }

func (r *poolRotator) ExitIPFor(spec string) string { return r.pool.ExitIP(spec) }
