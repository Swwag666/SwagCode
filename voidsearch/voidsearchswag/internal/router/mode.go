package router

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

type Mode string

const (
	ModeAuto    Mode = "auto"
	ModeFast    Mode = "fast"
	ModeStealth Mode = "stealth"
	ModeDeep    Mode = "deep"
)

type Decision struct {
	Mode     Mode     `json:"mode"`
	Reason   string   `json:"reason"`
	Fallback []Mode   `json:"fallback"`
	Signals  []string `json:"signals,omitempty"`
}

// Сигналы разбиты на три класса, потому что наивный strings.Contains ломает
// роутинг в обе стороны.
//
// Ложные срабатывания. "лог " совпадает внутри "диалог" и "монолог", "слив" -
// внутри "сливы" и "сливочное", "паст" - внутри "паста", "тори" - внутри
// "виктория". Запрос про пасту карбонара уходил в deep, то есть в onion-поиск
// через tor: в разы медленнее и с выдачей не про еду.
//
// Ложные пропуски. Сигнал "база слита" - это буквальная фраза с фиксированным
// порядком слов, поэтому "слитая база пользователей" не совпадала и запрос про
// утечку уходил в fast.
//
// words - целое слово, границы проверяются с обеих сторон: "tor" не совпадёт
// внутри "motor", "log" - внутри "dialog".
//
// stems - начало слова, граница только слева. Нужны потому, что русская
// морфология меняет окончание: "утечка", "утечки", "утечек" все начинаются с
// "утечк". Левая граница обязательна - именно она отсекает "диалог".
//
// raw - буквальная подстрока без проверки границ. Только для токенов, которые
// и так неоднозначными быть не могут: поисковые операторы с двоеточием,
// ".onion", "cve-".
type signalSet struct {
	words []string
	stems []string
	raw   []string
}

var deepSignals = signalSet{
	words: []string{
		"onion", "darknet", "tor", "tori", "leak", "leaks", "leaked", "dump",
		"dumps", "combo", "combos", "creds", "credential", "credentials",
		"shodan", "exploit", "exploits", "pastebin", "paste", "breach",
		"breaches",
		// "слив" обязан быть целым словом, а не основой. Как основа он
		// совпадает со "сливы", "сливочное", "сливки", и рецепт со сливами
		// уходил в onion-поиск. С проверкой правой границы "сливами" больше не
		// матчится, а отдельное "скачать базу слив" матчится как раньше.
		"слив",
		// То же про логи. Основой "лог" брать нельзя: она совпадает с "логика"
		// и "логический". Формы множественного числа однозначны.
		"логи", "логов", "логам", "логами",
	},
	// "паст" убран насовсем: и основой, и словом он совпадает с "паста" и
	// "пастель". Смысл перехвачен словом "pastebin".
	stems: []string{
		"даркнет", "утечк", "слит", "тор-браузер", "тор браузер", "луков",
		"слив баз", "слив данны", "слив логов", "сливы данны", "слив кредов",
	},
	raw: []string{".onion", "cve-", "db dump", "база слита", "базы слиты"},
}

var stealthSignals = signalSet{
	words: []string{
		"facebook", "instagram", "linkedin", "tiktok", "amazon", "avito",
		"ozon", "wildberries", "kleinanzeigen", "booking", "airbnb",
		"cloudflare",
	},
	stems: []string{"капч", "captcha", "антибот", "antibot"},
}

var fastSignals = signalSet{
	words: []string{"news", "weather", "wikipedia"},
	stems: []string{"новост", "погод", "википеди", "определени", "что такое", "как работает"},
	raw:   nil,
}

var operatorSignals = signalSet{
	raw: []string{
		"site:", "inurl:", "intitle:", "filetype:", "cache:", "related:",
	},
}

