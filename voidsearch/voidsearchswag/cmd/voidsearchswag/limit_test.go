package main

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// Замер до правки на HEAD 3e7d426. Флаг --limit команды search не проверялся, а
// search.Engine подменял любое неположительное значение на DefaultN:
//
//	search --no-tor --no-cache --limit 1  --json "leak database"  rc=0, limit=1,  count=1
//	search --no-tor --no-cache --limit 0  --json "leak database"  rc=0, limit=20, count=10
//	search --no-tor --no-cache --limit -5 --json "leak database"  rc=0, limit=20, count=10
//
// На уровне ядра то же: Options.Limit=0, -5 и -1000 дают Outcome.Limit=20.
func TestCheckLimitRejectsNonPositive(t *testing.T) {
	for _, n := range []int{0, -1, -5, -1000} {
		err := checkLimit("limit", n)
		if err == nil {
			t.Errorf("--limit %d принят, хочу ошибку", n)
			continue
		}
		want := "--limit должен быть положительным, получено " + strconv.Itoa(n)
		if err.Error() != want {
			t.Errorf("--limit %d: %q, хочу %q", n, err.Error(), want)
		}
	}
}

func TestCheckLimitAcceptsPositive(t *testing.T) {
	for _, n := range []int{1, 2, 20, 400, maxFlagLimit} {
		if err := checkLimit("limit", n); err != nil {
			t.Errorf("--limit %d отклонён: %v", n, err)
		}
	}
}

func TestCheckLimitRejectsAboveCeiling(t *testing.T) {
	for _, n := range []int{maxFlagLimit + 1, 100000, 1 << 30} {
		err := checkLimit("limit", n)
		if err == nil {
			t.Errorf("--limit %d принят, хочу ошибку о пределе", n)
			continue
		}
		if !strings.Contains(err.Error(), "предел 10000") {
			t.Errorf("--limit %d: %q, хочу упоминание предела", n, err.Error())
		}
	}
}

// Имя флага обязано попадать в текст: --limit есть у семи команд, и сообщение без
// имени не говорит, какой именно флаг виноват.
func TestCheckLimitNamesFlag(t *testing.T) {
	err := checkLimit("max-hosts", 0)
	if err == nil {
		t.Fatal("ноль принят")
	}
	if !strings.Contains(err.Error(), "--max-hosts") {
		t.Errorf("сообщение не называет флаг: %q", err.Error())
	}
}

func TestValidLimitReturnsValueUnchanged(t *testing.T) {
	for _, n := range []int{1, 7, 20, maxFlagLimit} {
		if got := validLimit("limit", n); got != n {
			t.Errorf("validLimit(%d) = %d", n, got)
		}
	}
}

// Прогон через main: проверка обязана срабатывать в настоящей команде, а не
// только в вызове функции, и срабатывать до конфига, базы и сети. Ноль в списке
// значений не участвует: с этапа 144 он означает «взять ResultLimit из конфига» и
// проверен отдельным тестом.
func TestSearchRejectsNegativeLimitInRealRun(t *testing.T) {
	t.Setenv("VOIDSEARCH_DATA_DIR", t.TempDir())
	t.Setenv("VOIDSEARCH_BACKUP_DIR", "")
	t.Setenv("VOIDSEARCH_TOR", "off")
	t.Setenv("VOIDSEARCH_TRANSPORT", "direct")

	for _, value := range []string{"-5", "-1"} {
		_, stderr, code := runMainSplit(t, "search", "--no-tor", "--limit", value, "запрос")
		if code == 0 {
			t.Errorf("--limit %s: прогон вернул ноль, хочу отказ", value)
		}
		if !strings.Contains(stderr, "--limit должен быть положительным, получено "+value) {
			t.Errorf("--limit %s: stderr не называет причину, фактически %q", value, stderr)
		}
	}
}

