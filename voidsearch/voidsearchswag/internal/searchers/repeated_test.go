package searchers

import (
	"strings"
	"testing"
)

// Живой прогон deep-режима: Ahmia при пустой выдаче отдала ссылки со своей
// главной, и описание у всех четырёх результатов было дословно одинаковым -
// шапка сайта. Заголовки ниже взяты из того же прогона.
func ahmiaHomepageResults() []Result {
	shared := "Ahmia searches hidden services on the Tor network. To access these hidden services, you need the Tor browser bundle. Abuse material is not allowed on Ahmia. See..."
	return []Result{
		{Title: "(без названия)", URL: "http://juhanurmihxlp77nkq76byazcldy2hlmovfu2epvl5ankdibsot4csyd.onion/", Snippet: shared, Source: "ahmia"},
		{Title: "Tor browser bundle", URL: "https://www.torproject.org/projects/torbrowser.html", Snippet: shared, Source: "ahmia"},
		{Title: "contribute to the source code", URL: "https://github.com/ahmia/ahmia-site", Snippet: shared, Source: "ahmia"},
		{Title: "The Tor Project", URL: "https://www.torproject.org/", Snippet: shared, Source: "ahmia"},
	}
}

func TestDropRepeatedSnippetsRemovesSiteChrome(t *testing.T) {
	// Главный сценарий: один источник, один и тот же сниппет у всех результатов.
	// Это шапка сайта, а не описания разных страниц.
	in := ahmiaHomepageResults()
	got := DropRepeatedSnippets(in)

	if len(got) != len(in) {
		t.Fatalf("число результатов изменилось: %d вместо %d", len(got), len(in))
	}
	for i, r := range got {
		if r.Snippet != "" {
			t.Errorf("[%d] шапка сайта осталась в сниппете: %q", i, r.Snippet)
		}
		// Заголовки и адреса обязаны уцелеть: обнуляется только ложное описание.
		if r.Title != in[i].Title {
			t.Errorf("[%d] заголовок изменён: %q", i, r.Title)
		}
		if r.URL != in[i].URL {
			t.Errorf("[%d] адрес изменён: %q", i, r.URL)
		}
	}
}

func TestDropRepeatedSnippetsKeepsDistinctSnippets(t *testing.T) {
	// Настоящая выдача: описания различаются, потому что взяты с разных страниц.
	in := []Result{
		{Title: "A", URL: "http://a.onion/1", Snippet: "Электронная библиотека книг", Source: "ahmia"},
		{Title: "B", URL: "http://b.onion/2", Snippet: "Каталог научных статей", Source: "ahmia"},
		{Title: "C", URL: "http://c.onion/3", Snippet: "Архив документов и отчётов", Source: "ahmia"},
	}
	got := DropRepeatedSnippets(in)
	for i, r := range got {
		if r.Snippet != in[i].Snippet {
			t.Errorf("[%d] различающийся сниппет обнулён: %q", i, r.Snippet)
		}
	}
}

func TestDropRepeatedSnippetsScopedPerSource(t *testing.T) {
	// Проверка идёт по источнику, а не по всей выдаче: разные движки законно
	// могут дать пересекающийся текст, и это не шапка.
	shared := "Общее описание для двух разных движков"
	in := []Result{
		{Title: "A", URL: "http://a.onion/1", Snippet: shared, Source: "ahmia"},
		{Title: "B", URL: "http://b.onion/2", Snippet: shared, Source: "torch"},
		{Title: "C", URL: "http://c.onion/3", Snippet: "Своё описание у torch", Source: "torch"},
	}
	got := DropRepeatedSnippets(in)
	for i, r := range got {
		if r.Snippet != in[i].Snippet {
			t.Errorf("[%d] сниппет обнулён при пересечении между источниками: %q", i, r.Snippet)
		}
	}
}

func TestDropRepeatedSnippetsSingleResultKept(t *testing.T) {
	// Единственный результат источника не обнуляется: повторять нечего, и
	// описание может быть настоящим.
	in := []Result{
		{Title: "A", URL: "http://a.onion/1", Snippet: "Единственное описание страницы", Source: "ahmia"},
	}
	got := DropRepeatedSnippets(in)
	if got[0].Snippet != in[0].Snippet {
		t.Errorf("сниппет единственного результата обнулён: %q", got[0].Snippet)
	}
}

func TestDropRepeatedSnippetsPartialOverlapKept(t *testing.T) {
	// Два результата из трёх делят сниппет, но не все: это не шапка сайта, а
	// случайное совпадение описаний, поэтому обнулять нельзя.
	shared := "Описание, которое встретилось дважды"
	in := []Result{
		{Title: "A", URL: "http://a.onion/1", Snippet: shared, Source: "ahmia"},
		{Title: "B", URL: "http://b.onion/2", Snippet: shared, Source: "ahmia"},
		{Title: "C", URL: "http://c.onion/3", Snippet: "Совершенно другое описание", Source: "ahmia"},
	}
	got := DropRepeatedSnippets(in)
	for i, r := range got {
		if r.Snippet != in[i].Snippet {
			t.Errorf("[%d] частичное совпадение обнулено: %q", i, r.Snippet)
		}
	}
}

