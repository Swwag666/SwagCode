package setup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"voidsearchswag/internal/netx"
)

// bundleBytes собирает содержимое tar.gz в памяти: тот же формат, что
// makeBundle, но пригодный для отдачи через HTTP.
func bundleBytes(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	path := makeBundle(t, entries)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// distServer поднимает стаб-зеркало каталога тор-дистрибутивов и
// переключает setup на него через VOIDSEARCH_TOR_DIST.
func distServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	// Переменная окружения обязана завершаться слешом: distBase() его
	// добавит сам, но явная проверка поведения важнее.
	t.Setenv(EnvTorDist, srv.URL+"/")
	return srv
}

func TestDistBaseDefault(t *testing.T) {
	t.Setenv(EnvTorDist, "")
	if got := distBase(); got != defaultTorDistBase {
		t.Errorf("distBase=%q", got)
	}
}

func TestDistBaseFromEnv(t *testing.T) {
	t.Setenv(EnvTorDist, "https://mirror.example/torbrowser")
	// Завершающий слеш добавляется сам: без него склейка base+"15.0.23/"
	// даст битый адрес и установка молча уйдёт не туда.
	if got := distBase(); got != "https://mirror.example/torbrowser/" {
		t.Errorf("distBase=%q", got)
	}

	t.Setenv(EnvTorDist, "https://mirror.example/torbrowser/")
	if got := distBase(); got != "https://mirror.example/torbrowser/" {
		t.Errorf("слеш удвоен: %q", got)
	}

	// Пробельное значение не должно подменять официальный каталог.
	t.Setenv(EnvTorDist, "   ")
	if got := distBase(); got != defaultTorDistBase {
		t.Errorf("пробельный env принят: %q", got)
	}
}

func TestLatestVersionPicksHighestFromServer(t *testing.T) {
	distServer(t, func(w http.ResponseWriter, r *http.Request) {
		// Порядок в разметке произвольный, есть дубликаты и альфа-ветка:
		// выбрать нужно старшую стабильную.
		fmt.Fprint(w, `<a href="../">../</a>
<a href="0.4.8.13/">0.4.8.13/</a>
<a href="15.0.23/">15.0.23/</a>
<a href="0.4.10.9/">0.4.10.9/</a>
<a href="15.0.23/">15.0.23/</a>
<a href="16.0a11/">16.0a11/</a>
<a href="15.0.20/">15.0.20/</a>`)
	})

	got, err := latestVersion(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != "15.0.23" {
		t.Errorf("версия %q, ожидала 15.0.23", got)
	}
}

func TestLatestVersionEmptyCatalog(t *testing.T) {
	distServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<html><body>пусто</body></html>")
	})

	_, err := latestVersion(context.Background())
	if err == nil {
		t.Fatal("пустой каталог принят")
	}
	if !strings.Contains(err.Error(), "пуст") {
		t.Errorf("ошибка не объясняет причину: %v", err)
	}
}

func TestLatestVersionHTTPError(t *testing.T) {
	distServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})

	_, err := latestVersion(context.Background())
	if err == nil {
		t.Fatal("503 принят")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("код ответа не назван: %v", err)
	}
}

func TestLatestVersionCancelledContext(t *testing.T) {
	distServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<a href="15.0.23/">15.0.23/</a>`)
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := latestVersion(ctx); err == nil {
		t.Error("отменённый контекст не остановил запрос")
	}
}

func TestPickBundleMatchesPlatform(t *testing.T) {
	bundleName := bundleNameForPlatform()
	distServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<a href="tor-expert-bundle-linux-aarch64-0.4.9.1.tar.gz">linux arm</a>
<a href="%s">нужный</a>
<a href="tor-expert-bundle-macos-universal-0.4.9.1.tar.gz">macos</a>`, bundleName)
	})

	got, err := pickBundle(context.Background(), "0.4.9.1")
	if err != nil {
		t.Fatal(err)
	}
	if got != bundleName {
		t.Errorf("выбран %q, ожидала %q", got, bundleName)
	}
}

