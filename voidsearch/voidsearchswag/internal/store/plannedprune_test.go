package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// PlannedPrune считает жертв ротации до того, как она случилась: спрашивать
// подтверждение после удаления файлов бессмысленно.
func TestPlannedPrune(t *testing.T) {
	dir := t.TempDir()

	// Каталога бэкапов на первом запуске нет, и это не ошибка: ротации там
	// делать нечего.
	got, err := PlannedPrune(filepath.Join(dir, "backups"), 7)
	if err != nil {
		t.Fatalf("несуществующий каталог: %v", err)
	}
	if got != 0 {
		t.Errorf("несуществующий каталог: planned=%d, хочу 0", got)
	}

	for _, name := range []string{
		"voidsearchswag-20260920-080000.000.db",
		"voidsearchswag-20260922-090000.000.db",
		"voidsearchswag-20260924-100000.000.db",
		"voidsearchswag-20260926-110000.000.db",
		// Посторонние файлы: чужая база и снимок с неверным расширением.
		"other-20260926-120000.000.db",
		"voidsearchswag-20260926-130000.000.txt",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("fixture"), 0o644); err != nil {
			t.Fatalf("фикстура %s: %v", name, err)
		}
	}

	for _, c := range []struct {
		keep int
		want int
		why  string
	}{
		{7, 0, "четыре снимка плюс новый укладываются в keep"},
		{5, 0, "ровно по границе: пять снимков после создания"},
		{4, 1, "штатная ротация: новый вытесняет один старый"},
		{1, 4, "сброс истории до одного снимка"},
		{0, 0, "keep<=0 трактуется как 7, как в PruneBackups и BackupRotate"},
		{-3, 0, "отрицательный keep тоже трактуется как 7"},
		{30, 0, "потолок keep не меняет расчёт"},
	} {
		got, err := PlannedPrune(dir, c.keep)
		if err != nil {
			t.Fatalf("keep=%d: %v", c.keep, err)
		}
		if got != c.want {
			t.Errorf("keep=%d: planned=%d, хочу %d (%s)", c.keep, got, c.want, c.why)
		}
	}
}

// Отсутствующий каталог - не ошибка, а вот прочитать существующий путь, который
// каталогом не является, обязано закончиться ошибкой: молча вернуть ноль значило
// бы пропустить ротацию там, где снимки всё-таки есть.
func TestPlannedPruneUnreadableDir(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("фикстура: %v", err)
	}
	got, err := PlannedPrune(file, 7)
	if err == nil {
		t.Fatalf("планирование на файле вместо каталога: %d, %v", got, err)
	}
	if got != 0 {
		t.Errorf("при ошибке план обязан быть нулевым, а не %d", got)
	}
	if !strings.Contains(err.Error(), "не каталог") {
		t.Errorf("в ошибке не названа причина: %v", err)
	}
}

// Расчёт обязан совпадать с тем, что реально удаляет PruneBackups после
// создания одного снимка: расхождение означало бы, что подтверждение
// запрашивается на одно число, а стирается другое.
func TestPlannedPruneMatchesPruneBackups(t *testing.T) {
	names := []string{
		"voidsearchswag-20260920-080000.000.db",
		"voidsearchswag-20260922-090000.000.db",
		"voidsearchswag-20260924-100000.000.db",
		"voidsearchswag-20260926-110000.000.db",
	}
	fresh := "voidsearchswag-20260928-080000.000.db"

	for _, keep := range []int{1, 2, 4, 5, 7} {
		// Каталог каждой итерации собирается заново: ротация удаляет файлы, и
		// на следующем keep сравнивать было бы уже нечего.
		dir := t.TempDir()
		for _, name := range names {
			if err := os.WriteFile(filepath.Join(dir, name), []byte("fixture"), 0o644); err != nil {
				t.Fatalf("фикстура %s: %v", name, err)
			}
		}

		planned, err := PlannedPrune(dir, keep)
		if err != nil {
			t.Fatalf("keep=%d: %v", keep, err)
		}
		// Имитация BackupRotate: один снимок добавлен, затем ротация.
		if err := os.WriteFile(filepath.Join(dir, fresh), []byte("fresh"), 0o644); err != nil {
			t.Fatalf("новый снимок: %v", err)
		}
		pruned, err := PruneBackups(dir, keep)
		if err != nil {
			t.Fatalf("PruneBackups keep=%d: %v", keep, err)
		}
		if pruned != planned {
			t.Errorf("keep=%d: план %d, факт %d", keep, planned, pruned)
		}
		if _, err := os.Stat(filepath.Join(dir, fresh)); err != nil {
			t.Errorf("keep=%d: ротация удалила свежий снимок: %v", keep, err)
		}
	}
}
