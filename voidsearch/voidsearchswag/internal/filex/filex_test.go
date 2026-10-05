package filex

import (
	"net/http"
	"strings"
	"testing"
)

const v2a = "abcdefghijklmnop"

func TestExtOf(t *testing.T) {
	cases := []struct{ in, want string }{
		{"file.zip", "zip"},
		{"FILE.PDF", "pdf"},
		{"archive.tar.gz", "gz"},
		{"noext", ""},
		{"trailing.", ""},
		{"weird.z?x=1", ""},
		{"too.longextension", ""},
		{"dots.in.name.txt", "txt"},
		{".hidden", "hidden"},
		{"file.7z", "7z"},
	}
	for _, c := range cases {
		if got := ExtOf(c.in); got != c.want {
			t.Errorf("ExtOf(%q) = %q, ожидала %q", c.in, got, c.want)
		}
	}
}

func TestKnownExt(t *testing.T) {
	// Изображения входят в список с этапа 61: их пустили в каталог вместе с
	// правилом отбраковки оформления сайта (IsSiteDecoration), поэтому сами по
	// себе png и svg больше не признак мусора.
	for _, e := range []string{"zip", "pdf", "sql", "kdbx", "apk", "torrent", "mp4", "png", "svg", "jpg", "djvu", "fb2"} {
		if !KnownExt(e) {
			t.Errorf("расширение %q не распознано", e)
		}
	}
	// Разметка, стили, скрипты, шрифты и иконки содержимом не являются: это
	// инфраструктура сайта, а не объект для скачивания.
	for _, e := range []string{"css", "js", "html", "php", "ico", "woff2", ""} {
		if KnownExt(e) {
			t.Errorf("%q принято за файл содержимого", e)
		}
	}
}

func TestMimeForExt(t *testing.T) {
	if got := MimeForExt("pdf"); got != "application/pdf" {
		t.Errorf("mime pdf = %q", got)
	}
	if got := MimeForExt("nope"); got != "" {
		t.Errorf("mime неизвестного = %q", got)
	}
}

func TestSafeName(t *testing.T) {
	if got := SafeName("  file.zip  "); got != "file.zip" {
		t.Errorf("пробелы не срезаны: %q", got)
	}
	if got := SafeName("a\x00b.txt"); strings.Contains(got, "\x00") {
		t.Errorf("управляющий символ остался: %q", got)
	}
	if got := SafeName(".."); got != "" {
		t.Errorf(".. не отброшено: %q", got)
	}
	if got := SafeName("."); got != "" {
		t.Errorf(". не отброшено: %q", got)
	}
	long := strings.Repeat("x", 300) + ".txt"
	if got := SafeName(long); len([]rune(got)) > 200 {
		t.Errorf("имя не обрезано: %d", len([]rune(got)))
	}
}

func TestExtractLinks(t *testing.T) {
	body := `<html><body>
<a href="/files/a.zip">A</a>
<a href="http://` + v2a + `.onion/b.pdf">B</a>
<img src="/img/c.png">
<a href="javascript:void(0)">no</a>
<a href="/files/a.zip">dup</a>
</body></html>`
	got := ExtractLinks(body, "http://"+v2a+".onion/")
	if len(got) != 3 {
		t.Fatalf("найдено %d ссылок, ожидала 3 (javascript отсеян, дубль схлопнут): %v", len(got), got)
	}
	if !contains(got, "/files/a.zip") {
		t.Error("ссылка потеряна")
	}
	if contains(got, "javascript:void(0)") {
		t.Error("javascript-ссылка попала в список")
	}
}

func TestExtractLinksDropsSchemes(t *testing.T) {
	body := `<a href="data:text/plain,x">d</a>
<a href="mailto:a@b.c">m</a>
<a href="tel:+1">t</a>
<a href="blob:xyz">b</a>
<a href="about:blank">a</a>`
	if got := ExtractLinks(body, "http://a.onion/"); len(got) != 0 {
		t.Errorf("посторонние схемы не отсеяны: %v", got)
	}
}

func TestExtractLinksMalformed(t *testing.T) {
	body := `<a href="">empty</a><a>none</a>`
	got := ExtractLinks(body, "http://a.onion/")
	if contains(got, "") {
		t.Error("пустая ссылка попала в список")
	}
}

