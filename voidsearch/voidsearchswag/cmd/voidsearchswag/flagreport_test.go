package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// flagMessage обязан покрывать все шесть форматов, которыми пакет flag
// сообщает об ошибке разбора: неизвестный флаг, пропущенное значение, неверное
// значение, два булевых случая и неверный синтаксис. Нераспознанный текст не
// теряется, а уходит в общее сообщение вместе с оригиналом.
func TestFlagMessage(t *testing.T) {
	rows := []struct {
		raw  string
		want string
	}{
		{
			"flag provided but not defined: -nonsense",
			"неизвестный флаг -nonsense команды stats",
		},
		{
			"flag needs an argument: -limit",
			"флагу -limit команды stats нужно значение",
		},
		{
			`invalid value "abc" for flag -limit: parse error`,
			`неверное значение флага команды stats: "abc" for flag -limit: parse error`,
		},
		{
			`invalid boolean value "yes" for -json: parse error`,
			`неверное булево значение флага команды stats: "yes" for -json: parse error`,
		},
		{
			// Префикс снимается целиком, поэтому имя флага остаётся в хвосте:
			// первое ожидание этого случая было написано без «-json» и пало на
			// первом же прогоне.
			"invalid boolean flag -json: parse error",
			"неверный булев флаг команды stats: -json: parse error",
		},
		{
			"bad flag syntax: -",
			"неверный синтаксис флага команды stats: -",
		},
		{
			"сообщение из будущей версии библиотеки",
			"ошибка разбора флагов команды stats: сообщение из будущей версии библиотеки",
		},
	}
	for _, r := range rows {
		if got := flagMessage("stats", r.raw); got != r.want {
			t.Errorf("flagMessage(%q) = %q, хочу %q", r.raw, got, r.want)
		}
	}
}

func setFlagReportEnv(t *testing.T) {
	t.Helper()
	t.Setenv("VOIDSEARCH_DATA_DIR", t.TempDir())
	t.Setenv("VOIDSEARCH_TOR", "off")
	t.Setenv("VOIDSEARCH_TRANSPORT", "direct")
	t.Setenv("VOIDSEARCH_BACKUP_DIR", "")
}

// Живой замер ДО на бинаре c488560: stats --nonsense давал rc=2, stdout 0 байт
// и stderr «flag provided but not defined: -nonsense» со стандартным «Usage of
// stats:». Английская причина без имени команды и без списка допустимых флагов
// - это то, что оператор видит чаще всего: опечатка в флаге случается в каждом
// втором служебном запуске.
func TestUnknownFlagReportedInRussian(t *testing.T) {
	setFlagReportEnv(t)

	stdout, stderr, code := runMainSplit(t, "stats", "--nonsense")
	if code != 2 {
		t.Errorf("код возврата %d, хочу 2: ошибка аргументов не должна менять контракт", code)
	}
	if strings.Contains(stderr, "flag provided but not defined") {
		t.Errorf("английское сообщение пакета flag осталось: %s", firstN(stderr, 300))
	}
	if strings.Contains(stderr, "Usage of stats:") {
		t.Errorf("английский заголовок справки остался: %s", firstN(stderr, 300))
	}
	if !strings.Contains(stderr, "неизвестный флаг -nonsense команды stats") {
		t.Errorf("русская причина не напечатана: %s", firstN(stderr, 300))
	}
	if !strings.Contains(stderr, "флаги команды stats:") {
		t.Errorf("заголовок списка флагов не напечатан: %s", firstN(stderr, 300))
	}
	if !strings.Contains(stderr, "-json") {
		t.Errorf("список флагов не напечатан: %s", firstN(stderr, 300))
	}
	if stdout != "" {
		t.Errorf("без --json в stdout попал текст: %s", firstN(stdout, 200))
	}
}

