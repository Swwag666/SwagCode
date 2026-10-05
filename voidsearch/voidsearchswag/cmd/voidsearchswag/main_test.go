package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"voidsearchswag/internal/config"
	"voidsearchswag/internal/search"
	"voidsearchswag/internal/searchers"
)

// captureStdout перехватывает вывод команды через временный файл, а не через
// канал: часть команд печатает отчёт прямо в os.Stdout, и без перехвата тест
// засорял бы вывод прогона.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	f, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = f
	fn()
	f.Close()
	os.Stdout = old
	b, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{1, "1 B"},
		{512, "512 B"},
		{1023, "1023 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1024 * 1024, "1.0 MiB"},
		{19_327_352, "18.4 MiB"},
		{1024 * 1024 * 1024, "1.0 GiB"},
		{int64(1024) * 1024 * 1024 * 1024, "1.0 TiB"},
	}
	for _, c := range cases {
		if got := humanBytes(c.in); got != c.want {
			t.Errorf("humanBytes(%d)=%q, ожидала %q", c.in, got, c.want)
		}
	}
}

func TestHumanBytesLargeUnits(t *testing.T) {
	// Шкала "KMGTPE" должна выдерживать петабайты и эксабайты без выхода
	// за индекс: иначе паника на большом размере файла.
	if got := humanBytes(int64(1) << 50); !strings.HasSuffix(got, "PiB") {
		t.Errorf("петабайты: %q", got)
	}
	if got := humanBytes(int64(1) << 60); !strings.HasSuffix(got, "EiB") {
		t.Errorf("эксабайты: %q", got)
	}
	if got := humanBytes(int64(4) << 60); got != "4.0 EiB" {
		t.Errorf("4 EiB: %q", got)
	}
}

func TestHumanBytesNegative(t *testing.T) {
	// Отрицательный размер - признак битой записи, но форматирование не
	// должно паниковать.
	if got := humanBytes(-5); got == "" {
		t.Error("отрицательный размер дал пустую строку")
	}
}

func TestClipLine(t *testing.T) {
	if got := clipLine("короткая", 70); got != "короткая" {
		t.Errorf("короткая строка изменена: %q", got)
	}
	if got := clipLine("ровно123456", 100); got != "ровно123456" {
		t.Errorf("строка в пределах изменена: %q", got)
	}
	got := clipLine("длинная строка для обрезки", 6)
	if got != "длинна..." {
		t.Errorf("обрезка: %q", got)
	}
}

func TestClipLineByRunes(t *testing.T) {
	// Обрезка идёт по рунам: по байтам кириллица рвётся посередине
	// символа и в отчёте появляется мусор.
	got := clipLine("привет мир", 3)
	if strings.Count(got, "\uFFFD") != 0 {
		t.Errorf("появился символ замены: %q", got)
	}
	if !strings.HasPrefix(got, "при") {
		t.Errorf("обрезка не по рунам: %q", got)
	}
}

func TestClipLineExactLength(t *testing.T) {
	s := strings.Repeat("я", 70)
	if got := clipLine(s, 70); got != s {
		t.Errorf("строка ровно в лимит обрезана: %d рун", len([]rune(got)))
	}
	if got := clipLine(s+"!", 70); len([]rune(got)) != 73 {
		t.Errorf("длина %d, ожидала 73 (70 + ...)", len([]rune(got)))
	}
}

func TestOnionCount(t *testing.T) {
	if got := onionCount(nil); got != 0 {
		t.Errorf("nil-движок дал %d", got)
	}
	if got := onionCount(&search.Engine{}); got != 0 {
		t.Errorf("движок без каталога дал %d", got)
	}

	eng := &search.Engine{Onion: &searchers.OnionCatalog{
		Engines: []*searchers.OnionEngine{{Name_: "a"}, {Name_: "b"}},
	}}
	if got := onionCount(eng); got != 2 {
		t.Errorf("счёт %d, ожидала 2", got)
	}
}

func TestBuildDiscoverNilEngine(t *testing.T) {
	cfg := config.Config{RequestTimeout: time.Second}
	if got := buildDiscover(cfg, stderrLogger{}, nil, nil); got != nil {
		t.Error("без движка собран discover")
	}
	if got := buildDiscover(cfg, stderrLogger{}, nil, &search.Engine{}); got != nil {
		t.Error("без клиента собран discover")
	}
}

func TestBuildProberNilEngine(t *testing.T) {
	cfg := config.Config{RequestTimeout: time.Second}
	if got := buildProber(cfg, stderrLogger{}, nil, nil, 0); got != nil {
		t.Error("без движка собран пробер")
	}
	if got := buildProber(cfg, stderrLogger{}, nil, &search.Engine{}, 0); got != nil {
		t.Error("без клиента собран пробер")
	}
}

