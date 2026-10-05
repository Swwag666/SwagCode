package mcpserver

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"voidsearchswag/internal/catalog"
	"voidsearchswag/internal/discover"
	"voidsearchswag/internal/hunt"
	"voidsearchswag/internal/metrics"
	"voidsearchswag/internal/netx"
	"voidsearchswag/internal/parser"
	"voidsearchswag/internal/promote"
	"voidsearchswag/internal/router"
	"voidsearchswag/internal/search"
	"voidsearchswag/internal/searchers"
	"voidsearchswag/internal/store"
)

// taskSeq уникализирует task_id параллельных вызовов collect_files. Смоук
// этапа 179 (конкурентный стресс): два одновременных вызова в одну
// миллисекунду получали одинаковый «mcp-...462», CreateTask на конфликте id
// затирал чужую запись, и tasks_total терял задачу - а метки каталога двух
// прогонов становились неразличимы для file_search.
var taskSeq atomic.Int64

// finalizeBudget - запас времени на запись финального статуса задачи.
// Вынесен в переменную для теста: бюджет обязан отсчитываться от конца
// обхода, а не от начала вызова (смоук этапа 179: обход 60s при бюджете 30s
// от старта - UpdateTask на истёкшем контексте молча отказывал, и
// tasks_running монотонно рос).
var finalizeBudget = 30 * time.Second

type Deps struct {
	Version    string
	Store      *store.Store
	Rot        netx.Rotator
	Search     *search.Engine
	Discover   *discover.Pool
	Prober     *discover.Prober
	Collector  *catalog.Collector
	Parser     *parser.Parser
	Hunter     *hunt.Runner
	Promoter   *promote.Promoter
	Peers      []string
	PeerToken  string
	BackupDir  string
	BackupKeep int
	Srv        *server.MCPServer
	Started    time.Time
}

