package searchers

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"voidsearchswag/internal/netx"
)

type Result struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
	Source  string `json:"source"`
	Rank    int    `json:"rank"`
	Onion   bool   `json:"onion"`
}

type Searcher interface {
	Name() string
	Search(ctx context.Context, query string, limit int) ([]Result, error)
}

type Chain struct {
	searchers []Searcher
	log       netx.Logger
	PerEngine time.Duration
}

func NewChain(log netx.Logger, ss ...Searcher) *Chain {
	return &Chain{searchers: ss, log: log, PerEngine: 25 * time.Second}
}

func (c *Chain) Names() []string {
	out := make([]string, 0, len(c.searchers))
	for _, s := range c.searchers {
		out = append(out, s.Name())
	}
	return out
}

type ChainReport struct {
	Results []Result
	Tried   []string
	// Empty - движки, которые отработали без ошибки, но ничего не нашли.
	// Отдельно от Failed, потому что это противоположные состояния: «по запросу
	// ничего нет» и «движок недоступен». Раньше пустая выдача записывалась в
	// Failed как "пустая выдача", и Search сообщал «все поисковики упали», когда
	// на самом деле поиск сработал и честно не нашёл ничего.
	Empty  []string
	Failed map[string]string
}

func (c *Chain) Search(ctx context.Context, query string, limit int) ([]Result, error) {
	rep := c.SearchDetailed(ctx, query, limit)
	if len(rep.Results) == 0 {
		if len(rep.Failed) == 0 {
			// Ничего не упало: либо цепочка пуста, либо все движки честно
			// вернули ноль. Второе - не ошибка, и сообщать о нём как о поломке
			// значит провоцировать клиента на повторные попытки и смену режима.
			if len(rep.Empty) > 0 {
				return nil, nil
			}
			return nil, errors.New("нет ни одного поисковика в цепочке")
		}
		// Часть движков действительно упала. Если при этом были и пустые
		// выдачи, это тоже полезно назвать: «все упали» при одном упавшем и
		// трёх пустых вводит в заблуждение не меньше.
		parts := make([]string, 0, len(rep.Failed))
		for _, name := range rep.Tried {
			if msg, ok := rep.Failed[name]; ok {
				parts = append(parts, name+": "+msg)
			}
		}
		if len(rep.Empty) > 0 {
			return nil, fmt.Errorf("поисковики не дали результатов: упали (%s), пустая выдача (%s)",
				strings.Join(parts, "; "), strings.Join(rep.Empty, ", "))
		}
		return nil, fmt.Errorf("все поисковики упали (%s)", strings.Join(parts, "; "))
	}
	return rep.Results, nil
}

func (c *Chain) SearchDetailed(ctx context.Context, query string, limit int) ChainReport {
	rep := ChainReport{Failed: map[string]string{}}
	var collected []Result
	for _, s := range c.searchers {
		if ctx.Err() != nil {
			break
		}
		rep.Tried = append(rep.Tried, s.Name())
		res, err := c.searchOne(ctx, s, query, limit)
		if err != nil {
			c.logf("%s: %v", s.Name(), err)
			rep.Failed[s.Name()] = err.Error()
			continue
		}
		if len(res) == 0 {
			c.logf("%s: пусто", s.Name())
			rep.Empty = append(rep.Empty, s.Name())
			continue
		}
		c.logf("%s: %d результатов", s.Name(), len(res))
		collected = append(collected, res...)
		if len(collected) >= limit {
			break
		}
	}
	rep.Results = Dedupe(collected, limit)
	return rep
}

func (c *Chain) searchOne(ctx context.Context, s Searcher, query string, limit int) ([]Result, error) {
	t := c.PerEngine
	if t <= 0 {
		t = 25 * time.Second
	}
	sctx, cancel := context.WithTimeout(ctx, t)
	defer cancel()
	return s.Search(sctx, query, limit)
}

func (c *Chain) logf(format string, args ...any) {
	if c.log != nil {
		c.log.Infof(format, args...)
	}
}

// onionHostRE - строгая база onion-адреса: 16 или 56 знаков base32.
//
// Прежний диапазон {16,56} пропускал любую длину между ними: 17, 20, 33
// знака формально проходили проверку и получали onion=true. Живой прогон
// смоука этапа 179 ловил IsOnion на адресах с произвольной длиной - для
// v2-адресов (56) и проприетарной схемы (16) промежуточных длин не бывает,
// любая другая - битая контрольная сумма, которая не откроется в tor.
var onionHostRE = regexp.MustCompile(`(?i)^(?:[a-z2-7]{16}|[a-z2-7]{56})\.onion$`)

func IsOnion(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return onionHostRE.MatchString(u.Hostname())
}

func Dedupe(in []Result, limit int) []Result {
	if limit <= 0 {
		limit = 50
	}
	return dedupeUpTo(in, limit)
}

