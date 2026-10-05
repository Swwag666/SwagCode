package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// parseSize разбирает размер файла: число с необязательным суффиксом.
//
// Поддерживаются как десятичные (kb, mb, gb, tb - степень 1000), так и двоичные
// (kib, mib, gib, tib - степень 1024) суффиксы, плюс короткие k/m/g/t и
// русские «кб», «мб», «гб». Регистр и пробел между числом и суффиксом не важны.
//
// Нужен потому, что флаг был объявлен как Int64, и `-min-size 1MB` падал с
// «parse error» ещё на разборе флагов - до всякой логики. Каталог показывает
// размеры в человекочитаемом виде («2.7 MiB», «18.4 MiB»), поэтому ожидать от
// пользователя байты в фильтре непоследовательно: он видит MiB и пишет MiB.
// Число без суффикса остаётся байтами, так что прежнее поведение сохранено.
func parseSize(s string) (int64, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, fmt.Errorf("пустое значение размера")
	}
	// Знак не поддерживается намеренно: отрицательный размер бессмыслен, а
	// молчаливое принятие «-1» дало бы фильтр, который ничего не фильтрует.
	// Минус и плюс разбираются отдельно, чтобы сообщение объясняло причину, а
	// не утверждало «нет числа» там, где число есть, и не называло
	// отрицательным значение со знаком плюса.
	if strings.HasPrefix(t, "-") {
		return 0, fmt.Errorf("размер не может быть отрицательным: %q", s)
	}
	if strings.HasPrefix(t, "+") {
		return 0, fmt.Errorf("знак в размере не поддерживается, пишите число без плюса: %q", s)
	}
	i := 0
	for i < len(t) && (t[i] >= '0' && t[i] <= '9' || t[i] == '.') {
		i++
	}
	numPart := strings.TrimSpace(t[:i])
	suffix := strings.ToLower(strings.TrimSpace(t[i:]))
	if numPart == "" {
		return 0, fmt.Errorf("в %q нет числа", s)
	}
	val, err := strconv.ParseFloat(numPart, 64)
	if err != nil {
		return 0, fmt.Errorf("в %q нет числа: %w", s, err)
	}
	if val < 0 {
		return 0, fmt.Errorf("размер не может быть отрицательным: %q", s)
	}

	var mult float64
	switch suffix {
	case "", "b", "байт", "bytes":
		mult = 1
	case "k", "kb", "кб", "kilo":
		mult = 1000
	case "ki", "kib":
		mult = 1024
	case "m", "mb", "мб", "mega":
		mult = 1000 * 1000
	case "mi", "mib":
		mult = 1024 * 1024
	case "g", "gb", "гб", "giga":
		mult = 1000 * 1000 * 1000
	case "gi", "gib":
		mult = 1024 * 1024 * 1024
	case "t", "tb", "тб", "tera":
		mult = 1000 * 1000 * 1000 * 1000
	case "ti", "tib":
		mult = 1024 * 1024 * 1024 * 1024
	default:
		return 0, fmt.Errorf("неизвестный суффикс %q в %q: доступно b, kb/k, mb/m, gb/g, tb/t и двоичные kib, mib, gib, tib", suffix, s)
	}

	out := val * mult
	// Граница проверяется как «больше либо равно», а не строго больше:
	// float64(math.MaxInt64) округляется ровно до 2^63, поэтому прежнее
	// условие пропускало значение, в точности равное пределу, и int64(out)
	// давал -9223372036854775808. Отрицательный размер молча превращал фильтр
	// в пустой: условие «размер больше отрицательного числа» проходило бы для
	// всех строк, а «меньше» - ни для одной, и команда печатала бы «файлов не
	// найдено» без намёка на опечатку во флаге.
	//
	// Из-за того же округления отвергается и MaxInt64, на один байт меньший
	// предела: во float64 эти значения неотличимы. Восемь эбибайт - заведомо
	// опечатка, поэтому отказ здесь безопаснее принятия.
	if out >= float64(math.MaxInt64) {
		return 0, fmt.Errorf("размер %q превышает предел int64", s)
	}
	return int64(out), nil
}