func TestDropRepeatedSnippetsIgnoresEmpty(t *testing.T) {
	// Пустые сниппеты не считаются: иначе источник, у которого все результаты
	// без описания, выглядел бы как «все делят один сниппет».
	in := []Result{
		{Title: "A", URL: "http://a.onion/1", Snippet: "", Source: "ahmia"},
		{Title: "B", URL: "http://b.onion/2", Snippet: "", Source: "ahmia"},
		{Title: "C", URL: "http://c.onion/3", Snippet: "Настоящее описание страницы", Source: "ahmia"},
	}
	got := DropRepeatedSnippets(in)
	if got[2].Snippet != "Настоящее описание страницы" {
		t.Errorf("единственный непустой сниппет обнулён: %q", got[2].Snippet)
	}
}

func TestDropRepeatedSnippetsWhitespaceVariantsTreatedSame(t *testing.T) {
	// Одно и то же описание с разными пробелами обязано считаться одинаковым:
	// иначе шапка сайта прошла бы фильтр за счёт лишних пробелов в разметке.
	base := "Ahmia searches hidden services on the Tor network"
	in := []Result{
		{Title: "A", URL: "http://a.onion/1", Snippet: base, Source: "ahmia"},
		{Title: "B", URL: "http://b.onion/2", Snippet: "  " + base + "  ", Source: "ahmia"},
		{Title: "C", URL: "http://c.onion/3", Snippet: base + " ", Source: "ahmia"},
	}
	got := DropRepeatedSnippets(in)
	for i, r := range got {
		if strings.TrimSpace(r.Snippet) != "" {
			t.Errorf("[%d] вариант с пробелами не распознан как повтор: %q", i, r.Snippet)
		}
	}
}

func TestDropRepeatedSnippetsDoesNotMutateInput(t *testing.T) {
	// Выдача кэшируется, поэтому мутирование на месте испортило бы кэш.
	in := ahmiaHomepageResults()
	original := in[0].Snippet
	got := DropRepeatedSnippets(in)

	if in[0].Snippet != original {
		t.Errorf("входной срез изменён: %q", in[0].Snippet)
	}
	if got[0].Snippet != "" {
		t.Errorf("результат не обработан: %q", got[0].Snippet)
	}
}

func TestDropRepeatedSnippetsEmptyAndNil(t *testing.T) {
	if got := DropRepeatedSnippets(nil); len(got) != 0 {
		t.Errorf("nil дал %d записей", len(got))
	}
	if got := DropRepeatedSnippets([]Result{}); len(got) != 0 {
		t.Errorf("пустой срез дал %d записей", len(got))
	}
}

func TestAhmiaHomepageBoilerplateIsFiltered(t *testing.T) {
	// Второй слой защиты: самоописание поисковика распознаётся как boilerplate
	// независимо от повторов. Живой прогон показал именно этот текст.
	samples := []string{
		"Ahmia searches hidden services on the Tor network. To access these hidden services, you need the Tor browser bundle.",
		"Abuse material is not allowed on Ahmia. See our service blacklist.",
		"contribute to the source code",
		"You need the Tor browser bundle to access these services",
		"See our service blacklist for details",
	}
	for _, s := range samples {
		if !isBoilerplateSnippet(s) {
			t.Errorf("самоописание движка не распознано: %q", s)
		}
		if got := acceptSnippet(s); got != "" {
			t.Errorf("acceptSnippet пропустил самоописание: %q", got)
		}
	}
}

func TestRealDescriptionsSurviveBoilerplateFilter(t *testing.T) {
	// Обратная сторона: настоящие описания не должны пострадать от маркеров
	// самоописания движка.
	real := []string{
		"Ahmia is a search engine for Tor hidden services with a clean interface",
		"Электронная библиотека, книги и статьи по разным темам",
		"Каталог научных статей и учебников в открытом доступе",
		"Forum for discussing hidden services and Tor network usage",
	}
	// Первый элемент содержит «Ahmia», но это не маркер boilerplate: маркер -
	// «searches hidden services», а здесь «is a search engine».
	for _, s := range real[1:] {
		if isBoilerplateSnippet(s) {
			t.Errorf("настоящее описание принято за boilerplate: %q", s)
		}
	}
}

func TestDropRepeatedSnippetsDoesNotInflateScore(t *testing.T) {
	// Конечный смысл фильтра: score начисляет балл за каждый токен запроса,
	// найденный в сниппете. Шапка сайта с перечислением «hidden services»,
	// «Tor network», «browser» завышала оценку всем четырём результатам сразу.
	toks := queryTokens("tor browser hidden services")
	in := ahmiaHomepageResults()

	before := 0.0
	for _, r := range in {
		before += score(r, toks, "deep")
	}
	got := DropRepeatedSnippets(in)
	after := 0.0
	for _, r := range got {
		after += score(r, toks, "deep")
	}
	if after >= before {
		t.Errorf("оценка не снизилась после отсева шапки: %v -> %v", before, after)
	}
}