func TestExtractLinksPicksDownloadAttr(t *testing.T) {
	body := `<a href="/x.bin" download="report.tar.gz">x</a>`
	got := ExtractLinks(body, "http://a.onion/")
	if !contains(got, "/x.bin") {
		t.Errorf("ссылка с download не взята: %v", got)
	}
	if !contains(got, "report.tar.gz") {
		t.Errorf("значение атрибута download не взято: %v", got)
	}
}

func TestAbsolutise(t *testing.T) {
	base := "http://" + v2a + ".onion/dir/page.html"
	cases := []struct{ in, want string }{
		{"/files/a.zip", "http://" + v2a + ".onion/files/a.zip"},
		{"a.zip", "http://" + v2a + ".onion/dir/a.zip"},
		{"http://other.onion/x.pdf", "http://other.onion/x.pdf"},
		{"//other.onion/x.pdf", "http://other.onion/x.pdf"},
		{"#frag", ""},
		{"javascript:alert(1)", ""},
		{"mailto:a@b.c", ""},
		{"data:text/plain,x", ""},
		{"", ""},
		{"ftp://host/x.zip", ""},
	}
	for _, c := range cases {
		if got := Absolutise(c.in, base); got != c.want {
			t.Errorf("Absolutise(%q) = %q, ожидала %q", c.in, got, c.want)
		}
	}
}

func TestAbsolutiseStripsFragment(t *testing.T) {
	got := Absolutise("/a.zip#section", "http://"+v2a+".onion/")
	if strings.Contains(got, "#") {
		t.Errorf("фрагмент не отсечён: %q", got)
	}
}

func TestStripQuery(t *testing.T) {
	if got := StripQuery("http://a.onion/x?y=1#z"); got != "http://a.onion/x" {
		t.Errorf("StripQuery = %q", got)
	}
	if got := StripQuery("::bad::"); got != "" {
		t.Errorf("битый URL должен дать пусто: %q", got)
	}
}

func TestDownloadHint(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"/download", true},
		{"/dl/x", true},
		{"/files/report", true},
		{"/get?id=1", true},
		{"/attachment/7", true},
		{"/uploads/a", true},
		{"/media/v", true},
		{"/dump", true},
		{"/about", false},
		{"/", false},
		{"/contact", false},
		{"/Downloads", true},
	}
	for _, c := range cases {
		if got := DownloadHint(c.in); got != c.want {
			t.Errorf("DownloadHint(%q) = %v, ожидала %v", c.in, got, c.want)
		}
	}
}

func TestAttachmentName(t *testing.T) {
	// Два ожидания здесь изменены намеренно, прежние кодировали дефекты:
	//
	// 1. filename*=UTF-8''%D1%84.zip ожидало строку процентов. Значение по
	//    RFC 5987 процент-закодировано, и возвращать его как есть значит
	//    писать в каталог «%D1%84.zip» вместо «ф.zip».
	// 2. inline; filename="x.zip" ожидало отказ. Тип содержимого не отменяет
	//    наличие имени: сервер, отдающий книгу как inline, не давал имени
	//    вовсе, и каталог получал имя, выведенное из URL, - то самое
	//    склеенное из query-строки, на которое жаловались в живом прогоне.
	cases := []struct {
		disp string
		want string
		ok   bool
	}{
		{`attachment; filename="dump.zip"`, "dump.zip", true},
		{`attachment; filename=dump.zip`, "dump.zip", true},
		{`attachment;filename="a b.pdf"`, "a b.pdf", true},
		{`attachment; filename*=UTF-8''%D1%84.zip`, "ф.zip", true},
		{`inline; filename="x.zip"`, "x.zip", true},
		{``, "", false},
		{`attachment`, "", false},
		{`attachment; filename="a.zip"; size=10`, "a.zip", true},
		{`ATTACHMENT; FILENAME="up.ZIP"`, "up.ZIP", true},
		{`attachment; file="up.ZIP"`, "", false},
	}
	for _, c := range cases {
		got, ok := AttachmentName(c.disp)
		if ok != c.ok {
			t.Errorf("AttachmentName(%q) ok=%v, ожидала %v", c.disp, ok, c.ok)
		}
		if got != c.want {
			t.Errorf("AttachmentName(%q) = %q, ожидала %q", c.disp, got, c.want)
		}
	}
}

