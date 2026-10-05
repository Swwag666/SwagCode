package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"voidsearchswag/internal/store"
)

// cleanReport - типизированный разбор JSON-отчёта команды clean.
//
// Типизированная структура вместо map[string]any нужна потому, что числа из
// JSON приходят float64, и сравнение их с целыми через приведение легко
// теряет точность на больших значениях: каталог в этом тесте больше 5000
// строк, а float64 точно представляет целые до 2^53, поэтому здесь
// расхождение невозможно, но читать его приходится в одном месте.
type cleanReport struct {
	Applied        bool                `json:"applied"`
	FilesRemoved   int64               `json:"files_removed"`
	TitlesRemoved  int64               `json:"titles_removed"`
	VerdictFilled  int                 `json:"verdict_filled"`
	FilesBefore    int64               `json:"files_before"`
	FilesAfter     int64               `json:"files_after"`
	Candidates     []map[string]string `json:"candidates"`
	candidateTotal int64
}

func parseCleanJSON(t *testing.T, out string) cleanReport {
	t.Helper()
	var rep cleanReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("JSON-отчёт не разобран: %v\nвывод: %s", err, out)
	}
	rep.candidateTotal = int64(len(rep.Candidates))
	return rep
}

// numberAfterField возвращает число, стоящее в выводе сразу за словом field.
func numberAfterField(t *testing.T, out, field string) int64 {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		tok := strings.FieldsFunc(line, func(r rune) bool {
			return r == ' ' || r == ',' || r == ':'
		})
		for i, w := range tok {
			if w == field && i+1 < len(tok) {
				n, err := strconv.ParseInt(tok[i+1], 10, 64)
				if err != nil {
					t.Fatalf("после %q стоит не число: %q (строка %q)", field, tok[i+1], line)
				}
				return n
			}
		}
	}
	t.Fatalf("в выводе нет поля %q: %s", field, out)
	return 0
}

func TestCmdCleanDryRunKeepsCatalog(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	st := openTestStore(t, dir)
	// Три мусорных записи трёх разных классов: оформление сайта, инфраструктура
	// по расширению и страница вместо файла. Плюс две записи содержимого, одна
	// из них изображение - оно обязано остаться после этапа 61.
	mustAddFile(t, st, "http://a.onion/logo.png", "logo.png", "png", 10, "t", "")
	mustAddFile(t, st, "http://a.onion/style.css", "style.css", "css", 10, "t", "")
	mustAddFile(t, st, "http://a.onion/dir/", "dir", "", 0, "t", "")
	mustAddFile(t, st, "http://a.onion/book.epub", "book.epub", "epub", 10, "t", "")
	mustAddFile(t, st, "http://a.onion/photo.jpg", "photo.jpg", "jpg", 10, "t", "")
	closeStore(st)

	out := captureStdout(t, func() { cmdClean(nil) })
	if !strings.Contains(out, "ничего не удалено") {
		t.Errorf("пробный прогон не обозначен: %q", out)
	}
	if !strings.Contains(out, "файлов к удалению 3 из 5") {
		t.Errorf("неверные числа предпросмотра: %q", out)
	}
	for _, want := range []string{"logo.png", "style.css"} {
		if !strings.Contains(out, want) {
			t.Errorf("кандидат %s не показан: %q", want, out)
		}
	}
	if !strings.Contains(out, "оформления") || !strings.Contains(out, "не является содержимым") {
		t.Errorf("причины удаления не названы: %q", out)
	}
	if !strings.Contains(out, "повторите с --apply") {
		t.Errorf("нет указания, как применить: %q", out)
	}
	// Содержимое не должно попасть в список кандидатов.
	if strings.Contains(out, "book.epub") || strings.Contains(out, "photo.jpg") {
		t.Errorf("настоящий файл назван мусором: %q", out)
	}

	// Пробный прогон обязан оставлять каталог нетронутым: это единственное, что
	// отличает его от --apply, и ошибка здесь означала бы удаление данных без
	// подтверждения.
	st = openTestStore(t, dir)
	defer closeStore(st)
	ctx := t.Context()
	if n, err := st.CountFiles(ctx); err != nil || n != 5 {
		t.Errorf("после пробного прогона в каталоге %d строк (err %v), ожидала 5", n, err)
	}
}