// Машина, которая просила JSON, обязана получить тело: до правки stdout
// оставался пустым (0 байт) и потребитель видел только код 2 без причины.
func TestUnknownFlagJSONGoesToStdout(t *testing.T) {
	setFlagReportEnv(t)

	stdout, stderr, code := runMainSplit(t, "poolsearch", "--json", "--nope")
	if code != 2 {
		t.Errorf("код возврата %d, хочу 2", code)
	}
	body := decodeJSON(t, stdout)
	msg, _ := body["error"].(string)
	if !strings.Contains(msg, "неизвестный флаг -nope команды poolsearch") {
		t.Errorf("в JSON нет причины: %s", firstN(stdout, 300))
	}
	if !strings.Contains(stderr, "ERROR: неизвестный флаг -nope команды poolsearch") {
		t.Errorf("в stderr нет причины для человека: %s", firstN(stderr, 300))
	}
	if strings.Contains(stderr, "flag provided but not defined") {
		t.Errorf("английское сообщение осталось в stderr: %s", firstN(stderr, 300))
	}
	// Список флагов в машинном режиме не печатается: он попал бы в stdout
	// вместе с телом или засорил stderr.
	if strings.Contains(stderr, "Использование: voidsearchswag poolsearch") {
		t.Errorf("в режиме --json напечатана справка: %s", firstN(stderr, 200))
	}
}

// Свой usage команды пережил перехват: у poolsearch он объясняет позиционный
// запрос, которого в списке флагов просто нет.
func TestOwnUsageSurvivesFlagError(t *testing.T) {
	setFlagReportEnv(t)

	_, stderr, code := runMainSplit(t, "poolsearch", "--nope")
	if code != 2 {
		t.Errorf("код возврата %d, хочу 2", code)
	}
	if !strings.Contains(stderr, "неизвестный флаг -nope команды poolsearch") {
		t.Errorf("причина не напечатана: %s", firstN(stderr, 300))
	}
	if !strings.Contains(stderr, "Использование: voidsearchswag poolsearch") {
		t.Errorf("свой usage потерян: %s", firstN(stderr, 300))
	}
	if strings.Contains(stderr, "Usage of poolsearch") {
		t.Errorf("справка напечатана дважды или стандартным заголовком: %s", firstN(stderr, 300))
	}
	// Свой usage не должен подменяться стандартным списком: без этой проверки
	// мутация «всегда печатать свой список флагов» проходила незамеченной,
	// потому что poolsearch пишет шапку напрямую в stderr, а список флагов - в
	// поток набора, и шапка оставалась на месте при любом исходе.
	if strings.Contains(stderr, "флаги команды poolsearch:") {
		t.Errorf("свой usage подменён стандартным списком флагов: %s", firstN(stderr, 400))
	}
}

// Справка остаётся справкой: -h и --help завершаются нулём и не печатают отказ.
// Пакет flag зовёт Usage без сообщения об ошибке, и печатать там текст про
// неизвестный флаг значило бы показать оператору отказ вместо помощи.
func TestHelpStillExitsZero(t *testing.T) {
	setFlagReportEnv(t)

	cases := [][]string{
		{"stats", "-h"},
		{"backup", "--help"},
		{"poolsearch", "-h"},
	}
	for _, args := range cases {
		stdout, stderr, code := runMainSplit(t, args...)
		if code != 0 {
			t.Errorf("%v: код возврата %d, хочу 0 (stderr %s)", args, code, firstN(stderr, 200))
		}
		if strings.Contains(stderr, "ERROR:") {
			t.Errorf("%v: справка напечатала отказ: %s", args, firstN(stderr, 200))
		}
		if strings.Contains(stderr, "неизвестный флаг") {
			t.Errorf("%v: справка названа неизвестным флагом: %s", args, firstN(stderr, 200))
		}
		if stdout != "" {
			t.Errorf("%v: справка ушла в stdout: %s", args, firstN(stdout, 200))
		}
		if !strings.Contains(stderr, "-json") {
			t.Errorf("%v: список флагов не напечатан: %s", args, firstN(stderr, 200))
		}
	}
}

