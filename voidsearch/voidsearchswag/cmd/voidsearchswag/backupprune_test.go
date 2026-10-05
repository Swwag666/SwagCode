package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// backupPruneFixture готовит каталог данных с backups/, в котором лежат n файлов
// с именами снимков. Содержимое не важно: ротация выбирает жертвы по имени, а
// настоящий снимок создаёт VACUUM INTO из базы, которую команда заведёт сама.
// Имена заведомо старше любого снимка, который команда создаст сейчас.
func backupPruneFixture(t *testing.T, n int) string {
	t.Helper()
	dir := t.TempDir()
	backups := filepath.Join(dir, "backups")
	if err := os.MkdirAll(backups, 0o755); err != nil {
		t.Fatalf("каталог снимков: %v", err)
	}
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("voidsearchswag-202609%02d-080000.000.db", i+1)
		if err := os.WriteFile(filepath.Join(backups, name), []byte("fixture"), 0o644); err != nil {
			t.Fatalf("фикстура снимка %s: %v", name, err)
		}
	}
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	t.Setenv("VOIDSEARCH_BACKUP_DIR", "")
	t.Setenv("VOIDSEARCH_TOR", "off")
	t.Setenv("VOIDSEARCH_TRANSPORT", "direct")
	return backups
}

// countSnapshots считает файлы с маской имени снимка, игнорируя остальное.
func countSnapshots(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("чтение каталога снимков: %v", err)
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "voidsearchswag-") && strings.HasSuffix(e.Name(), ".db") {
			n++
		}
	}
	return n
}

// До правки backup --keep 1 на каталоге из четырёх снимков возвращал rc=0 и
// ответ {"keep":1,"snapshot":...}: история резервных копий исчезала молча.
func TestCmdBackupRefusesMassPrune(t *testing.T) {
	backups := backupPruneFixture(t, 4)

	stdout, stderr, code := runMainSplit(t, "backup", "--keep", "1", "--json")
	if code != 1 {
		t.Fatalf("код возврата %d, хочу 1: стирание истории требует подтверждения (stdout %.200s)", code, stdout)
	}
	if got := countSnapshots(t, backups); got != 4 {
		t.Errorf("снимков %d, хочу 4: отказ обязан оставить историю нетронутой", got)
	}
	if !strings.Contains(stderr, "--prune") {
		t.Errorf("в stderr нет способа подтвердить: %q", firstN(stderr, 300))
	}
	if !strings.Contains(stderr, "4 снимков") {
		t.Errorf("в stderr нет числа жертв: %q", firstN(stderr, 300))
	}
	// Отказ в машинном режиме - JSON в stdout (этап 87), а не пустой поток.
	var body map[string]any
	if err := json.Unmarshal([]byte(stdout), &body); err != nil {
		t.Fatalf("stdout при --json не разбирается: %v (%.200s)", err, stdout)
	}
	reason, _ := body["error"].(string)
	if !strings.Contains(reason, "--prune") {
		t.Errorf("в машинной ошибке нет способа подтвердить: %q", reason)
	}
}

func TestCmdBackupPruneFlagConfirms(t *testing.T) {
	backups := backupPruneFixture(t, 4)

	stdout, stderr, code := runMainSplit(t, "backup", "--keep", "1", "--prune", "--json")
	if code != 0 {
		t.Fatalf("код возврата %d, хочу 0 (stderr %q)", code, firstN(stderr, 300))
	}
	if got := countSnapshots(t, backups); got != 1 {
		t.Errorf("снимков %d, хочу 1: --prune разрешает ротацию", got)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(stdout), &body); err != nil {
		t.Fatalf("stdout не JSON: %v (%.200s)", err, stdout)
	}
	if body["pruned"] != float64(4) {
		t.Errorf("pruned = %v, хочу 4: ответ обязан называть число стёртых", body["pruned"])
	}
	snap, _ := body["snapshot"].(string)
	if !strings.HasSuffix(snap, ".db") {
		t.Errorf("новый снимок не назван: %+v", body)
	}
}

