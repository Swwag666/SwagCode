package store

import (
	"context"
	"testing"

	"voidsearchswag/internal/filex"
)

func TestCountFilesWithoutVerdict(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	if n, err := st.CountFilesWithoutVerdict(ctx); err != nil {
		t.Fatal(err)
	} else if n != 0 {
		t.Errorf("в пустом каталоге %d строк без вердикта", n)
	}

	entries := []FileEntry{
		{TaskID: "t", URL: "http://a.onion/book.epub", Filename: "book.epub", Ext: "epub"},
		{TaskID: "t", URL: "http://a.onion/doc.pdf", Filename: "doc.pdf", Ext: "pdf"},
		{TaskID: "t", URL: "http://a.onion/new.zip", Filename: "new.zip", Ext: "zip", Verdict: "archive"},
	}
	for _, f := range entries {
		if err := st.AddFile(ctx, f); err != nil {
			t.Fatal(err)
		}
	}

	n, err := st.CountFilesWithoutVerdict(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("строк без вердикта %d, ожидала 2", n)
	}
}

func TestBackfillFileVerdictsFillsEmptyColumn(t *testing.T) {
	// Колонка verdict существовала с самого начала, но не заполнялась никогда:
	// оба продакшн-места записи опускали поле. Поэтому строки, собранные до
	// правки, остались бы пустыми навсегда, и фильтр по метке молча не находил
	// бы их.
	st := newStore(t)
	ctx := context.Background()

	entries := []FileEntry{
		{TaskID: "t", URL: "http://a.onion/book.epub", Filename: "book.epub", Ext: "epub", Size: 100},
		{TaskID: "t", URL: "http://a.onion/doc.pdf", Filename: "doc.pdf", Ext: "pdf", Size: 200},
		{TaskID: "t", URL: "http://a.onion/setup.exe", Filename: "setup.exe", Ext: "exe", Size: 300},
		{TaskID: "t", URL: "http://a.onion/pack.zip", Filename: "pack.zip", Ext: "zip", Size: 400},
	}
	for _, f := range entries {
		if err := st.AddFile(ctx, f); err != nil {
			t.Fatal(err)
		}
	}

	filled, err := st.BackfillFileVerdicts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if filled != 4 {
		t.Errorf("заполнено %d строк, ожидала 4", filled)
	}

	files, err := st.SearchFiles(ctx, FileQuery{})
	if err != nil {
		t.Fatal(err)
	}
	byExt := map[string]string{}
	for _, f := range files {
		if f.Verdict == "" {
			t.Errorf("у %s вердикт остался пустым", f.Filename)
		}
		byExt[f.Ext] = f.Verdict
	}

	want := map[string]string{
		"epub": filex.VerdictEbook,
		"pdf":  filex.VerdictDocument,
		"exe":  filex.VerdictExecutable,
		"zip":  filex.VerdictArchive,
	}
	for ext, w := range want {
		if got := byExt[ext]; got != w {
			t.Errorf("у %s вердикт %q, ожидала %q", ext, got, w)
		}
	}

	// Счётчик пустых строк обязан обнулиться.
	if n, err := st.CountFilesWithoutVerdict(ctx); err != nil {
		t.Fatal(err)
	} else if n != 0 {
		t.Errorf("после заполнения осталось %d пустых строк", n)
	}
}

