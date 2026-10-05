package search

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"voidsearchswag/internal/httpc"
	"voidsearchswag/internal/metrics"
	"voidsearchswag/internal/netx"
	"voidsearchswag/internal/router"
	"voidsearchswag/internal/searchers"
	"voidsearchswag/internal/store"
)

type Engine struct {
	Store    *store.Store
	Client   *httpc.Client
	Direct   *httpc.Client
	Metrics  *metrics.Counters
	Health   *searchers.HealthPool
	Onion    *searchers.OnionCatalog
	SearXNG  *searchers.SearXNG
	Ahmia    *searchers.AhmiaClear
	DDG      []searchers.Searcher
	Browser  searchers.Searcher
	Rot      netx.Rotator
	Log      netx.Logger
	CacheTTL time.Duration
	DefaultN int
	Grace    time.Duration
	// AllowPrivateTarget снимает барьер служебных адресов в FetchURL. Поле
	// нужно для локальной отладки и тестов, где стенд по определению живёт на
	// 127.0.0.1; продуктовые входы оставляют его выключенным.
	AllowPrivateTarget bool

	// muBoost защищает hostBoost и boostHosts. Кэш бонусов хранится по
	// query_hash: оценки судьи принадлежат запросу, а одна карта на все запросы
	// переносила чужие голоса в чужую выдачу.
	muBoost    sync.Mutex
	hostBoost  map[string]boostEntry
	boostHosts int
}

// boostEntry - бонусы хостов одного запроса вместе с моментом пересчёта.
type boostEntry struct {
	m  map[string]float64
	at time.Time
}

// boostTTL - насколько хватает пересчитанных бонусов одного запроса.
const boostTTL = time.Hour

// boostCacheLimit ограничивает число запросов в кэше бонусов. MCP-сервер живёт
// долго, запросов за это время проходит много, и кэш без потолка рос бы
// пропорционально всей истории обращений. Потерянная при переполнении запись
// стоит одного лишнего чтения базы, а не памяти.
const boostCacheLimit = 256

// pruneBoosts выкидывает самые старые записи, пока их число не упадёт до
// половины предела. Половина, а не одна запись: выкидывать по одной на каждый
// новый запрос значило бы сортировать карту на каждый поиск.
func pruneBoosts(m map[string]boostEntry) {
	if len(m) <= boostCacheLimit {
		return
	}
	type aged struct {
		k  string
		at time.Time
	}
	list := make([]aged, 0, len(m))
	for k, v := range m {
		list = append(list, aged{k: k, at: v.at})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].at.Before(list[j].at) })
	for i := 0; i < len(list)-boostCacheLimit/2; i++ {
		delete(m, list[i].k)
	}
}

func (e *Engine) Rotator() netx.Rotator { return e.Rot }

// engineGracePeriod - сколько ждём отстающие движки после того, как
// результатов уже набрано вдвое больше лимита. Без этого один
// зависший onion-поисковик держит всю выдачу до своего таймаута.
// Для tor 6с слишком мало: движки отвечают с разбросом от секунд до
// десятков секунд, и короткое окно молча режет выдачу до одного движка.
const engineGracePeriod = 20 * time.Second

func (e *Engine) grace() time.Duration {
	if e.Grace > 0 {
		return e.Grace
	}
	return engineGracePeriod
}

type Options struct {
	Query     string
	Mode      router.Mode
	Limit     int
	NoCache   bool
	ProxyPool bool
	// Validate проверяет топ выдачи живыми запросами: адрес, не ответивший
	// или ответивший 404/410, выкидывается с пометкой в Note. Опция
	// опциональна и по умолчанию выключена: проверка ходит по сети и стоит
	// до validateBudget времени. Замер ДО (смоук трёх агентов, v0.1.0):
	// hunt-выдача несла 3 URL из 3 с «no such host», и агент тратил
	// попытки на адреса, которых нет.
	Validate bool
}

type Outcome struct {
	Query    string             `json:"query"`
	Mode     router.Mode        `json:"mode"`
	UsedMode router.Mode        `json:"used_mode"`
	Degraded bool               `json:"degraded,omitempty"`
	Decision router.Decision    `json:"decision"`
	Results  []searchers.Result `json:"results"`
	Count    int                `json:"count"`
	Cached   bool               `json:"cached"`
	Report   Report             `json:"report"`
	Duration string             `json:"duration"`
	// Limit - сколько результатов запрашивалось, когда выдача была получена.
	// Поле нужно кэшу, а не пользователю: запись кэша одна на запрос и режим,
	// поэтому при чтении надо знать, хватает ли в ней результатов. Если в
	// записи меньше, чем просят сейчас, её нельзя отдавать как полный ответ.
	//
	// Ноль означает «запись сделана до появления поля». Такую запись кэш
	// считает недостачей и не отдаёт, иначе старые записи продолжили бы
	// возвращать урезанный ответ на любой limit.
	Limit int `json:"limit"`
	// CacheNote - почему кэш в этом прогоне не сработал: не прочитался или не
	// записался. Поле живёт отдельно от Report.Note, которая объясняет
	// деградацию выдачи («onion-выдача пуста, отработал clearnet»): это разные
	// факты, и смешанные в одной строке они перестали бы означать каждый своё.
	//
	// Пусто, когда кэш отработал штатно. Непустое значение означает, что поиск
	// удался, но каждый следующий запрос пойдёт в движки заново, - а это ровно
	// то, о чём вызывающий обязан знать.
	CacheNote string `json:"cache_note,omitempty"`
	// Note - зеркало Report.Note на верхнем уровне ответа. Смоук-агент этапа
	// 179 не нашёл канал потерь: пояснения о выкинутых токен-фильтром
	// результатах и о срезе по лимиту лежали в report.note, а оператор читает
	// верхний уровень - count, cached, duration. Поле дублирует Report.Note
	// финальным значением, чтобы канал потерь был виден без знания вложенности
	// схемы.
	Note string `json:"note,omitempty"`
}