func New(d Deps) *server.MCPServer {
	s := server.NewMCPServer(
		"voidsearchswag",
		d.Version,
		server.WithToolCapabilities(true),
		server.WithPromptCapabilities(true),
		server.WithResourceCapabilities(false, false),
		server.WithInstructions("VoidSearchSwag - умный поисковый движок: clearnet/Tor поиск, авто-выбор режима (fast/stealth/deep), onion-разведка, кэш результатов, каталог файлов."),
		// Recovery включается явно и по умолчанию выключен. Без него паника в
		// обработчике инструмента выживала только случайно: по stdio её ловил
		// worker библиотеки, а по HTTP - net/http, который при этом обрывал
		// соединение вместо JSON-RPC-ошибки, и клиент видел молча оборвавшийся
		// SSE-поток без внятной причины. Обработчики ресурсов по stdio шли через
		// цикл чтения вообще без recover, то есть паника там была фатальной для
		// процесса.
		server.WithRecovery(),
		server.WithResourceRecovery(),
	)
	// Srv пробрасывается до регистрации хендлеров: method value копирует
	// ресивер в момент AddTool, поздняя запись сюда бы не попала.
	d.Srv = s
	s.AddTool(
		mcp.NewTool("status",
			mcp.WithDescription("Состояние VoidSearchSwag: версия, аптайм, транспорт, статистика базы и поисковых движков. db.tasks_total и tasks_running - задачи сбора файлов (collect_files), а не фоновая разведка: каждый вызов collect_files регистрирует задачу (running на время обхода, done или failed по исходу, обрезка срока - done с пояснением в задаче), фоновый crawl пополняет пул и каталог, не создавая записей в задачах. Блок search несёт onion_engines (число подключённых движков) и onion_live (число живых по последним замерам); число адресов пула - db.onion_total. Времена во всех ответах - UTC (суффикс Z); локальное время сервера появляется только в человекочитаемых артефактах: task_id, имена снимков бэкапов и строки лога"),
		),
		d.statusHandler,
	)
	s.AddTool(
		mcp.NewTool("search",
			mcp.WithDescription("Поиск с авто-выбором режима. fast - clearnet (DuckDuckGo), stealth - clearnet через cloaked-браузер, deep - onion-поисковики через tor плюс ahmia-мост без tor. В auto при пустой выдаче поиск сам идёт по цепочке фоллбэков (fast -> stealth -> deep). Явный mode - закон: подмены не будет, пустая выдача или мёртвые движки дают пустой ответ с пояснением в note отчёта. Все пояснения о потерях выдачи - поле note верхнего уровня ответа (зеркало report.note): токен-фильтр называет число выкинутых результатов, срез по limit - последний шаг после реранка и фильтра - называет, сколько релевантных результатов осталось за пределами топа. Параметр validate (по умолчанию выключен) проверяет первые 8 адресов живыми запросами и выкидывает мёртвые: без него выдача может нести ссылки с «no such host»; проверка стоит до 90 секунд и снимается, если не подтверждается ни один адрес. Предела ожидания в схеме нет: каждый движок ограничен сетевым бюджетом транспорта сервера, свои elapsed и флаг timeout видны в report.engines; отдельного потолка на вызов, кроме validate, не существует. stealth при недоступном cloaked-браузере (стартовый лог «браузер: ...») не отказывается и не предупреждает, а выполняется теми же HTTP-движками, что fast: браузерного движка в report.engines нет, и именно это - видимый след деградации. Состав движков deep между вызовами меняется: движок с недавними провалами отдыхает (кулдаун), в окно вызова не попадает и в engine_fail не входит. Числа report.engines[].count - ссылки движка до дедупа, токен-фильтра и среза limit: итоговая выдача короче, разница (дубли между движками, снятые limit'ом) - норма, а не потеря; отдельного счётчика дедупликации ответ не несёт. Страница самого поисковика несёт токены запроса в сниппете и токен-фильтром не отсекается."),
			mcp.WithString("query", mcp.Required(), mcp.Description("поисковый запрос")),
			mcp.WithString("mode", mcp.Description("auto|fast|stealth|deep (по умолчанию auto)")),
			mcp.WithNumber("limit", mcp.Description("максимум результатов, по умолчанию 20")),
			mcp.WithBoolean("no_cache", mcp.Description("игнорировать кэш при чтении; свежий прогон обновляет запись для следующих вызовов")),
			mcp.WithBoolean("validate", mcp.Description("проверить первые 8 адресов живыми запросами, выкинуть мёртвые (до 90 сек)")),
		),
		d.searchHandler,
	)
	s.AddTool(
		mcp.NewTool("route",
			mcp.WithDescription("Показать, какой режим выберет роутер для запроса и почему - без самого поиска. Поле signals появляется только при сработавших признаках (onion/утечки, операторские префиксы): пустое отсутствует, а не приходит пустым массивом. fallback в ответе - план порядка попыток, а не гарантия шагов: например, stealth требует браузера, и его доступность видна в стартовом логе сервера (строка «браузер: ...»), а не в ответе"),
			mcp.WithString("query", mcp.Required(), mcp.Description("поисковый запрос")),
		),
		d.routeHandler,
	)
	s.AddTool(
		mcp.NewTool("onion_health",
			mcp.WithDescription("Живость onion-поисковиков: пинг через tor, латентность, доля успехов, отключённые движки. last_probe каждой записи - UTC в RFC3339 до секунд; null значит «движок ещё не пробовали». Движок без единого замера приходит с probes=0 и live=false: это «не проверялся», а не «мёртв» - замер делают параметр probe (живой пинг сейчас) или фоновый ход поиска; пока замеров нет, search.onion_live=0 при непустом onion_engines в status читается именно как «замеров ещё не было», а не «все движки умерли»"),
			mcp.WithBoolean("probe", mcp.Description("сделать живой пинг прямо сейчас, а не отдать прошлые замеры")),
		),
		d.onionHealthHandler,
	)
	s.AddTool(
		mcp.NewTool("fetch",
			mcp.WithDescription("Забрать одну страницу целиком через антидетект-транспорт: статус, заголовки, распакованное тело"),
			mcp.WithString("url", mcp.Required(), mcp.Description("URL страницы")),
			mcp.WithNumber("max_chars", mcp.Description("ограничение длины тела, по умолчанию 20000")),
		),
		d.fetchHandler,
	)
	s.AddTool(
		mcp.NewTool("discover_onions",
			mcp.WithDescription("Onion-разведка: сбор адресов из clearnet-источников - восемь каналов: ahmia двумя (список адресов и поиск), dark.fail, onion.live, tor-watch, thehiddenwiki.org, thehiddenwiki.com, tor.link - с дедупликацией по адресу, при желании рекурсивный обход найденных сервисов через tor. Найденное пишется в пул. Фоновый тик разведки (bg_discover_runs в metrics) трёхфазный: сначала обход источников, затем crawl найденных сервисов (минуты на сотни адресов) и только затем запись батчем в пул - между стартом тика и ростом pool_total проходят минуты, это порядок фаз, а не потерянный тик. Параметр timeout (сек, 5..600, дефолт 120) обрезает обход, но не запись: всё собранное до обрезки доезжает до базы (адреса пула, мета, живость, файлы) под несократимым бюджетом, ответ несёт timeout_hit, а timeout_note называет, что именно срезал срок: обход (crawl.cancelled) или только замер и запись. Ответ приходит за секунды локальной записи после обрезки, а не за минуту сети: замер размеров файлов (сеть) живёт в бюджете вызова и после обрезки не ходит - недомерянные ссылки пишутся с size=0. Итог фазы замера виден в под-отчёте crawl: file_refs_measured/file_refs_unmeasured делят найденные ссылки на несущие известный размер (мера HEAD либо каталог прошлых прогонов) и оставшиеся с size=0 (не доехали до Content-Length: окно 60s, потолок 200 замеров, свежие ссылки первыми), а measure_cutoff называет срез окна замера до конца списка. Список адресов в ответе - окно offset/max_addresses (дефолт 0/200, потолок 2000) с addresses_total, полный список - через pool_status; порядок адресов не сортирован: сначала находки источников в порядке опроса каналов, затем новые адреса обхода. Счётчики new/updated - вклад этого вызова: new - адреса, которых в пуле не было; updated - только смена title или категории; живость, латентность и last_probe пишутся отдельным каналом пробы (probed) и в updated не входят: updated=0 при обновившейся живости записи - норма, не потеря; addresses_total - размер дедупнутого списка, собранного этим вызовом, а не размер пула: пул несёт и прошлые находки (полный список - pool_status), поэтому new=0 при больших addresses_total - норма, а не потеря: всё уже записано. found минус new - адреса, уже записанные в пуле до вызова: пересечение свежего сбора источников с накопленным пулом. Источник с ok=false может всё же принести адреса (частичная выдача уцелела ниже порога) - они участвуют в дедупе и записи наравне с успешными. Пул пишется батчем в одной транзакции: хвост записи на больших пулах - секунды, а не десятки секунд. max_hosts и per_host_delay_ms - применённые потолки и пауза; обход идёт хостами параллельно, per_host_delay_ms отсчитывает паузу между запросами к одному хосту, поэтому elapsed меньше суммы латентностей страниц. Файлы, найденные при crawl=true, пишутся в каталог под file_task_id из ответа: file_search task_id=<она> возвращает их; повторный обход не переписывает происхождение - его счётчик revisited в ответе discover, а под-отчёт crawl называет находки file_refs_found (ссылки), а не files (записи). Под-отчёт crawl несёт события стопа: limit_hit - только достигнутый потолок хостов, cancelled - обход обрезан отменой контекста (таймаут вызова), seeds_cut - сколько кандидатов не стало сидами из-за потолка max_hosts: срез именно списка сидов, не событие обхода (очередь найденных для обхода режется тем же потолком - это limit_hit «достигнут потолок хостов», а не seeds_cut); limit_hit и cancelled могут стоять одновременно. new_hosts в под-отчёте crawl - адреса, новые для этого обхода, а не для пула: вклад вызова в пул читается по new/updated корня. Размер файла в pages_detail всегда 0: замер живёт после обхода, агрегат file_refs несёт снятые размеры, недомерённые - с size=0. Поле depth под-отчёта - потолок глубины из конфигурации, depth у страницы в pages_detail - фактическая глубина этой страницы (0 - корневая): очередь может не дойти до потолка, и это не противоречие. crawl.found - уникальные хосты после дедупа: сумма addresses по страницам может быть выше на повторы. Поля событий сериализуются только когда событие случилось (omitempty): отсутствие поля значит «не случилось», а не false; latency_ms у страницы появляется при состоявшемся запросе - это замер GET, который потом сглаживается пулом в latency_avg. Текст ошибки context deadline exceeded встречается у двух разных пределов: собственный таймаут страницы (десятки секунд: запрос ушёл и не вернулся - честный отказ, живость пишется провалом) и обрезка бюджета вызова (запрос убит до предела страницы - за это живость не пишется, страница помечается cut, crawl-счётчик cut её называет, и в failed она не входит). probed - сколько записей живости лёгло в базу по итогам обхода; probe_failed - сбои самой записи в базу (не отказы хостов: отказ страницы - результат, он честно пишется провалом живости), probe_skipped - записи без вердикта (нет тор или прокси). Значение timeout вне 5..600 обрезается к границе: отдельного поля с применённым пределом нет, при срезе срока его называет timeout_note. Аналогично max_hosts и max_addresses приводятся к диапазону, а depth ниже нуля читается нулём. Источники в ответе - только начатые каналы: не стартовавший из-за обрезки канал не приходит вовсе (его видно вычитанием из восьми), начатый и упавший приходит с ok=false и причиной - это разные события. new_hosts под-отчёта crawl включает и операторские сиды: «новый для этого обхода» значит «страницу не обходили раньше», а не «оператор про хост не знает». file_task_id - метка каталога, а не гарантия выдачи: у файла одно происхождение, повторный обход оставляет записи под первой меткой (revisited-лог объясняет), file_search по свежей метке вернёт только новые файлы, note file_search называет причину пустоты. Поле elapsed - время прогона внутри сервера (сбор под бюджетом timeout плюс несократимая запись); сетевой и сериализационный хвост ответа в него не входит, фактическая длительность вызова может быть заметно дольше elapsed."),
			mcp.WithNumber("depth", mcp.Description("глубина рекурсивного обхода: 1 - только страницы найденных сервисов, 2 - и их ссылки, по умолчанию 2; 0 и меньше - глубина из конфига сервера (обычно 2). Без crawl=true параметр ни на что не влияет")),
			mcp.WithNumber("max_hosts", mcp.Description("потолок обходимых хостов от 1 до 1000: и срез сидов, и сам обход не шире этого числа; 0 и меньше - потолок из конфига сервера (обычно 50). Без crawl=true параметр ни на что не влияет")),
			mcp.WithBoolean("crawl", mcp.Description("обходить найденные сервисы, а не только собирать из источников")),
			mcp.WithString("seeds", mcp.Description("свои хосты для рекурсивного обхода (crawl) через запятую: меняется только список хостов обхода, опрос источников не отключается, и новые адреса всё равно пишутся в пул")),
			mcp.WithNumber("timeout", mcp.Description("потолок времени прогона в секундах, 5..600, по умолчанию 120")),
			mcp.WithNumber("max_addresses", mcp.Description("сколько адресов вернуть в окне, по умолчанию 200, потолок 2000; 0 - пустое окно, только счётчики")),
			mcp.WithNumber("offset", mcp.Description("смещение окна адресов в полном списке, по умолчанию 0")),
		),
		d.discoverHandler,
	)
	s.AddTool(
		mcp.NewTool("pool_status",
			mcp.WithDescription("Состояние onion-пула: сколько адресов известно, сколько живых, разбивка по статусам (live - жив по последней решающей пробе; dead - три провала подряд; unknown - живость не решена: проб не было, либо отказы не добрали порога смерти при отсутствии успеха) и записи накопленной статистики - живые и быстрые первыми (тот же порядок выборки, что у onion_search: success_rate вниз, latency вверх; при равных ключах - адрес по алфавиту, url ASC: одинаковая история соседей даёт один и тот же порядок от вызова к вызову), «последняя проба» - поле last_probe каждой записи, а не порядок списка: свежие по времени записи стоят не в голове. Каждая запись: url, status, latency_avg, success_rate, fail_streak, last_probe. last_probe - время последней пробы, а у попавших в пул без проб (свежие записи discover) - время попадания; очередью охвата такие всё равно читаются как ни разу не проверенные. success_rate - сглаженная живость (EMA: успех тянет к 1 на десятую, провал спускает на десятую; после первого успеха 0.1, после второго 0.19), а не доля успешных проб: одинаковые значения у соседей - одинаковая история исходов, а не копия глобального числа. fail_streak - провалы подряд после последнего успеха: live с fail_streak=2 - штатно, статус живёт до третьей неудачи. latency_avg - сглаженная латентность (четверть веса новой пробы; по обходу пишется фактическое время страницы, у записи без измеренной латентности первая успешная проба становится avg целиком - сглаживание включается со второй точки, у неуспешных проб латентность не переписывается; запись хранится в целых миллисекундах, дробная часть сглаживания отбрасывается: 1808.5 виден как 1808). category появляется в записи только присвоенной: сбор адресов категории не назначает, отсутствие ключа значит «не присвоена», а не пустая строка-обещание. total, live и by_status корня описывают пул целиком (полный состав), а status_filter и limit режут только entries - выборку: отфильтрованный ответ всё равно несёт полную разбивку, dead и unknown отдельными ключами корня не дублируются - их разложение живёт в by_status. success_rate в записях округлена до четырёх знаков после запятой. Значение status_filter вне live|unknown|dead - ошибка вызова: фильтр с опечаткой не молчит и не выдаёт весь пул за отфильтрованный."),
			mcp.WithString("status_filter", mcp.Description("фильтр по статусу: live|unknown|dead|пусто - все; прежнее имя параметра status тоже принимается")),
			mcp.WithNumber("limit", mcp.Description("сколько записей показать, по умолчанию 50, потолок 500; 0 и меньше читаются как 50")),
		),
		d.poolStatusHandler,
	)
	s.AddTool(
		mcp.NewTool("probe_pool",
			mcp.WithDescription("Пробы живости onion-пула через tor: волна строится по очереди охвата - сначала ни разу не проверенные адреса, затем unknown с историей (меньше отказов - раньше), затем самые устаревшие записи пула независимо от статуса: live и dead по старшинству last_probe; при равных приоритетах очередь доезжает по адресу (url ASC). Прогрев не держит пул замороженным: live обновляет латентность и может умереть тремя провалами подряд, dead при успехе воскрешается в live; воскрешённая запись без измеренной латентности (latency_avg=0) получает первую успешную меру целиком - сглаживание четвертью веса включается со второй точки, - а неуспешные пробы латентность не пишут вовсе. Статус, латентность и доля успехов пишутся в пул. Три провала подряд помечают сервис мёртвым. Поле cancelled - волна обрезана отменой контекста (таймаут вызова или сигнал); limit_hit у пробы не событие, потолок выборки задаётся параметром limit. Поля волны: total = live + dead + skipped; skipped - адреса волны, до которых запрос не дошёл: нет ни tor, ни прокси (это не смерть адреса); rejected - кандидаты, отброшенные до волны: невалидный onion или дубль выборки (rejected_addrs - их список, в total не входят); live_hosts - адреса удачных проб, по алфавиту; requested_limit - потолок выборки из пула, а не размер волны (в addr-режиме, когда проверяется один заданный адрес, выборки из пула нет и requested_limit приходит 0); concurrency, timeout_ms и delay_ms - применённые пределы волны (после подмены неположительных дефолтами); elapsed - длительность волны; записи results всегда идут по алфавиту адреса (url ASC): очередь приоритетов решает, кто попадает в волну, а не место записи в ответе, - время завершения проб на порядок тоже не влияет. Каждый адрес пробуется со своим пределом timeout_ms из ответа: error с latency_ms, примерно равным timeout_ms - обрезка контекста даёт небольшой разброс вокруг предела - до десятков миллисекунд в обе стороны, - проба не дождалась своего предела, а не отмена волны; причина отказа видна в error (context deadline exceeded либо транспортная, например socks connect), cancelled при этом не ставится. Счётчики live и dead считают исходы этой волны по одному на адрес: dead=3 значит, что у трёх разных адресов проба не удалась, а не что один сервис провалился трижды. Поле pool_status у записи - накопительный вердикт порогами, а не счётчик волны, поэтому dead=3 при pool_status=«unknown» читается так: ни один из трёх провалов не добрал собственного порога смерти (три подряд). Очередь строится по состоянию пула на момент вызова: параллельно идущие фоновые пробы обновляют last_probe и двигают границы групп охвата, поэтому состав волны меняется от вызова к вызову, и адрес, ранний по алфавиту, может опаздывать за более устаревшим соседом - очередь про охват, а не сортировку выдачи (записи results при этом всегда по алфавиту)."),
			mcp.WithNumber("limit", mcp.Description("сколько неизвестных адресов проверить, по умолчанию 20")),
			mcp.WithString("addr", mcp.Description("проверить один конкретный onion-адрес")),
			mcp.WithNumber("timeout_ms", mcp.Description("предел пробы одного адреса в миллисекундах, по умолчанию 20000")),
			mcp.WithNumber("delay_ms", mcp.Description("пауза между пробами одного хоста в миллисекундах, по умолчанию 2000")),
			mcp.WithNumber("concurrency", mcp.Description("сколько проб идёт параллельно, по умолчанию 16")),
		),
		d.probePoolHandler,
	)
	s.AddTool(
		mcp.NewTool("file_search",
			mcp.WithDescription("Поиск по каталогу собранных файлов: подстрока в имени, URL или странице-источнике, расширение, диапазон размера. Отдаёт записи и разбивку каталога по расширениям: returned - сколько записей отдано после limit, а by_ext - статистика всего каталога, не среза фильтра; общего счёта под фильтр и пагинации в ответе нет. Файлы, чей размер не удалось снять при сборе, под min_size/max_size не попадают - их отбирает unknown_size=true."),
			mcp.WithString("query", mcp.Description("подстрока в имени файла, его URL или адресе страницы, где он найден")),
			mcp.WithString("ext", mcp.Description("расширение без точки, одно или списком через запятую: zip, pdf или epub,pdf")),
			mcp.WithString("verdict", mcp.Description("метка категории, одна или списком через запятую: ebook,document или executable,secret")),
			mcp.WithBoolean("risk_only", mcp.Description("только исполняемый код и ключи")),
			mcp.WithString("task_id", mcp.Description("отобрать файлы конкретной задачи сбора")),
			mcp.WithNumber("min_size", mcp.Description("минимальный размер в байтах")),
			mcp.WithNumber("max_size", mcp.Description("максимальный размер в байтах")),
			mcp.WithBoolean("unknown_size", mcp.Description("только файлы неизвестного размера (сервер не снял размер с заголовков): под min_size/max_size они не попадают")),
			mcp.WithNumber("limit", mcp.Description("сколько записей вернуть, по умолчанию 100")),
		),
		d.fileSearchHandler,
	)
	s.AddTool(
		mcp.NewTool("onion_search",
			mcp.WithDescription("Поиск по собранной базе onion-сервисов: подстрока в заголовке, описании или адресе. По умолчанию мёртвые сервисы исключены. Каждая запись services: url, status, title/description/category при наличии, latency_avg_ms при измеренной латентности. С текстовым запросом первым ключом порядка идёт релевантность (совпадение в заголовке, потом в описании, потом в адресе), статистика - вторым: без запроса - живые и быстрые первыми (success_rate вниз, latency вверх); равные ключи разрешаются по алфавиту адреса."),
			mcp.WithString("query", mcp.Description("подстрока для поиска по заголовку, описанию или адресу")),
			mcp.WithString("status", mcp.Description("фильтр по статусу: live|unknown|dead")),
			mcp.WithBoolean("include_dead", mcp.Description("включить мёртвые сервисы в выдачу")),
			mcp.WithNumber("limit", mcp.Description("сколько записей вернуть, по умолчанию 50")),
		),
		d.onionSearchHandler,
	)
	s.AddTool(
		mcp.NewTool("collect_files",
			mcp.WithDescription("Обойти onion-сервисы и собрать файлы в каталог: ссылки с файловыми расширениями, размер через заголовки, запись в базу. Без аргумента hosts берутся живые хосты пула, а при их отсутствии - unknown-адреса (свежие, ни разу не проверенные). Параметр timeout (сек, 5..900, дефолт 300) обрезает обход, ответ несёт timeout_hit и счётчики успевшего (timeout_hit - булево с omitempty: выводится только при фактической обрезке, при её отсутствии поля в ответе нет, это норма, а не пропуск); собранное до обрезки доезжает до базы. Каждый вызов регистрируется задачей в db.tasks_total (status done по завершении, failed при ошибке, сообщение - счётчики исхода); task_id в ответе - и метка записей каталога, и id задачи. Финализация задачи не зависит от соединения клиента: уход клиента в середине обхода не оставляет задачу висящей в running, запись доезжает до done или failed независимо от контекста вызова. Новые файлы лежат под task_id задачи; повторные находки (файл уже в каталоге от прежнего прогона) считаются полем revisited - происхождение первой находки не переписывается, и file_search по свежему task_id вернёт только новое. task_id уникален при параллельных вызовах (порядковый суффикс). links - все найденные ссылки, saved - новые записи каталога, revisited - повторные (в каталоге от прежнего прогона или от параллельного вызова, выигравшего гонку записи): их сумма - все находки прогона, by_ext разбивает находки по расширениям. failed_hosts - карта «хост -> причина отказа» для каждого хоста из failed: битый адрес (отказ за миллисекунды) и молчащий хост (таймаут страницы) различимы по тексту причины, а не только по elapsed. elapsed может превышать заданный потолок на миллисекунды: срок режет обход, а локальная финализация записи и статистики идёт после него. Повторный обход живого onion-хоста недетерминирован: сам сервис может отдать разный набор ссылок в двух прогонах, и links между вызовами отличается - это свойство источника, а не рассинхрон каталога. Потолок max_files считается по saved: повторные находки не съедают его, поэтому links может превысить max_files без события limit_hit; limit_hit (строка) ставится только когда сохранённых стало max_files, и отсутствует, если лимит не достигнут."),
			mcp.WithArray("hosts", mcp.Description("хосты или URL для обхода"), mcp.WithStringItems()),
			mcp.WithNumber("limit", mcp.Description("сколько живых хостов взять из пула, если hosts не заданы")),
			mcp.WithNumber("max_files", mcp.Description("потолок файлов за прогон")),
			mcp.WithNumber("timeout", mcp.Description("потолок времени сбора в секундах, 5..900, по умолчанию 300")),
		),
		d.collectFilesHandler,
	)
	d.registerPrompts(s)
	d.registerExtended(s)
	d.registerResources(s)
	return s
}