func TestCmdCleanApplyRemovesJunkAndFillsVerdicts(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	st := openTestStore(t, dir)
	mustAddFile(t, st, "http://a.onion/logo.png", "logo.png", "png", 10, "t", "")
	mustAddFile(t, st, "http://a.onion/style.css", "style.css", "css", 10, "t", "")
	mustAddFile(t, st, "http://a.onion/dir/", "dir", "", 0, "t", "")
	mustAddFile(t, st, "http://a.onion/book.epub", "book.epub", "epub", 10, "t", "")
	mustAddFile(t, st, "http://a.onion/photo.jpg", "photo.jpg", "jpg", 10, "t", "")
	closeStore(st)

	out := captureStdout(t, func() { cmdClean([]string{"--apply"}) })
	if !strings.Contains(out, "вычищено: файлов 3") {
		t.Errorf("неверное число удалённого: %q", out)
	}
	if !strings.Contains(out, "заполнено вердиктов: 2") {
		t.Errorf("вердикты не заполнены: %q", out)
	}

	st = openTestStore(t, dir)
	defer closeStore(st)
	ctx := t.Context()
	if n, err := st.CountFiles(ctx); err != nil || n != 2 {
		t.Fatalf("после чистки в каталоге %d строк (err %v), ожидала 2", n, err)
	}
	if n, err := st.CountFilesWithoutVerdict(ctx); err != nil || n != 0 {
		t.Errorf("осталось %d строк без вердикта (err %v)", n, err)
	}
	files, err := st.SearchFiles(ctx, store.FileQuery{})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f.Ext != "epub" && f.Ext != "jpg" {
			t.Errorf("мусор остался в каталоге: %+v", f)
		}
	}
}

func TestCmdCleanPreviewMatchesApply(t *testing.T) {
	// Главное свойство команды: предпросмотр обязан называть ровно то число,
	// которое удалит --apply. Расхождение означает, что пробный прогон и
	// настоящая чистка применяют разные критерии, и пользователь, проверивший
	// список, всё равно теряет данные.
	//
	// Оба прогона выполняются на одной базе подряд, поэтому сравниваются
	// фактические числа, а не ожидания теста.
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	st := openTestStore(t, dir)
	for i := 0; i < 40; i++ {
		mustAddFile(t, st, fmt.Sprintf("http://a.onion/junk%03d.ico", i),
			fmt.Sprintf("junk%03d.ico", i), "ico", 10, "t", "")
	}
	mustAddFile(t, st, "http://a.onion/logo.png", "logo.png", "png", 10, "t", "")
	mustAddFile(t, st, "http://a.onion/book.epub", "book.epub", "epub", 10, "t", "")
	mustAddFile(t, st, "http://a.onion/photo.jpg", "photo.jpg", "jpg", 10, "t", "")
	closeStore(st)

	preview := captureStdout(t, func() { cmdClean(nil) })
	promised := numberAfterField(t, preview, "удалению")
	total := numberAfterField(t, preview, "из")
	if total != 43 {
		t.Errorf("знаменатель предпросмотра %d, ожидала 43", total)
	}

	applied := captureStdout(t, func() { cmdClean([]string{"--apply"}) })
	removed := numberAfterField(t, applied, "файлов")
	if removed != promised {
		t.Errorf("предпросмотр обещал %d, а --apply удалил %d", promised, removed)
	}

	st = openTestStore(t, dir)
	defer closeStore(st)
	if n, err := st.CountFiles(t.Context()); err != nil || n != int(total-promised) {
		t.Errorf("в каталоге осталось %d строк (err %v), ожидала %d", n, err, total-promised)
	}
}

func TestCmdCleanCountsWholeCatalogBeyondListFilesLimit(t *testing.T) {
	// Регресс на дефект отчёта: прежняя версия строила числа на выборке
	// ListFiles, которая обрезана внутренним пределом store (5000) и
	// отсортирована по свежести. На каталоге в 6000 строк предпросмотр печатал
	// «файлов к удалению 4500 из 5000», а JSON с --apply давал files_after =
	// -500, потому что удалялось по всей таблице, а вычитали из обрезанной
	// выборки.
	const junk, keep = 5010, 7

	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	st := openTestStore(t, dir)
	ctx := t.Context()
	for i := 0; i < junk; i++ {
		f := store.FileEntry{
			TaskID:   "t",
			URL:      fmt.Sprintf("http://a.onion/junk%05d.ico", i),
			Filename: fmt.Sprintf("junk%05d.ico", i),
			Ext:      "ico",
			Size:     10,
		}
		if err := st.AddFile(ctx, f); err != nil {
			t.Fatalf("вставка %d: %v", i, err)
		}
	}
	for i := 0; i < keep; i++ {
		mustAddFile(t, st, fmt.Sprintf("http://a.onion/book%02d.epub", i),
			fmt.Sprintf("book%02d.epub", i), "epub", 10, "t", "")
	}
	closeStore(st)

	preview := captureStdout(t, func() { cmdClean(nil) })
	if !strings.Contains(preview, fmt.Sprintf("файлов к удалению %d из %d", junk, junk+keep)) {
		t.Errorf("знаменатель предпросмотра обрезан: %s", firstLines(preview, 1))
	}
	// Человекочитаемый список сокращается, и это обязано быть сказано явно:
	// молчаливое сокращение выглядит как полный список.
	if !strings.Contains(preview, "показаны первые 50") {
		t.Errorf("сокращение списка не объявлено: %s", preview)
	}
	if rest := junk - 50; !strings.Contains(preview, fmt.Sprintf("и ещё %d", rest)) {
		t.Errorf("не показано, сколько кандидатов скрыто: %s", preview)
	}

	out := captureStdout(t, func() { cmdClean([]string{"--apply", "--json"}) })
	rep := parseCleanJSON(t, out)
	if rep.FilesBefore != junk+keep {
		t.Errorf("files_before = %d, ожидала %d", rep.FilesBefore, junk+keep)
	}
	if rep.FilesRemoved != junk {
		t.Errorf("files_removed = %d, ожидала %d", rep.FilesRemoved, junk)
	}
	if rep.FilesAfter != keep {
		t.Errorf("files_after = %d, ожидала %d", rep.FilesAfter, keep)
	}
	if rep.FilesAfter < 0 {
		t.Errorf("отрицательный остаток каталога: %d", rep.FilesAfter)
	}
	if !rep.Applied {
		t.Error("applied не установлен")
	}
	if rep.VerdictFilled != keep {
		t.Errorf("verdict_filled = %d, ожидала %d", rep.VerdictFilled, keep)
	}
}