// Ноль у флага количества с этапа 144 означает «взять ResultLimit из конфига»,
// а не «отказать»: до правки команда не могла отличить незаданный флаг от
// заданного двадцатью, потому что дефолт флага совпадал с envDefault конфига, и
// переменная окружения не действовала вовсе.
func TestSearchZeroLimitTakesConfigValue(t *testing.T) {
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	t.Setenv("VOIDSEARCH_REQUEST_TIMEOUT", "2s")
	t.Setenv("VOIDSEARCH_RESULT_LIMIT", "7")

	// --no-cache обязателен: без него второй прогон берёт кэш первого и
	// печатает его limit, а не применённый сейчас. Так мутация дефолта флага
	// один раз прошла бы незамеченной во втором случае.
	for _, args := range [][]string{
		{"search", "--no-tor", "--no-cache", "--json", "test"},
		{"search", "--no-tor", "--no-cache", "--json", "--limit", "0", "test"},
	} {
		stdout, stderr, code := runMainSplit(t, args...)
		if code != 0 {
			t.Fatalf("%s: код возврата %d, stderr %q", strings.Join(args, " "), code, stderr)
		}
		var rep struct {
			Limit int `json:"limit"`
		}
		if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
			t.Fatalf("%s: отчёт не JSON: %v, вывод: %s", strings.Join(args, " "), err, firstN(stdout, 300))
		}
		if rep.Limit != 7 {
			t.Errorf("%s: limit=%d, хочу 7 из VOIDSEARCH_RESULT_LIMIT", strings.Join(args, " "), rep.Limit)
		}
	}
}

// Флаг обязан перебивать конфиг, иначе переменная окружения стала бы потолком, а
// не значением по умолчанию.
func TestSearchFlagLimitOverridesConfigValue(t *testing.T) {
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	t.Setenv("VOIDSEARCH_REQUEST_TIMEOUT", "2s")
	t.Setenv("VOIDSEARCH_RESULT_LIMIT", "300")

	stdout, stderr, code := runMainSplit(t, "search", "--no-tor", "--no-cache", "--json", "--limit", "11", "test")
	if code != 0 {
		t.Fatalf("код возврата %d, stderr %q", code, stderr)
	}
	var rep struct {
		Limit int `json:"limit"`
	}
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("отчёт не JSON: %v, вывод: %s", err, firstN(stdout, 300))
	}
	if rep.Limit != 11 {
		t.Errorf("limit=%d, хочу 11 из флага, а не 300 из конфига", rep.Limit)
	}
}

func TestSearchRejectsHugeLimitInRealRun(t *testing.T) {
	t.Setenv("VOIDSEARCH_DATA_DIR", t.TempDir())
	t.Setenv("VOIDSEARCH_BACKUP_DIR", "")
	t.Setenv("VOIDSEARCH_TOR", "off")
	t.Setenv("VOIDSEARCH_TRANSPORT", "direct")

	_, stderr, code := runMainSplit(t, "search", "--no-tor", "--limit", "100001", "запрос")
	if code == 0 {
		t.Fatal("--limit 100001: прогон вернул ноль, хочу отказ")
	}
	if !strings.Contains(stderr, "слишком большой: 100001") {
		t.Errorf("stderr не называет значение и предел, фактически %q", stderr)
	}
}

// Положительное значение проверка пропускает: прогон обязан дойти до конфига и
// упасть на мусорном таймауте, а не на --limit.
func TestSearchPositiveLimitPassesValidation(t *testing.T) {
	t.Setenv("VOIDSEARCH_DATA_DIR", t.TempDir())
	t.Setenv("VOIDSEARCH_BACKUP_DIR", "")
	t.Setenv("VOIDSEARCH_TOR", "off")
	t.Setenv("VOIDSEARCH_TRANSPORT", "direct")
	t.Setenv("VOIDSEARCH_REQUEST_TIMEOUT", "не-длительность")

	_, stderr, code := runMainSplit(t, "search", "--no-tor", "--limit", "5", "запрос")
	if code == 0 {
		t.Fatal("прогон с мусорным таймаутом вернул ноль: ранний отказ не сработал")
	}
	if strings.Contains(stderr, "--limit") {
		t.Errorf("рабочий --limit 5 вызвал предупреждение: %q", stderr)
	}
	if !strings.Contains(stderr, "конфиг") {
		t.Errorf("прогон не дошёл до конфига, фактически %q", stderr)
	}
}