func (d Deps) statusHandler(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	out := map[string]any{
		"version": d.Version,
		"uptime":  time.Since(d.Started).Round(time.Second).String(),
	}
	if d.Rot != nil {
		out["transport"] = map[string]any{
			"kind":    d.Rot.Kind(),
			"spec":    netx.MaskSpec(d.Rot.TransportSpec()),
			"healthy": d.Rot.Healthy(),
		}
	} else {
		out["transport"] = map[string]any{"kind": "none"}
	}
	if d.Store != nil {
		// Общий сборщик отдаёт всю сводку, а не только пул и задачи, как было
		// раньше: статус - это диагностика, и неполный набор источников в нём
		// означает, что часть картины оператор не видит вовсе. Непрочитанные
		// источники перечислены в db["errors"], поэтому отсутствие числа больше
		// неотличимо от поломки.
		db, _ := dbSummary(ctx, d.Store)
		out["db"] = db
	}
	if d.Search != nil {
		engines := map[string]any{"onion_engines": len(d.onionEngineNames())}
		if live, total := d.onionCounts(); total > 0 {
			engines["onion_live"] = live
			// Этап 178 (смоук-G): ключ onion_total убран из блока search -
			// он дублировал onion_engines и одновременно сталкивался с
			// db.onion_total (адреса пула) тем же именем в одном ответе.
			// Слепой прогон видел два onion_total с разными значениями и
			// трактовал это как противоречие.
		}
		out["search"] = engines
	}
	return jsonResult(out)
}

