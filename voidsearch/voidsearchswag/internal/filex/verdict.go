package filex

import (
	"strings"
)

// FileVerdict - короткая устойчивая метка того, что представляет собой файл.
//
// Колонка verdict в каталоге файлов существовала с самого начала, но не
// заполнялась никогда: оба продакшн-места записи (catalog.Collect и
// discover.Pool) опускали поле Verdict при вставке. Поэтому у всех строк
// каталога оно было пустым, а фильтр по verdict в SearchFiles работал только на
// тестах, которые вставляли значения сами. Единственные упоминания меток
// («archive», «executable-risk», «safe-doc») жили в api_test.go как произвольные
// строки, продюсера у них не было.
//
// Метка выводится из расширения и MIME, без сети и без чтения содержимого:
// каталог описывает найденные ссылки, и скачивать файл ради вердикта означало бы
// превратить сбор каталога в загрузку всего даркнета.
//
// Значения устойчивые и короткие, потому что по ним фильтрует SearchFiles и их
// печатает CLI. Русские подписи живут в слое вывода, а не здесь: метка уходит в
// базу и в JSON, где смешение языков мешает сравнению.
const (
	VerdictExecutable = "executable" // исполняемый код и установщики
	VerdictArchive    = "archive"    // контейнеры и образы
	VerdictDocument   = "document"   // документы и таблицы
	VerdictEbook      = "ebook"      // книги для читалок
	VerdictMedia      = "media"      // аудио, видео, изображения
	VerdictData       = "data"       // структурированные данные и дампы
	VerdictSecret     = "secret"     // ключи, сертификаты, базы паролей
	VerdictTorrent    = "torrent"    // раздачи
	VerdictMetadata   = "metadata"   // подписи и контрольные суммы
	VerdictOther      = "other"      // известное расширение без явной категории
	VerdictUnknown    = "unknown"    // расширение не распознано
)

// executableExts - расширения, которые запускаются как код или устанавливают
// его. Отдельная категория нужна потому, что это единственная метка с
// практическим смыслом риска: пользователь, скачивающий файл из даркнета,
// обязан видеть, что это исполняемый код, до того как откроет его.
var executableExts = map[string]bool{
	"exe": true, "msi": true, "dll": true, "apk": true, "deb": true,
	"rpm": true, "scr": true, "com": true, "bat": true, "cmd": true,
	"ps1": true, "vbs": true, "jar": true, "sh": true, "appimage": true,
}

// archiveExts - контейнеры, сжимающие или вмещающие другие файлы, и образы
// дисков. Образы отнесены сюда, а не в отдельную категорию: для пользователя
// это то же самое - «распаковать или смонтировать».
var archiveExts = map[string]bool{
	"zip": true, "rar": true, "7z": true, "tar": true, "gz": true,
	"bz2": true, "xz": true, "tgz": true, "zst": true, "lz4": true,
	"iso": true, "img": true,
}

// documentExts - документы, таблицы и презентации.
var documentExts = map[string]bool{
	"pdf": true, "doc": true, "docx": true, "odt": true, "ods": true,
	"odp": true, "ppt": true, "pptx": true, "rtf": true, "txt": true,
	"md": true, "log": true, "nfo": true, "xls": true, "xlsx": true,
	"djvu": true,
}

// ebookExts - форматы читалок. Выделены отдельно от документов, потому что
// каталог в значительной степени состоит из библиотек, и «книга» против
// «документ» - содержательное различие для поиска по даркнет-библиотекам.
var ebookExts = map[string]bool{
	"epub": true, "mobi": true, "azw": true, "azw3": true, "fb2": true,
	"lit": true, "pdb": true,
}

// mediaExts - аудио, видео и изображения.
var mediaExts = map[string]bool{
	"mp4": true, "mkv": true, "avi": true, "mov": true, "webm": true,
	"flv": true, "wmv": true, "mp3": true, "flac": true, "m4a": true,
	"wav": true, "ogg": true, "opus": true, "aac": true, "jpg": true,
	"jpeg": true, "png": true, "gif": true, "webp": true, "bmp": true,
	"svg": true, "tif": true, "tiff": true,
}

// dataExts - структурированные данные, дампы баз и экспорты.
var dataExts = map[string]bool{
	"csv": true, "json": true, "xml": true, "sql": true, "db": true,
	"sqlite": true, "mdb": true, "yaml": true, "yml": true, "tsv": true,
	"parquet": true, "dat": true, "bin": true,
}

// secretExts - ключи, сертификаты и базы паролей. Отдельная категория потому,
// что такие файлы в открытом каталоге - находка с практическим смыслом, и её
// стоит отличать от служебной подписи.
var secretExts = map[string]bool{
	"kdbx": true, "pgp": true, "gpg": true, "pem": true, "key": true,
	"crt": true, "cer": true, "der": true, "p12": true, "pfx": true,
}