type Report struct {
	Engines  []EngineReport `json:"engines"`
	Live     int            `json:"live_engines"`
	Total    int            `json:"total_engines"`
	Degraded bool           `json:"degraded,omitempty"`
	Note     string         `json:"note,omitempty"`
	// Fallbacks - запасные режимы, в которые ушёл поиск после пустой выдачи.
	//
	// Поле добавлено замером: Duration охватывает все прогоны движков, а Engines
	// описывает только тот режим, чья выдача осталась в ответе. Живой прогон на
	// HEAD d4c9f03, молчащий socks-прокси, VOIDSEARCH_REQUEST_TIMEOUT=2s,
	// search test --limit 2 --no-cache длился 16184 мс и напечатал
	// «режим: fast | 0 результатов | 16.011s» при двух движках с elapsed 4.002s
	// каждый: двенадцать секунд из шестнадцати не объясняло ничто, потому что
	// stealth и deep отработали и исчезли из отчёта.
	Fallbacks []FallbackReport `json:"fallbacks,omitempty"`
}

// FallbackReport - один запасной режим: сколько он стоил времени и что дал.
type FallbackReport struct {
	Mode     string `json:"mode"`
	Duration string `json:"duration"`
	Engines  int    `json:"engines"`
	Live     int    `json:"live_engines"`
}

type EngineReport struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Count   int    `json:"count"`
	Error   string `json:"error,omitempty"`
	Elapsed string `json:"elapsed,omitempty"`

	// Retried отмечает, что движок опрашивался повторно после смены tor-цепи.
	//
	// Поле добавлено вместе со слиянием отчётов. Прежняя версия дописывала
	// отчёты повторного опроса к отчётам первой попытки, поэтому каждый
	// onion-движок попадал в отчёт дважды, а rep.Total удваивался. Живой прогон
	// deep-режима показывал «движки: ahmia:fail, ahmia:fail, ahmia-clear:fail» -
	// три записи там, где движков два, и по отчёту было не понять, движков
	// столько или попыток.
	//
	// Сам факт повтора терять нельзя: он объясняет, почему поиск занял дольше
	// ожидаемого и почему движок, который обычно отвечает, здесь лёг. Поэтому
	// отчёты сливаются в один на движок, а повтор отмечается отдельно.
	Retried bool `json:"retried,omitempty"`

	// Timeout отмечает, что движок не уложился в срок: его ошибка - отказ
	// контекста, а не ответ «пусто» и не отказ транспорта вроде блокировки.
	//
	// Поле нужно решению о запасных режимах: когда каждый движок лёг по сроку,
	// транспорт не отвечает вовсе, и повторный опрос в другом режиме купит ровно
	// тот же отказ за полную цену. Замер до правки на HEAD 49e5da6, молчащий
	// socks-прокси, VOIDSEARCH_REQUEST_TIMEOUT=2s, search test --limit 2
	// --no-cache --json: duration=16.029s, оба движка с ошибкой «ddg: context
	// deadline exceeded», запасные режимы stealth 4.001s и deep 8.018s, то есть
	// двенадцать секунд из шестнадцати ушли на повтор того же отказа.
	//
	// Отличать срок от других причин по тексту ошибки нельзя: internal/hunt уже
	// делает так и это хрупко, поэтому признак выставляется здесь, пока ошибка
	// ещё доступна через errors.Is.
	Timeout bool `json:"timeout,omitempty"`
}

// mergeRetryReports подменяет отчёты первой попытки отчётами повторного опроса.
//
// Один отчёт на движок: дубли делали отчёт нечитаемым и завышали rep.Total.
// Если движка не было в первой попытке (например, каталог между опросами
// пополнил фоновый промоут), его отчёт добавляется.
//
// Флаг Retried выставляется на итоговом отчёте в обоих случаях: и когда повтор
// удался, и когда снова не удался. Ошибка берётся из повторной попытки, потому
// что она свежее: первая попытка могла лечь из-за битой цепи, которую сменили,
// и её текст уже не объясняет текущее состояние.
//
// Функция чистая: возвращает новый срез и не меняет входные, потому что отчёты
// уходят в JSON и в кэш, и мутирование на месте испортило бы уже собранные
// данные.
func mergeRetryReports(first, retry []EngineReport) []EngineReport {
	out := make([]EngineReport, len(first))
	copy(out, first)

	byName := make(map[string]int, len(out))
	for i := range out {
		byName[out[i].Name] = i
	}

	for _, r := range retry {
		r.Retried = true
		if i, ok := byName[r.Name]; ok {
			out[i] = r
			continue
		}
		byName[r.Name] = len(out)
		out = append(out, r)
	}
	return out
}

