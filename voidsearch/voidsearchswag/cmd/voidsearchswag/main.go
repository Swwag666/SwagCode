package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"voidsearchswag/internal/parser"
	"voidsearchswag/internal/search"
)

// version печатается командой version и уходит в MCP-протокол (initialize).
// Значение заливается линкером при релизной сборке:
//
//	go build -ldflags "-X main.version=v0.1.0 -X main.buildCommit=... -X main.buildDate=..."
//
// (Makefile и .github/workflows/ci.yml делают это автоматически). Без заливки
// остаётся суффикс -dev: такая сборка честно не притворяется релизом, и
// «voidsearchswag version» на проде сразу виден как самосбор.
var version = "0.1.0-dev"

// buildCommit и buildDate заливаются тем же -ldflags. unknown - признак
// локальной сборки без Makefile/CI.
var (
	buildCommit = "unknown"
	buildDate   = "unknown"
)

// verboseLogs включает информационные сообщения.
//
// По умолчанию выключено. Информационные строки вроде «здоровье движков
// восстановлено: 7», «сессия: direct/chrome_131_win через direct» и «дозор 20s»
// печатались при каждом запуске и тонули в полезном выводе: их три-четыре на
// одну команду, а сообщают они о штатной работе, а не о проблеме. При этом
// убрать их совсем нельзя - при разборе «почему движок отвалился» именно они
// показывают, какой транспорт и какая сессия использовались.
//
// Переключатель пакетный, а не поле логгера, потому что stderrLogger
// конструируется в 57 местах как значение без аргументов; протаскивать уровень
// через каждый вызов значило бы менять все 57 и все сигнатуры между ними.
// Warnf печатается всегда: предупреждение по определению не штатная работа.
var verboseLogs bool

type stderrLogger struct{}

func (stderrLogger) Infof(f string, a ...any) {
	if !verboseLogs {
		return
	}
	fmt.Fprintf(os.Stderr, f+"\n", a...)
}

func (stderrLogger) Warnf(f string, a ...any) { fmt.Fprintf(os.Stderr, "WARN: "+f+"\n", a...) }

type zlogAdapter struct{ l zerolog.Logger }

func (z zlogAdapter) Infof(f string, a ...any) { z.l.Info().Msgf(f, a...) }