func TestBuildCollectorNilEngine(t *testing.T) {
	cfg := config.Config{RequestTimeout: time.Second, DiscoverMaxHosts: 5}
	if got := buildCollector(cfg, stderrLogger{}, nil, nil); got != nil {
		t.Error("без движка собран сборщик")
	}
	if got := buildCollector(cfg, stderrLogger{}, nil, &search.Engine{}); got != nil {
		t.Error("без клиента собран сборщик")
	}
}

func TestStderrLoggerWrites(t *testing.T) {
	// Логгер пишет в stderr и не должен паниковать ни на одном формате.
	log := stderrLogger{}
	log.Infof("инфо %d", 1)
	log.Warnf("предупреждение %s", "текст")
	log.Infof("без аргументов")
}

func TestZlogAdapterWrites(t *testing.T) {
	var buf strings.Builder
	z := zlogAdapter{l: zerolog.New(&buf)}
	z.Infof("инфо %d", 7)
	z.Warnf("варн %s", "текст")
	got := buf.String()
	if !strings.Contains(got, "инфо 7") {
		t.Errorf("инфо не прошло в zerolog: %q", got)
	}
	if !strings.Contains(got, "варн текст") {
		t.Errorf("варн не прошёл в zerolog: %q", got)
	}
	if !strings.Contains(got, `"level":"warn"`) {
		t.Errorf("уровень warn потерян: %q", got)
	}
}

func TestCmdFilesEmptyCatalog(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	out := captureStdout(t, func() { cmdFiles(nil) })
	if !strings.Contains(out, "каталог пуст") {
		t.Errorf("пустой каталог не объяснён: %q", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "voidsearchswag.db")); err != nil {
		t.Errorf("база не создана: %v", err)
	}
}

func TestCmdFilesWithEntries(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	// Каталог наполняется напрямую через store: команда должна показать
	// и статистику, и сами записи.
	st := openTestStore(t, dir)
	mustAddFile(t, st, "http://abc.onion/dump.sql", "dump.sql", "sql", 2048, "task-1", "http://abc.onion/")
	mustAddFile(t, st, "http://abc.onion/keys.kdbx", "keys.kdbx", "kdbx", 4096, "task-1", "http://abc.onion/")
	closeStore(st)

	out := captureStdout(t, func() { cmdFiles(nil) })
	if !strings.Contains(out, "dump.sql") || !strings.Contains(out, "keys.kdbx") {
		t.Errorf("файлы не показаны: %q", out)
	}
	if !strings.Contains(out, "sql=1") || !strings.Contains(out, "kdbx=1") {
		t.Errorf("разбивка по расширениям неверна: %q", out)
	}
	if !strings.Contains(out, "2.0 KiB") {
		t.Errorf("размер не отформатирован: %q", out)
	}
}

func TestCmdFilesFilterByExt(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	st := openTestStore(t, dir)
	mustAddFile(t, st, "http://abc.onion/dump.sql", "dump.sql", "sql", 2048, "task-1", "")
	mustAddFile(t, st, "http://abc.onion/keys.kdbx", "keys.kdbx", "kdbx", 4096, "task-1", "")
	closeStore(st)

	out := captureStdout(t, func() { cmdFiles([]string{"--ext", "kdbx"}) })
	if !strings.Contains(out, "keys.kdbx") {
		t.Errorf("фильтр по расширению потерял запись: %q", out)
	}
	if strings.Contains(out, "  dump.sql") {
		t.Errorf("фильтр не отсёк чужое расширение: %q", out)
	}
}

func TestCmdFilesJSON(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	st := openTestStore(t, dir)
	mustAddFile(t, st, "http://abc.onion/dump.sql", "dump.sql", "sql", 2048, "task-1", "")
	closeStore(st)

	out := captureStdout(t, func() { cmdFiles([]string{"--json"}) })
	// Формат JSON-отчёта команды files: объект со статистикой и списком,
	// а не голый массив. Ключи PascalCase - во всех структурах store нет
	// тегов, поэтому имя берётся из имени поля.
	if !strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Errorf("JSON-вывод не объект: %q", out)
	}
	for _, key := range []string{`"catalog_total"`, `"by_ext"`, `"files"`, `"returned"`} {
		if !strings.Contains(out, key) {
			t.Errorf("в отчёте нет %s: %q", key, out)
		}
	}
	if !strings.Contains(out, "dump.sql") {
		t.Errorf("запись пропала из JSON: %q", out)
	}
}

func TestCmdPoolSearchEmptyPool(t *testing.T) {
	t.Setenv("VOIDSEARCH_DATA_DIR", t.TempDir())
	out := captureStdout(t, func() { cmdPoolSearch(nil) })
	if !strings.Contains(out, "пул: всего 0") {
		t.Errorf("пустой пул не отчитан: %q", out)
	}
}