func TestPickBundleNoMatchListsAvailable(t *testing.T) {
	distServer(t, func(w http.ResponseWriter, r *http.Request) {
		// Ни один бандл не подходит под текущую платформу: ошибка обязана
		// назвать доступные файлы, иначе пользователь не поймёт, что делать.
		fmt.Fprint(w, `<a href="tor-expert-bundle-plan9-mips-0.4.9.1.tar.gz">чужая платформа</a>
<a href="tor-expert-bundle-haiku-x86-0.4.9.1.tar.gz">ещё чужая</a>`)
	})

	_, err := pickBundle(context.Background(), "0.4.9.1")
	if err == nil {
		t.Fatal("чужая платформа принята")
	}
	if !strings.Contains(err.Error(), "plan9-mips") {
		t.Errorf("в ошибке нет списка доступных: %v", err)
	}
}

func TestPickBundleEmptyCatalog(t *testing.T) {
	distServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<html>ничего нет</html>")
	})
	_, err := pickBundle(context.Background(), "15.0.23")
	if err == nil {
		t.Fatal("пустой каталог принят")
	}
}

func TestPickBundleHTTPError(t *testing.T) {
	distServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	_, err := pickBundle(context.Background(), "0.0.0")
	if err == nil {
		t.Fatal("404 принят")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("код не назван: %v", err)
	}
}

// bundleNameForPlatform собирает имя бандла, которое pickBundle обязан
// выбрать на текущей платформе.
func bundleNameForPlatform() string {
	tokens := platformTokens()
	return "tor-expert-bundle-" + strings.Join(tokens, "-") + "-0.4.9.1.tar.gz"
}

func TestGetLimitsBody(t *testing.T) {
	// Потолок защищает память: каталог версий может быть сколь угодно
	// большим, а целиком он не нужен.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(strings.Repeat("x", 9<<20)))
	}))
	defer srv.Close()

	got, err := get(context.Background(), srv.URL+"/", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 8<<20 {
		t.Errorf("прочитано %d, ожидала потолок %d", len(got), 8<<20)
	}
}

func TestGetHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	_, err := get(context.Background(), srv.URL+"/", 5*time.Second)
	if err == nil {
		t.Fatal("403 принят")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("код не назван: %v", err)
	}
}

func TestGetBadURL(t *testing.T) {
	if _, err := get(context.Background(), "://мусор", time.Second); err == nil {
		t.Error("битый URL принят")
	}
}

func TestGetUnreachable(t *testing.T) {
	if _, err := get(context.Background(), "http://127.0.0.1:1/", 2*time.Second); err == nil {
		t.Error("недоступный адрес принят")
	}
}

func TestDownloadWritesTempFile(t *testing.T) {
	body := bundleBytes(t, map[string]string{"tor/" + torBinary(): "бинарь"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		w.Write(body)
	}))
	defer srv.Close()

	path, err := download(context.Background(), srv.URL+"/bundle.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(body) {
		t.Errorf("скачано %d байт, ожидала %d", len(got), len(body))
	}
	if filepath.Base(path) == "" || !strings.Contains(path, "tor-expert-") {
		t.Errorf("временный файл без ожидаемого префикса: %q", path)
	}
}

func TestDownloadHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	if _, err := download(context.Background(), srv.URL+"/bundle.tar.gz"); err == nil {
		t.Fatal("500 принят")
	}
}

func TestDownloadUnreachable(t *testing.T) {
	if _, err := download(context.Background(), "http://127.0.0.1:1/x.tar.gz"); err == nil {
		t.Error("недоступный адрес принят")
	}
}

func TestDownloadBadURL(t *testing.T) {
	if _, err := download(context.Background(), "://мусор"); err == nil {
		t.Error("битый URL принят")
	}
}