func (e *Engine) Search(ctx context.Context, o Options) (*Outcome, error) {
	query := strings.TrimSpace(o.Query)
	if query == "" {
		return nil, errors.New("пустой запрос")
	}
	limit := o.Limit
	if limit <= 0 {
		limit = e.DefaultN
	}
	if limit <= 0 {
		limit = 20
	}

	mode := o.Mode
	// autoRouted запоминается до перезаписи mode: дальше по коду mode уже
	// конкретный, и отличить «пользователь задал режим сам» от «роутер выбрал»
	// станет невозможно.
	autoRouted := mode == "" || mode == router.ModeAuto
	decision := router.Decision{}
	if autoRouted {
		decision = router.Route(query)
		mode = decision.Mode
	} else {
		// Неизвестный режим больше не уходит в ветку default ниже и не
		// превращается молча в clearnet. До правки движок принимал любое
		// значение: «телепорт» и «tor» давали одинаковый ответ - запросы в
		// открытый интернет, Decision.Reason «режим задан явно» и план
		// отступления [fast stealth deep], построенный для режима, которого не
		// существует. Режим deep с опечаткой означал утечку намерения в
		// открытую сеть, о которой вызывающий не узнавал.
		//
		// Проверка стоит до ключа кэша и до счётчиков, чтобы мусорный прогон не
		// успел записать себя как полноценный.
		//
		// Нормализация - не работа движка: router.Parse приводит tor и onion к
		// deep, и вызывающий обязан пройти через него. Принимать алиасы здесь
		// значило бы держать второй список синонимов, который разъедется с
		// первым при первом же дополнении.
		switch mode {
		case router.ModeFast, router.ModeStealth, router.ModeDeep:
		default:
			return nil, fmt.Errorf("недопустимый режим %q: auto|fast|stealth|deep", mode)
		}
		decision = router.Decision{Mode: mode, Reason: "режим задан явно", Fallback: router.FallbacksFor(mode)}
	}

	start := time.Now()
	key := store.CacheKey(query, string(mode))
	e.Metrics.Inc("search_total")
	e.Metrics.Inc(metrics.Key("search_mode", string(mode)))

	cacheNote := ""
	if !o.NoCache && e.Store != nil {
		payload, ok, err := e.Store.CacheGet(ctx, key)
		switch {
		case err != nil:
			// Ошибка чтения кэша больше не растворяется в условии
			// err == nil && ok. Поиск продолжается: кэш - ускорение, а не
			// источник выдачи. Но вызывающий обязан узнать, что ускорения нет,
			// иначе он будет ждать мгновенных повторных запросов и получать
			// каждый раз полноценный прогон по движкам.
			cacheNote = "кэш не прочитан, запрос пошёл в движки: " + err.Error()
			e.Metrics.Inc("search_cache_error")
		case ok:
			var cached Outcome
			// Запись годится, только если в ней не меньше результатов, чем
			// просят сейчас. Проверка по Limit, а не по len(Results): движки
			// могли честно вернуть меньше запрошенного, и тогда len совпал бы
			// со старым limit случайно.
			if json.Unmarshal([]byte(payload), &cached) == nil && cached.Limit >= limit {
				cached.Cached = true
				// Оговорка о кэше из сохранённой записи не переносится: кэш
				// отработал штатно, и прежняя поломка к этому ответу отношения
				// не имеет. Без очистки запись, сделанная при неработающем
				// кэше, вечно сообщала бы о поломке, которой уже нет.
				cached.CacheNote = ""
				cached.Duration = time.Since(start).Round(time.Millisecond).String()
				if len(cached.Results) > limit {
					cached.Results = cached.Results[:limit]
				}
				cached.Count = len(cached.Results)
				// Решение подставляем текущее, а не сохранённое. Запись кэша
				// разделяется по режиму, но не по тому, КАК режим был выбран:
				// явный `-mode fast` и авто-запрос, смаршрутизированный в fast,
				// попадают в одну и ту же запись. Если вернуть сохранённое
				// решение, авто-запрос отрапортует «режим задан явно», хотя
				// пользователь режим не задавал.
				if autoRouted {
					cached.Decision = decision
					cached.Mode = mode
				}
				e.Metrics.Inc("search_cached")
				// Validate на кэш-хите проверяет адреса заново: живость - свойство
				// сети в момент вызова, а не момент записи. Иначе второй агент
				// получал бы чужой давний вердикт по ссылкам, которые с тех пор
				// умерли или поднялись.
				if o.Validate {
					e.validateTop(ctx, &cached)
					// Кэш-хит обязан отчитывать своё настоящее время: validate
					// проверяет адреса заново, и это его секунды, а не миг
					// чтения записи.
					cached.Duration = time.Since(start).Round(time.Millisecond).String()
				}
				// Записи кэша, сделанные до появления зеркала, несут пояснения
				// только в Report.Note - копируем на верхний уровень после
				// validate, который может дописать туда мёртвые адреса.
				cached.Note = cached.Report.Note
				return &cached, nil
			}
		}
	}

	out := &Outcome{Query: query, Mode: mode, UsedMode: mode, Decision: decision, Limit: limit, CacheNote: cacheNote}
	switch mode {
	case router.ModeDeep:
		out.Results, out.Report = e.searchDeep(ctx, query, limit)
		if out.Report.Degraded {
			out.Degraded = true
		}
	case router.ModeStealth:
		out.Results, out.Report = e.searchClearnet(ctx, query, limit, true)
	default:
		out.Results, out.Report = e.searchClearnet(ctx, query, limit, false)
	}
	out.Count = len(out.Results)

	if len(out.Results) == 0 && len(decision.Fallback) > 0 {
		if !autoRouted {
			// Этап 162: явный режим - закон. Живой замер ДО (смоук трёх
			// независимых агентов на v0.1.0 c00d86b): юзер звал mode=fast,
			// движки clearnet лежали - сервер тихо подменил режим на deep с
			// used_mode=deep и reason «режим задан явно -> фоллбэк на deep».
			// Просящий «не ходить в tor» получал запрос в tor, а мусорная
			// ссылка из deep выдавалась за ответ clearnet. Теперь пустая
			// выдача и мёртвые движки дают честный пустой исход с
			// пояснением; цепочка запасных режимов - достояние авто-режима,
			// где решение о транспорте принимал роутер и сам может
			// передумать.
			reason := "выдача пуста"
			if allEnginesTimedOut(out.Report.Engines) {
				reason = "все движки легли по сроку, транспорт не отвечает"
			}
			out.Report.Note = joinNote(out.Report.Note,
				"режим задан явно: "+reason+", запасные режимы не запускались")
			e.logf("режим %s задан явно: %s, запасные режимы не запускаю", mode, reason)
		} else {
			// Все движки легли по сроку - значит транспорт не отвечает вовсе, и
			// запасной режим купит ровно тот же отказ за полную цену. Замер до
			// правки на HEAD 49e5da6, молчащий socks-прокси на 127.0.0.1:18999,
			// VOIDSEARCH_REQUEST_TIMEOUT=2s, search test --limit 2 --no-cache --json:
			// duration=16.029s, оба движка с ошибкой «ddg: context deadline
			// exceeded», запасные режимы stealth 4.001s и deep 8.018s. Двенадцать
			// секунд из шестнадцати ушли на повтор того же отказа, и ни один режим
			// не имел шанса: выход в сеть не работал.
			//
			// Признак берётся из поля Timeout отчёта, а не из текста ошибки: текст
			// принадлежит транспорту и меняется, а поле выставлено через errors.Is
			// там, где ошибка ещё живая.
			if allEnginesTimedOut(out.Report.Engines) {
				out.Report.Degraded = true
				out.Report.Note = joinNote(out.Report.Note,
					"запасные режимы не запускались: все движки легли по сроку, транспорт не отвечает")
				e.logf("режим %s: все движки легли по сроку, запасные режимы не запускаю", mode)
			} else {
				for _, fb := range decision.Fallback {
					if fb == mode {
						continue
					}
					e.logf("режим %s: пусто, фоллбэк на %s", mode, fb)
					fbStart := time.Now()
					var res []searchers.Result
					var rep Report
					switch fb {
					case router.ModeDeep:
						res, rep = e.searchDeep(ctx, query, limit)
					case router.ModeStealth:
						res, rep = e.searchClearnet(ctx, query, limit, true)
					default:
						res, rep = e.searchClearnet(ctx, query, limit, false)
					}
					// Запись о запасном режиме добавляется до возможной замены отчёта:
					// иначе успешный фоллбэк стёр бы сам факт того, что выдачу дали не
					// с первой попытки, и Duration снова осталась бы необъяснённой.
					fallbacks := append(out.Report.Fallbacks, FallbackReport{
						Mode:     string(fb),
						Duration: time.Since(fbStart).Round(time.Millisecond).String(),
						Engines:  rep.Total,
						Live:     rep.Live,
					})
					if len(res) > 0 {
						out.UsedMode = fb
						out.Degraded = true
						out.Results = res
						out.Report = rep
						out.Count = len(res)
						out.Decision.Reason += " -> фоллбэк на " + string(fb)
						out.Report.Fallbacks = fallbacks
						break
					}
					out.Report.Fallbacks = fallbacks
				}
			}
		}
	}

	// Реранк после всех фолбэков и до кэширования: в кэш ложится уже
	// отсортированная выдача, повторный разбор её не меняет.
	//
	// Отсев повторяющихся сниппетов идёт до реранка: score начисляет балл за
	// каждый токен запроса, найденный в сниппете, поэтому шапка сайта, общая для
	// всех результатов движка, завышала бы им оценку и влияла на порядок.
	out.Results = searchers.DropRepeatedSnippets(out.Results)
	// Бонусы берутся по тому же запросу, по которому голосовал судья: оценка
	// релевантности одного запроса не должна переставлять выдачу другого.
	// Область "judge" та же, что в judge_submit, иначе хэши разошлись бы и
	// голоса никогда не находились.
	out.Results = searchers.RerankWithHosts(out.Results, query, string(out.UsedMode),
		e.boosts(ctx, store.QueryHash(query, "judge")))
	out.Count = len(out.Results)

	// Отсев шума без сети: результат, в котором нет ни одного токена запроса,
	// - витрина движка, а не находка. Жалоба смоук-агентов на v0.1.0: запрос
	// «acme corp leak database» в deep приносил leak-маркеты без слова «acme»
	// вообще. Отдельный штраф в реранке этот мусор топнул, но из выдачи не
	// убирал. Фильтр обязан сниматься, когда совпадений нет ни у кого: пустой
	// ответ хуже шумного, а движки отвечают и синонимами - выкидывать всё
	// из-за строгости сравнения строк нельзя.
	//
	// Кэш пишет уже отфильтрованную выдачу, поэтому повторный запрос не
	// фильтрует заново: то же сравнение тех же строк дало бы тот же итог.
	if kept, dropped := searchers.DropQueryMiss(out.Results, query); len(dropped) > 0 {
		if len(kept) == 0 {
			out.Report.Note = joinNote(out.Report.Note,
				fmt.Sprintf("выдача не содержит ни одного токена запроса: фильтр шума снят, возвращено %d результатов", len(dropped)))
			e.logf("запрос %q: ни одного совпадения, фильтр шума снят (%d результатов)", query, len(dropped))
		} else {
			out.Results = kept
			out.Count = len(kept)
			out.Report.Note = joinNote(out.Report.Note,
				fmt.Sprintf("выкинуто %d результатов без единого токена запроса", len(dropped)))
			// Счётчик считает результаты, а не прогоны: имя
			// search_query_miss_dropped обещает «сколько выброшено», а Inc
			// на прогон при 8 выкинутых результатах в одном запросе
			// отчитывался единицей (смоук этапа 179 поймал 7 при минимум
			// 8 отсечённых в одном прогоне).
			e.Metrics.Add("search_query_miss_dropped", int64(len(dropped)))
			e.logf("запрос %q: выкинуто %d результатов без токенов запроса", query, len(dropped))
		}
	}

	// Срез по лимиту - последний шаг выдачи. Раньше его делал Dedupe внутри
	// searchDeep, до реранка и фильтра: витрина первого движка доедала лимит,
	// фильтр выбрасывал её остатки, и выдача приходила короче лимита при
	// живом втором движке с релевантными адресами (BEFORE этапа 179, «market»:
	// 7 из 15 при tornet ok=19 и torch ok=25). Теперь реранк и фильтр видят
	// весь пул, лимит получает уже отсортированный и очищенный топ. Note
	// называет число оставшихся за пределами: прежде молчаливый срез прятал
	// потери от оператора, и смоук-агент ловил выдачу короче лимита без
	// единого пояснения.
	if limit > 0 && len(out.Results) > limit {
		beyond := len(out.Results) - limit
		out.Results = out.Results[:limit]
		out.Count = limit
		out.Report.Note = joinNote(out.Report.Note,
			fmt.Sprintf("срез по лимиту: %d результатов за пределами топ-%d", beyond, limit))
	}

	if out.Degraded {
		e.Metrics.Inc("search_degraded")
	}
	e.Metrics.Add("search_results_total", int64(len(out.Results)))

	// Запись идёт и при NoCache: флаг отключает чтение, а не запись.
	// Прежний код пропускал и запись, и fresh-прогон с no_cache не обновлял
	// устаревшую запись кэша: следующий запрос без флага получал из кэша
	// выдачу, которую свежий прогон уже опроверг (BEFORE этапа 179: no_cache
	// вернул 7 результатов tornet, повтор без флага - 14 старых torch
	// с cached=true, дважды подряд).
	if e.Store != nil && len(out.Results) > 0 {
		payload, mErr := json.Marshal(out)
		if mErr != nil {
			// Исход не собрался - значит и следующему запросу нечего будет
			// отдавать из кэша. Это тоже поломка ускорения, и она обязана быть
			// видна, а не раствориться в пропущенной записи.
			out.CacheNote = joinCacheNote(out.CacheNote, "исход поиска не собран для кэша: "+mErr.Error())
			e.Metrics.Inc("search_cache_error")
		} else {
			ttl := e.CacheTTL
			if ttl <= 0 {
				ttl = time.Hour
			}
			// Отказ записи больше не уходит в _. Следующий запрос пойдёт в
			// движки заново, и это стоит времени и трафика: молчание
			// превращало постоянную поломку кэша в незамечаемую.
			if pErr := e.Store.CachePut(ctx, key, string(mode), string(payload), ttl); pErr != nil {
				out.CacheNote = joinCacheNote(out.CacheNote, "запись в кэш не удалась: "+pErr.Error())
				e.Metrics.Inc("search_cache_error")
			}
		}
	}
	// Валидация - после записи кэша и записана так намеренно. Кэш хранит
	// движковую выдачу, а «жив ли адрес» - свойство сети в момент вызова:
	// повторный запрос обязан проверить ссылку заново, а не получить чужой
	// давний вердикт. Кэш-хит выше прогоняет validate тем же кодом.
	if o.Validate {
		e.validateTop(ctx, out)
	}
	// Единственная точка замера времени живого пути - самый финал, после
	// validate. Раньше поле переприсваивалось по дороге (после движков, после
	// фоллбэков, после реранка, внутри validate), и любое из присваиваний
	// могло остаться последним: смоук этапа 165 поймал ответ, где секунды
	// validate не вошли в duration. Теперь пишется один раз и последним -
	// промежуточные присваивания убраны.
	out.Duration = time.Since(start).Round(time.Millisecond).String()
	// Зеркало Report.Note на верхний уровень: ставится последним, после
	// validate и среза по лимиту, чтобы совпасть с финальным текстом
	// пояснений - см. поле Note в Outcome.
	out.Note = out.Report.Note
	return out, nil
}

