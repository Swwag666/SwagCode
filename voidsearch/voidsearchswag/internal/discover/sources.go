package discover

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/PuerkitoBio/goquery"

	"voidsearchswag/internal/httpc"
)

type Logger interface {
	Infof(format string, args ...any)
}

type Candidate struct {
	Address     string `json:"address"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Category    string `json:"category,omitempty"`
	Source      string `json:"source"`
	Via         string `json:"via"`
	Depth       int    `json:"depth"`
}

func (c Candidate) Host() string { return HostOf(c.Address) }

// Key - ключ дедупликации: один onion-адрес в пуле ровно один раз,
// сколько бы источников его ни отдали.
func (c Candidate) Key() string { return c.Host() }

var (
	onionRE = regexp.MustCompile(`(?i)\b([a-z2-7]{16}|[a-z2-7]{56})\.onion\b`)
	hrefRE  = regexp.MustCompile(`(?i)href\s*=\s*["']([^"']+)["']`)
)

// HostOf вытаскивает onion-хост из ссылки, включая голый адрес без схемы.
// Возвращает пустую строку для всего, что не является корректным .onion:
// адрес обязан быть base32-строкой длиной 16 (v2) или 56 (v3). Проверка
// строгая и для ссылок со схемой, иначе url.Parse пропускает мусор вроде
// aaa111bbb222cccc.onion, и он уходит в пул и жжёт tor-цепочку впустую.
func HostOf(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if !strings.HasSuffix(host, ".onion") {
		return ""
	}
	if !validOnionHost(host) {
		return ""
	}
	return host
}

// validOnionHost проверяет адрес целиком: base32 нужной длины и допустимый
// алфавит. Цифры 0, 1, 8 и 9 в base32 не существуют.
func validOnionHost(host string) bool {
	label := strings.TrimSuffix(host, ".onion")
	switch len(label) {
	case 16, 56:
	default:
		return false
	}
	for _, c := range label {
		if (c >= 'a' && c <= 'z') || (c >= '2' && c <= '7') {
			continue
		}
		return false
	}
	return true
}

// Extract собирает onion-адреса из произвольного текста: ссылки, голые
// адреса в тексте, адреса внутри data-атрибутов. Дедуп по хосту.
func Extract(body string) []string {
	seen := map[string]bool{}
	var out []string

	add := func(raw string) {
		h := HostOf(raw)
		if h == "" || seen[h] {
			return
		}
		seen[h] = true
		out = append(out, h)
	}

	for _, m := range hrefRE.FindAllStringSubmatch(body, -1) {
		add(m[1])
	}
	for _, m := range onionRE.FindAllString(body, -1) {
		add(m)
	}
	return out
}

type source struct {
	name string
	url  string
	// selector — если задан, адреса берутся только из этих ссылок;
	// иначе сканируется весь текст страницы.
	selector string
}

// clearnetSources — публичные каталоги и официальные фронты. Доступны
// обычным GET, tor для них не нужен, поэтому они не тратят канал.
// onion.live/directory убран: страница отдаёт 404.
// tor.watch и thehiddenwiki.org стоят за Cloudflare: без совпадения
// отпечатка они отдают challenge, поэтому их трафик идёт прямым клиентом
// с имперсонацией, а не через tor-выход.
var clearnetSources = []source{
	{name: "ahmia-address", url: "https://ahmia.fi/address/", selector: "#addressList a, .addressList a, li a"},
	{name: "ahmia-search", url: "https://ahmia.fi/search/?q=onion+index", selector: ".result h4 a"},
	{name: "darkfail", url: "https://dark.fail/"},
	{name: "onionlive", url: "https://onion.live/"},
	{name: "tor-watch", url: "https://tor.watch/"},
	{name: "hidden-wiki", url: "https://thehiddenwiki.org/"},
	{name: "hidden-wiki-net", url: "https://thehiddenwiki.com/"},
	{name: "torlink", url: "https://tor.link/"},
}

// Finder собирает адреса из clearnet-каталогов. Клиент объявлен
// интерфейсом: каталоги лежат на живых хостах, а тесты должны проверять
// разбор разметки и отчёты, не поднимая сеть.
type Finder struct {
	Client Fetcher
	Log    Logger
	// Attempts - число попыток на источник. Больше единицы имеет смысл:
	// каталоги тяжёлые (ahmia отдаёт 3 МБ), и один таймаут не должен
	// выбрасывать источник из прогона целиком.
	Attempts int
	// MinYield - минимальная выдача, при которой источник считается живым.
	// Страница может отвечать 200 и при этом содержать один адрес: каталог за
	// ней мёртв, а отчёт годами показывает «ок». Ноль означает дефолт.
	MinYield int
}

// sourceAttempts - сколько раз пробовать источник до отказа.
const sourceAttempts = 2

// sourceMinYield - порог живой выдачи. Единица не годится: один адрес мог
// приехать из шапки страницы или из бокового блока, а не из каталога.
const sourceMinYield = 2

func NewFinder(client Fetcher, log Logger) *Finder {
	return &Finder{Client: client, Log: log}
}

func (f *Finder) attempts() int {
	if f.Attempts <= 0 {
		return sourceAttempts
	}
	return f.Attempts
}

func (f *Finder) minYield() int {
	if f.MinYield <= 0 {
		return sourceMinYield
	}
	return f.MinYield
}

// Sources перечисляет имена clearnet-источников — для отчётов и тестов.
func Sources() []string {
	out := make([]string, 0, len(clearnetSources))
	for _, s := range clearnetSources {
		out = append(out, s.name)
	}
	return out
}

// DiscoverClearnet обходит каждый clearnet-источник и собирает адреса.
// Падение одного источника не роняет остальные: их отчёты независимы.
func (f *Finder) DiscoverClearnet(ctx context.Context) ([]Candidate, []SourceReport) {
	type result struct {
		idx   int
		cands []Candidate
		rep   SourceReport
	}

	ch := make(chan result, len(clearnetSources))
	for i, s := range clearnetSources {
		go func(i int, s source) {
			start := time.Now()
			cands, err := f.fetchSource(ctx, s)
			rep := SourceReport{
				Name:    s.name,
				URL:     s.url,
				Found:   len(cands),
				Elapsed: time.Since(start).Round(time.Millisecond).String(),
				Via:     "clearnet",
			}
			if err != nil {
				rep.Error = trimErr(err)
			} else if len(cands) < f.minYield() {
				// Источник ответил, но почти ничего не отдал. Страница жива,
				// каталог за ней мёртв: без этой проверки ahmia-search годами
				// рапортует «ок», отдавая один адрес, и отчёт о здоровье
				// источников перестаёт отражать реальность.
				rep.Sparse = true
				rep.Error = fmt.Sprintf("выдача %d при пороге %d: каталог пуст или источник мёртв",
					len(cands), f.minYield())
			} else {
				rep.OK = true
			}
			ch <- result{idx: i, cands: cands, rep: rep}
		}(i, s)
	}

	results := make([]result, 0, len(clearnetSources))
	for range clearnetSources {
		select {
		case <-ctx.Done():
			results = append(results, result{})
		case r := <-ch:
			results = append(results, r)
		}
	}

	sort.Slice(results, func(a, b int) bool { return results[a].idx < results[b].idx })

	var out []Candidate
	reports := make([]SourceReport, 0, len(results))
	for _, r := range results {
		if r.rep.Name == "" {
			continue
		}
		out = append(out, r.cands...)
		reports = append(reports, r.rep)
		if !r.rep.OK && f.Log != nil {
			f.Log.Infof("discover/%s: %s", r.rep.Name, r.rep.Error)
		}
	}
	return out, reports
}

func (f *Finder) fetchSource(ctx context.Context, s source) ([]Candidate, error) {
	if f.Client == nil {
		return nil, fmt.Errorf("клиент не задан")
	}

	var lastErr error
	for attempt := 0; attempt < f.attempts(); attempt++ {
		if ctx.Err() != nil {
			if lastErr == nil {
				lastErr = ctx.Err()
			}
			break
		}
		cands, err := f.fetchOnce(ctx, s)
		if err == nil {
			return cands, nil
		}
		lastErr = err
		// Повтор бессмысленен, если отказ не сетевой: HTTP 4xx означает,
		// что источник отказал осознанно, и вторая попытка получит то же.
		if !retryable(err) {
			break
		}
		if f.Log != nil && attempt+1 < f.attempts() {
			f.Log.Infof("discover/%s: %v, повтор", s.name, err)
		}
	}
	return nil, lastErr
}

// retryable отличает транзиентный сетевой сбой от осознанного отказа.
// Таймаут и обрыв соединения стоит повторить; HTTP 4xx и «адресов не
// найдено» - нет: источник ответит тем же.
func retryable(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	switch {
	case strings.HasPrefix(msg, "HTTP 4"):
		return false
	case strings.Contains(msg, "адресов не найдено"):
		return false
	case strings.HasPrefix(msg, "HTTP 5"):
		return true
	default:
		return true
	}
}

func (f *Finder) fetchOnce(ctx context.Context, s source) ([]Candidate, error) {
	fctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	resp, err := f.Client.Fetch(fctx, httpc.Request{URL: s.url, Method: http.MethodGet})
	if err != nil {
		return nil, err
	}
	if resp.Status >= 400 {
		return nil, fmt.Errorf("HTTP %d", resp.Status)
	}

	cands := f.extractFromSource(string(resp.Body), s)
	if len(cands) == 0 {
		return nil, fmt.Errorf("адресов не найдено")
	}

	for i := range cands {
		cands[i].Source = s.name
		cands[i].Via = "clearnet"
	}
	return cands, nil
}

// extractFromSource вытаскивает адреса и их названия. Имя берётся только
// из текста самой ссылки: каталоги вида ahmia отдают список подписанных
// ссылок, и этот текст - единственная надёжная мета. Подпись, которая
// оказалась самим адресом или URL, отбрасывается как бесполезная.
func (f *Finder) extractFromSource(body string, s source) []Candidate {
	if s.selector != "" {
		if doc, err := goquery.NewDocumentFromReader(strings.NewReader(body)); err == nil {
			seen := map[string]bool{}
			var out []Candidate
			doc.Find(s.selector).Each(func(_ int, a *goquery.Selection) {
				href, _ := a.Attr("href")
				h := HostOf(href)
				if h == "" || seen[h] {
					return
				}
				seen[h] = true
				out = append(out, Candidate{Address: h, Title: cleanTitle(h, a.Text())})
			})
			if len(out) > 0 {
				return out
			}
		}
	}

	addrs := Extract(body)
	out := make([]Candidate, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, Candidate{Address: a})
	}
	return out
}

// cleanTitle приводит подпись ссылки к пригодному виду. Каталоги часто
// пишут «Название http://адрес.onion» или «Название адрес.onion» одним
// текстом - берём часть до URL. Подпись, равная адресу или его хосту,
// смысла не несёт и отбрасывается: заголовок-адрес хуже отсутствующего,
// потому что по нему нельзя искать, а вид он делает.
func cleanTitle(addr, raw string) string {
	t := strings.Join(strings.Fields(raw), " ")
	if t == "" {
		return ""
	}
	low := strings.ToLower(t)

	cut := -1
	for _, marker := range []string{"http://", "https://", "www."} {
		if i := strings.Index(low, marker); i >= 0 && (cut < 0 || i < cut) {
			cut = i
		}
	}
	if i := strings.Index(low, ".onion"); i >= 0 {
		tail := low[:i]
		if j := strings.LastIndex(tail, " "); j >= 0 && (cut < 0 || j < cut) {
			cut = j
		} else if cut < 0 {
			cut = 0
		}
	}
	if cut == 0 {
		return ""
	}
	if cut > 0 {
		t = strings.TrimSpace(t[:cut])
	}
	if t == "" {
		return ""
	}

	lower := strings.ToLower(t)
	host := strings.ToLower(addr)
	label := strings.TrimSuffix(host, ".onion")
	switch lower {
	case host, label, "www." + host, "www." + label:
		return ""
	}
	if !plausibleTitle(t) {
		return ""
	}
	t = stripLatencyPrefix(unescapeEntities(t))
	r := []rune(t)
	if len(r) > 160 {
		t = string(r[:160])
	}
	return t
}

// unescapeEntities раскодирует основные HTML-сущности. goquery отдаёт
// текст ссылки уже распакованным, но каталоги с битой разметкой
// показывают «&#039;» буквально.
func unescapeEntities(t string) string {
	repl := []struct{ from, to string }{
		{"&#039;", "'"}, {"&#39;", "'"}, {"&apos;", "'"},
		{"&quot;", `"`}, {"&#34;", `"`},
		{"&amp;", "&"}, {"&lt;", "<"}, {"&gt;", ">"},
		{"&nbsp;", " "}, {"&mdash;", "-"}, {"&ndash;", "-"},
		{"&hellip;", "..."}, {"&laquo;", "\""}, {"&raquo;", "\""},
	}
	for _, r := range repl {
		t = strings.ReplaceAll(t, r.from, r.to)
	}
	return t
}

