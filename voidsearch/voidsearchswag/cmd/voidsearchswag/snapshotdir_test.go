package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// snapshotFixture собирает каталог данных, в котором нет рабочей базы, но лежат
// снимки: один в самом каталоге (так получается при ручном переносе даты) и один
// в backups/ (штатное место). Рядом кладутся файлы, которые снимками не
// являются: рабочая база, чужой файл с тем же суффиксом и снимок с неверным
// расширением.
func snapshotFixture(t *testing.T) string {
	t.Helper()
	// Каталог снимков задаётся явно пустым значением: если в окружении машины
	// есть VOIDSEARCH_BACKUP_DIR, предупреждение стало бы искать снимки там и
	// тест начал бы зависеть от чужих настроек.
	t.Setenv("VOIDSEARCH_BACKUP_DIR", "")
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "backups"), 0o755); err != nil {
		t.Fatalf("каталог снимков: %v", err)
	}
	for _, name := range []string{
		filepath.Join(dir, "voidsearchswag-20260926-090000.000.db"),
		filepath.Join(dir, "backups", "voidsearchswag-20260927-120000.000.db"),
		filepath.Join(dir, "other-20260927-130000.000.db"),
		filepath.Join(dir, "voidsearchswag-20260927-140000.000.txt"),
	} {
		if err := os.WriteFile(name, []byte("fixture"), 0o644); err != nil {
			t.Fatalf("фикстура %s: %v", name, err)
		}
	}
	return dir
}

func TestListSnapshotsTakesOnlyOwnMask(t *testing.T) {
	dir := snapshotFixture(t)

	got := listSnapshots(dir, filepath.Join(dir, "backups"))
	if len(got) != 2 {
		t.Fatalf("найдено %d снимков, хочу 2: %v", len(got), got)
	}
	for _, p := range got {
		base := filepath.Base(p)
		if !strings.HasPrefix(base, "voidsearchswag-") || !strings.HasSuffix(base, ".db") {
			t.Errorf("в выборку попал посторонний файл %s", base)
		}
	}
}

func TestListSnapshotsDedupesSameDir(t *testing.T) {
	dir := snapshotFixture(t)

	// Каталог бэкапов по умолчанию может совпасть с каталогом данных, если
	// VOIDSEARCH_BACKUP_DIR указал на него же: дважды посчитанный снимок
	// завысил бы число в предупреждении.
	got := listSnapshots(dir, dir)
	if len(got) != 1 {
		t.Errorf("один и тот же каталог дал %d снимков, хочу 1: %v", len(got), got)
	}
}

func TestListSnapshotsSurvivesMissingAndEmptyDir(t *testing.T) {
	dir := snapshotFixture(t)

	got := listSnapshots(filepath.Join(dir, "нет-такого-каталога"), "")
	if len(got) != 0 {
		t.Errorf("несуществующий каталог дал %v", got)
	}
	got = listSnapshots(t.TempDir())
	if len(got) != 0 {
		t.Errorf("пустой каталог дал %v", got)
	}
	got = listSnapshots(filepath.Join(dir, "нет-такого-каталога"), dir)
	if len(got) != 1 {
		t.Errorf("список из двух каталогов, где первый не читается: %v", got)
	}
}

func TestWarnSnapshotOnlyDataDir(t *testing.T) {
	dir := snapshotFixture(t)
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	// Рабочей базы в фикстуре нет: именно так выглядит дата, из которой базу
	// удалили или не донесли при переносе.
	if _, err := os.Stat(filepath.Join(dir, "voidsearchswag.db")); err == nil {
		t.Fatal("в фикстуре оказалась рабочая база, предупреждение не сработает")
	}

	out := captureStderr(t, func() { warnSnapshotOnlyDataDir("stats") })
	if !strings.Contains(out, "внимание") {
		t.Fatalf("предупреждения нет: %q", out)
	}
	if !strings.Contains(out, "2 снимков") {
		t.Errorf("число снимков не названо: %q", out)
	}
	// Назван последний снимок по времени в имени, а не первый попавшийся: файл
	// с более ранней датой в фикстуре создан раньше, но имя решает.
	if !strings.Contains(out, "voidsearchswag-20260927-120000.000.db") {
		t.Errorf("последний снимок не назван: %q", out)
	}
	if !strings.Contains(out, "восстановление") || !strings.Contains(out, "-wal") {
		t.Errorf("в предупреждении нет порядка восстановления: %q", out)
	}
}

func TestWarnSilentWhenDatabasePresent(t *testing.T) {
	dir := snapshotFixture(t)
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	db := filepath.Join(dir, "voidsearchswag.db")
	if err := os.WriteFile(db, []byte("SQLite format 3\x00"), 0o644); err != nil {
		t.Fatalf("фикстура базы: %v", err)
	}

	out := captureStderr(t, func() { warnSnapshotOnlyDataDir("stats") })
	if out != "" {
		t.Errorf("при живой базе предупреждение не нужно: %q", out)
	}
}

func TestWarnSilentWhenNoSnapshots(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	t.Setenv("VOIDSEARCH_BACKUP_DIR", "")

	// Первый запуск - нормальное состояние: пустой каталог данных без снимков
	// не должен пугать оператора.
	out := captureStderr(t, func() { warnSnapshotOnlyDataDir("stats") })
	if out != "" {
		t.Errorf("в пустом каталоге данных предупреждение не нужно: %q", out)
	}
}

func TestWarnSkipsCommandsWithoutDatabase(t *testing.T) {
	dir := snapshotFixture(t)
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	for _, cmd := range []string{"version", "--version", "-v", "help", "--help", "-h"} {
		out := captureStderr(t, func() { warnSnapshotOnlyDataDir(cmd) })
		if out != "" {
			t.Errorf("%s не открывает базу, но предупредила: %q", cmd, out)
		}
	}
}

// Предупреждение обязано оставаться в stderr: машинный контракт - stdout, и
// поток, который разбирает скрипт, не должен получить кириллицу.
func TestSnapshotWarningGoesToStderrLive(t *testing.T) {
	dir := snapshotFixture(t)
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	t.Setenv("VOIDSEARCH_TOR", "off")
	t.Setenv("VOIDSEARCH_TRANSPORT", "direct")

	stdout, stderr, code := runMainSplit(t, "stats", "--json")
	if code != 0 {
		t.Fatalf("код возврата %d, хочу 0: пустая база - не отказ", code)
	}
	if !strings.Contains(stderr, "внимание: базы") {
		t.Errorf("в stderr нет предупреждения: %q", firstN(stderr, 200))
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(stdout), &body); err != nil {
		t.Fatalf("stdout перестал быть JSON: %v (%.200s)", err, stdout)
	}
	if strings.Contains(stdout, "внимание") {
		t.Errorf("предупреждение попало в stdout: %.200s", stdout)
	}
}

func TestSnapshotWarningAbsentForVersionLive(t *testing.T) {
	dir := snapshotFixture(t)
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	stdout, stderr, code := runMainSplit(t, "version")
	if code != 0 {
		t.Fatalf("код возврата %d, хочу 0", code)
	}
	if strings.TrimSpace(stdout) != "voidsearchswag "+version {
		t.Errorf("вывод version изменился: %q", stdout)
	}
	if stderr != "" {
		t.Errorf("version получила предупреждение в stderr: %q", stderr)
	}
}
