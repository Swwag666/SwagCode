package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBackupCreatesReadableSnapshot(t *testing.T) {
	st, err := Open(t.TempDir() + "/b.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertOnion(ctx, Onion{URL: "http://a.onion", Status: "live"}); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	dest, err := st.BackupRotate(ctx, dir, 7)
	if err != nil {
		t.Fatal(err)
	}
	// Снимок обязан открываться как полноценная база с теми же данными.
	ro, err := Open(filepath.Join(dir, filepath.Base(dest)))
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	o, err := ro.GetOnion(ctx, "http://a.onion")
	if err != nil {
		t.Fatalf("снимок без данных: %v", err)
	}
	if o.Status != "live" {
		t.Errorf("статус в снимке %q", o.Status)
	}
}

func TestBackupRejectsEmptyDir(t *testing.T) {
	st, err := Open(t.TempDir() + "/b.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.BackupRotate(context.Background(), "  ", 7); err == nil {
		t.Error("пустой каталог принят")
	}
}

func TestPruneBackupsKeepsNewest(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{
		"voidsearchswag-20240101-000000.db",
		"voidsearchswag-20240102-000000.db",
		"voidsearchswag-20240103-000000.db",
		"notes.txt",
	} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pruned, err := PruneBackups(dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 1 {
		t.Errorf("вычищено %d, ожидала 1", pruned)
	}
	// Старейший снимок ушёл, средний и свежий целы, чужой файл не тронут.
	for _, n := range []string{"voidsearchswag-20240102-000000.db", "voidsearchswag-20240103-000000.db", "notes.txt"} {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Errorf("файл %s пропал: %v", n, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "voidsearchswag-20240101-000000.db")); err == nil {
		t.Error("старейший снимок не удалён")
	}
}

func TestPruneBackupsMissingDir(t *testing.T) {
	if _, err := PruneBackups(filepath.Join(t.TempDir(), "нет-каталога"), 7); err == nil {
		t.Error("отсутствующий каталог принят")
	}
}

// TestBackupRotateTwiceInSameSecond закрывает коллизию имён снимков.
//
// SQLite VACUUM INTO завершается ошибкой, если файл назначения уже существует,
// а имена снимков различались только до секунды. Два вызова подряд -
// backup_create дважды или ручной backup одновременно с фоновым тиком - падали
// с невнятной ошибкой SQLite, и PruneBackups после этого не выполнялся, поэтому
// ротация молча останавливалась.
func TestBackupRotateTwiceInSameSecond(t *testing.T) {
	st, err := Open(t.TempDir() + "/b.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()

	first, err := st.BackupRotate(ctx, dir, 7)
	if err != nil {
		t.Fatalf("первый снимок: %v", err)
	}
	if _, err := os.Stat(first); err != nil {
		t.Fatalf("первый снимок не создан: %v", err)
	}

	second, err := st.BackupRotate(ctx, dir, 7)
	if err != nil {
		t.Fatalf("второй снимок в ту же секунду: %v", err)
	}
	if first == second {
		t.Error("оба снимка получили одно имя")
	}
	if _, err := os.Stat(second); err != nil {
		t.Fatalf("второй снимок не создан: %v", err)
	}
}

func TestBackupRotateNameHasFractionalSeconds(t *testing.T) {
	st, err := Open(t.TempDir() + "/b.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	dest, err := st.BackupRotate(ctx, t.TempDir(), 7)
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Base(dest)
	if !strings.HasPrefix(base, "voidsearchswag-") || !strings.HasSuffix(base, ".db") {
		t.Fatalf("имя вне маски: %s", base)
	}
	// Маска YYYYMMDD-HHMMSS.mmm.db: две точки - перед долями и перед расширением.
	if strings.Count(base, ".") != 2 {
		t.Errorf("в имени нет долей секунды: %s", base)
	}
}

func TestBackupRejectsExistingDestination(t *testing.T) {
	// Если коллизия всё же случилась, VACUUM INTO обязан упасть, а не
	// перезаписать прежний снимок молча.
	st, err := Open(t.TempDir() + "/b.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	dest, err := st.BackupRotate(ctx, t.TempDir(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Backup(ctx, dest); err == nil {
		t.Error("VACUUM INTO на существующий файл не вызвал ошибку")
	}
}

func TestPruneBackupsHandlesFractionalNames(t *testing.T) {
	// Доли секунды не должны ломать ни маску, ни хронологический порядок:
	// PruneBackups сортирует имена лексикографически и фильтрует по префиксу
	// и суффиксу.
	dir := t.TempDir()
	for _, n := range []string{
		"voidsearchswag-20240101-000000.000.db",
		"voidsearchswag-20240102-000000.500.db",
		"voidsearchswag-20240103-000000.999.db",
		"voidsearchswag-20240104-000000.001.db",
		"чужой-файл.db",
		"voidsearchswag-20240105-000000.002.txt",
	} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pruned, err := PruneBackups(dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 2 {
		t.Errorf("удалено %d, ожидала 2", pruned)
	}
	for _, keep := range []string{
		"чужой-файл.db",
		"voidsearchswag-20240105-000000.002.txt",
		"voidsearchswag-20240103-000000.999.db",
		"voidsearchswag-20240104-000000.001.db",
	} {
		if _, err := os.Stat(filepath.Join(dir, keep)); err != nil {
			t.Errorf("файл %s пропал: %v", keep, err)
		}
	}
	for _, gone := range []string{
		"voidsearchswag-20240101-000000.000.db",
		"voidsearchswag-20240102-000000.500.db",
	} {
		if _, err := os.Stat(filepath.Join(dir, gone)); err == nil {
			t.Errorf("старый снимок %s не удалён", gone)
		}
	}
}

func TestEngineSeedsRoundtrip(t *testing.T) {
	st, err := Open(t.TempDir() + "/e.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	if err := st.UpsertEngineSeed(ctx, EngineSeed{Name: "mydrive", Base: "http://mydrive.onion", Auto: true}); err != nil {
		t.Fatal(err)
	}
	got, err := st.ListEngineSeeds(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("сидов %d", len(got))
	}
	// Дефолты подставились: путь, селектор, категория, auto.
	if got[0].Path != "/search?q={q}" || got[0].Selector != "a[href*='.onion']" {
		t.Errorf("дефолты не подставились: %+v", got[0])
	}
	if got[0].Category != "general" || !got[0].Auto {
		t.Errorf("флаги неверны: %+v", got[0])
	}
	if err := st.TouchEngineSeed(ctx, "mydrive"); err != nil {
		t.Errorf("touch: %v", err)
	}
	if err := st.TouchEngineSeed(ctx, "нет-такого"); err == nil {
		t.Error("touch чужого имени прошёл")
	}
	if err := st.UpsertEngineSeed(ctx, EngineSeed{}); err == nil {
		t.Error("пустой сид принят")
	}
}
