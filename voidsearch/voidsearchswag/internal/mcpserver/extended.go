package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"voidsearchswag/internal/classifier"
	"voidsearchswag/internal/hunt"
	"voidsearchswag/internal/metrics"
	"voidsearchswag/internal/netx"
	"voidsearchswag/internal/parser"
	"voidsearchswag/internal/promote"
	"voidsearchswag/internal/store"
)

// maxVotes - потолок на число голосов в одном вызове judge_submit. Пачка
// пишется одной транзакцией, но даже транзакция на сто тысяч строк держала бы
// единственное соединение базы достаточно долго, чтобы заблокировать остальные
// инструменты.
const maxVotes = 5000

// registerExtended вешает инструменты этапов 3-6: самосборный парсер,
// классификатор, охоту и статистику. Вызывается из New; при nil-зависимостях
// хендлеры отдают ошибку, а не паникуют.
func (d Deps) registerExtended(s *server.MCPServer) {
	s.AddTool(
		mcp.NewTool("build_parser",
			mcp.WithDescription("Собрать парсер под сайт: забирает страницу и отдаёт DOM-скелет. LLM-клиент возвращает mapping field->selector через save_selector."),
			mcp.WithString("url", mcp.Required(), mcp.Description("URL страницы")),
			mcp.WithNumber("max_chars", mcp.Description("ограничение длины скелета, по умолчанию 20000")),
		),
		d.buildParserHandler,
	)
	s.AddTool(
		mcp.NewTool("save_selector",
			mcp.WithDescription("Сохранить селектор поля в реестр парсеров (mapping от LLM-клиента после build_parser)"),
			mcp.WithString("url_pattern", mcp.Required(), mcp.Description("хост из build_parser (pattern)")),
			mcp.WithString("field", mcp.Required(), mcp.Description("имя поля")),
			mcp.WithString("selector", mcp.Required(), mcp.Description("CSS-селектор")),
			mcp.WithString("strategy", mcp.Description("css (по умолчанию)")),
		),
		d.saveSelectorHandler,
	)
	s.AddTool(
		mcp.NewTool("parse",
			mcp.WithDescription("Извлечь поля со страницы: сначала сохранённые селекторы хоста, при их отсутствии или поломке - self-healing (fuzzy/regex/structural/generic). healed=true значит селектор надо перегенерировать."),
			// Этап 178 (смоук-K): fields обязателен и в схеме. Рантайм
			// требует массив имён («параметр fields обязателен»), а схема
			// требовала только url: MCP-клиент строил запрос по схеме,
			// получал отказ рантайма и не мог узнать причину до вызова.
			mcp.WithString("url", mcp.Required(), mcp.Description("URL страницы")),
			mcp.WithArray("fields", mcp.Required(), mcp.Description("поля для извлечения (массив имён полей, обязателен)"), mcp.WithStringItems()),
		),
		d.parseHandler,
	)
	s.AddTool(
		mcp.NewTool("classify_source",
			mcp.WithDescription("Классифицировать источник: public|private|paid|scam + качество и популярность по правилам (форма входа, paywall, капча, скам-связки)"),
			mcp.WithString("url", mcp.Required(), mcp.Description("URL источника")),
		),
		d.classifyHandler,
	)
	s.AddTool(
		mcp.NewTool("hunt_create",
			mcp.WithDescription("Завести фоновый мониторинг запроса: периодический прогон, diff по hash выдачи. Тот же запрос в том же режиме не считается дублем и не переиспользует существующую охоту: заводится вторая строка со своим id и своим расписанием, фоновый тик прогоняет обе"),
			mcp.WithString("query", mcp.Required(), mcp.Description("запрос мониторинга")),
			mcp.WithString("mode", mcp.Description("auto|fast|stealth|deep (по умолчанию auto)")),
			mcp.WithNumber("schedule_min", mcp.Description("период в минутах, по умолчанию 360")),
		),
		d.huntCreateHandler,
	)
	s.AddTool(
		mcp.NewTool("hunt_list",
			mcp.WithDescription("Список фоновых мониторингов: запрос, режим, последний прогон. Поля строк: id, query, mode, schedule_min, last_run, last_hash, created_at; пустой список отдаёт hunts: [] (не null). last_run нулевой (0001-01-01T00:00:00Z) - охота создана, но ни одного прогона не было: точка отсчёта, а не сбой часов; каждый прогон, включая отказавший, двигает время дальше."),
		),
		d.huntListHandler,
	)
	s.AddTool(
		mcp.NewTool("hunt_run",
			mcp.WithDescription("Прогнать охоту: без id - все с вышедшим расписанием, с id (принимается и имя hunt_id из ответа hunt_create) - одну вне расписания. Охота без единого прогона считается вышедшей: её первый прогон фиксирует базу и находкой не считается. Отказ поиска - не сбой вызова: без id он виден счётчиком failed и last_error, с id - теми же полями failed/last_error в ответе. Счётчик движков в last_error («N из N») считает движки режимной цепочки прогона - включая clearnet-мосты deep-режима, - а не onion_engines из status: «6 из 6» при onion_engines=4 значит, что цепочка глубже каталога onion-движков. Находки дублируются push-уведомлением notifications/hunt_update только в stdio-транспорте; HTTP-сессия остаётся plain JSON, находки целиком в ответе."),
			mcp.WithNumber("id", mcp.Description("id охоты (алиас hunt_id), пусто - все due")),
		),
		d.huntRunHandler,
	)
	s.AddTool(
		mcp.NewTool("hunt_watch",
			mcp.WithDescription("Ждать находок блокирующим вызовом вместо поллинга hunt_run: прогоняет указанные охоты до первой смены hash или до таймаута. Расписание игнорируется. Отказ поиска не прерывает ожидание: счётчик failed и last_error объясняют, почему опросы пусты; оба поля omitempty - нет ключа значит, что отказов не было. Смена hash - изменение выдачи относительно прошлого прогона: первый прогон новой охоты задаёт базовую линию и находкой не считается - last_count покажет размер выдачи, а hits придут со второго прогона, когда состав ссылок изменится. Неположительные timeout_s/interval_s молча подменяются дефолтами (120/30) - та же политика, что у остальных числовых параметров сервера; note смешанного прогона называет успешные прогоны и отказы поиска отдельно (checked/failed), «успешных прогонов 0» значит, что поиск не выполнился ни разу."),
			mcp.WithNumber("id", mcp.Description("id охоты (алиас hunt_id), пусто - все")),
			mcp.WithNumber("timeout_s", mcp.Description("ждать не дольше, секунд, по умолчанию 120, потолок 600; неположительное значение подменяется дефолтом 120")),
			mcp.WithNumber("interval_s", mcp.Description("пауза между прогонами, секунд, по умолчанию 30; неположительное значение подменяется дефолтом 30")),
		),
		d.huntWatchHandler,
	)
	s.AddTool(
		mcp.NewTool("hunt_hits",
			mcp.WithDescription("История находок охоты: url, которые охота увидела при смене выдачи, с запросом и режимом на момент находки, датой и счётчиком повторов. Поля строк: id, hunt_id, query, mode, url, first_found, last_found, times_seen. Без id - все охоты. Пустая история отдаёт hits: [] (не null)."),
			mcp.WithNumber("id", mcp.Description("id охоты (алиас hunt_id), пусто - все охоты")),
			mcp.WithNumber("limit", mcp.Description("сколько строк вернуть, по умолчанию 100, потолок 1000")),
		),
		d.huntHitsHandler,
	)
	s.AddTool(
		mcp.NewTool("stats",
			mcp.WithDescription("Сводная статистика: база (пул, файлы, задачи, охоты, селекторы), движки (onion_total здесь - движки поиска, не адреса пула: адреса - db.onion_total), транспорт (kind, spec, healthy)"),
		),
		d.statsHandler,
	)
	s.AddTool(
		mcp.NewTool("promote_engines",
			mcp.WithDescription("Автопромоут: проверить живые сервисы пула на поисковую форму и поднять прошедшие в onion-каталог. Проверки идут через tor, каждая - секунды. Бюджет всего вызова около трёх минут: по его истечении проверка останавливается, поле stopped в ответе объясняет досрочную остановку, счётчики checked/promoted показывают успевшее. Судьба каждого проверенного адреса названа: promoted - подняты, failed - проверка не состоялась (в last_error причина), save_failed - сид не записался в базу, already_engines - форма нашлась, но движок по базе уже существует (новой строкой не становится). Поднятые движки сразу видны в onion_engines статуса."),
			mcp.WithNumber("limit", mcp.Description("сколько живых сервисов проверить, по умолчанию 10, потолок 50")),
		),
		d.promoteHandler,
	)
	s.AddTool(
		mcp.NewTool("metrics",
			mcp.WithDescription("Счётчики сервера: поиски по режимам (search_total), движки ok/fail, находки охот, фоновые прогоны. Пустой ответ {} - событий ещё не было с перезапуска: счётчик рождается первым событием, а не нулём, поэтому «нет ключа» и «нулевое событие» не различаются. engine_fail считает только движки, которым достался слот окна поиска: движок, не попавший в окно режима (композиция/кулдаун), не провалился - и в engine_fail не входит. search_total, search_results_total и engine_ok_*/engine_fail_* включают фоновые прогоны (охоту, разведку) наравне с вызовами оператора: их вклад отделён счётчиками bg_*. bg_hunt_runs/bg_hunt_hits/bg_discover_runs/bg_discover_new/bg_probe_runs/bg_promote_runs/bg_promote_engines/bg_backup_runs/bg_peer_runs - тики фоновых циклов с их эпохи старта: счётчик в памяти, после перезапуска начинается с нуля. hunt_hits_total - общее число находок охот из базы: переживает перезапуск. Каждое поднятие движка фоновым промоутом пишется в лог строкой «фон: промоут: поднят движок ...»: рост числа движков между вызовами onion_engines объясняется там."),
		),
		d.metricsHandler,
	)
	s.AddTool(
		mcp.NewTool("backup_create",
			mcp.WithDescription("Снимок базы SQLite онлайн (VACUUM INTO) в каталог бэкапов с ротацией"),
			mcp.WithNumber("keep", mcp.Description("сколько снимков держать, по умолчанию 7")),
			mcp.WithBoolean("confirm_prune", mcp.Description("обязателен, если ротация удалит больше одного снимка")),
		),
		d.backupHandler,
	)
	s.AddTool(
		mcp.NewTool("judge_submit",
			mcp.WithDescription("Оценки LLM-судьи по выдаче search: [{url, score 0..1}]. Учат бонусы хостов в реранке (ночной пересчёт). Голос привязан к запросу: бонус, вес автора и порог доверия считаются в пределах того же запроса, чужие оценки порядок выдачи не меняют. Хосты с высокими оценками всплывают. Ответ сводит пачку: votes_total, accepted (обработано), new_rows (сколько легло новыми строками), updated_rows (сколько голосов легло поверх существующих строк), clamped (оценка вне 0..1 приведена к границе), skipped_no_url, skipped_bad_score, skipped_not_object и stored_rows со stored_authors - сколько голосов и сколько разных судей хранится по этому запросу. Счётчики событий (clamped, skipped_*, clamped_reason, skipped_reason) присутствуют в ответе только при событии: при чистой пачке их нет, а не нули - «нет ключа» значит «события не было»."),
			mcp.WithString("query", mcp.Required(), mcp.Description("запрос из search")),
			mcp.WithArray("votes", mcp.Description("оценки: [{url, score 0..1}]")),
			mcp.WithString("author", mcp.Description("кто судит (дефолт пусто); вес голоса 1/sqrt(голосов автора)")),
		),
		d.judgeHandler,
	)
	s.AddTool(
		mcp.NewTool("peer_list",
			mcp.WithDescription("Здоровье пиров локального кластера: latency /health каждого"),
		),
		d.peerListHandler,
	)
	s.AddTool(
		mcp.NewTool("peer_sync",
			mcp.WithDescription("Синк с пирами: забрать их пул и охоты, смержить в свою базу (дедуп по адресу и запросу). Сервер без пиров (VOIDSEARCH_PEERS не задан) - не ошибка: ответ приходит пустым списком peers с нулевыми счётчиками и note, синк просто нечего обходить."),
			mcp.WithNumber("limit", mcp.Description("адресов с пира, по умолчанию 500, потолок 2000")),
		),
		d.peerSyncHandler,
	)
}