// stripLatencyPrefix убирает замер времени в начале подписи: каталоги
// отдают строку вида «4.0s Описание сервиса», и без чистки латентность
// становится частью названия.
func stripLatencyPrefix(t string) string {
	fields := strings.Fields(t)
	if len(fields) < 2 {
		return t
	}
	head := fields[0]
	if len(head) < 2 {
		return t
	}
	unit := head[len(head)-1]
	if unit != 's' && unit != 'm' && unit != 'h' {
		return t
	}
	digits := strings.TrimSuffix(head, string(unit))
	if digits == "" {
		return t
	}
	for _, c := range digits {
		if (c < '0' || c > '9') && c != '.' {
			return t
		}
	}
	return strings.Join(fields[1:], " ")
}

// plausibleTitle отсекает обрывки разметки, которые каталоги отдают как
// текст ссылки: «/"», «"», «/tdw"», «href="/x». Название начинается с
// буквы или цифры, состоит в основном из букв, цифр и обычных знаков
// текста и не содержит фрагментов атрибутов.
func plausibleTitle(t string) bool {
	t = strings.TrimSpace(t)
	if strings.Contains(t, `="`) || strings.Contains(t, `='`) {
		return false
	}
	if strings.HasPrefix(strings.ToLower(t), "href") {
		return false
	}
	runes := []rune(t)
	if len(runes) < 3 {
		return false
	}
	first := runes[0]
	if !unicode.IsLetter(first) && !unicode.IsDigit(first) {
		return false
	}
	var alnum, ok int
	for _, c := range runes {
		switch {
		case unicode.IsLetter(c) || unicode.IsDigit(c):
			alnum++
			ok++
		case strings.ContainsRune(" .,:-_&'!?()+/", c):
			ok++
		}
	}
	if alnum < 3 {
		return false
	}
	return ok*2 >= len(runes)
}

type SourceReport struct {
	Name    string `json:"name"`
	URL     string `json:"url"`
	OK      bool   `json:"ok"`
	Found   int    `json:"found"`
	Elapsed string `json:"elapsed"`
	Via     string `json:"via"`
	Error   string `json:"error,omitempty"`
	// Sparse означает «ответ получен, но выдача ниже порога». Отдельный флаг
	// нужен, чтобы отличать мёртвый каталог от сетевой ошибки: чинить их
	// надо по-разному, а в отчёте они иначе выглядят одинаково.
	Sparse bool `json:"sparse,omitempty"`
}

// Merge схлопывает кандидатов по onion-адресу, сохраняя порядок первого
// появления: источники идут от лучших к худшим, поэтому первый и главный.
func Merge(cands []Candidate) []Candidate {
	seen := map[string]bool{}
	out := make([]Candidate, 0, len(cands))
	for _, c := range cands {
		k := c.Key()
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, c)
	}
	return out
}

func trimErr(err error) string {
	if err == nil {
		return ""
	}
	s := strings.Join(strings.Fields(err.Error()), " ")
	r := []rune(s)
	if len(r) > 140 {
		return string(r[:140])
	}
	return s
}
