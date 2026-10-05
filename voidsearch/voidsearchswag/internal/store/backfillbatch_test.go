package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// Тесты порционного заполнения колонки verdict.
//
// Проверяются три свойства, которых не было у прежней версии: порция
// ограничена в самом запросе, а не в памяти после выгрузки всей таблицы;
// каждая порция фиксируется своей транзакцией, поэтому прерывание не
// откатывает уже сделанное; запросы идут по индексу, а не полным сканом
// таблицы, когда пустых меток мало.

// bulkFilesWithSparseEmptyVerdicts вставляет total строк каталога одной
// транзакцией, оставляя пустую метку у каждой everyN-й строки, и обновляет
// статистику планировщика.
//
// Одна транзакция вместо total вызовов AddFile нужна потому, что тест плана
// требует десятков тысяч строк: на маленькой таблице SQLite не имеет
// статистики и выбирает план произвольно, такой замер ничего не доказывает.
// Возвращает число строк с пустой меткой.
func bulkFilesWithSparseEmptyVerdicts(t *testing.T, st *Store, total, everyN int) int {
	t.Helper()
	ctx := context.Background()

	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO file_catalog(task_id, url, filename, ext, size, verdict, found_at)
		VALUES ('t', ?, ?, 'epub', 1024, ?, datetime('now'))`)
	if err != nil {
		tx.Rollback()
		t.Fatalf("prepare: %v", err)
	}
	empty := 0
	for i := 0; i < total; i++ {
		v := "ebook"
		if i%everyN == 0 {
			v = ""
			empty++
		}
		if _, err := stmt.Exec(
			fmt.Sprintf("http://h%05d.onion/f%05d.epub", i, i),
			fmt.Sprintf("file%05d.epub", i), v); err != nil {
			stmt.Close()
			tx.Rollback()
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	stmt.Close()
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if _, err := st.db.ExecContext(ctx, `ANALYZE`); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	return empty
}

func TestBackfillFileVerdictsFillsMoreThanOneBatch(t *testing.T) {
	// 950 строк - это три порции по 400. Проверяется, что цикл доходит до
	// конца и завершается, а не заполняет первую порцию и выходит.
	st := newStore(t)
	ctx := context.Background()

	for i := 0; i < 950; i++ {
		if err := st.AddFile(ctx, FileEntry{
			TaskID: "t", URL: fmt.Sprintf("http://a.onion/f%04d.epub", i),
			Filename: fmt.Sprintf("f%04d.epub", i), Ext: "epub", Size: int64(i + 1),
		}); err != nil {
			t.Fatal(err)
		}
	}

	filled, err := st.BackfillFileVerdicts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if filled != 950 {
		t.Errorf("заполнено %d строк, ожидала 950", filled)
	}

	left, err := st.CountFilesWithoutVerdict(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("после заполнения осталось %d пустых строк", left)
	}
}

func TestBackfillOneBatchBoundedAndCommitted(t *testing.T) {
	// Одна порция обязана выбрать ровно limit строк и зафиксировать их: если бы
	// фиксация не состоялась, счётчик пустых вернулся бы к исходному значению.
	st := newStore(t)
	ctx := context.Background()

	for i := 0; i < 1000; i++ {
		if err := st.AddFile(ctx, FileEntry{
			TaskID: "t", URL: fmt.Sprintf("http://b.onion/f%04d.pdf", i),
			Filename: fmt.Sprintf("f%04d.pdf", i), Ext: "pdf", Size: int64(i + 1),
		}); err != nil {
			t.Fatal(err)
		}
	}

	const limit = 400
	selected, updated, err := st.backfillOneBatch(ctx, limit)
	if err != nil {
		t.Fatal(err)
	}
	if selected != limit {
		t.Errorf("выбрано %d строк, ожидала %d", selected, limit)
	}
	if updated != limit {
		t.Errorf("обновлено %d строк, ожидала %d", updated, limit)
	}

	left, err := st.CountFilesWithoutVerdict(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if left != 600 {
		t.Errorf("осталось %d пустых строк, ожидала 600: порция не зафиксирована", left)
	}
}

func TestBackfillResumesAfterPartialRun(t *testing.T) {
	// Смысл порционной фиксации: прерванное заполнение возобновляется, а не
	// начинается заново. Прежняя версия при откате теряла всё.
	st := newStore(t)
	ctx := context.Background()

	for i := 0; i < 950; i++ {
		if err := st.AddFile(ctx, FileEntry{
			TaskID: "t", URL: fmt.Sprintf("http://c.onion/f%04d.epub", i),
			Filename: fmt.Sprintf("f%04d.epub", i), Ext: "epub", Size: int64(i + 1),
		}); err != nil {
			t.Fatal(err)
		}
	}

	if _, _, err := st.backfillOneBatch(ctx, backfillBatch); err != nil {
		t.Fatal(err)
	}

	rest, err := st.BackfillFileVerdicts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rest != 550 {
		t.Errorf("второй вызов заполнил %d строк, ожидала 550 (остаток после порции в 400)", rest)
	}

	left, err := st.CountFilesWithoutVerdict(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("после возобновления осталось %d пустых строк", left)
	}
}

func TestBackfillCancelledContextKeepsCommittedBatches(t *testing.T) {
	// Отмена контекста возвращает ошибку, но уже зафиксированная порция
	// остаётся в базе. Проверка идёт на свежем контексте: на отменённом
	// любой запрос вернул бы ошибку и тест ничего бы не доказал.
	st := newStore(t)
	ctx := context.Background()

	for i := 0; i < 950; i++ {
		if err := st.AddFile(ctx, FileEntry{
			TaskID: "t", URL: fmt.Sprintf("http://d.onion/f%04d.epub", i),
			Filename: fmt.Sprintf("f%04d.epub", i), Ext: "epub", Size: int64(i + 1),
		}); err != nil {
			t.Fatal(err)
		}
	}

	if _, _, err := st.backfillOneBatch(ctx, backfillBatch); err != nil {
		t.Fatal(err)
	}

	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := st.BackfillFileVerdicts(cctx); err == nil {
		t.Error("отменённый контекст не вернул ошибку")
	}

	left, err := st.CountFilesWithoutVerdict(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if left != 550 {
		t.Errorf("после отмены осталось %d пустых строк, ожидала 550: зафиксированная порция откатилась", left)
	}
}

// explainDetails возвращает строки плана запроса.
func explainDetails(t *testing.T, st *Store, query string) []string {
	t.Helper()
	rows, err := st.db.QueryContext(context.Background(), `EXPLAIN QUERY PLAN `+query)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		out = append(out, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

func TestVerdictQueriesUseIndexWhenFewRowsEmpty(t *testing.T) {
	// Штатное состояние после первого заполнения: пустых меток мало. Именно в
	// этом случае прежний предикат с IS NULL стоил дороже всего - он не
	// позволял планировщику использовать индекс, и каждый запрос обходил всю
	// таблицу.
	//
	// Замер на копии живой базы (20000 строк, 200 пустых, после ANALYZE):
	//   новый предикат  SEARCH file_catalog USING INDEX idx_file_verdict_size_found
	//   прежний         SCAN file_catalog
	//
	// Тест намеренно зависит от статистики планировщика, поэтому таблица
	// заполняется целиком и вызывается ANALYZE: на пустой таблице SQLite
	// выбирает план произвольно, и утверждение было бы ложным.
	st := newStore(t)
	empty := bulkFilesWithSparseEmptyVerdicts(t, st, 20000, 100)
	if empty != 200 {
		t.Fatalf("фикстура неверна: пустых меток %d вместо 200", empty)
	}

	queries := []struct{ name, sql string }{
		{
			"выборка порции заполнения",
			`SELECT id, ext, filename FROM file_catalog WHERE verdict = '' LIMIT 400`,
		},
		{
			"счётчик незаполненных строк",
			`SELECT COUNT(*) FROM file_catalog WHERE verdict = ''`,
		},
	}
	for _, q := range queries {
		details := explainDetails(t, st, q.sql)
		joined := strings.Join(details, "; ")
		if strings.Contains(joined, "SCAN file_catalog") {
			t.Errorf("%s: план обходит всю таблицу: %s", q.name, joined)
		}
		if !strings.Contains(joined, "idx_file_verdict") {
			t.Errorf("%s: план не использует индекс по verdict: %s", q.name, joined)
		}
	}

	// Прежний предикат проверяется здесь же, чтобы утверждение в комментарии к
	// CountFilesWithoutVerdict не устарело молча: если SQLite научится
	// использовать индекс для формы с IS NULL, тест скажет об этом, и
	// комментарий нужно будет исправить.
	old := strings.Join(explainDetails(t, st,
		`SELECT id, ext, filename FROM file_catalog WHERE verdict IS NULL OR verdict = '' LIMIT 400`), "; ")
	if !strings.Contains(old, "SCAN file_catalog") {
		t.Errorf("предикат с IS NULL больше не приводит к полному скану, комментарий устарел: %s", old)
	}
}

func TestBackfillBatchQueryBoundedWhenAllRowsEmpty(t *testing.T) {
	// Момент первого заполнения: пусты все строки. Замер показал, что в этом
	// случае планировщик выбирает SCAN file_catalog для обоих предикатов -
	// индекс не имеет селективности. Ограничение порции всё равно делает запрос
	// дешёвым, потому что LIMIT останавливает обход на 400 строках, тогда как
	// прежний запрос без LIMIT выгружал всю таблицу в память.
	st := newStore(t)
	ctx := context.Background()

	const total = 2000
	for i := 0; i < total; i++ {
		if err := st.AddFile(ctx, FileEntry{
			TaskID: "t", URL: fmt.Sprintf("http://e.onion/f%04d.epub", i),
			Filename: fmt.Sprintf("f%04d.epub", i), Ext: "epub", Size: int64(i + 1),
		}); err != nil {
			t.Fatal(err)
		}
	}

	selected, updated, err := st.backfillOneBatch(ctx, backfillBatch)
	if err != nil {
		t.Fatal(err)
	}
	if selected != backfillBatch || updated != backfillBatch {
		t.Errorf("порция при полностью пустой колонке: выбрано %d, обновлено %d, ожидала по %d",
			selected, updated, backfillBatch)
	}
	left, err := st.CountFilesWithoutVerdict(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if left != total-backfillBatch {
		t.Errorf("осталось %d пустых строк, ожидала %d", left, total-backfillBatch)
	}
}
