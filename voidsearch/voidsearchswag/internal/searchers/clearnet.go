package searchers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/PuerkitoBio/goquery"

	"voidsearchswag/internal/httpc"
	"voidsearchswag/internal/netx"
)

type DuckDuckGo struct {
	Client *httpc.Client
	Log    netx.Logger
	Lite   bool
}

func (d *DuckDuckGo) Name() string {
	if d.Lite {
		return "ddg-lite"
	}
	return "ddg-html"
}

func (d *DuckDuckGo) endpoint(query string) string {
	if d.Lite {
		return "https://lite.duckduckgo.com/lite/?q=" + url.QueryEscape(query)
	}
	return "https://html.duckduckgo.com/html/?q=" + url.QueryEscape(query)
}

// Search запрашивает выдачу DuckDuckGo и возвращает не больше limit записей.
//
// Параметр limit раньше принимался и полностью игнорировался: parseDDG
// возвращал всё, что нашёл на странице. Формально это работало, потому что
// вызывающий всё равно обрезал результат в Dedupe, но делало параметр
// бессмысленным именно здесь и мешало понять, где настоящий потолок выдачи.
//
// Потолок задаёт сам DuckDuckGo. Эндпоинты html и lite отдают фиксированную
// страницу без параметра числа результатов и без пагинации, поэтому запросить
// больше, чем движок вернул, невозможно в принципе. Обрезка до limit здесь -
// не способ получить больше, а способ честно соблюсти контракт метода.
func (d *DuckDuckGo) Search(ctx context.Context, query string, limit int) ([]Result, error) {
	if d.Client == nil {
		return nil, errors.New("ddg: клиент не задан")
	}
	resp, err := d.Client.Fetch(ctx, httpc.Request{
		URL:    d.endpoint(query),
		Method: http.MethodGet,
	})
	if err != nil {
		return nil, fmt.Errorf("ddg: %w", err)
	}
	if err := httpc.Classify(resp); err != nil {
		return nil, err
	}
	if !resp.OK() {
		return nil, fmt.Errorf("ddg: HTTP %d", resp.Status)
	}
	out, err := parseDDG(resp.Body, d.Name())
	if err != nil {
		return nil, err
	}
	return clampResults(out, limit), nil
}

// clampResults обрезает выдачу до limit записей.
//
// Вынесена отдельно потому, что эндпоинт DuckDuckGo зашит в код и направить
// Search на тестовый сервер нельзя, поэтому проверка обрезки возможна только
// на чистой функции. limit <= 0 означает «не ограничивать»: так вызывающий
// может запросить всё, что отдал движок.
//
// Обрезка идёт по порядку, в котором результаты пришли со страницы, - то есть
// по порядку самого движка. Переставлять их здесь нельзя: ранжированием
// занимается RerankWithHosts, и локальная сортировка внутри одного движка
// перечёркивала бы его собственную оценку релевантности.
func clampResults(in []Result, limit int) []Result {
	if limit > 0 && len(in) > limit {
		return in[:limit]
	}
	return in
}

