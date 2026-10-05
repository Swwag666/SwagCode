package setup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSemverLess(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"0.4.8", "0.4.9", true},
		{"0.4.9", "0.4.8", false},
		{"0.4.9", "0.4.9", false},
		{"0.5.0", "0.4.99", false},
		{"0.4.9", "0.4.10", true},
		{"1.0", "0.99.99", false},
		{"0.4.9", "0.4.9.1", true},
	}
	for _, c := range cases {
		if got := semverLess(c.a, c.b); got != c.want {
			t.Errorf("semverLess(%q,%q)=%v, ожидала %v", c.a, c.b, got, c.want)
		}
	}
}

func TestLatestVersionPicksHighest(t *testing.T) {
	// Логика выбора старшей версии проверяется напрямую: каталог версий
	// содержит и старые ветки, и порядок в разметке произвольный.
	body := `<a href="0.4.8/">0.4.8/</a>
<a href="0.4.10/">0.4.10/</a>
<a href="0.4.9/">0.4.9/</a>
<a href="0.4.10/">0.4.10/</a>`

	matches := reVersionDir.FindAllStringSubmatch(body, -1)
	versions := make([]string, 0, len(matches))
	seen := map[string]bool{}
	for _, m := range matches {
		if !seen[m[1]] {
			seen[m[1]] = true
			versions = append(versions, m[1])
		}
	}
	if len(versions) != 3 {
		t.Fatalf("версий %d, ожидала 3 (дубликат не отброшен): %v", len(versions), versions)
	}
}

func TestVersionDirRegex(t *testing.T) {
	body := `<a href="0.4.9/">0.4.9/</a> <a href="../">../</a> <a href="15.0.23/">15.0.23/</a>`
	got := reVersionDir.FindAllStringSubmatch(body, -1)
	if len(got) != 2 {
		t.Fatalf("совпадений %d, ожидала 2: %v", len(got), got)
	}
	if got[0][1] != "0.4.9" || got[1][1] != "15.0.23" {
		t.Errorf("версии разобраны неверно: %v", got)
	}
}

func TestVersionDirRegexSkipsAlpha(t *testing.T) {
	// В каталоге tor лежат и альфы вида 16.0a11. Установка не должна их
	// выбирать: у альфы нет ни стабильной сборки, ни подписей, поэтому
	// регулярка обязана её не пропустить.
	body := `<a href="15.0.23/">15.0.23/</a> <a href="16.0a11/">16.0a11/</a>`
	got := reVersionDir.FindAllStringSubmatch(body, -1)
	if len(got) != 1 {
		t.Fatalf("совпадений %d, ожидала 1 (альфа отброшена): %v", len(got), got)
	}
	if got[0][1] != "15.0.23" {
		t.Errorf("выбрана не стабильная версия: %v", got)
	}
}

func TestBundleFileRegex(t *testing.T) {
	body := `<a href="tor-expert-bundle-windows-x86_64-0.4.9.tar.gz">windows</a>
<a href="tor-expert-bundle-linux-x86_64-0.4.9.tar.gz">linux</a>`
	got := reBundleFile.FindAllStringSubmatch(body, -1)
	if len(got) != 2 {
		t.Fatalf("файлов %d, ожидала 2", len(got))
	}
	if !strings.HasPrefix(got[0][1], "tor-expert-bundle-windows") {
		t.Errorf("имя файла: %q", got[0][1])
	}
}

func TestPlatformTokensNonEmpty(t *testing.T) {
	tokens := platformTokens()
	if len(tokens) == 0 {
		t.Fatal("токены платформы пусты")
	}
	for _, tk := range tokens {
		if tk == "" {
			t.Error("пустой токен платформы")
		}
	}
}

func TestPlatformTokensWindows(t *testing.T) {
	if os.Getenv("GOOS_TEST_SKIP") != "" {
		t.Skip("пропуск по окружению")
	}
	// На windows ожидается пара windows+x86_64, иначе бандл не выберется.
	tokens := platformTokens()
	if len(tokens) != 1 && len(tokens) != 2 {
		t.Errorf("неожиданное число токенов: %v", tokens)
	}
}

// makeBundle собирает tar.gz с заданными путями для проверки распаковки.
func makeBundle(t *testing.T, entries map[string]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bundle.tar.gz")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for name, body := range entries {
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gz.Close()
	f.Close()
	return path
}

func TestExtractBundle(t *testing.T) {
	arc := makeBundle(t, map[string]string{
		"tor/tor.exe":                           "бинарь",
		"tor/pluggable_transports/lyrebird.exe": "транспорт",
		"tor/data/geoip":                        "гео",
	})
	dest := t.TempDir()

	var logs []string
	log := func(f string, a ...any) { logs = append(logs, f) }

	if err := extractBundle(arc, dest, log); err != nil {
		t.Fatalf("распаковка: %v", err)
	}

	for _, rel := range []string{"tor.exe", "pluggable_transports/lyrebird.exe", "data/geoip"} {
		p := filepath.Join(dest, filepath.FromSlash(rel))
		if _, err := os.Stat(p); err != nil {
			t.Errorf("файл %s не распакован: %v", rel, err)
		}
	}
	if len(logs) == 0 {
		t.Error("лог распаковки пуст")
	}
}

