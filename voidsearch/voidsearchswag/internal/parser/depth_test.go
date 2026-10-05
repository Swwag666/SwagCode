package parser

import (
	"strings"
	"testing"
)

// TestSkeletonOfSurvivesDeepNesting фиксирует реальное поведение на глубокой
// разметке, измеренное, а не предполагаемое.
//
// Первоначальная гипотеза была такой: html.Parse итеративен, поэтому вход из
// миллионов вложенных div даёт столько же кадров стека, а переполнение стека в
// Go не перехватывается через recover(). Проверка это опровергла: у html.Parse
// есть собственный предел в 512 открытых элементов, и на более глубоком входе
// он возвращает ошибку "open stack of elements exceeds 512 nodes". Дерева на
// миллионы узлов не существует, значит и переполнения стека здесь быть не
// могло.
//
// Зато вскрылся настоящий дефект: потолок обхода в 256 отрезал содержимое
// страниц глубиной от 256 до 512, которые парсер обрабатывает нормально. На
// глубине 300 листовой узел исчезал из скелета, а скелет используется для
// генерации селекторов. Потолок поднят до 512 - ровно предел парсера, поэтому
// он теперь страховка, а не потеря данных.
func TestSkeletonOfSurvivesDeepNesting(t *testing.T) {
	// Глубина за пределом html.Parse: парсер отказывает, обход не запускается.
	// Функция обязана вернуть управление, а не упасть.
	body := strings.Repeat("<div>", 5000) + `<a href="/leaf">лист</a>` + strings.Repeat("</div>", 5000)
	got := SkeletonOf(body, 0)
	// Пустой результат здесь означает, что html.Parse вернул ошибку. Это
	// отдельный недостаток диагностики, но не падение и не зависание.
	t.Logf("глубина 5000: скелет %d байт", len(got))

	// Глубина внутри предела парсера: всё содержимое обязано дойти до скелета.
	shallow := strings.Repeat("<div>", 300) + `<a href="/leaf">лист</a>` + strings.Repeat("</div>", 300)
	got = SkeletonOf(shallow, 0)
	if got == "" {
		t.Fatal("скелет пуст на разметке глубиной 300, которую парсер принимает")
	}
	if !strings.Contains(got, "лист") {
		t.Error("потолок обхода отрезал листовой узел на глубине 300")
	}
	if !strings.Contains(got, "div") {
		t.Error("в скелете нет тегов div")
	}
}

func TestSkeletonDepthMatchesParserLimit(t *testing.T) {
	// Потолок обхода обязан быть не меньше предела html.Parse, иначе обход
	// отбрасывает узлы, которые парсер успешно построил. Это и есть регресс,
	// найденный измерением: 256 < 512 теряло данные на глубине 300.
	if maxSkeletonDepth < 512 {
		t.Errorf("maxSkeletonDepth=%d меньше предела html.Parse (512): обход теряет узлы", maxSkeletonDepth)
	}
}

func TestSkeletonOfKeepsNormalDepthIntact(t *testing.T) {
	// Обычная страница не должна пострадать от потолка.
	body := `<html><body><div class="wrap"><div id="inner">` +
		`<a href="/f.zip">файл</a></div></div></body></html>`
	got := SkeletonOf(body, 0)
	if !strings.Contains(got, "#inner") {
		t.Errorf("id не попал в скелет: %q", got)
	}
	if !strings.Contains(got, ".wrap") {
		t.Errorf("класс не попал в скелет: %q", got)
	}
	if !strings.Contains(got, "файл") {
		t.Errorf("текст ссылки потерян: %q", got)
	}
}

func TestSkeletonOfMaxCharsStopsEarly(t *testing.T) {
	// Ранняя проверка maxChars стоит после рекурсивного вызова, поэтому без
	// явного потолка глубокая цепочка успевала спуститься до конца раньше
	// любого ограничения размера. Проверяем, что ограничение всё же срабатывает.
	body := strings.Repeat(`<div class="x">`, 400)
	got := SkeletonOf(body, 200)
	if len(got) > 200 {
		t.Errorf("скелет %d байт при maxChars=200", len(got))
	}
}
