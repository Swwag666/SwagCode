package main

import (
	"strconv"
	"strings"
	"testing"

	"voidsearchswag/internal/config"
)

// Живой замер до правки на HEAD 06547a8, пересобранный бинарник, копия боевой
// базы, тор выключен, транспорт direct,
// discover --crawl -depth 1 -max-hosts N --seeds http://unknaaaaaaaaaaaa.onion/
// -timeout 8s --json:
//
//	-1       rc=0, crawled=1, crawl.pages=1, crawl.limit_hit пуст, stderr пуст
//	0        rc=0, crawled=1, crawl.pages=1, crawl.limit_hit пуст, stderr пуст
//	1        rc=0, crawled=1, crawl.pages=1, crawl.limit_hit пуст, stderr пуст
//	100000   rc=0, crawled=1, crawl.pages=1, crawl.limit_hit пуст, stderr пуст
//
// Все четыре прогона неотличимы друг от друга: отчёт не называет применённый
// потолок, а stderr молчит. Программный зонд показал, куда девались числа:
// config.Validate обрезает DiscoverMaxHosts до 5000 и поднимает отрицательное до
// единицы, но присваивание в команде идёт после загрузки конфига, поэтому флаг
// попадал в CrawlConfig как есть, а withDefaults подменял неположительное на 50:
//
//	env VOIDSEARCH_DISCOVER_MAX_HOSTS=-1     -> config.DiscoverMaxHosts=50
//	env VOIDSEARCH_DISCOVER_MAX_HOSTS=100000 -> config.DiscoverMaxHosts=5000
//	CrawlConfig.MaxHosts=-1                  -> 50
//	CrawlConfig.MaxHosts=100000              -> 100000
func TestDiscoverRejectsMaxHostsOutsideRangeInRealRun(t *testing.T) {
	// Офлайн-окружение обязательно: без него сломанная проверка уводит прогон в
	// базу из конфига, то есть в боевую. Так уже случилось на этапе 133.
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)

	for _, value := range []string{"-1", "-5", "5001", "100000"} {
		stdout, stderr, code := runMainSplit(t, "discover", "--crawl", "-max-hosts", value, "-timeout", "1s")
		want := "--max-hosts должен быть в диапазоне от 0 до " + strconv.Itoa(maxDiscoverHosts) + ", получено " + value
		if !strings.Contains(stderr, want) {
			t.Errorf("-max-hosts %s: stderr не называет причину, хочу %q, фактически %q", value, want, stderr)
		}
		if code != 1 {
			t.Errorf("-max-hosts %s: код возврата %d, хочу 1", value, code)
		}
		if strings.Contains(stdout, "crawl") {
			t.Errorf("-max-hosts %s: обход состоялся несмотря на отказ: %s", value, firstN(stdout, 200))
		}
	}
}

// Потолок команды обязан совпадать с пределом конфига: иначе флаг либо запрещал бы
// рабочую ширину обхода, либо пропускал значение, которое конфиг всё равно
// обрезал бы для переменной окружения.
func TestDiscoverMaxHostsCeilingMatchesConfigRule(t *testing.T) {
	if maxDiscoverHosts != 5000 {
		t.Errorf("maxDiscoverHosts = %d, хочу 5000: таков верхний предел DiscoverMaxHosts в конфиге", maxDiscoverHosts)
	}
	t.Setenv("VOIDSEARCH_DATA_DIR", t.TempDir())
	t.Setenv("VOIDSEARCH_DISCOVER_MAX_HOSTS", "100000")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("конфиг не собрался: %v", err)
	}
	if cfg.DiscoverMaxHosts != maxDiscoverHosts {
		t.Errorf("конфиг ограничивает потолок хостов до %d, а команда держит %d",
			cfg.DiscoverMaxHosts, maxDiscoverHosts)
	}
}

// Ноль означает «возьми значение из конфига», а не «не обходить ничего»: живой
// замер на -max-hosts 0 и на -max-hosts 1 дал одинаковый отчёт, то есть ноль
// никогда не доходил до обхода как ноль.
func TestDiscoverMaxHostsPrefersFlagOverConfig(t *testing.T) {
	cases := []struct {
		flag int
		cfg  int
		want int
	}{
		{0, 50, 50},
		{0, 1, 1},
		{1, 50, 1},
		{50, 5000, 50},
		{maxDiscoverHosts, 50, maxDiscoverHosts},
	}
	for _, c := range cases {
		if got := discoverMaxHosts(c.flag, c.cfg); got != c.want {
			t.Errorf("discoverMaxHosts(%d, %d) = %d, хочу %d", c.flag, c.cfg, got, c.want)
		}
	}
}

// Глубина и потолок хостов обязаны следовать одному правилу, иначе ноль в одном
// флаге означал бы одно, а в другом другое. Обе функции - обёртки над общей.
func TestFlagOrConfigIsSharedRule(t *testing.T) {
	for _, pair := range []struct{ flag, cfg int }{{0, 7}, {3, 7}, {7, 3}, {0, 0}} {
		if d, h := discoverDepth(pair.flag, pair.cfg), discoverMaxHosts(pair.flag, pair.cfg); d != h {
			t.Errorf("на флаге %d и конфиге %d глубина дала %d, а потолок хостов %d",
				pair.flag, pair.cfg, d, h)
		}
		if got := flagOrConfig(pair.flag, pair.cfg); got != discoverDepth(pair.flag, pair.cfg) {
			t.Errorf("flagOrConfig(%d, %d) = %d, а обёртка дала %d", pair.flag, pair.cfg, got, discoverDepth(pair.flag, pair.cfg))
		}
	}
}

// Справка обязана называть диапазон и смысл нуля: до правки она говорила просто
// «потолок обходимых хостов», и оператор не мог узнать ни границу, ни то, что ноль
// означает значение из конфига.
func TestDiscoverMaxHostsHelpNamesRangeAndFallback(t *testing.T) {
	_, stderr, _ := runMainSplit(t, "discover", "-h")
	if !strings.Contains(stderr, "от 1 до 5000") {
		t.Errorf("справка не называет диапазон потолка хостов, фактически:\n%s", firstN(stderr, 700))
	}
	if !strings.Contains(stderr, "0 - значение из конфига") {
		t.Errorf("справка не объясняет ноль у потолка хостов, фактически:\n%s", firstN(stderr, 700))
	}
}
