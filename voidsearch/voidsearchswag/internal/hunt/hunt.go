// Package hunt - фоновые мониторинги из плана v3 (этап 6).
//
// Периодический прогон запроса по расписанию, diff по hash выдачи:
// новые находки отдаются вызывающему, а не тонут в повторной выдаче.
// Хранилище уже есть (таблица hunts + CreateHunt/ListHunts/TouchHunt),
// здесь только логика расписания и сравнения.
package hunt

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"voidsearchswag/internal/router"
	"voidsearchswag/internal/store"
)

// ErrSearch помечает отказ поиска, а не поломку самой охоты. Метка нужна
// ожиданию: WatchDetailed отличает «поиск не ответил» от «охоты с таким id нет»
// или «база не читается» и в первом случае продолжает опросы, накапливая
// отказы, а во втором прерывается. Без метки любое возвращение ошибки из RunOne
// обрывало watch на первом же опросе, и диагностика терялась ровно там, где она
// нужнее всего - при мёртвом tor.
var ErrSearch = errors.New("поиск не выполнен")

// SearchError - отказ поиска с причиной. Отдельный тип нужен, чтобы ожидание
// показывало причину без служебного префикса: Note() уже говорит «поиск не
// выполнился ни разу», и тот же текст внутри причины читался как «поиск не
// выполнился ни разу: отказов 4: поиск не выполнен: все движки поиска отказали».
type SearchError struct {
	// Reason - почему поиск не выполнен: текст ошибки движка или счётчик
	// отказавших движков.
	Reason string
}

func (e *SearchError) Error() string { return ErrSearch.Error() + ": " + e.Reason }

// Unwrap возвращает метку ErrSearch: вызывающий проверяет её через errors.Is, а
// причину достаёт через errors.As.
func (e *SearchError) Unwrap() error { return ErrSearch }

// searchReason достаёт причину отказа поиска без служебного префикса. Ошибки
// без типа SearchError отдаются как есть: терять текст нельзя, даже если он
// пришёл не из RunOne.
func searchReason(err error) string {
	var se *SearchError
	if errors.As(err, &se) && se.Reason != "" {
		return se.Reason
	}
	return err.Error()
}

// ctxReason отличает служебные тексты пакета context от настоящей причины
// отказа. Оператору они не говорят ничего: «context deadline exceeded» не
// объясняет, кто именно оборвал работу и почему.
func ctxReason(s string) bool {
	return strings.Contains(s, context.DeadlineExceeded.Error()) ||
		strings.Contains(s, context.Canceled.Error())
}

// SearchOutcome - что поиск рассказал охоте. URL нужны для hash, а счётчики
// движков - чтобы отличить «выдача пуста» от «спрашивать было некого». Без
// второй половины отказ всех движков выглядел как честная пустая выдача: охота
// записывала sha256 пустой строки базовым hash, затирала настоящий и выдавала
// changed=true с нулём URL, а следующий живой прогон приносил ложную находку.
type SearchOutcome struct {
	URLs []string
	// EnginesFailed - сколько движков не ответило.
	EnginesFailed int
	// EnginesTotal - сколько движков опрашивалось вообще. Ноль означает, что
	// отчёта движков не было (например, выдачу целиком отдал кэш), и тогда
	// пустая выдача считается пустой выдачей: выводить отказ не из чего.
	EnginesTotal int
	// Degraded - ядро сообщило о деградации: фолбэк на другой режим или
	// «onion-выдача пуста, отработал clearnet».
	Degraded bool
	// Note - пояснение ядра к исходу поиска.
	Note string
}

// EnginesDown сообщает, что опрашивать было некого: движки известны и ни один
// не ответил. Неравенство строгое, а не «URL пустой», потому что живой движок
// без результатов - это факт о мире, и сравнивать с базой нужно именно его.
func (o SearchOutcome) EnginesDown() bool {
	return o.EnginesTotal > 0 && o.EnginesFailed >= o.EnginesTotal
}

// Reason собирает причину отказа для LastError и для ошибки RunOne: счётчик
// говорит, сколько охот не выполнилось, а оператору нужно знать, что чинить.
func (o SearchOutcome) Reason() string {
	reason := fmt.Sprintf("все движки поиска отказали (%d из %d)", o.EnginesFailed, o.EnginesTotal)
	if o.Note != "" {
		reason += ": " + o.Note
	}
	return reason
}

