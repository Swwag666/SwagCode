package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
)

// Флаг --fields у parse объявлен через fs.String, поэтому его повтор молча
// перезаписывает прежнее значение: «parse --fields title --fields email»
// разбирает только email. Живой замер на бинаре этапа 108 подтверждает потерю и
// показывает, что она меняет код возврата:
//
//	parse --no-tor --json --fields title --fields description https://example.com/
//	  rc=1, fields содержит только description, error «ни одно поле не извлечено»
//
// Запрос внешнего хоста нестабилен по TLS, поэтому воспроизведение здесь идёт
// через локальный стенд и VOIDSEARCH_ALLOW_PRIVATE=on, который снимает барьер
// служебных адресов в Engine.FetchURL.
func newParsePage(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<html><head><title>Локальная страница</title></head>`+
			`<body><h1>Каталог</h1><a href="mailto:shop@example.test">написать</a></body></html>`)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/"
}

// parseEnv задаёт окружение офлайн-прогона: своя база, без tor, прямой транспорт
// и разрешение ходить на локальный стенд.
func parseEnv(t *testing.T) {
	t.Helper()
	t.Setenv("VOIDSEARCH_DATA_DIR", t.TempDir())
	t.Setenv("VOIDSEARCH_BACKUP_DIR", "")
	t.Setenv("VOIDSEARCH_TOR", "off")
	t.Setenv("VOIDSEARCH_TRANSPORT", "direct")
	t.Setenv("VOIDSEARCH_ALLOW_PRIVATE", "true")
	t.Setenv("VOIDSEARCH_REQUEST_TIMEOUT", "8s")
}

// parseJSON разбирает машинный ответ parse на поля, предупреждение и ошибку.
func parseJSON(t *testing.T, stdout string) (map[string]map[string]any, string, string) {
	t.Helper()
	var rep struct {
		Fields  map[string]map[string]any `json:"fields"`
		Warning string                    `json:"warning"`
		Error   string                    `json:"error"`
	}
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("разбор JSON: %v, вывод %q", err, firstN(stdout, 400))
	}
	return rep.Fields, rep.Warning, rep.Error
}

// fieldNames возвращает отсортированные имена полей ответа.
func fieldNames(fields map[string]map[string]any) []string {
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func TestCmdParseAccumulatesRepeatedFields(t *testing.T) {
	parseEnv(t)
	url := newParsePage(t)

	stdout, stderr, code := runMainSplit(t, "parse", "--no-tor", "--json",
		"--fields", "title", "--fields", "email", url)
	fields, _, errText := parseJSON(t, stdout)

	if len(fields) != 2 {
		t.Errorf("повторяющийся --fields дал %d полей %v, хочу 2; error=%q stderr=%q",
			len(fields), fieldNames(fields), errText, firstN(stderr, 200))
	}
	if _, ok := fields["title"]; !ok {
		t.Errorf("поле первого --fields потеряно: %v", fieldNames(fields))
	}
	if _, ok := fields["email"]; !ok {
		t.Errorf("поле второго --fields потеряно: %v", fieldNames(fields))
	}
	if code != 0 {
		t.Errorf("код возврата %d при двух извлечённых полях, error=%q", code, errText)
	}
}

func TestCmdParseRepeatedFieldsMatchesCommaForm(t *testing.T) {
	parseEnv(t)
	url := newParsePage(t)

	repeated, _, _ := parseJSON(t, firstOfRunMainSplit(t, "parse", "--no-tor", "--json",
		"--fields", "title", "--fields", "email", url))
	comma, _, _ := parseJSON(t, firstOfRunMainSplit(t, "parse", "--no-tor", "--json",
		"--fields", "title,email", url))

	if len(repeated) != len(comma) {
		t.Errorf("повтор флага дал %d полей %v, форма через запятую %d полей %v",
			len(repeated), fieldNames(repeated), len(comma), fieldNames(comma))
	}
}

// Потеря поля меняет не только состав ответа, но и код возврата: страница без
// поля nosuchfield даёт ошибку «ни одно поле не извлечено», хотя title извлечён
// и команда обязана завершиться нулём.
func TestCmdParseRepeatedFieldsKeepsExitCode(t *testing.T) {
	parseEnv(t)
	url := newParsePage(t)

	stdout, _, code := runMainSplit(t, "parse", "--no-tor", "--json",
		"--fields", "title", "--fields", "nosuchfield", url)
	fields, _, errText := parseJSON(t, stdout)

	if code != 0 {
		t.Errorf("код возврата %d, хотя title извлекается; error=%q поля=%v",
			code, errText, fieldNames(fields))
	}
	if fields["title"]["value"] != "Локальная страница" {
		t.Errorf("title не извлечён: %+v", fields["title"])
	}
	if _, ok := fields["nosuchfield"]; !ok {
		t.Errorf("второе поле не попало в ответ: %v", fieldNames(fields))
	}
}

// Контроль: одиночный флаг, форма через запятую и дефолт ведут себя как прежде.
func TestCmdParseFieldFormsUnchanged(t *testing.T) {
	parseEnv(t)
	url := newParsePage(t)

	single, _, _ := parseJSON(t, firstOfRunMainSplit(t, "parse", "--no-tor", "--json",
		"--fields", "title", url))
	if len(single) != 1 {
		t.Errorf("одиночный --fields дал %d полей %v", len(single), fieldNames(single))
	}

	comma, _, _ := parseJSON(t, firstOfRunMainSplit(t, "parse", "--no-tor", "--json",
		"--fields", "title,email", url))
	if len(comma) != 2 {
		t.Errorf("форма через запятую дала %d полей %v", len(comma), fieldNames(comma))
	}

	def, _, _ := parseJSON(t, firstOfRunMainSplit(t, "parse", "--no-tor", "--json", url))
	if len(def) != 1 {
		t.Errorf("без --fields дано %d полей %v, хочу одно title", len(def), fieldNames(def))
	}
	if _, ok := def["title"]; !ok {
		t.Errorf("дефолтное поле не title: %v", fieldNames(def))
	}
}

// firstOfRunMainSplit возвращает только stdout подпроцесса.
func firstOfRunMainSplit(t *testing.T, args ...string) string {
	t.Helper()
	stdout, _, _ := runMainSplit(t, args...)
	return stdout
}
