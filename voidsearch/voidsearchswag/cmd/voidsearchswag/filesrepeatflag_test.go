package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"voidsearchswag/internal/store"
)

// Повторяющийся флаг-список молча терял прежнее значение. Живой замер на копии
// боевой базы (каталог 19 файлов: epub 6, mp4 11, pdf 2) дал returned=6 для
// «files -ext pdf -ext epub» против returned=8 для «files -ext pdf,epub» и ровно
// столько же для «files -ext epub»: расширение из первого флага исчезло без
// единого слова предупреждения. Причина в том, что флаг объявлен как fs.String, а
// у него одно хранимое значение, поэтому второй вызов перезаписывает первый.
// Та же картина с метками категории: «-verdict ebook -verdict document» вернул
// 2 записи вместо 8.
func TestCmdFilesAccumulatesRepeatedExtFlags(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	st := openTestStore(t, dir)
	mustAddFile(t, st, "http://abc.onion/f/book.epub", "book.epub", "epub", 100, "t", "")
	mustAddFile(t, st, "http://abc.onion/f/report.pdf", "report.pdf", "pdf", 200, "t", "")
	mustAddFile(t, st, "http://abc.onion/f/video.mp4", "video.mp4", "mp4", 300, "t", "")
	closeStore(st)

	out := captureStdout(t, func() { cmdFiles([]string{"-ext", "pdf", "-ext", "epub"}) })
	if !strings.Contains(out, "book.epub") {
		t.Errorf("значение второго -ext (epub) потеряно: %q", out)
	}
	if !strings.Contains(out, "report.pdf") {
		t.Errorf("значение первого -ext (pdf) потеряно: %q", out)
	}
	if strings.Contains(out, "video.mp4") {
		t.Errorf("фильтр пропустил запись вне списка: %q", out)
	}
	if !strings.Contains(out, "показано 2") {
		t.Errorf("счётчик отобранных записей не равен двум: %q", out)
	}
}

// Машинный режим обязан совпадать с текстовым: параметр ext в MCP-инструменте
// file_search приходит в то же поле FileQuery.Ext, и расхождение двух входов CLI
// было бы невидимым до первого же спора о выдаче.
func TestCmdFilesRepeatedExtMatchesCommaForm(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	st := openTestStore(t, dir)
	mustAddFile(t, st, "http://abc.onion/f/book.epub", "book.epub", "epub", 100, "t", "")
	mustAddFile(t, st, "http://abc.onion/f/report.pdf", "report.pdf", "pdf", 200, "t", "")
	mustAddFile(t, st, "http://abc.onion/f/video.mp4", "video.mp4", "mp4", 300, "t", "")
	closeStore(st)

	repeated := filesReturned(t, "-ext", "pdf", "-ext", "epub")
	comma := filesReturned(t, "-ext", "pdf,epub")
	if repeated != 2 {
		t.Errorf("повторяющийся -ext отобрал %d записей, хочу 2", repeated)
	}
	if repeated != comma {
		t.Errorf("повторяющийся -ext дал %d записей, а форма через запятую %d", repeated, comma)
	}
}

// Метки категории страдают тем же: fs.String хранит одно значение.
func TestCmdFilesAccumulatesRepeatedVerdictFlags(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	st := openTestStore(t, dir)
	for _, f := range []store.FileEntry{
		{TaskID: "t", URL: "http://a.onion/book.epub", Filename: "book.epub", Ext: "epub", Size: 100, Verdict: "ebook"},
		{TaskID: "t", URL: "http://a.onion/doc.pdf", Filename: "doc.pdf", Ext: "pdf", Size: 200, Verdict: "document"},
		{TaskID: "t", URL: "http://a.onion/pack.zip", Filename: "pack.zip", Ext: "zip", Size: 300, Verdict: "archive"},
	} {
		if err := st.AddFile(context.Background(), f); err != nil {
			t.Fatalf("AddFile(%s): %v", f.Filename, err)
		}
	}
	closeStore(st)

	out := captureStdout(t, func() { cmdFiles([]string{"-verdict", "ebook", "-verdict", "document"}) })
	if !strings.Contains(out, "book.epub") {
		t.Errorf("значение первой метки потеряно: %q", out)
	}
	if !strings.Contains(out, "doc.pdf") {
		t.Errorf("значение второй метки потеряно: %q", out)
	}
	if strings.Contains(out, "pack.zip") {
		t.Errorf("фильтр пропустил запись вне списка меток: %q", out)
	}
	if !strings.Contains(out, "показано 2") {
		t.Errorf("счётчик отобранных записей не равен двум: %q", out)
	}
}

