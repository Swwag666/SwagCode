package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// bulkCatalogFiles вносит total строк каталога одной транзакцией и возвращает
// число внесённых.
//
// Одна транзакция вместо total вызовов AddFile нужна потому, что тесты обхода
// работают на каталоге больше внутреннего предела MaxStoreLimit: по одной
// вставке это тысячи отдельных запросов и минуты прогона.
func bulkCatalogFiles(t *testing.T, st *Store, total int) int {
	t.Helper()
	ctx := context.Background()

	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO file_catalog(task_id, url, filename, ext, size, verdict, found_at)
		VALUES ('t', ?, ?, 'epub', 1024, 'ebook', datetime('now'))`)
	if err != nil {
		tx.Rollback()
		t.Fatalf("prepare: %v", err)
	}
	for i := 0; i < total; i++ {
		if _, err := stmt.Exec(
			fmt.Sprintf("http://h%05d.onion/f%05d.epub", i, i),
			fmt.Sprintf("file%05d.epub", i)); err != nil {
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
	return total
}

// scanTotal обходит каталог и возвращает число увиденных записей.
func scanTotal(t *testing.T, st *Store, batch int) int {
	t.Helper()
	n := 0
	if err := st.ScanFileCatalog(context.Background(), batch, func(FileEntry) error {
		n++
		return nil
	}); err != nil {
		t.Fatalf("обход: %v", err)
	}
	return n
}

func TestCountFilesIsExactBeyondListFilesLimit(t *testing.T) {
	// Причина, по которой появился отдельный счётчик: ListFiles обрезан
	// внутренним пределом, поэтому len(ListFiles) не равен числу строк в
	// каталоге, и отчёт, построенный на этой длине, врёт.
	const total = 6000
	st := newStore(t)
	bulkCatalogFiles(t, st, total)

	got, err := st.CountFiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != total {
		t.Errorf("CountFiles = %d, ожидала %d", got, total)
	}

	listed, err := st.ListFiles(context.Background(), "", 10000)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != MaxStoreLimit {
		t.Fatalf("ListFiles вернул %d строк, ожидала предел %d", len(listed), MaxStoreLimit)
	}
	if len(listed) >= got {
		t.Errorf("предел ListFiles (%d) не меньше настоящего числа строк (%d): фикстура не демонстрирует дефект", len(listed), got)
	}
}

func TestScanFileCatalogCoversWholeCatalog(t *testing.T) {
	const total = 6000
	st := newStore(t)
	bulkCatalogFiles(t, st, total)

	ctx := context.Background()
	seen := map[string]bool{}
	var prevID int64
	var n int
	err := st.ScanFileCatalog(ctx, 500, func(f FileEntry) error {
		n++
		if f.ID <= prevID {
			t.Errorf("порядок нарушен: id %d после %d", f.ID, prevID)
		}
		prevID = f.ID
		if seen[f.Filename] {
			t.Errorf("запись %s отдана дважды", f.Filename)
		}
		seen[f.Filename] = true

		// Обход обязан отдавать запись целиком: по нему строят отчёт о
		// кандидатах на удаление, и потерянное поле исказило бы причину.
		if f.URL == "" || f.Ext != "epub" || f.Size != 1024 || f.Verdict != "ebook" || f.FoundAt.IsZero() {
			t.Errorf("поля записи потеряны: %+v", f)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != total {
		t.Errorf("обход увидел %d записей, ожидала %d", n, total)
	}
	if len(seen) != total {
		t.Errorf("уникальных имён %d, ожидала %d", len(seen), total)
	}
}

func TestScanFileCatalogReachesRowsListFilesMisses(t *testing.T) {
	// Смысл метода: добраться до строк, которых ListFiles не возвращает. Тест
	// проверяет не число, а именно состав - каждая запись, пропущенная
	// ListFiles, обязана найтись при обходе.
	const total = 6000
	st := newStore(t)
	bulkCatalogFiles(t, st, total)
	ctx := context.Background()

	listed, err := st.ListFiles(ctx, "", 10000)
	if err != nil {
		t.Fatal(err)
	}
	inList := map[string]bool{}
	for _, f := range listed {
		inList[f.Filename] = true
	}

	var missedByList, seenByScan int
	err = st.ScanFileCatalog(ctx, 500, func(f FileEntry) error {
		seenByScan++
		if !inList[f.Filename] {
			missedByList++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seenByScan != total {
		t.Errorf("обход увидел %d, ожидала %d", seenByScan, total)
	}
	want := total - len(listed)
	if missedByList != want {
		t.Errorf("обход нашёл %d записей вне выборки ListFiles, ожидала %d", missedByList, want)
	}
	if missedByList == 0 {
		t.Error("фикстура не демонстрирует разницу: ListFiles вернул всё")
	}
}

func TestScanFileCatalogStopsOnHandlerError(t *testing.T) {
	st := newStore(t)
	bulkCatalogFiles(t, st, 100)

	sentinel := errors.New("достаточно")
	n := 0
	err := st.ScanFileCatalog(context.Background(), 10, func(FileEntry) error {
		n++
		if n == 10 {
			return sentinel
		}
		return nil
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("ошибка обработчика не возвращена: %v", err)
	}
	if n != 10 {
		t.Errorf("обход продолжился после ошибки: %d вызовов", n)
	}
}

func TestScanFileCatalogEmptyCatalog(t *testing.T) {
	st := newStore(t)
	n := 0
	if err := st.ScanFileCatalog(context.Background(), 500, func(FileEntry) error {
		n++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("пустой каталог дал %d вызовов", n)
	}
	if total, err := st.CountFiles(context.Background()); err != nil || total != 0 {
		t.Errorf("CountFiles на пустом каталоге = %d, err = %v", total, err)
	}
}

func TestScanFileCatalogNilHandler(t *testing.T) {
	// Обработчик обязателен: обход без него - бессмысленная работа, и молчаливый
	// возврат nil скрыл бы ошибку вызывающего.
	st := newStore(t)
	bulkCatalogFiles(t, st, 5)
	if err := st.ScanFileCatalog(context.Background(), 500, nil); err == nil {
		t.Error("обход без обработчика принят")
	}
}

func TestScanFileCatalogBatchNormalization(t *testing.T) {
	const total = 1200
	st := newStore(t)
	bulkCatalogFiles(t, st, total)

	// Ноль означает «возьми размер по умолчанию», а не «не читай ничего».
	if got := scanTotal(t, st, 0); got != total {
		t.Errorf("batch=0: увидела %d, ожидала %d", got, total)
	}
	// Значение выше внутреннего предела режется, но обход остаётся полным.
	if got := scanTotal(t, st, 999999); got != total {
		t.Errorf("batch=999999: увидела %d, ожидала %d", got, total)
	}
	// Мелкая порция обязана дать тот же результат: размер порции не влияет на
	// полноту обхода.
	if got := scanTotal(t, st, 7); got != total {
		t.Errorf("batch=7: увидела %d, ожидала %d", got, total)
	}
	if got := scanTotal(t, st, 1); got != total {
		t.Errorf("batch=1: увидела %d, ожидала %d", got, total)
	}
}

func TestScanFileCatalogCancelledContext(t *testing.T) {
	st := newStore(t)
	bulkCatalogFiles(t, st, 100)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	n := 0
	err := st.ScanFileCatalog(ctx, 10, func(FileEntry) error {
		n++
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("отменённый контекст дал %v, ожидала context.Canceled", err)
	}
	if n != 0 {
		t.Errorf("обход с отменённым контекстом сделал %d вызовов", n)
	}
}

func TestScanPaginationUsesPrimaryKey(t *testing.T) {
	// Закрепляет замер, на котором основан выбор пагинации: порог по id
	// планируется как поиск по первичному ключу, а вариант с OFFSET - как
	// полное сканирование. Если запрос обхода когда-нибудь перепишут на OFFSET,
	// тест укажет на потерю индекса.
	const total = 6000
	st := newStore(t)
	bulkCatalogFiles(t, st, total)

	details := explainDetails(t, st,
		`SELECT id, filename FROM file_catalog WHERE id > 100 ORDER BY id LIMIT 500`)
	joined := strings.Join(details, " | ")
	if !strings.Contains(joined, "SEARCH") || !strings.Contains(joined, "PRIMARY KEY") {
		t.Errorf("запрос обхода не использует первичный ключ: %s", joined)
	}
	if strings.Contains(joined, "SCAN file_catalog") {
		t.Errorf("запрос обхода сканирует таблицу: %s", joined)
	}

	offsetPlan := strings.Join(explainDetails(t, st,
		`SELECT id, filename FROM file_catalog ORDER BY id LIMIT 500 OFFSET 5000`), " | ")
	if !strings.Contains(offsetPlan, "SCAN") {
		t.Errorf("фикстура неверна: вариант с OFFSET не сканирует таблицу: %s", offsetPlan)
	}
}

func TestCountFilesTracksDeletion(t *testing.T) {
	// Счётчик обязан отражать удаление: отчёт clean печатает остаток каталога,
	// и кэш или неверный запрос дали бы число, которое не сходится с фактом.
	//
	// Строки перечислены явно, а не отобраны по LIKE: имена в фикстуре
	// пятизначные (file00000...file00019), и шаблон file000% совпал бы со всеми
	// двадцатью, после чего тест проверял бы ноль вместо остатка.
	st := newStore(t)
	ctx := context.Background()
	bulkCatalogFiles(t, st, 20)

	if got, err := st.CountFiles(ctx); err != nil || got != 20 {
		t.Fatalf("до удаления: %d, err %v", got, err)
	}
	if _, err := st.db.ExecContext(ctx, `DELETE FROM file_catalog WHERE filename IN
		('file00000.epub','file00001.epub','file00002.epub','file00003.epub','file00004.epub')`); err != nil {
		t.Fatal(err)
	}
	got, err := st.CountFiles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != 15 {
		t.Errorf("после удаления пяти строк счётчик = %d, ожидала 15", got)
	}
	if n := scanTotal(t, st, 500); n != 15 {
		t.Errorf("обход после удаления увидел %d записей, ожидала 15", n)
	}
}