func TestBackfillFileVerdictsIdempotent(t *testing.T) {
	// Заполнение безопасно повторять: второй вызов не находит пустых строк и
	// возвращает ноль, а значения не меняются.
	st := newStore(t)
	ctx := context.Background()

	if err := st.AddFile(ctx, FileEntry{
		TaskID: "t", URL: "http://a.onion/book.epub", Filename: "book.epub", Ext: "epub", Size: 100,
	}); err != nil {
		t.Fatal(err)
	}

	first, err := st.BackfillFileVerdicts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first != 1 {
		t.Errorf("первый вызов заполнил %d, ожидала 1", first)
	}

	second, err := st.BackfillFileVerdicts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if second != 0 {
		t.Errorf("второй вызов заполнил %d, ожидала 0", second)
	}

	files, err := st.SearchFiles(ctx, FileQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Verdict != filex.VerdictEbook {
		t.Errorf("значение изменилось при повторе: %+v", files)
	}
}

func TestBackfillFileVerdictsDoesNotOverwrite(t *testing.T) {
	// Уже заполненная метка обязана остаться: перезапись сломала бы ручные
	// правки и метки, проставленные другим инструментом.
	st := newStore(t)
	ctx := context.Background()

	if err := st.AddFile(ctx, FileEntry{
		TaskID: "t", URL: "http://a.onion/book.epub", Filename: "book.epub", Ext: "epub",
		Size: 100, Verdict: "custom-label",
	}); err != nil {
		t.Fatal(err)
	}

	filled, err := st.BackfillFileVerdicts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if filled != 0 {
		t.Errorf("заполнено %d строк, ожидала 0", filled)
	}

	files, err := st.SearchFiles(ctx, FileQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if files[0].Verdict != "custom-label" {
		t.Errorf("существующая метка перезаписана: %q", files[0].Verdict)
	}
}

func TestBackfillFileVerdictsUsesFilenameWhenExtEmpty(t *testing.T) {
	// Сборщик не всегда заполняет Ext. Без вывода из имени такие строки получили
	// бы «unknown» при вполне распознаваемом файле.
	st := newStore(t)
	ctx := context.Background()

	if err := st.AddFile(ctx, FileEntry{
		TaskID: "t", URL: "http://a.onion/download/5", Filename: "report.pdf",
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := st.BackfillFileVerdicts(ctx); err != nil {
		t.Fatal(err)
	}

	files, err := st.SearchFiles(ctx, FileQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("в базе %d файлов", len(files))
	}
	if files[0].Verdict != filex.VerdictDocument {
		t.Errorf("вердикт = %q, ожидала %q по имени файла", files[0].Verdict, filex.VerdictDocument)
	}
}

func TestBackfillFileVerdictsUnrecognizableGetsUnknown(t *testing.T) {
	// Метка «unknown» тоже записывается: пустая колонка неотличима от
	// незаполненной, а явная метка показывает, что файл разобран, но расширение
	// не распознано.
	st := newStore(t)
	ctx := context.Background()

	if err := st.AddFile(ctx, FileEntry{
		TaskID: "t", URL: "http://a.onion/download/5", Filename: "download",
	}); err != nil {
		t.Fatal(err)
	}

	filled, err := st.BackfillFileVerdicts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if filled != 1 {
		t.Errorf("заполнено %d, ожидала 1", filled)
	}

	files, err := st.SearchFiles(ctx, FileQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if files[0].Verdict != filex.VerdictUnknown {
		t.Errorf("вердикт = %q, ожидала %q", files[0].Verdict, filex.VerdictUnknown)
	}
	if n, err := st.CountFilesWithoutVerdict(ctx); err != nil {
		t.Fatal(err)
	} else if n != 0 {
		t.Errorf("строка с unknown всё ещё считается пустой: %d", n)
	}
}

func TestBackfillFileVerdictsMakesFiltersWork(t *testing.T) {
	// Конечный смысл заполнения: фильтры начинают работать на старых данных.
	st := newStore(t)
	ctx := context.Background()

	entries := []FileEntry{
		{TaskID: "t", URL: "http://a.onion/book.epub", Filename: "book.epub", Ext: "epub", Size: 100},
		{TaskID: "t", URL: "http://a.onion/setup.exe", Filename: "setup.exe", Ext: "exe", Size: 300},
		{TaskID: "t", URL: "http://a.onion/doc.pdf", Filename: "doc.pdf", Ext: "pdf", Size: 200},
	}
	for _, f := range entries {
		if err := st.AddFile(ctx, f); err != nil {
			t.Fatal(err)
		}
	}

	// До заполнения фильтр не находит ничего.
	before, err := st.SearchFiles(ctx, FileQuery{Verdict: "ebook"})
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 0 {
		t.Errorf("до заполнения найдено %d, ожидала 0", len(before))
	}
	riskBefore, err := st.SearchFiles(ctx, FileQuery{RiskOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(riskBefore) != 0 {
		t.Errorf("до заполнения риск-фильтр нашёл %d, ожидала 0", len(riskBefore))
	}

	if _, err := st.BackfillFileVerdicts(ctx); err != nil {
		t.Fatal(err)
	}

	after, err := st.SearchFiles(ctx, FileQuery{Verdict: "ebook"})
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || after[0].Ext != "epub" {
		t.Errorf("после заполнения найдено %d: %+v", len(after), after)
	}

	riskAfter, err := st.SearchFiles(ctx, FileQuery{RiskOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(riskAfter) != 1 || riskAfter[0].Ext != "exe" {
		t.Errorf("риск-фильтр после заполнения дал %d: %+v", len(riskAfter), riskAfter)
	}
}

func TestBackfillFileVerdictsEmptyCatalog(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	filled, err := st.BackfillFileVerdicts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if filled != 0 {
		t.Errorf("в пустом каталоге заполнено %d", filled)
	}
}

func TestBackfillFileVerdictsCancelledContext(t *testing.T) {
	// Отмена контекста обязана вернуть ошибку, а не оставить базу в
	// полузаполненном состоянии без сигнала.
	st := newStore(t)
	ctx := context.Background()

	for i := 0; i < 20; i++ {
		if err := st.AddFile(ctx, FileEntry{
			TaskID: "t", URL: "http://a.onion/f" + string(rune('a'+i)),
			Filename: "f.epub", Ext: "epub", Size: int64(i + 1),
		}); err != nil {
			t.Fatal(err)
		}
	}

	cctx, cancel := context.WithCancel(ctx)
	cancel()

	if _, err := st.BackfillFileVerdicts(cctx); err == nil {
		t.Error("отменённый контекст не вернул ошибку")
	}
}
