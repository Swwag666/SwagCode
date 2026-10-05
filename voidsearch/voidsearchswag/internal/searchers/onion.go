package searchers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"

	"voidsearchswag/internal/httpc"
	"voidsearchswag/internal/netx"
)

type OnionEngine struct {
	Name_    string
	Base     string
	Path     string
	Selector string
	Client   *httpc.Client
}

func (o *OnionEngine) Name() string { return o.Name_ }

func (o *OnionEngine) Search(ctx context.Context, query string, limit int) ([]Result, error) {
	if o.Client == nil {
		return nil, errors.New("onion: клиент не задан")
	}
	target := o.target(query)
	resp, err := o.Client.Fetch(ctx, httpc.Request{URL: target, Method: http.MethodGet})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", o.Name_, err)
	}
	if !resp.OK() {
		return nil, fmt.Errorf("%s: HTTP %d", o.Name_, resp.Status)
	}
	return parseOnion(resp.Body, o.Name_, o.Selector)
}

func (o *OnionEngine) target(query string) string {
	base := strings.TrimSuffix(o.Base, "/")
	path := o.Path
	if path == "" {
		path = "/search?q={q}"
	}
	if strings.Contains(path, "{q}") {
		return base + strings.ReplaceAll(path, "{q}", url.QueryEscape(query))
	}
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return base + path + sep + "q=" + url.QueryEscape(query)
}

