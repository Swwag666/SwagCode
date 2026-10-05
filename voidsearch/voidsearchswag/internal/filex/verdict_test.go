package filex

import (
	"testing"
)

func TestClassifyFileCategories(t *testing.T) {
	cases := []struct {
		ext  string
		want string
	}{
		// Исполняемый код и установщики.
		{"exe", VerdictExecutable},
		{"msi", VerdictExecutable},
		{"dll", VerdictExecutable},
		{"apk", VerdictExecutable},
		{"deb", VerdictExecutable},
		{"rpm", VerdictExecutable},
		{"bat", VerdictExecutable},
		{"ps1", VerdictExecutable},
		{"sh", VerdictExecutable},
		{"jar", VerdictExecutable},
		{"scr", VerdictExecutable},

		// Архивы и образы.
		{"zip", VerdictArchive},
		{"rar", VerdictArchive},
		{"7z", VerdictArchive},
		{"tar", VerdictArchive},
		{"gz", VerdictArchive},
		{"tgz", VerdictArchive},
		{"iso", VerdictArchive},
		{"img", VerdictArchive},

		// Книги отделены от документов: каталог в значительной степени состоит
		// из библиотек, и это различие содержательно для поиска по ним.
		{"epub", VerdictEbook},
		{"mobi", VerdictEbook},
		{"fb2", VerdictEbook},
		{"azw3", VerdictEbook},
		{"djvu", VerdictDocument},

		// Документы и таблицы.
		{"pdf", VerdictDocument},
		{"doc", VerdictDocument},
		{"docx", VerdictDocument},
		{"xlsx", VerdictDocument},
		{"rtf", VerdictDocument},
		{"txt", VerdictDocument},
		{"md", VerdictDocument},
		{"nfo", VerdictDocument},

		// Медиа.
		{"mp4", VerdictMedia},
		{"mkv", VerdictMedia},
		{"mp3", VerdictMedia},
		{"flac", VerdictMedia},
		{"jpg", VerdictMedia},
		{"png", VerdictMedia},

		// Данные и дампы.
		{"csv", VerdictData},
		{"json", VerdictData},
		{"xml", VerdictData},
		{"sql", VerdictData},
		{"db", VerdictData},
		{"sqlite", VerdictData},
		{"bin", VerdictData},

		// Секреты.
		{"kdbx", VerdictSecret},
		{"pgp", VerdictSecret},
		{"gpg", VerdictSecret},
		{"p12", VerdictSecret},

		// Раздачи.
		{"torrent", VerdictTorrent},

		// Служебные артефакты.
		{"sig", VerdictMetadata},
		{"md5", VerdictMetadata},
		{"sha256", VerdictMetadata},
		{"asc", VerdictMetadata},
	}
	for _, c := range cases {
		if got := ClassifyFile(c.ext); got != c.want {
			t.Errorf("ClassifyFile(%q) = %q, ожидала %q", c.ext, got, c.want)
		}
	}
}

func TestClassifyFileNormalizesInput(t *testing.T) {
	// Расширение приходит из разных источников: из URL, из Content-Disposition,
	// из базы. Регистр, пробелы и точка встречаются в любом сочетании.
	for _, ext := range []string{"EXE", "Exe", " exe ", ".exe", ".EXE", " .exe "} {
		if got := ClassifyFile(ext); got != VerdictExecutable {
			t.Errorf("ClassifyFile(%q) = %q, ожидала %q", ext, got, VerdictExecutable)
		}
	}
}

func TestClassifyFileEmptyIsUnknown(t *testing.T) {
	// Пустое расширение - «неизвестно», а не «прочее». Это разные ответы:
	// «unknown» означает, что ссылку не удалось разобрать, а «other» - что
	// расширение распознано, но категории нет. Смешивание потеряло бы сигнал.
	for _, ext := range []string{"", "   ", ".", ".."} {
		if got := ClassifyFile(ext); got != VerdictUnknown {
			t.Errorf("ClassifyFile(%q) = %q, ожидала %q", ext, got, VerdictUnknown)
		}
	}
}

