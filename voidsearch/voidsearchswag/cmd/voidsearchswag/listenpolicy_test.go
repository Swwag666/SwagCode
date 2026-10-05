package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"voidsearchswag/internal/config"
	"voidsearchswag/internal/mcpserver"
)

// Команда run обязана отказать до тяжёлой инициализации, если адрес слушателя
// не петлевой, а токен не задан.
//
// Живой замер ДО на бинаре e5eb39b: точно такой же запуск поднимал сервер на
// 0.0.0.0:18080 и [::]:18080, и с адреса 192.168.0.18 без заголовка
// Authorization отдавались initialize (200 и сессия), tools/list (200, 27
// инструментов, среди них backup_create, judge_submit, fetch, hunt_create) и
// tools/call stats (200, содержимое базы). Единственной защитой была строка WRN
// в лог.
func TestRunRefusesOpenAddressWithoutToken(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	t.Setenv("VOIDSEARCH_TOR", "off")
	t.Setenv("VOIDSEARCH_TRANSPORT", "direct")
	t.Setenv("VOIDSEARCH_BACKUP_DIR", "")
	t.Setenv("VOIDSEARCH_HTTP_TOKEN", "")
	t.Setenv("VOIDSEARCH_HTTP_ALLOW_OPEN", "")

	stdout, stderr, code := runMainSplit(t, "run", "--http", "0.0.0.0:18091", "--no-tor")
	if code == 0 {
		t.Fatalf("код возврата 0: сервер на 0.0.0.0 без токена поднялся (stderr %s)",
			firstN(stderr, 300))
	}
	combined := stdout + stderr
	if !strings.Contains(combined, "открытый адрес без токена") {
		t.Errorf("причина не названа: stdout=%s stderr=%s",
			firstN(stdout, 200), firstN(stderr, 300))
	}
	if !strings.Contains(combined, "0.0.0.0:18091") {
		t.Errorf("в отказе нет адреса слушателя: %s", firstN(combined, 400))
	}
	if !strings.Contains(combined, "VOIDSEARCH_HTTP_ALLOW_OPEN") {
		t.Errorf("отказ не подсказывает переключатель осознанности: %s", firstN(combined, 400))
	}

	// Отказ обязан случиться до открытия базы: иначе после отказа на диске
	// останутся файл базы и применённые миграции.
	matches, err := filepath.Glob(filepath.Join(dir, "*.db"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) > 0 {
		t.Errorf("база создана до отказа политики: %v", matches)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) > 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("каталог данных не пуст после отказа: %v", names)
	}
}

// Адрес без хоста (:порт) слушает все интерфейсы точно так же, как явный
// 0.0.0.0, и это самый частый вариант опечатки: оператор пишет --http :8800 по
// документации и открывает сервер всей сети.
func TestRunRefusesAddressWithoutHost(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	t.Setenv("VOIDSEARCH_TOR", "off")
	t.Setenv("VOIDSEARCH_TRANSPORT", "direct")
	t.Setenv("VOIDSEARCH_BACKUP_DIR", "")
	t.Setenv("VOIDSEARCH_HTTP_TOKEN", "")
	t.Setenv("VOIDSEARCH_HTTP_ALLOW_OPEN", "")

	stdout, stderr, code := runMainSplit(t, "run", "--http", ":18092", "--no-tor")
	if code == 0 {
		t.Fatalf("код возврата 0: адрес без хоста поднял сервер (stderr %s)", firstN(stderr, 300))
	}
	if !strings.Contains(stdout+stderr, "открытый адрес без токена") {
		t.Errorf("причина не названа: stdout=%s stderr=%s",
			firstN(stdout, 200), firstN(stderr, 300))
	}
}

