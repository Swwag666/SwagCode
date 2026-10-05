package filex

import (
	"strings"
	"testing"
)

// TestCatalogJunkReason проверяет единую точку отбраковки записей каталога.
//
// Функция существует потому, что критерии раньше жили в трёх местах независимо
// - в FromURL, в probeFile коллектора и в store.CleanFileCatalog, - и
// расхождение между ними было живым дефектом: FromURL отклонял служебные
// расширения, а probeFile принимал всё из KnownExts, поэтому подписи попадали в
// каталог и тут же вычищались, бесконечно добавляясь заново.
func TestCatalogJunkReason(t *testing.T) {
	cases := []struct {
		ext      string
		name     string
		url      string
		wantJunk bool
		contains string
	}{
		// Служебные расширения.
		{"asc", "key.asc", "http://a.onion/key.asc", true, "служебное расширение"},
		{"sig", "file.sig", "http://a.onion/file.sig", true, "служебное расширение"},
		{"sha256", "x.sha256", "http://a.onion/x.sha256", true, "служебное расширение"},
		{"pem", "ca.pem", "http://a.onion/ca.pem", true, "служебное расширение"},
		// Неизвестное расширение.
		{"onion", "addr.onion", "http://a.onion/addr.onion", true, "не является содержимым"},
		{"php", "s.php", "http://a.onion/s.php", true, "не является содержимым"},
		{"", "noext", "http://a.onion/noext", true, "нет расширения"},
		// Служебные имена с «контентным» расширением.
		{"json", "manifest.json", "http://a.onion/manifest.json", true, "служебное имя"},
		{"txt", "robots.txt", "http://a.onion/robots.txt", true, "служебное имя"},
		{"txt", "LICENSE.txt", "http://a.onion/LICENSE.txt", true, "служебное имя"},
		{"txt", "publicpgp.txt", "http://a.onion/publicpgp.txt", true, "служебное имя"},
		{"xml", "opensearch_html.xml", "http://a.onion/static/opensearch_html.xml", true, "служебное имя"},
		// Эндпоинты API с «контентным» расширением.
		{"json", "7105.json", "http://a.onion/wp-json/wp/v2/pages/7105.json", true, "эндпоинт API"},
		{"json", "embedc427.json", "http://a.onion/wp-json/oembed/1.0/embedc427.json", true, "эндпоинт API"},
		// Настоящее содержимое - все три фильтра молчат.
		{"epub", "book.epub", "http://a.onion/download/X/book.epub", false, ""},
		{"mp4", "introo.mp4", "http://a.onion/videos/introo.mp4", false, ""},
		{"pdf", "lt.pdf", "http://a.onion/static/cv/lt.pdf", false, ""},
		{"json", "dataset.json", "http://a.onion/data/dataset.json", false, ""},
		{"zip", "dump.zip", "http://a.onion/files/dump.zip", false, ""},
		{"txt", "notes.txt", "http://a.onion/notes.txt", false, ""},
		{"iso", "ubuntu.iso", "http://a.onion/ubuntu.iso", false, ""},
	}
	for _, c := range cases {
		got := CatalogJunkReason(c.ext, c.name, c.url)
		if (got != "") != c.wantJunk {
			t.Errorf("%s/%s: причина %q, ожидала junk=%v", c.name, c.ext, got, c.wantJunk)
			continue
		}
		if c.contains != "" && !strings.Contains(got, c.contains) {
			t.Errorf("%s: причина %q не содержит %q", c.name, got, c.contains)
		}
		if got == "" && IsCatalogJunk(c.ext, c.name, c.url) {
			t.Errorf("%s: IsCatalogJunk расходится с CatalogJunkReason", c.name)
		}
	}
}

// TestCatalogJunkReasonMatchesFromURL проверяет главное свойство единой точки:
// то, что отклоняет FromURL, обязано признаваться мусором и здесь. Иначе
// фильтр на входе и чистка снова разойдутся.
func TestCatalogJunkReasonMatchesFromURL(t *testing.T) {
	urls := []string{
		"http://a.onion/manifest.json",
		"http://a.onion/robots.txt",
		"http://a.onion/key.asc",
		"http://a.onion/wp-json/wp/v2/pages/1.json",
		"http://a.onion/book.epub",
		"http://a.onion/video.mp4",
		"http://a.onion/report.pdf",
	}
	for _, abs := range urls {
		ref, accepted := FromURL(abs, "http://a.onion/", "a.onion")
		if !accepted {
			// Отклонённое FromURL обязано быть мусором и для чистки.
			continue
		}
		if reason := CatalogJunkReason(ref.Ext, ref.Filename, ref.URL); reason != "" {
			t.Errorf("FromURL принял %s, а чистка сочла мусором: %s", abs, reason)
		}
	}
}

func TestCatalogJunkReasonCaseInsensitive(t *testing.T) {
	// Расширение и имя в реальной базе встречаются в разном регистре:
	// LICENSE.txt, X.PNG.
	for _, c := range []struct{ ext, name, url string }{
		{"ASC", "KEY.ASC", "http://a.onion/KEY.ASC"},
		{"TXT", "Robots.TXT", "http://a.onion/Robots.TXT"},
		{"JSON", "MANIFEST.JSON", "http://a.onion/MANIFEST.JSON"},
	} {
		if CatalogJunkReason(c.ext, c.name, c.url) == "" {
			t.Errorf("%s/%s не признан мусором", c.name, c.ext)
		}
	}
}

func TestCatalogJunkReasonMalformedURL(t *testing.T) {
	// Неразбираемый URL не должен ни ронять проверку, ни объявлять запись
	// мусором по одному этому признаку: содержимое с кривым адресом чистится
	// отдельным правилом в store, а не здесь.
	if reason := CatalogJunkReason("epub", "book.epub", "://не url"); reason != "" {
		t.Errorf("кривой URL дал причину %q", reason)
	}
	if reason := CatalogJunkReason("epub", "book.epub", ""); reason != "" {
		t.Errorf("пустой URL дал причину %q", reason)
	}
}
