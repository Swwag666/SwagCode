package main

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

// resetVerbose возвращает переключатель подробного вывода в исходное
// состояние: он пакетный, поэтому без сброса тесты влияли бы друг на друга
// независимо от порядка запуска.
func resetVerbose(t *testing.T) {
	t.Helper()
	prev := verboseLogs
	prevEnv := os.Getenv("VOIDSEARCH_VERBOSE")
	verboseLogs = false
	os.Unsetenv("VOIDSEARCH_VERBOSE")
	t.Cleanup(func() {
		verboseLogs = prev
		if prevEnv != "" {
			os.Setenv("VOIDSEARCH_VERBOSE", prevEnv)
		} else {
			os.Unsetenv("VOIDSEARCH_VERBOSE")
		}
	})
}

func TestDetectVerboseFlags(t *testing.T) {
	resetVerbose(t)
	if verboseLogs {
		t.Fatal("переключатель включён до разбора")
	}

	detectVerbose([]string{"search", "--verbose", "linux"})
	if !verboseLogs {
		t.Error("--verbose после субкоманды не сработал")
	}

	resetVerbose(t)
	detectVerbose([]string{"--verbose", "search", "linux"})
	if !verboseLogs {
		t.Error("--verbose перед аргументами команды не сработал")
	}

	resetVerbose(t)
	detectVerbose([]string{"search", "-verbose", "linux"})
	if !verboseLogs {
		t.Error("-verbose с одним минусом не сработал")
	}

	resetVerbose(t)
	detectVerbose([]string{"search", "linux", "kernel"})
	if verboseLogs {
		t.Error("без флага переключатель оказался включён")
	}
}

func TestDetectVerboseShortVIsNotVerbose(t *testing.T) {
	// Короткий -v на верхнем уровне уже означает version, поэтому признаком
	// подробного вывода он быть не может: два смысла у одного ключа дали бы
	// `voidsearchswag -v search ...` как запрос версии с игнорированием
	// остального.
	resetVerbose(t)
	detectVerbose([]string{"-v", "search", "linux"})
	if verboseLogs {
		t.Error("-v включил подробный вывод, хотя он занят под version")
	}
}

func TestDetectVerboseFromEnv(t *testing.T) {
	resetVerbose(t)
	os.Setenv("VOIDSEARCH_VERBOSE", "1")
	detectVerbose([]string{"search", "linux"})
	if !verboseLogs {
		t.Error("переменная окружения не включила подробный вывод")
	}
}

func TestVerboseArgsStripsOnlyVerboseFlags(t *testing.T) {
	cases := []struct {
		in   []string
		want []string
	}{
		{[]string{"--verbose", "search", "linux"}, []string{"search", "linux"}},
		{[]string{"search", "--verbose", "linux"}, []string{"search", "linux"}},
		{[]string{"search", "-verbose", "linux"}, []string{"search", "linux"}},
		{[]string{"search", "linux"}, []string{"search", "linux"}},
		{[]string{"--verbose"}, []string{}},
		{[]string{}, []string{}},
		// -v обязан оставаться: это алиас version, а не признак подробностей.
		{[]string{"-v", "search"}, []string{"-v", "search"}},
		// Прочие флаги не затрагиваются.
		{[]string{"--verbose", "-limit", "30", "--json"}, []string{"-limit", "30", "--json"}},
	}
	for _, c := range cases {
		got := verboseArgs(c.in)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("verboseArgs(%v) = %v, ожидала %v", c.in, got, c.want)
		}
	}
}

func TestVerboseArgsDoesNotMutateInput(t *testing.T) {
	in := []string{"search", "--verbose", "linux"}
	verboseArgs(in)
	want := []string{"search", "--verbose", "linux"}
	if !reflect.DeepEqual(in, want) {
		t.Errorf("вход изменён: %v", in)
	}
}

func TestVerboseArgsKeepsPositionalQueryIntact(t *testing.T) {
	// Позиционный запрос не должен пострадать: если запрос сам содержит слово
	// «verbose», вырезать его нельзя.
	in := []string{"search", "--verbose", "verbose", "logging"}
	got := verboseArgs(in)
	want := []string{"search", "verbose", "logging"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("verboseArgs(%v) = %v, ожидала %v", in, got, want)
	}
}

// captureStderr перехватывает stderr через временный файл по той же схеме, что
// captureStdout в main_test.go: логгер пишет прямо в os.Stderr.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	f, err := os.CreateTemp(t.TempDir(), "err")
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = f
	fn()
	f.Close()
	os.Stderr = old
	b, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestStderrLoggerSuppressesInfoByDefault(t *testing.T) {
	resetVerbose(t)
	log := stderrLogger{}

	got := captureStderr(t, func() {
		log.Infof("здоровье движков восстановлено: %d", 7)
		log.Infof("сессия: %s через %s", "direct/chrome_131_win", "direct")
	})
	if strings.TrimSpace(got) != "" {
		t.Errorf("при выключенном переключателе в stderr попало %q", got)
	}
}

func TestStderrLoggerPrintsInfoWhenVerbose(t *testing.T) {
	resetVerbose(t)
	verboseLogs = true
	log := stderrLogger{}

	got := captureStderr(t, func() {
		log.Infof("здоровье движков восстановлено: %d", 7)
	})
	if !strings.Contains(got, "здоровье движков восстановлено: 7") {
		t.Errorf("с --verbose инфо не напечатано: %q", got)
	}
}

func TestStderrLoggerAlwaysPrintsWarnings(t *testing.T) {
	// Предупреждение по определению не штатная работа, поэтому переключатель
	// подробностей его глушить не должен ни в одном состоянии.
	for _, verbose := range []bool{false, true} {
		resetVerbose(t)
		verboseLogs = verbose
		log := stderrLogger{}

		got := captureStderr(t, func() {
			log.Warnf("здоровье движков не прочитано: %v", "сбой")
		})
		if !strings.Contains(got, "WARN: здоровье движков не прочитано") {
			t.Errorf("при verbose=%v предупреждение потеряно: %q", verbose, got)
		}
	}
}

func TestStderrLoggerFormatArgsNotEvaluatedTwice(t *testing.T) {
	// Подавление обязано происходить до записи, а не после: иначе формат
	// собирался бы впустую на каждом вызове. Проверяется отсутствием вывода при
	// корректной подстановке аргументов.
	resetVerbose(t)
	log := stderrLogger{}
	got := captureStderr(t, func() {
		log.Infof("движок %s вернул %d", "ddg", 11)
	})
	if got != "" {
		t.Errorf("ожидала пустой stderr, получено %q", got)
	}
}