func TestCmdCleanDryRunJSONListsEveryCandidate(t *testing.T) {
	// Человекочитаемый вывод сокращает список кандидатов, чтобы число и причина
	// не тонули в тысячах строк. JSON читают машины, поэтому там список обязан
	// быть полным: обрезанный candidates означает, что скрипт проверки увидит
	// лишь часть мусора.
	const junk = 120

	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	st := openTestStore(t, dir)
	ctx := t.Context()
	for i := 0; i < junk; i++ {
		f := store.FileEntry{
			TaskID:   "t",
			URL:      fmt.Sprintf("http://a.onion/junk%03d.ico", i),
			Filename: fmt.Sprintf("junk%03d.ico", i),
			Ext:      "ico",
			Size:     10,
		}
		if err := st.AddFile(ctx, f); err != nil {
			t.Fatalf("вставка %d: %v", i, err)
		}
	}
	mustAddFile(t, st, "http://a.onion/book.epub", "book.epub", "epub", 10, "t", "")
	closeStore(st)

	out := captureStdout(t, func() { cmdClean([]string{"--json"}) })
	rep := parseCleanJSON(t, out)
	if rep.Applied {
		t.Error("пробный прогон отчитался как применённый")
	}
	if rep.FilesRemoved != junk {
		t.Errorf("files_removed = %d, ожидала %d", rep.FilesRemoved, junk)
	}
	if rep.candidateTotal != junk {
		t.Errorf("в JSON %d кандидатов, ожидала все %d", rep.candidateTotal, junk)
	}
	if rep.FilesBefore != junk+1 {
		t.Errorf("files_before = %d, ожидала %d", rep.FilesBefore, junk+1)
	}
	// files_after в пробном прогоне - прогноз остатка после чистки, а не
	// текущее состояние каталога: так поле понимала и прежняя версия.
	// Физическая неизменность базы проверяется отдельно ниже, и именно она
	// отличает пробный прогон от --apply.
	if rep.FilesAfter != 1 {
		t.Errorf("files_after = %d, ожидала прогноз остатка 1", rep.FilesAfter)
	}
	for _, c := range rep.Candidates {
		if c["reason"] == "" || c["filename"] == "" || c["url"] == "" {
			t.Errorf("кандидат без обязательных полей: %+v", c)
		}
	}

	// Пробный прогон не трогает базу, даже когда вывод идёт в JSON.
	st = openTestStore(t, dir)
	defer closeStore(st)
	if n, err := st.CountFiles(t.Context()); err != nil || n != junk+1 {
		t.Errorf("после пробного JSON-прогона в каталоге %d строк (err %v), ожидала %d", n, err, junk+1)
	}
}

func TestCmdCleanEmptyCatalog(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	out := captureStdout(t, func() { cmdClean(nil) })
	if !strings.Contains(out, "файлов к удалению 0 из 0") {
		t.Errorf("пустой каталог отчитан неверно: %q", out)
	}
	if strings.Contains(out, "повторите с --apply") {
		t.Errorf("пустой каталог предлагает удаление: %q", out)
	}

	applied := captureStdout(t, func() { cmdClean([]string{"--apply"}) })
	if !strings.Contains(applied, "вычищено: файлов 0") {
		t.Errorf("чистка пустого каталога: %q", applied)
	}
}

// firstLines возвращает первые n строк вывода для короткого сообщения об ошибке.
func firstLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, " / ")
}
