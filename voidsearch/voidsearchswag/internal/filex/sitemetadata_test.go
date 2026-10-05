package filex

import "testing"

// TestIsSiteMetadata проверяет отсечение служебных файлов сайта. Все имена
// взяты из реальной базы: они лежали в каталоге на равных с книгами и
// образами, хотя контентом не являются.
func TestIsSiteMetadata(t *testing.T) {
	real := []string{
		"manifest.json",
		"opensearch_html.xml",
		"publicpgp.txt",
		"pgp-key.txt",
		"pgp.txt",
		"canary.txt",
		"badge.txt",
		"LICENSE.txt",
	}
	for _, n := range real {
		if !IsSiteMetadata(n) {
			t.Errorf("%q не признан служебным", n)
		}
	}
	// Проверка регистронезависима: в реальной базе лежало LICENSE.txt, а
	// фильтр обязан ловить и license.txt.
	for _, n := range []string{"license.txt", "License.TXT", "  MANIFEST.JSON  "} {
		if !IsSiteMetadata(n) {
			t.Errorf("%q не признан служебным", n)
		}
	}
	// Общеизвестные служебные имена.
	for _, n := range []string{
		"robots.txt", "sitemap.xml", "favicon.ico", "humans.txt", "security.txt",
		"ads.txt", "crossdomain.xml", "browserconfig.xml", "index.php",
		"xmlrpc.php", "admin-ajax.php", "package-lock.json", "yarn.lock",
		"site.webmanifest", "readme.md",
	} {
		if !IsSiteMetadata(n) {
			t.Errorf("%q не признан служебным", n)
		}
	}
}

func TestIsSiteMetadataKeepsRealContent(t *testing.T) {
	// Обратная сторона важнее прямой: фильтр не должен выбрасывать настоящие
	// файлы. Имена взяты из той же реальной базы.
	keep := []string{
		"introo.mp4",
		"tm_payforia7.mp4",
		"lt.pdf",
		"en.pdf",
		"1cashATM.mp4",
		"3paypal.mp4",
		"McFadden, Freida - Die verschlossene Tür.epub",
		"Chua, Amy - Das letzte Geständnis.epub",
		"dump.sql",
		"backup.zip",
		"ubuntu-24.04.iso",
		"report.pdf",
		"notes.txt",
		"data.json",
		"export.csv",
		"archive.tar.gz",
	}
	for _, n := range keep {
		if IsSiteMetadata(n) {
			t.Errorf("%q ложно признан служебным", n)
		}
	}
}

func TestIsAPIPath(t *testing.T) {
	// Оба пути взяты из реальной базы: машинальные ответы WordPress лежали в
	// каталоге как файлы.
	paths := []string{
		"/wp-json/oembed/1.0/embedc427.json",
		"/wp-json/wp/v2/pages/7105.json",
		"/wp-admin/admin-ajax.php",
		"/api/v1/users",
		"/graphql",
		"/rest/v2/items.json",
		"/jsonapi/node/page",
		"/feed/rss.xml",
	}
	for _, p := range paths {
		if !IsAPIPath(p) {
			t.Errorf("%q не признан эндпоинтом API", p)
		}
	}
	// Регистр не важен.
	if !IsAPIPath("/WP-JSON/wp/v2/pages/1.json") {
		t.Error("регистр не учтён")
	}
}

func TestIsAPIPathKeepsRealFiles(t *testing.T) {
	keep := []string{
		"/videos/introo.mp4",
		"/assets/videos/tm_payforia7.mp4",
		"/static/cv/lt.pdf",
		"/download/ULb1bQmficVhGyLX/McFadden.epub",
		"/files/1cashATM.mp4",
		"/book/h5Qnk2MxJb9sbhAW",
		"/static/opensearch_html.xml",
	}
	for _, p := range keep {
		if IsAPIPath(p) {
			t.Errorf("%q ложно признан эндпоинтом API", p)
		}
	}
}

// TestFromURLRejectsSiteMetadata закрывает сквозное поведение: служебный файл
// обязан отклоняться ещё до записи в каталог, а не только вычищаться позже.
func TestFromURLRejectsSiteMetadata(t *testing.T) {
	for _, abs := range []string{
		"http://a.onion/manifest.json",
		"http://a.onion/static/opensearch_html.xml",
		"http://a.onion/robots.txt",
		"http://a.onion/LICENSE.txt",
		"http://a.onion/publicpgp.txt",
	} {
		if ref, ok := FromURL(abs, "http://a.onion/", "a.onion"); ok {
			t.Errorf("%s принят как %q", abs, ref.Filename)
		}
	}
}

func TestFromURLRejectsAPIPaths(t *testing.T) {
	for _, abs := range []string{
		"http://a.onion/wp-json/oembed/1.0/embedc427.json",
		"http://a.onion/wp-json/wp/v2/pages/7105.json",
		"http://a.onion/api/v1/dump.json",
	} {
		if ref, ok := FromURL(abs, "http://a.onion/", "a.onion"); ok {
			t.Errorf("%s принят как %q", abs, ref.Filename)
		}
	}
}

func TestFromURLStillAcceptsContent(t *testing.T) {
	// Фильтры не должны задеть обычные файлы из той же реальной базы.
	cases := map[string]string{
		"http://a.onion/videos/introo.mp4":                                   "introo.mp4",
		"http://a.onion/static/cv/lt.pdf":                                    "lt.pdf",
		"http://a.onion/download/X/McFadden%2C%20Freida%20-%20T%C3%BCr.epub": "McFadden, Freida - Tür.epub",
		"http://a.onion/files/1cashATM.mp4":                                  "1cashATM.mp4",
	}
	for abs, want := range cases {
		ref, ok := FromURL(abs, "http://a.onion/", "a.onion")
		if !ok {
			t.Errorf("%s отклонён", abs)
			continue
		}
		if ref.Filename != want {
			t.Errorf("%s: имя %q, ожидала %q", abs, ref.Filename, want)
		}
	}
}

// TestFromURLIgnoresQueryString проверяет, что имя выводится из пути, а не из
// query-строки. В реальной базе лежал URL
// /wp-json/oembed/1.0/embedc427.json?url=https%3A%2F%2Fsite%2F,
// и имя обязано браться из последнего сегмента пути.
func TestFromURLIgnoresQueryString(t *testing.T) {
	abs := "http://a.onion/files/book.epub?utm_source=x&format=zip"
	ref, ok := FromURL(abs, "http://a.onion/", "a.onion")
	if !ok {
		t.Fatal("файл отклонён")
	}
	if ref.Filename != "book.epub" {
		t.Errorf("имя %q, ожидала book.epub (query просочилась в имя)", ref.Filename)
	}
	if ref.Ext != "epub" {
		t.Errorf("расширение %q, ожидала epub", ref.Ext)
	}
}