func (d Deps) searchHandler(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if d.Search == nil {
		return mcp.NewToolResultError("поисковый движок не инициализирован"), nil
	}
	query, err := req.RequireString("query")
	if err != nil {
		return mcp.NewToolResultError("параметр query обязателен"), nil
	}
	modeRaw := req.GetString("mode", "")
	mode, ok := router.Parse(modeRaw)
	if !ok {
		return mcp.NewToolResultError("неизвестный режим " + modeRaw + " (ожидается auto|fast|stealth|deep)"), nil
	}
	limit := req.GetInt("limit", 20)
	if limit <= 0 {
		limit = 20
	}
	if limit > 200 {
		limit = 200
	}
	out, err := d.Search.Search(ctx, search.Options{
		Query:    query,
		Mode:     mode,
		Limit:    limit,
		NoCache:  req.GetBool("no_cache", false),
		Validate: req.GetBool("validate", false),
	})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return jsonResult(out)
}

func (d Deps) routeHandler(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	query, err := req.RequireString("query")
	if err != nil {
		return mcp.NewToolResultError("параметр query обязателен"), nil
	}
	return jsonResult(router.Route(query))
}

func (d Deps) onionHealthHandler(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if d.Search == nil {
		return mcp.NewToolResultError("поисковый движок не инициализирован"), nil
	}
	out := map[string]any{}
	if req.GetBool("probe", false) {
		live, total, skipped := d.Search.ProbeOnion(ctx)
		out["live"] = live
		out["total"] = total
		out["probed"] = true
		if skipped > 0 {
			// Без tor проверка не состоялась, и прежнее «live=0 total=7»
			// читалось клиентом как массовая смерть движков.
			out["skipped"] = skipped
			out["note"] = searchers.ProbeReport{Total: total, Skipped: skipped}.Note()
		}
	}
	out["engines"] = engineHealthOut(d.Search.HealthReport())
	if _, ok := out["live"]; !ok {
		live, total := d.onionCounts()
		out["live"] = live
		out["total"] = total
		out["probed"] = false
	}
	return jsonResult(out)
}

// engineHealthOut переводит снимок движков в ответ инструмента onion_health.
// Этап 178 (смоук G-8, I-9): прежде время шло в time.Time с тегом по умолчанию
// - RFC3339Nano в локальной зоне сервера: +07:00 с наносекундами. Слепой
// прогон видел «2026-02-05T12:44:06.9454692+07:00» и не мог ни сравнить его с
// RFC3339-метками остальных ответов (UTC), ни найти секунды под наносекундами.
// Теперь: UTC, секунды, нулевое время (движок ещё не пробовали) - null.
func engineHealthOut(list []searchers.EngineHealth) []map[string]any {
	out := make([]map[string]any, 0, len(list))
	for _, e := range list {
		row := map[string]any{
			"name": e.Name, "url": e.URL, "live": e.Live,
			"latency_avg_ms": e.LatencyAvg, "success_rate": e.SuccessRate,
			"fail_streak": e.FailStreak, "probes": e.Probes,
			"successes": e.Successes, "disabled": e.Disabled,
		}
		if e.LastProbe.IsZero() {
			row["last_probe"] = nil
		} else {
			row["last_probe"] = e.LastProbe.UTC().Format(time.RFC3339)
		}
		if e.LastError != "" {
			row["last_error"] = e.LastError
		}
		out = append(out, row)
	}
	return out
}

func (d Deps) fetchHandler(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if d.Search == nil {
		return mcp.NewToolResultError("поисковый движок не инициализирован"), nil
	}
	rawURL, err := req.RequireString("url")
	if err != nil {
		return mcp.NewToolResultError("параметр url обязателен"), nil
	}
	maxChars := req.GetInt("max_chars", 20000)
	if maxChars <= 0 {
		maxChars = 20000
	}
	resp, err := d.Search.FetchURL(ctx, rawURL)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	body := resp.Text()
	truncated := false
	if len(body) > maxChars {
		body = body[:maxChars]
		truncated = true
	}
	// Этап 178 (смоук-G): описание обещает «статус, заголовки, распакованное
	// тело», а заголовков в ответе не было - только однострочный proto.
	// Канонические имена (Content-Type) сохранены как пришли от транспорта.
	return jsonResult(map[string]any{
		"url":       resp.URL,
		"status":    resp.Status,
		"proto":     resp.Proto,
		"headers":   resp.Header,
		"duration":  resp.Duration.Round(time.Millisecond).String(),
		"length":    len(resp.Body),
		"truncated": truncated,
		"body":      body,
	})
}

// Ограничения на целочисленные аргументы инструментов.
//
// Они обязательны, а не косметичны. Значения аргументов приходят из
// JSON-RPC-запроса и дальше попадают в `make([]T, 0, limit)` в слое хранилища.
// Запрос вида {"limit": 9000000000000000000} - валидное положительное int64,
// которое доходит до make и роняет процесс с "makeslice: cap out of range";
// {"limit": 1000000000} даёт выделение на сотни гигабайт и OOM. Оба варианта
// убивают сервер одним вызовом инструмента.
//
// Потолок задан по реальному смыслу, а не «побольше»: пул и каталог не имеют
// тысяч записей, которые кто-то стал бы запрашивать одним вызовом, поэтому
// ограничение не мешает нормальной работе и не даёт упасть.
const (
	maxToolLimit    = 1000
	maxToolHosts    = 1000
	maxToolResults  = 200
	maxToolChars    = 200000
	maxCollectFiles = 100000
)

