package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Токен-файл - единственный способ передать секрет, не светя его в командной
// строке (замер этапа 156: run --token X показывал X в Win32_Process). Оба
// пути обязаны падать громко: молчаливая ошибка оставляет сервер без токена,
// а «HTTP без токена» на петлевом адресе - это открытый всем fetch и backup.
func TestRunTokenFileFlagMissingFileIsFatal(t *testing.T) {
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	t.Setenv("VOIDSEARCH_HTTP_TOKEN", "")
	t.Setenv("VOIDSEARCH_HTTP_TOKEN_FILE", "")

	// Документационный адрес: при молчаливом проглатывании ошибки файла
	// сервер дошёл бы до политики открытого адреса и отказался бы там, а не
	// повис бы слушателем - тест различает эти отказы по словам.
	stdout, stderr, code := runMainSplit(t, "run", "--http", "192.0.2.1:18097",
		"--no-tor", "--token-file", filepath.Join(dir, "нет-такого.txt"))
	combined := stdout + stderr
	if code == 0 {
		t.Fatalf("код возврата 0 при битом файле токена: сервер поднялся открытым")
	}
	// Отказ политики открытого адреса тоже содержит слово «токен», но с
	// другим префиксом («слушатель: ... без токена: ...»). Ошибка файла
	// обязана звучать как «токен: ...», иначе битый файл молчит под
	// чужим сообщением.
	if !strings.Contains(combined, "токен: ") {
		t.Errorf("ошибка не названа ошибкой токена: %s", firstN(combined, 300))
	}
}

// Переменная окружения применяется той же логикой в config.Load: юнит
// systemd передаёт её через EnvironmentFile, и опечатка в пути обязана
// останавливать запуск, а не раскрывать сервер.
func TestRunTokenFileEnvMissingFileIsFatal(t *testing.T) {
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	t.Setenv("VOIDSEARCH_HTTP_TOKEN", "")
	t.Setenv("VOIDSEARCH_HTTP_TOKEN_FILE", filepath.Join(dir, "тоже-нет.txt"))

	stdout, stderr, code := runMainSplit(t, "run", "--http", "127.0.0.1:18098", "--no-tor")
	combined := stdout + stderr
	if code == 0 {
		t.Fatalf("код возврата 0 при битом env-файле токена: сервер поднялся открытым")
	}
	if !strings.Contains(combined, "токен") {
		t.Errorf("ошибка не названа ошибкой токена: %s", firstN(combined, 300))
	}
}

// Позитивный путь без долгоживущего сервера: файл читается, токен попадает в
// конфиг, политика слушателя пропускает открытый адрес. Проверка через
// listenPolicyFor - тот же вход, которым командует run до поднятия слушателя.
func TestRunTokenFileFlagSatisfiesListenPolicy(t *testing.T) {
	dir := t.TempDir()
	tok := filepath.Join(dir, "http-token.txt")
	if err := os.WriteFile(tok, []byte("s3cret-from-file"), 0o600); err != nil {
		t.Fatal(err)
	}
	setOfflineProbeEnv(t, dir)
	t.Setenv("VOIDSEARCH_HTTP_TOKEN", "")
	t.Setenv("VOIDSEARCH_HTTP_TOKEN_FILE", tok)

	stdout, stderr, code := runMainSplit(t, "run", "--http", "192.0.2.1:18099", "--no-tor")
	combined := stdout + stderr
	// Документационный адрес не биндится, поэтому сервер упадёт от слушателя -
	// но не от политики: это и значит, что токен из файла дошёл до
	// CheckListenPolicy.
	if strings.Contains(combined, "открытый адрес без токена") {
		t.Fatalf("токен из файла не дошёл до политики: %s", firstN(combined, 300))
	}
	if code == 0 {
		t.Fatalf("код возврата 0: сервер не должен подняться на документационном адресе")
	}
	if !strings.Contains(combined, "mcp:") {
		t.Errorf("отказ пришёл не от слушателя: %s", firstN(combined, 300))
	}
}
