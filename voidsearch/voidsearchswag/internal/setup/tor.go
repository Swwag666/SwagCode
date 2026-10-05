package setup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"voidsearchswag/internal/netx"
)

const defaultTorDistBase = "https://dist.torproject.org/torbrowser/"

// EnvTorDist позволяет взять бандл с зеркала: официальный dist.torproject.org
// регулярно отдаёт 503 и не всегда доступен из-за гео-ограничений, поэтому
// без зеркала установка tor превращается в лотерею.
const EnvTorDist = "VOIDSEARCH_TOR_DIST"

var (
	reVersionDir = regexp.MustCompile(`href="([0-9]+\.[0-9]+(?:\.[0-9]+)?)/"`)
	reBundleFile = regexp.MustCompile(`href="(tor-expert-bundle-[^"]+\.tar\.gz)"`)
)

type LogFn func(format string, args ...any)

// distBase отдаёт корень каталога дистрибутивов. Значение из окружения
// важнее дефолта и всегда нормализуется к завершающему слешу: без него
// склейка вида base+"15.0.23/" даёт битый адрес.
func distBase() string {
	if v := strings.TrimSpace(os.Getenv(EnvTorDist)); v != "" {
		if !strings.HasSuffix(v, "/") {
			v += "/"
		}
		return v
	}
	return defaultTorDistBase
}

func EnsureTor(ctx context.Context, verify bool, log LogFn) (string, error) {
	bin := filepath.Join(netx.VendorDir(), netx.TorBinaryName())
	if st, err := os.Stat(bin); err == nil && !st.IsDir() {
		log("tor уже на месте: %s", bin)
		return bin, nil
	}
	ver, err := latestVersion(ctx)
	if err != nil {
		return "", fmt.Errorf("список версий tor: %w", err)
	}
	log("последняя версия tor: %s", ver)
	file, err := pickBundle(ctx, ver)
	if err != nil {
		return "", err
	}
	log("скачиваю %s", file)
	arc, err := download(ctx, distBase()+ver+"/"+file)
	if err != nil {
		return "", err
	}
	defer os.Remove(arc)
	if verify {
		if err := verifySHA256(ctx, ver, file, arc, log); err != nil {
			return "", err
		}
		log("sha256 совпал")
	}
	if err := extractBundle(arc, netx.VendorDir(), log); err != nil {
		return "", err
	}
	if _, err := os.Stat(bin); err != nil {
		return "", fmt.Errorf("после распаковки бинарь не найден: %s", bin)
	}
	log("tor установлен: %s", bin)
	return bin, nil
}

func latestVersion(ctx context.Context) (string, error) {
	body, err := get(ctx, distBase(), 30*time.Second)
	if err != nil {
		return "", err
	}
	matches := reVersionDir.FindAllStringSubmatch(string(body), -1)
	if len(matches) == 0 {
		return "", fmt.Errorf("каталог версий пуст: %s", distBase())
	}
	versions := make([]string, 0, len(matches))
	seen := map[string]bool{}
	for _, m := range matches {
		if !seen[m[1]] {
			seen[m[1]] = true
			versions = append(versions, m[1])
		}
	}
	sort.Slice(versions, func(i, j int) bool { return semverLess(versions[i], versions[j]) })
	return versions[len(versions)-1], nil
}

func semverLess(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		ai, _ := strconv.Atoi(pa[i])
		bi, _ := strconv.Atoi(pb[i])
		if ai != bi {
			return ai < bi
		}
	}
	return len(pa) < len(pb)
}

func pickBundle(ctx context.Context, ver string) (string, error) {
	body, err := get(ctx, distBase()+ver+"/", 30*time.Second)
	if err != nil {
		return "", err
	}
	files := reBundleFile.FindAllStringSubmatch(string(body), -1)
	tokens := platformTokens()
	for _, m := range files {
		name := m[1]
		ok := true
		for _, t := range tokens {
			if !strings.Contains(name, t) {
				ok = false
				break
			}
		}
		if ok {
			return name, nil
		}
	}
	avail := make([]string, 0, len(files))
	for _, m := range files {
		avail = append(avail, m[1])
	}
	return "", fmt.Errorf("нет expert bundle под %s/%s (доступны: %s)", runtime.GOOS, runtime.GOARCH, strings.Join(avail, ", "))
}

func platformTokens() []string {
	switch runtime.GOOS {
	case "windows":
		return []string{"windows", "x86_64"}
	case "linux":
		if runtime.GOARCH == "arm64" {
			return []string{"linux", "aarch64"}
		}
		return []string{"linux", "x86_64"}
	case "darwin":
		return []string{"macos", "universal"}
	}
	return []string{runtime.GOOS}
}

func get(ctx context.Context, url string, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 8<<20))
}

func download(ctx context.Context, url string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	tmp, err := os.CreateTemp("", "tor-expert-*.tar.gz")
	if err != nil {
		return "", err
	}
	defer tmp.Close()
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return tmp.Name(), nil
}

func verifySHA256(ctx context.Context, ver, file, arc string, log LogFn) error {
	body, err := get(ctx, distBase()+ver+"/sha256sums-unsigned-build.txt", 30*time.Second)
	if err != nil {
		return fmt.Errorf("sha256sums не получен: %w (перезапусти setup с --no-verify, если доверяешь источнику)", err)
	}
	want := ""
	for _, line := range strings.Split(string(body), "\n") {
		fs := strings.Fields(line)
		if len(fs) == 2 && fs[1] == file {
			want = strings.ToLower(fs[0])
			break
		}
	}
	if want == "" {
		return fmt.Errorf("в sha256sums нет записи для %s (перезапусти с --no-verify, если доверяешь)", file)
	}
	f, err := os.Open(arc)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != want {
		return fmt.Errorf("sha256 не сошёлся: файл %s, got %s, want %s", file, got, want)
	}
	return nil
}

// withinDir проверяет, что путь лежит внутри каталога. Сравнение идёт по
// чистым путям, потому что filepath.Join уже схлопнул "..", но не проверил
// границу: он охотно вернёт каталог выше destDir.
func withinDir(dir, path string) bool {
	cleanDir, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	cleanPath, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(cleanDir, cleanPath)
	if err != nil {
		return false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return rel != "."
}

func extractBundle(arc, destDir string, log LogFn) error {
	f, err := os.Open(arc)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	count := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar: %w", err)
		}
		name := strings.TrimPrefix(filepath.ToSlash(hdr.Name), "./")
		var rel string
		switch {
		case strings.HasPrefix(name, "tor/"):
			rel = strings.TrimPrefix(name, "tor/")
		case strings.HasPrefix(name, "pluggable_transports/"):
			rel = name
		default:
			continue
		}
		if rel == "" || hdr.FileInfo().IsDir() {
			continue
		}
		// Имя файла берётся из архива, то есть из недоверенных данных:
		// путь вида tor/../../evil после среза префикса уводит запись за
		// пределы каталога установки. Проверка обязана отсекать такой
		// побег, иначе любой подменённый архив пишет файлы куда угодно.
		dest := filepath.Join(destDir, filepath.FromSlash(rel))
		if !withinDir(destDir, dest) {
			log("пропущен опасный путь в архиве: %s", hdr.Name)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		out, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, tr); err != nil {
			out.Close()
			return err
		}
		out.Close()
		count++
	}
	if count == 0 {
		return fmt.Errorf("в архиве не нашлось файлов tor/")
	}
	log("распаковано файлов: %d → %s", count, destDir)
	return nil
}
