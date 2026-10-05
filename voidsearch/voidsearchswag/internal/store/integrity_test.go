package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCheckIntegrityOnLiveDatabase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "live.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertOnion(ctx, Onion{URL: "http://a.onion", Status: "live"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	got, err := CheckIntegrity(ctx, path)
	if err != nil {
		t.Fatalf("проверка живой базы: %v", err)
	}
	if got != "ok" {
		t.Errorf("integrity_check = %q, хочу ok", got)
	}
}

func TestCheckIntegrityOnSnapshot(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "main.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertOnion(ctx, Onion{URL: "http://b.onion", Status: "live"}); err != nil {
		t.Fatal(err)
	}
	dest, err := st.BackupRotate(ctx, filepath.Join(dir, "backups"), 7)
	if err != nil {
		t.Fatalf("снимок: %v", err)
	}

	got, err := CheckIntegrity(ctx, dest)
	if err != nil {
		t.Fatalf("проверка снимка: %v", err)
	}
	if got != "ok" {
		t.Errorf("integrity_check снимка = %q, хочу ok", got)
	}
}

// Проверка снимка не имеет права оставлять рядом файлы журнала: каталог бэкапов
// перечитывается ротацией и восстановлением по маске, и посторонний -wal рядом
// со снимком означал бы, что копия перестала быть самодостаточной.
func TestCheckIntegrityLeavesNoSideFiles(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "main.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	snapDir := filepath.Join(dir, "backups")
	dest, err := st.BackupRotate(ctx, snapDir, 7)
	if err != nil {
		t.Fatalf("снимок: %v", err)
	}

	if _, err := CheckIntegrity(ctx, dest); err != nil {
		t.Fatalf("проверка снимка: %v", err)
	}

	entries, err := os.ReadDir(snapDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("после проверки в каталоге снимков %d файлов: %v", len(entries), names)
	}
	if entries[0].Name() != filepath.Base(dest) {
		t.Errorf("в каталоге появился посторонний файл %s", entries[0].Name())
	}
}

// Файл, который базой не является, обязан дать ошибку, а не «ok»: восстановление
// из такого файла означало бы подменить базу мусором.
func TestCheckIntegrityRejectsGarbage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "garbage.db")
	body := []byte(strings.Repeat("это не база sqlite, а просто текст ", 40))
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	got, err := CheckIntegrity(ctx, path)
	if err == nil && got == "ok" {
		t.Fatalf("мусорный файл прошёл проверку: %q, %v", got, err)
	}
}

func TestCheckIntegrityEmptyPath(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := CheckIntegrity(ctx, "   "); err == nil {
		t.Fatal("пустой путь обязан давать ошибку")
	}
}

// База, которая опознаётся как SQLite, но побита внутри, обязана дать результат,
// отличный от "ok": именно такой снимок опаснее всего, потому что копируется на
// место рабочей базы без ошибки открытия.
func TestCheckIntegrityDetectsDamagedPage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "live.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := st.Migrate(ctx); err != nil {
		st.Close()
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		if err := st.UpsertOnion(ctx, Onion{URL: "http://h" + string(rune('a'+i%26)) + string(rune('0'+i/26)) + ".onion", Status: "live"}); err != nil {
			st.Close()
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	damaged := filepath.Join(dir, "damaged.db")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(damaged, body, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(damaged, os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	// Нули поверх второй страницы: там корень sqlite_master, и integrity_check
	// обязан заметить, что схема не читается.
	if _, err := f.WriteAt(make([]byte, 512), 4096); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := CheckIntegrity(ctx, damaged)
	if err == nil && got == "ok" {
		t.Fatalf("побитая страница прошла проверку: %q, %v", got, err)
	}
	t.Logf("integrity_check побитой базы: %q, err=%v", firstLine(got), err)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
