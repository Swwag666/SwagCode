package mcpserver

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// Этап 178, смоук-раунд 3 (G/H/I на vss178c): часть дефектов закрыта правкой
// кода (snake_case, failed/last_error, заголовки fetch, UTC-поля), а часть -
// контрактами описаний: слепой агент читает только tools/list, и если деградация
// stealth, кулдаун deep или «пустой metrics - нет событий» не названы текстом,
// агент трактовал их как потерянные данные. Эти ловцы держат каждую фразу
// этапа на месте: удалить её из описания - упасть здесь.

func toolDescription(t *testing.T, name string) string {
	t.Helper()
	c := newClient(t, Deps{Version: "test", Started: time.Now()})
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	for _, tool := range res.Tools {
		if tool.Name == name {
			return tool.Description
		}
	}
	t.Fatalf("инструмент %s не найден", name)
	return ""
}

func requirePhrases(t *testing.T, name string, wants []string) {
	t.Helper()
	desc := toolDescription(t, name)
	for _, want := range wants {
		if !strings.Contains(desc, want) {
			t.Errorf("описание %s не содержит %q", name, want)
		}
	}
}

// Смоук-G флаг 1: stealth без cloaked-браузера молча выполнялся движками fast.
// Ответ не несёт ни note, ни поля о браузере - единственное честное место для
// контракта - описание: деградацию видит состав report.engines.
func TestSearchDescriptionNamesStealthDegradation(t *testing.T) {
	requirePhrases(t, "search", []string{
		"stealth при недоступном cloaked-браузере",
		"браузерного движка в report.engines нет",
		"след деградации",
	})
}

// Смоук-G флаг 16 и вопрос 6: состав deep меняется между вызовами, а предела
// ожидания в схеме нет. Обе вещи названы текстом, чтобы агент не считал
// отсутствующий движок потерей, а долгий вызов - зависанием.
func TestSearchDescriptionNamesEngineWindowAndBudget(t *testing.T) {
	requirePhrases(t, "search", []string{
		"движок с недавними провалами отдыхает (кулдаун)",
		"отдельного потолка на вызов",
		"сетевым бюджетом транспорта сервера",
	})
}

// Смоук-G флаг 17 и вопрос 4: count движка не равен итоговой выдаче, страница
// поисковика проходит токен-фильтр. Без контракта агент читал разницу как
// потерю данных.
func TestSearchDescriptionNamesCountersAndSearcherPages(t *testing.T) {
	requirePhrases(t, "search", []string{
		"до дедупа, токен-фильтра и среза limit",
		"токен-фильтром не отсекается",
	})
}

// Смоук-G вопрос 1 и 2: пустой metrics {} при живом сервере и счётчики, в
// которых фоновые прогоны смешаны с вызовами оператора.
func TestMetricsDescriptionNamesEmptyMapAndBackgroundRuns(t *testing.T) {
	requirePhrases(t, "metrics", []string{
		"Пустой ответ {}",
		"рождается первым событием",
		"включают фоновые прогоны",
		"отделён счётчиками bg_*",
	})
}

// Смоук-G флаг 14: поля событий judge_submit появляются только при событиях -
// иначе отсутствие skipped_* читалось как потерянная диагностика.
func TestJudgeSubmitDescriptionNamesEventFields(t *testing.T) {
	requirePhrases(t, "judge_submit", []string{
		"присутствуют в ответе только при событии",
		"updated_rows",
		"«нет ключа» значит «события не было»",
	})
}

// Смоук-H D4 и смоук-I Д2: подмены аргументов ниже диапазона. Описание обязано
// назвать сам факт приведения к границе - иначе «timeout=3 превратился в 5»
// читалось как игнор аргумента.
func TestDiscoverDescriptionNamesClamping(t *testing.T) {
	requirePhrases(t, "discover_onions", []string{
		"вне 5..600 обрезается к границе",
		"max_hosts и max_addresses приводятся к диапазону",
		"depth ниже нуля читается нулём",
	})
}

// Смоук-I Д10: при обрезке не стартовавшие источники не приходят вовсе - без
// контракта агент считал пустой список источников потерей каналов.
func TestDiscoverDescriptionNamesUnstartedSources(t *testing.T) {
	requirePhrases(t, "discover_onions", []string{
		"только начатые каналы",
		"не стартовавший из-за обрезки канал не приходит вовсе",
		"разные события",
	})
}

// Смоук-H D6: new_hosts содержит и операторские сиды.
func TestDiscoverDescriptionNamesNewHostsSemantics(t *testing.T) {
	requirePhrases(t, "discover_onions", []string{
		"включает и операторские сиды",
		"«новый для этого обхода»",
	})
}

// Смоук-H D5: файлы повторного обхода остаются под первой меткой file_task_id.
func TestDiscoverDescriptionNamesFileTaskOrigin(t *testing.T) {
	requirePhrases(t, "discover_onions", []string{
		"у файла одно происхождение",
		"повторный обход оставляет записи под первой меткой",
	})
}

// Смоук-I вопрос 1: elapsed - пролет внутреннего времени, а не длительность
// вызова: агент видел 10.47s при фактических 20s и не мог объяснить разницу.
func TestDiscoverDescriptionNamesElapsedScope(t *testing.T) {
	requirePhrases(t, "discover_onions", []string{
		"время прогона внутри сервера",
		"хвост ответа в него не входит",
	})
}

