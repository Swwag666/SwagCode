package main

import (
	"context"
	"strings"
	"testing"

	"voidsearchswag/internal/filex"
	"voidsearchswag/internal/store"
)

// Предупреждение о риске в каталоге печатается в два приёма: решение «нужно ли
// предупреждать» принимает filex.VerdictIsRisk, а текст объяснения строит
// riskNote. VerdictIsRisk приводит вход к нижнему регистру и обрезает пробелы, и
// её комментарий прямо объясняет зачем: метка может прийти из фильтра CLI или из
// JSON, собранного другим инструментом. riskNote сравнивает вход как есть,
// поэтому та же строка распознаётся как риск, но объяснение уходит в ветку по
// умолчанию. Замер до правки на фикстуре с verdict «EXECUTABLE»:
//
//	внимание: файл требует осторожности
//
// вместо «исполняемый код, запускать только понимая происхождение файла», то есть
// предупреждение теряло смысл ровно для той категории, ради которой существует.
const (
	noteExecutable = "исполняемый код, запускать только понимая происхождение файла"
	noteSecret     = "ключи или база паролей, файл может содержать чужие секреты"
	noteFallback   = "файл требует осторожности"
)

// addVerdictFile кладёт в каталог запись с явно заданной меткой: mustAddFile
// оставляет verdict пустым, и его заполняет разбор расширения, который всегда
// выдаёт нижний регистр.
func addVerdictFile(t *testing.T, st *store.Store, url, name, ext, verdict string) {
	t.Helper()
	f := store.FileEntry{
		TaskID:   "t",
		URL:      url,
		Filename: name,
		Ext:      ext,
		Size:     100,
		Verdict:  verdict,
	}
	if err := st.AddFile(context.Background(), f); err != nil {
		t.Fatalf("add file %s: %v", url, err)
	}
}

// riskNoteFor возвращает эталонный текст объяснения для метки.
func riskNoteFor(verdict string) string {
	switch strings.ToLower(strings.TrimSpace(verdict)) {
	case filex.VerdictExecutable:
		return noteExecutable
	case filex.VerdictSecret:
		return noteSecret
	}
	return noteFallback
}

func TestRiskNoteExplainsEveryRiskVerdict(t *testing.T) {
	for _, verdict := range filex.RiskVerdicts {
		for _, form := range []string{verdict, strings.ToUpper(verdict), "  " + verdict + "\t"} {
			got := riskNote(form)
			if got == noteFallback {
				t.Errorf("riskNote(%q) = %q: рисковая метка получила объяснение по умолчанию", form, got)
			}
			if want := riskNoteFor(verdict); got != want {
				t.Errorf("riskNote(%q) = %q, хочу %q", form, got, want)
			}
		}
	}
}

// Метка вне списка рисков объяснения не получает, и ветка по умолчанию остаётся
// единственной: иначе riskNote начала бы выдумывать смысл для неизвестной метки.
func TestRiskNoteKeepsFallbackForOtherVerdicts(t *testing.T) {
	for _, verdict := range []string{filex.VerdictArchive, filex.VerdictEbook, filex.VerdictUnknown, "", "  "} {
		if got := riskNote(verdict); got != noteFallback {
			t.Errorf("riskNote(%q) = %q, хочу %q", verdict, got, noteFallback)
		}
	}
}

func TestCmdFilesPrintsExactRiskNoteForUppercaseVerdict(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	st := openTestStore(t, dir)
	addVerdictFile(t, st, "http://abc.onion/f/tool.exe", "tool.exe", "exe", "EXECUTABLE")
	closeStore(st)

	out := captureStdout(t, func() { cmdFiles([]string{}) })
	if !strings.Contains(out, "внимание: "+noteExecutable) {
		t.Errorf("объяснение риска потеряно для метки в верхнем регистре:\n%s", out)
	}
	if strings.Contains(out, noteFallback) {
		t.Errorf("напечатано объяснение по умолчанию вместо конкретного:\n%s", out)
	}
	if !strings.Contains(out, "tool.exe") {
		t.Errorf("запись не попала в выдачу:\n%s", out)
	}
}

func TestCmdFilesPrintsExactRiskNoteForPaddedSecret(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	st := openTestStore(t, dir)
	addVerdictFile(t, st, "http://abc.onion/f/keys.kdbx", "keys.kdbx", "kdbx", " secret ")
	closeStore(st)

	out := captureStdout(t, func() { cmdFiles([]string{}) })
	if !strings.Contains(out, "внимание: "+noteSecret) {
		t.Errorf("объяснение риска потеряно для метки с пробелами:\n%s", out)
	}
	if strings.Contains(out, noteFallback) {
		t.Errorf("напечатано объяснение по умолчанию вместо конкретного:\n%s", out)
	}
}

// Контроль: обычный путь, где метка лежит в хранящемся виде, не изменился.
func TestCmdFilesPrintsRiskNoteForLowercaseVerdict(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	st := openTestStore(t, dir)
	addVerdictFile(t, st, "http://abc.onion/f/tool.exe", "tool.exe", "exe", filex.VerdictExecutable)
	addVerdictFile(t, st, "http://abc.onion/f/keys.kdbx", "keys.kdbx", "kdbx", filex.VerdictSecret)
	closeStore(st)

	out := captureStdout(t, func() { cmdFiles([]string{}) })
	if !strings.Contains(out, "внимание: "+noteExecutable) {
		t.Errorf("объяснение для executable потеряно:\n%s", out)
	}
	if !strings.Contains(out, "внимание: "+noteSecret) {
		t.Errorf("объяснение для secret потеряно:\n%s", out)
	}
}

// Вердикт в базе хранится как пришёл, поэтому предупреждение обязано срабатывать
// и при отборе: фильтр -verdict приводит ввод к нижнему регистру, а запись
// нормализуется при сохранении, и две стороны встречаются в одном виде.
func TestCmdFilesRiskNoteSurvivesVerdictFilter(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	st := openTestStore(t, dir)
	addVerdictFile(t, st, "http://abc.onion/f/tool.exe", "tool.exe", "exe", "EXECUTABLE")
	addVerdictFile(t, st, "http://abc.onion/f/book.epub", "book.epub", "epub", "ebook")
	closeStore(st)

	out := captureStdout(t, func() { cmdFiles([]string{"-verdict", "executable"}) })
	if !strings.Contains(out, "tool.exe") {
		t.Errorf("фильтр по метке не нашёл запись в верхнем регистре:\n%s", out)
	}
	if !strings.Contains(out, "внимание: "+noteExecutable) {
		t.Errorf("объяснение риска потеряно при отборе по метке:\n%s", out)
	}
}
