package main

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"voidsearchswag/internal/config"
)

// Живой замер до правки на HEAD 2bf9853, пересобранный бинарник, копия боевой
// базы, каталог снимков во временной папке:
//
//	VOIDSEARCH_BACKUP_KEEP=3 backup --json --dir ...
//	    rc=0, keep=7, pruned=0
//	backup --json --dir ... (без переменной окружения)
//	    rc=0, keep=7
//	backup --json --dir ... --keep 0
//	    rc=0, keep=7
//	backup --json --dir ... --keep 365
//	    rc=0, keep=30
//	backup --json --dir ... --keep -4
//	    rc=0, keep=7
//	backup -h
//	    -keep int
//	        сколько снимков держать (default 7)
//
// Дефолт флага семь совпадал с envDefault поля BackupKeep, поэтому переменная
// окружения не действовала вовсе, ноль и отрицательное значение молча превращались в
// семь, а потолок в тридцать снимков стоял только во флаге: config.Validate держит
// для BackupKeep диапазон от одного до трёхсот шестидесяти пяти.
func backupKeepApplied(t *testing.T, args ...string) int {
	t.Helper()
	full := append([]string{"backup", "--json"}, args...)
	stdout, stderr, code := runMainSplit(t, full...)
	if code != 0 {
		t.Fatalf("%s: код возврата %d, stderr: %s", strings.Join(full, " "), code, stderr)
	}
	var body struct {
		Keep int `json:"keep"`
	}
	if err := json.Unmarshal([]byte(stdout), &body); err != nil {
		t.Fatalf("%s: ответ не JSON: %v, вывод: %s", strings.Join(full, " "), err, firstN(stdout, 300))
	}
	return body.Keep
}

func TestBackupKeepTakesConfigValue(t *testing.T) {
	backupPruneFixture(t, 0)
	t.Setenv("VOIDSEARCH_BACKUP_KEEP", "3")

	if got := backupKeepApplied(t); got != 3 {
		t.Errorf("keep = %d, хочу 3 из VOIDSEARCH_BACKUP_KEEP", got)
	}
}

func TestBackupKeepZeroTakesConfigValue(t *testing.T) {
	backupPruneFixture(t, 0)
	t.Setenv("VOIDSEARCH_BACKUP_KEEP", "4")

	if got := backupKeepApplied(t, "--keep", "0"); got != 4 {
		t.Errorf("keep = %d, хочу 4 из конфига при явном нуле флага", got)
	}
}

func TestBackupKeepFlagOverridesConfigValue(t *testing.T) {
	backupPruneFixture(t, 0)
	t.Setenv("VOIDSEARCH_BACKUP_KEEP", "3")

	if got := backupKeepApplied(t, "--keep", "5"); got != 5 {
		t.Errorf("keep = %d, хочу 5 из флага, а не 3 из конфига", got)
	}
}

func TestBackupKeepWithoutEnvTakesConfigDefault(t *testing.T) {
	backupPruneFixture(t, 0)

	if got := backupKeepApplied(t); got != 7 {
		t.Errorf("keep = %d, хочу 7 из envDefault конфига", got)
	}
}

// Отрицательное значение больше не превращается молча в семь: оно означает ошибку
// оператора, и команда обязана назвать её до того, как коснётся каталога снимков.
func TestBackupRejectsNegativeKeep(t *testing.T) {
	backups := backupPruneFixture(t, 2)

	for _, value := range []string{"-4", "-1"} {
		stdout, stderr, code := runMainSplit(t, "backup", "--keep", value, "--json")
		want := "--keep должен быть положительным, получено " + value
		if !strings.Contains(stderr, want) {
			t.Errorf("--keep %s: stderr не называет причину, хочу %q, фактически %q", value, want, stderr)
		}
		if code != 1 {
			t.Errorf("--keep %s: код возврата %d, хочу 1", value, code)
		}
		if strings.Contains(stdout, "snapshot") {
			t.Errorf("--keep %s: снимок создан несмотря на отказ: %s", value, firstN(stdout, 200))
		}
		if got := countSnapshots(t, backups); got != 2 {
			t.Errorf("--keep %s: снимков %d, хочу 2 - отказ не имеет права ничего менять", value, got)
		}
	}
}

