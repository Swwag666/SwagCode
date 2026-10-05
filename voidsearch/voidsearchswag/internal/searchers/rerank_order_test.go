package searchers

import (
	"fmt"
	"testing"
)

// TestRerankIndependentOfInputOrder проверяет, что порядок результатов не
// зависит от того, в каком порядке движки ответили.
//
// Прежняя версия начисляла штраф за однообразие источника в порядке
// поступления, то есть в порядке, который при параллельном опросе задаёт
// гонка. Какие результаты окажутся «четвёртыми и дальше», зависело от случая,
// и один и тот же запрос ранжировался по-разному между запусками. В живых
// прогонах это выглядело как недетерминированное число результатов: 6, затем
// 5, затем снова 6.
func TestRerankIndependentOfInputOrder(t *testing.T) {
	// Счётчики намеренно все различны. Проверять порядок на равных счётах
	// бессмысленно: сортировка стабильная и на равенстве обязана сохранять
	// порядок поступления, поэтому обратный вход закономерно даёт обратный
	// порядок внутри равной группы.
	//
	// Токены запроса - «квантовые» и «компьютеры»: заголовок даёт +3 за токен,
	// сниппет +1, url +1, наличие сниппета +1. Отсюда 9, 7, 6, 5, 3, 1, 0.
	base := []Result{
		{URL: "https://a.example/1", Title: "квантовые компьютеры", Snippet: "квантовые компьютеры", Source: "a"}, // 9
		{URL: "https://a.example/квантовые-2", Title: "квантовые компьютеры", Source: "a"},                        // 7
		{URL: "https://a.example/3", Title: "квантовые компьютеры", Source: "a"},                                  // 6
		{URL: "https://a.example/4", Title: "квантовые", Snippet: "компьютеры", Source: "a"},                      // 5
		{URL: "https://a.example/5", Title: "квантовые", Source: "a"},                                             // 3
		{URL: "https://b.example/1", Title: "иное", Snippet: "квантовые", Source: "b"},                            // 2
		{URL: "https://b.example/2", Title: "постороннее", Source: "b"},                                           // 0
	}

	first := Rerank(append([]Result{}, base...), "квантовые компьютеры", "fast")
	if len(first) != len(base) {
		t.Fatalf("потеряны результаты: %d из %d", len(first), len(base))
	}

	rev := make([]Result, 0, len(base))
	for i := len(base) - 1; i >= 0; i-- {
		rev = append(rev, base[i])
	}
	second := Rerank(rev, "квантовые компьютеры", "fast")

	if len(second) != len(first) {
		t.Fatalf("разное число результатов: %d и %d", len(first), len(second))
	}
	for i := range first {
		if first[i].URL != second[i].URL {
			t.Errorf("позиция %d: %s против %s - ранжирование зависит от порядка поступления",
				i, first[i].URL, second[i].URL)
		}
	}
}

// TestRerankDiversityPenaltyDoesNotDependOnArrival проверяет именно то, что
// было сломано: штраф за однообразие начислялся в порядке поступления, поэтому
// при параллельном опросе «четвёртыми и дальше» оказывались случайные
// результаты - те, чей движок ответил позже, а не те, что менее релевантны.
func TestRerankDiversityPenaltyDoesNotDependOnArrival(t *testing.T) {
	relevant := Result{
		URL: "https://a.example/best", Title: "квантовые компьютеры",
		Snippet: "квантовые компьютеры подробно", Source: "a",
	}
	filler := make([]Result, 0, 6)
	for i := 0; i < 6; i++ {
		filler = append(filler, Result{
			URL: fmt.Sprintf("https://b.example/%d", i), Title: "посторонняя тема", Source: "b",
		})
	}

	// Релевантный результат первым.
	forward := append([]Result{relevant}, filler...)
	// Он же последним - как если бы движок a ответил позже всех.
	backward := append(append([]Result{}, filler...), relevant)

	gotForward := Rerank(forward, "квантовые компьютеры", "fast")
	gotBackward := Rerank(backward, "квантовые компьютеры", "fast")

	if gotForward[0].URL != relevant.URL {
		t.Errorf("при прямом порядке первым идёт %s", gotForward[0].URL)
	}
	if gotBackward[0].URL != relevant.URL {
		t.Errorf("при обратном порядке релевантный результат потерял первое место: %s. "+
			"Штраф за однообразие зависит от порядка ответа движков", gotBackward[0].URL)
	}
}

