package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Юнит systemd - боевой артефакт деплоя, и его свойства обязаны держаться
// против правки «на скорую руку»: запуск от root, токен в argv или снятая
// песочница не видны ни в одном существующем тесте, потому что юнит - не код.
//
// Замер этапа 156 показал суть дыры: run --token X раскрывал X в
// Win32_Process.CommandLine любому локальному пользователю, а юнит
// запускал бинарь от root без единой ограничительной директивы.
func TestServiceUnitKeepsTokenOutOfArgv(t *testing.T) {
	unit := readDoc(t, "voidsearchswag.service")
	for _, banned := range []string{"--token ", "--token=", "CHANGE_ME"} {
		if strings.Contains(unit, banned) {
			t.Errorf("юнит передаёт секрет в argv (%q): ps/tasklist показывают командную строку всем", banned)
		}
	}
	for _, want := range []string{
		"--token-file ",
		"ExecStart=/opt/voidsearchswag/voidsearchswag run --http 127.0.0.1:8800 --token-file",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("юнит не передаёт токен файлом, хочу %q", want)
		}
	}
}

func TestServiceUnitRunsUnprivilegedAndSandboxed(t *testing.T) {
	unit := readDoc(t, "voidsearchswag.service")
	// Не от root: бинарь ходит в сеть и запускает tor и chromium, причин
	// владеть машиной у него нет.
	for _, want := range []string{"User=", "Group=", "StateDirectory="} {
		if !strings.Contains(unit, want) {
			t.Errorf("юнит не называет %q: запуск от root остался возможен", want)
		}
	}
	for _, want := range []string{
		"NoNewPrivileges=true",
		"CapabilityBoundingSet=",
		"ProtectSystem=strict",
		"ProtectHome=true",
		"PrivateTmp=true",
		"ProtectKernelTunables=true",
		"ProtectControlGroups=true",
		"RestrictAddressFamilies=",
		"ReadWritePaths=/var/lib/voidsearchswag",
		"MemoryMax=",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("юнит не называет %q: песочница неполная", want)
		}
	}
	// HOME внутрь каталога данных: rod качает chromium в $HOME/.cache, а
	// ProtectHome=true прячет настоящий /home. Без этой строки сервис
	// перекачивал бы ~150 МБ после каждого рестарта.
	if !strings.Contains(unit, "Environment=HOME=/var/lib/voidsearchswag") {
		t.Error("юнит не задаёт HOME внутрь каталога данных: chromium теряет кэш")
	}
	if !strings.Contains(unit, "EnvironmentFile=") {
		t.Error("юнит не читает env-файл: секреты вернутся в unit-файл")
	}
}

func TestWindowsServiceDocKeepsTokenOutOfArgv(t *testing.T) {
	doc := readDoc(t, "windows-service.md")
	for _, banned := range []string{"--token CHANGE_ME", "--token-file CHANGE_ME", "Bearer CHANGE_ME"} {
		if strings.Contains(doc, banned) {
			t.Errorf("документ ставит службу с заглушкой секрета в argv: %q", banned)
		}
	}
	for _, want := range []string{
		"sc create voidsearchswag binPath=",
		"--token-file ",
		"Get-CimInstance Win32_Process",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("документ не показывает %q", want)
		}
	}
}

func readDoc(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", name))
	if err != nil {
		t.Fatalf("прочитать docs/%s: %v", name, err)
	}
	return string(b)
}