func TestBackupAcceptsKeepAtConfigCeiling(t *testing.T) {
	backupPruneFixture(t, 0)

	if got := backupKeepApplied(t, "--keep", strconv.Itoa(maxFlagBackupKeep)); got != maxFlagBackupKeep {
		t.Errorf("keep = %d, хочу %d на границе потолка", got, maxFlagBackupKeep)
	}
}

func TestBackupRejectsKeepAboveConfigCeiling(t *testing.T) {
	backups := backupPruneFixture(t, 2)

	for _, value := range []string{"366", "100000"} {
		stdout, stderr, code := runMainSplit(t, "backup", "--keep", value, "--json")
		want := "--keep слишком большой: " + value + " (предел " + strconv.Itoa(maxFlagBackupKeep) + ")"
		if !strings.Contains(stderr, want) {
			t.Errorf("--keep %s: stderr не называет значение и предел, хочу %q, фактически %q", value, want, stderr)
		}
		if code != 1 {
			t.Errorf("--keep %s: код возврата %d, хочу 1", value, code)
		}
		if strings.Contains(stdout, "snapshot") {
			t.Errorf("--keep %s: снимок создан несмотря на отказ: %s", value, firstN(stdout, 200))
		}
		if got := countSnapshots(t, backups); got != 2 {
			t.Errorf("--keep %s: снимков %d, хочу 2", value, got)
		}
	}
}

// Потолок флага обязан совпадать с границей, которую config.Validate держит для
// BackupKeep: одна величина не может быть законной из переменной окружения и
// незаконной из флага.
func TestMaxFlagBackupKeepMatchesConfigCeiling(t *testing.T) {
	if maxFlagBackupKeep != 365 {
		t.Errorf("maxFlagBackupKeep = %d, хочу 365: такова верхняя граница BackupKeep в config.Validate",
			maxFlagBackupKeep)
	}

	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	t.Setenv("VOIDSEARCH_BACKUP_DIR", "")

	t.Setenv("VOIDSEARCH_BACKUP_KEEP", strconv.Itoa(maxFlagBackupKeep))
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("конфиг отверг %d, хотя флаг его допускает: %v", maxFlagBackupKeep, err)
	}
	if cfg.BackupKeep != maxFlagBackupKeep {
		t.Errorf("конфиг дал BackupKeep=%d при заданном %d: граница разошлась с флагом",
			cfg.BackupKeep, maxFlagBackupKeep)
	}

	t.Setenv("VOIDSEARCH_BACKUP_KEEP", strconv.Itoa(maxFlagBackupKeep+1))
	over, err := config.Load()
	if err != nil {
		t.Fatalf("конфиг отверг %d с ошибкой, хотя должен сжать молча: %v", maxFlagBackupKeep+1, err)
	}
	if over.BackupKeep == maxFlagBackupKeep+1 {
		t.Errorf("конфиг оставил BackupKeep=%d: потолок флага отстал от потолка конфига", over.BackupKeep)
	}
}

func TestBackupHelpNamesKeepConfigRule(t *testing.T) {
	_, stderr, _ := runMainSplit(t, "backup", "-h")
	want := "сколько снимков держать, от 1 до 365 (0 - значение из конфига)"
	if !strings.Contains(stderr, want) {
		t.Errorf("справка backup не описывает правило нуля и диапазон целиком, хочу %q, фактически:\n%s",
			want, firstN(stderr, 1200))
	}
	if strings.Contains(stderr, "(default 7)") {
		t.Errorf("справка backup всё ещё печатает дефолт семь:\n%s", firstN(stderr, 1200))
	}
}
