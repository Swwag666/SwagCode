package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"voidsearchswag/internal/store"
)

// Живой замер до правки на HEAD e408b91, пересобранный бинарник, копия боевой
// базы, tor выключен, транспорт direct, collect --limit N --delay 1ms --json:
//
//	2   rc=0, ключи [hosts,pages,links,saved,skipped,failed,elapsed], hosts=2
//	20  rc=0, hosts=20
//	0   rc=0, hosts=100
//	-5  rc=0, hosts=100
//
// stderr пуст во всех четырёх прогонах. Ноль и отрицательное значение
// превращались в сотню обходов: discover.Known уходит в ListOnions, а тот
// подставляет дефолт 100 вместо незаданного значения. Каждый обход без tor
// заканчивался отказом и записью failed в пул, то есть мусорное значение флага
// портило накопительную статистику живости сотни адресов, и отчёт об этом не
// говорил ни словом. Значения свыше пяти тысяч обрезались так же молча.
func TestCollectRejectsNonPositiveLimitInRealRun(t *testing.T) {
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	seedUnknown(t, dir, 5)

	for _, value := range []string{"0", "-5", "-1000"} {
		stdout, stderr, _ := runMainSplit(t, "collect", "--limit", value, "--delay", "1ms", "--json")
		want := "--limit должен быть положительным, получено " + value
		if !strings.Contains(stderr, want) {
			t.Errorf("--limit %s: stderr не называет причину, хочу %q, фактически %q", value, want, stderr)
		}
		if strings.Contains(stdout, "hosts") {
			t.Errorf("--limit %s: обход состоялся несмотря на отказ: %s", value, firstN(stdout, 200))
		}
	}
}

func TestCollectRejectsLimitAboveStoreCeilingInRealRun(t *testing.T) {
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	seedUnknown(t, dir, 5)

	for _, value := range []string{"5001", "20000"} {
		stdout, stderr, _ := runMainSplit(t, "collect", "--limit", value, "--delay", "1ms", "--json")
		want := "--limit слишком большой: " + value + " (предел " + strconv.Itoa(store.MaxStoreLimit) + ")"
		if !strings.Contains(stderr, want) {
			t.Errorf("--limit %s: stderr не называет значение и предел, хочу %q, фактически %q", value, want, stderr)
		}
		if strings.Contains(stdout, "hosts") {
			t.Errorf("--limit %s: обход состоялся несмотря на отказ: %s", value, firstN(stdout, 200))
		}
	}
}

// Рабочие значения доходят до обхода без подмены: отчёт несёт ровно то число
// хостов, которое попросили, а на пуле короче запроса - весь пул.
func TestCollectReportsRequestedLimitInRealRun(t *testing.T) {
	const seeded = 5
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)
	seedUnknown(t, dir, seeded)

	for _, value := range []string{"1", "2", "5", "5000"} {
		asked, err := strconv.Atoi(value)
		if err != nil {
			t.Fatalf("в тесте опечатка: %v", err)
		}
		want := asked
		if want > seeded {
			want = seeded
		}
		stdout, stderr, _ := runMainSplit(t, "collect", "--limit", value, "--delay", "1ms", "--json")
		if strings.Contains(stderr, "--limit") {
			t.Errorf("--limit %s: рабочее значение вызвало предупреждение %q", value, stderr)
		}
		var rep struct {
			Hosts  int `json:"hosts"`
			Failed int `json:"failed"`
		}
		if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
			t.Fatalf("--limit %s: разбор JSON: %v (%q)", value, err, firstN(stdout, 200))
		}
		if rep.Hosts != want {
			t.Errorf("--limit %s: отчёт несёт hosts=%d, хочу %d (в пуле %d адресов)",
				value, rep.Hosts, want, seeded)
		}
		if rep.Hosts > asked {
			t.Errorf("--limit %s: обойдено %d хостов, больше запрошенного", value, rep.Hosts)
		}
		if rep.Failed != rep.Hosts {
			t.Errorf("--limit %s: без tor отказов %d из %d, хочу по одному на хост",
				value, rep.Failed, rep.Hosts)
		}
	}
}

// Потолок collect обязан совпадать с потолком хранилища: проверка, которая
// пропускает значение выше него, обещает обход, который ListOnions всё равно
// обрежет.
func TestCollectLimitCeilingIsStoreCeiling(t *testing.T) {
	if err := checkLimitCeiling("limit", store.MaxStoreLimit, store.MaxStoreLimit); err != nil {
		t.Errorf("потолок хранилища отклонён: %v", err)
	}
	err := checkLimitCeiling("limit", store.MaxStoreLimit+1, store.MaxStoreLimit)
	if err == nil {
		t.Fatal("значение на единицу выше потолка принято")
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("предел %d", store.MaxStoreLimit)) {
		t.Errorf("отказ не называет предел: %v", err)
	}
}
