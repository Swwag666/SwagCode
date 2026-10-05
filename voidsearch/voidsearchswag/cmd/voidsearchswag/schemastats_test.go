package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"voidsearchswag/internal/store"
)

// Номер версии равен позиции оператора в списке миграций: 11 - это CREATE INDEX
// idx_onion_status. При дописывании новых миграций в конец номер не меняется.
const journalHoleVersion = 11

func TestStatsReportsSchemaVersion(t *testing.T) {
	dir := seedStatsBase(t)
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	out, code := runMain(t, "stats")
	if code != 0 {
		t.Fatalf("код возврата %d, хочу 0: %s", code, out)
	}
	want := fmt.Sprintf("схема: %d/%d", store.MigrationCount(), store.MigrationCount())
	if !strings.Contains(out, want) {
		t.Errorf("в выводе нет %q: %s", want, out)
	}
	if strings.Contains(out, "восстановлены пропущенные") {
		t.Errorf("на здоровой базе напечатано лечение: %s", out)
	}
}

func TestStatsJSONCarriesSchema(t *testing.T) {
	dir := seedStatsBase(t)
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	out, code := runMain(t, "stats", "--json")
	if code != 0 {
		t.Fatalf("код возврата %d, хочу 0: %s", code, out)
	}
	var body struct {
		SchemaKnown    int   `json:"schema_known"`
		SchemaApplied  int   `json:"schema_applied"`
		SchemaRepaired []int `json:"schema_repaired"`
	}
	if err := json.Unmarshal([]byte(out), &body); err != nil {
		t.Fatalf("JSON не разобран: %v\nвывод: %s", err, out)
	}
	if body.SchemaKnown != store.MigrationCount() {
		t.Errorf("schema_known = %d, хочу %d", body.SchemaKnown, store.MigrationCount())
	}
	if body.SchemaApplied != store.MigrationCount() {
		t.Errorf("schema_applied = %d, хочу %d", body.SchemaApplied, store.MigrationCount())
	}
	if len(body.SchemaRepaired) != 0 {
		t.Errorf("на здоровой базе schema_repaired = %v", body.SchemaRepaired)
	}
}

// База, мигрированная бинарём новее, обязана давать отказ: печатать счётчики по
// схеме, которую этот код не знает, значит сообщать цифры, которым нельзя верить.
func TestStatsRefusesSchemaFromTheFuture(t *testing.T) {
	dir := seedStatsBase(t)
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	db := sqlDB(t, dir)
	if _, err := db.ExecContext(context.Background(), `INSERT INTO schema_migrations(version) VALUES (999)`); err != nil {
		t.Fatalf("фикстура: %v", err)
	}

	out, code := runMain(t, "stats")
	if code != 1 {
		t.Errorf("код возврата %d, хочу 1: %s", code, out)
	}
	for _, want := range []string{"999", "миграции", fmt.Sprint(store.MigrationCount())} {
		if !strings.Contains(out, want) {
			t.Errorf("в отказе нет %q: %s", want, out)
		}
	}
	if strings.Contains(out, "пул: ") {
		t.Errorf("отказ напечатал счётчики: %s", out)
	}

	// Машинный режим: причина полем error в stdout, поток разбирается, код
	// возврата тот же. Поток читается раздельно, потому что fatalf печатает
	// человеку строку в stderr и машину в stdout одновременно.
	jout, jerr, jcode := runMainSplit(t, "stats", "--json")
	if jcode != 1 {
		t.Errorf("код возврата JSON %d, хочу 1", jcode)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(jout), &body); err != nil {
		t.Fatalf("JSON не разобран: %v\nstdout: %s", err, jout)
	}
	reason, _ := body["error"].(string)
	if !strings.Contains(reason, "999") {
		t.Errorf("в JSON нет чужой версии: %v", body)
	}
	if !strings.Contains(jerr, "999") {
		t.Errorf("в stderr нет причины для человека: %s", jerr)
	}
}

// Дырка в журнале лечится командой, и факт лечения виден: молча починить схему и
// не сказать - то же замалчивание, из-за которого дырка вообще стала возможной.
func TestStatsRepairsHoleAndReports(t *testing.T) {
	dir := seedStatsBase(t)
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	db := sqlDB(t, dir)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version=?`, journalHoleVersion); err != nil {
		t.Fatalf("фикстура: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DROP INDEX IF EXISTS idx_onion_status`); err != nil {
		t.Fatalf("фикстура: %v", err)
	}

	out, code := runMain(t, "stats")
	if code != 0 {
		t.Fatalf("код возврата %d, хочу 0: %s", code, out)
	}
	if !strings.Contains(out, fmt.Sprintf("восстановлены пропущенные миграции: [%d]", journalHoleVersion)) {
		t.Errorf("в выводе нет факта лечения: %s", out)
	}
	if !strings.Contains(out, fmt.Sprintf("схема: %d/%d", store.MigrationCount(), store.MigrationCount())) {
		t.Errorf("в выводе нет полной схемы: %s", out)
	}

	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_onion_status'`).Scan(&n); err != nil {
		t.Fatalf("sqlite_master: %v", err)
	}
	if n != 1 {
		t.Errorf("индекс не восстановлен: %d записей", n)
	}

	// Повторный прогон молчит: лечить больше нечего.
	second, secondCode := runMain(t, "stats")
	if secondCode != 0 {
		t.Fatalf("повторный прогон: код %d: %s", secondCode, second)
	}
	if strings.Contains(second, "восстановлены пропущенные") {
		t.Errorf("повторный прогон снова рапортует лечение: %s", second)
	}
}