func (d Deps) parseHandler(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if d.Parser == nil {
		return mcp.NewToolResultError("парсер не инициализирован"), nil
	}
	rawURL, err := req.RequireString("url")
	if err != nil {
		return mcp.NewToolResultError("параметр url обязателен"), nil
	}
	fields := req.GetStringSlice("fields", nil)
	if len(fields) == 0 {
		return mcp.NewToolResultError("параметр fields обязателен (массив имён полей)"), nil
	}
	values, healed, warn, err := d.Parser.ParseDetailed(ctx, rawURL, fields)
	if err != nil {
		if len(values) > 0 {
			out := map[string]any{"fields": values, "healed": healed, "error": err.Error()}
			if warn != "" {
				out["warning"] = warn
			}
			return jsonResult(out)
		}
		return mcp.NewToolResultError(err.Error()), nil
	}
	out := map[string]any{"fields": values, "healed": healed}
	// Предупреждение идёт до note о селекторах и отдельно от него: healed=true
	// при недоступной базе советует перегенерировать селекторы, которые на самом
	// деле в порядке, поэтому причина обязана быть названа рядом с советом.
	if warn != "" {
		out["warning"] = warn
	}
	if healed {
		out["note"] = "часть полей извлечена запасной стратегией (confidence < 1): перегенерируй селекторы через build_parser + save_selector"
	}
	return jsonResult(out)
}

