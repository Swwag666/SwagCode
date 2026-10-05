package main

import (
	"flag"
	"io"
	"strings"
	"testing"
	"time"
)

// parseDiscoverFlags разбирает аргументы discover тем же набором флагов, что и
// команда, но без tor, транспорта и базы.
func parseDiscoverFlags(t *testing.T, args ...string) *discoverFlags {
	t.Helper()
	fs := flag.NewFlagSet("discover", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	o := newDiscoverFlags(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("разбор флагов discover %+v: %v", args, err)
	}
	return o
}

const (
	seedA = "http://abcdefghijklmnop.onion/list"
	seedB = "http://qrstuvwxyz012345.onion/list"
)

// Замер до правки на HEAD 2ebd63f: --seeds был объявлен через fs.String, поэтому
// повтор флага перезаписывал значение и «--seeds A --seeds B» давало
// "http://qrstuvwxyz012345.onion/list". Первый сид исчезал молча: живой прогон
// discover --crawl с двумя флагами завершился rc=0 за 5.1с с пустым stderr, а
// listFlag, через который уже жили -ext, -verdict и --fields, на тех же
// аргументах возвращал оба значения.
func TestDiscoverSeedsAccumulateOnRepeat(t *testing.T) {
	o := parseDiscoverFlags(t, "--seeds", seedA, "--seeds", seedB)
	got := o.seedList()
	want := []string{seedA, seedB}
	if len(got) != len(want) {
		t.Fatalf("сидов %d, хочу %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("сид %d = %s, хочу %s", i, got[i], want[i])
		}
	}
}

func TestDiscoverSeedsCommaFormUnchanged(t *testing.T) {
	o := parseDiscoverFlags(t, "--seeds", seedA+","+seedB)
	got := o.seedList()
	if len(got) != 2 || got[0] != seedA || got[1] != seedB {
		t.Errorf("форма через запятую дала %v", got)
	}
}

func TestDiscoverSeedsMixRepeatAndComma(t *testing.T) {
	o := parseDiscoverFlags(t, "--seeds", seedA+","+seedB, "--seeds", "http://thirdhost0000000.onion/")
	got := o.seedList()
	want := []string{seedA, seedB, "http://thirdhost0000000.onion/"}
	if len(got) != len(want) {
		t.Fatalf("сидов %d, хочу %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("сид %d = %s, хочу %s", i, got[i], want[i])
		}
	}
}

func TestDiscoverSeedsDropEmptyParts(t *testing.T) {
	for _, value := range []string{",", " , , ", seedA + ", ,"} {
		o := parseDiscoverFlags(t, "--seeds", value)
		for _, s := range o.seedList() {
			if s == "" {
				t.Errorf("значение %q дало пустой сид в %v", value, o.seedList())
			}
		}
	}
	o := parseDiscoverFlags(t, "--seeds", seedA+", ,")
	if got := o.seedList(); len(got) != 1 || got[0] != seedA {
		t.Errorf("значение с пустыми частями дало %v, хочу [%s]", got, seedA)
	}
}

func TestDiscoverSeedsAbsentStaysEmpty(t *testing.T) {
	o := parseDiscoverFlags(t, "--crawl")
	if got := o.seedList(); len(got) != 0 {
		t.Errorf("без --seeds список %v, хочу пустой", got)
	}
	if got := o.seeds.value(); got != "" {
		t.Errorf("без --seeds value() = %q, хочу пустую строку", got)
	}
}

// Предупреждение о неприменённых сидах обязано видеть все сиды, а не последний:
// оно строится по value(), и до правки туда попадало только перезаписанное
// значение.
func TestWarnUnusedSeedsSeesEverySeed(t *testing.T) {
	o := parseDiscoverFlags(t, "--seeds", seedA, "--seeds", seedB)
	value := o.seeds.value()
	for _, seed := range []string{seedA, seedB} {
		if !strings.Contains(value, seed) {
			t.Errorf("в value() %q нет сида %s", value, seed)
		}
	}
	if got := discoverSeedsWarning(value, false); got == "" {
		t.Error("предупреждение о сидах без --crawl не напечатано")
	}
	if got := discoverSeedsWarning(value, true); got != "" {
		t.Errorf("с --crawl напечатано предупреждение %q", got)
	}
}

// Вынос набора флагов не имел права сдвинуть ни дефолт, ни имена: их печатает
// --help и на них смотрят скрипты.
func TestDiscoverFlagsKeepDefaults(t *testing.T) {
	o := parseDiscoverFlags(t)
	if o.depth != 0 {
		t.Errorf("depth по умолчанию = %d, хочу 0", o.depth)
	}
	// Ноль, а не пятьдесят: дефолт флага, равный envDefault конфига, глушил
	// VOIDSEARCH_DISCOVER_MAX_HOSTS, хотя справка обещала «0 - значение из
	// конфига». Замер до правки на HEAD 8345ec3:
	//
	//	VOIDSEARCH_DISCOVER_MAX_HOSTS=7   discover --crawl --depth 1 --json
	//	    rc=0, crawl.max_hosts=50
	//	VOIDSEARCH_DISCOVER_MAX_HOSTS=500 discover --crawl --depth 1 --json
	//	    rc=0, crawl.max_hosts=50
	if o.maxHosts != 0 {
		t.Errorf("max-hosts по умолчанию = %d, хочу 0", o.maxHosts)
	}
	if o.delay != 0 {
		t.Errorf("delay по умолчанию = %v, хочу 0", o.delay)
	}
	if o.crawl {
		t.Error("crawl по умолчанию включён")
	}
	if o.jsonOut {
		t.Error("json по умолчанию включён")
	}
	if o.timeout != 10*time.Minute {
		t.Errorf("timeout по умолчанию = %v, хочу 10m", o.timeout)
	}
}

func TestDiscoverFlagsParseEveryValue(t *testing.T) {
	o := parseDiscoverFlags(t,
		"--depth", "3",
		"--max-hosts", "12",
		"--delay", "250ms",
		"--crawl",
		"--seeds", seedA,
		"--json",
		"--timeout", "90s",
	)
	if o.depth != 3 {
		t.Errorf("depth = %d, хочу 3", o.depth)
	}
	if o.maxHosts != 12 {
		t.Errorf("max-hosts = %d, хочу 12", o.maxHosts)
	}
	if o.delay != 250*time.Millisecond {
		t.Errorf("delay = %v, хочу 250ms", o.delay)
	}
	if !o.crawl {
		t.Error("--crawl не применён")
	}
	if !o.jsonOut {
		t.Error("--json не применён")
	}
	if o.timeout != 90*time.Second {
		t.Errorf("timeout = %v, хочу 90s", o.timeout)
	}
	if got := o.seedList(); len(got) != 1 || got[0] != seedA {
		t.Errorf("seeds = %v", got)
	}
}

// Порядок накопления совпадает с порядком флагов в командной строке: обход идёт
// по очереди, и перестановка сидов меняла бы порядок обхода.
func TestDiscoverSeedsKeepCommandLineOrder(t *testing.T) {
	for run := 0; run < 20; run++ {
		o := parseDiscoverFlags(t, "--seeds", seedA, "--seeds", seedB, "--seeds", seedA)
		got := o.seedList()
		want := []string{seedA, seedB, seedA}
		if len(got) != len(want) {
			t.Fatalf("прогон %d: сидов %d, хочу %d: %v", run, len(got), len(want), got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("прогон %d: позиция %d = %s, хочу %s", run, i, got[i], want[i])
			}
		}
	}
}