func TestCmdPoolSearchWithEntries(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	st := openTestStore(t, dir)
	ctx := t.Context()
	if err := st.UpsertOnion(ctx, storeOnion("http://live1.onion", "Живой сервис", "live", 0.9, 120)); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertOnion(ctx, storeOnion("http://live2.onion", "", "live", 0.5, 900)); err != nil {
		t.Fatal(err)
	}
	closeStore(st)

	out := captureStdout(t, func() { cmdPoolSearch([]string{"--status", "live", "--limit", "10"}) })
	if !strings.Contains(out, "Живой сервис") {
		t.Errorf("заголовок не показан: %q", out)
	}
	if !strings.Contains(out, "(без названия)") {
		t.Errorf("пустой заголовок не заменён: %q", out)
	}
	// Лучший по живости идёт первым.
	if strings.Index(out, "live1.onion") > strings.Index(out, "live2.onion") {
		t.Errorf("порядок не по живости: %q", out)
	}
	if !strings.Contains(out, "120ms") {
		t.Errorf("латентность не показана: %q", out)
	}
}

func TestCmdPoolSearchJSON(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	st := openTestStore(t, dir)
	ctx := t.Context()
	if err := st.UpsertOnion(ctx, storeOnion("http://live1.onion", "Сервис", "live", 0.9, 120)); err != nil {
		t.Fatal(err)
	}
	closeStore(st)

	out := captureStdout(t, func() { cmdPoolSearch([]string{"--json", "--limit", "5"}) })
	trimmed := strings.TrimSpace(out)
	if !strings.HasPrefix(trimmed, "[") {
		t.Errorf("JSON-вывод не массив: %q", out)
	}
	if !strings.Contains(out, "live1.onion") {
		t.Errorf("запись пропала: %q", out)
	}
}

// --- диспетчер команд: проверяется подпроцессом, потому что main вызывает
// os.Exit, а внутри теста это убило бы весь прогон. ---

// argSep разделяет аргументы в переменной окружения. Нулевой байт здесь не
// годится: переменная окружения не может содержать NUL, и на Windows
// значение обрезалось бы до первой части.
const argSep = "\x1f"

func runMain(t *testing.T, args ...string) (string, int) {
	t.Helper()
	// Будильник и WaitDelay - та же страховка, что и в runMainSplit: без него
	// молчаливое зависание подпроцесса вешало весь пакетный прогон.
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcessMain", "-test.timeout=90s")
	cmd.Env = subprocEnv(t, args)
	cmd.WaitDelay = 105 * time.Second
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("подпроцесс: %v\nвывод:\n%s", err, string(out))
		}
		code = ee.ExitCode()
	}
	return string(out), code
}

func TestHelperProcessMain(t *testing.T) {
	if os.Getenv("VSS_HELPER_MAIN") != "1" {
		t.Skip("это вспомогательный процесс")
	}
	args := []string{"voidsearchswag"}
	if raw := os.Getenv("VSS_MAIN_ARGS"); raw != "" {
		args = append(args, strings.Split(raw, argSep)...)
	}
	os.Args = args
	main()
	// Если main() вернулась (не вызвала os.Exit), завершаем процесс с нулём,
	// чтобы тестовый фреймворк не печатал PASS/FAIL поверх вывода команды.
	os.Exit(0)
}

func TestMainVersionExitsZero(t *testing.T) {
	for _, arg := range []string{"version", "--version", "-v"} {
		out, code := runMain(t, arg)
		if code != 0 {
			t.Errorf("%s: код %d", arg, code)
		}
		if !strings.Contains(out, "voidsearchswag "+version) {
			t.Errorf("%s: вывод %q", arg, out)
		}
	}
}

func TestMainNoArgsExitsTwo(t *testing.T) {
	out, code := runMain(t)
	if code != 2 {
		t.Errorf("без аргументов код %d, ожидала 2", code)
	}
	if !strings.Contains(out, "Команды:") {
		t.Errorf("справка не показана: %q", out)
	}
}

func TestMainHelpExitsZero(t *testing.T) {
	for _, arg := range []string{"help", "--help", "-h"} {
		out, code := runMain(t, arg)
		if code != 0 {
			t.Errorf("%s: код %d", arg, code)
		}
		if !strings.Contains(out, "Команды:") {
			t.Errorf("%s: справка не показана: %q", arg, out)
		}
	}
}

func TestMainUnknownCommandExitsTwo(t *testing.T) {
	out, code := runMain(t, "телепорт")
	if code != 2 {
		t.Errorf("неизвестная команда: код %d, ожидала 2", code)
	}
	if !strings.Contains(out, "неизвестная команда") {
		t.Errorf("ошибка не названа: %q", out)
	}
	if !strings.Contains(out, "телепорт") {
		t.Errorf("имя команды не echoes: %q", out)
	}
}

