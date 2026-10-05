package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTokenFileAppliesWhenTokenEmpty(t *testing.T) {
	tok := filepath.Join(t.TempDir(), "tok.txt")
	if err := os.WriteFile(tok, []byte("  s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VOIDSEARCH_HTTP_TOKEN", "")
	t.Setenv("VOIDSEARCH_HTTP_TOKEN_FILE", tok)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.HTTPToken != "s3cret" {
		t.Errorf("HTTPToken=%q", c.HTTPToken)
	}
}

func TestExplicitTokenWinsOverFile(t *testing.T) {
	tok := filepath.Join(t.TempDir(), "tok.txt")
	if err := os.WriteFile(tok, []byte("from-file"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VOIDSEARCH_HTTP_TOKEN", "explicit")
	t.Setenv("VOIDSEARCH_HTTP_TOKEN_FILE", tok)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.HTTPToken != "explicit" {
		t.Errorf("HTTPToken=%q", c.HTTPToken)
	}
}

func TestMissingTokenFileIsLoud(t *testing.T) {
	// Прежнее поведение было дефектом, и тест его закреплял: отсутствующий
	// файл токена молча оставлял HTTPToken пустым, а пустой токен означает
	// «аутентификация выключена». Оператор, который явно задал
	// VOIDSEARCH_HTTP_TOKEN_FILE, безусловно хочет защиту; опечатка в пути
	// давала полностью открытый HTTP-сервер (fetch, collect_files,
	// backup_create, /peer/export, /metrics) и единственное общее
	// предупреждение, которое читалось как шум.
	//
	// Теперь Load обязан вернуть ошибку, чтобы сервер не стартовал открытым.
	t.Setenv("VOIDSEARCH_HTTP_TOKEN", "")
	t.Setenv("VOIDSEARCH_HTTP_TOKEN_FILE", filepath.Join(t.TempDir(), "nope.txt"))
	c, err := Load()
	if err == nil {
		t.Fatalf("отсутствующий файл токена не вызвал ошибку, HTTPToken=%q", c.HTTPToken)
	}
	if !strings.Contains(err.Error(), "nope.txt") {
		t.Errorf("в ошибке нет пути к файлу: %v", err)
	}
}

func TestEmptyTokenFileIsLoud(t *testing.T) {
	// Пустой файл - та же ловушка: прочитался без ошибки, токен пустой,
	// сервер открыт. Отличать «файл не читается» от «файл пуст» полезно в
	// диагностике, но оба случая обязаны быть громкими.
	tok := filepath.Join(t.TempDir(), "tok.txt")
	if err := os.WriteFile(tok, []byte("   \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VOIDSEARCH_HTTP_TOKEN", "")
	t.Setenv("VOIDSEARCH_HTTP_TOKEN_FILE", tok)
	if _, err := Load(); err == nil {
		t.Error("пустой файл токена не вызвал ошибку")
	} else if !strings.Contains(err.Error(), "пуст") {
		t.Errorf("ошибка не объясняет причину: %v", err)
	}
}

func TestBackupDirDefault(t *testing.T) {
	t.Setenv("VOIDSEARCH_BACKUP_DIR", "")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.BackupDirOrDefault() == "" {
		t.Error("пустой каталог бэкапов")
	}
	if !strings.HasSuffix(c.BackupDirOrDefault(), "backups") {
		t.Errorf("дефолт не backups: %q", c.BackupDirOrDefault())
	}
	t.Setenv("VOIDSEARCH_BACKUP_DIR", "/x/backups")
	c, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.BackupDirOrDefault() != "/x/backups" {
		t.Errorf("явный каталог потерян: %q", c.BackupDirOrDefault())
	}
}