// Неверное значение флага получает ту же обработку, что неизвестный флаг.
func TestBadValueReportedInRussian(t *testing.T) {
	setFlagReportEnv(t)

	_, stderr, code := runMainSplit(t, "poolsearch", "--limit", "abc")
	if code != 2 {
		t.Errorf("код возврата %d, хочу 2", code)
	}
	if !strings.Contains(stderr, "неверное значение флага команды poolsearch") {
		t.Errorf("причина не напечатана по-русски: %s", firstN(stderr, 300))
	}
	if !strings.Contains(stderr, "abc") {
		t.Errorf("в причине нет спорного значения: %s", firstN(stderr, 300))
	}
	if strings.Contains(stderr, "invalid value") {
		t.Errorf("английский префикс остался: %s", firstN(stderr, 300))
	}
}

// hunt create, hunt run и hunt watch разбирают аргументы без hoistFlags, то
// есть идут через parseFlagsRaw: отчёт обязан работать и в этих ветках.
func TestHuntBranchesReportToo(t *testing.T) {
	setFlagReportEnv(t)

	cases := []struct {
		args []string
		name string
	}{
		{[]string{"hunt", "create", "--nope"}, "hunt create"},
		{[]string{"hunt", "run", "--nope"}, "hunt run"},
		{[]string{"hunt", "watch", "--nope"}, "hunt watch"},
	}
	for _, c := range cases {
		_, stderr, code := runMainSplit(t, c.args...)
		if code != 2 {
			t.Errorf("%v: код возврата %d, хочу 2 (stderr %s)", c.args, code, firstN(stderr, 200))
			continue
		}
		want := "неизвестный флаг -nope команды " + c.name
		if !strings.Contains(stderr, want) {
			t.Errorf("%v: нет строки %q, stderr: %s", c.args, want, firstN(stderr, 300))
		}
		if strings.Contains(stderr, "flag provided but not defined") {
			t.Errorf("%v: английское сообщение осталось: %s", c.args, firstN(stderr, 200))
		}
	}
}

// Ловушка не должна ломать обычный разбор: --json остаётся машинным выводом, а
// код возврата прежний.
func TestKnownFlagsStillWork(t *testing.T) {
	setFlagReportEnv(t)

	stdout, stderr, code := runMainSplit(t, "stats", "--json")
	if code != 0 {
		t.Fatalf("stats --json: код %d, stderr %s", code, firstN(stderr, 300))
	}
	body := decodeJSON(t, stdout)
	if _, ok := body["onion_total"]; !ok {
		t.Errorf("в stats --json нет onion_total: %s", firstN(stdout, 300))
	}
	if strings.Contains(stderr, "ERROR:") {
		t.Errorf("успешный запуск напечатал отказ: %s", firstN(stderr, 200))
	}
}

// parseFlags по-прежнему поднимает флаги из хвоста: без hoistFlags команда
// приняла бы --json за часть запроса и напечатала текст вместо JSON.
func TestHoistingStillAppliedAfterReport(t *testing.T) {
	setFlagReportEnv(t)

	stdout, stderr, code := runMainSplit(t, "poolsearch", "library", "--json", "--limit", "1")
	if code != 0 {
		t.Fatalf("код %d, stderr %s", code, firstN(stderr, 400))
	}
	trimmed := strings.TrimSpace(stdout)
	if !strings.HasPrefix(trimmed, "[") && !strings.HasPrefix(trimmed, "{") {
		t.Errorf("stdout не похож на JSON: %s", firstN(stdout, 200))
	}
	var decoded any
	if err := json.Unmarshal([]byte(trimmed), &decoded); err != nil {
		t.Errorf("stdout не разбирается как JSON: %v (%s)", err, firstN(stdout, 200))
	}
}