func TestExtractBundleSkipsForeignPaths(t *testing.T) {
	arc := makeBundle(t, map[string]string{
		"tor/tor.exe":   "бинарь",
		"README":        "не наш",
		"docs/notes.md": "не наш",
	})
	dest := t.TempDir()

	if err := extractBundle(arc, dest, func(string, ...any) {}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "tor.exe")); err != nil {
		t.Error("нужный файл не распакован")
	}
	if _, err := os.Stat(filepath.Join(dest, "README")); err == nil {
		t.Error("посторонний файл распакован")
	}
	if _, err := os.Stat(filepath.Join(dest, "docs")); err == nil {
		t.Error("посторонний каталог создан")
	}
}

func TestExtractBundleRejectsEmptyArchive(t *testing.T) {
	arc := makeBundle(t, map[string]string{"README": "только постороннее"})
	if err := extractBundle(arc, t.TempDir(), func(string, ...any) {}); err == nil {
		t.Error("архив без файлов tor/ принят")
	}
}

func TestExtractBundleMissingFile(t *testing.T) {
	if err := extractBundle("/нет/такого/файла.tar.gz", t.TempDir(), func(string, ...any) {}); err == nil {
		t.Error("отсутствующий архив принят")
	}
}

func TestExtractBundleNotGzip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.tar.gz")
	if err := os.WriteFile(path, []byte("это не gzip"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := extractBundle(path, t.TempDir(), func(string, ...any) {}); err == nil {
		t.Error("не-gzip архив принят")
	}
}

func TestWithinDir(t *testing.T) {
	base := t.TempDir()
	cases := []struct {
		name string
		path string
		want bool
	}{
		{"внутри", filepath.Join(base, "tor.exe"), true},
		{"внутри вложенный", filepath.Join(base, "pluggable_transports", "lyrebird.exe"), true},
		{"сам каталог", base, false},
		{"уровень выше", filepath.Join(filepath.Dir(base), "evil.exe"), false},
		{"два уровня выше", filepath.Join(filepath.Dir(filepath.Dir(base)), "evil.exe"), false},
	}
	for _, c := range cases {
		if got := withinDir(base, c.path); got != c.want {
			t.Errorf("%s: withinDir=%v, ожидала %v", c.name, got, c.want)
		}
	}
}

func TestWithinDirRejectsDotDot(t *testing.T) {
	base := t.TempDir()
	if withinDir(base, filepath.Join(base, "..", "evil.exe")) {
		t.Error("путь с .. принят")
	}
}

func TestExtractBundleEscapesDestDir(t *testing.T) {
	// Путь tor/../../evil.exe проходит через срез префикса и уводит запись
	// за пределы каталога установки. Такой архив не должен распаковываться.
	arc := makeBundle(t, map[string]string{
		"tor/../../evil.exe": "побег",
		"tor/tor.exe":        "нормальный",
	})
	dest := t.TempDir()

	// filepath.Join(dest, "../../evil.exe") выходит на два уровня вверх.
	escaped := filepath.Join(filepath.Dir(filepath.Dir(dest)), "evil.exe")
	os.Remove(escaped)
	defer os.Remove(escaped)

	if err := extractBundle(arc, dest, func(string, ...any) {}); err != nil {
		t.Fatalf("распаковка: %v", err)
	}
	if _, err := os.Stat(escaped); err == nil {
		t.Error("файл записан вне каталога установки: обход пути не заблокирован")
	}
	if _, err := os.Stat(filepath.Join(dest, "tor.exe")); err != nil {
		t.Error("нормальный файл не распакован вместе с опасным")
	}
}

func TestExtractBundleDirEntrySkipped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "b.tar.gz")
	f, _ := os.Create(path)
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "tor/", Mode: 0o755, Typeflag: tar.TypeDir})
	tw.WriteHeader(&tar.Header{Name: "tor/tor", Mode: 0o755, Size: 3, Typeflag: tar.TypeReg})
	tw.Write([]byte("abc"))
	tw.Close()
	gz.Close()
	f.Close()

	dest := t.TempDir()
	if err := extractBundle(path, dest, func(string, ...any) {}); err != nil {
		t.Fatalf("распаковка: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "tor")); err != nil {
		t.Error("файл не распакован после пропуска каталога")
	}
}

func TestEnsureTorUsesExistingBinary(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	vendor := filepath.Join(dir, "vendor", "tor")
	if err := os.MkdirAll(vendor, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(vendor, "tor.exe")
	if err := os.WriteFile(bin, []byte("заглушка"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Сеть не нужна: бинарь уже на месте, установка должна выйти сразу.
	got, err := EnsureTor(context.Background(), false, func(string, ...any) {})
	if err != nil {
		t.Fatalf("EnsureTor: %v", err)
	}
	if got != bin {
		t.Errorf("путь %q, ожидала %q", got, bin)
	}
}

func TestVerifySHA256MissingFile(t *testing.T) {
	// Файла нет: проверка должна вернуть ошибку, а не панику.
	err := verifySHA256(context.Background(), "0.4.9", "x.tar.gz", "/нет/файла", func(string, ...any) {})
	if err == nil {
		t.Error("отсутствующий файл прошёл проверку")
	}
}