func TestAttachmentNameRFC5987(t *testing.T) {
	// Расширенная форма обязана проходить процентное декодирование и иметь
	// приоритет над обычной, как того требует RFC 6266.
	cases := []struct {
		disp string
		want string
	}{
		{`attachment; filename*=UTF-8''%D0%BA%D0%BD%D0%B8%D0%B3%D0%B0.epub`, "книга.epub"},
		{`attachment; filename*=UTF-8'ru'%D0%BA%D0%BD%D0%B8%D0%B3%D0%B0.pdf`, "книга.pdf"},
		{`attachment; filename="fallback.zip"; filename*=UTF-8''%D0%B0%D1%80%D1%85%D0%B8%D0%B2.zip`, "архив.zip"},
		{`attachment; filename*=UTF-8''a%20b.pdf`, "a b.pdf"},
		// Плюс в имени файла - законный символ, а не закодированный пробел:
		// PathUnescape его не трогает, и это правильное поведение.
		{`attachment; filename*=UTF-8''c%2B%2B.pdf`, "c++.pdf"},
	}
	for _, c := range cases {
		got, ok := AttachmentName(c.disp)
		if !ok {
			t.Errorf("AttachmentName(%q) не нашло имя", c.disp)
			continue
		}
		if got != c.want {
			t.Errorf("AttachmentName(%q) = %q, ожидала %q", c.disp, got, c.want)
		}
	}
}

func TestAttachmentNameMalformedEncodingFallsBack(t *testing.T) {
	// Неразбираемая процент-последовательность не должна терять имя целиком:
	// неполное имя в каталоге лучше пустого.
	got, ok := AttachmentName(`attachment; filename*=UTF-8''bad%ZZ.zip`)
	if !ok {
		t.Fatal("имя потеряно на неразбираемой кодировке")
	}
	if got == "" {
		t.Error("вернулась пустая строка")
	}
}

func TestAttachmentNameQuotedSemicolon(t *testing.T) {
	// Точка с запятой законно встречается внутри имени в кавычках, поэтому
	// значение в кавычках ищется по закрывающей кавычке, а не по первой ';'.
	got, ok := AttachmentName(`attachment; filename="a;b.zip"`)
	if !ok {
		t.Fatal("имя не найдено")
	}
	if got != "a;b.zip" {
		t.Errorf("получено %q, ожидала %q", got, "a;b.zip")
	}
}

func TestAttachmentNameDoesNotMatchSubstringKey(t *testing.T) {
	// «xfilename=» не должно совпадать с «filename=»: прежнему поиску по
	// подстроке было всё равно, что стоит перед ключом.
	if got, ok := AttachmentName(`attachment; xfilename="a.zip"`); ok {
		t.Errorf("ложное совпадение: %q", got)
	}
}

func TestAttachmentNameDoubleQuoteInName(t *testing.T) {
	// Прежняя версия искала префикс charset'lang' через LastIndex("''"),
	// поэтому имя, содержащее такую пару, обрезалось. Проверка на реальном
	// случае: апострофы в именах встречаются.
	got, ok := AttachmentName(`attachment; filename*=UTF-8''it''s.zip`)
	if !ok {
		t.Fatal("имя не найдено")
	}
	if got != "it''s.zip" {
		t.Errorf("получено %q, ожидала %q", got, "it''s.zip")
	}
}

func TestAttachmentNameFormAndOtherTypes(t *testing.T) {
	// form-data - третий законный носитель filename, особенно у выгрузок.
	for _, disp := range []string{
		`form-data; name="f"; filename="up.zip"`,
		`inline; filename=plain.pdf`,
		`INLINE; FILENAME="case.zip"`,
	} {
		if _, ok := AttachmentName(disp); !ok {
			t.Errorf("%q отклонено", disp)
		}
	}
}

func TestMimeOf(t *testing.T) {
	if got := MimeOf("text/html; charset=utf-8"); got != "text/html" {
		t.Errorf("MimeOf = %q", got)
	}
	if got := MimeOf(""); got != "" {
		t.Errorf("пустой mime = %q", got)
	}
	if got := MimeOf("APPLICATION/PDF"); got != "application/pdf" {
		t.Errorf("регистр не приведён: %q", got)
	}
}