// validateBatch - сколько верхних результатов проверяет validateTop.
//
// Восемь, а не весь список: проверка ходит по сети, deep-выдача длинная, а
// адреса ниже топа агент почти не читает. Замеры смоука показали мусор уже
// в первой тройке, поэтому восьми хватает для боли и хватает для бюджета.
const validateBatch = 8

// validateBudget - общий потолок живых проверок одного вызова. Onion-адрес
// может молчать до своего срока запроса, и без общего потолка выдача из
// восьми мёртвых адресов держала бы вызов восемь полных таймаутов подряд.
const validateBudget = 90 * time.Second

// validateTop проверяет верх validateBatch результатов живыми запросами и
// выкидывает мёртвые. Мёртвым считается адрес, который не ответил (сеть,
// «no such host», срок) или ответил 404/410: хост жив, но страницы нет, и
// ссылка агенту бесполезна. Прочие статусы - от 200 до 403 и 5xx - считаются
// живыми: onion-серверы отвечают 403 сплошь, а 5xx -server лежит, но адрес
// существует.
//
// Страховка от полного выкоса: если не подтвердился ни один проверенный
// адрес, фильтр снимается. «Все мёртвые» чаще означает лежащий транспорт,
// а не восемь битых ссылок подряд, и подменять пустотой честную выдачу из-за
// собственной слепоты валидатор не имеет права. Note объясняет съём.
//
// Поле Duration здесь не пишется: единственная точка замера - финал Search
// (и финал кэш-ветки), чтобы ни один промежуточный путь не мог оставить
// устаревшее значение. Жалоба смоук-агента этапа 165 (заявлено 21.766s при
// реальных 113.86s) и контрольный замер этапа 166 (stealth 24.6s wall против
// 13.627s заявлено - на текущей сборке не воспроизведён, три повтора
// сходятся) дали правило: время пишется один раз, последним.
func (e *Engine) validateTop(ctx context.Context, out *Outcome) {
	if len(out.Results) == 0 {
		return
	}
	n := validateBatch
	if len(out.Results) < n {
		n = len(out.Results)
	}
	if e.Client == nil && e.Direct == nil {
		out.Report.Note = joinNote(out.Report.Note,
			"validate пропущен: транспорт не поднят")
		return
	}
	vctx, cancel := context.WithTimeout(ctx, validateBudget)
	defer cancel()

	alive := make([]searchers.Result, 0, len(out.Results))
	dead, checked := 0, 0
	for _, r := range out.Results {
		if r.URL == "" {
			alive = append(alive, r)
			continue
		}
		if checked < n {
			checked++
			if !e.urlAlive(vctx, r.URL) {
				dead++
				continue
			}
		}
		alive = append(alive, r)
	}
	e.Metrics.Add("search_validate_checked", int64(checked))
	if dead == 0 {
		if checked > 0 {
			out.Report.Note = joinNote(out.Report.Note,
				fmt.Sprintf("validate: проверено %d адресов, мёртвых нет", checked))
		}
		return
	}
	if len(alive) == 0 {
		out.Report.Note = joinNote(out.Report.Note,
			fmt.Sprintf("validate не подтвердил ни один из %d адресов: фильтр снят, транспорт мог лечь", checked))
		e.Metrics.Inc("search_validate_all_dead")
		return
	}
	out.Results = alive
	out.Count = len(alive)
	e.Metrics.Add("search_validate_dead", int64(dead))
	out.Report.Note = joinNote(out.Report.Note,
		fmt.Sprintf("validate: проверено %d адресов, выкинуто %d мёртвых", checked, dead))
}

