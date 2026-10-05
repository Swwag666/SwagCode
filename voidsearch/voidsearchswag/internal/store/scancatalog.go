package store

import (
	"context"
	"database/sql"
	"fmt"
)

// scanBatch - размер порции обхода каталога по умолчанию.
//
// Выбран по тому же принципу, что backfillBatch: порция должна быть достаточно
// большой, чтобы число запросов не влияло на время обхода, и достаточно малой,
// чтобы не держать в памяти весь каталог. При 500 строках обход таблицы на
// 100 000 записей делает 200 запросов и держит в памяти одну порцию.
const scanBatch = 500

// CountFiles возвращает точное число строк в каталоге файлов.
//
// Отдельный метод нужен потому, что ListFiles ограничен MaxStoreLimit и не
// может служить источником числа: при каталоге больше 5000 строк len(ListFiles)
// возвращает 5000, и любой отчёт, построенный на этой длине, врёт. FileStats
// тоже даёт точное число, но вместе с ним считает сумму размеров и разбивку по
// расширениям, то есть делает лишнюю работу там, где нужен только счётчик.
func (s *Store) CountFiles(ctx context.Context) (int, error) {
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM file_catalog`).Scan(&total); err != nil {
		return 0, fmt.Errorf("count files: %w", err)
	}
	return total, nil
}

// ScanFileCatalog перебирает весь каталог файлов порциями и передаёт каждую
// запись в fn. Обход прерывается, если fn вернёт ошибку; она возвращается
// вызывающему как есть.
//
// Метод добавлен из-за предела в ListFiles: тот возвращает не больше
// MaxStoreLimit (5000) строк и сортирует их по found_at DESC, поэтому любой
// вызов, которому нужен весь каталог, получал только 5000 самых свежих
// записей. Так предпросмотр команды clean показывал «файлов к удалению 4500 из
// 5000» на каталоге в 6000 строк: знаменатель был обрезан, а тысяча мусорных
// записей не попадала в список вовсе, потому что они были старее остальных.
//
// Пагинация идёт по id, а не через OFFSET, и выбор измерен: на таблице в 50 000
// строк после ANALYZE запрос с порогом id планируется как SEARCH file_catalog
// USING INTEGER PRIMARY KEY (rowid>?), а вариант с OFFSET - как SCAN
// file_catalog. Полный обход порциями по 500 строк занял 100 мс против 653 мс,
// то есть OFFSET оказался медленнее в 6,5 раза, и разрыв растёт с размером
// каталога, потому что каждая порция перечитывает пропущенные строки. Оба
// способа прочитали все 50 000 строк, поэтому речь только о стоимости.
// Абсолютные миллисекунды зависят от нагрузки на машину, значение имеет
// соотношение; план запроса закрепляет тест, чтобы обоснование не устарело
// молча.
//
// Записи отдаются в порядке возрастания id, а не по свежести: для обхода всего
// каталога порядок не содержателен, а устойчивый порядок позволяет повторить
// обход и получить те же строки.
//
// Метод не подходит для записи во время обхода: при SetMaxOpenConns(1) курсор
// держит единственное соединение, поэтому вставка или удаление из обработчика
// будут ждать его освобождения вечно. CleanFileCatalog потому и ведёт
// собственную keyset-пагинацию, что удаляет порцию сразу после её закрытия.
// Обход годится для чтения: подсчёта, отбора кандидатов, построения отчёта.
func (s *Store) ScanFileCatalog(ctx context.Context, batch int, fn func(FileEntry) error) error {
	if batch <= 0 {
		batch = scanBatch
	}
	if batch > MaxStoreLimit {
		batch = MaxStoreLimit
	}
	if fn == nil {
		return fmt.Errorf("scan file catalog: не задан обработчик записей")
	}

	var lastID int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		rows, err := s.db.QueryContext(ctx, `
			SELECT id, task_id, url, filename, ext, size, mime, source_page, verdict, found_at
			FROM file_catalog WHERE id > ? ORDER BY id LIMIT ?`, lastID, batch)
		if err != nil {
			return fmt.Errorf("scan file catalog: %w", err)
		}

		var seen int
		var scanErr error
		for rows.Next() {
			var f FileEntry
			var found sql.NullString
			if err := rows.Scan(&f.ID, &f.TaskID, &f.URL, &f.Filename, &f.Ext,
				&f.Size, &f.MIME, &f.SourcePage, &f.Verdict, &found); err != nil {
				scanErr = err
				break
			}
			f.FoundAt = parseTS(found)
			seen++
			lastID = f.ID
			if err := fn(f); err != nil {
				scanErr = err
				break
			}
		}
		if scanErr == nil {
			scanErr = rows.Err()
		}
		rows.Close()
		if scanErr != nil {
			return scanErr
		}
		if seen == 0 {
			return nil
		}
	}
}
