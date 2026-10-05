package searchers

import (
	"strings"
	"testing"
)

// Разметка Ahmia: контейнер результата с заголовком-ссылкой и отдельным блоком
// описания. Именно такой вид движок отдаёт вживую.
const ahmiaMarkup = `
<div class="result">
  <h3><a href="http://zlibrary24tuxziyiyfr7zd46ytefdqbqd2axkmxm4o5374ptpc52fad.onion/">Z-Library</a></h3>
  <div class="result-body">
    <div class="description">Электронная библиотека, книги и статьи</div>
  </div>
</div>
<div class="result">
  <h3><a href="http://libgenfrialc7tguyjywa36vtrdcplwpxaw43h6o63dmmwhvavo5rqqd.onion/">Library Genesis</a></h3>
  <div class="result-body">
    <div class="description">Научные статьи и учебники</div>
  </div>
</div>`

func TestParseOnionExtractsSnippets(t *testing.T) {
	// Главный дефект: onion-движки не заполняли сниппет вовсе, поэтому в
	// deep-режиме выдача шла без описаний. Это не косметика - score начисляет
	// балл за каждый токен запроса, найденный в сниппете, поэтому результаты
	// onion-движков систематически оценивались ниже clearnet при равной
	// релевантности.
	res, err := parseOnion([]byte(ahmiaMarkup), "ahmia", "h3 a")
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 {
		t.Fatalf("найдено %d, ожидала 2", len(res))
	}
	if res[0].Title != "Z-Library" {
		t.Errorf("заголовок = %q", res[0].Title)
	}
	if !strings.Contains(res[0].Snippet, "Электронная библиотека") {
		t.Errorf("сниппет не извлечён: %q", res[0].Snippet)
	}
	if !strings.Contains(res[1].Snippet, "Научные статьи") {
		t.Errorf("второй сниппет не извлечён: %q", res[1].Snippet)
	}
	// Сниппет не должен повторять заголовок: CLI напечатал бы строку дважды.
	if strings.Contains(res[0].Snippet, res[0].Title) {
		t.Errorf("сниппет дублирует заголовок: %q", res[0].Snippet)
	}
}

// Разметка с описанием в атрибуте data-*, как у части движков, которые прячут
// описание от видимой разметки.
const dataAttrMarkup = `
<div class="row">
  <a href="http://aaaa.onion/docs" data-description="Собрание документов и архивов">docs</a>
</div>`