// urlAlive решает по одному адресу. Onion идёт через tor-клиент: зону .onion
// резолвит только резолвер тора, прямой транспорт получил бы NXDOMAIN от
// провайдера и заодно слил бы факт поиска onion-адреса в открытый DNS.
// Clearnet идёт через прямой клиент, а перед этим - через барьер служебных
// сетей: движковая выдача - не внешний диктант адреса, но ссылка на
// 127.0.0.1 в выдаче - аномалия, и подтверждать её запросом в локальную
// сеть машина не обязана. AllowPrivateTarget снимает барьер для тестов.
func (e *Engine) urlAlive(ctx context.Context, rawURL string) bool {
	host := urlHost(rawURL)
	if strings.HasSuffix(host, ".onion") {
		if e.Client == nil {
			return false
		}
		resp, err := e.Client.Fetch(ctx, httpc.Request{URL: rawURL, Method: "GET"})
		return err == nil && resp != nil && resp.Status != 404 && resp.Status != 410
	}
	if !e.AllowPrivateTarget {
		if err := netx.CheckPublicTarget(ctx, rawURL); err != nil {
			return false
		}
	}
	c := e.Direct
	if c == nil {
		c = e.Client
	}
	if c == nil {
		return false
	}
	resp, err := c.Fetch(ctx, httpc.Request{URL: rawURL, Method: "GET"})
	return err == nil && resp != nil && resp.Status != 404 && resp.Status != 410
}

