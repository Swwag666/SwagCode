package main

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// Живой замер до правки на HEAD 5cd38c0, пересобранный бинарник, копия боевой базы,
// --no-tor, --json, --budget 4s:
//
//	VOIDSEARCH_PROMOTE_LIMIT=3 promote --no-tor --json --budget 4s
//	    rc=1, checked=10, promoted=0, failed=10
//	promote --no-tor --json --budget 4s (без переменной окружения)
//	    rc=1, checked=10, promoted=0, failed=10
//	promote ... --limit 0
//	    rc=1, ERROR: --limit должен быть положительным, получено 0
//	promote ... --limit 200
//	    rc=1, ERROR: --limit слишком большой: 200 (предел 50)
//	promote ... --limit 4
//	    rc=1, checked=4, promoted=0, failed=4
//	promote -h
//	    -limit int
//	        сколько живых сервисов проверить (default 10)
//
// Дефолт флага десять совпадал с envDefault поля PromoteLimit, поэтому переменная
// окружения не действовала вовсе, ноль отвергался, а потолок флага в пятьдесят
// расходился с конфиг-потолком в двести.
//
// Пул во всех тестах ниже длиннее любого проверяемого предела: на коротком пуле
// число проверок упиралось бы в число адресов, и подмену дефолта не было бы видно.
func promoteChecked(t *testing.T, args ...string) int {
	t.Helper()
	full := append([]string{"promote", "--no-tor", "--json", "--budget", "20s"}, args...)
	stdout, stderr, code := runMainSplit(t, full...)
	// Единица здесь штатная: на недоступных кандидатах ничего не поднимается, и
	// promoteExitCode возвращает отказ при нуле поднятых сидов. Важен не код, а
	// число состоявшихся проверок.
	if code != 1 {
		t.Fatalf("%s: код возврата %d, хочу 1 при недоступных кандидатах, stderr: %s",
			strings.Join(full, " "), code, firstN(stderr, 300))
	}
	var body struct {
		Checked int `json:"checked"`
	}
	if err := json.Unmarshal([]byte(stdout), &body); err != nil {
		t.Fatalf("%s: отчёт promote не JSON: %v, вывод: %s",
			strings.Join(full, " "), err, firstN(stdout, 300))
	}
	return body.Checked
}

const promotePoolSize = 12

func TestPromoteLimitTakesConfigValue(t *testing.T) {
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	seedLiveHosts(t, dir, promotePoolSize)
	t.Setenv("VOIDSEARCH_PROMOTE_LIMIT", "3")

	if got := promoteChecked(t); got != 3 {
		t.Errorf("checked = %d, хочу 3 из VOIDSEARCH_PROMOTE_LIMIT", got)
	}
}

func TestPromoteLimitZeroTakesConfigValue(t *testing.T) {
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	seedLiveHosts(t, dir, promotePoolSize)
	t.Setenv("VOIDSEARCH_PROMOTE_LIMIT", "4")

	if got := promoteChecked(t, "--limit", "0"); got != 4 {
		t.Errorf("checked = %d, хочу 4 из конфига при явном нуле флага", got)
	}
}

func TestPromoteLimitFlagOverridesConfigValue(t *testing.T) {
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	seedLiveHosts(t, dir, promotePoolSize)
	t.Setenv("VOIDSEARCH_PROMOTE_LIMIT", "3")

	if got := promoteChecked(t, "--limit", "2"); got != 2 {
		t.Errorf("checked = %d, хочу 2 из флага, а не 3 из конфига", got)
	}
}

// Без переменной окружения команда берёт envDefault конфига, десять проверок, а не
// весь пул и не ноль.
func TestPromoteLimitWithoutEnvTakesConfigDefault(t *testing.T) {
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	seedLiveHosts(t, dir, promotePoolSize)

	if got := promoteChecked(t); got != 10 {
		t.Errorf("checked = %d, хочу 10 из envDefault конфига на пуле из %d адресов",
			got, promotePoolSize)
	}
}

// Значение на границе нового потолка обязано доходить до прогона: до правки флаг
// отклонял всё, что больше пятидесяти, хотя конфиг принимал до двухсот.
func TestPromoteAcceptsLimitAtConfigCeiling(t *testing.T) {
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	seedLiveHosts(t, dir, promotePoolSize)

	if got := promoteChecked(t, "--limit", strconv.Itoa(maxPromoteLimit)); got != promotePoolSize {
		t.Errorf("checked = %d, хочу %d - весь пул при пределе %d", got, promotePoolSize, maxPromoteLimit)
	}
}

func TestPromoteHelpNamesLimitConfigRule(t *testing.T) {
	_, stderr, _ := runMainSplit(t, "promote", "-h")
	want := "сколько живых сервисов проверить, не больше 200 (0 - значение из конфига)"
	if !strings.Contains(stderr, want) {
		t.Errorf("справка promote не описывает правило нуля и потолок целиком, хочу %q, фактически:\n%s",
			want, firstN(stderr, 1200))
	}
	if strings.Contains(stderr, "(default 10)") {
		t.Errorf("справка promote всё ещё печатает дефолт десять:\n%s", firstN(stderr, 1200))
	}
}
