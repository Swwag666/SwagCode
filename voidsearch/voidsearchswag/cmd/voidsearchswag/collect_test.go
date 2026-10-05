package main

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"voidsearchswag/internal/catalog"
)

// dropCatalogTable удаляет таблицу каталога отдельным соединением: публичный API
// store не даёт сломать схему, а проверить отчёт на нечитаемой базе нужно.
func dropCatalogTable(t *testing.T, dir string) {
	t.Helper()
	db, err := sql.Open("sqlite", dir+"/voidsearchswag.db")
	if err != nil {
		t.Fatalf("открытие базы: %v", err)
	}
	defer db.Close()
	if _, err := db.ExecContext(context.Background(), `DROP TABLE file_catalog`); err != nil {
		t.Fatalf("удаление таблицы: %v", err)
	}
}

func TestPrintCollectReportShowsFailedHosts(t *testing.T) {
	// До правки число отказавших хостов существовало только в JSON. Текстовый
	// отчёт показывал «сбор: хостов 2, страниц 0, ссылок 0, файлов 0», и полный
	// отказ обоих адресов читался как обход живых хостов без файлов.
	dir := t.TempDir()
	st := openTestStore(t, dir)
	defer closeStore(st)
	mustAddFile(t, st, "http://a.onion/book.epub", "book.epub", "epub", 2048, "t", "")

	var problems []string
	out := captureStdout(t, func() {
		problems = printCollectReport(context.Background(), st, catalog.Report{
			Hosts: 2, Failed: 2, Elapsed: "5ms",
		}, "collect-test")
	})
	if len(problems) != 0 {
		t.Errorf("на здоровой базе есть проблемы: %v", problems)
	}
	if !strings.Contains(out, "отказавших хостов: 2 из 2") {
		t.Errorf("отказы не показаны: %s", out)
	}
	if !strings.Contains(out, "задача: collect-test") {
		t.Errorf("идентификатор задачи не напечатан: %s", out)
	}
	if !strings.Contains(out, "каталог всего: 1 файлов") {
		t.Errorf("итог каталога не напечатан: %s", out)
	}
}

func TestPrintCollectReportShowsUnsavedFiles(t *testing.T) {
	dir := t.TempDir()
	st := openTestStore(t, dir)
	defer closeStore(st)

	out := captureStdout(t, func() {
		printCollectReport(context.Background(), st, catalog.Report{
			Hosts: 1, Links: 4, Saved: 1, Skipped: 3, Elapsed: "1s",
		}, "collect-test")
	})
	if !strings.Contains(out, "не записано в каталог: 3") {
		t.Errorf("незаписанные файлы не показаны: %s", out)
	}
	// Нулевые счётчики не должны появляться: строка «отказавших хостов: 0»
	// только засоряла бы отчёт.
	if strings.Contains(out, "отказавших хостов") {
		t.Errorf("при нуле отказов напечатана строка об отказах: %s", out)
	}
}

func TestPrintCollectReportHealthyRun(t *testing.T) {
	dir := t.TempDir()
	st := openTestStore(t, dir)
	defer closeStore(st)

	out := captureStdout(t, func() {
		printCollectReport(context.Background(), st, catalog.Report{
			Hosts:      3,
			Pages:      5,
			Links:      2,
			Saved:      2,
			ByExt:      map[string]int{"epub": 1, "pdf": 1},
			SampleURLs: []string{"http://a.onion/1.epub", "http://a.onion/2.pdf"},
			Elapsed:    "2s",
		}, "collect-1")
	})
	if !strings.Contains(out, "сбор: хостов 3, страниц 5, ссылок 2, файлов 2 за 2s") {
		t.Errorf("основная строка отчёта изменилась: %s", out)
	}
	if !strings.Contains(out, "по расширениям: epub=1 pdf=1") {
		t.Errorf("разбивка по расширениям неверна: %s", out)
	}
	if !strings.Contains(out, "примеры: http://a.onion/1.epub http://a.onion/2.pdf") {
		t.Errorf("примеры адресов не напечатаны: %s", out)
	}
	if strings.Contains(out, "не записано") || strings.Contains(out, "отказавших") {
		t.Errorf("здоровый прогон показал предупреждения: %s", out)
	}
}

func TestPrintCollectReportClipsSamples(t *testing.T) {
	dir := t.TempDir()
	st := openTestStore(t, dir)
	defer closeStore(st)

	samples := make([]string, 0, 20)
	for i := 0; i < 20; i++ {
		samples = append(samples, "http://a.onion/f.epub")
	}
	out := captureStdout(t, func() {
		printCollectReport(context.Background(), st, catalog.Report{
			Hosts: 1, Saved: 20, SampleURLs: samples, Elapsed: "1s",
		}, "collect-test")
	})
	line := ""
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "примеры:") {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("строка примеров не напечатана: %s", out)
	}
	if got := len(strings.Fields(line)) - 1; got != 3 {
		t.Errorf("напечатано %d примеров, ожидала 3: %q", got, line)
	}
}

func TestPrintCollectReportShowsCeiling(t *testing.T) {
	dir := t.TempDir()
	st := openTestStore(t, dir)
	defer closeStore(st)

	out := captureStdout(t, func() {
		printCollectReport(context.Background(), st, catalog.Report{
			Hosts: 1, Saved: 500, LimitHit: "достигнут потолок файлов", Elapsed: "1s",
		}, "collect-test")
	})
	if !strings.Contains(out, "потолок: достигнут потолок файлов") {
		t.Errorf("потолок не показан: %s", out)
	}
}

func TestPrintCollectReportWarnsOnUnreadableCatalog(t *testing.T) {
	// До правки итоговая строка просто исчезала: отчёт обрывался на
	// идентификаторе задачи, и пользователь не знал, что каталог не удалось
	// прочитать. Молчание здесь хуже предупреждения, потому что именно итог
	// отвечает на вопрос «сколько всего собрано».
	dir := t.TempDir()
	st := openTestStore(t, dir)
	mustAddFile(t, st, "http://a.onion/book.epub", "book.epub", "epub", 2048, "t", "")
	closeStore(st)
	dropCatalogTable(t, dir)

	st = openTestStore(t, dir)
	defer closeStore(st)

	var problems []string
	out := captureStdout(t, func() {
		problems = printCollectReport(context.Background(), st, catalog.Report{
			Hosts: 1, Saved: 1, Elapsed: "1s",
		}, "collect-test")
	})
	if len(problems) != 1 {
		t.Fatalf("проблем %d, ожидала 1: %v", len(problems), problems)
	}
	if !strings.Contains(problems[0], "итог каталога") {
		t.Errorf("проблема не названа: %v", problems)
	}
	if !strings.Contains(problems[0], "no such table") {
		t.Errorf("причина не приведена: %v", problems)
	}
	if strings.Contains(out, "каталог всего") {
		t.Errorf("строка итога напечатана при непрочитанном каталоге: %s", out)
	}
	// Остальной отчёт обязан остаться: потеря итога не повод выбрасывать
	// сведения о самом сборе.
	if !strings.Contains(out, "сбор: хостов 1") || !strings.Contains(out, "задача: collect-test") {
		t.Errorf("отчёт о сборе потерялся: %s", out)
	}
}
