package searchers

import (
	"strings"
	"testing"
)

// Строки ниже взяты из живого прогона deep-режима, а не придуманы: именно их
// движки отдали в качестве описания результата.
const (
	// Шапка и навигация поисковика, выданная за описание страницы.
	liveNavBoilerplate = "Results for search query: BOOK /// Search engine for searching hidden services on the TOR network. Search For: Marketplaces, Drugs, Porn, Adult, Cryptocurrency,..."
	// Футерный призыв со ссылкой на сам сайт.
	liveFooterBoilerplate = "Read more in our article on deepweb4wt3m4dhutpxpe7d7wxdftfdf4hhag4sizgon6th5lcefloid.onion"
)

func TestIsBoilerplateSnippetLiveSamples(t *testing.T) {
	for _, s := range []string{liveNavBoilerplate, liveFooterBoilerplate} {
		if !isBoilerplateSnippet(s) {
			t.Errorf("boilerplate из живого прогона не распознан: %q", s)
		}
	}
}

func TestIsBoilerplateSnippetCategories(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		// Навигация и меню.
		{"Search For: Marketplaces, Drugs, Porn", true},
		{"Search engine for searching hidden services", true},
		{"Results for search query: book", true},
		{"Skip to content Main navigation", true},
		{"Toggle navigation Menu Home About", true},
		{"Main menu: home, about, contact", true},
		// Футер и самопрезентация.
		{"All rights reserved 2024", true},
		{"Copyright Some Site", true},
		{"See our Privacy Policy for details", true},
		{"Terms of Service apply", true},
		{"Terms and conditions of use", true},
		{"Read our cookie policy", true},
		{"Powered by WordPress", true},
		{"Learn more about us here", true},
		// Призывы к действию и реклама.
		{"Subscribe to our newsletter", true},
		{"Sign up for free account", true},
		{"Follow us on Twitter", true},
		{"Share this page with friends", true},
		{"Add to cart and checkout", true},
		{"Buy now with discount", true},
		{"Click here to continue", true},

		// Настоящие описания не должны отсеиваться.
		{"Электронная библиотека, книги и статьи по разным темам", false},
		{"I need to find an academic Brazilian book called Métodos", false},
		{"Create a Flip Book for any product category with custom posts", false},
		{"Каталог научных статей и учебников в открытом доступе", false},
		{"Форум вопросов и ответов про тёмную сеть", false},
	}
	for _, c := range cases {
		if got := isBoilerplateSnippet(c.text); got != c.want {
			t.Errorf("isBoilerplateSnippet(%q) = %v, ожидала %v", c.text, got, c.want)
		}
	}
}

func TestIsBoilerplateCaseInsensitive(t *testing.T) {
	// Классы и текст приходят в разном регистре, поэтому сравнение обязано быть
	// регистронезависимым.
	for _, s := range []string{
		"SEARCH FOR: markets",
		"Search For: markets",
		"search for: markets",
		"ALL RIGHTS RESERVED",
		"Powered By Something",
	} {
		if !isBoilerplateSnippet(s) {
			t.Errorf("регистр помешал распознаванию: %q", s)
		}
	}
}

// Разметка, в которой контейнер описания содержит навигацию движка. Так выглядел
// живой случай: класс description есть, а внутри шапка поисковика.
//
// Имя источника и хост намеренно не пересекаются с torch/tor66/tornet/ahmia:
// dropEngineNoise отбрасывает результат как self-ссылку, если хост содержит имя
// движка, и тест проверял бы не фильтр сниппетов, а шум движка. Строка
// boilerplate при этом сохранена дословно из живого прогона.
const navBoilerplateMarkup = `
<div class="result">
  <a href="http://zq7m3xvf2klnprg4wbqjz5t6yvwxhc3kqsg2jzbcnw4vavsa7rwjz2id.onion/s/book">BOOK - search in TORNET</a>
  <div class="description">Results for search query: BOOK /// Search engine for searching hidden services on the TOR network. Search For: Marketplaces, Drugs, Porn</div>
</div>`

