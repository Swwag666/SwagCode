package main

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"voidsearchswag/internal/store"
)

// Замер до правки на HEAD e054dc4, копия боевой базы, история наполнена через
// временную программу: 150 строк, затем 1500 строк. Команда печатала в отчёт
// заявленное значение, а хранилище приводило его к своему диапазону:
//
//	hunt hits --limit 50   --json  rc=0, limit=50,    count=50
//	hunt hits --limit 150  --json  rc=0, limit=150,   count=150
//	hunt hits --limit 0    --json  rc=0, limit=0,     count=100
//	hunt hits --limit -5   --json  rc=0, limit=-5,    count=100
//	hunt hits --limit -1000 --json rc=0, limit=-1000, count=100
//	hunt hits --limit 5000 --json  rc=0, limit=5000,  count=1000   (в истории 1500)
//	hunt hits --limit 1001 --json  rc=0, limit=1001,  count=1000   (в истории 1500)
//
// Ноль и отрицательные значения ListHuntHits подменял сотней, значения свыше
// тысячи обрезал, и ни одно из четырёх расхождений не доходило ни до stderr, ни
// до отчёта: поле limit утверждало одно, count показывал другое.
func TestCheckLimitCeilingRejectsNonPositive(t *testing.T) {
	for _, n := range []int{0, -1, -5, -1000} {
		err := checkLimitCeiling("limit", n, store.MaxHuntHitsLimit)
		if err == nil {
			t.Errorf("--limit %d принят, хочу ошибку", n)
			continue
		}
		if !strings.Contains(err.Error(), "--limit должен быть положительным") {
			t.Errorf("--limit %d: %q, хочу причину про положительное значение", n, err.Error())
		}
	}
}

func TestCheckLimitCeilingRejectsAboveCeiling(t *testing.T) {
	for _, n := range []int{store.MaxHuntHitsLimit + 1, 5000, maxFlagLimit} {
		err := checkLimitCeiling("limit", n, store.MaxHuntHitsLimit)
		if err == nil {
			t.Errorf("--limit %d принят, хочу ошибку о пределе хранилища", n)
			continue
		}
		if !strings.Contains(err.Error(), "предел 1000") {
			t.Errorf("--limit %d: %q, хочу упоминание предела 1000", n, err.Error())
		}
	}
}

func TestCheckLimitCeilingAcceptsUpToCeiling(t *testing.T) {
	for _, n := range []int{1, 50, 999, store.MaxHuntHitsLimit} {
		if err := checkLimitCeiling("limit", n, store.MaxHuntHitsLimit); err != nil {
			t.Errorf("--limit %d отклонён: %v", n, err)
		}
	}
}

func TestValidLimitCeilingReturnsValue(t *testing.T) {
	for _, n := range []int{1, 7, store.MaxHuntHitsLimit} {
		if got := validLimitCeiling("limit", n, store.MaxHuntHitsLimit); got != n {
			t.Errorf("validLimitCeiling(%d) = %d", n, got)
		}
	}
}

// Общий потолок флага количества остался прежним: проверка search не должна
// зависеть от того, что у истории находок свой предел.
func TestCheckLimitKeepsOwnCeiling(t *testing.T) {
	if err := checkLimit("limit", 5000); err != nil {
		t.Errorf("--limit 5000 отклонён общей проверкой: %v", err)
	}
	if err := checkLimit("limit", maxFlagLimit+1); err == nil {
		t.Error("--limit сверх общего предела принят")
	}
}

// Потолок CLI обязан совпадать с потолком хранилища, иначе расхождение вернётся.
func TestHuntHitsCeilingIsStoreCeiling(t *testing.T) {
	if store.MaxHuntHitsLimit != 1000 {
		t.Errorf("store.MaxHuntHitsLimit = %d, хочу 1000: замер до правки снят на этом числе", store.MaxHuntHitsLimit)
	}
}

func TestHuntHitsRejectsNonPositiveLimitInRealRun(t *testing.T) {
	t.Setenv("VOIDSEARCH_DATA_DIR", t.TempDir())
	t.Setenv("VOIDSEARCH_BACKUP_DIR", "")
	t.Setenv("VOIDSEARCH_TOR", "off")
	t.Setenv("VOIDSEARCH_TRANSPORT", "direct")

	for _, value := range []string{"0", "-5", "-1000"} {
		_, stderr, code := runMainSplit(t, "hunt", "hits", "--limit", value, "--json")
		if code == 0 {
			t.Errorf("--limit %s: прогон вернул ноль, хочу отказ", value)
		}
		if !strings.Contains(stderr, "--limit должен быть положительным, получено "+value) {
			t.Errorf("--limit %s: stderr не называет причину, фактически %q", value, stderr)
		}
	}
}

func TestHuntHitsRejectsLimitAboveStoreCeilingInRealRun(t *testing.T) {
	t.Setenv("VOIDSEARCH_DATA_DIR", t.TempDir())
	t.Setenv("VOIDSEARCH_BACKUP_DIR", "")
	t.Setenv("VOIDSEARCH_TOR", "off")
	t.Setenv("VOIDSEARCH_TRANSPORT", "direct")

	for _, value := range []string{"1001", "5000"} {
		_, stderr, code := runMainSplit(t, "hunt", "hits", "--limit", value, "--json")
		if code == 0 {
			t.Errorf("--limit %s: прогон вернул ноль, хочу отказ", value)
		}
		if !strings.Contains(stderr, "слишком большой: "+value+" (предел 1000)") {
			t.Errorf("--limit %s: stderr не называет значение и предел, фактически %q", value, stderr)
		}
	}
}

// Рабочие значения проходят проверку, а отчёт несёт то число, которое реально
// ушло в запрос: до правки limit в JSON и фактическая выборка расходились.
func TestHuntHitsReportsCheckedLimitInRealRun(t *testing.T) {
	t.Setenv("VOIDSEARCH_DATA_DIR", t.TempDir())
	t.Setenv("VOIDSEARCH_BACKUP_DIR", "")
	t.Setenv("VOIDSEARCH_TOR", "off")
	t.Setenv("VOIDSEARCH_TRANSPORT", "direct")

	for _, value := range []string{"1", "7", "1000"} {
		want, err := strconv.Atoi(value)
		if err != nil {
			t.Fatalf("в тесте опечатка: %v", err)
		}
		stdout, stderr, code := runMainSplit(t, "hunt", "hits", "--limit", value, "--json")
		if code != 0 {
			t.Fatalf("--limit %s: прогон вернул %d, stderr %q", value, code, stderr)
		}
		if strings.Contains(stderr, "--limit") {
			t.Errorf("--limit %s: рабочее значение вызвало предупреждение %q", value, stderr)
		}
		var out struct {
			Limit int `json:"limit"`
			Count int `json:"count"`
		}
		if err := json.Unmarshal([]byte(stdout), &out); err != nil {
			t.Fatalf("--limit %s: разбор JSON: %v (%q)", value, err, stdout)
		}
		if out.Limit != want {
			t.Errorf("--limit %s: отчёт несёт limit=%d, хочу %d", value, out.Limit, want)
		}
		if out.Count > out.Limit {
			t.Errorf("--limit %s: строк %d больше заявленных %d", value, out.Count, out.Limit)
		}
	}
}
