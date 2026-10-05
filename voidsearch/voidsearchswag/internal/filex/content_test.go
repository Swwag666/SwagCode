package filex

import (
	"strings"
	"testing"
)

func TestIsContentKnownContentExts(t *testing.T) {
	for _, ext := range []string{"zip", "pdf", "mp4", "txt", "json", "epub", "gz", "7z"} {
		if !IsContent(ext) {
			t.Errorf("%s не признан содержимым", ext)
		}
	}
}

func TestIsContentRejectsMetadata(t *testing.T) {
	// Подписи и контрольные суммы описывают другой файл, а не являются им.
	// В каталоге они раздувают счётчик и создают видимость раздачи контента.
	for _, ext := range []string{"asc", "sig", "md5", "sha1", "sha256", "sha512", "sfv", "pem", "crt", "cer", "der", "key"} {
		if IsContent(ext) {
			t.Errorf("служебный %s признан содержимым", ext)
		}
		if !IsMetadata(ext) {
			t.Errorf("%s не признан служебным", ext)
		}
	}
}

func TestIsContentDeliberateContentCases(t *testing.T) {
	// Эти расширения выглядят служебными, но являются самостоятельным
	// содержимым: torrent - объект раздачи, gpg - зашифрованный архив.
	for _, ext := range []string{"torrent", "gpg"} {
		if !IsContent(ext) {
			t.Errorf("%s ошибочно отнесён к служебным", ext)
		}
		if IsMetadata(ext) {
			t.Errorf("%s ошибочно помечен служебным", ext)
		}
	}
}

func TestIsContentRejectsUnknownAndTLD(t *testing.T) {
	// Неизвестное расширение контентом не считается: сначала оно должно
	// попасть в KnownExts. Доменные зоны - тем более.
	for _, ext := range []string{"", "onion", "i2p", "php", "com", "html", "css", "js", "sig", "sha256"} {
		if IsContent(ext) {
			t.Errorf("%q признано содержимым", ext)
		}
	}
}

func TestIsContentAcceptsExecutable(t *testing.T) {
	// Исполняемый файл - самостоятельное содержимое каталога, а не служебные
	// данные о другом файле.
	if !IsContent("exe") {
		t.Error("exe не признан содержимым")
	}
	if IsMetadata("exe") {
		t.Error("exe помечен служебным")
	}
}

func TestIsContentNormalizesCase(t *testing.T) {
	for _, ext := range []string{"ASC", "Asc", " asc ", "PDF", " Pdf "} {
		want := IsContent(strings.ToLower(strings.TrimSpace(ext)))
		if got := IsContent(ext); got != want {
			t.Errorf("IsContent(%q)=%v, ожидала %v", ext, got, want)
		}
	}
	if !IsContent("  PDF  ") {
		t.Error("пробелы и регистр не нормализованы")
	}
	if IsContent("  ASC  ") {
		t.Error("служебное расширение прошло после нормализации")
	}
}

func TestIsMetadataFalseForContent(t *testing.T) {
	for _, ext := range []string{"zip", "pdf", "mp4", "onion", "", "php"} {
		if IsMetadata(ext) {
			t.Errorf("%q помечено служебным", ext)
		}
	}
}

func TestKnownExtStillAcceptsMetadata(t *testing.T) {
	// Разделение не должно ломать определение MIME: служебные расширения,
	// которые входят в KnownExts, обязаны сохранять свой MIME - иначе их
	// нельзя корректно описать в ответе.
	for _, ext := range []string{"asc", "pem", "crt", "key"} {
		if !KnownExt(ext) {
			t.Errorf("%s выпал из KnownExts", ext)
		}
		if MimeForExt(ext) == "" {
			t.Errorf("у %s пропал MIME", ext)
		}
		if !IsMetadata(ext) {
			t.Errorf("%s не помечен служебным", ext)
		}
		if IsContent(ext) {
			t.Errorf("%s признан содержимым", ext)
		}
	}
}

func TestMetadataClassificationIndependentOfKnownExts(t *testing.T) {
	// Части этих расширений нет в KnownExts, но классификация обязана
	// оставаться верной: если их когда-нибудь добавят в список содержимого,
	// они не должны начать считаться контентом.
	for _, ext := range []string{"sig", "md5", "sha1", "sha256", "sha512", "sfv", "cer", "der"} {
		if !IsMetadata(ext) {
			t.Errorf("%s не помечен служебным", ext)
		}
		if IsContent(ext) {
			t.Errorf("%s признан содержимым", ext)
		}
	}
}

func TestMetadataExtsAreNeverContent(t *testing.T) {
	// Инвариант для всей карты: ни одно служебное расширение не должно
	// проходить как содержимое, иначе чистка каталога его пропустит.
	for ext := range MetadataExts {
		if IsContent(ext) {
			t.Errorf("служебное %s прошло как содержимое", ext)
		}
	}
}

func TestFromURLRejectsMetadataFiles(t *testing.T) {
	// Ворота FromURL обязаны совпадать с чисткой каталога. Когда FromURL
	// пропускал всё из KnownExts, коллектор добавлял .asc-подписи, а
	// CleanFileCatalog их тут же удалял: запись бесконечно добавлялась и
	// вычищалась, а счётчик файлов между прогонами показывал лишнее.
	const host = "libraryfyuybp7oyidyya3ah5xvwgyx6weauoini7zyz555litmmumad.onion"
	reject := []string{
		"http://" + host + "/dist.iso.asc",
		"http://" + host + "/ca.pem",
		"http://" + host + "/host.crt",
		"http://" + host + "/priv.key",
		"http://" + host + "/CHECKSUMS.sha256",
		"http://" + host + "/release.sig",
	}
	for _, u := range reject {
		if ref, ok := FromURL(u, "http://"+host+"/", host); ok {
			t.Errorf("служебный файл принят на входе: %+v", ref)
		}
	}

	accept := []string{
		"http://" + host + "/book.epub",
		"http://" + host + "/archive.zip",
		"http://" + host + "/release.torrent",
		"http://" + host + "/sealed.gpg",
		"http://" + host + "/manual.pdf",
	}
	for _, u := range accept {
		if _, ok := FromURL(u, "http://"+host+"/", host); !ok {
			t.Errorf("содержимое отвергнуто на входе: %s", u)
		}
	}
}

func TestFromURLKeepsMimeForAcceptedContent(t *testing.T) {
	const host = "abcdefghijklmnop.onion"
	ref, ok := FromURL("http://"+host+"/book.epub", "http://"+host+"/", host)
	if !ok {
		t.Fatal("epub отвергнут")
	}
	if ref.MIME == "" {
		t.Error("MIME не заполнен")
	}
	if ref.Ext != "epub" {
		t.Errorf("ext=%q", ref.Ext)
	}
}

func TestMimeForExtStillKnowsMetadata(t *testing.T) {
	// Отказ пускать служебные файлы в каталог не отменяет знания их MIME:
	// он нужен, чтобы описать найденное в отчёте или в ответе поисковика.
	for _, ext := range []string{"asc", "pem", "crt", "key"} {
		if MimeForExt(ext) == "" {
			t.Errorf("у %s пропал MIME", ext)
		}
	}
}