// splitList режет значение флага-списка на части: пустые отбрасываются, пробелы
// по краям обрезаются. Вынесено из Set, потому что тем же правилом разбирается
// значение по умолчанию.
func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// checkTimeout проверяет длительность и возвращает ошибку вместо завершения
// процесса, чтобы правило можно было покрыть тестами.
func checkTimeout(name string, d time.Duration) error {
	if d <= 0 {
		return fmt.Errorf("--%s должен быть положительным, получено %s", name, d)
	}
	if d > 24*time.Hour {
		return fmt.Errorf("--%s слишком большой: %s (предел 24h)", name, d)
	}
	return nil
}

// validTimeout проверяет флаг длительности перед созданием контекста.
//
// Непроверенное значение даёт два разных тихих отказа. Ноль или отрицательное
// число создают уже истёкший контекст, поэтому каждая операция падает
// немедленно, и сообщение об ошибке выглядит как сбой сети или базы, а не как
// опечатка во флаге. Огромное значение переполняет int64 в наносекундах:
// --timeout 3000000h в сумме с добавочной минутой уходит в отрицательную
// область, контекст снова оказывается истёкшим, и hunt watch возвращался
// мгновенно с невнятной ошибкой про превышение срока.
//
// Потолок в сутки выбран по смыслу: ни одна команда не работает дольше, а
// значение сверх него почти наверняка опечатка.
func validTimeout(name string, d time.Duration) time.Duration {
	if err := checkTimeout(name, d); err != nil {
		fatalf("%v", err)
	}
	return d
}

// addTimeout складывает длительности без переполнения int64. Если сумма
// выходит за предел, возвращается math.MaxInt64: контекст с таким сроком
// практически вечный, но валидный.
func addTimeout(d, extra time.Duration) time.Duration {
	if extra > 0 && d > math.MaxInt64-extra {
		return math.MaxInt64
	}
	return d + extra
}

// checkDurationRange проверяет флаг длительности против диапазона и возвращает
// ошибку вместо завершения процесса, чтобы правило можно было покрыть тестами.
// Отдельная функция от checkTimeout нужна там, где ноль законен: пауза нулём
// означает «не переопределять значение конфига», и требовать положительности
// значило бы запретить рабочий режим.
func checkDurationRange(name string, d, lo, hi time.Duration) error {
	if d < lo {
		return fmt.Errorf("--%s должен быть не меньше %s, получено %s", name, lo, d)
	}
	if d > hi {
		return fmt.Errorf("--%s слишком большой: %s (предел %s)", name, d, hi)
	}
	return nil
}

// validDurationRange проверяет флаг длительности и завершает прогон, если значение
// не прошло.
//
// Без проверки пауза подменялась молча и в двух местах сразу. Замер до правки на
// HEAD 71d085a, копия боевой базы, тор выключен, транспорт direct:
//
//	collect --limit 2 --delay -1s --json   rc=0, hosts=2, failed=2, stderr пуст
//	collect --limit 2 --delay -5m --json   rc=0, hosts=2, failed=2, stderr пуст
//	collect --limit 2 --delay 0s --json    rc=0, hosts=2, failed=2, stderr пуст
//	discover --crawl --delay -1s ...       rc=0, found=11116, crawled=1, stderr пуст
//	discover --crawl --delay 100000h ...   rc=0, found=11116, crawled=1, stderr пуст
//
// Программный зонд показал подмену: NewRateLimiter(-1s).Delay() даёт 2s,
// CrawlConfig.PerHostDelay=-1s через withDefaults даёт 2s,
// catalog.Config.PerHostDelay=-1s через withDefaults даёт 2s. То есть оператор,
// попросивший не делать пауз или ошибившийся в знаке, получал две секунды на хост
// без единого слова, а сто тысяч часов проходили до обхода как есть.
func validDurationRange(name string, d, lo, hi time.Duration) time.Duration {
	if err := checkDurationRange(name, d, lo, hi); err != nil {
		fatalf("%v", err)
	}
	return d
}

// checkLimit проверяет флаг количества и возвращает ошибку вместо завершения
// процесса, чтобы правило можно было покрыть тестами.
func checkLimit(name string, n int) error {
	return checkLimitCeiling(name, n, maxFlagLimit)
}

