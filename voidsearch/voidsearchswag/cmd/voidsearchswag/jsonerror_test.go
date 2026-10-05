package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// helperDataDir возвращает каталог данных для прогона в подпроцессе: заданный
// тестом, а при его отсутствии - свежий временный. Боевой каталог не
// подставляется никогда.
//
// Правило появилось после инцидента на этапе 133: мутационный прогон сломал
// проверку глубины, discover --crawl не отклонил -depth -1, дошёл до базы из
// конфига и записал в боевой файл. Содержимое таблиц не изменилось, пул остался
// 11088 строк с теми же статусами и тем же максимальным last_probe, но mtime
// боевой базы сдвинулся. Полагаться на «запись оказалась пустой» нельзя, поэтому
// прогон без явно заданного каталога больше не начинается вовсе.
func helperDataDir(t *testing.T) string {
	t.Helper()
	if dir := os.Getenv("VOIDSEARCH_DATA_DIR"); dir != "" {
		return dir
	}
	return t.TempDir()
}

// subprocEnv собирает окружение для прогона main в подпроцессе. Каталог данных
// подставляется одной записью: дубль переменной оставил бы поведение зависимым от
// порядка записей, а цена ошибки здесь - боевая база.
func subprocEnv(t *testing.T, args []string) []string {
	t.Helper()
	base := os.Environ()
	env := make([]string, 0, len(base)+3)
	for _, kv := range base {
		if strings.HasPrefix(kv, "VOIDSEARCH_DATA_DIR=") {
			continue
		}
		env = append(env, kv)
	}
	return append(env,
		"VOIDSEARCH_DATA_DIR="+helperDataDir(t),
		"VSS_HELPER_MAIN=1",
		"VSS_MAIN_ARGS="+strings.Join(args, argSep),
	)
}

// runMainSplit запускает main в подпроцессе и возвращает stdout и stderr
// раздельно. Общий runMain из main_test.go склеивает потоки через CombinedOutput,
// а предмет этого этапа - именно поток: при склейке тест не отличил бы JSON в
// stdout от JSON, случайно напечатанного в stderr.
//
// Подпроцесс получает собственный -test.timeout: раньше runMainSplit запускал
// его без будильника, и молчаливое зависание main() внутри TestHelperProcessMain
// навсегда вешало родительский прогон пакета. Теперь зависший подпроцесс сам
// роняет себя через тестовый таймаут и приносит стек висящей горутины в stderr,
// а WaitDelay добивает процесс, если будильник по какой-то причине не сработал.
func runMainSplit(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcessMain", "-test.timeout=90s")
	cmd.Env = subprocEnv(t, args)
	cmd.WaitDelay = 105 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("подпроцесс: %v\nstderr:\n%s", err, stderr.String())
		}
		code = ee.ExitCode()
	}
	return stdout.String(), stderr.String(), code
}

// jsonErrorCases - отказы, которые случаются до сети и до тяжёлой работы.
var jsonErrorCases = []struct {
	name string
	args []string
	want string
}{
	{"hunt без подкоманды", []string{"hunt", "--json"}, "подкоманда"},
	{"search без запроса", []string{"search", "--json"}, "нужен поисковый запрос"},
	{"hunt hits --clear без --id", []string{"hunt", "hits", "--clear", "--json"}, "нужен --id"},
	{"hunt run с несуществующей охотой", []string{"hunt", "run", "--id", "999", "--no-tor", "--json"}, "запись не найдена"},
	{"hunt hits --clear у несуществующей охоты", []string{"hunt", "hits", "--clear", "--id", "999", "--json"}, "запись не найдена"},
}

// Отказ при --json обязан оставить тело в stdout: код возврата 1 говорит
// «что-то не так», но причину машина читает из потока данных.
func TestJSONErrorGoesToStdout(t *testing.T) {
	t.Setenv("VOIDSEARCH_DATA_DIR", t.TempDir())
	for _, tc := range jsonErrorCases {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, code := runMainSplit(t, tc.args...)
			if code != 1 {
				t.Errorf("код возврата %d, хочу 1", code)
			}
			if strings.TrimSpace(stdout) == "" {
				t.Fatalf("stdout пуст: потребитель JSON не получил ничего, stderr=%q", strings.TrimSpace(stderr))
			}
			var body map[string]any
			if err := json.Unmarshal([]byte(stdout), &body); err != nil {
				t.Fatalf("stdout не разбирается как один JSON-документ: %v (%.160s)", err, stdout)
			}
			msg, _ := body["error"].(string)
			if msg == "" {
				t.Errorf("в теле нет строковой error: %v", body)
			}
			if !strings.Contains(msg, tc.want) {
				t.Errorf("error = %q, хочу подстроку %q", msg, tc.want)
			}
			if len(body) != 1 {
				t.Errorf("в теле %d полей, хочу одно error: %v", len(body), body)
			}
			// Человекочитаемая строка остаётся: оператор в терминале не должен
			// терять причину из-за того, что вызывающий попросил JSON.
			if !strings.Contains(stderr, "ERROR: ") || !strings.Contains(stderr, tc.want) {
				t.Errorf("в stderr нет причины: %q", strings.TrimSpace(stderr))
			}
		})
	}
}

