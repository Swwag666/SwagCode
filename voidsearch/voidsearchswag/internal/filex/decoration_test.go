package filex

import (
	"net/url"
	"strings"
	"testing"
)

// Тесты правила отбраковки оформления сайта.
//
// Правило существует потому, что изображения пущены в каталог (этап 61), а
// точный список siteMetadataNames ловит только favicon.ico и
// apple-touch-icon.png. Проверяются обе стороны: оформление отбраковывается, а
// самостоятельные изображения и документы с похожими словами в имени - нет.
// Вторая сторона важнее: ложное срабатывание молча теряет данные, а
// пропущенная иконка оставляет в каталоге лишнюю строку.

func TestIsSiteDecorationRejectsChromeByName(t *testing.T) {
	cases := []struct{ ext, name string }{
		{"png", "logo.png"},
		{"png", "LOGO.PNG"},
		{"png", "site-logo-dark.png"},
		{"svg", "logotype.svg"},
		{"svg", "sprite.svg"},
		{"png", "sprite@2x.png"},
		{"jpg", "banner.jpg"},
		{"webp", "bg.webp"},
		{"png", "background.png"},
		{"png", "avatar.png"},
		{"jpg", "thumbnail_001.jpg"},
		{"gif", "spinner.gif"},
		{"gif", "blank.gif"},
		{"png", "placeholder.png"},
		{"png", "watermark.png"},
		{"png", "button.png"},
		{"svg", "arrow.svg"},
		{"png", "header.png"},
		{"png", "footer.png"},
		{"png", "menu.png"},
		{"jpg", "default.jpg"},
		{"png", "no-image.png"},
		{"gif", "1x1.gif"},
		{"gif", "1x1-pixel.gif"},
		{"svg", "gear_icon.svg"},
		{"png", "social-icons.png"},
		{"png", "icon-arrow.png"},
		{"png", "icon_small.png"},
		{"svg", "icons.svg"},
		{"png", "apple-touch-icon-120.png"},
		{"svg", "badge.svg"},
		{"png", "emoji.png"},
	}
	for _, c := range cases {
		if !IsSiteDecoration(c.ext, c.name, "/images/"+c.name) {
			t.Errorf("%s не признано оформлением сайта", c.name)
		}
		if reason := CatalogJunkReason(c.ext, c.name, "http://a.onion/images/"+c.name); reason == "" {
			t.Errorf("%s прошло фильтр каталога", c.name)
		} else if !strings.Contains(reason, "оформления") {
			t.Errorf("%s отбраковано с неожиданной причиной: %s", c.name, reason)
		}
	}
}

func TestIsSiteDecorationRejectsChromeByPath(t *testing.T) {
	// Имя нейтральное, но путь выдаёт ассеты темы или набора иконок.
	cases := []struct{ path, name string }{
		{"/assets/photo.jpg", "photo.jpg"},
		{"/wp-content/themes/store/img/pic.png", "pic.png"},
		{"/wp-includes/images/crystal/x.png", "x.png"},
		{"/skins/default/a.jpg", "a.jpg"},
		{"/theme/img/b.png", "b.png"},
		{"/icons/telegram.svg", "telegram.svg"},
		{"/sprites/main.png", "main.png"},
		{"/static/img/pic.jpg", "pic.jpg"},
		{"/images/icons/star.png", "star.png"},
		{"/img/ui/panel.png", "panel.png"},
		{"/fonts/icon.svg", "icon.svg"},
		{"/dist/x.png", "x.png"},
	}
	for _, c := range cases {
		if !IsSiteDecoration(ExtOf(c.name), c.name, c.path) {
			t.Errorf("%s в пути %s не признано оформлением", c.name, c.path)
		}
	}
}

func TestIsSiteDecorationKeepsRealImages(t *testing.T) {
	cases := []struct{ ext, name string }{
		{"jpg", "photo001.jpg"},
		{"tiff", "scan_0012.tiff"},
		{"tif", "page-01.tif"},
		{"jpg", "cover.jpg"},
		{"png", "screenshot.png"},
		{"jpg", "IMG_2049.jpg"},
		{"jpeg", "portrait.jpeg"},
		{"webp", "wallpaper.webp"},
		{"png", "DSC_0001.png"},
		{"jpg", "diagram.jpg"},
		{"svg", "scheme.svg"},
		{"png", "map.png"},
		{"jpg", "iconography.jpg"},
		{"png", "iconology-diagram.png"},
		{"jpg", "bicon.jpg"},
		// Реальный случай из замера на живых страницах: маркер icon_ как
		// подстрока отбраковал фотографию с конференции, потому что
		// «WikiCon_2026» в нижнем регистре содержит «icon_». Сигнал перенесён в
		// начало имени, и этот кейс держит его там.
		{"jpg", "330px-2026-09-19_WikiCon_2026_in_Regensburg_STP_3416.jpg"},
		{"jpg", "Wikicon-2026.jpg"},
		{"png", "lexicon_2026.png"},
		{"png", "menu-analysis.png"},
		{"jpg", "header-design.jpg"},
		{"png", "buttons-of-1920s.png"},
	}
	for _, c := range cases {
		// Общий путь намеренно не входит в признаки оформления: библиотеки и
		// архивы хранят сканы и фотографии именно в /images/ и /img/.
		if IsSiteDecoration(c.ext, c.name, "/images/"+c.name) {
			t.Errorf("%s ложно признано оформлением сайта", c.name)
		}
		if reason := CatalogJunkReason(c.ext, c.name, "http://a.onion/images/"+c.name); reason != "" {
			t.Errorf("%s отбраковано: %s", c.name, reason)
		}
	}
}

