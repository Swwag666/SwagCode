package searchers

import (
	"strings"
	"testing"
)

// Страница Ahmia без результатов: селектор .result h4 a не находит ничего, но на
// странице есть навигация и футер движка. Именно такой ответ живого прогона
// deep-режима превращался в «результаты».
const ahmiaNoResultsMarkup = `
<html><body>
<div class="nav">
  <a href="https://www.torproject.org/projects/torbrowser.html">Tor browser bundle</a>
  <a href="https://github.com/ahmia/ahmia-site">contribute to the source code</a>
</div>
<div class="main">
  <a href="http://juhanurmihxlp77nkq76byazcldy2hlmovfu2epvl5ankdibsot4csyd.onion/">Ahmia</a>
  <p>No results found for your query.</p>
</div>
<div class="footer">
  <a href="https://www.torproject.org/">The Tor Project</a>
</div>
</body></html>`

func TestParseOnionEmptySelectorResultIsNotHomepage(t *testing.T) {
	// Главный дефект: пустой результат селектора означает «по запросу ничего
	// нет», а не «селектор сломался». Откат к универсальному обходу превращал
	// честный пустой ответ в набор ссылок с главной страницы движка.
	// Этап 182: пустой ответ - успех без результатов, а не ошибка.
	res, err := parseOnion([]byte(ahmiaNoResultsMarkup), "ahmia", ".result h4 a")
	if err != nil {
		t.Fatalf("пустой ответ движка - не отказ: %v", err)
	}
	if len(res) != 0 {
		t.Fatalf("навигация движка подменила пустой ответ: %d результатов", len(res))
	}
}

func TestParseOnionHomepageLinksNotReturnedWithSelector(t *testing.T) {
	// Даже если ошибка не проверена, ссылки с главной не должны попасть в выдачу.
	// Проверяется отдельно, потому что вызывающий может решить игнорировать
	// ошибку и работать с частичным результатом.
	res, _ := parseOnion([]byte(ahmiaNoResultsMarkup), "ahmia", ".result h4 a")
	for _, r := range res {
		if strings.Contains(r.URL, "torproject.org") || strings.Contains(r.URL, "github.com") {
			t.Errorf("навигация движка попала в выдачу: %s", r.URL)
		}
		if strings.Contains(r.Title, "Tor browser bundle") || strings.Contains(r.Title, "The Tor Project") {
			t.Errorf("заголовок навигации в выдаче: %q", r.Title)
		}
	}
}

func TestParseOnionWithoutSelectorStillUsesGenericScan(t *testing.T) {
	// Обратная сторона: без селектора универсальный обход - единственный способ
	// что-либо извлечь, поэтому он обязан работать. Это осознанная эвристика, а
	// не подмена пустого ответа.
	res, err := parseOnion([]byte(ahmiaNoResultsMarkup), "unknown-engine", "")
	if err != nil {
		t.Fatalf("универсальный обход не сработал: %v", err)
	}
	if len(res) == 0 {
		t.Fatal("без селектора не найдено ничего")
	}
	// Фильтр шума движка по-прежнему обязан работать: nav и footer отсеиваются.
	for _, r := range res {
		if strings.Contains(r.Title, "Tor browser bundle") {
			t.Errorf("навигация не отсеяна фильтром шума: %q", r.Title)
		}
	}
}

func TestParseOnionSelectorMatchWins(t *testing.T) {
	// Когда селектор находит результаты, универсальный обход не подключается
	// вовсе: иначе к настоящей выдаче примешивалась бы навигация страницы.
	markup := `
<html><body>
<div class="nav"><a href="https://www.torproject.org/">The Tor Project</a></div>
<div class="result"><h4><a href="http://zlibrary.onion/">Z-Library</a></h4>
  <div class="description">Электронная библиотека книг и статей</div></div>
<div class="result"><h4><a href="http://libgen.onion/">Library Genesis</a></h4>
  <div class="description">Каталог научных статей и учебников</div></div>
</body></html>`

	res, err := parseOnion([]byte(markup), "ahmia", ".result h4 a")
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 {
		t.Fatalf("найдено %d, ожидала 2", len(res))
	}
	for _, r := range res {
		if strings.Contains(r.URL, "torproject.org") {
			t.Errorf("навигация примешана к настоящей выдаче: %s", r.URL)
		}
	}
	if res[0].Title != "Z-Library" || res[1].Title != "Library Genesis" {
		t.Errorf("заголовки = %q, %q", res[0].Title, res[1].Title)
	}
	if !strings.Contains(res[0].Snippet, "Электронная библиотека") {
		t.Errorf("сниппет потерян: %q", res[0].Snippet)
	}
}

func TestParseOnionPartialSelectorMatchKeepsResults(t *testing.T) {
	// Селектор нашёл часть результатов: этого достаточно, откат не нужен.
	// Иначе к настоящему результату примешалась бы навигация.
	markup := `
<html><body>
<div class="result"><h4><a href="http://real.onion/page">Настоящий результат</a></h4>
  <div class="description">Описание настоящей найденной страницы</div></div>
<div class="nav"><a href="https://www.torproject.org/">The Tor Project</a></div>
</body></html>`

	res, err := parseOnion([]byte(markup), "ahmia", ".result h4 a")
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 {
		t.Fatalf("найдено %d, ожидала 1", len(res))
	}
	if res[0].Title != "Настоящий результат" {
		t.Errorf("заголовок = %q", res[0].Title)
	}
}

func TestParseOnionSelectorMatchesButAllFilteredOut(t *testing.T) {
	// Селектор сработал, но все найденные ссылки отброшены как шум: это тоже
	// пустой ответ, и навигация страницы не должна его заменять.
	markup := `
<html><body>
<div class="result"><h4><a href="/relative/path">Относительная ссылка</a></h4></div>
<div class="nav"><a href="https://www.torproject.org/">The Tor Project</a></div>
</body></html>`

	res, err := parseOnion([]byte(markup), "ahmia", ".result h4 a")
	if err == nil && len(res) > 0 {
		for _, r := range res {
			if strings.Contains(r.URL, "torproject.org") {
				t.Errorf("навигация подменила пустой ответ: %s", r.URL)
			}
		}
	}
}

func TestParseOnionEmptyPageWithSelector(t *testing.T) {
	// Совсем пустая страница с заданным селектором: пустая выдача без
	// ошибки и без паники. Этап 182: страница разобрана, движок ответил -
	// репортится как ok=true count=0.
	res, err := parseOnion([]byte("<html><body></body></html>"), "ahmia", ".result h4 a")
	if err != nil {
		t.Fatalf("пустая страница - не отказ движка: %v", err)
	}
	if len(res) != 0 {
		t.Fatalf("пустая страница отдала результаты: %d", len(res))
	}
}

func TestParseOnionGarbageWithSelector(t *testing.T) {
	// Мусор вместо разметки не должен паниковать. Этап 182: разбора без
	// ошибки достаточно - пустая выдача, остальное не страшно.
	res, err := parseOnion([]byte("это не html вообще"), "ahmia", ".result h4 a")
	if err != nil {
		t.Fatalf("мусор не должен падать ошибкой: %v", err)
	}
	if len(res) != 0 {
		t.Fatalf("мусор дал результаты: %d", len(res))
	}
}