func Route(query string) Decision {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return Decision{Mode: ModeFast, Reason: "пустой запрос -> быстрый режим", Fallback: []Mode{ModeStealth}}
	}

	signals := matchSignals(q, deepSignals)
	if len(signals) > 0 {
		return Decision{
			Mode:     ModeDeep,
			Reason:   "признаки onion/утечек: " + strings.Join(signals, ", "),
			Fallback: []Mode{ModeFast, ModeStealth},
			Signals:  signals,
		}
	}

	if ops := matchSignals(q, operatorSignals); len(ops) > 0 {
		return Decision{
			Mode:     ModeFast,
			Reason:   "поисковые операторы: " + strings.Join(ops, ", "),
			Fallback: []Mode{ModeStealth, ModeDeep},
			Signals:  ops,
		}
	}

	if s := matchSignals(q, stealthSignals); len(s) > 0 {
		return Decision{
			Mode:     ModeStealth,
			Reason:   "домен с известной антибот-защитой: " + strings.Join(s, ", "),
			Fallback: []Mode{ModeFast, ModeDeep},
			Signals:  s,
		}
	}

	if f := matchSignals(q, fastSignals); len(f) > 0 {
		return Decision{
			Mode:     ModeFast,
			Reason:   "быстрый информационный запрос: " + strings.Join(f, ", "),
			Fallback: []Mode{ModeStealth, ModeDeep},
			Signals:  f,
		}
	}

	return Decision{
		Mode:     ModeFast,
		Reason:   "явных признаков нет -> быстрый режим по умолчанию",
		Fallback: []Mode{ModeStealth, ModeDeep},
	}
}

func Parse(raw string) (Mode, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "auto":
		return ModeAuto, true
	case "fast":
		return ModeFast, true
	case "stealth":
		return ModeStealth, true
	case "deep", "tor", "onion":
		return ModeDeep, true
	default:
		return "", false
	}
}

func FallbacksFor(m Mode) []Mode {
	switch m {
	case ModeFast:
		return []Mode{ModeStealth, ModeDeep}
	case ModeStealth:
		return []Mode{ModeFast, ModeDeep}
	case ModeDeep:
		return []Mode{ModeFast, ModeStealth}
	default:
		return []Mode{ModeFast, ModeStealth, ModeDeep}
	}
}

func matchSignals(q string, set signalSet) []string {
	var hits []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		for _, h := range hits {
			if h == s {
				return
			}
		}
		hits = append(hits, s)
	}
	// Порядок проверки - от самого однозначного к самому рискованному: сырые
	// подстроки (операторы с двоеточием, ".onion") ложных срабатываний не
	// дают, целые слова строже основ, а основы строже свободного текста.
	for _, s := range set.raw {
		if len(hits) >= maxSignals {
			return hits
		}
		if strings.Contains(q, s) {
			add(s)
		}
	}
	for _, s := range set.words {
		if len(hits) >= maxSignals {
			return hits
		}
		if scanSignal(q, s, true) {
			add(s)
		}
	}
	for _, s := range set.stems {
		if len(hits) >= maxSignals {
			return hits
		}
		if scanSignal(q, s, false) {
			add(s)
		}
	}
	return hits
}

// maxSignals ограничивает список признаков в решении. Больше четырёх всё равно
// не читаются, а перебирать весь список на каждом запросе незачем.
const maxSignals = 4

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// scanSignal ищет sig в q и проверяет границы вхождения по Unicode-буквам и
// цифрам.
//
// Граница слева обязательна всегда: именно она не даёт "диалог" совпасть с
// "лог", а "виктория" - с "тори". Граница справа нужна только для целых слов,
// потому что у основы окончание меняется: "утечк" обязана находить и "утечка",
// и "утечки", и "утечек".
//
// Проверяются все вхождения, а не первое. Иначе запрос вида "паста и cve-"
// споткнулся бы о безобидное "паста" в начале и не дошёл до настоящего
// признака.
func scanSignal(q, sig string, wholeWord bool) bool {
	if sig == "" || len(sig) > len(q) {
		return false
	}
	for off := 0; off+len(sig) <= len(q); {
		i := strings.Index(q[off:], sig)
		if i < 0 {
			return false
		}
		start := off + i
		end := start + len(sig)

		leftOK := true
		if start > 0 {
			r, _ := utf8.DecodeLastRuneInString(q[:start])
			leftOK = !isWordRune(r)
		}
		rightOK := true
		if wholeWord && end < len(q) {
			r, _ := utf8.DecodeRuneInString(q[end:])
			rightOK = !isWordRune(r)
		}
		if leftOK && rightOK {
			return true
		}
		// Следующее вхождение ищем со сдвигом на один байт, а не на длину
		// сигнала: вхождения могут перекрываться.
		off = start + 1
	}
	return false
}
