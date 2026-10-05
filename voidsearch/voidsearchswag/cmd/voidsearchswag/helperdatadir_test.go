package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"voidsearchswag/internal/config"
)

// Правило появилось после инцидента: мутационный прогон этапа 133 сломал
// проверку глубины, discover --crawl дошёл до базы из конфига и записал в боевой
// файл. Тесты держат защиту, чтобы любой прогон в подпроцессе был безопасен по
// построению, а не по везению.
func TestHelperDataDirPrefersValueFromTest(t *testing.T) {
	want := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", want)
	if got := helperDataDir(t); got != want {
		t.Errorf("helperDataDir = %q, хочу каталог теста %q", got, want)
	}
}

func TestHelperDataDirNeverFallsBackToBattle(t *testing.T) {
	t.Setenv("VOIDSEARCH_DATA_DIR", "")
	dir := helperDataDir(t)
	if dir == "" {
		t.Fatal("helperDataDir вернул пустую строку: прогон открыл бы базу из конфига")
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("helperDataDir вернул несуществующий каталог %q: %v", dir, err)
	}
	if !info.IsDir() {
		t.Fatalf("helperDataDir вернул не каталог, а %q", dir)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("конфиг не собрался: %v", err)
	}
	if battle := filepath.Dir(cfg.DBPath()); filepath.Clean(dir) == filepath.Clean(battle) {
		t.Errorf("helperDataDir вернул боевой каталог %q", dir)
	}
}

// Запись обязана быть одна: дубль переменной оставил бы выбор каталога
// зависимым от порядка записей в окружении подпроцесса.
func TestSubprocEnvCarriesSingleDataDir(t *testing.T) {
	t.Setenv("VOIDSEARCH_DATA_DIR", "")
	env := subprocEnv(t, []string{"stats"})
	count := 0
	value := ""
	for _, kv := range env {
		if strings.HasPrefix(kv, "VOIDSEARCH_DATA_DIR=") {
			count++
			value = kv
		}
	}
	if count != 1 {
		t.Errorf("записей VOIDSEARCH_DATA_DIR в окружении подпроцесса %d, хочу ровно одну", count)
	}
	if strings.HasSuffix(value, "=") {
		t.Errorf("окружение подпроцесса несёт пустой каталог данных %q", value)
	}
	if strings.Contains(value, ".voidsearchswag") {
		t.Errorf("окружение подпроцесса несёт боевой каталог %q", value)
	}
	if !strings.Contains(strings.Join(env, "\n"), "VSS_MAIN_ARGS=stats") {
		t.Error("аргументы команды не дошли до окружения подпроцесса")
	}
}

// Заданный тестом каталог обязан дойти до подпроцесса без дубля: почти все
// тесты пакета сеют базу именно так.
func TestSubprocEnvKeepsDataDirFromTest(t *testing.T) {
	want := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", want)
	env := subprocEnv(t, []string{"stats"})
	count := 0
	for _, kv := range env {
		if kv == "VOIDSEARCH_DATA_DIR="+want {
			count++
		}
	}
	if count != 1 {
		t.Errorf("каталог теста дошёл до окружения %d раз, хочу ровно один", count)
	}
}