// dispatchedCommands разбирает исходник main.go и возвращает имена команд,
// которые реально разбирает диспетчер.
//
// Список берётся из кода, а не из хардкода в тесте: прежняя версия теста
// перечисляла 11 имён руками, тогда как диспетчер разбирал 19 команд. Справка
// содержала все 19, то есть код был верен, но тест не поймал бы удаление любой из
// восьми непроверенных строк - tord, parse, classify, hunt, promote, backup,
// clean, stats. Команда исчезла бы из справки молча, и пользователь не узнал бы о
// ней больше ниоткуда: другого описания команд в программе нет.
//
// До этапа 113 перечень команд жил в switch внутри main(), и тест вытаскивал имена
// разбором исходника: нормализовал CRLF, искал тело main(), резал его до
// следующей функции и собирал строки из case-меток, отсеивая чужие метки того же
// файла - суффиксы размеров в parseSize («kib», «байт»), статусы пула («live»,
// «dead») и подкоманды hunt. Такой разбор ломается молча при любой правке формата.
// Теперь диспетчер и справка берут команды из таблицы commands, и тест читает ту
// же таблицу: сравниваются значения, а не тексты.
//
// Алиасы с дефисом (--version, -h) командами не считаются: в справке их нет и не
// должно быть. Псевдокоманда help добавляется явно, потому что диспетчер её
// принимает, а в таблице её нет.
func dispatchedCommands(t *testing.T) map[string]bool {
	t.Helper()

	out := map[string]bool{}
	for _, name := range commandNames() {
		out[name] = true
	}
	// Псевдокоманда help принимается диспетчером, но в таблицу не входит: она и
	// есть справка.
	out["help"] = true

	// Защита от сломавшегося источника: куцый список означал бы, что таблица
	// команд перестала читаться, и тесты начали бы сравнивать пустые множества
	// между собой. Команд в диспетчере не может стать меньше пятнадцати без
	// переписывания main().
	if len(out) < 15 {
		t.Fatalf("в таблице команд только %d имён: %v", len(out), out)
	}
	return out
}

// usageCommands возвращает имена команд, описанные в справке.
func usageCommands(t *testing.T) map[string]bool {
	t.Helper()
	// Вывод нормализуется по той же причине, что и исходник: справка задана
	// строковым литералом в main.go, а литерал копирует байты файла, поэтому в
	// рабочей копии с CRLF он печатается с \r\n.
	out := strings.ReplaceAll(captureStderr(t, usage), "\r\n", "\n")
	// Строка справки выглядит как два пробела, имя команды, выравнивание и
	// описание. Строки других блоков либо начинаются с дефиса (--verbose), либо
	// не выровнены двумя и более пробелами.
	re := regexp.MustCompile(`(?m)^ {2}([a-z][a-z0-9-]*) {2,}\S`)
	found := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(out, -1) {
		found[m[1]] = true
	}
	if len(found) < 15 {
		t.Fatalf("в справке разобрано только %d команд: %v", len(found), found)
	}
	return found
}

func TestUsageCoversEveryDispatchedCommand(t *testing.T) {
	// Каждая команда диспетчера обязана быть в справке: иначе пользователь не
	// узнает о ней, а справка начнёт врать. Проверка идёт в обе стороны, потому
	// что строка справки без команды в диспетчере - это обещание, которого нет.
	dispatch := dispatchedCommands(t)
	documented := usageCommands(t)

	// Служебная псевдокоманда не описывает сама себя: help и есть справка.
	skip := map[string]bool{"help": true}

	for name := range dispatch {
		if skip[name] {
			continue
		}
		if !documented[name] {
			t.Errorf("команда %q разбирается диспетчером, но не описана в справке", name)
		}
	}
	for name := range documented {
		if !dispatch[name] {
			t.Errorf("команда %q описана в справке, но диспетчер её не разбирает", name)
		}
	}
}

func TestDispatchCoversDocumentedCommandsExplicitly(t *testing.T) {
	// Явный список команд: страховка на случай, если разбор исходника и разбор
	// справки сломаются согласованно и оба теста начнут сравнивать пустые
	// множества между собой. Здесь имена перечислены руками, поэтому тест
	// падает при исчезновении любой команды из диспетчера.
	dispatch := dispatchedCommands(t)
	for _, name := range []string{
		"setup", "run", "tord", "poolcheck", "search", "onioncheck",
		"discover", "probe", "poolsearch", "collect", "files", "parse",
		"classify", "hunt", "promote", "backup", "restore", "clean",
		"stats", "version",
	} {
		if !dispatch[name] {
			t.Errorf("команда %q не разбирается диспетчером", name)
		}
	}
}