// ProbeURL отдаёт адрес для проверки живости: путь без плейсхолдера,
// потому что пинг не ищет, а лишь проверяет, что движок отвечает.
func (o *OnionEngine) ProbeURL() string {
	base := strings.TrimSuffix(o.Base, "/")
	path := o.Path
	if path == "" {
		return base + "/"
	}
	if i := strings.IndexAny(path, "?"); i >= 0 {
		path = path[:i]
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if path == "/" {
		return base + "/"
	}
	return base + path
}

func parseOnion(body []byte, source, selector string) ([]Result, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("%s: разбор DOM: %w", source, err)
	}
	var out []Result

	if selector != "" {
		doc.Find(selector).Each(func(_ int, a *goquery.Selection) {
			href := attrOr(a, "href")
			if href == "" {
				return
			}
			if !strings.HasPrefix(href, "http") {
				return
			}
			// Флаг onion проставляется в обеих ветках: иначе результат,
			// найденный по селектору движка, выглядит как clearnet, и
			// вызывающий не поймёт, что открывать его нужно через tor.
			title, snippet := resultText(a, href)
			out = append(out, Result{
				Title:   title,
				URL:     href,
				Snippet: snippet,
				Source:  source,
				Onion:   strings.HasSuffix(hostOf(href), ".onion"),
			})
		})
	}

	// Универсальная ветка собирает все внешние ссылки страницы. Она нужна для
	// движков без известного селектора, но не как откат при пустой выдаче.
	//
	// Прежняя версия проваливалась сюда всякий раз, когда селектор не нашёл
	// ничего. Для поисковика пустой результат селектора означает «по запросу
	// ничего нет», а не «селектор сломался», поэтому откат превращал честный
	// пустой ответ в набор ссылок с главной страницы движка. Живой прогон
	// deep-режима дал именно это: Ahmia вернула «Tor browser bundle»
	// (torproject.org), «contribute to the source code» (github.com/ahmia) и
	// «The Tor Project» - собственную навигацию вместо результатов.
	//
	// Хуже того, что выдача нерелевантна: она выглядит как настоящий ответ, и
	// пользователь не может отличить «движок ничего не нашёл» от «движок сломал
	// парсер». Пустой ответ честнее.
	//
	// Откат сохранён только для случая, когда селектор не задан вовсе: там
	// универсальный обход - единственный способ что-либо извлечь, и это
	// осознанная эвристика, а не подмена пустого ответа.
	if len(out) == 0 && selector == "" {
		doc.Find("a[href^=http]").Each(func(_ int, a *goquery.Selection) {
			href := attrOr(a, "href")
			host := hostOf(href)
			if host == "" {
				return
			}
			if isEngineNoise(a, href, source) {
				return
			}
			title, snippet := resultText(a, href)
			out = append(out, Result{
				Title:   title,
				URL:     href,
				Snippet: snippet,
				Source:  source,
				Onion:   strings.HasSuffix(host, ".onion"),
			})
		})
	}

	out = dropEngineNoise(out, source)

	// Пустая выдача - не отказ движка. Прежний код возвращал здесь ошибку
	// «ссылок не найдено», и живой движок, честно не нашедший ничего по
	// запросу, репортился как упавший: смоуки этапов 179-181 стабильно
	// видели ahmia-clear ok=false «ссылок не найдено» за 350мс и tor66
	// ok=false за 8.4с - мост жив, ответил быстро, но читался мёртвым.
	// Пустой список идёт выше и попадает в отчёт как ok=true, count=0:
	// «по запросу ничего нет» и «движок не работает» - противоположные
	// состояния, и путать их нельзя.
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// isEngineNoise отсеивает ссылки самого движка: навигацию, служебные
// страницы и self-ссылки. Без этого движок, чей селектор не сработал,
// отдаёт свою навигацию как результаты поиска.
func isEngineNoise(a *goquery.Selection, href, source string) bool {
	if strings.TrimSpace(a.Text()) == "" {
		return true
	}
	path := href
	if i := strings.Index(path, "://"); i >= 0 {
		path = path[i+3:]
	}
	if i := strings.Index(path, "/"); i >= 0 {
		path = path[i:]
	} else {
		path = "/"
	}
	noisePath := []string{"/about", "/privacy", "/terms", "/contact", "/legal", "/blacklist",
		"/add", "/donate", "/faq", "/help", "/dmca", "/rules", "/serviceinfo", "/advertis",
		"/directory", "/stats", "/login", "/register", "/index.php", "/tags/", "/tag/"}
	for _, n := range noisePath {
		if strings.HasPrefix(strings.ToLower(path), n) {
			return true
		}
	}
	noiseText := []string{"about", "privacy", "terms", "contact", "advertis", "donate",
		"правила", "о сервисе", "добавить", "english", "russian", "login", "sign in",
		"home", "назад", "back", "top", "next", "prev", "add url", "random onion", "fresh onion"}
	tx := strings.ToLower(strings.TrimSpace(a.Text()))
	for _, n := range noiseText {
		if tx == n {
			return true
		}
	}
	host := hostOf(href)
	parent := a.Parent()
	for i := 0; i < 3 && parent.Length() > 0; i++ {
		cls, _ := parent.Attr("class")
		id, _ := parent.Attr("id")
		sig := strings.ToLower(cls + " " + id)
		if strings.Contains(sig, "nav") || strings.Contains(sig, "header") ||
			strings.Contains(sig, "footer") || strings.Contains(sig, "menu") ||
			strings.Contains(sig, "sidebar") || strings.Contains(sig, "breadcrumb") {
			return true
		}
		if strings.Contains(sig, "result") || strings.Contains(sig, "sresult") {
			return false
		}
		parent = parent.Parent()
	}
	if host != "" && strings.Contains(strings.ToLower(source), "tor66") {
		if strings.Contains(href, "/serviceinfo/") {
			return true
		}
	}
	return false
}

func dropEngineNoise(in []Result, source string) []Result {
	selfHost := ""
	switch {
	case strings.Contains(strings.ToLower(source), "torch"):
		selfHost = "torch"
	case strings.Contains(strings.ToLower(source), "tor66"):
		selfHost = "tor66"
	case strings.Contains(strings.ToLower(source), "tornet"):
		selfHost = "tornet"
	case strings.Contains(strings.ToLower(source), "ahmia"):
		selfHost = "ahmia"
	}
	out := make([]Result, 0, len(in))
	for _, r := range in {
		if selfHost != "" && strings.Contains(hostOf(r.URL), selfHost) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// OnionCatalog - живой набор onion-поисковиков.
//
// Каталог меняется во время работы: фоновый тик discover и вызов инструмента
// promote_engines поднимают в него новые движки, а поисковые запросы из
// goroutine MCP-сессий его читают. Поэтому доступ к списку идёт через mu.
//
// Поле Engines остаётся экспортированным ради сборки каталога на старте и
// тестов, но прямой доступ к нему безопасен только до запуска сервера, пока
// нет параллельных читателей и писателей. В рантайме обязательны Snapshot для
// чтения и AttachEngine для записи.
type OnionCatalog struct {
	Engines []*OnionEngine
	Log     netx.Logger

	mu sync.RWMutex
}

// Snapshot возвращает копию списка движков для безопасного обхода.
//
// Копия снимается один раз, и это важно не только для гонки: SearchAll раньше
// вычислял вместимость канала по len(c.Engines), а затем ещё раз обходил тот же
// срез. Если между двумя чтениями список вырастал, цикл приёма ждал больше
// элементов, чем было отправлено, и блокировался на <-ch навсегда.
func (c *OnionCatalog) Snapshot() []*OnionEngine {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.Engines) == 0 {
		return nil
	}
	out := make([]*OnionEngine, len(c.Engines))
	copy(out, c.Engines)
	return out
}

// AttachEngine добавляет движок, если его ещё нет по имени, и сообщает,
// добавлен ли он.
//
// Проверка дубликата и добавление выполняются под одной блокировкой. Прежняя
// версия в promote.Attach делала их раздельно: два одновременных вызова могли
// оба пройти проверку и оба добавить один и тот же движок.
func (c *OnionCatalog) AttachEngine(eng *OnionEngine) bool {
	if c == nil || eng == nil {
		return false
	}
	name := eng.Name()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.Engines {
		if e.Name() == name {
			return false
		}
	}
	c.Engines = append(c.Engines, eng)
	return true
}

func (c *OnionCatalog) SearchAll(ctx context.Context, query string, limit int) []Result {
	type item struct {
		res []Result
	}
	engines := c.Snapshot()
	if len(engines) == 0 {
		return nil
	}
	ch := make(chan item, len(engines))
	for _, e := range engines {
		go func(e *OnionEngine) {
			ectx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()
			res, err := e.Search(ectx, query, limit)
			if err != nil {
				c.logf("%s: %v", e.Name(), err)
				ch <- item{}
				return
			}
			ch <- item{res: res}
		}(e)
	}

	var all []Result
	for range engines {
		it := <-ch
		all = append(all, it.res...)
		if len(all) >= limit*2 {
			break
		}
	}
	return Dedupe(all, limit)
}

func (c *OnionCatalog) logf(format string, args ...any) {
	if c.Log != nil {
		c.Log.Infof(format, args...)
	}
}
