// Package filex - разбор ссылок на файлы без сетевых зависимостей. Пакет
// вынесен отдельно, потому что им пользуются и разведка, и сборщик
// каталога: держать логику в одном из них значит получить цикл импортов.
package filex

import (
	"net/url"
	"path"
	"strings"
	"unicode"

	"golang.org/x/net/html"
)

// KnownExts - расширения, которые считаются файлами содержимого. Список
// намеренно узкий: css, js и картинки разметки сюда не входят, иначе
// каталог топит в мусоре, который не является содержимым сервиса.
//
// Доменные зоны (onion, i2p, ip) сюда входить не должны. «onion» уже лежал в
// этом списке, и из-за него любая ссылка на onion-адрес становилась файлом:
// сайт, который печатает собственный адрес текстом, отдавал запись вида
// «<адрес>.onion [размер неизвестен]» с MIME application/octet-stream. Это
// не файл, а само-ссылка, и чистить такой мусор по одному сложнее, чем не
// пускать его в список.
var KnownExts = map[string]string{
	"zip":     "application/zip",
	"rar":     "application/vnd.rar",
	"7z":      "application/x-7z-compressed",
	"tar":     "application/x-tar",
	"gz":      "application/gzip",
	"bz2":     "application/x-bzip2",
	"xz":      "application/x-xz",
	"zst":     "application/zstd",
	"pdf":     "application/pdf",
	"doc":     "application/msword",
	"docx":    "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	"xls":     "application/vnd.ms-excel",
	"xlsx":    "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	"ppt":     "application/vnd.ms-powerpoint",
	"pptx":    "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	"csv":     "text/csv",
	"txt":     "text/plain",
	"sql":     "application/sql",
	"db":      "application/vnd.sqlite3",
	"sqlite":  "application/vnd.sqlite3",
	"mdb":     "application/x-msaccess",
	"json":    "application/json",
	"xml":     "application/xml",
	"log":     "text/plain",
	"md":      "text/markdown",
	"rtf":     "application/rtf",
	"odt":     "application/vnd.oasis.opendocument.text",
	"ods":     "application/vnd.oasis.opendocument.spreadsheet",
	"epub":    "application/epub+zip",
	"mobi":    "application/x-mobipocket-ebook",
	"iso":     "application/x-iso9660-image",
	"img":     "application/octet-stream",
	"bin":     "application/octet-stream",
	"dat":     "application/octet-stream",
	"exe":     "application/vnd.microsoft.portable-executable",
	"msi":     "application/x-msi",
	"dll":     "application/vnd.microsoft.portable-executable",
	"apk":     "application/vnd.android.package-archive",
	"deb":     "application/vnd.debian.binary-package",
	"rpm":     "application/x-rpm",
	"pem":     "application/x-pem-file",
	"key":     "application/octet-stream",
	"crt":     "application/x-x509-ca-cert",
	"kdbx":    "application/x-keepass2",
	"pgp":     "application/pgp-encrypted",
	"gpg":     "application/pgp-encrypted",
	"asc":     "application/pgp-signature",
	"torrent": "application/x-bittorrent",
	"mp4":     "video/mp4",
	"mkv":     "video/x-matroska",
	"avi":     "video/x-msvideo",
	"mov":     "video/quicktime",
	"mp3":     "audio/mpeg",
	"flac":    "audio/flac",
	"m4a":     "audio/mp4",
	"wav":     "audio/wav",
	"vcf":     "text/vcard",
	"eml":     "message/rfc822",
	"pst":     "application/vnd.ms-outlook",

	// Расширения, которые классификатор вердиктов относит к содержательным
	// категориям, но которых здесь не хватало. Отсутствие было не нейтральным:
	// IsContent возвращает false, CatalogJunkReason объявлял файл мусором, и
	// store.CleanFileCatalog удалял такие строки, а FromURL и коллектор не
	// пускали их в каталог вовсе. В живой проверке так ушли book0997.djvu и
	// book0999.azw3. Инвариант «всякая категория вердиктов проходит фильтр
	// содержимого» закреплён тестом TestEveryVerdictExtensionIsContent.
	//
	// Книги и документы. djvu и fb2 - основные форматы русскоязычных
	// библиотек, nfo намеренно считается содержимым (см. MetadataExts).
	"djvu": "image/vnd.djvu",
	"fb2":  "application/x-fictionbook+xml",
	"azw":  "application/vnd.amazon.ebook",
	"azw3": "application/vnd.amazon.ebook",
	"lit":  "application/x-ms-reader",
	"pdb":  "application/vnd.palm",
	"nfo":  "text/x-nfo",
	"odp":  "application/vnd.oasis.opendocument.presentation",

	// Архивы.
	"tgz": "application/gzip",
	"lz4": "application/x-lz4",

	// Данные.
	"yaml":    "application/yaml",
	"yml":     "application/yaml",
	"tsv":     "text/tab-separated-values",
	"parquet": "application/vnd.apache.parquet",

	// Контейнеры ключей: находка с практическим смыслом, а не служебная
	// подпись, поэтому в MetadataExts не входят.
	"p12": "application/x-pkcs12",
	"pfx": "application/x-pkcs12",

	// Аудио и видео.
	"aac":  "audio/aac",
	"ogg":  "audio/ogg",
	"opus": "audio/opus",
	"webm": "video/webm",
	"flv":  "video/x-flv",
	"wmv":  "video/x-ms-wmv",

	// Исполняемое и скрипты: категория риска, которую пользователь должен
	// видеть, а не терять. Скрипты - обычный текст, поэтому MIME у них
	// текстовый; риск определяется категорией вердикта, а не MIME.
	"jar":      "application/java-archive",
	"sh":       "application/x-sh",
	"bat":      "text/plain",
	"cmd":      "text/plain",
	"ps1":      "text/plain",
	"vbs":      "text/plain",
	"scr":      "application/vnd.microsoft.portable-executable",
	"appimage": "application/vnd.appimage",

	// Изображения. Пущены в каталог не сами по себе, а вместе с правилом
	// отбраковки оформления сайта (см. IsSiteDecoration): без него logo.png,
	// sprite.svg и banner.jpg легли бы в каталог как содержимое и вернули бы
	// дефект этапа 38, где инфраструктура сайта составляла заметную долю
	// выдачи. Правило применяется только к графическим расширениям, поэтому
	// документы и книги с похожими словами в имени не затрагивает.
	"jpg":  "image/jpeg",
	"jpeg": "image/jpeg",
	"png":  "image/png",
	"gif":  "image/gif",
	"webp": "image/webp",
	"bmp":  "image/bmp",
	"svg":  "image/svg+xml",
	"tif":  "image/tiff",
	"tiff": "image/tiff",

	// Намеренно НЕ добавлено, хотя классификатор вердиктов его знает. Причина
	// закреплена тестом TestVerdictContentExceptions - он требует, чтобы
	// исключение оставалось исключением, поэтому случайное добавление сломает
	// тест и заставит решение принять осознанно.
	//
	// com - совпадает с доменной зоной. Адрес, напечатанный на странице
	// текстом, разбирался бы как файл с расширением com: тот же дефект, из-за
	// которого из списка убран onion (см. TestKnownExtsExcludeTLDs).
}

