package main

import (
	"strings"
	"testing"
)

// Замер до правки на HEAD 97194a2. Обход пускает сид в очередь только если
// discover.HostOf вернул хост, а HostOf принимает base32 длиной 16 или 56
// символов. Всё остальное отбрасывалось молча: живой прогон на копии боевой базы
// с выключенным тором дал
//
//	--seeds http://abcdefghijklmnop.onion/list --seeds http://qrstuvwxyz012345.onion/list
//	  rc=0, pages=1, no_transport=1, stderr 0 байт
//	--seeds http://qrstuvwxyz012345.onion/list
//	  rc=0, pages=0, no_transport=0, stderr 0 байт
//	--seeds https://example.com/list
//	  rc=0, pages=0, no_transport=0, stderr 0 байт
//
// Второй адрес отличается от рабочего только цифрами, которых нет в base32.
// Оператор получал rc=0 и отчёт, где обход просто обошёл меньше хостов, чем
// задано, и не мог отличить опечатку от недоступной сети.
func TestInvalidSeedsWarningNamesBadSeed(t *testing.T) {
	const valid = "http://abcdefghijklmnop.onion/list"
	const bad = "http://qrstuvwxyz012345.onion/list"
	got := invalidSeedsWarning([]string{valid, bad})
	if got == "" {
		t.Fatal("нераспознанный сид не вызвал предупреждения")
	}
	if !strings.Contains(got, bad) {
		t.Errorf("предупреждение не называет плохой сид %q: %q", bad, got)
	}
	if strings.Contains(got, valid) {
		t.Errorf("предупреждение назвало рабочий сид %q: %q", valid, got)
	}
}

func TestInvalidSeedsWarningListsEveryBadSeed(t *testing.T) {
	bad := []string{
		"http://qrstuvwxyz012345.onion/list",
		"https://example.com/list",
		"tooshort.onion",
	}
	got := invalidSeedsWarning(bad)
	for _, seed := range bad {
		if !strings.Contains(got, seed) {
			t.Errorf("предупреждение не называет %q: %q", seed, got)
		}
	}
}

func TestInvalidSeedsWarningExplainsRule(t *testing.T) {
	got := invalidSeedsWarning([]string{"http://qrstuvwxyz012345.onion/list"})
	for _, want := range []string{"16", "56", "base32", "обход их пропустит"} {
		if !strings.Contains(got, want) {
			t.Errorf("в предупреждении нет %q: %q", want, got)
		}
	}
}

func TestInvalidSeedsWarningSilentWhenAllValid(t *testing.T) {
	// base32-хост v2 - 16 символов, v3 - 56 символов. Строка ниже составлена из
	// 32 + 24 символа, оба куска внутри алфавита a-z2-7.
	const v3 = "abcdefghijklmnopqrstuvwxyz234567abcdefghijklmnopqrstuvwx.onion"
	if len(strings.TrimSuffix(v3, ".onion")) != 56 {
		t.Fatalf("в тесте опечатка: длина метки %d, хочу 56", len(strings.TrimSuffix(v3, ".onion")))
	}
	seeds := []string{
		"http://abcdefghijklmnop.onion/list",
		"qrstuvwxyzabcdef.onion",
		"https://" + v3 + "/x",
	}
	if got := invalidSeedsWarning(seeds); got != "" {
		t.Errorf("все сиды распознаны, а предупреждение напечатано: %q", got)
	}
}

func TestInvalidSeedsWarningSilentWithoutSeeds(t *testing.T) {
	for _, seeds := range [][]string{nil, {}, {"", " ", "  "}} {
		if got := invalidSeedsWarning(seeds); got != "" {
			t.Errorf("сиды %+v не несут ни одного адреса, а предупреждение напечатано: %q", seeds, got)
		}
	}
}

