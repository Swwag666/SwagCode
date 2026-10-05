package classifier

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// buildText собирает текст, в котором классификатор ищет маркеры.
//
// Прежняя версия делала strings.ToLower(strings.Join(...)): Join выделял буфер
// на весь объём и копировал части, затем ToLower выделял второй буфер того же
// размера и копировал снова. На теле в 12 МБ это две копии и 295 мс из 337 мс
// всего вызова Classify (замер BenchmarkToLowerJoin). Здесь части приводятся к
// нижнему регистру прямо при записи в один буфер: одна копия, один проход.
//
// Результат побайтово совпадает со strings.ToLower(strings.Join(...)), включая
// поведение на некорректном UTF-8: одиночный байт, который декодируется в
// U+FFFD, записывается как U+FFFD - ровно так же поступает strings.Map, на
// котором построена strings.ToLower.
func buildText(title, snippet, htmlHint string) string {
	var sb strings.Builder
	sb.Grow(len(title) + len(snippet) + len(htmlHint) + 2)
	writeLowered(&sb, title)
	sb.WriteByte('\n')
	writeLowered(&sb, snippet)
	sb.WriteByte('\n')
	writeLowered(&sb, htmlHint)
	return sb.String()
}

// writeLowered пишет s в sb в нижнем регистре. Сравнение идёт по рунам, как в
// strings.Map(unicode.ToLower, s), поэтому греческая сигма, турецкие и прочие
// особые символы обрабатываются тем же правилом, что и раньше. Некорректный
// байт декодируется в U+FFFD и записывается как U+FFFD - ровно так же
// поступает strings.Map внутри strings.ToLower.
//
// Быстрый путь для ASCII-пробегов (побайтовое приведение и сплошное
// копирование кусками) был реализован и отклонён по замеру в одном прогоне на
// теле 4 МБ: на строчном ASCII-тяжёлом тексте он давал 72,1 мс против 84,3 мс,
// на разметке со смешанным регистром 82,0 против 84,3 мс (в пределах шума), а
// на преимущественно кириллическом тексте 81,5 мс против 75,9 мс - то есть
// проигрывал на 7%. Кириллица для этого каталога основной корпус, поэтому
// выигрыш на частном случае не окупает ни проигрыш на основном, ни порционный
// буфер и два прохода вместо одного.
func writeLowered(sb *strings.Builder, s string) {
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		sb.WriteRune(unicode.ToLower(r))
	}
}