// Ref - найденный файл. Размер и MIME заполняются вызывающим: без сети их
// взять неоткуда, а пакет сети не касается.
type Ref struct {
	URL        string `json:"url"`
	Filename   string `json:"filename"`
	Ext        string `json:"ext"`
	Size       int64  `json:"size"`
	MIME       string `json:"mime,omitempty"`
	SourcePage string `json:"source_page,omitempty"`
	Host       string `json:"host,omitempty"`
}

func KnownExt(ext string) bool {
	_, ok := KnownExts[ext]
	return ok
}

// MetadataExts - расширения, которые описывают или удостоверяют другой файл,
// но сами содержимым не являются: подписи, контрольные суммы, сертификаты и
// ключи.
//
// MIME для них определяется как для обычных файлов (см. KnownExts), но в
// каталог содержимого они не попадают. Каталог отвечает на вопрос «что здесь
// можно скачать и использовать», а подпись без подписанного файла или
// контрольная сумма без образа бесполезны: они раздувают счётчик файлов и
// создают впечатление, что сервис раздаёт контент, которого там нет.
//
// Намеренно НЕ считаются служебными: torrent (это и есть раздаваемый
// объект), gpg (зашифрованный архив - самостоятельный контент), nfo
// (сопроводительный текст релиза).
var MetadataExts = map[string]bool{
	"asc":    true, // подпись PGP
	"sig":    true, // подпись
	"md5":    true, // контрольная сумма
	"sha1":   true,
	"sha256": true,
	"sha512": true,
	"sfv":    true,
	"pem":    true, // сертификат или ключ
	"crt":    true,
	"cer":    true,
	"der":    true,
	"key":    true,
}

// IsContent сообщает, является ли файл самостоятельным содержимым, а не
// служебными данными о другом файле. Неизвестное расширение контентом не
// считается: сначала оно должно попасть в KnownExts.
func IsContent(ext string) bool {
	ext = strings.ToLower(strings.TrimSpace(ext))
	if !KnownExt(ext) {
		return false
	}
	return !MetadataExts[ext]
}