func TestPathOf(t *testing.T) {
	if got := PathOf("http://a.onion/dir/x.zip?y=1"); got != "/dir/x.zip" {
		t.Errorf("PathOf = %q", got)
	}
	if got := PathOf("::bad::"); got != "::bad::" {
		t.Errorf("битый URL должен возвращаться как есть: %q", got)
	}
}

func TestFromURL(t *testing.T) {
	ref, ok := FromURL("http://a.onion/data/dump.sql", "http://a.onion/", "a.onion")
	if !ok {
		t.Fatal("файл не распознан")
	}
	if ref.Filename != "dump.sql" || ref.Ext != "sql" {
		t.Errorf("разбор неверен: %+v", ref)
	}
	if ref.MIME != "application/sql" {
		t.Errorf("mime=%q", ref.MIME)
	}
	if ref.Size != 0 {
		t.Errorf("размер без сети должен быть нулевым: %d", ref.Size)
	}
}

func TestFromURLRejectsNonFiles(t *testing.T) {
	for _, u := range []string{
		"http://a.onion/",
		"http://a.onion/about",
		"http://a.onion/style.css",
		"http://a.onion/app.js",
		"http://a.onion/img/logo.png",
	} {
		if _, ok := FromURL(u, "http://a.onion/", "a.onion"); ok {
			t.Errorf("%q принято за файл содержимого", u)
		}
	}
}

func TestFromURLPercentEncoded(t *testing.T) {
	ref, ok := FromURL("http://a.onion/files/report%20final.pdf", "", "a.onion")
	if !ok {
		t.Fatal("файл с пробелом в имени не распознан")
	}
	if ref.Ext != "pdf" {
		t.Errorf("ext=%q", ref.Ext)
	}
}

func TestContentLengthHeader(t *testing.T) {
	h := http.Header{}
	h.Set("Content-Length", "12345")
	if v := h.Get("Content-Length"); v != "12345" {
		t.Errorf("заголовок не читается: %q", v)
	}
}

// TestKnownExtsExcludeTLDs - регрессия на дефект, из-за которого в каталоге
// появлялись «файлы» с расширением onion. Доменная зона не является
// расширением файла: ссылка на onion-адрес обязана отклоняться.
func TestKnownExtsExcludeTLDs(t *testing.T) {
	for _, ext := range []string{"onion", "i2p", "ip", "com", "org", "net", "ru", "io", "html", "htm", "php", "asp", "jsp"} {
		if KnownExt(ext) {
			t.Errorf("расширение %q принято за файл содержимого", ext)
		}
	}
}

func TestFromURLRejectsOnionSelfLink(t *testing.T) {
	// Реальная запись из каталога: сайт печатает собственный адрес текстом,
	// и путь заканчивается на «.onion». Это не файл.
	const addr = "blackpasspn7734jqltjj2qx4qez5gcpcwujuugymky3lzcmmcfpzbyd.onion"
	u := "http://" + addr + "/" + addr
	if ref, ok := FromURL(u, "http://"+addr+"/", addr); ok {
		t.Errorf("само-ссылка принята за файл: %+v", ref)
	}
}

func TestFromURLAcceptsFileOnOnionHost(t *testing.T) {
	// Хост в зоне .onion не должен мешать распознавать настоящий файл:
	// иначе каталог останется пустым на всём onion-содержимом.
	ref, ok := FromURL("http://abcdefghijklmnop.onion/files/dump.zip", "", "abcdefghijklmnop.onion")
	if !ok {
		t.Fatal("zip на onion-хосте не распознан")
	}
	if ref.Ext != "zip" || ref.Filename != "dump.zip" {
		t.Errorf("поля: ext=%q name=%q", ref.Ext, ref.Filename)
	}
	if ref.MIME != "application/zip" {
		t.Errorf("MIME=%q", ref.MIME)
	}
}

func TestMimeForExtOnionIsEmpty(t *testing.T) {
	if got := MimeForExt("onion"); got != "" {
		t.Errorf("MimeForExt(\"onion\")=%q, ожидала пусто", got)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