func (d Deps) buildParserHandler(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if d.Parser == nil {
		return mcp.NewToolResultError("парсер не инициализирован"), nil
	}
	rawURL, err := req.RequireString("url")
	if err != nil {
		return mcp.NewToolResultError("параметр url обязателен"), nil
	}
	// max_chars ограничивается один раз и потом переиспользуется.
	//
	// Прежняя версия читала значение из запроса дважды: один раз для Skeleton
	// и второй - для вычисления truncated. Skeleton нормализует max_chars <= 0
	// внутри себя, а сравнение len(skeleton) >= req.GetInt(...) брало исходное
	// неограниченное значение. При max_chars: -1 выражение len >= -1 истинно
	// всегда, поэтому ответ утверждал truncated: true, хотя обрезано не было
	// ничего. На огромном max_chars выходило обратное: скелет обрезан по
	// внутреннему дефолту, а флаг говорит false.
	//
	// Клиент решает по этому флагу, запрашивать ли остаток, то есть ошибка
	// здесь заставляет его принимать неверное решение в обе стороны.
	maxChars := clampInt(req.GetInt("max_chars", 20000), 1, maxToolChars)
	pattern, skeleton, err := d.Parser.Skeleton(ctx, rawURL, maxChars)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return jsonResult(map[string]any{
		"pattern":   pattern,
		"skel":      skeleton,
		"hint":      "верни mapping field->selector вызовом save_selector с url_pattern=" + pattern,
		"truncated": len(skeleton) >= maxChars,
	})
}