// Переменная окружения VOIDSEARCH_HTTP_ALLOW_OPEN снимает запрет: этим прогоном
// проверяется связь config -> политика внутри команды run, которую юнит-тесты
// самой политики не видят.
//
// Адрес взят из документационного диапазона 192.0.2.0/24: он не принадлежит
// машине, поэтому bind завершается ошибкой сразу и без резолвинга имени, а
// подпроцесс не остаётся висеть в ожидании сигнала. Политика к этому моменту
// уже отработала: если переменная не дошла, отказ приходит от неё, а не от
// слушателя.
func TestRunAllowOpenEnvReachesListener(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	t.Setenv("VOIDSEARCH_TOR", "off")
	t.Setenv("VOIDSEARCH_TRANSPORT", "direct")
	t.Setenv("VOIDSEARCH_BACKUP_DIR", "")
	t.Setenv("VOIDSEARCH_HTTP_TOKEN", "")
	t.Setenv("VOIDSEARCH_HTTP_ALLOW_OPEN", "1")

	stdout, stderr, code := runMainSplit(t, "run", "--http", "192.0.2.1:18093", "--no-tor")
	combined := stdout + stderr
	if strings.Contains(combined, "открытый адрес без токена") {
		t.Errorf("переменная не дошла до политики: %s", firstN(combined, 400))
	}
	if code == 0 {
		t.Fatalf("код возврата 0: сервер поднялся на документационном адресе, вывод %s",
			firstN(combined, 300))
	}
	if !strings.Contains(combined, "mcp:") {
		t.Errorf("отказ пришёл не от слушателя: %s", firstN(combined, 400))
	}
}

// Флаг --http-allow-open делает то же, что переменная окружения.
func TestRunAllowOpenFlagReachesListener(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	t.Setenv("VOIDSEARCH_TOR", "off")
	t.Setenv("VOIDSEARCH_TRANSPORT", "direct")
	t.Setenv("VOIDSEARCH_BACKUP_DIR", "")
	t.Setenv("VOIDSEARCH_HTTP_TOKEN", "")
	t.Setenv("VOIDSEARCH_HTTP_ALLOW_OPEN", "")

	stdout, stderr, code := runMainSplit(t, "run", "--http", "192.0.2.1:18094",
		"--http-allow-open", "--no-tor")
	combined := stdout + stderr
	if strings.Contains(combined, "открытый адрес без токена") {
		t.Errorf("флаг не дошёл до политики: %s", firstN(combined, 400))
	}
	if code == 0 {
		t.Fatalf("код возврата 0: сервер поднялся на документационном адресе, вывод %s",
			firstN(combined, 300))
	}
	if !strings.Contains(combined, "mcp:") {
		t.Errorf("отказ пришёл не от слушателя: %s", firstN(combined, 400))
	}
}

// Связь полей config с политикой: петлевой адрес и stdio проходят без токена,
// открытый адрес проходит только с токеном или с переключателем осознанности.
// Проверять это нужно именно через listenPolicyFor, потому что команда run
// поднимает долгоживущий сервер и ждать её завершения в тесте нельзя.
func TestListenPolicyFor(t *testing.T) {
	allowed := []config.Config{
		{HTTPAddr: "127.0.0.1:18095"},
		{HTTPAddr: "localhost:18095"},
		{HTTPAddr: "[::1]:18095"},
		{HTTPAddr: ""},
		{HTTPAddr: "", HTTPToken: "s3cret"},
		{HTTPAddr: "0.0.0.0:18095", HTTPToken: "s3cret"},
		{HTTPAddr: "192.168.0.18:18095", HTTPToken: "s3cret"},
		{HTTPAddr: "0.0.0.0:18095", HTTPAllowOpen: true},
		{HTTPAddr: ":18095", HTTPAllowOpen: true},
	}
	for _, cfg := range allowed {
		if err := listenPolicyFor(cfg); err != nil {
			t.Errorf("адрес %q токен %q allowOpen=%v отклонены: %v",
				cfg.HTTPAddr, cfg.HTTPToken, cfg.HTTPAllowOpen, err)
		}
	}

	refused := []config.Config{
		{HTTPAddr: "0.0.0.0:18095"},
		{HTTPAddr: ":18095"},
		{HTTPAddr: "192.168.0.18:18095"},
		{HTTPAddr: "[::]:18095"},
		{HTTPAddr: "93.184.216.34:18095"},
		{HTTPAddr: "myhost.local:18095"},
		{HTTPAddr: "[::]:18095", HTTPToken: "   "},
	}
	for _, cfg := range refused {
		err := listenPolicyFor(cfg)
		if err == nil {
			t.Errorf("адрес %q токен %q принят, хотя обязан быть отклонён",
				cfg.HTTPAddr, cfg.HTTPToken)
			continue
		}
		if !errors.Is(err, mcpserver.ErrOpenAddressWithoutToken) {
			t.Errorf("адрес %q: ошибка %v не распознаётся через errors.Is", cfg.HTTPAddr, err)
		}
	}
}