// TestClassifyFileUnrecognizedIsUnknown и
// TestClassifyFileKnownWithoutCategoryIsOther проверяют две половины одного
// различия: «не распознано» против «распознано, но без категории».
//
// Прежний тест TestClassifyFileUnknownExtIsOther держал в одном списке
// мусорные zzz и qqq рядом с настоящими eml, vcf и pst и допускал любой из двух
// ответов, поэтому не различал ровно то, что обязан различать классификатор:
// ClassifyFile возвращал other для любого непустого расширения, и тест это
// проглатывал.
func TestClassifyFileUnrecognizedIsUnknown(t *testing.T) {
	for _, ext := range []string{"zzz", "qqq", "abc123", "exe2", "mp99"} {
		if KnownExt(ext) {
			t.Fatalf("фикстура неверна: %q значится в KnownExts, случай не подходит", ext)
		}
		if got := ClassifyFile(ext); got != VerdictUnknown {
			t.Errorf("ClassifyFile(%q) = %q, ожидала %q", ext, got, VerdictUnknown)
		}
	}
}

func TestClassifyFileKnownWithoutCategoryIsOther(t *testing.T) {
	for _, ext := range []string{"vcf", "eml", "pst"} {
		if !KnownExt(ext) {
			t.Fatalf("фикстура неверна: %q не значится в KnownExts", ext)
		}
		if !IsContent(ext) {
			t.Fatalf("фикстура неверна: %q не проходит фильтр содержимого", ext)
		}
		if got := ClassifyFile(ext); got != VerdictOther {
			t.Errorf("ClassifyFile(%q) = %q, ожидала %q", ext, got, VerdictOther)
		}
	}
}

// TestNoKnownExtIsUnknown доводит различие до инварианта: unknown означает
// «каталог расширение не знает», поэтому ни одно расширение из KnownExts не
// может давать такую метку - ни категорию, ни other, ни metadata.
//
// До правки ClassifyFile тест был бы тривиально зелёным: other возвращался для
// любого непустого расширения, и unknown не доставался никому, кроме пустой
// строки.
func TestNoKnownExtIsUnknown(t *testing.T) {
	checked := 0
	for ext := range KnownExts {
		checked++
		if got := ClassifyFile(ext); got == VerdictUnknown {
			t.Errorf("известное каталогу %q получило метку %q", ext, got)
		}
	}
	if checked == 0 {
		t.Fatal("KnownExts пуст, тест ничего не проверил")
	}
}

// TestClassifyRefUnrecognizedExtIsUnknown проверяет тот же переход по пути,
// которым пользуется заполнение вердиктов: расширение выводится из имени файла.
func TestClassifyRefUnrecognizedExtIsUnknown(t *testing.T) {
	if got := ClassifyRef(Ref{Filename: "archive.qqq"}); got != VerdictUnknown {
		t.Errorf("имя с мусорным расширением дало %q, ожидала %q", got, VerdictUnknown)
	}
	if got := ClassifyRef(Ref{Filename: "card.vcf"}); got != VerdictOther {
		t.Errorf("имя с известным расширением без категории дало %q, ожидала %q", got, VerdictOther)
	}
	// Пустое расширение в Ref и имя без точки - по-прежнему unknown.
	if got := ClassifyRef(Ref{Filename: "download"}); got != VerdictUnknown {
		t.Errorf("имя без расширения дало %q, ожидала %q", got, VerdictUnknown)
	}
}

func TestClassifyFileMetadataWinsOverSecret(t *testing.T) {
	// pem, key и crt входят и в MetadataExts, и в secretExts. Служебная
	// проверка обязана идти первой: подпись без подписанного файла - служебные
	// данные, а не находка. Порядок согласован с IsMetadata, по которой каталог
	// отсеивает мусор.
	for _, ext := range []string{"pem", "key", "crt", "cer", "der"} {
		if got := ClassifyFile(ext); got != VerdictMetadata {
			t.Errorf("ClassifyFile(%q) = %q, ожидала metadata", ext, got)
		}
		if IsMetadata(ext) != true {
			t.Errorf("IsMetadata(%q) = false: классификатор разошёлся с отсевом мусора", ext)
		}
	}
	// kdbx и pgp служебными не являются, поэтому остаются секретами.
	for _, ext := range []string{"kdbx", "pgp", "gpg"} {
		if got := ClassifyFile(ext); got != VerdictSecret {
			t.Errorf("ClassifyFile(%q) = %q, ожидала secret", ext, got)
		}
	}
}

func TestClassifyRefUsesExt(t *testing.T) {
	r := Ref{URL: "http://a.onion/book.epub", Filename: "book.epub", Ext: "epub"}
	if got := ClassifyRef(r); got != VerdictEbook {
		t.Errorf("ClassifyRef = %q, ожидала %q", got, VerdictEbook)
	}
}