func (d Deps) saveSelectorHandler(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if d.Parser == nil {
		return mcp.NewToolResultError("парсер не инициализирован"), nil
	}
	pattern, err := req.RequireString("url_pattern")
	if err != nil {
		return mcp.NewToolResultError("параметр url_pattern обязателен"), nil
	}
	field, err := req.RequireString("field")
	if err != nil {
		return mcp.NewToolResultError("параметр field обязателен"), nil
	}
	selector, err := req.RequireString("selector")
	if err != nil {
		return mcp.NewToolResultError("параметр selector обязателен"), nil
	}
	if err := d.Parser.Save(ctx, pattern, field, selector, req.GetString("strategy", "css")); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return jsonResult(map[string]any{"saved": true, "pattern": pattern, "field": field})
}

func (d Deps) classifyHandler(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if d.Parser == nil {
		return mcp.NewToolResultError("парсер не инициализирован"), nil
	}
	rawURL, err := req.RequireString("url")
	if err != nil {
		return mcp.NewToolResultError("параметр url обязателен"), nil
	}
	resp, err := d.Parser.Fetch.FetchURL(ctx, rawURL)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	body := string(resp.Body)
	// Сырой body уходит в htmlHint: скелет-сниппет вырезает атрибуты
	// (type="password") и вне-теговый текст, а маркеры формы входа и
	// paywall живут именно там. Без него classify слеп.
	v := classifier.Classify(rawURL, parser.TitleOf(body), snippetOf(body), body)
	return jsonResult(v)
}

func snippetOf(body string) string {
	t := parser.SkeletonOf(body, 2000)
	lines := strings.Split(t, "\n")
	keep := lines
	if len(lines) > 30 {
		keep = lines[:30]
	}
	return strings.Join(keep, "\n")
}

func (d Deps) huntCreateHandler(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if d.Hunter == nil {
		return mcp.NewToolResultError("охота не инициализирована"), nil
	}
	query, err := req.RequireString("query")
	if err != nil {
		return mcp.NewToolResultError("параметр query обязателен"), nil
	}
	id, err := d.Hunter.Create(ctx, query, req.GetString("mode", "auto"), req.GetInt("schedule_min", 360))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return jsonResult(map[string]any{"hunt_id": id, "query": query})
}

