package filex

import (
	"sort"
	"strings"
	"testing"
)

// Инвариант, которого не хватало: всякое расширение, признанное классификатором
// вердиктов настоящей категорией, обязано проходить фильтр содержимого.
//
// Два прежних теста проверяли связь в другую сторону и расхождение поймать не
// могли. TestClassifyFileConsistentWithContentFilter обходит KnownExts и
// проверяет вердикт, поэтому отсутствующего в KnownExts расширения он не видит
// вовсе. TestClassifyFileCoversKnownContentExts проверяет, что fb2 даёт метку
// книги, но не проверяет, что fb2 считается содержимым. В результате файл с
// расширением, которое сам же пакет относит к книгам, отбраковывался как мусор:
// CatalogJunkReason возвращал «расширение не является содержимым», и
// store.CleanFileCatalog удалял такие строки из каталога.

// verdictExtLists возвращает все списки расширений классификатора с названием
// категории. Новая категория обязана появиться здесь, иначе инвариант
// проверяется не по всем спискам - и тест ниже это контролирует.
func verdictExtLists() []struct {
	category string
	list     map[string]bool
} {
	return []struct {
		category string
		list     map[string]bool
	}{
		{"executable", executableExts},
		{"archive", archiveExts},
		{"document", documentExts},
		{"ebook", ebookExts},
		{"media", mediaExts},
		{"data", dataExts},
		{"secret", secretExts},
	}
}

// verdictContentExceptions - расширения, которые классификатор вердиктов
// относит к содержательным категориям, но которые намеренно не пущены в
// KnownExts. Причина описана у самого списка расширений.
//
// Исключения проверяются в обе стороны: они обязаны оставаться исключениями,
// иначе список устареет и тест начнёт пропускать то, что должно падать.
//
// Форматы изображений были здесь до этапа 61 и убраны осознанно: их пустили в
// каталог вместе с правилом отбраковки оформления сайта (IsSiteDecoration),
// которое отличает logo.png и sprite.svg от самостоятельного изображения. Сам
// тест и поймал это изменение - он требует убирать исключение, как только
// расширение становится содержимым.
var verdictContentExceptions = map[string]string{
	// Совпадает с доменной зоной: адрес, напечатанный текстом, стал бы файлом.
	"com": "доменная зона",
}

func TestEveryVerdictExtensionIsContent(t *testing.T) {
	var missing []string
	for _, group := range verdictExtLists() {
		for ext := range group.list {
			// Служебные артефакты намеренно не содержимое: подпись или
			// контрольная сумма без подписанного файла бесполезны. Часть их
			// расширений входит и в категории (pem, key, crt в secret),
			// поэтому они пропускаются.
			if MetadataExts[ext] {
				continue
			}
			if _, exempt := verdictContentExceptions[ext]; exempt {
				continue
			}
			if !IsContent(ext) {
				missing = append(missing, ext+" ("+group.category+")")
			}
		}
	}
	// torrent разбирается в ClassifyFile отдельной веткой и в списки не входит,
	// но содержимым является намеренно - это и есть раздаваемый объект.
	if !IsContent("torrent") {
		missing = append(missing, "torrent (torrent)")
	}

	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("классификатор вердиктов знает эти расширения как содержимое, но фильтр его отбраковывает: %s",
			strings.Join(missing, ", "))
	}
}

// TestVerdictContentExceptions держит список исключений честным: каждое из них
// действительно обязано отбраковываться, а причина - быть непустой. Если
// расширение со временем добавят в KnownExts, тест укажет убрать его из
// исключений, а не молча ослабить инвариант.
func TestVerdictContentExceptions(t *testing.T) {
	for ext, reason := range verdictContentExceptions {
		if strings.TrimSpace(reason) == "" {
			t.Errorf("у исключения %q нет причины", ext)
		}
		if IsContent(ext) {
			t.Errorf("%q больше не является исключением: фильтр считает его содержимым, уберите его из списка исключений", ext)
		}
	}
	// Исключение обязано относиться к категории вердиктов, иначе оно здесь
	// лишнее и только ослабляет инвариант.
	known := map[string]bool{}
	for _, group := range verdictExtLists() {
		for ext := range group.list {
			known[ext] = true
		}
	}
	for ext := range verdictContentExceptions {
		if !known[ext] {
			t.Errorf("исключение %q не значится ни в одной категории вердиктов", ext)
		}
	}
}

// TestVerdictListsAreCompleteGuard защищает сам перечень категорий: если в
// verdict.go появится новый список расширений, а verdictExtLists не будет
// обновлён, инвариант начнёт проверяться не по всем спискам и молча ослабнет.
func TestVerdictListsAreCompleteGuard(t *testing.T) {
	want := map[string]int{
		"executable": len(executableExts),
		"archive":    len(archiveExts),
		"document":   len(documentExts),
		"ebook":      len(ebookExts),
		"media":      len(mediaExts),
		"data":       len(dataExts),
		"secret":     len(secretExts),
	}
	got := map[string]int{}
	for _, group := range verdictExtLists() {
		got[group.category] = len(group.list)
	}
	if len(got) != len(want) {
		t.Errorf("категорий в перечне %d, а списков в классификаторе %d", len(got), len(want))
	}
	for category, n := range want {
		if got[category] != n {
			t.Errorf("категория %s: в перечне %d расширений, в классификаторе %d", category, got[category], n)
		}
	}
}

// TestKnownExtsHaveUsableMime проверяет, что у каждого содержательного
// расширения MIME непустой и похож на тип: значение уходит в каталог и в вывод,
// а пустая строка неотличима от «расширение неизвестно».
func TestKnownExtsHaveUsableMime(t *testing.T) {
	for ext, mime := range KnownExts {
		if strings.TrimSpace(mime) == "" {
			t.Errorf("у %q пустой MIME", ext)
			continue
		}
		if !strings.Contains(mime, "/") {
			t.Errorf("у %q MIME без типа и подтипа: %q", ext, mime)
		}
		if strings.ToLower(mime) != mime {
			t.Errorf("у %q MIME не в нижнем регистре: %q", ext, mime)
		}
	}
}

// TestJunkReasonForRealBookFormats фиксирует конкретный случай, из-за которого
// инвариант добавлен: форматы русскоязычных библиотек не должны отбраковываться.
func TestJunkReasonForRealBookFormats(t *testing.T) {
	for _, tc := range []struct{ ext, name string }{
		{"djvu", "книга.djvu"},
		{"fb2", "книга.fb2"},
		{"azw3", "книга.azw3"},
		{"mobi", "книга.mobi"},
		{"epub", "книга.epub"},
		{"pdf", "книга.pdf"},
	} {
		url := "http://lib.onion/books/" + tc.name
		if reason := CatalogJunkReason(tc.ext, tc.name, url); reason != "" {
			t.Errorf("%s отбракован как мусор: %s", tc.ext, reason)
		}
	}
}