// clampInt приводит аргумент инструмента к рабочему диапазону.
func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func (d Deps) discoverHandler(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if d.Discover == nil {
		return mcp.NewToolResultError("discovery-слой не инициализирован (нужен тор и база)"), nil
	}
	// Этап 163: потолок времени на вызов. Живой замер ДО (смоук v0.1.0):
	// discover_onions шёл 224 секунды, клиент отваливался по 180s, а сервер
	// продолжал обход в одиночестве. Дефолт 120s покрывает обычный прогон
	// (источники + обход depth 2), потолок 600s оставляет место глубоким
	// сидам; всё, что свыше, обрезается с честным timeout_hit.
	timeout := clampInt(req.GetInt("timeout", 120), 5, 600)
	rctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	opts := discover.Options{
		Depth: req.GetInt("depth", 2),
		// Этап 171: молчание и неположительное значение - потолок из
		// конфига сервера, а не жёсткие 50. Прежде дефолт совпадал с
		// конфигом только случайно: на сервере с
		// VOIDSEARCH_DISCOVER_MAX_HOSTS=7 молчаливый вызов резал 50
		// сидов, и обходчик честно отчитывался max_hosts=7 при
		// limit_hit «достигнут потолок хостов» - потолок был чужим.
		// Явное значение клампится от нуля: 0 и минус означают «конфиг»,
		// та же договорённость, что у CLI-флага --max-hosts.
		MaxHosts:  clampInt(req.GetInt("max_hosts", 0), 0, maxToolHosts),
		WithCrawl: req.GetBool("crawl", false),
	}
	if opts.Depth < 0 {
		opts.Depth = 0
	}
	if raw := req.GetString("seeds", ""); raw != "" {
		for _, s := range strings.Split(raw, ",") {
			if s = strings.TrimSpace(s); s != "" {
				opts.Seeds = append(opts.Seeds, s)
			}
		}
	}
	res, err := d.Discover.Run(rctx, opts)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	// Пагинация адресов: живой ответ v0.1.0 нёс 12151 адресов (890KB
	// JSON), из которых вызывающему нужны первые десятки. Полный список
	// живёт в базе и доступен через pool_status, поэтому здесь - окно со
	// offset/max_addresses и счётчиками, а не выгрузка всего пула.
	total := len(res.Addresses)
	offset := req.GetInt("offset", 0)
	if offset < 0 {
		offset = 0
	}
	if offset > total {
		offset = total
	}
	// Этап 178 (смоук-I): ноль - легальное пустое окно, а не «дефолт».
	// Прежний clamp(…, 1, …) превращал явный max_addresses=0 в один адрес:
	// агент просил «не возвращать адреса, только счётчики», а получал
	// одинокую запись и трактовал это как мусор в окне. Молчание
	// параметра по-прежнему даёт 200: GetInt отличает отсутствие ключа
	// от нуля через дефолт.
	maxAddr := clampInt(req.GetInt("max_addresses", 200), 0, 2000)
	end := offset + maxAddr
	if end > total {
		end = total
	}
	page := append([]string(nil), res.Addresses[offset:end]...)
	res.Addresses = page
	out := struct {
		discover.Result
		AddressesTotal  int    `json:"addresses_total"`
		AddressesOffset int    `json:"addresses_offset"`
		Truncated       bool   `json:"truncated"`
		TimeoutHit      bool   `json:"timeout_hit,omitempty"`
		TimeoutNote     string `json:"timeout_note,omitempty"`
	}{Result: res, AddressesTotal: total, AddressesOffset: offset, Truncated: end < total}
	if rctx.Err() != nil {
		out.TimeoutHit = true
		// Этап 174: note называет, ЧТО именно срезал срок. Прежний
		// единый текст «источники и обход могли не закончиться» врал в
		// двух живых случаях: смоук этапа 172 (агент B, timeout=90) -
		// crawl закончился сам за 65с, срок съели дозапись и замер, а
		// ответ обвинял несуществующую обрезку обхода; смоук этапа 172
		// (агент A, timeout=15) - crawl обрезан отменой, и note не
		// называл виновника, хотя crawl.cancelled был рядом. Ветка
		// выбирается по фактам отчёта: обхода не было / обход обрезан /
		// обход закончился сам.
		note := fmt.Sprintf("прогон обрезан по сроку %ds", timeout)
		switch {
		case res.Crawl == nil:
			note += ": источники могли не закончиться, пул дозаполнен частично"
		case res.Crawl.Cancelled:
			note += ": обход обрезан отменой (crawl.cancelled=true), источники могли не закончиться, собранное доезжает до базы"
		default:
			note += ": источники и обход закончились, срок ушёл на замер и запись собранного (недомерённые файлы пишутся с size=0)"
		}
		out.TimeoutNote = note
	}
	return jsonResult(out)
}

func (d Deps) poolStatusHandler(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if d.Store == nil {
		return mcp.NewToolResultError("база не инициализирована"), nil
	}
	// Этап 178 (смоук-H): описание зовёт фильтр status_filter, а схема
	// принимала только status - и неправильное имя глоталось молча: смоук
	// передал status_filter="dead" и получил все записи. Основным именем
	// становится status_filter (как в тексте описания), прежний status
	// остаётся рабочим алиасом, чтобы клиенты прошлых этапов не сломались.
	status := req.GetString("status_filter", "")
	if status == "" {
		status = req.GetString("status", "")
	}
	// Этап 178 (смоук-I): неверное значение фильтра - ошибка вызова, а не
	// «фильтр выключен». Прежний статус-фильтр с опечаткой (status_filter=
	// "alive") молча отдавал весь пул: агент считал фильтр примённым и
	// делал выводы о пуле из неотфильтрованных данных. Перечень допустимых
	// значений тот же, что в описании инструмента: live, unknown, dead.
	switch status {
	case "", "live", "unknown", "dead":
	default:
		return mcp.NewToolResultError(
			"неизвестный статус " + status + " (ожидается live|unknown|dead)"), nil
	}
	limit := req.GetInt("limit", 50)
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}

	out := map[string]any{"status_filter": status, "limit": limit}
	// Сводка пула общим сборщиком: список непрочитанных источников попадает в
	// поле errors, и клиент отличает пустой пул от базы, которую не удалось
	// прочитать. Прежняя версия при ошибке не клала ключи total, live и
	// by_status, и ответ выглядел как пул без адресов.
	pool, _ := poolSummary(ctx, d.Store)
	for k, v := range pool {
		out[k] = v
	}

	onions, err := d.Store.ListOnions(ctx, status, limit)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	entries := make([]map[string]any, 0, len(onions))
	for _, o := range onions {
		e := map[string]any{
			"url":          o.URL,
			"status":       o.Status,
			"latency_avg":  o.LatencyAvg,
			"success_rate": math.Round(o.SuccessRate*1e4) / 1e4,
			"fail_streak":  o.FailStreak,
		}
		// Этап 175: category появляется только присвоенной. Сбор адресов
		// категории не назначает (ни один источник их не несёт), и по
		// живой базе 14248 записей поле стояло пустой строкой у каждой:
		// ключ-обещание, которое никогда не исполняется. Отсутствие поля
		// читается как «не присвоена» - тот же договор, что у событий
		// этапа 174: absent значит «не случилось».
		if o.Category != "" {
			e["category"] = o.Category
		}
		if !o.LastProbe.IsZero() {
			e["last_probe"] = o.LastProbe.UTC().Format(time.RFC3339)
		}
		entries = append(entries, e)
	}
	out["entries"] = entries
	out["returned"] = len(entries)
	return jsonResult(out)
}

