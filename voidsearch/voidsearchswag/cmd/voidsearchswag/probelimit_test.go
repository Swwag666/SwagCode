package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"voidsearchswag/internal/store"
)

// Замер до правки на HEAD 330ebde, пересобранный бинарник, копия боевой базы,
// tor выключен, транспорт direct, probe --limit N --json --timeout 2m:
//
//	3     rc=1, total=3,    live=0, skipped=3
//	20    rc=1, total=20,   live=0, skipped=20
//	0     rc=1, total=50,   live=0, skipped=50
//	-5    rc=1, total=50,   live=0, skipped=50
//	5001  rc=1, total=5000, live=0, skipped=5000
//	20000 rc=1, total=5000, live=0, skipped=5000
//
// stderr пуст во всех шести прогонах: ни подмена нулевого значения дефолтом 50,
// ни обрезка 20000 до 5000 не были видны. Код возврата 1 объясняется
// пропущенными пробами, поэтому по нему отказ от рабочего значения отличить
// нельзя - предметом проверки является stderr и наличие отчёта в stdout.
func TestProbeCeilingIsStoreCeiling(t *testing.T) {
	if store.MaxStoreLimit != 5000 {
		t.Errorf("store.MaxStoreLimit = %d, хочу 5000: замер до правки снят на этом числе",
			store.MaxStoreLimit)
	}
	if store.MaxStoreLimit >= maxFlagLimit {
		t.Errorf("потолок хранилища %d не ниже общего потолка флагов %d",
			store.MaxStoreLimit, maxFlagLimit)
	}
}

// base32Tail кодирует число в n символов алфавита base32: цифры 0, 1, 8 и 9 в
// onion-адресе недопустимы, и HostOf отбрасывает такой хост ещё до пробы.
func base32Tail(i, n int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz234567"
	b := make([]byte, n)
	for k := n - 1; k >= 0; k-- {
		b[k] = alphabet[i%32]
		i /= 32
	}
	return string(b)
}

// seedUnknown пишет в пул n адресов со статусом unknown: именно их выбирает
// NextProbeWave первыми, и без них total равен нулю при любом пределе. Хост
// обязан быть настоящим base32 v2-адресом длиной 16 символов, иначе волна
// проб отбросит его и отчёт покажет ноль адресов при полном пуле.
func seedUnknown(t *testing.T, dir string, n int) {
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
		host := "unkn" + base32Tail(i, 12)
		if len(host) != 16 {
			t.Fatalf("в тесте опечатка: хост %q длиной %d, хочу 16", host, len(host))
		}
		o := store.Onion{
			URL:    "http://" + host + ".onion/",
			Title:  fmt.Sprintf("unknown host %d", i),
			Status: "unknown",
		}
		if err := st.UpsertOnion(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestProbeRejectsNonPositiveLimitInRealRun(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	t.Setenv("VOIDSEARCH_BACKUP_DIR", "")
	t.Setenv("VOIDSEARCH_TOR", "off")
	t.Setenv("VOIDSEARCH_TRANSPORT", "direct")
	seedUnknown(t, dir, 10)

	for _, value := range []string{"0", "-5", "-1000"} {
		stdout, stderr, _ := runMainSplit(t, "probe", "--limit", value, "--json")
		want := "--limit должен быть положительным, получено " + value
		if !strings.Contains(stderr, want) {
			t.Errorf("--limit %s: stderr не называет причину, хочу %q, фактически %q", value, want, stderr)
		}
		if strings.Contains(stdout, "total") {
			t.Errorf("--limit %s: отчёт напечатан несмотря на отказ: %s", value, firstN(stdout, 200))
		}
	}
}

func TestProbeRejectsLimitAboveStoreCeilingInRealRun(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	t.Setenv("VOIDSEARCH_BACKUP_DIR", "")
	t.Setenv("VOIDSEARCH_TOR", "off")
	t.Setenv("VOIDSEARCH_TRANSPORT", "direct")
	seedUnknown(t, dir, 10)

	for _, value := range []string{"5001", "20000"} {
		stdout, stderr, _ := runMainSplit(t, "probe", "--limit", value, "--json")
		want := "слишком большой: " + value + " (предел " + strconv.Itoa(store.MaxStoreLimit) + ")"
		if !strings.Contains(stderr, want) {
			t.Errorf("--limit %s: stderr не называет значение и предел, хочу %q, фактически %q", value, want, stderr)
		}
		if strings.Contains(stdout, "total") {
			t.Errorf("--limit %s: отчёт напечатан несмотря на отказ: %s", value, firstN(stdout, 200))
		}
	}
}

// Рабочие значения проходят проверку и попадают в отчёт без изменений: до правки
// ноль превращался в 50, а теперь total равен тому числу, которое попросили, если
// в пуле столько непроверенных адресов. Значение выше размера пула отдаёт весь
// пул: предел ограничивает выборку, а не дополняет её.
func TestProbeReportsRequestedLimitInRealRun(t *testing.T) {
	const seeded = 10
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	t.Setenv("VOIDSEARCH_BACKUP_DIR", "")
	t.Setenv("VOIDSEARCH_TOR", "off")
	t.Setenv("VOIDSEARCH_TRANSPORT", "direct")
	seedUnknown(t, dir, seeded)

	for _, value := range []string{"1", "3", "10", "5000"} {
		asked, err := strconv.Atoi(value)
		if err != nil {
			t.Fatalf("в тесте опечатка: %v", err)
		}
		want := asked
		if want > seeded {
			want = seeded
		}
		stdout, stderr, _ := runMainSplit(t, "probe", "--limit", value, "--json")
		if strings.Contains(stderr, "--limit") {
			t.Errorf("--limit %s: рабочее значение вызвало предупреждение %q", value, stderr)
		}
		var rep struct {
			Total   int `json:"total"`
			Skipped int `json:"skipped"`
		}
		if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
			t.Fatalf("--limit %s: разбор JSON: %v (%q)", value, err, firstN(stdout, 200))
		}
		if rep.Total != want {
			t.Errorf("--limit %s: отчёт несёт total=%d, хочу %d (в пуле %d адресов)",
				value, rep.Total, want, seeded)
		}
		if rep.Total > asked {
			t.Errorf("--limit %s: отчёт несёт total=%d, больше запрошенного", value, rep.Total)
		}
		if rep.Skipped != rep.Total {
			t.Errorf("--limit %s: без tor пропущено %d из %d, хочу все", value, rep.Skipped, rep.Total)
		}
	}
}

// Предел хранилища обязан быть достижим: если проверка начнёт отклонять само
// число 5000, команда потеряет законную верхнюю границу.
func TestProbeAcceptsStoreCeilingItself(t *testing.T) {
	if err := checkLimitCeiling("limit", store.MaxStoreLimit, store.MaxStoreLimit); err != nil {
		t.Errorf("потолок хранилища отклонён: %v", err)
	}
	if err := checkLimitCeiling("limit", store.MaxStoreLimit+1, store.MaxStoreLimit); err == nil {
		t.Error("значение на единицу выше потолка принято")
	}
}