// urlHost вырезает хост из адреса в нижнем регистре. Отдельная от url.Parse
// небрежность не нужна: разбор обязан падать тихо, а не ронять валидацию
// целого вызова из-за кривой ссылки в выдаче - кривая просто считается
// мёртвой на уровне urlAlive.
func urlHost(rawURL string) string {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u == nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// joinCacheNote склеивает две оговорки о кэше. За один прогон может не
// прочитаться старая запись и не записаться новая, и терять вторую нельзя:
// иначе сообщение объяснит половину поломки.
func joinCacheNote(note, add string) string {
	return joinNote(note, add)
}

// joinNote склеивает две оговорки в одну строку. Правило общее для оговорок о кэше
// и о примечании отчёта: в deep-режиме Note уже несёт «onion-выдача пуста, отработал
// clearnet», и объяснение пропущенных запасных режимов обязано добавиться к нему, а
// не затереть.
func joinNote(note, add string) string {
	if note == "" {
		return add
	}
	return note + "; " + add
}

// allEnginesTimedOut сообщает, что каждый движок отчёта лёг по сроку.
//
// Пустой отчёт не считается отказом транспорта: движков могло не быть вовсе, и
// тогда запасной режим - единственная надежда на выдачу.
func allEnginesTimedOut(engines []EngineReport) bool {
	if len(engines) == 0 {
		return false
	}
	for _, er := range engines {
		if !er.Timeout {
			return false
		}
	}
	return true
}

func (e *Engine) searchClearnet(ctx context.Context, query string, limit int, stealth bool) ([]searchers.Result, Report) {
	var rep Report
	var all []searchers.Result

	ss := make([]searchers.Searcher, 0, len(e.DDG)+2)
	ss = append(ss, e.DDG...)
	if e.SearXNG != nil {
		ss = append(ss, e.SearXNG)
	}
	// Stealth-режим идёт через cloaked-браузер параллельно с HTTP-движками,
	// а не пожарным выходом в конце: иначе браузерный индекс никогда не
	// смешивается с HTTP-выдачей и режим stealth - фикция.
	if stealth && e.Browser != nil {
		ss = append(ss, e.Browser)
	}

	res, reports := e.querySearchersParallel(ctx, ss, query, limit)
	all = append(all, res...)
	rep.Engines = append(rep.Engines, reports...)
	rep.Total += len(ss)

	if len(all) == 0 && e.Browser != nil && !stealth {
		rep.Total++
		start := time.Now()
		res, err := e.Browser.Search(ctx, query, limit)
		er := EngineReport{Name: e.Browser.Name(), Elapsed: time.Since(start).Round(time.Millisecond).String()}
		if err != nil {
			er.Error = err.Error()
			e.logf("%s: %v", e.Browser.Name(), err)
		} else {
			er.OK = true
			er.Count = len(res)
			all = append(all, res...)
		}
		rep.Engines = append(rep.Engines, er)
	}

	// Тот же порядок, что в searchDeep: дедуп без среза, лимит режет
	// вызывающий - после реранка и фильтра шума.
	joined := searchers.DedupeAll(all)
	rep.Live = countOK(rep.Engines)
	return joined, rep
}

// querySearchersParallel опрашивает движки одновременно. Последовательный
// обход с ранним выходом по лимиту оставлял второй движок неопрошенным
// всякий раз, когда первый отдавал достаточно результатов.
// queryParallel опрашивает движки одновременно. Последовательный обход
// с ранним выходом по лимиту оставлял остальные движки неопрошенными
// всякий раз, когда первый отдавал достаточно результатов.
// Отстающий движок не держит выдачу: набрав вдвое больше лимита, ждём
// его лишь короткое окно, иначе таймаут одного движка тормозит весь поиск.
// engineErrText возвращает текст ошибки движка без повторного имени.
//
// Движок и так подписывает свою ошибку именем ("ahmia-clear: ссылок не
// найдено"), а отчёт и лог добавляли его ещё раз. Получалось
// "ahmia-clear: ahmia-clear: ссылок не найдено": половину строки занимает
// дубль, и причину сходу не видно. Особенно это мешало там, где ошибок и так
// много - в deep-режиме стабильно лежали два движка из четырёх, и разбор
// выдачи начинался с распутывания собственных имён.
func engineErrText(name string, err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if name == "" {
		return msg
	}
	prefix := name + ":"
	if rest, ok := strings.CutPrefix(msg, prefix); ok {
		return strings.TrimSpace(rest)
	}
	return msg
}

func (e *Engine) queryParallel(ctx context.Context, ss []searchers.Searcher, query string, limit int, markHealth bool) ([]searchers.Result, []EngineReport) {
	if len(ss) == 0 {
		return nil, nil
	}
	type outcome struct {
		idx int
		res []searchers.Result
		rep EngineReport
		err error
		el  time.Duration
	}

	ch := make(chan outcome, len(ss))
	for i, s := range ss {
		go func(i int, s searchers.Searcher) {
			start := time.Now()
			res, err := s.Search(ctx, query, limit)
			el := time.Since(start)
			ch <- outcome{
				idx: i,
				res: res,
				err: err,
				el:  el,
				rep: EngineReport{Name: s.Name(), Elapsed: el.Round(time.Millisecond).String()},
			}
		}(i, s)
	}

	outcomes := make([]outcome, 0, len(ss))
	got := 0
	var grace <-chan time.Time
	started := time.Now()
	// Причина обрыва опроса нужна отчёту: движки, которые не успели ответить,
	// обязаны попасть в него со своим именем и объяснением.
	var stopErr error
	graceHit := false

	for len(outcomes) < len(ss) {
		select {
		case <-ctx.Done():
			stopErr = ctx.Err()
			goto done
		case o := <-ch:
			outcomes = append(outcomes, o)
			if o.err == nil {
				got += len(o.res)
			}
			if grace == nil && limit > 0 && got >= limit*2 {
				grace = time.After(e.grace())
				e.logf("поиск: набрано %d результатов, дозор %v для отстающих", got, engineGracePeriod)
			}
		case <-grace:
			graceHit = true
			e.logf("поиск: окно дозора истекло, остальные движки не ждём")
			goto done
		}
	}

done:
	if len(outcomes) < len(ss) {
		e.logf("поиск: ответили %d из %d движков", len(outcomes), len(ss))
	}

	sort.Slice(outcomes, func(a, b int) bool { return outcomes[a].idx < outcomes[b].idx })

	var all []searchers.Result
	reports := make([]EngineReport, 0, len(ss))
	next := 0
	for i, s := range ss {
		if next >= len(outcomes) || outcomes[next].idx != i {
			// Движок не успел ответить: опрос оборвался по сроку вызывающего или
			// по окну дозора. Раньше он просто исчезал из отчёта, и в живом
			// прогоне search против молчащего прокси вернул
			// {"duration":"3m0s","report":{"engines":null,"live_engines":0,
			// "total_engines":2}} - ни имени, ни elapsed, ни причины. Оператор
			// видел «результатов нет» и шёл проверять запрос вместо выхода.
			//
			// Здоровье и engine_fail не трогаем: движок не отказал, он не успел,
			// а виноват срок, который выставил вызывающий.
			reason := stopReasonText(graceHit, stopErr)
			e.logf("%s: %s", s.Name(), reason)
			reports = append(reports, EngineReport{
				Name:    s.Name(),
				Elapsed: time.Since(started).Round(time.Millisecond).String(),
				Error:   reason,
				Timeout: errors.Is(stopErr, context.DeadlineExceeded),
			})
			continue
		}
		o := outcomes[next]
		next++
		if o.err != nil {
			o.rep.Error = engineErrText(o.rep.Name, o.err)
			o.rep.Timeout = errors.Is(o.err, context.DeadlineExceeded) || errors.Is(o.err, context.Canceled)
			if errors.Is(o.err, httpc.ErrOnionWithoutTor) {
				// Проверка не состоялась: без tor onion-движок недостижим в
				// принципе. FailStreak и SuccessRate не трогаем, иначе один
				// поиск в deep без tor приближал движки к вечному Disabled, а
				// счётчик engine_fail рос на ровном месте. В отчёте ошибка
				// остаётся - выдача действительно не получена.
				e.logf("%s: %s", o.rep.Name, o.rep.Error)
				reports = append(reports, o.rep)
				continue
			}
			e.Metrics.Inc(metrics.Key("engine_fail", o.rep.Name))
			if markHealth && e.Health != nil {
				e.Health.MarkResult(o.rep.Name, false, o.el)
			}
			e.logf("%s: %s", o.rep.Name, o.rep.Error)
		} else {
			o.rep.OK = true
			o.rep.Count = len(o.res)
			e.Metrics.Inc(metrics.Key("engine_ok", o.rep.Name))
			all = append(all, o.res...)
			if markHealth && e.Health != nil {
				e.Health.MarkResult(o.rep.Name, true, o.el)
			}
		}
		reports = append(reports, o.rep)
	}
	return all, reports
}

// stopReasonText объясняет, почему движок не попал в выдачу: опрос оборвался, не
// дождавшись его.
//
// Окно дозора и срок вызывающего - разные события, и путать их нельзя: в первом
// случае результаты уже набраны и поиск просто не стал ждать отстающих, во втором
// выдача пуста именно потому, что время вышло.
func stopReasonText(graceHit bool, stopErr error) string {
	if graceHit {
		return "окно дозора истекло, движок не успел ответить"
	}
	if errors.Is(stopErr, context.DeadlineExceeded) {
		return "срок поиска истёк, движок не ответил"
	}
	if stopErr != nil {
		return "поиск отменён, движок не ответил"
	}
	return "движок не ответил"
}

func (e *Engine) querySearchersParallel(ctx context.Context, ss []searchers.Searcher, query string, limit int) ([]searchers.Result, []EngineReport) {
	return e.queryParallel(ctx, ss, query, limit, false)
}

func (e *Engine) queryEnginesParallel(ctx context.Context, engines []*searchers.OnionEngine, query string, limit int) ([]searchers.Result, []EngineReport) {
	ss := make([]searchers.Searcher, 0, len(engines))
	for _, eng := range engines {
		ss = append(ss, eng)
	}
	return e.queryParallel(ctx, ss, query, limit, true)
}

func (e *Engine) searchDeep(ctx context.Context, query string, limit int) ([]searchers.Result, Report) {
	var rep Report
	var all []searchers.Result

	// Ahmia-clearnet идёт параллельно с onion-движками, но по прямому
	// каналу: поиск не жжёт tor-цепи, им нужен только для открытия адресов.
	// Поэтому deep отдаёт выдачу, даже когда tor-демон лёг целиком.
	type ahmiaOut struct {
		res []searchers.Result
		rep EngineReport
	}
	ahmiaCh := make(chan ahmiaOut, 1)
	if e.Ahmia != nil {
		go func() {
			start := time.Now()
			res, err := e.Ahmia.Search(ctx, query, limit)
			er := EngineReport{Name: e.Ahmia.Name(), Elapsed: time.Since(start).Round(time.Millisecond).String()}
			if err != nil {
				// Мост подписывает свою ошибку именем, поэтому имя снимается тем
				// же engineErrText, что и для движков в queryParallel. Без этого
				// отчёт нёс «ahmia-clear: ссылок не найдено», а печать и лог
				// добавляли имя ещё раз: «ahmia-clear: ahmia-clear: ссылок не
				// найдено».
				reason := engineErrText(er.Name, err)
				er.Error = reason
				e.logf("%s: %s", er.Name, reason)
			} else {
				er.OK = true
				er.Count = len(res)
			}
			ahmiaCh <- ahmiaOut{res: res, rep: er}
		}()
	}

	if e.Onion != nil {
		live := e.liveEngines()
		rep.Total = len(live)
		if len(live) == 0 {
			// Откат на все движки тоже через Snapshot: rep.Total и список
			// для обхода обязаны относиться к одному и тому же срезу, иначе
			// фоновый промоут между двумя чтениями сделает их несогласованными.
			all := e.Onion.Snapshot()
			rep.Total = len(all)
			e.logf("deep: живых onion-поисковиков нет, пробую все")
			live = all
		}
		all, rep.Engines = e.queryEnginesParallel(ctx, live, query, limit)
		rep.Live = countOK(rep.Engines)

		// Все onion-движки легли, а tor-транспорт жив: одна смена цепи и
		// повтор. Выходные ноды дохнут пачками, и первый прогон часто
		// упирается именно в битую цепь, а не в мёртвые движки.
		if rep.Live == 0 && e.Rot != nil {
			rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			rerr := e.Rot.Rotate(rctx)
			cancel()
			if rerr != nil {
				e.logf("deep: NEWNYM не ответил: %v", rerr)
			} else {
				e.logf("deep: цепь сменена, повторяю onion-опрос")
				retry, retryRep := e.queryEnginesParallel(ctx, live, query, limit)
				// Отчёты сливаются, а не дописываются: иначе каждый движок
				// попадает в отчёт дважды и rep.Total удваивается.
				rep.Engines = mergeRetryReports(rep.Engines, retryRep)
				// rep.Total не увеличивается: движки те же самые, опрос
				// повторный. Прежняя версия прибавляла len(live) и отчёт
				// утверждал, что движков вдвое больше, чем есть.
				all = append(all, retry...)
				rep.Live = countOK(rep.Engines)
			}
		}
	}

	if e.Ahmia != nil {
		ao := <-ahmiaCh
		rep.Total++
		rep.Engines = append(rep.Engines, ao.rep)
		all = append(all, ao.res...)
		rep.Live = countOK(rep.Engines)
	}

	if len(all) == 0 {
		e.logf("deep: onion пусто, фоллбэк на clearnet")
		rep.Degraded = true
		rep.Note = "onion-выдача пуста, отработал clearnet"
		res, crep := e.searchClearnet(ctx, query, limit, true)
		all = append(all, res...)
		rep.Engines = append(rep.Engines, crep.Engines...)
		rep.Live = countOK(rep.Engines)
		rep.Total += crep.Total
		if rep.Live == 0 {
			// Пояснение ставится до фоллбэка, поэтому при неудачном фоллбэке оно
			// врало: «отработал clearnet» при живых движках 0 из 10. Охота
			// подставляет этот текст в причину отказа, и оператор шёл проверять
			// запрос вместо того, чтобы чинить сеть.
			rep.Note = "onion-выдача пуста, фоллбэк на clearnet тоже не ответил"
		}
	}

	// Дедуп без среза: обрезка по лимиту переехала в конец Engine.Search,
	// за реранк и фильтр шума. Срез здесь отдавал монополию первому движку
	// окна - см. комментарий к DedupeAll.
	return searchers.DedupeAll(all), rep
}

// liveEngines отдаёт движки, по которым стоит искать прямо сейчас.
//
// Читает каталог через Snapshot: liveEngines вызывается из goroutine
// поисковых запросов, а фоновый тик discover и promote_engines в это же время
// дописывают в каталог новые движки. Обход поля Engines напрямую был бы гонкой.
func (e *Engine) liveEngines() []*searchers.OnionEngine {
	if e.Onion == nil {
		return nil
	}
	engines := e.Onion.Snapshot()
	if e.Health == nil {
		return engines
	}
	live := e.Health.Live()
	byName := make(map[string]*searchers.OnionEngine, len(engines))
	for _, eng := range engines {
		byName[eng.Name()] = eng
	}
	out := make([]*searchers.OnionEngine, 0, len(live))
	for _, h := range live {
		if eng, ok := byName[h.Name]; ok {
			out = append(out, eng)
		}
	}
	return out
}

func countOK(list []EngineReport) int {
	n := 0
	for _, e := range list {
		if e.OK {
			n++
		}
	}
	return n
}

func (e *Engine) logf(format string, args ...any) {
	if e.Log != nil {
		e.Log.Infof(format, args...)
	}
}

// boosts отдаёт выученные бонусы хостов для ОДНОГО запроса, обновляя их из базы
// не чаще раза в час. Без голосов по этому запросу - пустая карта, реранк
// работает на базовых весах: холодный старт ничего не меняет.
//
// Ключ кэша - query_hash, а не одна карта на всех. Голос судьи отвечает на
// вопрос «насколько этот URL подходит данному запросу», поэтому бонус,
// выученный на запросе про утечки, не имеет отношения к выдаче про рецепты.
// Живой замер ДО: три голоса по запросу «leak database» поднимали loved.example
// с седьмого места на пятое в выдаче «borsch recipe», где его никто не
// оценивал, а meh.example съезжал с пятого на седьмое.
//
// Чтение базы выполняется вне блокировки: при MaxOpenConns(1) скан relevance
// соревнуется за единственное соединение, и прежняя версия держала muBoost всё
// это время, из-за чего раз в час каждый параллельный поиск выстраивался в
// очередь за одним полным сканом. Выглядело как регулярный скачок задержки без
// видимой причины.
//
// Провал не кешируется. Прежняя запись `if err == nil { m = q }` означала, что
// одна переходная ошибка - отменённый контекст, на короткое время
// заблокированная база - оставляет пустую карту на час, и выученное
// ранжирование молча отключалось.
func (e *Engine) boosts(ctx context.Context, queryHash string) map[string]float64 {
	e.muBoost.Lock()
	if ent, ok := e.hostBoost[queryHash]; ok && time.Since(ent.at) < boostTTL {
		m := ent.m
		e.muBoost.Unlock()
		return m
	}
	prev, hadPrev := e.hostBoost[queryHash]
	e.muBoost.Unlock()

	m := map[string]float64{}
	if e.Store != nil {
		q, err := e.Store.HostQualityForQuery(ctx, queryHash, 3)
		if err != nil {
			// Прежние бонусы сохраняются: переходный сбой базы не повод
			// обнулять то, что уже выучено. Следующий вызов попробует снова.
			e.logf("бонусы хостов запроса не обновлены: %v", err)
			if hadPrev {
				return prev.m
			}
			return m
		}
		m = q
	}

	e.muBoost.Lock()
	if e.hostBoost == nil {
		e.hostBoost = map[string]boostEntry{}
	}
	e.hostBoost[queryHash] = boostEntry{m: m, at: time.Now()}
	pruneBoosts(e.hostBoost)
	e.muBoost.Unlock()
	return m
}

// RetuneBoosts сбрасывает выученные бонусы: зовётся ночным тиком и тестами.
// Возвращает число хостов, у которых есть бонус хотя бы по одному запросу:
// это сводка объёма выученного, ровно тот смысл, который вкладывал в
// возвращаемое значение прежний код.
//
// Кэш хранится по запросам, а ночной тик не знает, какие запросы придут
// завтра, поэтому он не пересчитывает карты, а выбрасывает их: следующий поиск
// каждого запроса перечитает свои голоса сам. Число хостов запоминается
// отдельно, чтобы сбой базы не выглядел как «голосов просто нет».
//
// Как и в boosts, чтение базы выполняется вне блокировки, а провал не
// записывается в кэш: иначе ночной тик на временно недоступной базе обнулял бы
// выученные веса до следующего успешного пересчёта.
func (e *Engine) RetuneBoosts(ctx context.Context) int {
	if e.Store == nil {
		e.muBoost.Lock()
		e.hostBoost = nil
		e.boostHosts = 0
		e.muBoost.Unlock()
		return 0
	}
	q, err := e.Store.HostQuality(ctx, 3)
	if err != nil {
		e.logf("пересчёт бонусов не удался, прежние сохранены: %v", err)
		e.muBoost.Lock()
		n := e.boostHosts
		e.muBoost.Unlock()
		return n
	}
	e.muBoost.Lock()
	e.hostBoost = nil
	e.boostHosts = len(q)
	e.muBoost.Unlock()
	return len(q)
}