// Штатная ротация не требует подтверждения: каждый вызов добавляет один снимок
// и ровно один вытесняет. Иначе флаг пришлось бы передавать каждый день.
func TestCmdBackupUsualRotationNeedsNoFlag(t *testing.T) {
	backups := backupPruneFixture(t, 7)

	stdout, stderr, code := runMainSplit(t, "backup", "--json")
	if code != 0 {
		t.Fatalf("код возврата %d, хочу 0: штатная ротация не должна требовать флаг (stderr %q)", code, firstN(stderr, 300))
	}
	if got := countSnapshots(t, backups); got != 7 {
		t.Errorf("снимков %d, хочу 7", got)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(stdout), &body); err != nil {
		t.Fatalf("stdout не JSON: %v (%.200s)", err, stdout)
	}
	if body["pruned"] != float64(1) {
		t.Errorf("pruned = %v, хочу 1", body["pruned"])
	}
}

// Пустой каталог бэкапов - первый запуск: команда обязана работать без флага.
func TestCmdBackupFirstRunNeedsNoFlag(t *testing.T) {
	backups := backupPruneFixture(t, 0)

	stdout, stderr, code := runMainSplit(t, "backup", "--keep", "1", "--json")
	if code != 0 {
		t.Fatalf("код возврата %d, хочу 0 (stderr %q)", code, firstN(stderr, 300))
	}
	if got := countSnapshots(t, backups); got != 1 {
		t.Errorf("снимков %d, хочу 1", got)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(stdout), &body); err != nil {
		t.Fatalf("stdout не JSON: %v (%.200s)", err, stdout)
	}
	if body["pruned"] != float64(0) {
		t.Errorf("pruned = %v, хочу 0: стирать было нечего", body["pruned"])
	}
}

func TestCmdBackupTextReportsPruned(t *testing.T) {
	backups := backupPruneFixture(t, 2)

	stdout, stderr, code := runMainSplit(t, "backup", "--keep", "1", "--prune")
	if code != 0 {
		t.Fatalf("код возврата %d, хочу 0 (stderr %q)", code, firstN(stderr, 300))
	}
	if got := countSnapshots(t, backups); got != 1 {
		t.Errorf("снимков %d, хочу 1", got)
	}
	if !strings.Contains(stdout, "снимок:") {
		t.Errorf("текстовый ответ потерял путь к снимку: %q", stdout)
	}
	if !strings.Contains(stdout, "ротация удалила старых снимков: 2") {
		t.Errorf("текстовый отчёт не назвал число стёртых: %q", stdout)
	}
	if strings.Contains(stdout, "{") {
		t.Errorf("без --json появился JSON: %q", stdout)
	}
}

// Явный каталог снимков тоже защищён: флаг --dir меняет место, но не правило.
func TestCmdBackupExplicitDirIsGuarded(t *testing.T) {
	backupPruneFixture(t, 0)
	other := t.TempDir()
	for i := 0; i < 3; i++ {
		name := fmt.Sprintf("voidsearchswag-202609%02d-080000.000.db", i+1)
		if err := os.WriteFile(filepath.Join(other, name), []byte("fixture"), 0o644); err != nil {
			t.Fatalf("фикстура %s: %v", name, err)
		}
	}

	_, stderr, code := runMainSplit(t, "backup", "--dir", other, "--keep", "1", "--json")
	if code != 1 {
		t.Fatalf("код возврата %d, хочу 1", code)
	}
	if got := countSnapshots(t, other); got != 3 {
		t.Errorf("снимков %d, хочу 3: отказ не трогает файлы", got)
	}
	if !strings.Contains(stderr, other) {
		t.Errorf("в отказе не назван каталог: %q", firstN(stderr, 300))
	}
}