func TestIsSiteDecorationNotAppliedToOtherFormats(t *testing.T) {
	// Область правила сужена до графики намеренно: слова logo, icon и banner
	// встречаются в названиях книг и документов, и выбросить их значило бы
	// потерять содержимое.
	cases := []struct{ ext, name string }{
		{"pdf", "logo.pdf"},
		{"epub", "icon.epub"},
		{"djvu", "banner.djvu"},
		{"fb2", "sprite.fb2"},
		{"docx", "header.docx"},
		{"zip", "themes.zip"},
		{"mp4", "logo-animation.mp4"},
		{"txt", "iconography.txt"},
		{"exe", "loader.exe"},
	}
	for _, c := range cases {
		if IsSiteDecoration(c.ext, c.name, "/assets/"+c.name) {
			t.Errorf("правило оформления сработало на не-графике: %s", c.name)
		}
		if reason := CatalogJunkReason(c.ext, c.name, "http://a.onion/files/"+c.name); reason != "" {
			t.Errorf("%s отбраковано: %s", c.name, reason)
		}
	}
}

func TestIsSiteDecorationNormalizesInput(t *testing.T) {
	// Расширение и имя приходят из разных источников: из URL, из
	// Content-Disposition и из базы, где регистр не гарантирован.
	if !IsSiteDecoration("PNG", "LOGO.PNG", "/") {
		t.Error("верхний регистр не приведён")
	}
	if !IsSiteDecoration(" png ", " logo.png ", "/") {
		t.Error("пробелы не срезаны")
	}
	if !IsSiteDecoration("png", "logo.png", "/ASSETS/x") {
		t.Error("путь в верхнем регистре не приведён")
	}
	if IsSiteDecoration("", "logo.png", "/") {
		t.Error("пустое расширение принято за графику")
	}
}

func TestFromURLRejectsDecorationAndAcceptsRealImage(t *testing.T) {
	if _, ok := FromURL("http://a.onion/images/logo.png", "", "a.onion"); ok {
		t.Error("оформление сайта принято за файл")
	}
	if _, ok := FromURL("http://a.onion/assets/photo.jpg", "", "a.onion"); ok {
		t.Error("изображение из ассетов принято за файл")
	}
	ref, ok := FromURL("http://a.onion/images/photo.jpg", "", "a.onion")
	if !ok {
		t.Fatal("самостоятельное изображение отброшено")
	}
	if ref.Ext != "jpg" || ref.MIME != "image/jpeg" {
		t.Errorf("неверная ссылка: %+v", ref)
	}
}

func TestCatalogJunkReasonMatchesPathForm(t *testing.T) {
	// Две формы одной функции обязаны совпадать: CatalogJunkReason разбирает
	// адрес сама, CatalogJunkReasonPath принимает готовый путь. Расхождение
	// означало бы, что чистка каталога и точки добавления применяют разные
	// критерии, - тот самый дефект, из-за которого появилась общая функция.
	cases := []struct{ ext, name, raw string }{
		{"png", "logo.png", "http://a.onion/images/logo.png"},
		{"jpg", "photo.jpg", "http://a.onion/images/photo.jpg"},
		{"jpg", "photo.jpg", "http://a.onion/assets/photo.jpg"},
		{"json", "manifest.json", "http://a.onion/manifest.json"},
		{"json", "page.json", "http://a.onion/wp-json/wp/v2/pages/7.json"},
		{"asc", "key.asc", "http://a.onion/key.asc"},
		{"", "dir", "http://a.onion/dir/"},
		{"epub", "book.epub", "http://a.onion/books/book.epub"},
		{"svg", "icon.svg", "http://a.onion/icons/icon.svg"},
		{"pdf", "logo.pdf", "http://a.onion/docs/logo.pdf"},
	}
	for _, c := range cases {
		want := CatalogJunkReason(c.ext, c.name, c.raw)
		p := ""
		if u, err := url.Parse(c.raw); err == nil {
			p = u.Path
		}
		if got := CatalogJunkReasonPath(c.ext, c.name, p); got != want {
			t.Errorf("%s: полная форма %q, форма с путём %q", c.name, want, got)
		}
	}
}

func TestCatalogJunkReasonMalformedURLSameAsEmptyPath(t *testing.T) {
	// Прежняя реализация при ошибке разбора адреса пропускала проверку
	// эндпоинта API. Новая обязана вести себя так же: неразобранный адрес
	// означает пустой путь, а не отбраковку.
	want := CatalogJunkReasonPath("epub", "book.epub", "")
	if got := CatalogJunkReason("epub", "book.epub", "http://[::1]:named/"); got != want {
		t.Errorf("битый адрес дал %q, ожидала %q", got, want)
	}
	if want != "" {
		t.Fatalf("фикстура неверна: пригодная запись отбракована: %s", want)
	}
}

func TestSiteMetadataNamesUnaffectedByDecoration(t *testing.T) {
	// Точный список служебных имён остался коротким: правило оформления не
	// подменяет его и не добавляет в него эвристик, против которых предостерегает
	// комментарий к siteMetadataNames.
	for _, name := range []string{"logo.png", "sprite.svg", "banner.jpg"} {
		if IsSiteMetadata(name) {
			t.Errorf("%s попало в точный список служебных имён", name)
		}
	}
	for _, name := range []string{"robots.txt", "favicon.ico", "manifest.json"} {
		if !IsSiteMetadata(name) {
			t.Errorf("%s выпало из точного списка служебных имён", name)
		}
	}
}
