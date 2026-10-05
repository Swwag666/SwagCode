package store

import (
	"context"
	"fmt"
	"testing"
)

// countFiles возвращает число строк в каталоге файлов.
//
// Использует существующий FileStats, а не отдельный COUNT-запрос: статистика
// уже считает total, и второй путь к той же цифре мог бы разойтись с ней.
func countFiles(t *testing.T, st *Store, ctx context.Context) int {
	t.Helper()
	total, _, _, err := st.FileStats(ctx)
	if err != nil {
		t.Fatalf("статистика каталога: %v", err)
	}
	return total
}

// TestCleanFileCatalogSpansMultipleBatches проверяет, что порционный обход
// проходит всю таблицу, а не первую порцию.
//
// Обход идёт по ключу (WHERE id > ? ORDER BY id LIMIT ?), и самая частая ошибка в
// такой схеме - неверное условие остановки: цикл завершается после первой порции
// и молча оставляет хвост таблицы нечищеным. Тест на объёме больше одной порции
// ловит именно это.
func TestCleanFileCatalogSpansMultipleBatches(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	// 1200 строк при размере порции 400 - три полные порции.
	const total = 1200
	for i := 0; i < total; i++ {
		f := FileEntry{
			TaskID:   "t",
			URL:      fmt.Sprintf("http://a.onion/file%d.bin", i),
			Filename: fmt.Sprintf("file%d.bin", i),
			Ext:      "bin",
			Size:     int64(i + 1),
		}
		// Каждая третья строка - мусор по критерию служебного расширения.
		if i%3 == 0 {
			f.URL = fmt.Sprintf("http://a.onion/sig%d.asc", i)
			f.Filename = fmt.Sprintf("sig%d.asc", i)
			f.Ext = "asc"
		}
		if err := st.AddFile(ctx, f); err != nil {
			t.Fatal(err)
		}
	}

	before := countFiles(t, st, ctx)
	if before != total {
		t.Fatalf("в каталоге %d строк, ожидала %d", before, total)
	}

	removed, err := st.CleanFileCatalog(ctx)
	if err != nil {
		t.Fatal(err)
	}

	want := int64(400) // каждая третья из 1200
	if removed != want {
		t.Errorf("удалено %d, ожидала %d: порционный обход не прошёл всю таблицу", removed, want)
	}

	after := countFiles(t, st, ctx)
	if after != total-int(want) {
		t.Errorf("осталось %d строк, ожидала %d", after, total-int(want))
	}

	// Мусорных расширений не осталось ни в одной порции.
	junk, err := st.SearchFiles(ctx, FileQuery{Ext: "asc", Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if len(junk) != 0 {
		t.Errorf("в каталоге осталось %d служебных файлов", len(junk))
	}
}

func TestCleanFileCatalogMultipleBatchesAllJunk(t *testing.T) {
	// Крайний случай: мусор в каждой строке. Курсор обязан двигаться по
	// последней просмотренной строке, а не по последней удалённой, иначе при
	// полном удалении порции обход встал бы на месте.
	st := newStore(t)
	ctx := context.Background()

	const total = 900
	for i := 0; i < total; i++ {
		if err := st.AddFile(ctx, FileEntry{
			TaskID:   "t",
			URL:      fmt.Sprintf("http://a.onion/sig%d.asc", i),
			Filename: fmt.Sprintf("sig%d.asc", i),
			Ext:      "asc",
			Size:     10,
		}); err != nil {
			t.Fatal(err)
		}
	}

	removed, err := st.CleanFileCatalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if removed != total {
		t.Errorf("удалено %d, ожидала %d", removed, total)
	}

	after := countFiles(t, st, ctx)
	if after != 0 {
		t.Errorf("осталось %d строк, ожидала 0", after)
	}
}

func TestCleanFileCatalogSecondPassBatched(t *testing.T) {
	// Вторая отбраковка - строки без имени и адреса, заканчивающиеся слэшем.
	// Она тоже обязана идти порциями: прежний безусловный DELETE по всей таблице
	// держал пишущую транзакцию на весь каталог.
	st := newStore(t)
	ctx := context.Background()

	const total = 1000
	for i := 0; i < total; i++ {
		f := FileEntry{
			TaskID: "t",
			URL:    fmt.Sprintf("http://a.onion/dir%d/", i),
			Ext:    "",
			Size:   5,
		}
		// Половина строк без имени, половина - адреса со слэшем на конце;
		// обе группы попадают под вторую отбраковку.
		if i%2 == 0 {
			f.Filename = ""
		} else {
			f.Filename = fmt.Sprintf("keep%d.bin", i)
			f.Ext = "bin"
			f.URL = fmt.Sprintf("http://a.onion/keep%d.bin", i)
		}
		if err := st.AddFile(ctx, f); err != nil {
			t.Fatal(err)
		}
	}

	removed, err := st.CleanFileCatalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 500 {
		t.Errorf("удалено %d, ожидала 500", removed)
	}

	after := countFiles(t, st, ctx)
	if after != 500 {
		t.Errorf("осталось %d строк, ожидала 500", after)
	}
}

func TestCleanFileCatalogEmptyTable(t *testing.T) {
	// Пустая таблица не должна ни паниковать, ни крутить цикл бесконечно.
	st := newStore(t)
	ctx := context.Background()

	removed, err := st.CleanFileCatalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Errorf("в пустом каталоге удалено %d", removed)
	}
}

// Идемпотентность чистки каталога уже покрыта TestCleanFileCatalogIdempotent в
// cleanmetadata_test.go: дублировать проверку не нужно.

func TestCleanFileCatalogCancelledContext(t *testing.T) {
	// Отмена контекста обязана прерывать обход между порциями, а не докручивать
	// его до конца: иначе Ctrl+C во время чистки большого каталога не сработал бы.
	st := newStore(t)
	ctx := context.Background()

	for i := 0; i < 100; i++ {
		if err := st.AddFile(ctx, FileEntry{
			TaskID:   "t",
			URL:      fmt.Sprintf("http://a.onion/sig%d.asc", i),
			Filename: fmt.Sprintf("sig%d.asc", i),
			Ext:      "asc",
			Size:     10,
		}); err != nil {
			t.Fatal(err)
		}
	}

	cctx, cancel := context.WithCancel(ctx)
	cancel()

	if _, err := st.CleanFileCatalog(cctx); err == nil {
		t.Error("отменённый контекст не вернул ошибку")
	}
}

func TestCleanOnionTitlesSpansMultipleBatches(t *testing.T) {
	// Те же риски, что у чистки каталога: цикл по порциям обязан пройти весь пул
	// и остановиться. Пул в одиннадцать тысяч адресов - штатный объём, а размер
	// порции 400, поэтому многoпорционный проход - основной сценарий, а не
	// крайний случай.
	st := newStore(t)
	ctx := context.Background()

	const total = 1000
	for i := 0; i < total; i++ {
		url := fmt.Sprintf("222222222222222222222222222222222222222222222222%04d.onion", i)
		// Половина заголовков - мусор, который чистка обязана убрать.
		title := fmt.Sprintf("Нормальный заголовок %d", i)
		if i%2 == 0 {
			title = url
		}
		if err := st.UpsertOnion(ctx, Onion{URL: url, Status: "live", Title: title}); err != nil {
			t.Fatal(err)
		}
	}

	removed, err := st.CleanOnionTitles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 500 {
		t.Errorf("очищено %d заголовков, ожидала 500", removed)
	}

	// Мусорных заголовков не осталось ни в одной порции.
	pool, err := st.ListOnions(ctx, "", 2000)
	if err != nil {
		t.Fatal(err)
	}
	if len(pool) != total {
		t.Fatalf("в пуле %d адресов, ожидала %d", len(pool), total)
	}
	cleared := 0
	for _, o := range pool {
		if o.Title == o.URL {
			t.Errorf("заголовок %q не очищен: он совпадает с адресом", o.URL)
		}
		if o.Title == "" {
			cleared++
		}
	}
	if cleared != 500 {
		t.Errorf("пустых заголовков %d, ожидала 500", cleared)
	}
}

func TestCleanOnionTitlesLatencyPrefixMultipleBatches(t *testing.T) {
	// Первый проход снимает префикс латентности вида «123ms Название». Проверяю
	// на объёме больше порции, чтобы убедиться, что обновление не остановилось на
	// первых четырёхстах строках.
	st := newStore(t)
	ctx := context.Background()

	const total = 900
	for i := 0; i < total; i++ {
		url := fmt.Sprintf("222222222222222222222222222222222222222222222222%04d.onion", i)
		title := fmt.Sprintf("450ms Сервис номер %d", i)
		if err := st.UpsertOnion(ctx, Onion{URL: url, Status: "live", Title: title}); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := st.CleanOnionTitles(ctx); err != nil {
		t.Fatal(err)
	}

	pool, err := st.ListOnions(ctx, "", 2000)
	if err != nil {
		t.Fatal(err)
	}
	if len(pool) != total {
		t.Fatalf("в пуле %d адресов", len(pool))
	}
	for i, o := range pool {
		if len(o.Title) > 4 && o.Title[:4] == "450m" {
			t.Errorf("строка %d: префикс латентности не снят: %q", i, o.Title)
		}
	}
}

func TestCleanOnionTitlesIdempotent(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	const url = "http://2222222222222222222222222222222222222222222222222222.onion"
	if err := st.UpsertOnion(ctx, Onion{URL: url, Status: "live", Title: url}); err != nil {
		t.Fatal(err)
	}

	first, err := st.CleanOnionTitles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first != 1 {
		t.Errorf("первая чистка изменила %d, ожидала 1", first)
	}

	second, err := st.CleanOnionTitles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if second != 0 {
		t.Errorf("вторая чистка изменила %d, ожидала 0", second)
	}
}

func TestCleanOnionTitlesEmptyPool(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	removed, err := st.CleanOnionTitles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Errorf("в пустом пуле очищено %d", removed)
	}
}

func TestCleanOnionTitlesCancelledContext(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	for i := 0; i < 50; i++ {
		url := fmt.Sprintf("222222222222222222222222222222222222222222222222%04d.onion", i)
		if err := st.UpsertOnion(ctx, Onion{URL: url, Status: "live", Title: url}); err != nil {
			t.Fatal(err)
		}
	}

	cctx, cancel := context.WithCancel(ctx)
	cancel()

	if _, err := st.CleanOnionTitles(cctx); err == nil {
		t.Error("отменённый контекст не вернул ошибку")
	}
}

func TestCleanOnionTitlesPreservesGoodTitles(t *testing.T) {
	// Порционность не должна менять критерии: нормальные заголовки остаются,
	// иначе чистка молча уничтожила бы полезные данные.
	st := newStore(t)
	ctx := context.Background()

	good := []string{
		"Z-Library",
		"Imperial Library of Trantor",
		"Тёмная библиотека",
		"Ahmia Search",
	}
	for i, title := range good {
		url := fmt.Sprintf("222222222222222222222222222222222222222222222222%04d.onion", i)
		if err := st.UpsertOnion(ctx, Onion{URL: url, Status: "live", Title: title}); err != nil {
			t.Fatal(err)
		}
	}

	removed, err := st.CleanOnionTitles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Errorf("нормальные заголовки очищены: %d", removed)
	}

	pool, err := st.ListOnions(ctx, "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(pool) != len(good) {
		t.Fatalf("в пуле %d адресов", len(pool))
	}
	seen := map[string]bool{}
	for _, o := range pool {
		if o.Title == "" {
			t.Errorf("заголовок %q очищен", o.URL)
		}
		seen[o.Title] = true
	}
	for _, title := range good {
		if !seen[title] {
			t.Errorf("заголовок %q потерян", title)
		}
	}
}
