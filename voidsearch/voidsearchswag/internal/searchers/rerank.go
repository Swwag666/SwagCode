package searchers

import (
	"sort"
	"strings"
)

// Rerank сортирует выдачу по релевантности запросу: пересечение токенов
// запроса с заголовком/урлом/сниппетом плюс mode-бонусы. Сортировка
// стабильная: равные по счёту сохраняют исходный порядок движков.
// Rank переназначается после сортировки, как в Dedupe.
func Rerank(in []Result, query, mode string) []Result {
	return RerankWithHosts(in, query, mode, nil)
}

// diversityPenaltyCap ограничивает штраф за однообразие источника.
//
// Без потолка двадцатый результат одного движка получал -(20-2)*1.5 = -27,
// тогда как точное совпадение заголовка со всеми токенами запроса даёт лишь
// около +3 за токен. В deep-режиме, где плодовитый onion-движок легко
// возвращает полсотни совпадений, все они уходили ниже почти пустого
// результата другого движка независимо от релевантности. Задуманное
// «четвёртый и дальше тонут» превращалось в «всё после третьего невидимо».
const diversityPenaltyCap = 6.0

// RerankWithHosts - то же плюс выученные бонусы хостов (таблица relevance):
// хосты, чьи страницы судья оценивал высоко, всплывают. Бонус уже
// клампирован при пересчёте, здесь применяется как есть.
func RerankWithHosts(in []Result, query, mode string, boosts map[string]float64) []Result {
	if len(in) == 0 {
		return in
	}
	toks := queryTokens(query)
	type scored struct {
		r Result
		s float64
	}
	sc := make([]scored, 0, len(in))
	for _, r := range in {
		s := score(r, toks, mode)
		if len(boosts) > 0 {
			// Ключ бонусов приводится к нижнему регистру с обеих сторон.
			// HostQuality получает host через strings.ToLower(pu.Hostname()),
			// но значение, переданное вызывающим напрямую, использовалось как
			// есть, поэтому хост в смешанном регистре никогда не совпадал с
			// этим поиском и выученный бонус молча не применялся.
			if b, ok := boosts[strings.ToLower(hostOf(r.URL))]; ok {
				s += b
			}
		}
		sc = append(sc, scored{r: r, s: s})
	}

	// Первый проход - чистая релевантность. Штраф за однообразие источника
	// обязан считаться после неё, а не до.
	//
	// Прежняя версия начисляла штраф в порядке поступления результатов, то
	// есть в порядке, в котором движки случайно ответили при параллельном
	// опросе. Какие именно результаты окажутся «четвёртыми и дальше»,
	// зависело от гонки, а не от релевантности, и один и тот же запрос ранжировался
	// по-разному между запусками. В живых прогонах это выглядело как
	// недетерминированное число результатов: 6, затем 5, затем снова 6.
	sort.SliceStable(sc, func(i, j int) bool { return sc[i].s > sc[j].s })

	// Второй проход - разнообразие по уже отсортированному порядку.
	seenSource := map[string]int{}
	for i := range sc {
		if n := seenSource[sc[i].r.Source]; n >= 3 {
			pen := float64(n-2) * 1.5
			if pen > diversityPenaltyCap {
				pen = diversityPenaltyCap
			}
			sc[i].s -= pen
		}
		seenSource[sc[i].r.Source]++
	}
	sort.SliceStable(sc, func(i, j int) bool { return sc[i].s > sc[j].s })

	out := make([]Result, 0, len(in))
	for i, s := range sc {
		s.r.Rank = i + 1
		out = append(out, s.r)
	}
	return out
}

func queryTokens(query string) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range strings.Fields(strings.ToLower(query)) {
		t = strings.Trim(t, `"'.,:;!?()[]`)
		if len([]rune(t)) < 2 || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

func score(r Result, toks []string, mode string) float64 {
	var s float64
	title := strings.ToLower(r.Title)
	snip := strings.ToLower(r.Snippet)
	url := strings.ToLower(r.URL)
	for _, t := range toks {
		if strings.Contains(title, t) {
			s += 3
		}
		if strings.Contains(snip, t) {
			s += 1
		}
		if strings.Contains(url, t) {
			s += 1
		}
	}
	// Onion-результат в deep-режиме - то, за чем пришли: без бонуса
	// clearnet-зеркала с набитым SEO вытесняют его из топа.
	if strings.ToLower(mode) == "deep" && (r.Onion || IsOnion(r.URL)) {
		s += 5
	}
	if strings.TrimSpace(r.Title) == "" {
		s -= 2
	}
	if strings.TrimSpace(r.Snippet) != "" {
		s += 1
	}
	return s
}