func TestVerifySHA256Success(t *testing.T) {
	body := []byte("содержимое бандла")
	sum := sha256.Sum256(body)
	want := hex.EncodeToString(sum[:])

	arc := filepath.Join(t.TempDir(), "bundle.tar.gz")
	if err := os.WriteFile(arc, body, 0o600); err != nil {
		t.Fatal(err)
	}

	// Формат официального файла: хеш, два пробела, имя файла.
	distServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/sha256sums-unsigned-build.txt") {
			t.Errorf("запрошен не файл сумм: %s", r.URL.Path)
		}
		fmt.Fprintf(w, "%s  tor-expert-bundle-windows-x86_64-15.0.23.tar.gz\n", want)
		fmt.Fprintf(w, "%s  другой-файл.tar.gz\n", strings.Repeat("0", 64))
	})

	var logged []string
	err := verifySHA256(context.Background(), "15.0.23",
		"tor-expert-bundle-windows-x86_64-15.0.23.tar.gz", arc,
		func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) })
	if err != nil {
		t.Fatalf("совпавшая сумма отклонена: %v", err)
	}
}

func TestVerifySHA256Mismatch(t *testing.T) {
	arc := filepath.Join(t.TempDir(), "bundle.tar.gz")
	if err := os.WriteFile(arc, []byte("настоящее содержимое"), 0o600); err != nil {
		t.Fatal(err)
	}

	distServer(t, func(w http.ResponseWriter, r *http.Request) {
		// Подменённая сумма обязана быть отклонена: это единственная защита
		// от подмены бандла на зеркале.
		fmt.Fprintf(w, "%s  нужный.tar.gz\n", strings.Repeat("a", 64))
	})

	err := verifySHA256(context.Background(), "15.0.23", "нужный.tar.gz", arc, func(string, ...any) {})
	if err == nil {
		t.Fatal("несовпавшая сумма принята")
	}
	if !strings.Contains(err.Error(), "не сошёлся") {
		t.Errorf("ошибка не объясняет причину: %v", err)
	}
}

func TestVerifySHA256NoEntryForFile(t *testing.T) {
	arc := filepath.Join(t.TempDir(), "bundle.tar.gz")
	if err := os.WriteFile(arc, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	distServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "abcd  совсем-другой-файл.tar.gz\n")
	})

	err := verifySHA256(context.Background(), "15.0.23", "нужный.tar.gz", arc, func(string, ...any) {})
	if err == nil {
		t.Fatal("отсутствие записи принято")
	}
	// Ошибка обязана подсказать выход: иначе пользователь упрётся в тупик.
	if !strings.Contains(err.Error(), "--no-verify") {
		t.Errorf("в ошибке нет подсказки: %v", err)
	}
}

