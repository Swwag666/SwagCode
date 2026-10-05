package main

import (
	"strings"
	"testing"
)

// Замер до правки на HEAD 0194448. Живой прогон пересобранным бинарником на копии
// боевой базы, тор выключен, транспорт direct, таймаут запроса 3s:
//
//	discover --seeds http://abcdefghijklmnop.onion/list --json
//	  rc=0, источников 8, найдено 11123, раздел crawl отсутствует,
//	  stderr 0 байт, сид не упомянут в выводе ни разу
//
//	discover --crawl --seeds http://abcdefghijklmnop.onion/list --json
//	  pages 1, в pages_detail host abcdefghijklmnop.onion с причиной
//	  «onion-адрес недостижим напрямую: нужен tor или прокси»
//
// То есть без обхода флаг принимался и отбрасывался молча: отчёт выглядел как
// штатная разведка, и понять, что заданный источник не участвовал, было негде.
func TestDiscoverSeedsWithoutCrawlWarns(t *testing.T) {
	got := discoverSeedsWarning("http://abcdefghijklmnop.onion/list", false)
	if got == "" {
		t.Fatal("предупреждение о неприменённых сидах пустое")
	}
	for _, want := range []string{"--seeds", "--crawl"} {
		if !strings.Contains(got, want) {
			t.Errorf("в предупреждении %q нет %q: человек не поймёт, каким флагом это лечится", got, want)
		}
	}
}

func TestDiscoverSeedsWithCrawlIsSilent(t *testing.T) {
	if got := discoverSeedsWarning("http://abcdefghijklmnop.onion/list", true); got != "" {
		t.Errorf("при --crawl сиды применяются, а предупреждение напечатано: %q", got)
	}
}

func TestDiscoverSeedsEmptyIsSilent(t *testing.T) {
	if got := discoverSeedsWarning("", false); got != "" {
		t.Errorf("флаг не задан, а предупреждение напечатано: %q", got)
	}
}

// Пустые части значения сидом не считаются: «--seeds ,» задаёт пустой список, и
// ругаться на нечего.
func TestDiscoverSeedsBlankIsSilent(t *testing.T) {
	for _, seeds := range []string{",", " , , ", ",,", "   "} {
		if got := discoverSeedsWarning(seeds, false); got != "" {
			t.Errorf("seeds=%q не несёт ни одного сида, а предупреждение напечатано: %q", seeds, got)
		}
	}
}

// Сидов может быть несколько, и предупреждение обязано называть оба флага один
// раз: повтор имени флага в одной строке - тот же дефект, что этап 115 снимал с
// отчёта движка.
func TestDiscoverSeedsWarningNamesFlagsOnce(t *testing.T) {
	got := discoverSeedsWarning("a.onion,b.onion", false)
	if n := strings.Count(got, "--seeds"); n != 1 {
		t.Errorf("--seeds встречается в предупреждении %d раз, ожидала 1: %q", n, got)
	}
	if n := strings.Count(got, "--crawl"); n != 1 {
		t.Errorf("--crawl встречается в предупреждении %d раз, ожидала 1: %q", n, got)
	}
}

func TestWarnUnusedSeedsPrintsToStderr(t *testing.T) {
	got := captureStderr(t, func() {
		warnUnusedSeeds(stderrLogger{}, "http://abcdefghijklmnop.onion/list", false)
	})
	if !strings.Contains(got, "--seeds") {
		t.Fatalf("предупреждение не дошло до stderr, фактически %q", got)
	}
	if !strings.HasPrefix(strings.TrimSpace(got), "WARN: discover:") {
		t.Errorf("предупреждение не помечено как WARN от discover, фактически %q", strings.TrimSpace(got))
	}
	if n := strings.Count(got, "--seeds"); n != 1 {
		t.Errorf("предупреждение напечатано %d раз, ожидала один: %q", n, got)
	}
}

func TestWarnUnusedSeedsSilentWithCrawl(t *testing.T) {
	got := captureStderr(t, func() {
		warnUnusedSeeds(stderrLogger{}, "http://abcdefghijklmnop.onion/list", true)
	})
	if got != "" {
		t.Errorf("при --crawl в stderr уехало %q, ожидала тишину", got)
	}
}

func TestWarnUnusedSeedsSilentWithoutSeeds(t *testing.T) {
	got := captureStderr(t, func() {
		warnUnusedSeeds(stderrLogger{}, "", false)
	})
	if got != "" {
		t.Errorf("без --seeds в stderr уехало %q, ожидала тишину", got)
	}
}

// Предупреждение обязано доходить до человека в реальном прогоне команды, а не
// только в прямом вызове функции. Мусорный таймаут роняет загрузку конфига сразу
// после разбора флагов: прогон не доходит до базы, транспорта и сети, а
// предупреждение печатается раньше конфига и поэтому доживает до отказа.
func TestDiscoverWarnsAboutUnusedSeedsInRealRun(t *testing.T) {
	t.Setenv("VOIDSEARCH_DATA_DIR", t.TempDir())
	t.Setenv("VOIDSEARCH_BACKUP_DIR", "")
	t.Setenv("VOIDSEARCH_TOR", "off")
	t.Setenv("VOIDSEARCH_TRANSPORT", "direct")
	t.Setenv("VOIDSEARCH_REQUEST_TIMEOUT", "не-длительность")

	_, stderr, code := runMainSplit(t, "discover", "--seeds", "http://abcdefghijklmnop.onion/list")
	if code == 0 {
		t.Fatal("прогон с мусорным таймаутом вернул ноль: ранний отказ не сработал и проверка ушла бы в сеть")
	}
	if !strings.Contains(stderr, "--seeds") {
		t.Errorf("предупреждение не дошло до stderr реального прогона, фактически %q", stderr)
	}
	if !strings.Contains(stderr, "--crawl") {
		t.Errorf("предупреждение не называет --crawl, фактически %q", stderr)
	}

	_, stderrCrawl, codeCrawl := runMainSplit(t, "discover", "--crawl", "--seeds", "http://abcdefghijklmnop.onion/list")
	if codeCrawl == 0 {
		t.Fatal("прогон с --crawl и мусорным таймаутом вернул ноль: ранний отказ не сработал")
	}
	if strings.Contains(stderrCrawl, "--seeds применяется") {
		t.Errorf("при --crawl сиды применяются, а реальный прогон всё равно напечатал предупреждение: %q", stderrCrawl)
	}
}