// SearchFunc прогоняет запрос и возвращает исход поиска. Реализация -
// поисковое ядро (Engine.Search -> Outcome), в тестах - заглушка.
type SearchFunc func(ctx context.Context, query, mode string, limit int) (SearchOutcome, error)

type Runner struct {
	Store  *store.Store
	Search SearchFunc
	Limit  int
	Now    func() time.Time
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Runner) limit() int {
	if r.Limit > 0 {
		return r.Limit
	}
	return 20
}

// Create заводит мониторинг. Пустой запрос отклоняется: охота без запроса
// будет сравнивать мусор с мусором и всегда находить "новое".
//
// Режим проверяется и приводится к каноническому значению здесь, а не в
// вызывающем коде: охоту заводят двое - CLI и MCP-инструмент hunt_create, - и
// проверка в одном из них оставляет дыру в другом. До правки любое значение
// ложилось в базу как есть, а адаптер поиска в cmd при разборе молча подменял его
// на auto: охота «мониторю даркнет» с опечаткой в режиме становилась обычным
// clearnet-поиском, находки приходили не те, и ни лог, ни код возврата об этом
// не говорили. Синонимы tor и onion приводятся к deep, пустой режим - к auto,
// чтобы hunt list не показывал три названия одного и того же.
func (r *Runner) Create(ctx context.Context, query, mode string, scheduleMin int) (int64, error) {
	if r.Store == nil {
		return 0, fmt.Errorf("хранилище не задано")
	}
	if strings.TrimSpace(query) == "" {
		return 0, fmt.Errorf("пустой запрос")
	}
	m, ok := router.Parse(mode)
	if !ok {
		return 0, fmt.Errorf("недопустимый режим %q: auto|fast|stealth|deep", mode)
	}
	return r.Store.CreateHunt(ctx, store.Hunt{Query: query, Mode: string(m), ScheduleMin: scheduleMin})
}

func (r *Runner) List(ctx context.Context) ([]store.Hunt, error) {
	if r.Store == nil {
		return nil, fmt.Errorf("хранилище не задано")
	}
	return r.Store.ListHunts(ctx)
}

// Hit - сработавший мониторинг: hash выдачи сменился.
type Hit struct {
	HuntID  int64    `json:"hunt_id"`
	Query   string   `json:"query"`
	Mode    string   `json:"mode"`
	URLs    []string `json:"urls"`
	Count   int      `json:"count"`
	Changed bool     `json:"changed"`
	// Saved - сколько url этой находки добавлено в историю hunt_hits впервые.
	// Ноль при Changed=true означает, что вся выдача уже была видна раньше:
	// hash сменился от порядка или от исчезнувших адресов, а нового ничего не
	// появилось. Без этого числа «находка» и «новый адрес» были одним словом.
	Saved int `json:"saved"`
	// SaveError - почему история не записана. Сама находка при этом не
	// отменяется: hash уже обновлён и откатить его нельзя, поэтому вызывающий
	// обязан увидеть предупреждение вместо молчаливой потери истории.
	SaveError string `json:"save_error,omitempty"`
}

// Report - итог прогона по всем охотам.
type Report struct {
	Checked int `json:"checked"`
	Skipped int `json:"skipped"`
	// Failed - охоты, у которых поиск вернул ошибку. Отдельно от Skipped
	// потому, что это разные события: Skipped означает «расписание ещё не
	// вышло, прогон не начинался», Failed - «прогон начался и не получился».
	// Пока счётчик не был отделён, отказ поиска увеличивал Skipped, и строка
	// «проверено 0, пропущено 3» читалась как «ещё не пора» вместо «все три
	// поиска легли».
	Failed int `json:"failed"`
	// LastError - текст первой ошибки поиска за прогон: счётчик говорит,
	// сколько охот не выполнилось, а причина нужна, чтобы понять, чинить tor,
	// запрос или базу.
	LastError string `json:"last_error,omitempty"`
	Hits      []Hit  `json:"hits"`
}

