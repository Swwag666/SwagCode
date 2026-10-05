package main

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// Живой замер до правки на HEAD 5810aca, пересобранный бинарник, копия боевой
// базы, promote --limit N --no-tor --json:
//
//	1   rc=1, checked=3,   failed=3,   promoted=0, 1017мс
//	2   rc=1, checked=6,   failed=6,   promoted=0, 1023мс
//	3   rc=1, checked=9,   failed=9,   promoted=0, 1020мс
//	10  rc=1, checked=30,  failed=30,  promoted=0, 1017мс
//	50  rc=1, checked=150, failed=150, promoted=0, 1015мс
//
// Ровно втрое больше запрошенного в каждом прогоне. Справка флага обещает
// «сколько живых сервисов проверить», а условие остановки цикла считало поднятые
// сиды: пока ничего не поднялось, цикл шёл до конца выборки, а выборка бралась
// втрое больше предела. Каждая лишняя проверка - обращение к живому onion-адресу
// через tor и строка в бюджете прогона.
func TestPromoteStopsAtRequestedChecks(t *testing.T) {
	const seeded = 5
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	seedLiveHosts(t, dir, seeded)

	for _, value := range []string{"1", "2", "3", "5"} {
		asked, err := strconv.Atoi(value)
		if err != nil {
			t.Fatalf("в тесте опечатка: %v", err)
		}
		stdout, stderr, _ := runMainSplit(t, "promote", "--limit", value, "--no-tor", "--json")
		if strings.Contains(stderr, "--limit") {
			t.Errorf("--limit %s: рабочее значение вызвало предупреждение %q", value, stderr)
		}
		var rep struct {
			Checked  int `json:"checked"`
			Failed   int `json:"failed"`
			Promoted []struct {
				Base string `json:"base"`
			} `json:"promoted"`
		}
		if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
			t.Fatalf("--limit %s: разбор JSON: %v (%q)", value, err, firstN(stdout, 300))
		}
		if rep.Checked != asked {
			t.Errorf("--limit %s: отчёт несёт checked=%d, хочу %d: проверка обязана быть ровно одна на запрошенный адрес",
				value, rep.Checked, asked)
		}
		if rep.Checked > asked {
			t.Errorf("--limit %s: сделано %d проверок вместо %d", value, rep.Checked, asked)
		}
		if rep.Failed != rep.Checked {
			t.Errorf("--limit %s: без tor отказов %d при %d проверках, хочу по одному на проверку",
				value, rep.Failed, rep.Checked)
		}
		if len(rep.Promoted) != 0 {
			t.Errorf("--limit %s: без tor поднято %d сидов, хочу ноль", value, len(rep.Promoted))
		}
	}
}

// На пуле длиннее запаса выборки прежний код проверял всю выборку: двенадцать
// адресов при --limit 4. Проверок обязано быть ровно четыре.
func TestPromoteKeepsChecksWithinLimitOnBigPool(t *testing.T) {
	const seeded = 12
	const asked = 4
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	seedLiveHosts(t, dir, seeded)

	stdout, stderr, _ := runMainSplit(t, "promote", "--limit", strconv.Itoa(asked), "--no-tor", "--json")
	if strings.Contains(stderr, "--limit") {
		t.Errorf("рабочее значение вызвало предупреждение %q", stderr)
	}
	var rep struct {
		Checked int `json:"checked"`
	}
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("разбор JSON: %v (%q)", err, firstN(stdout, 300))
	}
	if rep.Checked != asked {
		t.Errorf("отчёт несёт checked=%d, хочу %d: пул из %d адресов не повод проверять больше",
			rep.Checked, asked, seeded)
	}
}

// Текстовый отчёт называет то же число проверок, что и машинный: строка
// «проверено N» обязана совпадать с запрошенным пределом, когда пул длиннее.
func TestPromoteTextNamesRequestedCheckCount(t *testing.T) {
	const seeded = 9
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	seedLiveHosts(t, dir, seeded)

	stdout, _, _ := runMainSplit(t, "promote", "--limit", "3", "--no-tor")
	want := "проверено 3, поднято 0"
	if !strings.Contains(stdout, want) {
		t.Errorf("в выводе нет %q, фактически:\n%s", want, firstN(stdout, 400))
	}
	if strings.Contains(stdout, "проверено 9") {
		t.Errorf("отчёт назвал весь пул вместо запрошенных трёх проверок:\n%s", firstN(stdout, 400))
	}
}