func (d Deps) huntListHandler(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if d.Hunter == nil {
		return mcp.NewToolResultError("охота не инициализирована"), nil
	}
	hunts, err := d.Hunter.List(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	// Этап 178 (смоук-G): пустой список - это [], а не null: строгий клиент
	// обязан получать один и тот же тип поля при любой пустоте.
	if hunts == nil {
		hunts = []store.Hunt{}
	}
	return jsonResult(map[string]any{"hunts": hunts, "count": len(hunts)})
}

func (d Deps) huntRunHandler(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if d.Hunter == nil {
		return mcp.NewToolResultError("охота не инициализирована"), nil
	}
	// Этап 178 (смоук-I): параметр зовётся id, но hunt_create возвращает
	// поле hunt_id - слепой оператор переносил имя поля из ответа создателя в
	// аргумент и молча получал режим «все охоты». Алиас читается тем же
	// значением, приоритет у канонического id.
	id := req.GetInt("id", 0)
	if id == 0 {
		id = req.GetInt("hunt_id", 0)
	}
	if id > 0 {
		hit, err := d.Hunter.RunOne(ctx, int64(id))
		if err != nil {
			// Этап 178 (смоук-G): отказ поиска - результат прогона, а не сбой
			// инструмента. Без id та же причина уже приходила структурным JSON
			// с last_error и без isError; с id она же уходила текстом с
			// isError - инструмент сам с собой не был согласован. Формат
			// поиска выровнен по режиму «все охоты»: структурный ответ, где
			// last_error называет причину. Неизвестный id и ошибки базы
			// остаются isError - это настоящие сбои вызова.
			var se *hunt.SearchError
			if errors.As(err, &se) {
				return jsonResult(map[string]any{"hunt_id": id, "failed": 1, "last_error": se.Reason})
			}
			return mcp.NewToolResultError(err.Error()), nil
		}
		d.notifyHunt(ctx, []hunt.Hit{hit})
		return jsonResult(hit)
	}
	rep, err := d.Hunter.RunDue(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	d.notifyHunt(ctx, rep.Hits)
	return jsonResult(rep)
}

// huntHitsHandler отдаёт историю находок охоты. Читает через Runner, а не через
// стор напрямую: Runner.Hits приводит limit так же, как CLI, и два клиента не
// расходятся ни в потолке, ни в порядке строк.
func (d Deps) huntHitsHandler(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if d.Hunter == nil {
		return mcp.NewToolResultError("охота не инициализирована"), nil
	}
	// Этап 178 (смоук-I): алиас hunt_id - оператор переносит имя поля из
	// ответа hunt_create; до правки такой вызов молча означал «все охоты».
	id := req.GetInt("id", 0)
	if id == 0 {
		id = req.GetInt("hunt_id", 0)
	}
	hits, err := d.Hunter.Hits(ctx, int64(id), req.GetInt("limit", 100))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	// Этап 178 (смоук-G/I): count=0 отдаёт hits=[], не null - единый тип
	// пустоты с hunt_run и hunt_watch.
	if hits == nil {
		hits = []store.HuntHit{}
	}
	return jsonResult(map[string]any{"count": len(hits), "hits": hits})
}

// notifyHunt дублирует находки push-уведомлением клиенту. Best-effort:
// без живой сессии (stdio без initialize, http без SSE) отправка вернёт
// ошибку - находки при этом уже отданы в ответе tool, ничего не теряется.
//
// Этап 178 (смоук-I, критичный): в streamable HTTP уведомление, отправленное
// из контекста запроса, переключает сессию в SSE-режим НАВСЕГДА: mcp-go
// помечает сессию upgradeToSSE при первой же отправке, и все последующие
// ответы этой сессии - включая tools/list - идут в обёртке text/event-stream.
// Стенд заявляет plain JSON-RPC поверх POST, и клиент на нём ломался об
// event-строки после первого hunt_watch. Спека разрешает оба формата ответа,
// поэтому фикс проектный: в HTTP-транспорте push не отправляется вовсе -
// нахоки уже целиком присутствуют в ответе инструмента, а сессия остаётся
// plain JSON. В stdio уведомление остаётся: там обёртки нет и канал безопасен.
func (d Deps) notifyHunt(ctx context.Context, hits []hunt.Hit) {
	if d.Srv == nil || len(hits) == 0 {
		return
	}
	if sess := server.ClientSessionFromContext(ctx); sess != nil {
		// Сессия streamable HTTP узнаётся по интерфейсу конфигурации
		// транспорта: его реализует только http-сессия mcp-go.
		if _, ok := sess.(server.SessionWithStreamableHTTPConfig); ok {
			return
		}
	}
	fresh := make([]hunt.Hit, 0, len(hits))
	for _, h := range hits {
		if h.Changed {
			fresh = append(fresh, h)
		}
	}
	if len(fresh) == 0 {
		return
	}
	_ = d.Srv.SendNotificationToClient(ctx, "notifications/hunt_update", map[string]any{
		"hits":  fresh,
		"count": len(fresh),
	})
}

func (d Deps) huntWatchHandler(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if d.Hunter == nil {
		return mcp.NewToolResultError("охота не инициализирована"), nil
	}
	timeout := req.GetInt("timeout_s", 120)
	if timeout <= 0 {
		timeout = 120
	}
	if timeout > 600 {
		timeout = 600
	}
	interval := req.GetInt("interval_s", 30)
	if interval <= 0 {
		interval = 30
	}
	if interval > timeout {
		interval = timeout
	}
	// Этап 178 (смоук-I): алиас hunt_id для симметрии с hunt_run/hunt_hits.
	id := req.GetInt("id", 0)
	if id == 0 {
		id = req.GetInt("hunt_id", 0)
	}
	rep, err := d.Hunter.WatchDetailed(ctx, int64(id),
		time.Duration(timeout)*time.Second, time.Duration(interval)*time.Second)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	// Диагностика возвращается всегда, а не только при таймауте: число опросов
	// и размер последней выдачи - это то, по чему вызывающий отличает
	// «выдача стабильна» от «поиск больше ничего не находит».
	diag := map[string]any{
		"waited":     true,
		"polls":      rep.Polls,
		"last_count": rep.LastCount,
		"elapsed":    rep.Elapsed,
		"note":       rep.Note(),
	}
	if rep.Checked > 0 {
		diag["checked"] = rep.Checked
	}
	// Отказы поиска отдаются рядом со счётчиком проверок: без них клиент видел
	// бы checked=0 и note, но не мог отличить «охот не нашлось» от «все поиски
	// легли». В режиме конкретной охоты (id > 0) ошибка возвращается как ошибка
	// инструмента, поэтому счётчик здесь всегда относится к режиму «все охоты».
	if rep.Failed > 0 {
		diag["failed"] = rep.Failed
		if rep.LastError != "" {
			diag["last_error"] = rep.LastError
		}
	}
	if rep.Timeout {
		diag["hits"] = []hunt.Hit{}
		diag["timeout"] = true
		return jsonResult(diag)
	}
	d.notifyHunt(ctx, rep.Hits)
	if id > 0 && len(rep.Hits) > 0 {
		diag["hit"] = rep.Hits[0]
		return jsonResult(diag)
	}
	// Этап 178 (смоук-G/I): hits всегда массив - пустое ожидание без хитов
	// отдаёт [], а не null.
	if rep.Hits == nil {
		diag["hits"] = []hunt.Hit{}
	} else {
		diag["hits"] = rep.Hits
	}
	return jsonResult(diag)
}

func (d Deps) statsHandler(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	out := map[string]any{"version": d.Version}
	// Этап 178 (смоук-G): блок transport. Описание инструмента обещало
	// «транспорт» со времён рефакторинга сводки, но блок жил только в status:
	// stats отвечал без него, и обещание расходилось с ответом.
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
		// Сводка собирается общим сборщиком: он кладёт в блок db список
		// непрочитанных источников, поэтому клиент отличает «охот нет» от
		// «охоты не удалось прочитать». Прежняя версия на каждом из пяти
		// источников глотала ошибку и просто не клала ключ в map.
		db, _ := dbSummary(ctx, d.Store)
		out["db"] = db
	}
	if d.Search != nil {
		// Пул движков здесь описан именами, а в status - числом: stats -
		// расширенная сводка для человека, status - быстрая диагностика.
		// onion_total оставлен только в этом инструменте: он здесь единственный
		// претендент на имя, столкновения с db.onion_total в одном ответе нет.
		out["onion_engines"] = d.onionEngineNames()
		if live, total := d.onionCounts(); total > 0 {
			out["onion_live"] = live
			out["onion_total"] = total
		}
	}
	return jsonResult(out)
}

func (d Deps) promoteHandler(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if d.Promoter == nil || d.Store == nil {
		return mcp.NewToolResultError("промоут не инициализирован (нужны клиент и база)"), nil
	}
	limit := req.GetInt("limit", 10)
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}
	known := map[string]bool{}
	if d.Search != nil && d.Search.Onion != nil {
		// Snapshot: обработчик инструмента работает в своей горутине, а
		// фоновый тик discover одновременно поднимает движки в каталог.
		for _, e := range d.Search.Onion.Snapshot() {
			known[strings.ToLower(e.Base)] = true
		}
	}
	onions, err := d.Store.ListOnions(ctx, "live", limit*3)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	rep := map[string]any{"checked": 0, "promoted": []any{}}
	// Этап 178 (смоук-G, вопрос Q5): судьба каждого проверенного адреса
	// названа. Прежде адрес, у которого форма нашлась, но движок по базе уже
	// существует, выпадал из отчёта: checked его считал, promoted - нет,
	// failed - нет, и арифметика ответа не сходилась (checked=3,
	// failed=2, один «пропал»).
	knownSkipped := 0
	pr := promote.NewProgress(d.Promoter.Budget)
	for _, o := range onions {
		// Этап 178 (смоук-L): limit останавливает проверки, а не поднятия.
		// Прежний стоп по len(promoted) >= limit обещал в описании «сколько
		// живых сервисов проверить», а фактически проверял выборку limit*3
		// целиком, пока не наберётся limit движков: вызов без аргументов
		// проверял 30 сервисов при заявленных 10 и шёл две минуты.
		// Достигнутый потолок проверок и так виден в ответе (checked).
		if pr.Checked >= limit {
			break
		}
		// Потолок времени обязателен: без него limit=50 идёт полчаса, и вызов
		// инструмента выглядит зависшим, хотя процесс честно работает.
		if pr.Exceeded() {
			break
		}
		if known[strings.ToLower(o.URL)] {
			continue
		}
		pr.Checked++
		rep["checked"] = pr.Checked
		// Контекст проверки ограничен остатком бюджета: один медленный
		// кандидат не должен съесть всё время и оставить остальных.
		cctx := ctx
		if left := pr.Remaining(); left > 0 {
			var cancel context.CancelFunc
			cctx, cancel = context.WithTimeout(ctx, left)
			defer cancel()
		}
		cand, n, err := d.Promoter.Check(cctx, o.URL)
		if err != nil {
			// Отказ проверки обязан доехать до вызывающего инструмента.
			// Checked уже увеличен, и без счётчика ответ «checked: 1,
			// promoted: []» означал бы «кандидат проверен, движков не найдено»
			// вместо «проверка не состоялась».
			pr.Fail(err)
			continue
		}
		if known[strings.ToLower(cand.Base)] {
			knownSkipped++
			continue
		}
		if err := d.Store.UpsertEngineSeed(ctx, store.EngineSeed{
			Name: cand.Name, Base: cand.Base, Path: cand.Path,
			Selector: cand.Selector, Category: "general", Auto: true,
		}); err != nil {
			pr.FailSave(err)
			continue
		}
		if d.Search != nil && d.Search.Onion != nil {
			promote.Attach(d.Search.Onion, d.Search.Health, d.Promoter.Client, cand)
		}
		_ = d.Store.TouchEngineSeed(ctx, cand.Name)
		rep["promoted"] = append(rep["promoted"].([]any), map[string]any{
			"name": cand.Name, "base": cand.Base, "onion_hits": n,
		})
		known[strings.ToLower(cand.Base)] = true
	}
	rep["elapsed"] = pr.Elapsed().String()
	rep["failed"] = pr.Failed
	rep["save_failed"] = pr.SaveFailed
	// Счётчик «форма есть, движок уже существует» отдельным ключом: без него
	// checked не раскладывается на promoted + failed + save_failed.
	if knownSkipped > 0 {
		rep["already_engines"] = knownSkipped
	}
	if pr.LastError != "" {
		rep["last_error"] = pr.LastError
	}
	// Причина досрочной остановки видна вызывающему: без неё короткий список
	// promoted выглядит как «ничего не нашлось», хотя на самом деле кончилось
	// отведённое время и проверена только часть пула.
	if pr.StoppedReason != "" {
		rep["stopped"] = pr.StoppedReason
	}
	return jsonResult(rep)
}