// IsMetadata сообщает, что расширение обозначает служебный артефакт -
// подпись, контрольную сумму, сертификат или ключ.
//
// Намеренно не требует присутствия в KnownExts: часть этих расширений
// (sha256, sig, der и др.) в список содержимого никогда не входила, но
// классификация обязана оставаться верной и тогда, когда их туда добавят.
// Иначе расширение попало бы в каталог как контент в тот самый момент, когда
// кто-то расширит KnownExts.
func IsMetadata(ext string) bool {
	return MetadataExts[strings.ToLower(strings.TrimSpace(ext))]
}

func MimeForExt(ext string) string {
	if m, ok := KnownExts[ext]; ok {
		return m
	}
	return ""
}

// apiPathMarkers - фрагменты пути, по которым адрес узнаётся как эндпоинт
// программного интерфейса, а не как файл.
//
// Ответ API синтаксически неотличим от файла: у него бывает расширение .json
// или .xml, он отдаётся с Content-Disposition и имеет размер. Поэтому он
// проходил ворота расширений и ложился в каталог на равных с книгами и
// архивами. В реальной базе так оказались /wp-json/oembed/1.0/embed...json и
// /wp-json/wp/v2/pages/7105.json - машинальные ответы WordPress, которые
// никто не скачивает как содержимое.
//
// Каталог отвечает на вопрос «что здесь можно скачать и использовать». Ответ
// эндпоинта на этот вопрос не отвечает: он описывает страницу, а не является
// самостоятельным объектом, и к тому же меняется на каждый запрос, поэтому его
// копия в каталоге устаревает немедленно.
var apiPathMarkers = []string{
	"/wp-json/",
	"/wp-admin/admin-ajax.php",
	"/wp/v2/",
	"/oembed/",
	"/api/",
	"/graphql",
	"/jsonapi/",
	"/rest/",
	"/feed/",
	"/rss/",
	"/atom.xml",
}

// siteMetadataNames - имена служебных файлов сайта. Это не содержимое, а
// инфраструктура: манифесты, карты сайта, иконки, файлы подтверждения
// владения и лицензии.
//
// Расширение у них «контентное» - .json, .xml, .txt, - поэтому ворота по
// расширению их пропускают. В реальной базе они занимали заметную долю
// каталога: manifest.json, opensearch_html.xml, robots-подобные .txt,
// LICENSE.txt и pgp-ключи, отданные как текст. Пользователь, ищущий книги или
// образы, получал в выдаче этот мусор, а счётчик «34 файла, 27.9 MiB»
// описывал каталог, где значительная часть записей контентом не является.
//
// Список намеренно короткий и состоит из общеизвестных имён. Расширять его
// стоит только именами, которые одинаковы на всех сайтах: эвристики вроде
// «любое имя из трёх букв» выбросили бы и настоящие файлы.
var siteMetadataNames = map[string]bool{
	// Манифесты и машинные описания сайта.
	"manifest.json":       true,
	"site.webmanifest":    true,
	"browserconfig.xml":   true,
	"crossdomain.xml":     true,
	"opensearch.xml":      true,
	"opensearch_html.xml": true,
	"package.json":        true,
	"composer.json":       true,
	"config.json":         true,
	"settings.json":       true,
	"version.json":        true,
	"swagger.json":        true,
	"openapi.json":        true,
	"openapi.yaml":        true,

	// Файлы для роботов, людей и подтверждения владения.
	"robots.txt":        true,
	"humans.txt":        true,
	"ads.txt":           true,
	"app-ads.txt":       true,
	"security.txt":      true,
	"sitemap.xml":       true,
	"sitemap_index.xml": true,
	"sitemap.txt":       true,
	"feed.xml":          true,
	"rss.xml":           true,
	"atom.xml":          true,

	// Лицензии, версии и журналы изменений - описание релиза, а не релиз.
	"license.txt":   true,
	"licence.txt":   true,
	"license.md":    true,
	"copying.txt":   true,
	"readme.md":     true,
	"readme.txt":    true,
	"changelog.txt": true,
	"changelog.md":  true,
	"version.txt":   true,

	// Иконки и служебная графика.
	"favicon.ico":          true,
	"apple-touch-icon.png": true,

	// Ключи, подписи и «канарейки», отданные как обычный текст. Расширение
	// .asc отклоняется и по IsContent, но эти же данные часто лежат в .txt,
	// поэтому имя проверяется отдельно.
	"publicpgp.txt":      true,
	"pgp.txt":            true,
	"pgp-key.txt":        true,
	"pgp-key.asc":        true,
	"key.asc":            true,
	"canary.txt":         true,
	"badge.txt":          true,
	"warrant-canary.txt": true,

	// Точки входа и служебные скрипты CMS: это страницы и обработчики, а не
	// скачиваемые объекты.
	"index.php":            true,
	"index.html":           true,
	"index.htm":            true,
	"xmlrpc.php":           true,
	"wp-login.php":         true,
	"wp-config.php":        true,
	"wp-config-sample.php": true,
	"admin-ajax.php":       true,
	"phpinfo.php":          true,
	"info.php":             true,
	"cron.php":             true,
	"install.php":          true,
	"setup.php":            true,

	// Файлы блокировки зависимостей: переносимое описание сборки.
	"package-lock.json": true,
	"yarn.lock":         true,
	"composer.lock":     true,
}