// Обратная сторона: одиночный флаг и список через запятую обязаны работать как
// прежде, накопление не должно менять ни разбор одного значения, ни порядок.
func TestCmdFilesSingleExtFlagUnchanged(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	st := openTestStore(t, dir)
	mustAddFile(t, st, "http://abc.onion/f/book.epub", "book.epub", "epub", 100, "t", "")
	mustAddFile(t, st, "http://abc.onion/f/report.pdf", "report.pdf", "pdf", 200, "t", "")
	closeStore(st)

	out := captureStdout(t, func() { cmdFiles([]string{"-ext", "epub"}) })
	if !strings.Contains(out, "book.epub") {
		t.Errorf("одиночный -ext потерял запись: %q", out)
	}
	if strings.Contains(out, "report.pdf") {
		t.Errorf("одиночный -ext пропустил чужую запись: %q", out)
	}
	if !strings.Contains(out, "показано 1") {
		t.Errorf("счётчик отобранных записей не равен одному: %q", out)
	}

	if n := filesReturned(t, "-ext", "epub,pdf"); n != 2 {
		t.Errorf("форма через запятую отобрала %d записей, хочу 2", n)
	}
	if n := filesReturned(t); n != 3 {
		t.Errorf("без фильтра отобрано %d записей, хочу 3", n)
	}
}

// filesReturned гоняет files --json и возвращает счётчик отобранных записей.
func filesReturned(t *testing.T, args ...string) int {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	st := openTestStore(t, dir)
	mustAddFile(t, st, "http://abc.onion/f/book.epub", "book.epub", "epub", 100, "t", "")
	mustAddFile(t, st, "http://abc.onion/f/report.pdf", "report.pdf", "pdf", 200, "t", "")
	mustAddFile(t, st, "http://abc.onion/f/video.mp4", "video.mp4", "mp4", 300, "t", "")
	closeStore(st)

	full := append([]string{"--json"}, args...)
	out := captureStdout(t, func() { cmdFiles(full) })
	var rep struct {
		Returned int `json:"returned"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("разбор JSON: %v, вывод %q", err, firstN(out, 300))
	}
	return rep.Returned
}

// Накопитель разбирается напрямую: команда поднимает базу и каталог, а границы
// разбора списка - пустые части, пробелы, смешанная форма - должны проверяться
// без них.
func TestListFlagAccumulates(t *testing.T) {
	for _, c := range []struct {
		name string
		sets []string
		want string
	}{
		{"одно значение", []string{"pdf"}, "pdf"},
		{"повтор флага", []string{"pdf", "epub"}, "pdf,epub"},
		{"три повтора", []string{"pdf", "epub", "zip"}, "pdf,epub,zip"},
		{"список через запятую", []string{"pdf,epub"}, "pdf,epub"},
		{"смешанная форма", []string{"pdf,epub", "zip"}, "pdf,epub,zip"},
		{"пустые части отброшены", []string{"pdf,", ",epub", ""}, "pdf,epub"},
		{"пробелы по краям", []string{" pdf ", "epub\t"}, "pdf,epub"},
		{"флаг не задан", nil, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			var f listFlag
			for _, s := range c.sets {
				if err := f.Set(s); err != nil {
					t.Fatalf("Set(%q): %v", s, err)
				}
			}
			if got := f.value(); got != c.want {
				t.Errorf("value() = %q, хочу %q", got, c.want)
			}
			if got := f.String(); got != c.want {
				t.Errorf("String() = %q, хочу %q", got, c.want)
			}
		})
	}
}

// Значение по умолчанию подставляется только тогда, когда флаг не задан. Иначе
// «parse --fields ""» молча превратилось бы в title, и явный пустой список
// оказался бы неотличим от отсутствующего флага.
func TestListFlagListOrDefault(t *testing.T) {
	for _, c := range []struct {
		name string
		sets []string
		def  string
		want []string
	}{
		{"флаг не задан", nil, "title", []string{"title"}},
		{"дефолт из списка", nil, "title,price", []string{"title", "price"}},
		{"одно значение", []string{"price"}, "title", []string{"price"}},
		{"повтор флага", []string{"price", "email"}, "title", []string{"price", "email"}},
		{"пустое значение перебивает дефолт", []string{""}, "title", nil},
		{"пробелы вокруг дефолта", nil, " title , price ", []string{"title", "price"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var f listFlag
			for _, s := range c.sets {
				if err := f.Set(s); err != nil {
					t.Fatalf("Set(%q): %v", s, err)
				}
			}
			got := f.listOr(c.def)
			if len(got) != len(c.want) {
				t.Fatalf("listOr() = %q, хочу %q", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("listOr()[%d] = %q, хочу %q", i, got[i], c.want[i])
				}
			}
		})
	}
}

// Разбор значения по умолчанию идёт тем же правилом, что и значение флага.
func TestSplitListDropsEmptyParts(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"", ""},
		{",", ""},
		{"a,,b", "a,b"},
		{" a , b ", "a,b"},
		{"a;b", "a;b"},
		{"pdf,epub", "pdf,epub"},
	} {
		if got := strings.Join(splitList(c.in), ","); got != c.want {
			t.Errorf("splitList(%q) = %q, хочу %q", c.in, got, c.want)
		}
	}
}