func TestParseOnionRejectsNavigationAsSnippet(t *testing.T) {
	// Главный дефект: такой сниппет хуже пустого. Он выглядит как описание,
	// пользователь читает его и делает вывод о содержании страницы, которого
	// там нет. Плюс score начисляет баллы за найденные токены, поэтому
	// перечисление категорий («Marketplaces, Drugs, Porn, Cryptocurrency»)
	// искусственно поднимало нерелевантный результат по любому из этих слов.
	res, err := parseOnion([]byte(navBoilerplateMarkup), "sample-engine", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 {
		t.Fatalf("найдено %d", len(res))
	}
	if res[0].Snippet != "" {
		t.Errorf("навигация принята за сниппет: %q", res[0].Snippet)
	}
	// Заголовок при этом обязан остаться: фильтр бьёт только по описанию.
	if res[0].Title != "BOOK - search in TORNET" {
		t.Errorf("заголовок потерян: %q", res[0].Title)
	}
}

const footerBoilerplateMarkup = `
<div class="result">
  <a href="http://deepweb4wt3m4dhutpxpe7d7wxdftfdf4hhag4sizgon6th5lcefloid.onion/catalog/item">About Comic Book Library / DeepWeb</a>
  <div class="snippet">Read more in our article on deepweb4wt3m4dhutpxpe7d7wxdftfdf4hhag4sizgon6th5lcefloid.onion</div>
</div>`

func TestParseOnionRejectsFooterCallToAction(t *testing.T) {
	res, err := parseOnion([]byte(footerBoilerplateMarkup), "deepweb", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 {
		t.Fatalf("найдено %d", len(res))
	}
	if res[0].Snippet != "" {
		t.Errorf("футерный призыв принят за сниппет: %q", res[0].Snippet)
	}
}

func TestParseOnionKeepsRealDescriptionAlongsideBoilerplate(t *testing.T) {
	// Контейнер содержит и навигацию, и настоящее описание. Перебор обязан
	// продолжаться после отвергнутого кандидата и найти второе: остановка на
	// первом совпадении класса вернула бы пустоту там, где описание есть.
	markup := `
<div class="result">
  <a href="http://abc.onion/page">Название сервиса</a>
  <div class="description">Search For: Marketplaces, Drugs, Porn, Adult</div>
  <div class="snippet">Каталог научных статей и учебников в открытом доступе</div>
</div>`
	res, err := parseOnion([]byte(markup), "engine", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 {
		t.Fatalf("найдено %d", len(res))
	}
	if !strings.Contains(res[0].Snippet, "Каталог научных статей") {
		t.Errorf("настоящее описание пропущено из-за соседней навигации: %q", res[0].Snippet)
	}
}

func TestAcceptSnippetRejectsShortText(t *testing.T) {
	// Одно-два слова описанием не являются: это огрызок заголовка или метка
	// элемента разметки.
	for _, s := range []string{"Home", "Read more", "Search", "Docs", "О нас", "Меню", "ab"} {
		if got := acceptSnippet(s); got != "" {
			t.Errorf("короткий текст %q принят как сниппет: %q", s, got)
		}
	}
}

func TestAcceptSnippetKeepsRealDescriptions(t *testing.T) {
	// Порог не должен отсекать настоящие описания, в том числе русские:
	// кириллица короче по символам при той же смысловой нагрузке.
	for _, s := range []string{
		"Электронная библиотека",
		"Каталог книг и статей",
		"Free ebook download library",
		"Архив документов и отчётов",
	} {
		if got := acceptSnippet(s); got == "" {
			t.Errorf("настоящее описание отвергнуто: %q", s)
		}
	}
}

func TestAcceptSnippetTrimsAndCaps(t *testing.T) {
	if got := acceptSnippet("   Каталог книг и статей   "); got != "Каталог книг и статей" {
		t.Errorf("пробелы не убраны: %q", got)
	}
	long := strings.Repeat("слово ", 200)
	got := acceptSnippet(long)
	if n := len([]rune(got)); n > maxSnippetRunes {
		t.Errorf("длина %d больше предела %d", n, maxSnippetRunes)
	}
	if got == "" {
		t.Error("длинный текст отвергнут целиком вместо обрезки")
	}
}

func TestAcceptSnippetEmptyAndNilSafe(t *testing.T) {
	for _, s := range []string{"", "   ", "\t\n"} {
		if got := acceptSnippet(s); got != "" {
			t.Errorf("пустой ввод %q дал %q", s, got)
		}
	}
}

func TestBoilerplateFilterAppliedOnAllPaths(t *testing.T) {
	// Фильтр обязан работать на всех четырёх шагах extractSnippet, а не только
	// на одном: иначе результат зависел от того, какой шаг сработал первым.
	cases := []struct {
		name    string
		markup  string
		wantEmp bool
	}{
		{
			name:    "data-атрибут с навигацией",
			markup:  `<div><a href="http://abc.onion/p" data-description="Search For: Marketplaces, Drugs, Porn, Adult content">Сервис</a></div>`,
			wantEmp: true,
		},
		{
			name:    "блок описания с футером",
			markup:  `<div class="result"><a href="http://abc.onion/p">Сервис</a><div class="description">All rights reserved by the site owners</div></div>`,
			wantEmp: true,
		},
		{
			name:    "текст контейнера с навигацией",
			markup:  `<div class="result"><a href="http://abc.onion/p">Сервис</a><span>Powered by WordPress theme</span></div>`,
			wantEmp: true,
		},
		{
			name:    "настоящее описание проходит",
			markup:  `<div class="result"><a href="http://abc.onion/p">Сервис</a><div class="description">Каталог научных статей и учебников</div></div>`,
			wantEmp: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, err := parseOnion([]byte(c.markup), "engine", "")
			if err != nil {
				t.Fatal(err)
			}
			if len(res) != 1 {
				t.Fatalf("найдено %d", len(res))
			}
			if c.wantEmp && res[0].Snippet != "" {
				t.Errorf("boilerplate прошёл: %q", res[0].Snippet)
			}
			if !c.wantEmp && res[0].Snippet == "" {
				t.Error("настоящее описание отвергнуто")
			}
			if isBoilerplateSnippet(res[0].Snippet) {
				t.Errorf("в результате остался boilerplate: %q", res[0].Snippet)
			}
		})
	}
}

func TestBoilerplateDoesNotInflateRelevanceScore(t *testing.T) {
	// Конечный смысл фильтра: score начисляет балл за каждый токен запроса,
	// найденный в сниппете. Навигация с перечислением категорий поднимала
	// нерелевантный результат по любому из этих слов.
	toks := queryTokens("drugs marketplace")
	withBoilerplate := Result{
		Title:   "Случайный форум",
		URL:     "http://abc.onion/forum",
		Snippet: "Search For: Marketplaces, Drugs, Porn, Adult, Cryptocurrency",
	}
	withoutBoilerplate := Result{
		Title:   "Случайный форум",
		URL:     "http://abc.onion/forum",
		Snippet: "",
	}
	inflated := score(withBoilerplate, toks, "fast")
	honest := score(withoutBoilerplate, toks, "fast")
	if inflated <= honest {
		t.Errorf("оценка не показывает разницу: %v против %v", inflated, honest)
	}
	// После фильтра сниппет пустой, поэтому оценка не завышается.
	if got := acceptSnippet(withBoilerplate.Snippet); got != "" {
		t.Errorf("фильтр пропустил навигацию: %q", got)
	}
}