// Без --json поведение прежнее: stdout пуст, причина в stderr. Машинный вывод не
// должен появляться там, где его не просили.
func TestTextErrorKeepsStdoutEmpty(t *testing.T) {
	t.Setenv("VOIDSEARCH_DATA_DIR", t.TempDir())
	for _, tc := range jsonErrorCases {
		args := make([]string, 0, len(tc.args))
		for _, a := range tc.args {
			if a != "--json" {
				args = append(args, a)
			}
		}
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, code := runMainSplit(t, args...)
			if code != 1 {
				t.Errorf("код возврата %d, хочу 1", code)
			}
			if strings.TrimSpace(stdout) != "" {
				t.Errorf("stdout непуст без --json: %.160s", stdout)
			}
			if !strings.Contains(stderr, "ERROR: ") || !strings.Contains(stderr, tc.want) {
				t.Errorf("в stderr нет причины: %q", strings.TrimSpace(stderr))
			}
		})
	}
}

// Флаг ищется по аргументам, а не по подстрокам: иначе запрос «что значит
// --json» переводил бы обычный поиск в машинный режим.
func TestWantsJSON(t *testing.T) {
	yes := [][]string{
		{"--json"},
		{"-json"},
		{"search", "--json", "leak"},
		{"hunt", "hits", "--limit", "5", "--json"},
		{"--verbose", "stats", "--json"},
	}
	for _, args := range yes {
		if !wantsJSON(args) {
			t.Errorf("wantsJSON(%v) = false, хочу true", args)
		}
	}
	no := [][]string{
		{},
		{"search", "leak database"},
		{"search", "что значит --json"},
		{"--jsonish"},
		{"search", "-jsonx"},
		{"hunt", "list"},
	}
	for _, args := range no {
		if wantsJSON(args) {
			t.Errorf("wantsJSON(%v) = true, хочу false", args)
		}
	}
}

// Короткая форма флага разбирается так же: команды принимают и -json, и --json.
func TestJSONErrorWithShortFlag(t *testing.T) {
	t.Setenv("VOIDSEARCH_DATA_DIR", t.TempDir())
	stdout, _, code := runMainSplit(t, "search", "-json")
	if code != 1 {
		t.Errorf("код возврата %d, хочу 1", code)
	}
	var body map[string]string
	if err := json.Unmarshal([]byte(stdout), &body); err != nil {
		t.Fatalf("stdout с -json не JSON: %v (%.120s)", err, stdout)
	}
	if !strings.Contains(body["error"], "нужен поисковый запрос") {
		t.Errorf("error = %q", body["error"])
	}
}

// Обрыв потока при --json не должен превращать отказ в лавину. writeJSON сообщает
// о несостоявшейся записи через fatalf, а тот в режиме --json снова полез бы
// печатать JSON в мёртвый поток, и каждый виток оставлял бы новую строку в
// stderr. writeJSON гасит машинный режим на месте, поэтому сообщение о срыве
// записи остаётся одно.
func TestJSONErrorOnBrokenPipeStopsAfterOneAttempt(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	seedWideCatalog(t, dir, 400)

	code, stderr := runMainBrokenPipe(t, "files", "--json", "--limit", "5000")
	if code != 1 {
		t.Errorf("код возврата %d, хочу 1 (stderr: %q)", code, firstN(stderr, 200))
	}
	if n := strings.Count(stderr, "вывод:"); n != 1 {
		t.Errorf("сообщений о несостоявшейся записи %d, хочу 1: %q", n, firstN(stderr, 400))
	}
	if n := strings.Count(stderr, `"error"`); n != 0 {
		t.Errorf("JSON-тело пытались напечатать в мёртвый поток %d раз: %q", n, firstN(stderr, 400))
	}
	if len(stderr) > 4000 {
		t.Errorf("stderr раздулся до %d байт - похоже на лавину повторных попыток", len(stderr))
	}
}
