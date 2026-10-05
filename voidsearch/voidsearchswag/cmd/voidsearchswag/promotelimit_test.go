package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"voidsearchswag/internal/config"
	"voidsearchswag/internal/store"
)

// seedLiveHosts пишет в пул n адресов со статусом live: именно их выбирает
// promote, и без них checked равен нулю при любом пределе. Хост обязан быть
// настоящим base32-адресом длиной 16 символов, иначе список живых окажется
// короче посеянного.
func seedLiveHosts(t *testing.T, dir string, n int) {
	t.Helper()
	st, err := store.Open(filepath.Join(dir, "voidsearchswag.db"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		host := "live" + base32Tail(i, 12)
		if len(host) != 16 {
			t.Fatalf("в тесте опечатка: хост %q длиной %d, хочу 16", host, len(host))
		}
		o := store.Onion{
			URL:    "http://" + host + ".onion/",
			Title:  fmt.Sprintf("live host %d", i),
			Status: "live",
		}
		if err := st.UpsertOnion(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.ListOnions(ctx, "live", 500)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != n {
		t.Fatalf("в пуле %d живых адресов, хочу %d", len(got), n)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
}

// Живой замер до правки на HEAD 435cee1, пересобранный бинарник, копия боевой
// базы, --no-tor, --json:
//
//	--limit 0    rc=1, checked=30,  promoted=0, failed=30
//	--limit -5   rc=1, checked=30,  promoted=0, failed=30
//	--limit 3    rc=1, checked=9,   promoted=0, failed=9
//	--limit 500  rc=1, checked=150, promoted=0, failed=150
//
// stderr пуст во всех четырёх прогонах, и ни один отчёт не назвал подмену: ноль
// и минус пять превращались в десять, пятьсот - в пятьдесят, а число сетевых
// обращений было втрое больше. Оператор, попросивший три проверки, получал девять
// обращений к живым адресам и девять записей отказов, не зная об этом.
// Ноль в списке значений не участвует: он означает «взять PromoteLimit из конфига»
// и проверен в promotelimitconfig_test.go.
func TestPromoteRejectsNegativeLimitInRealRun(t *testing.T) {
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	seedLiveHosts(t, dir, 5)

	for _, value := range []string{"-5", "-1000"} {
		stdout, stderr, code := runMainSplit(t, "promote", "--limit", value, "--no-tor", "--json")
		want := "--limit должен быть положительным, получено " + value
		if !strings.Contains(stderr, want) {
			t.Errorf("--limit %s: stderr не называет причину, хочу %q, фактически %q", value, want, stderr)
		}
		if code != 1 {
			t.Errorf("--limit %s: код возврата %d, хочу 1", value, code)
		}
		if strings.Contains(stdout, "checked") {
			t.Errorf("--limit %s: прогон состоялся несмотря на отказ: %s", value, firstN(stdout, 200))
		}
	}
}

func TestPromoteRejectsLimitAboveOwnCeilingInRealRun(t *testing.T) {
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	seedLiveHosts(t, dir, 5)

	for _, value := range []string{"201", "500", "20000"} {
		stdout, stderr, code := runMainSplit(t, "promote", "--limit", value, "--no-tor", "--json")
		want := "--limit слишком большой: " + value + " (предел " + strconv.Itoa(maxPromoteLimit) + ")"
		if !strings.Contains(stderr, want) {
			t.Errorf("--limit %s: stderr не называет значение и предел, хочу %q, фактически %q", value, want, stderr)
		}
		if code != 1 {
			t.Errorf("--limit %s: код возврата %d, хочу 1", value, code)
		}
		if strings.Contains(stdout, "checked") {
			t.Errorf("--limit %s: прогон состоялся несмотря на отказ: %s", value, firstN(stdout, 200))
		}
	}
}

// Потолок promote ниже общего предела флагов и равен верхней границе, которую
// config.Validate держит для PromoteLimit: одна величина не может быть законной из
// переменной окружения и незаконной из флага. До правки здесь стояло пятьдесят, и
// живой замер на HEAD 5cd38c0 дал --limit 200 с отказом «предел 50» при том, что
// конфиг принимал двести молча.
func TestPromoteLimitCeilingMatchesConfigRule(t *testing.T) {
	if maxPromoteLimit != 200 {
		t.Errorf("maxPromoteLimit = %d, хочу 200: такова верхняя граница PromoteLimit в config.Validate",
			maxPromoteLimit)
	}
	if maxPromoteLimit >= maxFlagLimit {
		t.Errorf("потолок promote %d не ниже общего %d", maxPromoteLimit, maxFlagLimit)
	}
	if err := checkLimitCeiling("limit", maxPromoteLimit, maxPromoteLimit); err != nil {
		t.Errorf("потолок команды отклонён: %v", err)
	}
	err := checkLimitCeiling("limit", maxPromoteLimit+1, maxPromoteLimit)
	if err == nil {
		t.Fatal("значение на единицу выше потолка принято")
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("предел %d", maxPromoteLimit)) {
		t.Errorf("отказ не называет предел: %v", err)
	}

	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	t.Setenv("VOIDSEARCH_BACKUP_DIR", "")

	t.Setenv("VOIDSEARCH_PROMOTE_LIMIT", strconv.Itoa(maxPromoteLimit))
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("конфиг отверг %d, хотя флаг его допускает: %v", maxPromoteLimit, err)
	}
	if cfg.PromoteLimit != maxPromoteLimit {
		t.Errorf("конфиг дал PromoteLimit=%d при заданном %d: граница разошлась с флагом",
			cfg.PromoteLimit, maxPromoteLimit)
	}
}

// Рабочее значение доходит до прогона, и число проверок не превышает пул: на пяти
// живых адресах --limit 50 обязан дать ровно пять проверок, а не пятьдесят.
func TestPromoteChecksWholePoolWhenPoolIsShorter(t *testing.T) {
	const seeded = 5
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	seedLiveHosts(t, dir, seeded)

	stdout, stderr, _ := runMainSplit(t, "promote", "--limit", "50", "--no-tor", "--json")
	if strings.Contains(stderr, "--limit") {
		t.Errorf("рабочее значение вызвало предупреждение %q", stderr)
	}
	var rep struct {
		Checked    int `json:"checked"`
		Failed     int `json:"failed"`
		SaveFailed int `json:"save_failed"`
		Promoted   []struct {
			Base string `json:"base"`
		} `json:"promoted"`
	}
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("разбор JSON: %v (%q)", err, firstN(stdout, 300))
	}
	if rep.Checked != seeded {
		t.Errorf("отчёт несёт checked=%d, хочу %d: пул короче предела", rep.Checked, seeded)
	}
	if rep.Failed != seeded {
		t.Errorf("без tor отказов %d из %d проверок, хочу по одному на адрес", rep.Failed, rep.Checked)
	}
	if len(rep.Promoted) != 0 {
		t.Errorf("без tor поднято %d сидов, хочу ноль", len(rep.Promoted))
	}
	if rep.SaveFailed != 0 {
		t.Errorf("save_failed = %d, хочу ноль: сохранять было нечего", rep.SaveFailed)
	}
}