func (d Deps) metricsHandler(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	mc := metrics.Default
	if d.Search != nil && d.Search.Metrics != nil {
		mc = d.Search.Metrics
	}
	// Снимок метрик - map[string]int64, ответ - общий map: счётчики
	// переносятся по одному, чтобы поверх них можно было добавить
	// нечисловые ключи (problems) без приведения типов.
	snap := mc.Snapshot()
	out := make(map[string]any, len(snap)+1)
	for k, v := range snap {
		out[k] = v
	}
	// Этап 178: находки охот читаются из базы. Счётчик в памяти рождался бы
	// нулём после перезапуска при непустой истории, а описание инструмента
	// обещает «находки охот». Отказ базы не подменяется нулём: вызывающий
	// видит ключ problems, а не «охоты ничего не нашли».
	if d.Store != nil {
		if n, err := d.Store.CountHuntHits(ctx); err != nil {
			withProblems(out, []string{fmt.Sprintf("находки охот: %v", err)})
		} else {
			out["hunt_hits_total"] = n
		}
	}
	return jsonResult(out)
}

func (d Deps) backupHandler(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if d.Store == nil {
		return mcp.NewToolResultError("база не инициализирована"), nil
	}
	if strings.TrimSpace(d.BackupDir) == "" {
		return mcp.NewToolResultError("каталог бэкапов не настроен"), nil
	}
	keep := req.GetInt("keep", d.BackupKeep)
	if keep <= 0 {
		keep = 7
	}
	if keep > 30 {
		keep = 30
	}
	// Ротация, стирающая больше одного снимка, требует явного подтверждения:
	// инструмент доступен LLM-агенту, и до правки один вызов с keep=1 стирал всю
	// историю резервных копий, а ответ об этом молчал.
	planned, err := store.PlannedPrune(d.BackupDir, keep)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if planned > 1 && !req.GetBool("confirm_prune", false) {
		return mcp.NewToolResultError(fmt.Sprintf(
			"ротация удалит %d снимков из %d: передайте confirm_prune=true, если это намеренно",
			planned, planned+keep-1)), nil
	}
	dest, err := d.Store.BackupRotate(ctx, d.BackupDir, keep)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return jsonResult(map[string]any{"snapshot": dest, "keep": keep, "pruned": planned})
}

