package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// Версия 11 в списке миграций - это CREATE INDEX idx_onion_status, версия 9 -
// ALTER TABLE onion_pool ADD COLUMN title. Номера равны позициям операторов,
// поэтому тесты ниже называют их явно: при дописывании новой миграции в конец
// эти номера не меняются.
const (
	versionAlterTitle = 9
	versionIndexState = 11
)

func journalHas(t *testing.T, st *Store, version int) bool {
	t.Helper()
	var n int
	if err := st.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM schema_migrations WHERE version=?`, version).Scan(&n); err != nil {
		t.Fatalf("журнал: %v", err)
	}
	return n > 0
}

func indexExists(t *testing.T, st *Store, name string) bool {
	t.Helper()
	var n int
	if err := st.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?`, name).Scan(&n); err != nil {
		t.Fatalf("sqlite_master: %v", err)
	}
	return n > 0
}

// База, мигрированная бинарём новее, не может быть принята молча: код обращается
// к схеме, которой не знает, и первая ошибка вылезает в произвольном запросе без
// указания причины.
func TestMigrateRejectsSchemaFromTheFuture(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `INSERT INTO schema_migrations(version) VALUES (999)`); err != nil {
		t.Fatal(err)
	}

	err := st.Migrate(ctx)
	if err == nil {
		t.Fatal("база с версией 999 принята молча")
	}
	for _, want := range []string{"999", fmt.Sprint(MigrationCount()), "снимок"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("в отказе нет %q: %v", want, err)
		}
	}
	run := st.Migrations()
	if len(run.Foreign) != 1 || run.Foreign[0] != 999 {
		t.Errorf("чужие версии = %v, хочу [999]", run.Foreign)
	}
}

// Несколько чужих версий перечисляются все, а в тексте называется максимальная:
// оператору важно видеть, насколько база впереди бинаря.
func TestMigrateForeignListsEveryUnknownVersion(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `INSERT INTO schema_migrations(version) VALUES (30), (999), (41)`); err != nil {
		t.Fatal(err)
	}

	err := st.Migrate(ctx)
	if err == nil {
		t.Fatal("чужие версии приняты молча")
	}
	if !strings.Contains(err.Error(), "до версии 999") {
		t.Errorf("в отказе нет максимальной версии: %v", err)
	}
	run := st.Migrations()
	if fmt.Sprint(run.Foreign) != "[30 41 999]" {
		t.Errorf("чужие версии = %v, хочу [30 41 999]", run.Foreign)
	}
}

// Дырка в журнале оставляла базу без объекта навсегда: максимум рос, пропущенный
// оператор не выполнялся, и снаружи всё выглядело благополучно.
func TestMigrateRepairsHoleInJournal(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if !indexExists(t, st, "idx_onion_status") {
		t.Fatal("фикстура: индекса нет до поломки журнала")
	}
	if _, err := st.db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version=?`, versionIndexState); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `DROP INDEX IF EXISTS idx_onion_status`); err != nil {
		t.Fatal(err)
	}
	if indexExists(t, st, "idx_onion_status") {
		t.Fatal("фикстура: индекс не удалён")
	}

	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("лечение дырки: %v", err)
	}
	if !indexExists(t, st, "idx_onion_status") {
		t.Error("пропущенный индекс не восстановлен")
	}
	if !journalHas(t, st, versionIndexState) {
		t.Error("версия не вернулась в журнал")
	}
	run := st.Migrations()
	if fmt.Sprint(run.Repaired) != fmt.Sprint([]int{versionIndexState}) {
		t.Errorf("восстановленные версии = %v, хочу [%d]", run.Repaired, versionIndexState)
	}
	if run.Applied != run.Known || run.Known != MigrationCount() {
		t.Errorf("схема %d/%d, хочу %d/%d", run.Applied, run.Known, MigrationCount(), MigrationCount())
	}
	if len(run.Foreign) != 0 {
		t.Errorf("чужих версий быть не должно: %v", run.Foreign)
	}
}

// Пропущенный оператор может оказаться несовместим с уже применёнными: колонка
// добавлена вручную или журнал правили руками. Молча проглотить такую ошибку
// значит оставить оператора без объяснения, поэтому отказ называет номер версии.
func TestMigrateHoleWithIncompatibleOperatorFails(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version=?`, versionAlterTitle); err != nil {
		t.Fatal(err)
	}

	err := st.Migrate(ctx)
	if err == nil {
		t.Fatal("несовместимая пропущенная миграция принята молча")
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("миграция %d", versionAlterTitle)) {
		t.Errorf("в отказе нет номера версии: %v", err)
	}
	if !strings.Contains(err.Error(), "duplicate column") {
		t.Errorf("в отказе нет причины SQLite: %v", err)
	}
	if journalHas(t, st, versionAlterTitle) {
		t.Error("версия записана в журнал, хотя оператор не прошёл")
	}
}

// Пустая база проходит весь список: отказ по чужой версии не должен мешать
// штатному первому запуску.
func TestMigrateFreshDatabaseRunsEverything(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	run := st.Migrations()
	if run.Known != MigrationCount() {
		t.Errorf("Known = %d, хочу %d", run.Known, MigrationCount())
	}
	if run.Applied != run.Known {
		t.Errorf("Applied = %d, хочу %d", run.Applied, run.Known)
	}
	if len(run.Executed) != MigrationCount() {
		t.Errorf("выполнено %d миграций, хочу %d", len(run.Executed), MigrationCount())
	}
	if len(run.Repaired) != 0 {
		t.Errorf("на пустой базе нечего восстанавливать: %v", run.Repaired)
	}
	if len(run.Foreign) != 0 {
		t.Errorf("чужих версий нет: %v", run.Foreign)
	}
}

// Повторный прогон ничего не выполняет и не считает обычный догоняющий прогон
// лечением дырок.
func TestMigrateSecondRunExecutesNothing(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("повторный прогон: %v", err)
	}
	run := st.Migrations()
	if len(run.Executed) != 0 {
		t.Errorf("повторный прогон выполнил %v", run.Executed)
	}
	if len(run.Repaired) != 0 {
		t.Errorf("повторный прогон насчитал восстановленных %v", run.Repaired)
	}
	if run.Applied != run.Known {
		t.Errorf("схема %d/%d", run.Applied, run.Known)
	}
}

// Догоняющий прогон после обновления бинаря - штатная ситуация: новые версии
// ниже максимума не являются, потому что максимума ещё нет, и в Repaired не
// попадают.
func TestMigrateCatchUpIsNotRepair(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	// Убираем хвост журнала: так выглядит база, которую мигрировал старый бинарь.
	if _, err := st.db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version > ?`, MigrationCount()-2); err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("догоняющий прогон: %v", err)
	}
	run := st.Migrations()
	if len(run.Executed) != 2 {
		t.Errorf("выполнено %v, хочу две последние миграции", run.Executed)
	}
	if len(run.Repaired) != 0 {
		t.Errorf("догоняющий прогон сочтён лечением: %v", run.Repaired)
	}
	if run.Applied != run.Known {
		t.Errorf("схема %d/%d", run.Applied, run.Known)
	}
}

func TestMigrationCountMatchesList(t *testing.T) {
	if MigrationCount() != len(migrations) {
		t.Errorf("MigrationCount = %d, в списке %d", MigrationCount(), len(migrations))
	}
	if MigrationCount() == 0 {
		t.Error("список миграций пуст")
	}
}