// WatchReport - развёрнутый итог ожидания.
//
// Голый таймаут неотличим от сломанной охоты: в обоих случаях список находок
// пуст. Вызов MCP возвращал `hits: [], timeout: true`, и по этому ответу
// нельзя было понять, то ли охота работает и выдача просто не менялась, то ли
// поиск перестал возвращать результаты совсем. Счётчик опросов и размер
// последней выдачи разделяют эти два случая.
type WatchReport struct {
	Hits    []Hit `json:"hits"`
	Timeout bool  `json:"timeout"`
	// Polls - сколько раз охота реально прогонялась за ожидание.
	Polls int `json:"polls"`
	// LastCount - размер последней выдачи по конкретной охоте. Ноль при
	// пустой выдаче означает «поиск ничего не находит», а не «ничего не
	// изменилось».
	LastCount int `json:"last_count"`
	// Checked - сколько охот проверено суммарно за всё ожидание в режиме «все
	// по расписанию» (id <= 0). Накапливается, а не берётся из последнего
	// опроса: охота с расписанием due только в первом раунде, и значение
	// последнего раунда показало бы ноль вместо реально выполненной проверки.
	Checked int `json:"checked,omitempty"`
	// Failed - сколько прогонов поиска отказало за всё ожидание, суммарно по
	// опросам. Без него режим «все охоты» терял отказы вовсе: WatchDetailed
	// брал из ответа RunAll только Checked, а Note() при Checked == 0 утверждал,
	// что охота «не сломана по таймауту, она пуста», хотя поиск не выполнился
	// ни разу.
	Failed int `json:"failed,omitempty"`
	// LastError - причина первого отказа поиска за ожидание.
	LastError string `json:"last_error,omitempty"`
	Elapsed   string `json:"elapsed"`
}

// Note объясняет исход ожидания человеческим языком: вызывающему не должно
// приходиться угадывать разницу между пустой выдачей и неизменившейся.
//
// Отказы поиска проверяются раньше пустой выдачи и намеренно: при мёртвом tor
// охота не «пуста», она не проверена вовсе, а прежняя формулировка «не сломана
// по таймауту, она пуста» отправляла пользователя править запрос вместо того,
// чтобы починить поиск.
func (w WatchReport) Note() string {
	if len(w.Hits) > 0 {
		return "выдача изменилась"
	}
	if w.Polls == 0 {
		return "охота ни разу не прогнана"
	}
	if w.Failed > 0 {
		reason := ""
		if w.LastError != "" {
			reason = ": " + w.LastError
		}
		if w.Checked == 0 {
			return fmt.Sprintf("поиск не выполнился ни разу: отказов %d%s", w.Failed, reason)
		}
		// Этап 178 (смоук-K): формулировка не привязана к числу охот - id-режим
		// считает успешные прогоны одной охоты, режим «все охоты» складывает их
		// по пулу. «Проверено N охот» в id-режиме с единственной охотой
		// превращало смешанный ответ (2 успешных опроса, 1 отказ) в ложь
		// «поиск не выполнился ни разу» через соседнюю ветку Checked==0:
		// успешные опросы не инкрементировали Checked вовсе.
		return fmt.Sprintf("успешных прогонов %d, отказов поиска %d: картина неполная%s",
			w.Checked, w.Failed, reason)
	}
	// Checked из условия убран (смоук-K): в id-режиме успешный опрос теперь
	// честно инкрементирует Checked, и пустая охота с живым поиском обязана
	// по-прежнему называться пустой, а не «стабильной выдачей».
	if w.LastCount == 0 {
		return "поиск не вернул результатов ни в одном опросе: охота не сломана по таймауту, она пуста"
	}
	if w.Timeout {
		return "выдача стабильна, изменений за время ожидания не было"
	}
	return "ожидание завершено без находок"
}

// RunDue прогоняет все охоты, у которых вышло расписание. Первый прогон
// фиксирует базовый hash и находкой не считается: сравнивать пока не с чем.
func (r *Runner) RunDue(ctx context.Context) (Report, error) {
	return r.run(ctx, false)
}