func parseDDG(body []byte, source string) ([]Result, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("ddg: разбор DOM: %w", err)
	}
	var out []Result

	doc.Find("div.result, div.web-result").Each(func(_ int, sel *goquery.Selection) {
		a := sel.Find("a.result__a").First()
		if a.Length() == 0 {
			a = sel.Find("h2 a").First()
		}
		if a.Length() == 0 {
			return
		}
		href, ok := a.Attr("href")
		if !ok {
			return
		}
		href = ResolveRedirect(href)
		if href == "" || !strings.HasPrefix(href, "http") {
			return
		}
		title := collapse(a.Text())
		snippet := collapse(sel.Find(".result__snippet").First().Text())
		if snippet == "" {
			snippet = collapse(sel.Find("a.result__snippet").First().Text())
		}
		out = append(out, Result{Title: title, URL: href, Snippet: snippet, Source: source})
	})

	if len(out) == 0 {
		rows := doc.Find("table tr")
		rows.Each(func(i int, tr *goquery.Selection) {
			a := tr.Find("a.result-link").First()
			if a.Length() == 0 {
				return
			}
			href := attrOr(a, "href")
			if href == "" || !strings.HasPrefix(href, "http") {
				return
			}
			snippet := ""
			if next := rows.Eq(i + 1); next.Length() > 0 {
				snippet = collapse(next.Find("td.result-snippet").First().Text())
			}
			out = append(out, Result{
				Title:   collapse(a.Text()),
				URL:     ResolveRedirect(href),
				Snippet: snippet,
				Source:  source,
			})
		})
	}

	// Проверка антибот-заглушки идёт до фолбэков, а не после.
	//
	// Прежний порядок делал её мёртвым кодом: третий фолбэк собирал вообще все
	// внешние ссылки страницы без фильтрации шума, а страница аномалии или
	// капчи всегда содержит какие-то внешние ссылки - справку, юридические
	// страницы, домен вендора. Фолбэк срабатывал, isDDGChallenge не вызывался
	// никогда, и мусорные ссылки навигации возвращались как успешный поиск.
	// Дальше Engine.Search писал их в кэш на весь CacheTTL (час по умолчанию),
	// поэтому мимолётный rate-limit отравлял этот запрос на час, и вызывающий
	// не имел способа это обнаружить.
	if len(out) == 0 && isDDGChallenge(body) {
		return nil, errors.New("ddg: антибот-заглушка вместо выдачи")
	}

	if len(out) == 0 {
		doc.Find("a[href^=http]").Each(func(_ int, a *goquery.Selection) {
			href := attrOr(a, "href")
			host := hostOf(href)
			if host == "" || strings.Contains(host, "duckduckgo.com") {
				return
			}
			// Тот же фильтр шума, что и в parseOnion. Без него сюда попадали
			// «About», «Privacy», «Settings» и прочая навигация - ссылки,
			// которые выглядят как результаты и портят выдачу.
			if isEngineNoise(a, href, source) {
				return
			}
			out = append(out, Result{
				Title:  collapse(a.Text()),
				URL:    href,
				Source: source,
			})
		})
	}

	if len(out) == 0 {
		return nil, errors.New("ddg: разметка не распознана")
	}
	return out, nil
}

// isDDGChallenge распознаёт страницу антибот-проверки вместо выдачи.
//
// Проверяется только начало тела: страница капчи показывает свой текст сразу,
// а приводить к нижнему регистру 12 МБ ради поиска подстроки дорого и делается
// на каждый пустой результат. Прежняя версия приводила всё тело целиком.
//
// «challenge» отдельным маркером убран: это обычное слово в разметке и текстах
// DuckDuckGo, и оно давало ложные срабатывания на честной пустой выдаче.
// Оставлены специфические фразы страницы аномалии.
func isDDGChallenge(body []byte) bool {
	const headLen = 64 << 10
	head := body
	if len(head) > headLen {
		head = head[:headLen]
	}
	s := strings.ToLower(string(head))
	return strings.Contains(s, "anomaly") ||
		strings.Contains(s, "unusual traffic") ||
		strings.Contains(s, "not a robot") ||
		strings.Contains(s, "are you a human") ||
		strings.Contains(s, "captcha")
}

func attrOr(sel *goquery.Selection, name string) string {
	v, _ := sel.Attr(name)
	return strings.TrimSpace(v)
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

func collapse(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// parseBing разбирает выдачу Bing: li.b_algo > h2 > a, сниппет из .b_caption.
// Используется только браузерным движком: настоящий Chromium с JS получает
// полную разметку, а голый HTTP - пустую bot-оболочку без результатов
// (проверено: HTTP 200, 0 результатов; Mojeek с той же сети - 403,
// Marginalia - обрыв соединения). Поэтому третьего HTTP-движка нет осознанно:
// fast = пара DDG, второй индекс = только рендер через браузер.
func parseBing(body []byte, source string) ([]Result, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("bing: разбор DOM: %w", err)
	}
	var out []Result
	seen := map[string]bool{}

	add := func(title, href, snippet string) {
		href = ResolveRedirect(strings.TrimSpace(href))
		u, perr := url.Parse(href)
		if perr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return
		}
		if strings.Contains(strings.ToLower(u.Hostname()), "bing.com") ||
			strings.Contains(strings.ToLower(u.Hostname()), "microsoft.com") {
			return
		}
		key := NormalizeURL(href)
		if key == "" || seen[key] {
			return
		}
		seen[key] = true
		out = append(out, Result{Title: collapse(title), URL: href, Snippet: collapse(snippet), Source: source})
	}

	doc.Find("li.b_algo").Each(func(_ int, li *goquery.Selection) {
		a := li.Find("h2 a").First()
		if a.Length() == 0 {
			return
		}
		href, _ := a.Attr("href")
		snippet := li.Find("div.b_caption p").First().Text()
		if snippet == "" {
			snippet = li.Find("p").First().Text()
		}
		add(a.Text(), href, snippet)
	})

	if len(out) == 0 {
		return nil, errors.New("bing: разметка не распознана")
	}
	return out, nil
}