func (d Deps) collectFilesHandler(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if d.Store == nil {
		return mcp.NewToolResultError("база не инициализирована"), nil
	}
	if d.Collector == nil {
		return mcp.NewToolResultError("сборщик не инициализирован"), nil
	}

	hosts := req.GetStringSlice("hosts", nil)
	if len(hosts) == 0 {
		limit := clampInt(req.GetInt("limit", 10), 1, maxToolHosts)
		var err error
		hosts, err = discover.Known(ctx, d.Store, "live", limit)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if len(hosts) == 0 {
			hosts, err = discover.Known(ctx, d.Store, "unknown", limit)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
		}
		if len(hosts) == 0 {
			return mcp.NewToolResultError("пул пуст: сначала соберите адреса через discover_onions"), nil
		}
	}

	// max_files применяется к копии, а не к общему сборщику.
	//
	// Раньше здесь было `d.Collector.Cfg.MaxFiles = n`. Это сразу три дефекта
	// в одной строке. Первый - гонка: Cfg читается воркерами предыдущего
	// Collect, пока обработчик её переписывает. Второй - утечка состояния
	// между запросами: условие `n > 0` не позволяло вернуть значение назад,
	// поэтому один вызов с max_files=5 молча ограничивал все последующие
	// collect_files до конца жизни процесса, включая те, что аргумент не
	// передавали. Третий - фоновый тик и CLI пользуются своими сборщиками,
	// так что сервер и консоль начинали расходиться в настройках.
	//
	// Cfg в Collector - значение, остальные поля интерфейсы и указатели,
	// поэтому поверхностная копия разделяет клиент и хранилище, но получает
	// собственную конфигурацию. Именно то, что нужно для одного вызова.
	collector := *d.Collector
	if n := clampInt(req.GetInt("max_files", 0), 0, maxCollectFiles); n > 0 {
		collector.Cfg.MaxFiles = n
	}

	// Этап 163: потолок времени на вызов. Живой замер ДО (смоук v0.1.0):
	// collect_files шёл 4m05s молча. Дефолт 300s - нормальный обход десятка
	// живых хостов, потолок 900s - глубокие каталоги. При обрезке вызывающий
	// получает timeout_hit и счётчики успевшего.
	timeout := clampInt(req.GetInt("timeout", 300), 5, 900)
	rctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	// Миллисекунды в метке - этап 168: два collect_files в одну секунду
	// наследовали один task_id, и записи задач смешивались при file_search
	// по метке. Секунды хватало, пока между вызовами жили минуты обхода.
	// Этап 179 (смоук-C, Д1): миллисекунд мало при параллельных вызовах -
	// два одновременных collect_files получали одинаковый id, CreateTask
	// на конфликте затирал чужую запись, tasks_total терял задачу, а
	// file_search по метке смешивал прогоны. Порядковый номер снимает
	// коллизию целиком.
	taskID := fmt.Sprintf("mcp-%s.%d", time.Now().Format("20060102-150405.000"), taskSeq.Add(1))
	// Этап 178 (смоук-G): задача регистрируется в таблице tasks. Описание
	// status обещает «tasks_total и tasks_running - задачи сбора файлов
	// (collect_files)», но до правки collect_files писал метку только в
	// file_catalog, таблица tasks не имела ни одного вызывающего, и
	// tasks_total оставался нулём после успешного сбора - описание врало.
	// Прогон идёт синхронно, поэтому «running» здесь не видно другому
	// клиенту надолго: максимум на время обхода.
	//
	// Этап 178 (смоук-J, Д1): записи задач уходят с контекста финализации,
	// а не с контекстом запроса. HTTP-транспорт mcp-go отменяет ctx
	// обработчика при уходе клиента, и UpdateTask на отменённом контексте
	// отказывал молча: задача, чей обход честно закончился (или был
	// обрезан потолком 300/900с), оставалась running навсегда - смоук J
	// наблюдал tasks_running=1 спустя 17 минут после остановки прогресса.
	// Обход по-прежнему обрезается rctx (контракт потолка времени не
	// меняется), но судьба записи переживает отключение клиента.
	//
	// Этап 179 (смоук-C, Д3): бюджет финализации создавался один раз, в
	// начале вызова, и 30-секундный запас тратился на САМ обход: после
	// 60-секундного сбора UpdateTask падал на истёкшем контексте так же
	// молча, как в смоуке J, - tasks_running монотонно рос (смоук видел
	// 8 running из 9 спустя полминуты после всех ответов). Финальный
	// статус получает СВОЙ бюджет, отсчитанный от конца обхода.
	regCtx, regCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer regCancel()
	var taskErr string
	if err := d.Store.CreateTask(regCtx, store.Task{
		ID: taskID, Kind: "collect_files", Status: "running",
		Message: fmt.Sprintf("хостов %d", len(hosts)),
	}); err != nil {
		// Регистрация - не условие сбора: файлы собираются и без неё, отказ
		// базы на задачах не должен ронять сам сбор. Ошибка названа в ответе.
		taskErr = fmt.Sprintf("задача: %v", err)
	}
	rep, err := collector.Collect(rctx, hosts, taskID)
	finCtx, finCancel := context.WithTimeout(context.WithoutCancel(ctx), finalizeBudget)
	defer finCancel()
	if err != nil {
		_ = d.Store.UpdateTask(finCtx, taskID, "failed", 0, err.Error(), "")
		return mcp.NewToolResultError(err.Error()), nil
	}
	// Статус задачи пишется и при обрезке срока: «done» с пояснением честнее,
	// чем висящий running - прогон завершён, просто не всё успел.
	taskMsg := fmt.Sprintf("файлов %d, страниц %d", rep.Saved, rep.Pages)
	taskStatus := "done"
	if rctx.Err() != nil {
		taskMsg = fmt.Sprintf("обрезан по сроку %ds: %s", timeout, taskMsg)
	}
	_ = d.Store.UpdateTask(finCtx, taskID, taskStatus, 100, taskMsg, "")
	out := map[string]any{
		"task_id": taskID,
		"hosts":   rep.Hosts,
		"pages":   rep.Pages,
		"links":   rep.Links,
		"saved":   rep.Saved,
		// skipped и failed добавлены вместе с состоянием каталога: без них
		// saved=0 не отличал «хосты не дали файловых ссылок» от «запись в базу
		// не удалась». CLI печатает оба счётчика с этапа 67, а MCP-клиент
		// получал тот же catalog.Report урезанным: два потребителя одного
		// отчёта видели разное.
		"skipped":   rep.Skipped,
		"failed":    rep.Failed,
		"revisited": rep.Revisited,
		"elapsed":   rep.Elapsed,
	}
	if taskErr != "" {
		withProblems(out, []string{taskErr})
	}
	if rctx.Err() != nil {
		out["timeout_hit"] = true
		out["timeout_note"] = fmt.Sprintf(
			"сбор обрезан по сроку %ds: часть хостов не обойдена, страниц прочитано %d, хостов было %d", timeout, rep.Pages, rep.Hosts)
	}
	// Этап 181 (смоук-180): by_ext публикуется всегда, включая пустую
	// карту. Прежде поле появлялось только при находках, и клиент не мог
	// отличить «сбор без файлов» от «ответ обрезан»: в одном вызове by_ext
	// был, в двух соседних отсутствовал. Пустая карта - такой же честный
	// итог, как нули в saved: схема ответа одинакова для пустого и
	// непустого сбора.
	out["by_ext"] = rep.ByExt
	// Этап 182 (смоук-181): причины отказавших хостов. Прежде клиент видел
	// failed=1 за 31.6с и отличал битый адрес от молчащего хоста только по
	// elapsed; причина лежала в скрытом служебном логе. Карта всегда
	// согласована со счётчиком: по каждому ключу - текст ошибки обхода.
	if len(rep.FailedHosts) > 0 {
		out["failed_hosts"] = rep.FailedHosts
	}
	if len(rep.SampleURLs) > 0 {
		out["sample_urls"] = rep.SampleURLs
	}
	if rep.LimitHit != "" {
		out["limit_hit"] = rep.LimitHit
	}
	// Итог каталога читается уже после сбора, а сбор терпит незаписанные файлы,
	// поэтому ошибка здесь достижима: на сломанной таблице сбор «проходит»,
	// статистика нет. Прежняя версия при ferr != nil просто не добавляла три
	// ключа, и клиент не мог отличить неполный ответ от ответа про пустой
	// каталог.
	total, bytes, byExt, ferr := d.Store.FileStats(ctx)
	if ferr != nil {
		withProblems(out, []string{fmt.Sprintf("файлы: %v", ferr)})
	} else {
		out["catalog_total"] = total
		out["catalog_bytes"] = bytes
		out["catalog_by_ext"] = byExt
	}
	return jsonResult(out)
}

func (d Deps) onionSearchHandler(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if d.Store == nil {
		return mcp.NewToolResultError("база не инициализирована"), nil
	}
	text := req.GetString("query", "")
	status := req.GetString("status", "")
	includeDead := req.GetBool("include_dead", false)
	limit := req.GetInt("limit", 50)

	onions, err := d.Store.SearchOnions(ctx, text, status, limit, includeDead)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	out := map[string]any{"returned": len(onions), "query": text}
	// Состояние пула читается из той же таблицы, что и основной запрос, поэтому
	// изолированная поломка сюда практически не доходит: SearchOnions упадёт
	// раньше, и инструмент вернёт ошибку. Достижимый сценарий - сбой между двумя
	// запросами: истёк контекст, база заблокирована другим процессом. Молчание
	// означало бы отсутствие pool_total и pool_live без объяснения, поэтому
	// ошибка называется так же, как в dbSummary на этапе 66.
	total, live, err := d.Store.OnionStats(ctx)
	if err != nil {
		withProblems(out, []string{fmt.Sprintf("пул: %v", err)})
	} else {
		out["pool_total"] = total
		out["pool_live"] = live
	}

	entries := make([]map[string]any, 0, len(onions))
	for _, o := range onions {
		e := map[string]any{"url": o.URL, "status": o.Status}
		if o.Title != "" {
			e["title"] = o.Title
		}
		if o.Description != "" {
			e["description"] = o.Description
		}
		if o.Category != "" {
			e["category"] = o.Category
		}
		if o.LatencyAvg > 0 {
			e["latency_avg_ms"] = o.LatencyAvg
		}
		entries = append(entries, e)
	}
	out["services"] = entries
	return jsonResult(out)
}