// RunAll прогоняет все охоты, игнорируя их расписание.
//
// Нужно режиму ожидания. Watch - это явное ожидание изменения, и шаг опроса
// задаёт вызывающий, а не плановый интервал охоты. Через RunDue ожидание
// опрашивало пул один раз, а дальше до самого таймаута получало «пропущено по
// расписанию»: расписание по умолчанию 360 минут, поэтому заметить изменение
// внутри окна ожидания было физически невозможно.
func (r *Runner) RunAll(ctx context.Context) (Report, error) {
	return r.run(ctx, true)
}

func (r *Runner) run(ctx context.Context, ignoreSchedule bool) (Report, error) {
	if r.Store == nil {
		return Report{}, fmt.Errorf("хранилище не задано")
	}
	if r.Search == nil {
		return Report{}, fmt.Errorf("поиск не задан")
	}
	hunts, err := r.Store.ListHunts(ctx)
	if err != nil {
		return Report{}, err
	}
	now := r.now()
	// Этап 178 (смоук-G/I): пустой список отдаётся [], а не null: hunt_run и
	// hunt_watch уже обещали массив, а клиенты со строгой типизацией видели в
	// null три разных случая пустоты (hits=null у run, [] у watch, hunts=null
	// у list). Инициализация пустым срезом выравнивает все три.
	rep := Report{Hits: []Hit{}}
	for _, h := range hunts {
		sched := h.ScheduleMin
		if sched <= 0 {
			sched = 360
		}
		if !ignoreSchedule && !h.LastRun.IsZero() && now.Sub(h.LastRun) < time.Duration(sched)*time.Minute {
			rep.Skipped++
			continue
		}
		out, err := r.Search(ctx, h.Query, h.Mode, r.limit())
		if err != nil {
			// Отказ поиска больше не считается пропуском. Skipped означает
			// «расписание ещё не вышло», Failed - «прогон не получился», и
			// смешение этих двух случаев давало отчёт «проверено 0,
			// пропущено 3», который читался как «ещё не пора» при полностью
			// мёртвом tor.
			rep.Failed++
			if rep.LastError == "" {
				rep.LastError = err.Error()
			}
			// Этап 178 (смоук-G): last_run двигается и при отказе. Прогон
			// СОСТОЯЛСЯ - поиск вышел и вернулся ошибкой, - и hunt_list обязан
			// это показывать: до правки LastRun оставался zero-time после
			// трёх ручных и двух фоновых прогонов, все с отказами, и «последний
			// прогон» вранию. Hash не трогаем: базовый не затирается пустой
			// ошибкой, прежний перезаписывается собой же.
			_ = r.Store.TouchHunt(ctx, h.ID, h.LastHash)
			continue
		}
		if out.EnginesDown() {
			// Прогон начался и не получился, хотя err здесь nil: ядро отдаёт
			// пустую выдачу без ошибки, когда легли все движки. Считать такой
			// прогон выполненным значит записать sha256 пустой строки вместо
			// настоящего базового hash и подарить следующему живому прогону
			// находку, которой не было.
			rep.Failed++
			if rep.LastError == "" {
				rep.LastError = out.Reason()
			}
			_ = r.Store.TouchHunt(ctx, h.ID, h.LastHash)
			continue
		}
		urls := out.URLs
		hash := HashURLs(urls)
		if h.LastHash == "" {
			_ = r.Store.TouchHunt(ctx, h.ID, hash)
			rep.Checked++
			continue
		}
		rep.Checked++
		if hash != h.LastHash {
			_ = r.Store.TouchHunt(ctx, h.ID, hash)
			hit := Hit{
				HuntID: h.ID, Query: h.Query, Mode: h.Mode,
				URLs: urls, Count: len(urls), Changed: true,
			}
			hit.Saved, hit.SaveError = r.saveHits(ctx, hit)
			rep.Hits = append(rep.Hits, hit)
		}
	}
	return rep, nil
}