// checkLimitCeiling проверяет флаг количества против явного потолка. Отдельная
// функция, а не константа внутри checkLimit: у каждого хранилища свой предел, и
// CLI обязан отклонять значение до прогона, а не печатать в отчёте число, которое
// хранилище молча урезало. Для истории находок это store.MaxHuntHitsLimit.
func checkLimitCeiling(name string, n, ceiling int) error {
	if n <= 0 {
		return fmt.Errorf("--%s должен быть положительным, получено %d", name, n)
	}
	if n > ceiling {
		return fmt.Errorf("--%s слишком большой: %d (предел %d)", name, n, ceiling)
	}
	return nil
}

// validLimit проверяет флаг количества перед тем, как он уйдёт в поиск.
//
// Непроверенное значение не падает и не предупреждает: search.Engine подменяет
// любое неположительное Limit на DefaultN, то есть на 20. Замер до правки на HEAD
// 3e7d426, копия боевой базы, тор выключен, транспорт direct:
//
//	search --no-tor --no-cache --limit 1  --json "leak database"
//	  rc=0, limit=1, count=1
//	search --no-tor --no-cache --limit 0  --json "leak database"
//	  rc=0, limit=20, count=10
//	search --no-tor --no-cache --limit -5 --json "leak database"
//	  rc=0, limit=20, count=10
//
// Тот же замер на уровне ядра: Options.Limit=0, -5 и -1000 дают Outcome.Limit=20.
// То есть «--limit 0» и опечатка «--limit -5» молча превращались в дефолтные 20, и
// скрипт, который просил не выдавать ничего или падал на арифметике, получал
// штатный ответ с десятью ссылками. Отличить намеренный дефолт от подмены в
// отчёте было нельзя: поле limit уже несло подменённое значение.
func validLimit(name string, n int) int {
	if err := checkLimit(name, n); err != nil {
		fatalf("%v", err)
	}
	return n
}

// validLimitCeiling проверяет флаг количества против явного потолка и завершает
// прогон, если значение не прошло.
func validLimitCeiling(name string, n, ceiling int) int {
	if err := checkLimitCeiling(name, n, ceiling); err != nil {
		fatalf("%v", err)
	}
	return n
}

// discoverDepth выбирает глубину обхода: значение флага, а при нуле - значение
// конфига. Ноль здесь не «обход отключён», а «не переопределять конфиг»: обход
// включает --crawl. Правило вынесено из тела команды, потому что там его нельзя
// проверить без tor, сети и базы, а ошибка в нём меняет объём обхода в разы.
func discoverDepth(flagValue, cfgValue int) int {
	return flagOrConfig(flagValue, cfgValue)
}

// discoverMaxHosts выбирает потолок обходимых хостов тем же правилом, что и
// глубину: значение флага, а при нуле - значение конфига.
func discoverMaxHosts(flagValue, cfgValue int) int {
	return flagOrConfig(flagValue, cfgValue)
}

// checkRange проверяет флаг против диапазона и возвращает ошибку вместо
// завершения процесса, чтобы правило можно было покрыть тестами. Отдельная
// функция от checkLimitCeiling нужна там, где ноль законен: глубина обхода нулём
// говорит «возьми значение из конфига», и требовать положительности значило бы
// запретить рабочий режим.
func checkRange(name string, n, lo, hi int) error {
	if n < lo || n > hi {
		return fmt.Errorf("--%s должен быть в диапазоне от %d до %d, получено %d", name, lo, hi, n)
	}
	return nil
}

// validRange проверяет флаг против диапазона и завершает прогон, если значение не
// прошло.
//
// Без проверки флаг обходил валидацию конфига целиком: config.Load ограничивает
// DiscoverDepth восемью, но присваивание в cmdDiscover идёт после неё, поэтому
// значение из флага попадало в обход как есть. Замер до правки на HEAD 7c66f56,
// копия боевой базы, тор выключен, транспорт direct, discover --crawl -depth N
// --seeds http://unknaaaaaaaaaaaa.onion/ -timeout 8s --json:
//
//	-1   rc=0, crawl.depth=2,  stderr пуст
//	0    rc=0, crawl.depth=2,  stderr пуст
//	3    rc=0, crawl.depth=3,  stderr пуст
//	100  rc=0, crawl.depth=100, stderr пуст
//
// Минус один и ноль молча превращались в значение конфига, а сто проходили до
// обхода без единого предупреждения.
func validRange(name string, n, lo, hi int) int {
	if err := checkRange(name, n, lo, hi); err != nil {
		fatalf("%v", err)
	}
	return n
}