func (d Deps) fileSearchHandler(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if d.Store == nil {
		return mcp.NewToolResultError("база не инициализирована"), nil
	}
	// Фильтры те же, что принимает команда files в консоли: -query, -ext,
	// -verdict, -risk, -task, -min-size, -max-size, -limit. Разбор списка
	// расширений и меток живёт в хранилище, поэтому инструменту достаточно
	// передать строку как есть, и оба входа отвечают одинаково. До правки
	// инструмент отдавал verdict в каждой записи ответа, но отобрать по нему не
	// давал: агент видел метку и не мог ею воспользоваться.
	q := store.FileQuery{
		Text:        req.GetString("query", ""),
		Ext:         req.GetString("ext", ""),
		Verdict:     req.GetString("verdict", ""),
		RiskOnly:    req.GetBool("risk_only", false),
		TaskID:      req.GetString("task_id", ""),
		MinSize:     int64(req.GetInt("min_size", 0)),
		MaxSize:     int64(req.GetInt("max_size", 0)),
		UnknownSize: req.GetBool("unknown_size", false),
		Limit:       req.GetInt("limit", 100),
	}
	files, err := d.Store.SearchFiles(ctx, q)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	out := map[string]any{"returned": len(files)}
	// То же, что в onion_search: основной запрос читает file_catalog раньше,
	// поэтому изолированная поломка таблицы сюда доходит только как сбой между
	// двумя запросами. Без else клиент получил бы ответ без catalog_total и без
	// объяснения, почему его нет.
	total, bytes, byExt, err := d.Store.FileStats(ctx)
	if err != nil {
		withProblems(out, []string{fmt.Sprintf("файлы: %v", err)})
	} else {
		out["catalog_total"] = total
		out["catalog_bytes"] = bytes
		out["by_ext"] = byExt
	}

	// Подсказка про неизвестный размер: размерные фильтры такие записи
	// исключают по смыслу (неизвестно, сколько они весят), и без пояснения
	// пустой ответ читается как «файлов нет». Смоук-агент этапа 166 поймал
	// ровно это: 7 файлов с size=0 в каталоге, max_size=100 - пустота
	// без единого слова, и достать их нечем.
	if (q.MinSize > 0 || q.MaxSize > 0) && len(files) == 0 && !q.UnknownSize {
		if n, uErr := d.Store.FileUnknownCount(ctx); uErr == nil && n > 0 {
			out["unknown_size_files"] = n
			out["note"] = fmt.Sprintf(
				"размерный фильтр не вернул ничего, но в каталоге %d файлов неизвестного размера: min_size/max_size их не показывают, unknown_size=true достанет", n)
		}
	}
	if q.UnknownSize {
		out["unknown_size_only"] = true
	}

	entries := make([]map[string]any, 0, len(files))
	for _, f := range files {
		e := map[string]any{
			"id":       f.ID,
			"task_id":  f.TaskID,
			"url":      f.URL,
			"filename": f.Filename,
			"ext":      f.Ext,
			"size":     f.Size,
			"found_at": f.FoundAt.Format(time.RFC3339),
		}
		if f.MIME != "" {
			e["mime"] = f.MIME
		}
		if f.SourcePage != "" {
			e["source_page"] = f.SourcePage
		}
		if f.Verdict != "" {
			e["verdict"] = f.Verdict
		}
		entries = append(entries, e)
	}
	out["files"] = entries
	// Пустой результат по метке задачи - не «файлов нет»: повторный обход
	// находит те же ссылки, но AddFile держит происхождение первой записи,
	// и под меткой повтора записей не появится, сколько ни ищи. Живой
	// замер этапа 168: file_search по file_task_id из свежего discover
	// вернул files=[] без единого слова, и агент попытался «перезаписать»
	// задачу третьим discover. Здесь ответ обязан назвать причину.
	if q.TaskID != "" && len(files) == 0 {
		out["note"] = fmt.Sprintf(
			"под меткой %q записей нет: у файла одно происхождение, и повторные обходы оставляют записи под первой меткой (счётчик revisit в ответе discover); file_save_failed в discover назвал бы отказы записи",
			q.TaskID)
	}
	return jsonResult(out)
}

func (d Deps) probePoolHandler(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if d.Prober == nil {
		return mcp.NewToolResultError("пробы не инициализированы (нужен тор)"), nil
	}
	// Этап 178 (смоук-H): пределы волны обязаны доходить до пробера. До
	// правки параметры timeout_ms/delay_ms/concurrency не были зарегистрированы
	// в схеме, не читались и не применялись: волна всегда шла на серверных
	// 20000/2000/16 при ответе, обещающем «применённые пределы волны».
	timeoutMS := req.GetFloat("timeout_ms", 0)
	delayMS := req.GetFloat("delay_ms", 0)
	concurrency := req.GetFloat("concurrency", 0)
	prober := d.Prober.WithWaveLimits(int64(timeoutMS), int64(delayMS), int64(concurrency))
	if addr := req.GetString("addr", ""); addr != "" {
		rep := prober.Probe(ctx, []string{addr})
		return jsonResult(rep)
	}
	limit := req.GetInt("limit", 20)
	if limit <= 0 {
		limit = 20
	}
	if limit > 1000 {
		limit = 1000
	}
	rep, err := prober.ProbeWave(ctx, limit)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return jsonResult(rep)
}

func (d Deps) onionCounts() (live, total int) {
	list := d.Search.HealthReport()
	if len(list) == 0 {
		return 0, len(d.onionEngineNames())
	}
	for _, e := range list {
		total++
		if e.Live && !e.Disabled {
			live++
		}
	}
	return live, total
}

func (d Deps) onionEngineNames() []string {
	if d.Search == nil || d.Search.Onion == nil {
		return nil
	}
	// Snapshot, а не обход Engines: вызов идёт из goroutine MCP-запроса,
	// а фоновый промоут в это же время дописывает движки в каталог.
	engines := d.Search.Onion.Snapshot()
	names := make([]string, 0, len(engines))
	for _, e := range engines {
		names = append(names, e.Name())
	}
	return names
}

// maxRequestBody ограничивает размер тела входящего JSON-RPC-запроса.
//
// Без границы тело буферизуется целиком до того, как сработает любое
// ограничение внутри обработчика, поэтому запрос на несколько сотен
// мегабайт занимал память ещё до разбора аргументов. 8 МБ с запасом
// покрывают и пачку голосов judge_submit, и список хостов collect_files.
const maxRequestBody = 8 << 20

// limitBody оборачивает обработчик ограничением на размер тела.
func limitBody(h http.Handler, max int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, max)
		}
		h.ServeHTTP(w, r)
	})
}

// charsetWriter дотягивает charset=utf-8 до JSON-ответов.
//
// Живой замер смоук-агента этапа 168: POST /mcp отвечал
// Content-Type: application/json без параметров. PowerShell 5.1 и другие
// наивные клиенты без charset читают тело как ISO-8859-1, и все русские
// строки - note file_search, limit_hit, errors источников - превращались в
// mojibake, хотя тело реально в UTF-8. mcp-go ставит заголовок сам, поэтому
// параметр дотягивается обёрткой на выходе, а не правкой библиотеки.
//
// Замена только для точного application/json: text/event-stream (SSE-потоки
// streamable HTTP) и text/plain (http.Error) не трогаем.
type charsetWriter struct {
	http.ResponseWriter
	wrote bool
}

func (c *charsetWriter) WriteHeader(code int) {
	if !c.wrote {
		c.wrote = true
		if c.Header().Get("Content-Type") == "application/json" {
			c.Header().Set("Content-Type", "application/json; charset=utf-8")
		}
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *charsetWriter) Write(b []byte) (int, error) {
	if !c.wrote {
		c.WriteHeader(http.StatusOK)
	}
	return c.ResponseWriter.Write(b)
}

// Flush обязателен: без проброса SSE-поток streamable HTTP буферизуется
// обёрткой и клиент не получает события до разрыва соединения.
func (c *charsetWriter) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// charsetMiddleware ставит обёртку на все ответы дерева маршрутов.
func charsetMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&charsetWriter{ResponseWriter: w}, r)
	})
}

func jsonResult(v any) (*mcp.CallToolResult, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(string(b)), nil
}

var errNeedCertPair = errorString("нужны оба файла: --tls-cert и --tls-key")

type errorString string

func (e errorString) Error() string { return string(e) }

// ErrOpenAddressWithoutToken - отказ поднимать HTTP-слушатель на не-петлевом
// адресе без токена. Сравнивается через errors.Is.
var ErrOpenAddressWithoutToken = errorString("открытый адрес без токена")

