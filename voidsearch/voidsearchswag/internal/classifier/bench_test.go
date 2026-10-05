package classifier

import (
	"strings"
	"testing"
)

// bigPage собирает тело страницы указанного размера. Маркеры распределены по
// всему объёму, а не собраны в начале: иначе замер показывал бы стоимость
// короткого текста, а не реальной страницы.
func bigPage(size int) string {
	var b strings.Builder
	chunk := `<div class="row"><p>Обычный текст страницы без особых маркеров, просто наполнение объёма.</p></div>`
	for b.Len() < size {
		b.WriteString(chunk)
	}
	s := b.String()[:size]
	// Маркеры в середину и в конец: проверяем полный проход, а не ранний выход.
	return s
}

func BenchmarkClassifySmallPage(b *testing.B) {
	page := bigPage(4096)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Classify("http://example.onion/x", "Заголовок", "сниппет", page)
	}
}

func BenchmarkClassifyMediumPage(b *testing.B) {
	page := bigPage(512 * 1024)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Classify("http://example.onion/x", "Заголовок", "сниппет", page)
	}
}

func BenchmarkClassifyLargePage(b *testing.B) {
	page := bigPage(12 * 1024 * 1024)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Classify("http://example.onion/x", "Заголовок", "сниппет", page)
	}
}

// BenchmarkClassifyLargePageWithCrypto - худший случай: маркер денег есть,
// поэтому scamCheck не выходит рано и проверяются все списки обещаний и фармы.
func BenchmarkClassifyLargePageWithCrypto(b *testing.B) {
	page := "crypto investment " + bigPage(12*1024*1024)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Classify("http://example.onion/x", "Заголовок", "сниппет", page)
	}
}

// Эталонные замеры прежней реализации: classifyReference определена в
// marker_test.go и повторяет старый код дословно. Сравнение в одном прогоне
// убирает разброс между запусками, который на коротких текстах сравним с
// самим выигрышем.
func BenchmarkClassifyReferenceSmallPage(b *testing.B) {
	page := bigPage(4096)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		classifyReference("http://example.onion/x", "Заголовок", "сниппет", page)
	}
}

func BenchmarkClassifyReferenceMediumPage(b *testing.B) {
	page := bigPage(512 * 1024)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		classifyReference("http://example.onion/x", "Заголовок", "сниппет", page)
	}
}

func BenchmarkClassifyReferenceLargePage(b *testing.B) {
	page := bigPage(12 * 1024 * 1024)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		classifyReference("http://example.onion/x", "Заголовок", "сниппет", page)
	}
}

func BenchmarkClassifyReferenceLargePageWithCrypto(b *testing.B) {
	page := "crypto investment " + bigPage(12*1024*1024)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		classifyReference("http://example.onion/x", "Заголовок", "сниппет", page)
	}
}

// BenchmarkScanMarkersCost отделяет стоимость поиска маркеров от подготовки
// текста: автомат против прежнего цикла strings.Contains на одном и том же
// теле.
func BenchmarkScanMarkersCost(b *testing.B) {
	page := strings.ToLower(bigPage(12 * 1024 * 1024))
	b.Run("aho-corasick", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			h := scanMarkers(page)
			if len(h) != len(allMarkers) {
				b.Fatal("неверный размер набора")
			}
		}
	})
	b.Run("contains loop", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			n := 0
			for _, m := range allMarkers {
				if strings.Contains(page, m) {
					n++
				}
			}
			if n > len(allMarkers) {
				b.Fatal("невозможно")
			}
		}
	})
}

// BenchmarkBuildTextCost отделяет стоимость подготовки текста.
func BenchmarkBuildTextCost(b *testing.B) {
	hint := bigPage(12 * 1024 * 1024)
	b.Run("builder", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			t := buildText("Заголовок", "сниппет", hint)
			if len(t) == 0 {
				b.Fatal("пусто")
			}
		}
	})
	b.Run("join tolower", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			t := strings.ToLower(strings.Join([]string{"Заголовок", "сниппет", hint}, "\n"))
			if len(t) == 0 {
				b.Fatal("пусто")
			}
		}
	})
}

// BenchmarkRuneCountCost измеряет стоимость именно len([]rune(text)) в
// popularity: на 12 МБ это полное декодирование UTF-8 ради сравнения с порогом
// 1000.
func BenchmarkRuneCountCost(b *testing.B) {
	text := bigPage(12 * 1024 * 1024)
	b.Run("len([]rune)", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			n := len([]rune(text))
			if n < 0 {
				b.Fatal("невозможно")
			}
		}
	})
	b.Run("bounded count", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			n := boundedRuneCount(text, 1001)
			if n < 0 {
				b.Fatal("невозможно")
			}
		}
	})
}

// BenchmarkToLowerJoin измеряет стоимость подготовки текста: strings.Join трёх
// частей и последующий strings.ToLower дают две полные копии тела.
func BenchmarkToLowerJoin(b *testing.B) {
	title := "Заголовок страницы"
	snippet := "Короткий сниппет"
	hint := bigPage(12 * 1024 * 1024)
	b.Run("join then tolower", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			t := strings.ToLower(strings.Join([]string{title, snippet, hint}, "\n"))
			if len(t) == 0 {
				b.Fatal("пусто")
			}
		}
	})
	b.Run("builder lowercasing", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			var sb strings.Builder
			sb.Grow(len(title) + len(snippet) + len(hint) + 2)
			sb.WriteString(strings.ToLower(title))
			sb.WriteString("\n")
			sb.WriteString(strings.ToLower(snippet))
			sb.WriteString("\n")
			sb.WriteString(strings.ToLower(hint))
			if sb.Len() == 0 {
				b.Fatal("пусто")
			}
		}
	})
}