// ClassifyFile возвращает метку вердикта для файла по его расширению.
//
// Пустое расширение даёт VerdictUnknown, а не VerdictOther: «неизвестно» и
// «известно, но без категории» - разные ответы, и смешивать их значит потерять
// сигнал о том, что ссылку не удалось разобрать.
//
// То же различие соблюдается и для непустого расширения: VerdictOther
// возвращается только тогда, когда расширение значится в KnownExts, то есть
// каталог его действительно знает, но категории для него нет. Нераспознанное
// расширение даёт VerdictUnknown.
func ClassifyFile(ext string) string {
	e := strings.ToLower(strings.TrimSpace(ext))
	e = strings.TrimLeft(e, ".")
	if e == "" {
		return VerdictUnknown
	}

	// Служебные артефакты проверяются первыми: часть их расширений (pem, key,
	// crt) входит и в secretExts, а подпись без подписанного файла - это
	// служебные данные, а не находка. Порядок согласован с IsMetadata, которую
	// каталог использует для отсева мусора.
	if IsMetadata(e) {
		return VerdictMetadata
	}
	switch {
	case executableExts[e]:
		return VerdictExecutable
	case archiveExts[e]:
		return VerdictArchive
	case ebookExts[e]:
		return VerdictEbook
	case documentExts[e]:
		return VerdictDocument
	case mediaExts[e]:
		return VerdictMedia
	case dataExts[e]:
		return VerdictData
	case secretExts[e]:
		return VerdictSecret
	case e == "torrent":
		return VerdictTorrent
	}

	// Категория не найдена, и здесь два разных случая, которые прежняя версия
	// смешивала: она возвращала other для любого непустого расширения, поэтому
	// мусорное «qqq» получало метку, утверждающую, что каталог это расширение
	// знает. Сигнал «ссылку не удалось разобрать» терялся ровно там, где он
	// нужен, - в фильтре -verdict unknown.
	//
	// Источник знания о расширении - KnownExts, тот же список, по которому
	// IsContent пускает файл в каталог. Благодаря этому other и «каталог файл
	// принимает» остаются согласованными: other не может появиться у строки,
	// которая не прошла бы ворота содержимого.
	//
	// Исключения из KnownExts (com и форматы изображений, см. комментарий к
	// списку) до этой ветки не доходят: com отнесён к исполняемому,
	// изображения - к медиа, то есть категория у них есть.
	if KnownExt(e) {
		return VerdictOther
	}
	return VerdictUnknown
}

// ClassifyRef возвращает метку вердикта для найденного файла.
//
// Расширение берётся из Ref.Ext, а при пустом выводится из имени файла: сборщик
// не всегда заполняет Ext, но имя обычно есть. Это даёт метку там, где иначе
// было бы «unknown» при вполне распознаваемом файле.
func ClassifyRef(r Ref) string {
	if ext := strings.TrimSpace(r.Ext); ext != "" {
		return ClassifyFile(ext)
	}
	if name := strings.TrimSpace(r.Filename); name != "" {
		if ext := ExtOf(name); ext != "" {
			return ClassifyFile(ext)
		}
	}
	return VerdictUnknown
}

// RiskVerdicts - метки, требующие предупреждения пользователя.
//
// Список вынесен в переменную, потому что он нужен в трёх местах: VerdictIsRisk
// для вывода предупреждения, store.SearchFiles для фильтра «только рискованные»
// и инструменты MCP. Дублирование списка в SQL-запросе означало бы, что при
// расширении категорий правка в одном месте расходилась бы с остальными.
var RiskVerdicts = []string{VerdictExecutable, VerdictSecret}

// AllVerdicts возвращает все метки, которые может выдать ClassifyFile.
//
// Нужно справке CLI: пользователь обязан видеть доступные значения фильтра
// -verdict, а не угадывать их. Список собран из констант, поэтому добавление
// категории обновляет и справку.
func AllVerdicts() []string {
	return []string{
		VerdictExecutable, VerdictArchive, VerdictDocument, VerdictEbook,
		VerdictMedia, VerdictData, VerdictSecret, VerdictTorrent,
		VerdictMetadata, VerdictOther, VerdictUnknown,
	}
}

// VerdictIsRisk сообщает, требует ли метка предупреждения пользователя.
//
// Выделено в функцию, а не в сравнение со строкой на месте, потому что смысл
// метки важнее её текста: если список опасных категорий расширится, правка в
// одном месте обновит и вывод CLI, и инструменты MCP.
//
// Вход приводится к нижнему регистру. ClassifyFile всегда выдаёт метки в нижнем
// регистре, но строка может прийти и снаружи - из фильтра, который пользователь
// набрал в CLI, или из JSON, собранного другим инструментом. Отказывать в
// предупреждении из-за регистра значило бы пропустить риск ровно в тот момент,
// когда предупреждение нужнее всего.
func VerdictIsRisk(verdict string) bool {
	v := NormalizeVerdict(verdict)
	for _, r := range RiskVerdicts {
		if v == r {
			return true
		}
	}
	return false
}

// NormalizeVerdict приводит метку к тому виду, в котором она хранится и
// сравнивается: нижний регистр и без пробелов по краям.
//
// Правило вынесено сюда, потому что оно нужно трём разным слоям, и каждый из них
// раньше приводил вход сам: проверка риска в filex, разбор фильтра вердиктов в
// store и объяснение метки словами в выводе CLI. Расхождение правил даёт
// видимую пользователю поломку: метка «EXECUTABLE» распознавалась как риск, но
// объяснение уходило в ветку по умолчанию, а фильтр -verdict executable запись не
// находил, потому что сравнивал нормализованный ввод с ненормализованной
// колонкой.
//
// ClassifyFile всегда выдаёт нижний регистр, но метка приходит и снаружи: из
// фильтра, который пользователь набрал в CLI, из JSON, собранного другим
// инструментом, и из AddFile, который принимает verdict как поле записи.
func NormalizeVerdict(verdict string) string {
	return strings.ToLower(strings.TrimSpace(verdict))
}
