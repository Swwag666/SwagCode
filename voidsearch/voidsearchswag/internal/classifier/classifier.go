// Package classifier - классификатор источников из плана v3 (этап 4).
//
// Правила первыми: тип и качество считаются детерминированно по маркерам
// страницы. LLM-клиент подключается только для неоднозначных случаев -
// сервер отдает сниппет, судит клиент. Поэтому Score всегда сопровождается
// Reason: видно, какое правило сработало.
package classifier

import (
	"net/url"
	"strings"
	"unicode/utf8"
)

// Verdict - итог классификации.
type Verdict struct {
	Type       string  `json:"type"`
	Quality    float64 `json:"quality"`
	Popularity float64 `json:"popularity"`
	Reason     string  `json:"reason"`
}

// Types.
const (
	TypePublic  = "public"
	TypePrivate = "private"
	TypePaid    = "paid"
	TypeScam    = "scam"
)

// popularityRuneCap - предел подсчёта рун для оценки богатства текста. Пороги
// ветвления в popularity: 80, 300 и 1000, поэтому 1001 символа достаточно,
// чтобы выбрать ветку; считать тело в 12 МБ целиком ради этого не нужно.
const popularityRuneCap = 1001

// Classify разбирает источник по правилам. url обязателен, остальное -
// по наличию: чем больше входных данных, тем точнее вердикт.
//
// Тело страницы может быть большим (до 12 МБ), поэтому подготовка текста и
// поиск маркеров сделаны в один проход: buildText пишет одну копию вместо двух,
// scanMarkers находит все маркеры за один обход вместо 34.
func Classify(rawURL, title, snippet, htmlHint string) Verdict {
	u, _ := url.Parse(strings.TrimSpace(rawURL))
	host := ""
	if u != nil {
		host = strings.ToLower(u.Host)
	}
	text := buildText(title, snippet, htmlHint)
	hits := scanMarkers(text)

	if t, q, reason := scamCheck(hits); t != "" {
		return Verdict{Type: t, Quality: q, Popularity: 0.1, Reason: reason}
	}
	if hasLoginForm(hits) {
		return Verdict{Type: TypePrivate, Quality: 0.7, Popularity: popularity(host, text),
			Reason: "форма входа: password/login/sign-in в разметке или тексте"}
	}
	if hasPaywall(hits) {
		return Verdict{Type: TypePaid, Quality: 0.6, Popularity: popularity(host, text),
			Reason: "paywall-маркеры: подписка/premium/платный доступ"}
	}
	q := 0.5
	reasons := []string{"публичный источник без ограничений"}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(rawURL)), "https://") {
		q += 0.05
	}
	if strings.TrimSpace(title) != "" && strings.TrimSpace(snippet) != "" {
		q += 0.1
	}
	if hasCaptcha(hits) {
		q = min(q, 0.3)
		reasons = []string{"антибот-защита: captcha/challenge - содержимое видно не полностью"}
	}
	if q > 1.0 {
		q = 1.0
	}
	return Verdict{Type: TypePublic, Quality: round1(q),
		Popularity: popularity(host, text), Reason: strings.Join(reasons, "; ")}
}

// hasLoginForm отличает настоящую форму входа от ссылки «войти» в шапке.
func hasLoginForm(h markerHits) bool {
	// Один маркер вида "sign in" встречается и на публичных страницах
	// (ссылка в шапке). Пароль в разметке - уже форма входа.
	for i := groupLogin.lo; i < groupLogin.hi; i++ {
		if !h[i] {
			continue
		}
		if m := allMarkers[i]; m == `type="password"` || m == `type='password'` {
			return true
		}
	}
	return h.count(groupLogin) >= 2
}

func hasPaywall(h markerHits) bool {
	return h.any(groupPaywall)
}

func hasCaptcha(h markerHits) bool {
	return h.any(groupCaptcha)
}

// scamCheck ищет скам. Скам считается только связкой: деньги плюс гарантия или
// удвоение. По одному слову "crypto" или "прибыль" честные биржи и блоги тоже
// содержат.
func scamCheck(h markerHits) (string, float64, string) {
	if !h.any(groupMoney) {
		return "", 0, ""
	}
	if p, ok := h.first(groupPromise); ok {
		return TypeScam, 0.1, "скам-связка: деньги + гарантия/удвоение (" + p + ")"
	}
	if p, ok := h.first(groupPharma); ok {
		return TypeScam, 0.2, "скам-маркер: " + p
	}
	return "", 0, ""
}

func popularity(host, text string) float64 {
	trusted := []string{
		"wikipedia.org", "github.com", "stackoverflow.com", "arxiv.org",
		"python.org", "golang.org", "mozilla.org",
	}
	for _, t := range trusted {
		if strings.Contains(host, t) {
			return 0.9
		}
	}
	// Богатство описания - грубый прокси популярности: у заглушек и
	// дорвеев текста почти нет.
	switch l := boundedRuneCount(text, popularityRuneCap); {
	case l > 1000:
		return 0.6
	case l > 300:
		return 0.5
	case l > 80:
		return 0.4
	default:
		return 0.3
	}
}

// boundedRuneCount считает руны, но останавливается на limit. Прежняя версия
// делала len([]rune(text)): выделения памяти там не происходит (компилятор
// сворачивает такую конструкцию в счётчик), но полное декодирование UTF-8 на
// теле в 12 МБ стоило 34 мс на каждый вызов Classify - замер
// BenchmarkRuneCountCost. С пределом 1001 та же работа стоит 7,6 мкс.
func boundedRuneCount(s string, limit int) int {
	if limit <= 0 {
		return 0
	}
	count := 0
	for i := 0; i < len(s); {
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
		count++
		if count >= limit {
			return limit
		}
	}
	return count
}

func min(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func round1(f float64) float64 {
	return float64(int(f*10+0.5)) / 10
}