func (z zlogAdapter) Warnf(f string, a ...any) { z.l.Warn().Msgf(f, a...) }

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	// Обработчик сигналов ставится до разбора команды, а не после: прерывание
	// возможно и на этапе setup, который качает tor-бандл и chromium. Без него
	// Ctrl+C во время долгой разведки оставлял tor-процесс сиротой, потому что
	// os.Exit из defer не вызывается вовсе.
	NotifyShutdown()
	// Нормальное завершение тоже опустошает реестр: это страховка на случай,
	// если какой-то ресурс зарегистрирован, но defer снятия сработал не во всех
	// путях выхода из команды. Вызов идемпотентен, поэтому при чистой работе он
	// ничего не делает.
	defer RunShutdown()
	// Признак подробного вывода разбирается до субкоманды и вырезается из
	// аргументов, чтобы FlagSet команды не принял его за неизвестный флаг.
	detectVerbose(os.Args[1:])
	// Машинный вывод определяется до субкоманды и сразу по всем аргументам:
	// отказ случается и раньше, чем FlagSet конкретной команды доберётся до
	// своего --json (подкоманда не указана, не распознана, флаг неизвестен).
	jsonMode = wantsJSON(os.Args[1:])
	// Каталог данных, в котором нет рабочей базы, но лежат её снимки, - самая
	// частая ошибка оператора: дату перенесли или восстановили не до конца, а
	// сервис выглядит здоровым. Замер до правки: stats в таком каталоге давал
	// rc=0, 176 байт нулей в stdout и пустой stderr, а рядом создавалась пустая
	// база на 159744 байта. Предупреждение печатается до команды, потому что
	// нули напечатает каждая.
	warnSnapshotOnlyDataDir(os.Args[1])
	rest := verboseArgs(os.Args[2:])
	if run, ok := lookupCommand(os.Args[1]); ok {
		run(rest)
		return
	}
	switch os.Args[1] {
	case "--version", "-v":
		cmdVersion(rest)
	case "help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "неизвестная команда %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

// commandSpec - одна консольная команда: имя, строка справки и обработчик.
// Список commands служит единственным источником и для диспетчера, и для usage,
// поэтому команда не может появиться в одном месте и пропасть в другом. До правки
// switch диспетчера и текст справки были двумя копиями одного перечня: справка
// знала 20 команд, диспетчер - те же 20, а README перечислял 15, и расхождение
// не ловилось ничем.
type commandSpec struct {
	name string
	help string
	run  func([]string)
}

// Порядок - как в справке, а не алфавитный: сначала то, чем пользуются каждый
// день. Версия стоит последней, потому что это справочный вызов.
var commands = []commandSpec{
	{"setup", "поставить весь стек: tor, база, chromium, проверка (флаги: --offline, --no-verify, --skip-check, --no-browser, --warm-pool)", cmdSetup},
	{"run", "запустить MCP-сервер (stdio по умолчанию, --http :port для HTTP, --no-tor, --token/--token-file, --tls-cert/--tls-key)", cmdRun},
	{"tord", "держать tor живым для всех команд: остальные подключаются к нему и стартуют без 20-25с bootstrap (--survive)", cmdTorDaemon},
	{"search", "поиск из консоли (--mode auto|fast|stealth|deep)", cmdSearch},
	{"onioncheck", "пинг onion-поисковиков и отчёт о живости", cmdOnioncheck},
	{"discover", "onion-разведка: сбор адресов из каталогов и обход .onion", cmdDiscover},
	{"probe", "пробы живости пула: проверка неизвестных адресов через tor", cmdProbe},
	{"poolsearch", "поиск по собранной базе onion-сервисов: poolsearch <слово> (--status, --limit, --json)", cmdPoolSearch},
	{"collect", "сбор файлов с живых хостов пула в каталог", cmdCollect},
	{"files", "поиск по каталогу файлов: текст, расширение, размер", cmdFiles},
	{"parse", "извлечь поля со страницы (self-healing парсер)", cmdParse},
	{"classify", "классифицировать источник: public|private|paid|scam", cmdClassify},
	{"hunt", "фоновые мониторинги: create|list|run|watch|hits", cmdHunt},
	{"promote", "автопромоут: найти поисковики среди живых сервисов пула", cmdPromote},
	{"backup", "снимок базы SQLite с ротацией", cmdBackup},
	{"restore", "вернуть базу из снимка (--snapshot имя|путь|latest, --yes, --dir)", cmdRestore},
	{"clean", "вычистить мусор из каталога файлов и заголовков (--apply чтобы удалить)", cmdClean},
	{"stats", "сводная статистика базы и движков", cmdStats},
	{"poolcheck", "прогреть прокси-пул и показать статистику", cmdPoolcheck},
	{"version", "версия", cmdVersion},
}

// lookupCommand возвращает обработчик команды по имени. Вынесено из диспетчера,
// чтобы тест мог проверить маршрутизацию каждого имени, не запуская сами команды:
// часть из них уходит в сеть и поднимает tor.
func lookupCommand(name string) (func([]string), bool) {
	for i := range commands {
		if commands[i].name == name {
			return commands[i].run, true
		}
	}
	return nil, false
}

// commandNames возвращает имена всех команд в порядке справки.
func commandNames() []string {
	names := make([]string, 0, len(commands))
	for _, c := range commands {
		names = append(names, c.name)
	}
	return names
}

func usage() {
	var b strings.Builder
	fmt.Fprintf(&b, "VoidSearchSwag %s - умный поисковый движок (MCP-сервер)\n\nКоманды:\n", version)
	for _, c := range commands {
		fmt.Fprintf(&b, "  %-12s%s\n", c.name, c.help)
	}
	b.WriteString(`
Общий флаг для любой команды:
  --verbose   печатать служебные строки (здоровье движков, транспорт сессии,
              дозор). По умолчанию они скрыты: это штатная работа, а не
              события, и они тонули в полезном выводе. То же включает
              переменная VOIDSEARCH_VERBOSE. Предупреждения и ошибки
              печатаются всегда, независимо от флага.
              Короткий -v занят под version, поэтому только полная форма.

Данные и состояние: %USERPROFILE%\.voidsearchswag\ (или $VOIDSEARCH_DATA_DIR)
`)
	fmt.Fprint(os.Stderr, b.String())
}

// maxFlagPoolSize и maxFlagFetchLimit - потолки флагов команды poolcheck. Оба
// равны границам, которые config.Validate держит для ProxyPoolSize и
// ProxyFetchLimit: десять тысяч и сто тысяч. Значение сверх этого конфиг всё равно
// обрезал бы, но обрезал бы только переменную окружения, а флаг шёл в пул как есть.
const (
	maxFlagPoolSize   = 10000
	maxFlagFetchLimit = 100000
)

// jsonMode - вызывающий попросил машинный вывод. Переменная нужна fatalf: до
// правки отказ при --json уходил только в stderr, а stdout оставался пустым, и
// потребитель JSON не получал ни данных, ни причины. Измерено на пяти отказах:
// hunt run --id 999, hunt без подкоманды, search без запроса, hunt hits --clear
// без --id и неизвестный флаг; в четырёх из них rc=1 и 0 байт в stdout, в пятом
// rc=2 и те же 0 байт.
var jsonMode bool

// sizeFlag - флаг размера, принимающий и байты, и человекочитаемые значения.
type sizeFlag struct {
	val int64
	set bool
	raw string
}

func (f *sizeFlag) String() string {
	if !f.set {
		return "0"
	}
	return f.raw
}

func (f *sizeFlag) Set(s string) error {
	v, err := parseSize(s)
	if err != nil {
		return err
	}
	f.val = v
	f.set = true
	f.raw = s
	return nil
}

// listFlag - флаг-список, который копит значения при повторе.
//
// Объявить список через fs.String значило хранить одно значение: повтор флага
// перезаписывал прежнее, и «files -ext pdf -ext epub» молча отбирал только epub.
// Живой замер на копии боевой базы дал returned=6 против returned=8 у формы
// «files -ext pdf,epub» и столько же, сколько «files -ext epub», то есть
// расширение из первого флага исчезло без единого слова предупреждения. То же
// было с метками категории: «-verdict ebook -verdict document» вернул 2 записи
// вместо 8. Привычка повторять флаг пришла из git, docker и terraform, поэтому
// накопление - единственное поведение, которое не обманывает ожиданий.
//
// Значения склеиваются запятой: internal/store режет список по запятой, пробелу и
// точке с запятой, поэтому разбор там не меняется и форма «-ext pdf,epub»
// остаётся прежней. Пустые части отбрасываются, чтобы «-ext pdf,» не добавлял
// пустое расширение, а пробелы по краям значения не портили сравнение.
type listFlag struct {
	parts []string
	// set отличает «флаг не задан» от «флаг задан пустым значением»: у флагов с
	// дефолтом, например parse --fields title, подставлять дефолт можно только в
	// первом случае, иначе «--fields ""» молча превратилось бы в title.
	set bool
}

func (f *listFlag) String() string {
	return strings.Join(f.parts, ",")
}

func (f *listFlag) Set(s string) error {
	f.set = true
	f.parts = append(f.parts, splitList(s)...)
	return nil
}

// value возвращает строку для FileQuery.Ext и FileQuery.Verdict. Пустая строка
// при незаполненном флаге сохраняет прежний смысл «фильтр не задан».
func (f *listFlag) value() string {
	return strings.Join(f.parts, ",")
}

// listOr возвращает накопленные значения, а при незаполненном флаге - разбор
// значения по умолчанию.
func (f *listFlag) listOr(def string) []string {
	if f.set {
		return f.parts
	}
	return splitList(def)
}

// maxFlagDelay - потолок флагов паузы у collect, discover и probe. Равен потолку
// той же величины в конфиге: internal/config ограничивает
// VOIDSEARCH_DISCOVER_DELAY десятью минутами, и флаг, допускающий сутки,
// разрешал из командной строки то, что конфиг отвергает как слишком большое.
//
// Живой замер до правки на HEAD ac85bf8, пересобранный бинарник, копия боевой
// базы, тор выключен, транспорт direct:
//
//	probe --limit 3 --delay 15h --json
//	    rc=1, delay_ms=54000000, то есть пауза пятнадцать часов ушла в волну
//	collect --limit 1 --delay 15h --json
//	    rc=0, per_host_delay_ms=54000000
//	discover --crawl --depth 1 --delay 15h --json
//	    rc=0, crawl.per_host_delay_ms=54000000
//	VOIDSEARCH_DISCOVER_DELAY=15h probe --limit 3 --json
//	    rc=1, ERROR: конфиг: config: VOIDSEARCH_DISCOVER_DELAY слишком большой:
//	    15h0m0s (предел 10m0s)
//
// Одно и то же значение флаг принимал, а конфиг отвергал. Пауза больше десяти
// минут между запросами к одному хосту не нужна ни одной команде: она растягивает
// волну на часы, и почти наверняка это опечатка вроде перепутанной единицы
// измерения.
const maxFlagDelay = 10 * time.Minute

// maxFlagLimit - потолок флагов количества. Движки всё равно не вернут больше
// пары сотен ссылок, а значение сверх этого почти наверняка опечатка вроде
// лишнего нуля.
const maxFlagLimit = 10000

// maxFlagResultLimit - потолок флага --limit у search. Равен верхней границе
// ResultLimit в config.Validate: там та же величина урезается диапазоном от
// единицы до тысячи, и флаг, допускающий десять тысяч, обещал оператору выдачу,
// которую конфиг считает бессмысленной.
//
// Живой замер до правки на HEAD 719b6db, пересобранный бинарник, копия боевой
// базы, тор выключен, транспорт direct:
//
//	search --limit 5000 --no-tor --json test
//	    rc=0, в отчёте limit=5000
//	search --limit 10001 --no-tor --json test
//	    rc=1, ERROR: --limit слишком большой: 10001 (предел 10000)
//	VOIDSEARCH_RESULT_LIMIT=5000 search --no-tor --json test
//	    rc=0, stderr пуст, то есть конфиг молча урезал пять тысяч до тысячи
//
// Одна величина шла двумя путями с двумя разными потолками. Отдельная константа,
// а не правка maxFlagLimit: общий потолок используют --show у poolcheck и
// --max-files у collect, и у них нет пары в конфиге.
const maxFlagResultLimit = 1000

// maxPromoteLimit - потолок флага --limit у promote. Он ниже общего предела флагов,
// потому что каждая проверка кандидата идёт через tor по живому адресу и тратит
// бюджет прогона: значение в десять тысяч обещало бы прогон, который не уложится ни
// в один бюджет. Число равно верхней границе, которую config.Validate держит для
// PromoteLimit: обе стороны описывают одну величину, число проверок живых сервисов
// за прогон. До правки здесь стояло пятьдесят, и одно и то же число было законным из
// переменной окружения и незаконным из флага: живой замер на HEAD 5cd38c0 дал
// --limit 200 с отказом «предел 50» при конфиг-потолке двести.
const maxPromoteLimit = 200

// maxCollectConcurrency - потолок одновременных обходов у collect. Число не
// выдуманное: config.Validate держит DiscoverConcurrency в тех же границах, от
// единицы до 128, и обход хостов каталогом - та же работа через тот же транспорт.
// Значение выше всё равно упрётся в скорость tor, а не в число горутин.
const maxCollectConcurrency = 128

// maxFlagBackupKeep - потолок флага --keep у backup. Равен верхней границе, которую
// config.Validate держит для BackupKeep: обе стороны описывают одну величину, число
// снимков в каталоге, и расхождение между ними означало бы, что значение из
// переменной окружения законно, а то же значение из флага - нет. До правки флаг
// молча обрезал всё, что больше тридцати, и живой замер на HEAD 2bf9853 дал
// --keep 365 с ответом keep=30.
const maxFlagBackupKeep = 365

// maxDiscoverDepth - потолок глубины обхода у discover. Число не выдуманное:
// config.Validate держит DiscoverDepth в границах от единицы до восьми, и
// значение выше обещало бы обход, который не уложится ни в один таймаут.
const maxDiscoverDepth = 8

// maxDiscoverHosts - потолок числа обходимых хостов у discover. Равен верхней
// границе DiscoverMaxHosts в config.Validate, то есть пяти тысячам: значение выше
// конфиг всё равно обрезал бы, но обрезал бы молча и только для переменной
// окружения, а флаг шёл в обход валидации целиком.
const maxDiscoverHosts = 5000

// discoverFlags держит разобранные флаги команды discover. Набор вынесен из
// cmdDiscover, чтобы накопление --seeds проверялось без tor, транспорта, базы и
// сети: сама команда поднимает всё это до первого запроса.
type discoverFlags struct {
	depth    int
	maxHosts int
	delay    time.Duration
	crawl    bool
	seeds    listFlag
	jsonOut  bool
	timeout  time.Duration
}

// seedList возвращает накопленные стартовые хосты обхода.
func (o *discoverFlags) seedList() []string { return o.seeds.listOr("") }

// fileHostQueries - запросы, которые отбирают хосты по назначению, а не по
// порядку живости. Пул на полторы тысячи живых адресов состоит в основном из
// маркетплейсов и эскроу: файлов там нет, поэтому обход «первых N живых» давал
// каталог из пары десятков записей, почти целиком из одного видео.
//
// Запросы намеренно на английском и намеренно про содержание, а не про
// технологию: onion-поисковики индексируют тексты страниц, и «file hosting»
// находит раздающие хосты, тогда как «http server» нашёл бы что угодно.
var fileHostQueries = []string{
	"file hosting",
	"file mirror",
	"document library",
	"ebook library",
	"pdf books",
	"download archive",
	"shared files directory",
	"data dump",
	"leaked database archive",
	"public documents repository",
}

// engineFetch адаптирует поисковое ядро под parser.Fetcher: парсеру нужны
// только статус и тело, весь антидетект-транспорт остаётся внутри ядра.
type engineFetch struct{ eng *search.Engine }

func (e engineFetch) FetchURL(ctx context.Context, rawURL string) (parser.Response, error) {
	if e.eng == nil {
		return parser.Response{}, fmt.Errorf("поисковое ядро не инициализировано")
	}
	resp, err := e.eng.FetchURL(ctx, rawURL)
	if err != nil {
		return parser.Response{}, err
	}
	return parser.Response{Status: resp.Status, Body: resp.Body, URL: resp.URL}, nil
}

// statsDataOut - сводка по базе. Раньше здесь была map[string]any с обратным
// чтением files_bytes через type-switch: int64 из SQLite после JSON-круговорота
// приезжает как float64, и вывод врал. Структура держит int64 из рук в руки до
// самого принта.
//
// Поле Problems несёт источники, которые прочитать не удалось. Оно сериализуется
// как errors и в JSON, чтобы машину нельзя было обмануть нулём так же, как
// нельзя обмануть человека отсутствующей строкой предупреждения.
type statsDataOut struct {
	OnionTotal     int            `json:"onion_total"`
	OnionLive      int            `json:"onion_live"`
	FilesTotal     int            `json:"files_total"`
	FilesBytes     int64          `json:"files_bytes"`
	FilesByExt     map[string]int `json:"files_by_ext,omitempty"`
	TasksTotal     int            `json:"tasks_total"`
	TasksRunning   int            `json:"tasks_running"`
	HuntsTotal     int            `json:"hunts_total"`
	SelectorsTotal int            `json:"selectors_total"`
	JudgeVotes     int            `json:"judge_votes"`
	JudgeQueries   int            `json:"judge_queries"`
	JudgeHosts     int            `json:"judge_hosts"`
	BoostedHosts   int            `json:"boosted_hosts"`
	SchemaKnown    int            `json:"schema_known"`
	SchemaApplied  int            `json:"schema_applied"`
	SchemaRepaired []int          `json:"schema_repaired,omitempty"`
	Problems       []string       `json:"errors,omitempty"`
}
