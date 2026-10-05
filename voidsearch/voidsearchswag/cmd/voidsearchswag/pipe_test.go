package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"voidsearchswag/internal/store"
)

// runMainBrokenPipe запускает команду в подпроцессе и закрывает читающий конец
// её stdout сразу после старта. Так воспроизводится обрыв потока вывода: буфер
// канала конечен, и как только он заполняется при закрытом читателе, запись
// возвращает ошибку.
//
// Отличие от runMain существенно: тот использует CombinedOutput и вычитывает
// поток до конца, поэтому ошибка записи в нём недостижима в принципе.
func runMainBrokenPipe(t *testing.T, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcessMain")
	cmd.Env = append(os.Environ(),
		"VSS_HELPER_MAIN=1",
		"VSS_MAIN_ARGS="+strings.Join(args, argSep),
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	// Читатель закрыт до того, как команда начала печатать.
	if err := stdout.Close(); err != nil {
		t.Fatalf("закрытие pipe: %v", err)
	}
	err = cmd.Wait()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("подпроцесс: %v", err)
		}
		code = ee.ExitCode()
	}
	return code, stderr.String()
}

// seedWideCatalog готовит каталог, чей JSON-вывод заведомо больше буфера
// канала: без этого запись успела бы уйти целиком и обрыв потока не случился.
func seedWideCatalog(t *testing.T, dir string, rows int) int {
	t.Helper()
	st := openTestStore(t, dir)
	ctx := t.Context()
	long := strings.Repeat("path-segment", 10)
	total := 0
	for i := 0; i < rows; i++ {
		url := fmt.Sprintf("http://host%03d.onion/%s/file%04d.epub", i%17, long, i)
		if err := st.AddFile(ctx, store.FileEntry{
			TaskID:   "t",
			URL:      url,
			Filename: fmt.Sprintf("file%04d.epub", i),
			Ext:      "epub",
			Size:     int64(1024 + i),
			MIME:     "application/epub+zip",
			Verdict:  "ebook",
		}); err != nil {
			t.Fatalf("вставка %d: %v", i, err)
		}
		total += len(url)
	}
	closeStore(st)
	return total
}

func TestFilesJSONReportsBrokenPipe(t *testing.T) {
	// Команда обязана сообщить, что вывод не дошёл до потребителя. До правки
	// ошибка записи игнорировалась: enc.Encode возвращал ошибку обрыва потока,
	// её никто не смотрел, и процесс завершался с кодом 0, отдав в поток
	// обрезанный JSON. Для скрипта-потребителя это худший вариант - разбор
	// обрывается на середине, а код возврата говорит, что всё хорошо.
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	seedWideCatalog(t, dir, 400)

	code, stderr := runMainBrokenPipe(t, "files", "--json", "--limit", "5000")
	if code != 1 {
		t.Errorf("код возврата %d, ожидала 1: обрыв потока вывода остался незамеченным (stderr: %q)", code, firstN(stderr, 200))
	}
	if !strings.Contains(stderr, "вывод") {
		t.Errorf("в stderr нет сообщения о несостоявшейся записи: %q", firstN(stderr, 200))
	}
}

// TestJSONOutputGoesThroughWriteJSON - защита от регресса статическим разбором.
//
// Общий хелпер имеет смысл ровно до тех пор, пока в обход него не появится новый
// прямой энкодер: такая команда снова начала бы отдавать обрезанный JSON с кодом
// возврата 0. Проверка разбирает исходники пакета и требует, чтобы
// json.NewEncoder встречался один раз - внутри writeJSON - и чтобы там же были
// сообщение об ошибке записи и прежний отступ.
func TestJSONOutputGoesThroughWriteJSON(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("чтение каталога: %v", err)
	}
	total := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("чтение %s: %v", name, err)
		}
		// Окончания строк нормализуются: при core.autocrlf рабочая копия может
		// быть в CRLF, и поиск по многострочным фрагментам тогда не сработает.
		text := strings.ReplaceAll(string(src), "\r\n", "\n")
		n := strings.Count(text, "json.NewEncoder")
		if n > 0 && name != "output.go" {
			t.Errorf("%s: прямой энкодер в обход writeJSON, %d мест", name, n)
		}
		total += n
	}
	if total != 1 {
		t.Errorf("json.NewEncoder встречается %d раз во всём пакете, ожидала 1 - внутри writeJSON", total)
	}

	src, err := os.ReadFile("output.go")
	if err != nil {
		t.Fatalf("чтение output.go: %v", err)
	}
	text := strings.ReplaceAll(string(src), "\r\n", "\n")
	start := strings.Index(text, "func writeJSON(")
	if start < 0 {
		t.Fatal("в output.go нет функции writeJSON")
	}
	body := text[start:]
	if end := strings.Index(body, "\nfunc "); end > 0 {
		body = body[:end]
	}
	if !strings.Contains(body, "json.NewEncoder(os.Stdout)") {
		t.Error("writeJSON не пишет в stdout")
	}
	if !strings.Contains(body, `enc.SetIndent("", "  ")`) {
		t.Error("writeJSON не задаёт отступ: форма JSON-вывода всех команд изменилась бы")
	}
	if !strings.Contains(body, `fatalf("вывод: %v", err)`) {
		t.Error("writeJSON не сообщает об ошибке записи: обрыв потока снова станет молчаливым")
	}
}

// firstN обрезает строку для сообщения об ошибке, чтобы не печатать килобайты.
func firstN(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