func TestClassifyRefFallsBackToFilename(t *testing.T) {
	// Сборщик не всегда заполняет Ext, но имя обычно есть. Без вывода из имени
	// метка была бы «unknown» при вполне распознаваемом файле.
	r := Ref{URL: "http://a.onion/setup.exe", Filename: "setup.exe"}
	if got := ClassifyRef(r); got != VerdictExecutable {
		t.Errorf("ClassifyRef без Ext = %q, ожидала %q", got, VerdictExecutable)
	}
}

func TestClassifyRefExtWinsOverFilename(t *testing.T) {
	// Если Ext заполнен, он авторитетнее имени: имя могло прийти из
	// Content-Disposition и не совпадать с реальным расширением.
	r := Ref{URL: "http://a.onion/x", Filename: "readme.txt", Ext: "exe"}
	if got := ClassifyRef(r); got != VerdictExecutable {
		t.Errorf("ClassifyRef = %q, ожидала %q по Ext", got, VerdictExecutable)
	}
}

func TestClassifyRefEmpty(t *testing.T) {
	if got := ClassifyRef(Ref{}); got != VerdictUnknown {
		t.Errorf("ClassifyRef пустого Ref = %q, ожидала %q", got, VerdictUnknown)
	}
	// Пустой Ref с URL без расширения тоже unknown: выводить не из чего.
	r := Ref{URL: "http://a.onion/download?id=5"}
	if got := ClassifyRef(r); got != VerdictUnknown {
		t.Errorf("ClassifyRef без имени и Ext = %q, ожидала %q", got, VerdictUnknown)
	}
}

func TestVerdictIsRisk(t *testing.T) {
	// Смысл метки важнее её текста: если список опасных категорий расширится,
	// правка в одном месте обновит и вывод CLI, и инструменты MCP.
	for _, v := range []string{VerdictExecutable, VerdictSecret} {
		if !VerdictIsRisk(v) {
			t.Errorf("VerdictIsRisk(%q) = false", v)
		}
	}
	for _, v := range []string{VerdictArchive, VerdictDocument, VerdictEbook,
		VerdictMedia, VerdictData, VerdictTorrent, VerdictMetadata,
		VerdictOther, VerdictUnknown, ""} {
		if VerdictIsRisk(v) {
			t.Errorf("VerdictIsRisk(%q) = true", v)
		}
	}
}

func TestVerdictIsRiskNormalizesInput(t *testing.T) {
	for _, v := range []string{"EXECUTABLE", " executable ", "Executable"} {
		if !VerdictIsRisk(v) {
			t.Errorf("VerdictIsRisk(%q) = false", v)
		}
	}
}

func TestClassifyFileConsistentWithContentFilter(t *testing.T) {
	// Каталог пишет только файлы, прошедшие IsContent. Проверяю, что для них
	// вердикт никогда не получается «unknown»: иначе колонка осталась бы
	// бессмысленной ровно там, где она нужна.
	for ext := range KnownExts {
		if !IsContent(ext) {
			continue
		}
		got := ClassifyFile(ext)
		if got == VerdictUnknown {
			t.Errorf("у контента %q вердикт unknown", ext)
		}
		if got == "" {
			t.Errorf("у контента %q вердикт пустой", ext)
		}
	}
}

func TestClassifyFileCoversKnownContentExts(t *testing.T) {
	// Обратная проверка: содержательные расширения не должны сваливаться в
	// «other» там, где категория очевидна. Беру выборку, которую пользователь
	// реально видит в каталоге даркнет-библиотек.
	expect := map[string]string{
		"pdf": VerdictDocument, "epub": VerdictEbook, "mobi": VerdictEbook,
		"fb2": VerdictEbook, "zip": VerdictArchive, "exe": VerdictExecutable,
		"mp4": VerdictMedia, "csv": VerdictData, "torrent": VerdictTorrent,
	}
	for ext, want := range expect {
		if got := ClassifyFile(ext); got != want {
			t.Errorf("ClassifyFile(%q) = %q, ожидала %q", ext, got, want)
		}
		if got := ClassifyFile(ext); got == VerdictOther {
			t.Errorf("очевидная категория %q свалилась в other", ext)
		}
	}
}

func TestClassifyFileDeterministic(t *testing.T) {
	// Метка уходит в базу и используется в фильтре, поэтому обязана быть
	// устойчивой между вызовами.
	for _, ext := range []string{"exe", "pdf", "epub", "", "zzz"} {
		first := ClassifyFile(ext)
		for i := 0; i < 5; i++ {
			if got := ClassifyFile(ext); got != first {
				t.Errorf("ClassifyFile(%q) нестабилен: %q против %q", ext, got, first)
			}
		}
	}
}