// RunOne прогоняет одну охоту вне расписания: расписание игнорируется,
// diff считается так же. Несуществующий id - ErrNotFound из стора.
func (r *Runner) RunOne(ctx context.Context, id int64) (Hit, error) {
	if r.Store == nil {
		return Hit{}, fmt.Errorf("хранилище не задано")
	}
	if r.Search == nil {
		return Hit{}, fmt.Errorf("поиск не задан")
	}
	hunts, err := r.Store.ListHunts(ctx)
	if err != nil {
		return Hit{}, err
	}
	for _, h := range hunts {
		if h.ID != id {
			continue
		}
		out, err := r.Search(ctx, h.Query, h.Mode, r.limit())
		if err != nil {
			// Этап 178 (смоук-G): отказ поиска - тоже состоявшийся прогон,
			// last_run двигается (см. комментарий в run). Hash сохраняется
			// прежним, чтобы отказ не затирал ни базу, ни последнюю находку.
			_ = r.Store.TouchHunt(ctx, h.ID, h.LastHash)
			return Hit{}, &SearchError{Reason: err.Error()}
		}
		if out.EnginesDown() {
			// Ошибка, а не Hit с changed=false: прогон не выполнен, и вызывающий
			// обязан это увидеть - CLI печатает «прогон: ...» и выходит с кодом 1,
			// MCP отдаёт isError. Молчаливый Hit затёр бы базовый hash и выдал
			// пустую находку как изменение выдачи. Обёртка ErrSearch отличает
			// такой исход от неизвестного id и от ошибки базы.
			_ = r.Store.TouchHunt(ctx, h.ID, h.LastHash)
			return Hit{}, &SearchError{Reason: out.Reason()}
		}
		urls := out.URLs
		hash := HashURLs(urls)
		changed := h.LastHash != "" && hash != h.LastHash
		_ = r.Store.TouchHunt(ctx, h.ID, hash)
		hit := Hit{HuntID: h.ID, Query: h.Query, Mode: h.Mode,
			URLs: urls, Count: len(urls), Changed: changed}
		hit.Saved, hit.SaveError = r.saveHits(ctx, hit)
		return hit, nil
	}
	return Hit{}, store.ErrNotFound
}

// saveHits пишет находку в историю hunt_hits и возвращает число новых url вместе
// с текстом ошибки записи.
//
// Пишется только настоящая находка: первый прогон фиксирует базовый hash и
// находкой не считается, поэтому его выдача в историю не попадает - иначе
// история начиналась бы со снимка того, что охота видела всегда. Пустая выдача
// при смене hash тоже не пишется: url в ней нет, а строка «новых 0» ничего бы не
// объяснила.
//
// Ошибка записи возвращается текстом, а не ошибкой: hash уже обновлён, откатить
// его нельзя, и ронять прогон из-за незаписанной истории значило бы потерять
// ещё и саму находку из отчёта.
func (r *Runner) saveHits(ctx context.Context, h Hit) (int, string) {
	if r.Store == nil || !h.Changed || len(h.URLs) == 0 {
		return 0, ""
	}
	added, err := r.Store.SaveHuntHits(ctx, h.HuntID, h.Query, h.Mode, h.URLs)
	if err != nil {
		return 0, err.Error()
	}
	return added, ""
}

// Hits отдаёт историю находок охоты: свежие раньше. id <= 0 означает «все
// охоты». Отдельный метод нужен, чтобы CLI и MCP не ходили в стор напрямую и не
// расходились в приведении limit.
func (r *Runner) Hits(ctx context.Context, id int64, limit int) ([]store.HuntHit, error) {
	if r.Store == nil {
		return nil, fmt.Errorf("хранилище не задано")
	}
	return r.Store.ListHuntHits(ctx, id, limit)
}

// ClearHits стирает историю находок одной охоты и возвращает число удалённых
// строк. Охота, которой нет, даёт ErrNotFound из стора.
func (r *Runner) ClearHits(ctx context.Context, id int64) (int, error) {
	if r.Store == nil {
		return 0, fmt.Errorf("хранилище не задано")
	}
	return r.Store.DeleteHuntHits(ctx, id)
}

// Watch ждёт находок блокирующим вызовом вместо поллинга: прогоняет охоты
// до первой смены hash или до таймаута. Расписание игнорируется - это явное
// ожидание, а не плановый прогон. Возвращает находки и флаг таймаута.
func (r *Runner) Watch(ctx context.Context, id int64, timeout, interval time.Duration) ([]Hit, bool, error) {
	rep, err := r.WatchDetailed(ctx, id, timeout, interval)
	if err != nil {
		return nil, false, err
	}
	return rep.Hits, rep.Timeout, nil
}