// CheckListenPolicy проверяет сочетание адреса слушателя и токена до подъёма
// сервера.
//
// Живой замер ДО на бинаре e5eb39b: run --http :18080 без токена поднимал
// слушатель на 0.0.0.0:18080 и [::]:18080, и с адреса 192.168.0.18 без единого
// заголовка Authorization отдавал initialize (200 и сессия), tools/list (200,
// 27 инструментов, среди них backup_create, judge_submit, fetch, hunt_create,
// collect_files, discover_onions) и tools/call stats (200, содержимое базы:
// onion_total 11115). Единственной защитой была строка WRN в лог, которую при
// запуске службой или планировщиком никто не читает.
//
// Пустой адрес означает stdio-транспорт и проверке не подлежит. Loopback
// проходит без токена: это локальный агент и его MCP-клиент на одной машине.
// AllowOpen снимает проверку для осознанного открытого стенда в изолированной
// сети.
func CheckListenPolicy(addr, token string, allowOpen bool) error {
	addr = strings.TrimSpace(addr)
	if addr == "" || allowOpen || strings.TrimSpace(token) != "" {
		return nil
	}
	if netx.AddrIsLoopback(addr) {
		return nil
	}
	return fmt.Errorf("%w: адрес %s слушает не только петлевой интерфейс, а токен не задан; "+
		"задайте --token или --token-file (VOIDSEARCH_HTTP_TOKEN), привяжите адрес к "+
		"127.0.0.1 либо включите --http-allow-open (VOIDSEARCH_HTTP_ALLOW_OPEN=1), "+
		"если сеть изолирована и это осознанное решение", ErrOpenAddressWithoutToken, addr)
}

// authMiddleware закрывает Bearer'ом всё, кроме /health и /metrics
// (у них своя логика выше). Без токена - прозрачный проход.
func authMiddleware(tok string, next http.Handler) http.Handler {
	if strings.TrimSpace(tok) == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !checkBearer(tok, r) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="voidsearchswag"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// checkBearer сверяет токен из запроса с configured.
//
// Два исправления против прежней версии.
//
// Схема регистронезависима: RFC 7235 определяет auth-scheme как токен, а не
// как точную строку. Прежний TrimPrefix("Bearer ") не срабатывал на
// корректном `Authorization: bearer <токен>`, got оставался равным
// "bearer <токен>", сравнение падало, и правильно настроенный клиент получал
// 401 без единой подсказки о причине.
//
// Сравнение должно быть константным по времени. Обычное == прерывается на
// первом несовпавшем байте, что даёт измеримую разницу во времени ответа. Это
// единственная аутентификация на HTTP-сервере, который к тому же отдаёт fetch
// (SSRF) и /peer/export (весь пул и охоты), поэтому тут oracle неуместен даже
// теоретический.
func checkBearer(tok string, r *http.Request) bool {
	tok = strings.TrimSpace(tok)
	if tok == "" {
		return true
	}
	got := strings.TrimSpace(r.Header.Get("Authorization"))
	if scheme, rest, ok := strings.Cut(got, " "); ok && strings.EqualFold(strings.TrimSpace(scheme), "bearer") {
		got = strings.TrimSpace(rest)
	}
	if got == "" {
		got = strings.TrimSpace(r.Header.Get("X-API-Token"))
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(tok)) == 1
}

func Serve(ctx context.Context, s *server.MCPServer, httpAddr string, token ...string) error {
	tok := ""
	if len(token) > 0 {
		tok = strings.TrimSpace(token[0])
	}
	return ServeWithOptions(ctx, s, HTTPOptions{Addr: httpAddr, Token: tok})
}

// HTTPOptions - параметры HTTP-транспорта. Cert+Key включают TLS напрямую
// (без реверс-прокси); Metrics отдаёт JSON-снимок счётчиков на /metrics;
// Store включает /peer/export для синка локального кластера.
type HTTPOptions struct {
	Addr     string
	Token    string
	CertFile string
	KeyFile  string
	Metrics  *metrics.Counters
	Store    *store.Store
	// AllowOpen разрешает слушать не-петлевой адрес без токена. Без него
	// такое сочетание отказывается поднимать сервер: см. CheckListenPolicy.
	AllowOpen bool
	// RateLimitPerMin - лимит запросов в минуту с одного IP. 0 - выкл
	// (совместимость со старым поведением), отрицательное трактуется как 0.
	// Молчаливое включение лимита происходит на уровне config.Load, здесь -
	// только явное значение.
	RateLimitPerMin int
}

func ServeWithOptions(ctx context.Context, s *server.MCPServer, opt HTTPOptions) error {
	tok := strings.TrimSpace(opt.Token)
	mc := opt.Metrics
	if mc == nil {
		mc = metrics.Default
	}
	httpAddr := opt.Addr
	if err := CheckListenPolicy(httpAddr, tok, opt.AllowOpen); err != nil {
		return err
	}
	if httpAddr == "" {
		errCh := make(chan error, 1)
		go func() { errCh <- server.ServeStdio(s) }()
		select {
		case <-ctx.Done():
			return nil
		case err := <-errCh:
			return err
		}
	}
	hs := server.NewStreamableHTTPServer(s)
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	// /metrics за тем же Bearer, что и MCP: счётчики раскрывают нагрузку,
	// наружу без auth их не отдаём. Без токена endpoint открыт: сервер
	// и так открыт всем, скрывать нечего.
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		if tok != "" && !checkBearer(tok, r) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="voidsearchswag"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(mc.Snapshot())
	})
	// /peer/export - выгрузка пула и охот для синка локального кластера.
	// Пул адресов наружу без auth не отдаём никогда: без токена на сервере
	// endpoint закрыт всегда, с токеном - по Bearer.
	mux.HandleFunc("/peer/export", func(w http.ResponseWriter, r *http.Request) {
		if tok == "" {
			http.Error(w, "peer sync требует токен на сервере", http.StatusForbidden)
			return
		}
		if !checkBearer(tok, r) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="voidsearchswag"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		peerExport(w, r, opt.Store)
	})
	// MCP живёт на своём пути, а не на всём дереве.
	//
	// Прежняя регистрация mux.Handle("/", hs) отдавала обработчику любой путь.
	// Живой замер ДО на бинаре 55654ce, сервер на :18080 без токена, слушатель
	// на 0.0.0.0:
	//
	//	POST http://192.168.0.18:18080/foobar           -> 200, ответ initialize
	//	POST http://192.168.0.18:18080/admin/panel/deep -> 200, ответ initialize
	//	GET  http://127.0.0.1:18080/foobar              -> нет ответа 4 с
	//	DELETE http://127.0.0.1:18080/whatever          -> 200, пустое тело
	//
	// Произвольный путь исполнял JSON-RPC, GET на мусорном пути открывал
	// бесконечный SSE-поток без статуса - WriteTimeout на сервере сознательно
	// не выставлен, поэтому соединение висело до таймаута клиента, - а лог и
	// docs/INTEGRATE.md при этом обещали один путь /mcp.
	//
	// Точный корень оставлен за MCP намеренно: клиенты, настроенные на url без
	// пути, обязаны продолжить работать. Всё прочее отдаёт 404, чтобы
	// реверс-прокси мог раздавать соседние пути своему приложению и чтобы
	// сканер не получал висящее соединение на каждый мусорный запрос.
	mux.Handle("/mcp", authMiddleware(tok, hs))
	mux.Handle("/{$}", authMiddleware(tok, hs))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "нет такого маршрута", http.StatusNotFound)
	})
	// Таймауты сервера обязательны. Без ReadHeaderTimeout клиент, который
	// цедит байты заголовка по одному, держит горутину и файловый дескриптор
	// неограниченно долго - классический slowloris, и на порту, доступном из
	// сети, это исчерпание соединений даже при включённом токене.
	//
	// WriteTimeout намеренно не выставлен: streamable HTTP держит длинные
	// SSE-потоки, и любой разумный таймаут записи их оборвёт.
	// ReadTimeout тоже не ставим целиком - тело запроса ограничивается
	// отдельно через MaxBytesReader в обёртке ниже, а общий ReadTimeout
	// убил бы долгие POST.
	//
	// Rate limit стоит внешним слоем до limitBody: флуд с одного IP
	// отталкивается до чтения тела и auth-сверки. Живой замер ДО на бинаре
	// 627107d: 300 GET /health подряд, все 200, никакого счётчика. Для
	// петлевого self-hosted лимит неощутим, для открытого стенда это
	// единственный слой, который не даёт сканеру израсходовать CPU и
	// коннекты раньше, чем сработает авторизация.
	handler := http.Handler(limitBody(charsetMiddleware(mux), maxRequestBody))
	if opt.RateLimitPerMin > 0 {
		handler = rateLimitMiddleware(newRateLimiter(opt.RateLimitPerMin, time.Minute), handler)
	}
	srv := &http.Server{
		Addr:              httpAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	errCh := make(chan error, 1)
	if opt.CertFile != "" || opt.KeyFile != "" {
		if opt.CertFile == "" || opt.KeyFile == "" {
			return errNeedCertPair
		}
		go func() { errCh <- srv.ListenAndServeTLS(opt.CertFile, opt.KeyFile) }()
	} else {
		go func() { errCh <- srv.ListenAndServe() }()
	}
	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		_ = hs.Shutdown(shutCtx)
		return nil
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}
