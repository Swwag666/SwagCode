package main

import (
	"strconv"
	"strings"
	"testing"

	"voidsearchswag/internal/config"
)

// Живой замер до правки на HEAD 7c66f56, пересобранный бинарник, копия боевой
// базы, тор выключен, транспорт direct,
// discover --crawl -depth N --seeds http://unknaaaaaaaaaaaa.onion/ -timeout 8s --json:
//
//	-1   rc=0, found=11119, crawled=1, crawl.depth=2,   stderr пуст
//	0    rc=0, found=11119, crawled=1, crawl.depth=2,   stderr пуст
//	3    rc=0, found=11119, crawled=1, crawl.depth=3,   stderr пуст
//	100  rc=0, found=11119, crawled=1, crawl.depth=100, stderr пуст
//
// Отрицательное значение молча превращалось в значение конфига, а сто проходили
// до обхода как есть: config.Validate держит DiscoverDepth в границах от единицы
// до восьми, но присваивание в команде идёт после загрузки конфига, поэтому
// валидация до флага не доходила. Обход глубиной сто - это не опечатка, которую
// можно простить, а обещание лезть в каждую найденную ссылку сто раз подряд.
func TestDiscoverRejectsDepthOutsideRangeInRealRun(t *testing.T) {
	// Офлайн-окружение здесь обязательно, а не для красоты. Если проверка
	// глубины сломается, прогон пойдёт дальше и откроет базу из конфига, то есть
	// боевую: на мутации с убранной проверкой discover --crawl так и сделал,
	// записал найденные адреса и сдвинул mtime боевого файла. С временным каталогом
	// и выключенным тором сломанная проверка остаётся внутри теста.
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)

	for _, value := range []string{"-1", "-5", "9", "100"} {
		stdout, stderr, code := runMainSplit(t, "discover", "--crawl", "-depth", value, "-timeout", "1s")
		want := "--depth должен быть в диапазоне от 0 до " + strconv.Itoa(maxDiscoverDepth) + ", получено " + value
		if !strings.Contains(stderr, want) {
			t.Errorf("-depth %s: stderr не называет причину, хочу %q, фактически %q", value, want, stderr)
		}
		if code != 1 {
			t.Errorf("-depth %s: код возврата %d, хочу 1", value, code)
		}
		if strings.Contains(stdout, "crawl") {
			t.Errorf("-depth %s: обход состоялся несмотря на отказ: %s", value, firstN(stdout, 200))
		}
	}
}

// Диапазон проверяется чистой функцией, потому что ноль здесь законен: он
// означает «возьми значение из конфига», и правило положительности для такого
// флага не подходит.
func TestCheckRangeAcceptsBoundsAndRejectsOutside(t *testing.T) {
	for _, n := range []int{0, 1, 4, maxDiscoverDepth} {
		if err := checkRange("depth", n, 0, maxDiscoverDepth); err != nil {
			t.Errorf("значение %d внутри диапазона отклонено: %v", n, err)
		}
	}
	for _, n := range []int{-1, -100, maxDiscoverDepth + 1, 1000} {
		err := checkRange("depth", n, 0, maxDiscoverDepth)
		if err == nil {
			t.Errorf("значение %d вне диапазона принято", n)
			continue
		}
		want := "--depth должен быть в диапазоне от 0 до " + strconv.Itoa(maxDiscoverDepth) + ", получено " + strconv.Itoa(n)
		if err.Error() != want {
			t.Errorf("отказ для %d звучит как %q, хочу %q", n, err.Error(), want)
		}
	}
}

// Потолок команды обязан совпадать с тем, что config.Validate держит для глубины
// обхода: иначе флаг либо запрещал бы рабочую глубину, либо пропускал значение,
// которое конфиг всё равно обрезал бы.
func TestDiscoverDepthCeilingMatchesConfigRule(t *testing.T) {
	if maxDiscoverDepth != 8 {
		t.Errorf("maxDiscoverDepth = %d, хочу 8: таков верхний предел DiscoverDepth в конфиге", maxDiscoverDepth)
	}
	t.Setenv("VOIDSEARCH_DATA_DIR", t.TempDir())
	t.Setenv("VOIDSEARCH_DISCOVER_DEPTH", "100")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("конфиг не собрался: %v", err)
	}
	if cfg.DiscoverDepth != maxDiscoverDepth {
		t.Errorf("конфиг ограничивает глубину до %d, а команда держит потолок %d",
			cfg.DiscoverDepth, maxDiscoverDepth)
	}
}

// Ноль означает «возьми значение из конфига», а не «обход выключен»: живой замер
// на -depth 0 дал crawl.depth=2 и crawled=1, то есть обход состоялся с глубиной
// конфига. Правило вынесено в чистую функцию и проверяется здесь.
func TestDiscoverDepthPrefersFlagOverConfig(t *testing.T) {
	cases := []struct {
		flag int
		cfg  int
		want int
	}{
		{0, 2, 2},
		{0, 8, 8},
		{1, 2, 1},
		{3, 2, 3},
		{maxDiscoverDepth, 2, maxDiscoverDepth},
	}
	for _, c := range cases {
		if got := discoverDepth(c.flag, c.cfg); got != c.want {
			t.Errorf("discoverDepth(%d, %d) = %d, хочу %d", c.flag, c.cfg, got, c.want)
		}
	}
}

// Справка флага до правки обещала, что ноль означает «только источники», а замер
// показал обход с глубиной из конфига: crawl.depth=2 и crawled=1. Текст справки
// исправлен, и тест держит его, чтобы обещание не разъехалось с поведением снова.
func TestDiscoverDepthHelpNamesConfigFallback(t *testing.T) {
	_, stderr, _ := runMainSplit(t, "discover", "-h")
	if !strings.Contains(stderr, "0 - значение из конфига") {
		t.Errorf("справка не объясняет ноль, фактически:\n%s", firstN(stderr, 600))
	}
	if strings.Contains(stderr, "0 - только источники") {
		t.Errorf("справка по-прежнему обещает режим без обхода на нуле:\n%s", firstN(stderr, 600))
	}
	if !strings.Contains(stderr, "от 1 до 8") {
		t.Errorf("справка не называет диапазон глубины, фактически:\n%s", firstN(stderr, 600))
	}
}