// watchParams приводит таймаут и интервал ожидания к рабочим значениям.
// Вынесено из цикла, чтобы дефолты проверялись напрямую: прогон реального
// ожидания ради проверки подстановки значений длился бы две минуты.
func watchParams(timeout, interval time.Duration) (time.Duration, time.Duration) {
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	if interval <= 0 {
		interval = 30 * time.Second
	}
	if interval > timeout {
		interval = timeout
	}
	return timeout, interval
}

// runDeadline ограничивает один прогон дедлайном ожидания.
//
// Дедлайн входящего контекста учитывается: если вызывающий дал срок короче
// timeout, прогон обрывается по более раннему из двух, и поведение не
// отличается от прежнего. Когда внешний срок не короче, возвращается сам ctx и
// пустая функция отмены - лишний дочерний контекст не создаётся, а отмена
// внешнего срока по-прежнему доходит до поиска напрямую.
func runDeadline(ctx context.Context, deadline time.Time) (context.Context, context.CancelFunc) {
	if dl, ok := ctx.Deadline(); ok && !deadline.Before(dl) {
		return ctx, func() {}
	}
	return context.WithDeadline(ctx, deadline)
}

// WatchDetailed - то же ожидание, но с диагностикой: сколько опросов сделано
// и какого размера была последняя выдача.
func (r *Runner) WatchDetailed(ctx context.Context, id int64, timeout, interval time.Duration) (WatchReport, error) {
	var rep WatchReport
	if r.Store == nil {
		return rep, fmt.Errorf("хранилище не задано")
	}
	if r.Search == nil {
		return rep, fmt.Errorf("поиск не задан")
	}
	timeout, interval = watchParams(timeout, interval)
	// Цикл ожидания идёт по реальным монотонным часам, а не по внедряемому
	// r.now(). Хук Now нужен бизнес-логике расписания охот; если считать от
	// него дедлайн ожидания, то при зафиксированных часах дедлайн оказывается
	// в прошлом ещё до первого опроса, и Watch возвращает таймаут мгновенно.
	started := time.Now()
	deadline := started.Add(timeout)
	for {
		// Дедлайн проверяется до прогона, а не только после него: шаг ожидания
		// проскакивает дедлайн на миллисекунды (гранулярность таймеров Windows
		// около 15 мс), и прогон, начатый с уже истёкшим сроком, обрывался на
		// первом же чтении базы. Ошибка «list hunts: context deadline exceeded»
		// уходила вызывающему как фатальная вместо честного Timeout=true - так
		// сломались шесть существующих тестов, когда прогон получил дедлайн.
		if time.Until(deadline) <= 0 {
			rep.Timeout = true
			rep.Elapsed = time.Since(started).Round(time.Millisecond).String()
			return rep, nil
		}
		var hits []Hit
		// Прогон ограничивается дедлайном ожидания, а не внешним контекстом.
		// Без этого один медленный поиск перешагивал --timeout на всю свою
		// длительность: дедлайн проверялся только между прогонами, а контекст
		// прогона жил до внешнего запаса (timeout + минута), который CLI даёт
		// намеренно, чтобы ожидание выходило красивым путём Timeout=true, а не
		// грубым ctx.Err(). Живой замер ДО на копии базы
		// %TEMP%\vss\livedata103: hunt watch --id 2 --timeout 5s при транспорте
		// static с прокси 192.0.2.1:9999 (dial без ответа) длился 15190 мс и
		// вернул elapsed=14.854s вместо заявленных пяти секунд.
		runCtx, cancelRun := runDeadline(ctx, deadline)
		left := time.Until(deadline)
		pollStart := time.Now()
		if id > 0 {
			hit, err := r.RunOne(runCtx, id)
			switch {
			case err == nil:
				// Размер выдачи запоминается даже когда hash не сменился: именно
				// он отличает стабильный результат от пустого.
				rep.LastCount = hit.Count
				// Этап 178 (смоук-K): успешный опрос id-режима инкрементирует
				// Checked. До правки поле двигали только отказы (Failed) и режим
				// «все охоты», поэтому смешанный цикл (2 успешных опроса и 1
				// отказ) давал Checked=0, и Note() объявлял через ветку
				// «Checked == 0» ложь «поиск не выполнился ни разу» при
				// last_count=19.
				rep.Checked++
				if hit.Changed {
					hits = []Hit{hit}
				}
			case errors.Is(err, ErrSearch):
				// Отказ поиска не прерывает ожидание: tor может подняться, а
				// движок ответить на следующем опросе. Отказ накапливается в
				// Failed, чтобы Note() назвал его вместо «охота пуста».
				rep.Failed++
				if rep.LastError == "" {
					rep.LastError = searchReason(err)
				}
			default:
				cancelRun()
				return rep, err
			}
		} else {
			// Расписание игнорируется намеренно: шаг опроса здесь задаёт
			// вызывающий, а плановый интервал охоты (360 минут по умолчанию)
			// заблокировал бы повторные проверки внутри окна ожидания.
			due, err := r.RunAll(runCtx)
			if err != nil {
				cancelRun()
				return rep, err
			}
			rep.Checked += due.Checked
			// Отказы накапливаются вместе с проверками: иначе режим «все
			// охоты» терял их вовсе, и Note() при Checked == 0 утверждал, что
			// охота «не сломана по таймауту, она пуста», хотя поиск не
			// выполнился ни разу.
			rep.Failed += due.Failed
			if rep.LastError == "" {
				rep.LastError = due.LastError
			}
			hits = due.Hits
		}
		cancelRun()
		// Прогон, оборванный дедлайном ожидания, объясняется сроком, а не виной
		// движков: они не успели ответить потому, что вызывающий закрыл окно.
		// Служебные тексты пакета context подменяются целиком, а настоящая
		// причина движков сохраняется в скобках - счётчик «2 из 2» говорит
		// оператору, что опрашивать было некого, и терять его нельзя.
		//
		// Обрыв отличается от мгновенного отказа тем, что прогон упёрся в
		// дедлайн: took >= left. Без этого условия срок приписывался любому
		// отказу, случившемуся в последнем опросе, и падал существующий тест
		// TestWatchLastErrorHasNoDuplicatePrefix: там движки отказывают сразу, и
		// причина обязана остаться их собственной. Внешняя отмена сюда не
		// попадает - у неё своя причина, и она уходит вызывающему через
		// ctx.Err() в select ниже.
		if took := time.Since(pollStart); runCtx.Err() != nil && ctx.Err() == nil && took >= left {
			const cut = "поиск оборван: истёк срок ожидания"
			switch {
			case rep.LastError == "" || ctxReason(rep.LastError):
				rep.LastError = cut
			case !strings.Contains(rep.LastError, cut):
				rep.LastError = cut + " (" + rep.LastError + ")"
			}
		}
		rep.Polls++
		if len(hits) > 0 {
			rep.Hits = hits
			rep.Elapsed = time.Since(started).Round(time.Millisecond).String()
			return rep, nil
		}
		// Шаг не выходит за дедлайн: остаток может быть меньше интервала, и
		// полный сон вывел бы команду за заявленный срок. Отрицательный остаток
		// означает, что дедлайн уже наступил - time.After с таким значением
		// срабатывает немедленно, и следующий круг начнётся с проверки дедлайна
		// и вернёт Timeout.
		step := interval
		if wait := time.Until(deadline); step > wait {
			step = wait
		}
		select {
		case <-ctx.Done():
			rep.Elapsed = time.Since(started).Round(time.Millisecond).String()
			return rep, ctx.Err()
		case <-time.After(step):
		}
	}
}

// HashURLs считает hash выдачи: порядок не важен, дубли не важны.
// Без сортировки один и тот же набор в разном порядке давал бы "новое"
// на каждом прогоне.
func HashURLs(urls []string) string {
	seen := map[string]bool{}
	uniq := make([]string, 0, len(urls))
	for _, u := range urls {
		if u = strings.TrimSpace(u); u != "" && !seen[u] {
			seen[u] = true
			uniq = append(uniq, u)
		}
	}
	sort.Strings(uniq)
	h := sha256.Sum256([]byte(strings.Join(uniq, "\x00")))
	return hex.EncodeToString(h[:])
}