// DedupeAll убирает повторы по NormalizeURL, но не режет выдачу: срез по
// лимиту - работа вызывающего, и делает он это после реранка и фильтра шума.
// Прежний вызов Dedupe(all, limit) внутри searchDeep обрезал пул по порядку
// движения движков: движок №1 занимал все слоты ещё ДО реранка и фильтра,
// движок №2 с релевантными title-совпадениями выпадал из выдачи целиком,
// а витрину №1 доедал токен-фильтр уже ПОСЛЕ среза - выдача приходила
// короче лимита при живых движках (замер BEFORE этапа 179: «market»,
// tornet ok=19 и torch ok=25 при limit=15, выдача - 7, все tornet;
// «forum»: 34 от движков, выдано 15, note о потерях молчал, потому что
// 19 результатов срез съел до фильтра).
func DedupeAll(in []Result) []Result {
	return dedupeUpTo(in, 0)
}

// dedupeUpTo - общий проход дедупа; limit<=0 означает «не резать».
//
// Дедуп идёт по двум ключам. Первый - нормализованный адрес: снимает
// www/хвост-слэш/трекинг-параметры. Второй - сигнатура зеркала листинга:
// совпадение источника, пути и сниппета. Каталоги-агрегаторы
// публикуются на нескольких onion-зеркалах с дословно одинаковой
// выдачей, и движок честно приносит все копии: живой прогон этапа 182
// (BEFORE, «forum», deep) дал две записи path=/forum/ с идентичным
// сниппетом «OnionDir - DeepLink ✔, Porn ✔, ...» на разных хостах -
// рекламная прокладка каталога, размноженная зеркалами. Сигнатура без
// адреса хоста ловит именно этот случай, не задевая разные сайты с
// одинаковыми путями: у них описания различаются, а совпадение
// источника отсекает ложные пары между движками.
//
// Пустой сниппет в сигнатуру не входит: без описания совпадение
// слишком слабое, и разные страницы одного каталога с одинаковым путём
// задваивались бы без вины.
func dedupeUpTo(in []Result, limit int) []Result {
	limitless := limit <= 0
	seen := map[string]bool{}
	seenMirror := map[string]bool{}
	out := make([]Result, 0, len(in))
	for _, r := range in {
		key := NormalizeURL(r.URL)
		if key == "" || seen[key] {
			continue
		}
		if m := mirrorKey(r); m != "" {
			if seenMirror[m] {
				continue
			}
			seenMirror[m] = true
		}
		seen[key] = true
		r.Onion = r.Onion || IsOnion(r.URL)
		r.Rank = len(out) + 1
		out = append(out, r)
		if !limitless && len(out) >= limit {
			break
		}
	}
	return out
}

// mirrorKey - сигнатура зеркала листинга: источник, нормализованный путь и
// сниппет. Пустая строка - сигнатура не определена, дедуп по ней не идёт.
// Короткие обрывки отсеивает общий порог minSnippetRunes (см. snippet.go).
func mirrorKey(r Result) string {
	s := strings.TrimSpace(r.Snippet)
	if s == "" || len([]rune(s)) < minSnippetRunes {
		return ""
	}
	path := r.URL
	if i := strings.Index(path, "://"); i >= 0 {
		path = path[i+3:]
	}
	if i := strings.Index(path, "/"); i >= 0 {
		path = strings.ToLower(path[i:])
	} else {
		path = "/"
	}
	path = strings.TrimRight(path, "/")
	if path == "" {
		path = "/"
	}
	return r.Source + "|" + path + "|" + s
}

func NormalizeURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	u.Fragment = ""
	host := strings.ToLower(u.Host)
	host = strings.TrimPrefix(host, "www.")
	path := u.Path
	if path == "/" {
		path = ""
	}
	return u.Scheme + "://" + host + path + queryOf(u)
}

func queryOf(u *url.URL) string {
	q := u.Query()
	if len(q) == 0 {
		return ""
	}
	q.Del("utm_source")
	q.Del("utm_medium")
	q.Del("utm_campaign")
	q.Del("utm_term")
	q.Del("utm_content")
	q.Del("fbclid")
	q.Del("gclid")
	if len(q) == 0 {
		return ""
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	sb.WriteByte('?')
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte('&')
		}
		sb.WriteString(url.QueryEscape(k))
		sb.WriteByte('=')
		sb.WriteString(url.QueryEscape(q.Get(k)))
	}
	return sb.String()
}

func ResolveRedirect(raw string) string {
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "//") {
		raw = "https:" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, "duckduckgo.com") {
		if v := u.Query().Get("uddg"); v != "" {
			if dec, derr := url.QueryUnescape(v); derr == nil {
				return dec
			}
			return v
		}
	}
	if strings.Contains(host, "google.") && strings.HasPrefix(u.Path, "/url") {
		if v := u.Query().Get("q"); v != "" {
			return v
		}
	}
	if strings.Contains(host, "l.facebook.com") {
		if v := u.Query().Get("u"); v != "" {
			return v
		}
	}
	return raw
}

type Cache struct {
	mu sync.Mutex
	m  map[string][]Result
}

func NewCache() *Cache { return &Cache{m: map[string][]Result{}} }

func (c *Cache) Get(key string) ([]Result, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[key]
	return v, ok
}

func (c *Cache) Put(key string, v []Result) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[key] = v
}
