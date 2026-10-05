package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Инварианты install.bat - единственной точки входа для того, кому лень
// руками. Батник нельзя прогнать в тесте кросс-платформенно, поэтому
// держим контракт по тексту: ключевые строки, продиктованные
// предыдущими этапами, обязаны присутствовать, а анти-паттерны -
// отсутствовать.
//
// 156: токен не в argv - в генерируемом start.bat токен читается из
// файла через VOIDSEARCH_HTTP_TOKEN_FILE, вызов run не несёт --token.
// 158: батник показывает версию сборки - релиз отличим от самосбора.
// 159: путь офлайн-провайдера - чуждая батнику тема, здесь её нет.
// Release-ветка: батник обязан находить готовую сборку и проверять её
// SHA256 по sums.txt перед копированием.
func TestInstallBatchScriptInvariants(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "install.bat"))
	if err != nil {
		t.Fatalf("install.bat не читается: %v", err)
	}
	s := string(body)

	mustContain := []string{
		"voidsearchswag-windows-amd64.exe", // режим релиз-ветки
		"certutil -hashfile",               // проверка суммы
		"sums.txt",
		"token.txt",                  // токен в файле
		"VOIDSEARCH_HTTP_TOKEN_FILE", // и только так
		"127.0.0.1:3333",             // loopback-адрес по умолчанию
		"setup",                      // качает Tor
		"VOIDSEARCH_TOR_DIST",        // подсказка про зеркало при 503
		"go build",                   // режим исходников
		"\"%BIN%\" version",          // печатает версию сборки
	}
	for _, want := range mustContain {
		if !strings.Contains(s, want) {
			t.Errorf("install.bat не содержит %q", want)
		}
	}

	if strings.Contains(s, "--token ") {
		t.Errorf("install.bat светит токен в argv: этап 156 запрещает")
	}
	if strings.Contains(s, "VOIDSEARCH_HTTP_TOKEN=") {
		t.Errorf("install.bat ставит токен в открытую env-строку start.bat: только token-file")
	}

	// Генератор start.bat обязан уважать уже выставленные переменные
	// окружения юзера: if not defined - не затирать чужие настройки.
	if !strings.Contains(s, "if not defined VOIDSEARCH_HTTP_ADDR") {
		t.Errorf("start.bat-генератор затирает VOIDSEARCH_HTTP_ADDR юзера")
	}
}