// IsSiteMetadata сообщает, что файл с таким именем - инфраструктура сайта, а
// не содержимое. Проверка регистронезависимая.
func IsSiteMetadata(name string) bool {
	return siteMetadataNames[strings.ToLower(strings.TrimSpace(name))]
}

// IsAPIPath сообщает, что путь ведёт на эндпоинт программного интерфейса.
//
// Отдельная функция нужна потому, что у ответа API расширение бывает вполне
// «контентным»: /wp-json/wp/v2/pages/7105.json неотличим по имени от файла,
// который действительно можно скачать. Фильтр по расширению здесь бессилен,
// поэтому проверяется сам путь.
func IsAPIPath(p string) bool {
	low := strings.ToLower(p)
	for _, m := range apiPathMarkers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

// decorationExts - графические расширения, к которым применяется правило
// оформления сайта. Ограничение области принципиально: эвристика по имени
// ошибается, и цена ошибки для картинки оформления несопоставимо ниже, чем для
// книги или документа с похожим словом в названии.
var decorationExts = map[string]bool{
	"jpg": true, "jpeg": true, "png": true, "gif": true, "webp": true,
	"bmp": true, "svg": true, "tif": true, "tiff": true,
}

// decorationBaseNames - базовые имена графики оформления, без расширения.
// Совпадение точное: список состоит из имён, одинаковых на всех сайтах, как и
// siteMetadataNames.
var decorationBaseNames = map[string]bool{
	"logo": true, "site-logo": true, "sitelogo": true, "logotype": true,
	"favicon": true, "icon": true, "icons": true,
	"sprite": true, "sprites": true,
	"banner": true, "banners": true,
	"bg": true, "background": true,
	"header": true, "footer": true, "nav": true, "navbar": true, "menu": true,
	"thumb": true, "thumbnail": true, "avatar": true,
	"placeholder": true, "spinner": true, "loader": true, "loading": true,
	"button": true, "btn": true, "arrow": true, "bullet": true, "dot": true,
	"spacer": true, "divider": true, "separator": true,
	"pixel": true, "blank": true, "default": true,
	"no-image": true, "noimage": true, "no-photo": true, "nophoto": true,
	"watermark": true, "badge": true, "emoji": true, "smile": true,
}

// decorationNameMarkers - фрагменты имени, по которым узнаётся графика
// оформления, когда базовое имя не совпало целиком: site-logo-dark.png,
// thumbnail_001.jpg, sprite@2x.png.
//
// Намеренно короче списка базовых имён: подстрока ловит и лишнее, поэтому сюда
// вошли только фрагменты без правдоподобного содержательного значения. Слово
// icon входит лишь в формах с разделителем ПЕРЕД ним и проверяется отдельно в
// начале имени (см. hasIconPrefix), потому что подстрока icon_ или icon-
// ложно срабатывает: замер на реальных страницах отбраковал фотографию
// 330px-2026-09-19_WikiCon_2026_in_Regensburg_STP_3416.jpg, в которой
// «wikicon_» содержит «icon_». Слова header, menu, button, badge и flag
// оставлены исключительно в точных базовых именах, потому что
// header-analysis.png и menu.pdf - содержимое.
var decorationNameMarkers = []string{
	"logo", "sprite", "favicon", "banner", "placeholder", "spinner",
	"avatar", "thumb", "watermark", "noimage", "no-image", "nophoto",
	"no-photo", "1x1", "-icon", "_icon",
}

// hasIconPrefix сообщает, что базовое имя начинается с icon или icons, за
// которыми стоит разделитель или конец имени: icon-arrow.png, icon_small.png,
// icons.svg.
//
// Начало имени проверено отдельно от подстрок, потому что это единственная
// безопасная позиция для такого сигнала. «wikicon_2026» содержит «icon_», но не
// начинается с него, «iconography» и «iconology-diagram» начинаются с «icon», но
// дальше идёт буква, а не разделитель, - оба случая остаются содержимым.
func hasIconPrefix(base string) bool {
	var rest string
	switch {
	case strings.HasPrefix(base, "icons"):
		rest = base[len("icons"):]
	case strings.HasPrefix(base, "icon"):
		rest = base[len("icon"):]
	default:
		return false
	}
	if rest == "" {
		return true
	}
	return rest[0] == '-' || rest[0] == '_' || rest[0] == '.'
}

// decorationPathMarkers - фрагменты пути, где лежит графика оформления: темы
// CMS, ассеты сборки, наборы иконок и шрифты.
//
// Общие /images/ и /img/ сюда не входят намеренно: библиотеки и архивы хранят
// там сканы и фотографии, то есть содержимое.
var decorationPathMarkers = []string{
	"/assets/", "/wp-content/themes/", "/wp-includes/", "/skins/", "/theme/",
	"/themes/", "/sprites/", "/icons/", "/fonts/", "/css/", "/js/", "/dist/",
	"/build/", "/static/img/", "/img/icons/", "/img/ui/", "/images/icons/",
	"/images/ui/",
}

// siteDecorationSignal объясняет, почему изображение признано оформлением
// сайта. Пустая строка означает, что файл содержательный.
func siteDecorationSignal(ext, name, urlPath string) string {
	if !decorationExts[ext] {
		return ""
	}

	low := strings.ToLower(strings.TrimSpace(name))
	base := low
	if i := strings.LastIndex(base, "."); i > 0 {
		base = base[:i]
	}
	if decorationBaseNames[base] {
		return "служебное имя " + base
	}
	for _, m := range decorationNameMarkers {
		if strings.Contains(low, m) {
			return "признак оформления " + m + " в имени"
		}
	}
	if hasIconPrefix(base) {
		return "признак оформления icon в начале имени"
	}

	lowPath := strings.ToLower(strings.TrimSpace(urlPath))
	for _, m := range decorationPathMarkers {
		if strings.Contains(lowPath, m) {
			return "путь оформления " + m
		}
	}
	return ""
}

// IsSiteDecoration сообщает, что файл - графика оформления сайта, а не
// самостоятельное изображение.
//
// Правило нужно потому, что точный список siteMetadataNames ловит лишь
// favicon.ico и apple-touch-icon.png, а оформление сайта этими двумя именами не
// ограничивается. Расширять сам siteMetadataNames эвристиками нельзя: его
// комментарий запрещает правила вроде «любое короткое имя», потому что они
// выбросили бы настоящие документы и книги. Здесь область сужена до графики, и
// проверяются три сигнала: точное базовое имя, фрагмент имени и фрагмент пути.
func IsSiteDecoration(ext, name, urlPath string) bool {
	return siteDecorationSignal(strings.ToLower(strings.TrimSpace(ext)), name, urlPath) != ""
}

// CatalogJunkReasonPath объясняет, почему запись каталога содержимым не
// является, принимая уже разобранный путь адреса. Пустая строка означает, что
// запись пригодна.
//
// Отдельная от CatalogJunkReason форма нужна горячему пути: FromURL и probeFile
// разбирают адрес один раз ради имени файла, и второй разбор на каждую ссылку
// страницы был бы чистой тратой.
//
// Это единственная точка, где сведены все критерии отбраковки. Раньше они жили
// в трёх местах независимо - в FromURL, в probeFile коллектора и в
// store.CleanFileCatalog, - и именно расхождение между ними было живым
// дефектом: FromURL отклонял служебные расширения, а probeFile принимал всё из
// KnownExts, поэтому подписи попадали в каталог и тут же вычищались, бесконечно
// добавляясь заново.
//
// Отдельная функция нужна и потому, что «пробный прогон» чистки обязан
// показывать ровно то, что удалит настоящая чистка. При двух реализациях
// критериев расхождение между предпросмотром и фактическим удалением
// неизбежно, а предпросмотр, который врёт, хуже его отсутствия.
func CatalogJunkReasonPath(ext, name, urlPath string) string {
	ext = strings.ToLower(strings.TrimSpace(ext))
	name = strings.TrimSpace(name)
	if !IsContent(ext) {
		if IsMetadata(ext) {
			return "служебное расширение ." + ext
		}
		if ext == "" {
			return "нет расширения"
		}
		return "расширение ." + ext + " не является содержимым"
	}
	if IsSiteMetadata(name) {
		return "служебное имя файла"
	}
	if s := siteDecorationSignal(ext, name, urlPath); s != "" {
		return "графика оформления сайта: " + s
	}
	if IsAPIPath(urlPath) {
		return "эндпоинт API"
	}
	return ""
}

// CatalogJunkReason - форма CatalogJunkReasonPath, принимающая адрес целиком и
// разбирающая его сама. Нужна вызывающим, у которых разобранного пути нет:
// store.CleanFileCatalog работает со строками базы, а предпросмотр чистки в CLI
// печатает причину по сохранённому адресу.
func CatalogJunkReason(ext, name, rawURL string) string {
	p := ""
	if u, err := url.Parse(rawURL); err == nil {
		p = u.Path
	}
	return CatalogJunkReasonPath(ext, name, p)
}

// IsCatalogJunk - короткая форма CatalogJunkReason для вызывающих, которым
// причина не нужна.
func IsCatalogJunk(ext, name, rawURL string) bool {
	return CatalogJunkReason(ext, name, rawURL) != ""
}

// ExtOf достаёт расширение из имени файла: только строчные буквы и цифры,
// не длиннее восьми символов. Мусор вида «file.php?x=1» расширением не
// считается.
func ExtOf(name string) string {
	i := strings.LastIndex(name, ".")
	if i < 0 || i == len(name)-1 {
		return ""
	}
	raw := name[i+1:]
	if len(raw) > 8 {
		return ""
	}
	for _, c := range raw {
		if !unicode.IsLetter(c) && !unicode.IsDigit(c) {
			return ""
		}
	}
	return strings.ToLower(raw)
}

// SafeName приводит имя файла к безопасному виду: убирает управляющие
// символы и ограничивает длину. Имя приходит со чужого сервера и попадает
// в вывод как есть, поэтому обрезаем всё, что может его сломать.
func SafeName(name string) string {
	name = strings.TrimSpace(name)
	var b strings.Builder
	for _, c := range name {
		if unicode.IsControl(c) {
			continue
		}
		b.WriteRune(c)
	}
	name = b.String()
	if len([]rune(name)) > 200 {
		name = string([]rune(name)[:200])
	}
	if name == "" || name == "." || name == ".." {
		return ""
	}
	return name
}

// maxLinkDepth - потолок глубины обхода DOM при сборе ссылок. Равен
// собственному пределу html.Parse (512 открытых элементов), поэтому является
// страховкой, а не обрезанием содержимого. Причина та же, что у
// parser.maxSkeletonDepth: обход рекурсивен, а переполнение стека в Go не
// перехватывается через recover().
const maxLinkDepth = 512

// ExtractLinks собирает ссылки страницы. Схемы javascript, data, mailto и
// tel отбрасываются сразу: это не файлы, и пропускать их дальше значит
// гонять классификатор по мусору.
func ExtractLinks(body, base string) []string {
	doc, err := html.Parse(strings.NewReader(body))
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string

	add := func(v string) {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			return
		}
		low := strings.ToLower(v)
		for _, p := range []string{"javascript:", "data:", "mailto:", "tel:", "about:", "blob:"} {
			if strings.HasPrefix(low, p) {
				return
			}
		}
		seen[v] = true
		out = append(out, v)
	}

	// Ограничение глубины обязательно: обход рекурсивен, а переполнение стека
	// в Go - фатальная ошибка, которую recover() не перехватывает. Потолок
	// совпадает с пределом html.Parse, поэтому реальные страницы не теряют
	// ссылки. ExtractLinks находится на горячем пути обхода (discover.Crawler
	// и catalog.Collector), то есть достижим обычным краулингом враждебного
	// onion-контента, а не только прямым вызовом.
	var walk func(*html.Node, int)
	walk = func(n *html.Node, depth int) {
		if depth >= maxLinkDepth {
			return
		}
		if n.Type == html.ElementNode {
			for _, a := range n.Attr {
				switch strings.ToLower(a.Key) {
				case "href", "src", "data-src", "download":
					add(a.Val)
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, depth+1)
		}
	}
	walk(doc, 0)
	return out
}

// Absolutise превращает ссылку в абсолютную, отсекая схемы, которые не
// ведут на файл. Разметка каталогов часто содержит ссылки без схемы и с
// протокол-относительным началом «//».
func Absolutise(raw, base string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	low := strings.ToLower(raw)
	for _, p := range []string{"javascript:", "data:", "mailto:", "tel:", "#", "about:", "blob:"} {
		if strings.HasPrefix(low, p) {
			return ""
		}
	}
	if strings.HasPrefix(raw, "//") {
		if b, err := url.Parse(base); err == nil {
			raw = b.Scheme + ":" + raw
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	b, err := url.Parse(base)
	if err != nil {
		return ""
	}
	abs := b.ResolveReference(u)
	if abs.Scheme != "http" && abs.Scheme != "https" {
		return ""
	}
	abs.Fragment = ""
	return abs.String()
}

// StripQuery убирает запрос и фрагмент: нужен, когда разные параметры
// ведут на одну и ту же страницу.
func StripQuery(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

// DownloadHint отвечает, стоит ли тратить запрос на проверку ссылки без
// известного расширения. Намёк - слова в пути: сервисы отдают файлы через
// /download, /dl, /get и подобные маршруты.
func DownloadHint(p string) bool {
	low := strings.ToLower(p)
	for _, h := range []string{"download", "/dl/", "/file", "/get", "/attachment", "/uploads/", "/media/", "/dump"} {
		if strings.Contains(low, h) {
			return true
		}
	}
	return false
}

// AttachmentName вытаскивает имя файла из Content-Disposition.
//
// Поддерживаются оба вида параметра - filename= и filename*= в кодировке
// RFC 5987, - в кавычках и без. Приоритет у расширенной формы, как того
// требует RFC 6266. Значение filename*= дополнительно проходит процентное
// декодирование: прежде оно возвращалось как есть, поэтому
// filename*=UTF-8”%D0%BA%D0%BD%D0%B8%D0%B3%D0%B0.epub попадало в каталог
// строкой процентов вместо «книга.epub».
//
// Тип содержимого больше не фильтруется по слову «attachment». Прежняя
// проверка strings.Contains(low, "attachment") отклоняла inline и form-data,
// хотя оба законно несут filename: сервер, отдающий книгу как
// `Content-Disposition: inline; filename="book.epub"`, не давал имени вовсе, и
// каталог получал имя, выведенное из URL, - то самое склеенное из query-строки.
//
// Разбор идёт по одной строке. Прежняя версия искала позицию ключа в lower-копии,
// а срез брала из оригинала: strings.ToLower меняет длину в байтах для части
// символов Unicode, поэтому индексы разъезжались и имя обрезалось не там.
func AttachmentName(disp string) (string, bool) {
	disp = strings.TrimSpace(disp)
	if disp == "" {
		return "", false
	}
	// Расширенная форма важнее обычной, поэтому обход идёт в этом порядке.
	for _, key := range []string{"filename*=", "filename="} {
		v, ok := dispositionParam(disp, key)
		if !ok {
			continue
		}
		if key == "filename*=" {
			v = decodeExtendedFilename(v)
		}
		if v = strings.TrimSpace(v); v != "" {
			return v, true
		}
	}
	return "", false
}

// dispositionParam находит параметр key в заголовке и возвращает его значение
// без кавычек. Поиск регистронезависимый, срез всегда берётся из той же
// строки, в которой искалась позиция.
func dispositionParam(disp, key string) (string, bool) {
	low := strings.ToLower(disp)
	i := strings.Index(low, key)
	if i < 0 {
		return "", false
	}
	// Ключ обязан стоять в начале строки или сразу после разделителя, иначе
	// «xfilename=» совпадёт с «filename=».
	if i > 0 {
		prev := disp[i-1]
		if prev != ';' && prev != ' ' && prev != '\t' {
			return "", false
		}
	}
	v := strings.TrimSpace(disp[i+len(key):])
	if strings.HasPrefix(v, `"`) {
		// Значение в кавычках: ищем закрывающую, а не первый ';', потому что
		// точка с запятой законно встречается внутри имени.
		if end := strings.Index(v[1:], `"`); end >= 0 {
			return v[1 : 1+end], true
		}
		return strings.Trim(v, `"`), true
	}
	if j := strings.IndexByte(v, ';'); j >= 0 {
		v = v[:j]
	}
	return strings.TrimSpace(v), true
}

// decodeExtendedFilename разбирает значение вида charset'lang'percent-encoded
// по RFC 5987.
//
// Формат параметра - charset "'" [ language ] "'" value, то есть разделителей
// всегда два, но язык между ними может быть непустым. Поиск литеральной пары
// "”" здесь неправилен: для filename*=UTF-8'ru'... префикс не снимался, и в
// каталог попадало «UTF-8'ru'книга.pdf» вместо «книга.pdf». Разбор идёт по
// первым двум одинарным кавычкам по отдельности.
//
// LastIndex не годился по другой причине: он обрезал любое имя, в котором такая
// пара встречалась ещё раз.
func decodeExtendedFilename(v string) string {
	if i := strings.IndexByte(v, '\''); i >= 0 {
		if j := strings.IndexByte(v[i+1:], '\''); j >= 0 {
			v = v[i+1+j+1:]
		}
	}
	if decoded, err := url.PathUnescape(v); err == nil && decoded != "" {
		return decoded
	}
	// PathUnescape не принимает '+' как пробел, что здесь и нужно: в имени
	// файла плюс законный символ. Но на ошибке возвращаем исходное значение,
	// а не пустую строку: неполное имя лучше потерянного.
	return v
}

// MimeOf нормализует Content-Type: параметры вроде charset отбрасываются,
// регистр приводится к нижнему.
func MimeOf(ct string) string {
	if ct == "" {
		return ""
	}
	if i := strings.Index(ct, ";"); i >= 0 {
		ct = ct[:i]
	}
	return strings.TrimSpace(strings.ToLower(ct))
}

// PathOf отдаёт путь ссылки без запроса - полезно для вывода, где
// query-строка только мешает.
func PathOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return path.Clean(u.Path)
}

// NormalizeBase приводит адрес сервиса к форме, пригодной для HTTP-запроса:
// добавляет схему, если её нет, и убирает завершающие слеши.
//
// Пул onion-сервисов хранит голые хосты («abcdefghijklmnop.onion»), а не URL.
// Если такой хост отдать клиенту как есть, url.Parse не найдёт схему и запрос
// упадёт с «invalid URL scheme: []» - то есть сервис, который жив и отвечает,
// выглядит мёртвым. Нормализация обязана быть на входе в любой сетевой вызов,
// принимающий адрес из базы.
//
// Схема отделяется от хвоста до обрезки слешей: TrimSuffix снимал только один
// слэш, и «http://host///» превращался в «http://host//». Обрезать хвост
// целиком, не отделяя схему, тоже нельзя - у вырожденного «http://» съелись бы
// слэши самой схемы.
func NormalizeBase(baseURL string) string {
	base := strings.TrimSpace(baseURL)
	if base == "" {
		return ""
	}
	scheme := "http://"
	if i := strings.Index(base, "://"); i >= 0 {
		scheme = base[:i+3]
		base = base[i+3:]
	}
	return scheme + strings.TrimRight(base, "/")
}

// FromURL разбирает ссылку без сети: заполняет имя, расширение и MIME по
// адресу. Размер остаётся нулевым - его знает только сервер.
//
// Ворота - CatalogJunkReasonPath, которая внутри проверяет IsContent, а не
// KnownExt. Разница принципиальна: KnownExt отвечает «знаем ли мы такой MIME»,
// IsContent - «является ли это самостоятельным содержимым». Подписи и
// контрольные суммы MIME имеют, но файлом каталога не являются. При воротах по
// KnownExt коллектор добавлял .asc-подписи, а чистка каталога их тут же
// удаляла: одна и та же запись бесконечно добавлялась и вычищалась, а счётчик
// файлов между прогонами показывал то, чего в каталоге быть не должно.
func FromURL(abs, sourcePage, host string) (Ref, bool) {
	u, err := url.Parse(abs)
	if err != nil {
		return Ref{}, false
	}
	name := path.Base(u.Path)
	if name == "" || name == "/" || name == "." {
		return Ref{}, false
	}
	name = SafeName(name)
	if name == "" {
		return Ref{}, false
	}
	ext := ExtOf(name)
	// Критерии берутся из одной функции, а не повторяются здесь. Три
	// независимые копии этих проверок уже были живым дефектом: FromURL
	// отклонял служебные расширения, а probeFile коллектора принимал всё из
	// KnownExts, поэтому подписи добавлялись в каталог и тут же вычищались,
	// бесконечно возвращаясь.
	//
	// Одно расширение ловит не всё. Служебное имя: manifest.json, robots.txt,
	// opensearch_html.xml, LICENSE.txt и pgp-ключи, отданные как текст, имеют
	// «контентные» расширения .json, .txt и .xml, поэтому по расширению
	// неотличимы от книги или образа. Путь к эндпоинту API:
	// /wp-json/wp/v2/pages/7105.json тоже оканчивается на .json, но это
	// машинальный ответ WordPress, который меняется на каждый запрос и
	// самостоятельным объектом не является. Оба случая наблюдались в реальной
	// базе и составляли заметную долю каталога. Третий случай - графика
	// оформления сайта: без правила по имени и пути logo.png и sprite.svg легли
	// бы в каталог на равных с книгами.
	//
	// Путь передаётся уже разобранным: адрес разобран выше ради имени файла, и
	// второй разбор на каждую ссылку страницы был бы чистой тратой.
	if CatalogJunkReasonPath(ext, name, u.Path) != "" {
		return Ref{}, false
	}
	return Ref{
		URL:        abs,
		Filename:   name,
		Ext:        ext,
		MIME:       MimeForExt(ext),
		SourcePage: sourcePage,
		Host:       host,
	}, true
}