func TestRerankRelevantResultsBeatDiversityPenalty(t *testing.T) {
	// Плодовитый источник с точно релевантными результатами не должен
	// проигрывать почти пустому результату другого движка.
	//
	// Прежний штраф был неограничен: двадцатый результат одного движка
	// получал -(20-2)*1.5 = -27, тогда как совпадение заголовка со всеми
	// токенами запроса даёт около +3 за токен. В deep-режиме, где onion-движок
	// легко возвращает полсотни совпадений, все они уходили вниз независимо от
	// релевантности.
	in := make([]Result, 0, 22)
	for i := 0; i < 20; i++ {
		in = append(in, Result{
			URL:     fmt.Sprintf("https://rich.example/%d", i),
			Title:   "квантовые компьютеры",
			Snippet: "квантовые компьютеры подробно",
			Source:  "rich",
		})
	}
	in = append(in,
		Result{URL: "https://poor.example/1", Title: "", Source: "poor"},
		Result{URL: "https://poor.example/2", Title: "", Source: "poor"},
	)

	out := Rerank(in, "квантовые компьютеры", "fast")
	// Релевантные результаты обязаны занять верх, несмотря на то что их много
	// и все они из одного источника.
	for i := 0; i < 3; i++ {
		if out[i].Source != "rich" {
			t.Errorf("позиция %d занята %s вместо релевантного результата", i, out[i].Source)
		}
		if out[i].Title == "" {
			t.Errorf("позиция %d: пустой заголовок всплыл над точным совпадением", i)
		}
	}
}

func TestRerankDiversityStillApplies(t *testing.T) {
	// Обратная сторона: разнообразие остаётся целью. При равной релевантности
	// четвёртый и дальше результаты одного источника должны уступать другим.
	in := []Result{
		{URL: "https://a.example/1", Title: "тема", Source: "a"},
		{URL: "https://a.example/2", Title: "тема", Source: "a"},
		{URL: "https://a.example/3", Title: "тема", Source: "a"},
		{URL: "https://a.example/4", Title: "тема", Source: "a"},
		{URL: "https://b.example/1", Title: "тема", Source: "b"},
	}
	out := Rerank(in, "тема", "fast")
	if out[3].Source != "b" {
		t.Errorf("позиция 4: %s, ожидала другой источник при равной релевантности", out[3].Source)
	}
}

func TestRerankBoostKeysAreCaseInsensitive(t *testing.T) {
	// Хост в бонусах приводится к нижнему регистру и при записи, и при поиске.
	// Раньше значение, переданное вызывающим напрямую, использовалось как есть,
	// поэтому ключ в смешанном регистре никогда не совпадал с поиском и
	// выученный бонус молча не применялся.
	in := []Result{
		{URL: "https://Good.Example/page", Title: "обычный заголовок", Source: "a"},
		{URL: "https://other.example/page", Title: "обычный заголовок", Source: "b"},
	}
	// Ключ заглавными - как если бы он пришёл из внешнего источника.
	boosts := map[string]float64{"GOOD.EXAMPLE": 3}
	out := RerankWithHosts(in, "запрос", "fast", boosts)
	if out[0].URL != "https://Good.Example/page" {
		t.Errorf("бонус не применён к хосту в смешанном регистре: первым идёт %s", out[0].URL)
	}
}

func TestRerankEmptyAndSingle(t *testing.T) {
	if got := Rerank(nil, "q", "fast"); len(got) != 0 {
		t.Errorf("пустой вход дал %d результатов", len(got))
	}
	single := []Result{{URL: "https://a.example/1", Title: "t", Source: "a"}}
	got := Rerank(single, "q", "fast")
	if len(got) != 1 || got[0].Rank != 1 {
		t.Errorf("одиночный результат: %+v", got)
	}
}
