package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCmdBackupCreatesSnapshot(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	t.Setenv("VOIDSEARCH_TOR", "off")

	out := captureStdout(t, func() { cmdBackup([]string{"--keep", "3"}) })
	if !strings.Contains(out, "снимок:") {
		t.Errorf("снимок не создан: %q", out)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "backups", "voidsearchswag-*.db"))
	if err != nil || len(matches) != 1 {
		t.Errorf("снимков %+v, ошибка %v", matches, err)
	}
}

func TestCmdBackupJSON(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	out := captureStdout(t, func() { cmdBackup([]string{"--json"}) })
	if !strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Errorf("JSON не объект: %q", out)
	}
	if !strings.Contains(out, `"snapshot"`) {
		t.Errorf("нет пути снимка: %q", out)
	}
	_ = os.Getenv("PATH")
}