// Голый хост без схемы обход принимает, поэтому и предупреждение о нём молчит:
// правило одно и там, и там.
func TestInvalidSeedsWarningAcceptsBareHost(t *testing.T) {
	if got := invalidSeedsWarning([]string{"abcdefghijklmnop.onion"}); got != "" {
		t.Errorf("голый onion-хост не распознан: %q", got)
	}
	if got := invalidSeedsWarning([]string{"abc111.onion"}); got == "" {
		t.Error("мусорный адрес длиной 6 символов распознан как сид")
	}
}

func TestWarnInvalidSeedsPrintsToStderr(t *testing.T) {
	got := captureStderr(t, func() {
		warnInvalidSeeds(stderrLogger{}, []string{"http://qrstuvwxyz012345.onion/list"})
	})
	if !strings.Contains(got, "не распознаны как onion-адреса") {
		t.Errorf("предупреждение не дошло до stderr, фактически %q", got)
	}
	if !strings.Contains(got, "discover:") {
		t.Errorf("предупреждение не названо командой, фактически %q", got)
	}
}

func TestWarnInvalidSeedsSilentForValidSeeds(t *testing.T) {
	got := captureStderr(t, func() {
		warnInvalidSeeds(stderrLogger{}, []string{"http://abcdefghijklmnop.onion/list"})
	})
	if got != "" {
		t.Errorf("рабочий сид вызвал предупреждение %q, ожидала тишину", got)
	}
}

// Предупреждение о значении флага и предупреждение о режиме независимы: первое
// говорит «этот адрес не onion», второе - «сиды применяются только с --crawl».
// Мусорный сид без --crawl заслуживает обоих.
func TestInvalidSeedsWarningIndependentFromCrawl(t *testing.T) {
	const bad = "http://qrstuvwxyz012345.onion/list"
	if got := invalidSeedsWarning([]string{bad}); got == "" {
		t.Fatal("нет предупреждения о нераспознанном сиде")
	}
	if got := discoverSeedsWarning(bad, false); got == "" {
		t.Fatal("нет предупреждения о сидах без --crawl")
	}
	if got := discoverSeedsWarning(bad, true); got != "" {
		t.Errorf("при --crawl напечатано предупреждение о режиме: %q", got)
	}
}

// Предупреждение обязано доходить до человека в реальном прогоне команды.
// Мусорный таймаут роняет загрузку конфига сразу после разбора флагов: прогон не
// доходит до базы, транспорта и сети, а предупреждение печатается раньше конфига.
func TestDiscoverWarnsAboutInvalidSeedsInRealRun(t *testing.T) {
	t.Setenv("VOIDSEARCH_DATA_DIR", t.TempDir())
	t.Setenv("VOIDSEARCH_BACKUP_DIR", "")
	t.Setenv("VOIDSEARCH_TOR", "off")
	t.Setenv("VOIDSEARCH_TRANSPORT", "direct")
	t.Setenv("VOIDSEARCH_REQUEST_TIMEOUT", "не-длительность")

	const bad = "http://qrstuvwxyz012345.onion/list"
	_, stderr, code := runMainSplit(t, "discover", "--crawl", "--seeds", bad)
	if code == 0 {
		t.Fatal("прогон с мусорным таймаутом вернул ноль: ранний отказ не сработал и проверка ушла бы в сеть")
	}
	if !strings.Contains(stderr, bad) {
		t.Errorf("предупреждение не назвало плохой сид, фактически %q", stderr)
	}
	if !strings.Contains(stderr, "не распознаны как onion-адреса") {
		t.Errorf("предупреждение не дошло до stderr реального прогона, фактически %q", stderr)
	}

	_, stderrValid, codeValid := runMainSplit(t, "discover", "--crawl", "--seeds", "http://abcdefghijklmnop.onion/list")
	if codeValid == 0 {
		t.Fatal("прогон с рабочим сидом и мусорным таймаутом вернул ноль: ранний отказ не сработал")
	}
	if strings.Contains(stderrValid, "не распознаны как onion-адреса") {
		t.Errorf("рабочий сид вызвал предупреждение в реальном прогоне: %q", stderrValid)
	}
}
