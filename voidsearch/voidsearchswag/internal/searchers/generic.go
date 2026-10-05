package searchers

import (
	"net/url"
	"strings"

	"github.com/PuerkitoBio/goquery"
)

var noiseHosts = []string{
	"duckduckgo.com", "google.com", "google.", "bing.com", "yandex.",
	"facebook.com", "twitter.com", "x.com", "instagram.com",
	"doubleclick.net", "googlesyndication", "adservice",
	"schema.org", "w3.org", "gstatic.com",
}

func ParseGenericHTML(body []byte, source string) ([]Result, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	var out []Result
	seen := map[string]bool{}

	doc.Find("a[href]").Each(func(_ int, a *goquery.Selection) {
		href := attrOr(a, "href")
		if href == "" || strings.HasPrefix(href, "#") || strings.HasPrefix(href, "javascript:") {
			return
		}
		href = ResolveRedirect(href)
		u, perr := url.Parse(href)
		if perr != nil || u.Host == "" {
			return
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return
		}
		host := strings.ToLower(u.Hostname())
		if isNoiseHost(host) {
			return
		}
		text := collapse(a.Text())
		if len(text) < 3 {
			return
		}
		key := NormalizeURL(href)
		if key == "" || seen[key] {
			return
		}
		seen[key] = true
		// Заголовок и сниппет берутся через общую точку: раньше здесь стоял
		// только a.Text(), поэтому универсальный парсер не заполнял сниппет, а
		// ссылка с адресом вместо текста давала бесполезный заголовок.
		title, snippet := resultText(a, href)
		if len([]rune(collapse(title))) < 3 {
			return
		}
		out = append(out, Result{
			Title:   title,
			URL:     href,
			Snippet: snippet,
			Source:  source,
			Onion:   strings.HasSuffix(host, ".onion"),
		})
	})

	// Пустая выдача - не отказ парсера: та же семантика, что у parseOnion.
	// Прежний errNoLinks превращал «страница разобрана, ничего не
	// подошло» в fail движка, и репорт называл живой поиск упавшим.
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func isNoiseHost(host string) bool {
	for _, n := range noiseHosts {
		if strings.Contains(host, n) {
			return true
		}
	}
	return false
}