func (d Deps) judgeHandler(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if d.Store == nil {
		return mcp.NewToolResultError("база не инициализирована"), nil
	}
	query, err := req.RequireString("query")
	if err != nil {
		return mcp.NewToolResultError("параметр query обязателен"), nil
	}
	raw, _ := req.GetArguments()["votes"].([]any)
	if len(raw) == 0 {
		return mcp.NewToolResultError("параметр votes обязателен (массив {url, score})"), nil
	}
	// Потолок на пачку. Без него массив из ста тысяч элементов превращался в
	// сто тысяч последовательных записей в базу с одним соединением и без
	// атомарности, блокируя все остальные инструменты надолго. В паре с
	// отсутствовавшим ограничением на размер тела запроса это был легко
	// достижимый отказ обслуживания.
	if len(raw) > maxVotes {
		return mcp.NewToolResultError(fmt.Sprintf("votes: максимум %d за вызов, передано %d", maxVotes, len(raw))), nil
	}
	author := req.GetString("author", "")
	// QueryHash, а не CacheKey: голоса привязаны к запросу как к сущности, а
	// не к конкретной записи кэша. limit здесь смысла не имеет, а попади он в
	// хеш - голоса по одному запросу разъехались бы на разные записи.
	hash := store.QueryHash(query, "judge")
	// Число строк по запросу снимается до и после записи: разница отвечает на
	// вопрос «сколько голосов добавилось», который не выводится из счётчика
	// обработанных элементов пачки.
	before, _, err := d.Store.VotesForQuery(ctx, hash)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	votes := make([]store.Vote, 0, len(raw))
	var skippedNoURL, skippedScore, skippedNotObject, clamped int
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			skippedNotObject++
			continue
		}
		u, _ := m["url"].(string)
		u = strings.TrimSpace(u)
		// Пустой url отсеивается здесь, а не в базе. Прежний код отдавал такие
		// голоса в SubmitVotes, тот их отбрасывал, и они не попадали ни в
		// accepted, ни в skipped: пачка из одиннадцати элементов давала ответ
		// «accepted 7, skipped 2», и два голоса исчезали без следа. Клиент,
		// который сверяет числа, видел расхождение и не понимал, куда смотреть, а
		// клиент, который не сверяет, считал пачку принятой целиком.
		if u == "" {
			skippedNoURL++
			continue
		}
		// Оценка обязана разбираться из всех форм, в которых её присылает
		// клиент, и не может молча превращаться в ноль.
		//
		// Прежний switch не обрабатывал string, поэтому {"score": "0.9"} -
		// обычное дело для JS-клиентов MCP и вызовов из шаблонов - не попадал
		// ни в одну ветку, score оставался нулевым, и голос записывался как
		// 0.0, то есть как наихудшая возможная оценка. HostQuality после этого
		// задвигал такие хосты штрафом до -3, и порча сохранялась в SQLite
		// между перезапусками, тихо ухудшая каждый последующий поиск.
		//
		// Неразобранная оценка теперь пропускается, а не пишется нулём.
		var score float64
		var parsed bool
		switch v := m["score"].(type) {
		case float64:
			score, parsed = v, true
		case int:
			score, parsed = float64(v), true
		case json.Number:
			score, err = v.Float64()
			parsed = err == nil
		case string:
			score, err = strconv.ParseFloat(strings.TrimSpace(v), 64)
			parsed = err == nil
		}
		if !parsed || math.IsNaN(score) || math.IsInf(score, 0) {
			skippedScore++
			continue
		}
		// Оценка вне шкалы приводится к границе в SubmitVotes, и само приведение
		// остаётся там: одно место на все вызывающие. Здесь оно становится
		// видимым. Клиент, приславший 5.0 по забывчивости про диапазон, получал
		// голос «лучший результат» с бонусом +3 в реранке и ни слова о том, что
		// значение искажено; -2.0 точно так же превращался в штраф -3.
		if score < 0 || score > 1 {
			clamped++
		}
		votes = append(votes, store.Vote{QueryHash: hash, URL: u, Score: score, Author: author})
	}
	n, err := d.Store.SubmitVotes(ctx, votes)
	if err != nil {
		// Частичная запись обязана быть видна. Раньше возвращался только текст
		// ошибки, и клиент не мог узнать, сколько голосов всё-таки легло, -
		// повторял отправку и задваивал оценки.
		return mcp.NewToolResultError(fmt.Sprintf("%v (записано %d из %d)", err, n, len(votes))), nil
	}
	after, authors, err := d.Store.VotesForQuery(ctx, hash)
	if err != nil {
		// Голоса к этому моменту записаны, поэтому причина отказа сообщает и об
		// этом: иначе клиент повторит отправку и задвоит пачку.
		return mcp.NewToolResultError(fmt.Sprintf("%v (оценки записаны: %d из %d)", err, n, len(votes))), nil
	}
	added := after - before
	if added < 0 {
		added = 0
	}
	// updated - сколько оценок легло на уже существующие строки. Отрицательным
	// он становится, если между двумя замерами голоса добавил кто-то ещё: тогда
	// прирост строк больше числа обработанных оценок. Ноль честнее минуса,
	// потому что «минус три обновлённых строки» не означает ничего.
	updated := n - added
	if updated < 0 {
		updated = 0
	}
	// accepted - число обработанных оценок, new_rows - сколько из них легло
	// новыми строками. Повторная отправка той же пачки тем же автором обновляет
	// существующие строки: accepted остаётся прежним, new_rows равен нулю, и
	// клиент видит, что база не выросла.
	out := map[string]any{
		"query":          query,
		"votes_total":    len(raw),
		"accepted":       n,
		"new_rows":       added,
		"updated_rows":   updated,
		"stored_rows":    after,
		"stored_authors": authors,
	}
	// Этап 178 (смоук-K/J): счётчики событий живут по контракту описания -
	// «присутствуют в ответе только при событии: при чистой пачке их нет,
	// а не нули - «нет ключа» значит «события не было»». До этой правки
	// clamped и skipped_* кладутся всегда, и чистая пачка приходила с
	// четырьмя нулевыми ключами против собственного описания инструмента.
	// Договор «absent значит не случилось» - этап 174/175, тот же, что у
	// category и событий judge.
	if clamped > 0 {
		out["clamped"] = clamped
		out["clamped_reason"] = "оценка вне 0..1 приведена к границе шкалы"
	}
	if skippedNoURL > 0 {
		out["skipped_no_url"] = skippedNoURL
	}
	if skippedScore > 0 {
		out["skipped_bad_score"] = skippedScore
	}
	if skippedNotObject > 0 {
		out["skipped_not_object"] = skippedNotObject
	}
	if skipped := skippedNoURL + skippedScore + skippedNotObject; skipped > 0 {
		out["skipped"] = skipped
		out["skipped_reason"] = "пустой url, оценка не распознана как число или элемент не объект"
	}
	return jsonResult(out)
}

var (
	_ = parser.TitleOf
	_ = hunt.HashURLs
)