func TestVerifySHA256MalformedLinesIgnored(t *testing.T) {
	arc := filepath.Join(t.TempDir(), "bundle.tar.gz")
	body := []byte("тело")
	if err := os.WriteFile(arc, body, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	want := hex.EncodeToString(sum[:])

	distServer(t, func(w http.ResponseWriter, r *http.Request) {
		// Строки с лишними или недостающими полями не должны ни ронять
		// проверку, ни выбираться вместо нужной записи.
		fmt.Fprint(w, "\n")
		fmt.Fprint(w, "мусор\n")
		fmt.Fprint(w, "толькооднополе\n")
		fmt.Fprintf(w, "%s  нужный.tar.gz  лишнее-поле\n", want)
		fmt.Fprintf(w, "%s  нужный.tar.gz\n", want)
	})

	if err := verifySHA256(context.Background(), "1", "нужный.tar.gz", arc, func(string, ...any) {}); err != nil {
		t.Fatalf("валидная строка не найдена среди мусора: %v", err)
	}
}

func TestVerifySHA256UnreachableSums(t *testing.T) {
	distServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	err := verifySHA256(context.Background(), "1", "x.tar.gz", "/нет/файла", func(string, ...any) {})
	if err == nil {
		t.Fatal("отсутствие файла сумм принято")
	}
	if !strings.Contains(err.Error(), "--no-verify") {
		t.Errorf("в ошибке нет подсказки: %v", err)
	}
}

func TestEnsureTorDownloadsAndInstalls(t *testing.T) {
	// Сквозной прогон: бинаря нет, зеркало отдаёт каталог, список файлов и
	// сам бандл; результат - распакованный бинарь на диске.
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	binName := torBinary()
	body := bundleBytes(t, map[string]string{
		"tor/" + binName:                     "бинарь",
		"tor/data/geoip":                     "гео",
		"tor/pluggable_transports/lyrebird":  "транспорт",
		"tor/pluggable_transports/snowflake": "второй транспорт",
	})
	sum := sha256.Sum256(body)
	want := hex.EncodeToString(sum[:])

	bundleName := "tor-expert-bundle-" + strings.Join(platformTokens(), "-") + "-15.0.23.tar.gz"

	var paths []string
	distServer(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch {
		case strings.HasSuffix(r.URL.Path, "/sha256sums-unsigned-build.txt"):
			fmt.Fprintf(w, "%s  %s\n", want, bundleName)
		case strings.HasSuffix(r.URL.Path, "/"+bundleName):
			w.Header().Set("Content-Type", "application/gzip")
			w.Write(body)
		case r.URL.Path == "/15.0.23/":
			fmt.Fprintf(w, `<a href="%s">%s</a>`, bundleName, bundleName)
		case r.URL.Path == "/":
			fmt.Fprint(w, `<a href="15.0.23/">15.0.23/</a>`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	var msgs []string
	got, err := EnsureTor(context.Background(), true, func(f string, a ...any) {
		msgs = append(msgs, fmt.Sprintf(f, a...))
	})
	if err != nil {
		t.Fatalf("EnsureTor: %v (запросы: %v)", err, paths)
	}

	wantBin := filepath.Join(dir, "vendor", "tor", binName)
	if got != wantBin {
		t.Errorf("путь %q, ожидала %q", got, wantBin)
	}
	st, err := os.Stat(wantBin)
	if err != nil {
		t.Fatalf("бинарь не создан: %v", err)
	}
	if st.IsDir() {
		t.Error("вместо бинаря создан каталог")
	}
	content, err := os.ReadFile(wantBin)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "бинарь" {
		t.Errorf("содержимое бинаря %q", content)
	}
	// Соседние файлы бандла обязаны распаковаться рядом.
	if _, err := os.Stat(filepath.Join(dir, "vendor", "tor", "data", "geoip")); err != nil {
		t.Errorf("geoip не распакован: %v", err)
	}
	if !strings.Contains(strings.Join(msgs, "\n"), "sha256 совпал") {
		t.Errorf("в логе нет подтверждения проверки: %v", msgs)
	}
}

func TestEnsureTorNoVerifySkipsSums(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	binName := torBinary()
	body := bundleBytes(t, map[string]string{"tor/" + binName: "бинарь"})
	bundleName := "tor-expert-bundle-" + strings.Join(platformTokens(), "-") + "-15.0.23.tar.gz"

	sumsRequested := false
	distServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/sha256sums-unsigned-build.txt"):
			// Без --no-verify зеркало обязано быть запрошено; с ним - нет.
			sumsRequested = true
			w.WriteHeader(http.StatusNotFound)
		case strings.HasSuffix(r.URL.Path, "/"+bundleName):
			w.Write(body)
		case r.URL.Path == "/15.0.23/":
			fmt.Fprintf(w, `<a href="%s">%s</a>`, bundleName, bundleName)
		default:
			fmt.Fprint(w, `<a href="15.0.23/">15.0.23/</a>`)
		}
	})

	got, err := EnsureTor(context.Background(), false, func(string, ...any) {})
	if err != nil {
		t.Fatalf("EnsureTor без проверки: %v", err)
	}
	if filepath.Base(got) != binName {
		t.Errorf("бинарь %q", got)
	}
	if sumsRequested {
		t.Error("файл сумм запрошен при verify=false")
	}
}

func TestEnsureTorVerifyRejectsTamperedBundle(t *testing.T) {
	// Подменённый бандл не должен устанавливаться: проверка суммы есть
	// единственный барьер между зеркалом и выполнением чужого бинаря.
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	binName := torBinary()
	body := bundleBytes(t, map[string]string{"tor/" + binName: "вредоносный бинарь"})
	bundleName := "tor-expert-bundle-" + strings.Join(platformTokens(), "-") + "-15.0.23.tar.gz"

	distServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/sha256sums-unsigned-build.txt"):
			fmt.Fprintf(w, "%s  %s\n", strings.Repeat("f", 64), bundleName)
		case strings.HasSuffix(r.URL.Path, "/"+bundleName):
			w.Write(body)
		case r.URL.Path == "/15.0.23/":
			fmt.Fprintf(w, `<a href="%s">%s</a>`, bundleName, bundleName)
		default:
			fmt.Fprint(w, `<a href="15.0.23/">15.0.23/</a>`)
		}
	})

	if _, err := EnsureTor(context.Background(), true, func(string, ...any) {}); err == nil {
		t.Fatal("подменённый бандл установлен")
	}
	// Бинарь не должен появиться на диске.
	if _, err := os.Stat(filepath.Join(dir, "vendor", "tor", binName)); err == nil {
		t.Error("бинарь создан несмотря на провал проверки")
	}
}