// Смоук-I Д7: очередь охвата живая - фоновые пробы двигают границы групп,
// и алфавитный порядок между вызовами меняется законно.
func TestProbePoolDescriptionNamesLiveQueue(t *testing.T) {
	requirePhrases(t, "probe_pool", []string{
		"по состоянию пула на момент вызова",
		"состав волны меняется от вызова к вызову",
		"очередь про охват, а не сортировку выдачи",
	})
}

// Смоук-I вопрос 13: часовые пояса. Смешение UTC-полей с локальными task_id
// без контракта агент считал рассинхронизацией времени сервера.
func TestStatusDescriptionNamesTimezones(t *testing.T) {
	requirePhrases(t, "status", []string{
		"Времена во всех ответах - UTC",
		"локальное время сервера появляется только в человекочитаемых артефактах",
		"task_id, имена снимков бэкапов и строки лога",
	})
}

// Смоук-178D живой пруф: watch на новой охоте вернул last_count=10 при
// hits=[] и выстоял весь таймаут. Первый прогон задаёт базовую линию
// сравнения - не смена выдачи; без фразы слепой агент назовёт это
// потерей уже найденных ссылок.
func TestWatchDescriptionNamesFirstRunBaseline(t *testing.T) {
	requirePhrases(t, "hunt_watch", []string{
		"первый прогон новой охоты задаёт базовую линию",
		"находкой не считается",
		"hits придут со второго прогона",
	})
}

// Смоук-G флаг 9: LastRun «0001-01-01T00:00:00Z» у созданной охоты
// выглядел зависшим счётчиком. Нулевое время - момент создания до первого
// прогона; фраза закрывает трактовку «сбой часов».
func TestHuntListDescriptionNamesZeroLastRun(t *testing.T) {
	requirePhrases(t, "hunt_list", []string{
		"last_run нулевой (0001-01-01T00:00:00Z)",
		"ни одного прогона не было",
		"каждый прогон, включая отказавший, двигает время дальше",
	})
}

// Смоук-раунд 4 (L Д1/вопрос 2): неположительные timeout_s/interval_s
// hunt_watch молча подменяются дефолтами - блокирующий вызов держался
// двухминутный холд без единого предупреждения. Фразы общие - в описании
// инструмента, фразы параметров - в тексте схемы.
func TestWatchDescriptionNamesNonPositiveDefaults(t *testing.T) {
	requirePhrases(t, "hunt_watch", []string{
		"Неположительные timeout_s/interval_s молча подменяются дефолтами (120/30)",
	})
	requireSchemaPhrases(t, "hunt_watch", []string{
		"неположительное значение подменяется дефолтом 120",
		"неположительное значение подменяется дефолтом 30",
	})
}

// Смоук-раунд 4 (K вопрос 1): «6 из 6» при onion_engines=4 - счётчик
// движков режимной цепочки прогона, включая clearnet-мосты, а не каталог
// onion-движков из status.
func TestHuntRunDescriptionNamesEngineChainCounter(t *testing.T) {
	requirePhrases(t, "hunt_run", []string{
		"движки режимной цепочки прогона",
		"цепочка глубже каталога onion-движков",
	})
}

// Смоук-раунд 4 (J вопрос 1, J Д1): фолбэк unknown-адресов при пустом live и
// финализация задачи, не зависящая от соединения клиента.
func TestCollectFilesDescriptionNamesFallbackAndFinalization(t *testing.T) {
	requirePhrases(t, "collect_files", []string{
		"при их отсутствии - unknown-адреса",
		"Финализация задачи не зависит от соединения клиента",
		"не оставляет задачу висящей в running",
	})
}

// Смоук-раунд 4 (L флаг 5): движки отдают сырые строки, сервер
// дедуплицирует - счётчика дедупа в ответе нет, разница count - норма.
func TestSearchDescriptionNamesNoDedupCounter(t *testing.T) {
	requirePhrases(t, "search", []string{
		"отдельного счётчика дедупликации ответ не несёт",
		"дубли между движками",
	})
}

// Смоук-раунд 4 (L вопрос 3): success_rate - EMA, не доля: одинаковые
// значения у соседей читались как копия глобального числа.
func TestPoolStatusDescriptionNamesEMAFormula(t *testing.T) {
	requirePhrases(t, "pool_status", []string{
		"после первого успеха 0.1, после второго 0.19",
	})
}

// Смоук-раунд 4 (L вопрос 4): фоновый тик разведки трёхфазный - минуты
// между стартом тика и ростом пула это порядок фаз, а не потерянный тик.
func TestDiscoverDescriptionNamesThreePhases(t *testing.T) {
	requirePhrases(t, "discover_onions", []string{
		"трёхфазный",
		"порядок фаз, а не потерянный тик",
	})
}

// Смоук-раунд 4 (K Д2): fields обязателен и в схеме, и в тексте параметра.
func TestParseDescriptionNamesRequiredFields(t *testing.T) {
	requireSchemaPhrases(t, "parse", []string{
		"поля для извлечения (массив имён полей, обязателен)",
	})
}

// Смоук-раунд 4b (флаг 2): failed/last_error у hunt_watch omitempty -
// описание упоминает поля, но не говорило, что их отсутствие значит
// «отказов не было». Агент увидел молчание полей при нулевых отказах.
func TestWatchDescriptionNamesOmitEmptyFailed(t *testing.T) {
	requirePhrases(t, "hunt_watch", []string{
		"оба поля omitempty - нет ключа значит, что отказов не было",
	})
}