func TestExtractSnippetFromDataAttribute(t *testing.T) {
	res, err := parseOnion([]byte(dataAttrMarkup), "engine", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 {
		t.Fatalf("найдено %d", len(res))
	}
	if !strings.Contains(res[0].Snippet, "Собрание документов") {
		t.Errorf("описание из data-атрибута не извлечено: %q", res[0].Snippet)
	}
}

// Худший случай: у ссылки видимый текст - сам адрес.
const addressTitleMarkup = `
<div class="result">
  <a href="http://zlibrary24tuxziyiyfr7zd46ytefdqbqd2axkmxm4o5374ptpc52fad.onion/library/books">http://zlibrary24tuxziyiyfr7zd46ytefdqbqd2axkmxm4o5374ptpc52fad.onion/library/books</a>
  <div class="snippet">Каталог книг и учебников</div>
</div>
<div class="result">
  <a href="http://bible4u2lvhacg4b3to2e2veqpwmrc2c3tjf2wuuqiz332vlwmr4xbad.onion/">bible4u2lvhacg4b3to2e2veqpwmrc2c3tjf2wuuqiz332vlwmr4xbad.onion</a>
  <div class="snippet">Религиозные тексты</div>
</div>`

func TestParseOnionReplacesAddressTitle(t *testing.T) {
	// Заголовок-адрес хуже отсутствующего: по нему нельзя искать, а в выдаче он
	// занимает место, где должно быть название сервиса, и выглядит как
	// поломанный парсер.
	res, err := parseOnion([]byte(addressTitleMarkup), "engine", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 {
		t.Fatalf("найдено %d, ожидала 2", len(res))
	}
	for i, r := range res {
		if strings.Contains(strings.ToLower(r.Title), ".onion") {
			t.Errorf("[%d] заголовок остался адресом: %q", i, r.Title)
		}
		if strings.HasPrefix(r.Title, "http") {
			t.Errorf("[%d] заголовок остался URL: %q", i, r.Title)
		}
		if r.Title == "" {
			t.Errorf("[%d] заголовок пуст", i)
		}
	}
	// У первого результата путь заканчивается на books, поэтому осмысленное
	// слово берётся из пути.
	if !strings.Contains(strings.ToLower(res[0].Title), "books") {
		t.Errorf("заголовок не собран из пути: %q", res[0].Title)
	}
	// У второго путь корневой, осмысленного слова взять неоткуда: метка вместо
	// адреса и вместо пустой строки.
	if res[1].Title != untitledLabel {
		t.Errorf("заголовок без пути = %q, ожидала %q", res[1].Title, untitledLabel)
	}
	if !strings.Contains(res[0].Snippet, "Каталог книг") {
		t.Errorf("сниппет потерян: %q", res[0].Snippet)
	}
	if !strings.Contains(res[1].Snippet, "Религиозные тексты") {
		t.Errorf("второй сниппет потерян: %q", res[1].Snippet)
	}
}

func TestMeaningfulTitle(t *testing.T) {
	const onionURL = "http://zlibrary24tuxziyiyfr7zd46ytefdqbqd2axkmxm4o5374ptpc52fad.onion/books"
	cases := []struct {
		name      string
		text      string
		url       string
		wantTitle string
		wantAddr  bool
	}{
		{"обычный заголовок", "Z-Library", onionURL, "Z-Library", false},
		{"русский заголовок", "Библиотека книг", onionURL, "Библиотека книг", false},
		{"текст равен хосту", "zlibrary24tuxziyiyfr7zd46ytefdqbqd2axkmxm4o5374ptpc52fad.onion", onionURL, "", true},
		{"текст равен полному URL", onionURL, onionURL, "", true},
		{"текст равен URL со слэшем", onionURL + "/", onionURL, "", true},
		{"голый адрес без схемы", "abc234567890abcdefghijklmnopqrstuvwxy1234567890abcdef.onion", onionURL, "", true},
		{"http-префикс", "http://example.com/page", "http://example.com/page", "", true},
		{"пустой текст", "", onionURL, "", true},
		{"пробельный текст", "   ", onionURL, "", true},
		{"заголовок с точкой не адрес", "Wiki. The free encyclopedia", "http://example.onion/wiki", "Wiki. The free encyclopedia", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			title, isAddr := meaningfulTitle(c.text, c.url)
			if title != c.wantTitle {
				t.Errorf("title = %q, ожидала %q", title, c.wantTitle)
			}
			if isAddr != c.wantAddr {
				t.Errorf("isAddress = %v, ожидала %v", isAddr, c.wantAddr)
			}
		})
	}
}

func TestMeaningfulTitleKeepsRealTitlesContainingOnion(t *testing.T) {
	// Обратная сторона: заголовок, который честно упоминает .onion, адресом не
	// является. Иначе «Как зайти на .onion сайт» потерял бы заголовок.
	title, isAddr := meaningfulTitle("Как зайти на .onion сайт", "http://example.com/guide")
	if isAddr {
		t.Errorf("настоящий заголовок принят за адрес: %q", title)
	}
	if title != "Как зайти на .onion сайт" {
		t.Errorf("заголовок изменён: %q", title)
	}
}

func TestFallbackTitle(t *testing.T) {
	cases := []struct {
		url  string
		want string
	}{
		{"http://abc.onion/library/books", "books"},
		{"http://abc.onion/library/books/", "books"},
		{"http://abc.onion/science-fiction-novels", "science fiction novels"},
		{"http://abc.onion/download_files", "download files"},
		{"http://abc.onion/search?q=books&page=2", "search"},
		{"http://abc.onion/page#section", "page"},
		{"http://abc.onion/", ""},
		{"http://abc.onion", ""},
		{"http://abc.onion/a", ""},  // сегмент короче трёх рун
		{"http://abc.onion/ab", ""}, // сегмент короче трёх рун
		{"not a url at all", ""},    // нет пути
	}
	for _, c := range cases {
		if got := fallbackTitle(c.url); got != c.want {
			t.Errorf("fallbackTitle(%q) = %q, ожидала %q", c.url, got, c.want)
		}
	}
}

func TestClipRunesDoesNotBreakCyrillic(t *testing.T) {
	// Обрезка по байтам разрезала бы многобайтовую кириллицу посередине
	// символа и давала нечитаемый вывод.
	s := strings.Repeat("я", 10)
	got := clipRunes(s, 5)
	if len([]rune(got)) != 5 {
		t.Errorf("получено %d рун, ожидала 5", len([]rune(got)))
	}
	if got != "яяяяя" {
		t.Errorf("обрезка повредила символы: %q", got)
	}
	for _, r := range got {
		if r != 'я' {
			t.Errorf("символ повреждён: %q", r)
		}
	}
}

func TestClipRunesBoundaries(t *testing.T) {
	if got := clipRunes("abc", 0); got != "" {
		t.Errorf("n=0 дало %q", got)
	}
	if got := clipRunes("abc", -1); got != "" {
		t.Errorf("n<0 дало %q", got)
	}
	if got := clipRunes("abc", 10); got != "abc" {
		t.Errorf("короткая строка изменена: %q", got)
	}
	if got := clipRunes("  abc  ", 10); got != "abc" {
		t.Errorf("пробелы не убраны: %q", got)
	}
	if got := clipRunes("", 5); got != "" {
		t.Errorf("пустая строка дала %q", got)
	}
}

func TestSubtractText(t *testing.T) {
	cases := []struct {
		text, link, want string
	}{
		{"Заголовок Описание результата", "Заголовок", "Описание результата"},
		{"Описание результата Заголовок", "Заголовок", "Описание результата"},
		{"Заголовок", "Заголовок", ""},
		{"Описание", "", "Описание"},
		{"", "Заголовок", ""},
		// Слово заголовка законно встречается и в описании: убирается только
		// первое вхождение.
		{"books about books", "books", "about books"},
	}
	for _, c := range cases {
		if got := subtractText(c.text, c.link); got != c.want {
			t.Errorf("subtractText(%q, %q) = %q, ожидала %q", c.text, c.link, got, c.want)
		}
	}
}

// Разметка без описания: часть движков отдаёт только заголовок и адрес.
const bareMarkup = `
<div><a href="http://abc.onion/page">Название сервиса</a></div>`

func TestParseOnionWithoutSnippetIsNotAnError(t *testing.T) {
	// Пустой сниппет - нормальное состояние, а не ошибка: вызывающий его
	// переживает, score просто не начислит балл.
	res, err := parseOnion([]byte(bareMarkup), "engine", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 {
		t.Fatalf("найдено %d", len(res))
	}
	if res[0].Title != "Название сервиса" {
		t.Errorf("заголовок = %q", res[0].Title)
	}
	if res[0].Snippet != "" {
		t.Errorf("сниппет появился из ничего: %q", res[0].Snippet)
	}
}

func TestSnippetNotDuplicatingTitleInNestedMarkup(t *testing.T) {
	// Вложенная разметка: контейнер .content оборачивает и ссылку, и описание.
	// Без вычитания заголовка сниппет начинался бы с его повтора.
	markup := `
<div class="result">
  <div class="content">
    <a href="http://abc.onion/docs">Документы</a>
    <div class="description">Архив документов и отчётов</div>
  </div>
</div>`
	res, err := parseOnion([]byte(markup), "engine", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 {
		t.Fatalf("найдено %d", len(res))
	}
	if strings.Contains(res[0].Snippet, "Документы Архив") {
		t.Errorf("сниппет начался с заголовка: %q", res[0].Snippet)
	}
	if res[0].Snippet == "" {
		t.Error("сниппет потерян на вложенной разметке")
	}
}

func TestGenericHTMLNowExtractsSnippets(t *testing.T) {
	// Универсальный парсер тоже не заполнял сниппет.
	markup := `
<div class="result">
  <a href="http://example.onion/page">Название</a>
  <div class="snippet">Короткое описание страницы</div>
</div>`
	res, err := ParseGenericHTML([]byte(markup), "generic")
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 {
		t.Fatalf("найдено %d", len(res))
	}
	if !strings.Contains(res[0].Snippet, "Короткое описание") {
		t.Errorf("универсальный парсер не извлёк сниппет: %q", res[0].Snippet)
	}
	if !res[0].Onion {
		t.Error("флаг onion не проставлен")
	}
}

func TestSnippetLengthIsCapped(t *testing.T) {
	// Описание бывает полным абзацем. Ограничение нужно не для вывода (CLI
	// обрезает при печати), а чтобы длинный текст не доминировал в оценке
	// релевантности: score начисляет балл за каждый найденный токен.
	long := strings.Repeat("книга ", 200)
	markup := `<div class="result"><a href="http://abc.onion/p">Сервис</a><div class="description">` + long + `</div></div>`
	res, err := parseOnion([]byte(markup), "engine", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 {
		t.Fatalf("найдено %d", len(res))
	}
	if n := len([]rune(res[0].Snippet)); n > maxSnippetRunes {
		t.Errorf("сниппет %d рун, предел %d", n, maxSnippetRunes)
	}
	if res[0].Snippet == "" {
		t.Error("сниппет потерян")
	}
}