func TestEnsureTorReportsVersionListFailure(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	distServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})

	_, err := EnsureTor(context.Background(), false, func(string, ...any) {})
	if err == nil {
		t.Fatal("недоступный каталог принят")
	}
	// Ошибка обязана объяснять, что именно не получилось: «список версий»,
	// а не голый HTTP-код.
	if !strings.Contains(err.Error(), "список версий tor") {
		t.Errorf("ошибка не объясняет этап: %v", err)
	}
}

func TestEnsureTorReportsMissingBinaryAfterExtract(t *testing.T) {
	// Бандл без бинаря: установка обязана сообщить об этом явно, иначе
	// вызывающий получит путь к несуществующему файлу и упадёт позже.
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	body := bundleBytes(t, map[string]string{"tor/data/geoip": "только данные"})
	bundleName := "tor-expert-bundle-" + strings.Join(platformTokens(), "-") + "-15.0.23.tar.gz"

	distServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/"+bundleName):
			w.Write(body)
		case r.URL.Path == "/15.0.23/":
			fmt.Fprintf(w, `<a href="%s">%s</a>`, bundleName, bundleName)
		default:
			fmt.Fprint(w, `<a href="15.0.23/">15.0.23/</a>`)
		}
	})

	_, err := EnsureTor(context.Background(), false, func(string, ...any) {})
	if err == nil {
		t.Fatal("бандл без бинаря принят")
	}
	if !strings.Contains(err.Error(), "не найден") {
		t.Errorf("ошибка не объясняет причину: %v", err)
	}
}

func TestEnsureTorRemovesTempArchive(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	binName := torBinary()
	body := bundleBytes(t, map[string]string{"tor/" + binName: "бинарь"})
	bundleName := "tor-expert-bundle-" + strings.Join(platformTokens(), "-") + "-15.0.23.tar.gz"

	distServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/"+bundleName):
			w.Write(body)
		case r.URL.Path == "/15.0.23/":
			fmt.Fprintf(w, `<a href="%s">%s</a>`, bundleName, bundleName)
		default:
			fmt.Fprint(w, `<a href="15.0.23/">15.0.23/</a>`)
		}
	})

	before := countTempBundles(t)
	if _, err := EnsureTor(context.Background(), false, func(string, ...any) {}); err != nil {
		t.Fatal(err)
	}
	// Скачанный архив весит десятки мегабайт: оставлять его в temp после
	// каждой установки нельзя.
	after := countTempBundles(t)
	if after > before {
		t.Errorf("временный архив не удалён: было %d, стало %d", before, after)
	}
}

func countTempBundles(t *testing.T) int {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(os.TempDir(), "tor-expert-*.tar.gz"))
	if err != nil {
		t.Fatal(err)
	}
	return len(matches)
}

// torBinary берёт имя из netx: дублировать логику платформы в тесте
// бессмысленно, иначе тест продолжит проходить после изменения netx и
// перестанет проверять реальное поведение.
func torBinary() string { return netx.TorBinaryName() }

func TestPlatformTokensMatchRuntime(t *testing.T) {
	got := platformTokens()
	if len(got) == 0 {
		t.Fatal("токенов нет")
	}
	for _, tok := range got {
		if strings.TrimSpace(tok) == "" {
			t.Errorf("пустой токен в %v", got)
		}
	}
}
