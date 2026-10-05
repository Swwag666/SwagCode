package discover

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"voidsearchswag/internal/httpc"
	"voidsearchswag/internal/store"
)

var errNoStore = errors.New("store не задан")

// ProbeConfig управляет волной проб. Onion-сервисы отвечают медленно и
// часто падают, поэтому проба - это только факт ответа: тело не читаем,
// важен статус и время. Параллелизм держим умеренным: слишком много
// одновременных цепей через один tor-демон даёт таймауты у всех сразу.
type ProbeConfig struct {
	Concurrency int
	Timeout     time.Duration
	Delay       time.Duration
}

func (c ProbeConfig) withDefaults() ProbeConfig {
	if c.Concurrency <= 0 {
		c.Concurrency = 8
	}
	if c.Timeout <= 0 {
		c.Timeout = 30 * time.Second
	}
	return c
}

type ProbeResult struct {
	URL       string `json:"url"`
	OK        bool   `json:"ok"`
	Status    int    `json:"status,omitempty"`
	LatencyMS int64  `json:"latency_ms"`
	Error     string `json:"error,omitempty"`
	// NoTransport - запрос не ушёл в сеть: нет ни tor, ни прокси, и onion-барьер
	// httpc отверг обращение до соединения. Это состояние окружения автора, а не
	// ответ сервиса, поэтому такая проба не пишет вердикт живости в пул и не
	// попадает в счётчик мёртвых. Прежнее поведение записывало отказ транспорта
	// как отказ сервиса: три запуска `probe` на машине без tor переводили живой
	// адрес в dead, потому что порог fail_streak срабатывает на третьей неудаче.
	NoTransport bool `json:"no_transport,omitempty"`
	// PoolStatus - состояние адреса в пуле после записи этой пробы: «live»,
	// «dead» или «unknown». Пусто, если проба не записывалась (нет базы).
	//
	// Поле отделено от OK намеренно. OK - исход одного обращения, PoolStatus -
	// вердикт накопительной статистики. Они расходятся штатно: первая неудача
	// нового адреса даёт OK=false и PoolStatus="unknown", потому что порог «dead»
	// срабатывает на третьей неудаче подряд. Вывод, печатавший «мертв» по флагу
	// OK, утверждал состояние пула, которое эта проба не установила, и
	// противоречил poolsearch.
	PoolStatus string `json:"pool_status,omitempty"`
}

type ProbeReport struct {
	Total int `json:"total"`
	Live  int `json:"live"`
	Dead  int `json:"dead"`
	// Skipped - сколько адресов не было спрошено из-за отсутствия транспорта
	// (нет ни tor, ни прокси). Счётчик отделён от Dead: «живых 0 из 1» читалось
	// как смерть адреса, хотя запрос не покидал машину. Total при этом остаётся
	// числом адресов в волне, поэтому Total = Live + Dead + Skipped.
	Skipped int `json:"skipped"`
	// Rejected - сколько адресов из выборки не дошло до волны: хост не является
	// корректным onion-адресом либо уже встречался в этой выборке. Счётчик
	// отделён от Total намеренно: Total описывает состоявшуюся волну, Rejected -
	// то, что до неё не дошло.
	//
	// До правки отброшенные адреса исчезали бесследно. Живой замер на HEAD
	// 40a742b, очередь из пяти непроверенных адресов, где три невалидны:
	// probe --limit 5 --json напечатал total=2, results=2, и ни одно поле не
	// упомянуло три потерянных адреса. На очереди, где невалидны все пять,
	// отчёт сказал «живых 0 из 0» и команда завершилась с кодом 0, то есть
	// сообщила «проверять нечего» при пяти непроверенных записях в пуле.
	Rejected int `json:"rejected"`
	// RejectedAddrs - сами отброшенные строки в порядке выборки: без них
	// оператор знает количество, но не знает, что именно чистить.
	RejectedAddrs []string `json:"rejected_addrs,omitempty"`
	Elapsed       string   `json:"elapsed"`
	LimitHit      string   `json:"limit_hit,omitempty"`
	// Cancelled - волна проб обрезана отменой контекста. Этап 174: до него
	// единственное событие стопа жило строкой в LimitHit («контекст
	// отменён»), и словарь пробы расходился с crawl-отчётом, где этап 172
	// уже разделил предел и отмену по разным полям. У пробы потолка хостов
	// нет: в волну уходит вся выборка, поэтому limit_hit здесь исторически
	// пуст, и отмена жила в чужом поле как единственный жилец. Теперь поля
	// те же, что у crawl: limit_hit - только предел, cancelled - только
	// отмена, оба могут отсутствовать.
	Cancelled bool          `json:"cancelled,omitempty"`
	Results   []ProbeResult `json:"results,omitempty"`
	LiveHosts []string      `json:"live_hosts,omitempty"`
	// Применённые пределы волны. Без них отчёт показывал только итог, и
	// оператор не мог отличить «живых ноль из трёх, потому что в пуле три
	// непроверенных» от «живых ноль из трёх, потому что лимит три при
	// очереди в десять тысяч». Значения берутся уже после withDefaults, то
	// есть это те числа, с которыми волна реально шла, включая подмену
	// неположительных параллельности и таймаута дефолтами ядра. RequestedLimit
	// заполняет ProbeWave: это потолок выборки из пула, а не размер волны.
	RequestedLimit int   `json:"requested_limit"`
	Concurrency    int   `json:"concurrency"`
	TimeoutMS      int64 `json:"timeout_ms"`
	DelayMS        int64 `json:"delay_ms"`
}

type Prober struct {
	Client  *httpc.Client
	Limiter *RateLimiter
	Store   *store.Store
	Log     Logger
	Cfg     ProbeConfig
}

func NewProber(client *httpc.Client, st *store.Store, log Logger, cfg ProbeConfig) *Prober {
	cfg = cfg.withDefaults()
	return &Prober{
		Client:  client,
		Limiter: NewRateLimiter(cfg.Delay),
		Store:   st,
		Log:     log,
		Cfg:     cfg,
	}
}

// isolatedClients готовит по клиенту на воркер, каждый со своей цепью tor.
//
// Если транспорт не поддерживает изоляцию (прямое соединение или обычный
// http-прокси), все воркеры получают исходный клиент: там параллелизм
// обеспечивает сеть, а не tor, и отдельная цепь ничего бы не ускорила.
func (p *Prober) isolatedClients(n int) []*httpc.Client {
	if n < 1 {
		n = 1
	}
	out := make([]*httpc.Client, 0, n)
	for i := 0; i < n; i++ {
		if p.Client == nil {
			out = append(out, nil)
			continue
		}
		cl, err := p.Client.Isolated(fmt.Sprintf("probe%d", i))
		if err != nil {
			out = append(out, p.Client)
			continue
		}
		out = append(out, cl)
	}
	if len(out) == 0 {
		out = append(out, p.Client)
	}
	return out
}

// closeIsolated освобождает сессии временных клиентов. Общий клиент не
// трогаем: он принадлежит вызывающему и переживает волну проб.
func (p *Prober) closeIsolated(clients []*httpc.Client) {
	for _, cl := range clients {
		if cl == nil || cl == p.Client {
			continue
		}
		if err := cl.Close(); err != nil && p.Log != nil {
			p.Log.Infof("probe: закрытие изолированного клиента: %v", err)
		}
	}
}

// Probe прогоняет волну проб по адресам, попутно записывая результат в
// пул. Порядок результата детерминирован: адреса сортируются до обхода,
// поэтому отчёт не зависит от того, кто ответил первым.
func (p *Prober) Probe(ctx context.Context, addrs []string) ProbeReport {
	start := time.Now()
	var rep ProbeReport

	clean := make([]string, 0, len(addrs))
	rejected := make([]string, 0, 4)
	seen := map[string]bool{}
	for _, a := range addrs {
		h := HostOf(a)
		if h == "" || seen[h] {
			// Отброшенный адрес обязан попасть в отчёт: молчаливый пропуск
			// читался как пустая очередь, и на полностью невалидной выборке
			// команда возвращала успех, не сделав ни одной пробы.
			rejected = append(rejected, strings.TrimSpace(a))
			continue
		}
		seen[h] = true
		clean = append(clean, h)
	}
	sort.Strings(clean)
	rep.Rejected = len(rejected)
	if len(rejected) > 0 {
		rep.RejectedAddrs = rejected
	}
	// Пределы волны заполняются до любого раннего возврата: отчёт обязан
	// называть их и на полностью отброшенной выборке, где ни одна проба не
	// состоялась. Именно там «живых 0 из 0» без пределов читалось как пустая
	// очередь.
	applied := p.Cfg.withDefaults()
	rep.Concurrency = applied.Concurrency
	rep.TimeoutMS = applied.Timeout.Milliseconds()
	rep.DelayMS = applied.Delay.Milliseconds()
	if len(clean) == 0 {
		rep.Elapsed = time.Since(start).Round(time.Millisecond).String()
		return rep
	}
	rep.Total = len(clean)

	cfg := applied
	results := make([]ProbeResult, len(clean))

	// Пул фиксированных воркеров вместо «горутина на адрес под семафором»:
	// только так за каждым воркером закреплена своя цепь tor. При общем
	// клиенте восемь горутин упирались в одну цепь, и волна из 10 адресов
	// шла минуту - ровно столько, сколько один медленный onion-сайт.
	clients := p.isolatedClients(cfg.Concurrency)

	idx := make(chan int)
	var wg sync.WaitGroup
	// Прогресс нужен, потому что волна на 1500 адресов идёт десятки минут, и
	// без промежуточных отметок её не отличить от зависшей. Пишем через
	// атомарный счётчик, а не через мьютекс: воркеров много, и блокировка на
	// каждой пробе стала бы точкой сериализации, от которой мы как раз
	// избавляемся.
	var done int64
	total := int64(len(clean))
	step := total / 10
	if step < 1 {
		step = 1
	}
	for _, cl := range clients {
		wg.Add(1)
		go func(cl *httpc.Client) {
			defer wg.Done()
			for i := range idx {
				results[i] = p.probeOneWith(cl, ctx, clean[i])
				if p.Log != nil {
					n := atomic.AddInt64(&done, 1)
					if n%step == 0 || n == total {
						p.Log.Infof("probe: %d/%d адресов", n, total)
					}
				}
			}
		}(cl)
	}
	for i := range clean {
		if ctx.Err() != nil {
			// Этап 174: отмена - отдельное событие, не значение limit_hit.
			// Прежняя строка «контекст отменён» в limit_hit расходилась с
			// crawl-отчётом (этап 172 разделил эти события там) и с
			// CLI-печатью пробы, которая звала всё это «пределом».
			rep.Cancelled = true
			break
		}
		idx <- i
	}
	close(idx)
	wg.Wait()
	p.closeIsolated(clients)

	rep.Results = make([]ProbeResult, 0, len(clean))
	for _, r := range results {
		if r.URL == "" {
			continue
		}
		rep.Results = append(rep.Results, r)
		switch {
		case r.NoTransport:
			rep.Skipped++
		case r.OK:
			rep.Live++
			rep.LiveHosts = append(rep.LiveHosts, r.URL)
		default:
			rep.Dead++
		}
	}
	sort.Strings(rep.LiveHosts)
	rep.Elapsed = time.Since(start).Round(time.Millisecond).String()
	if p.Log != nil {
		// Служебная строка обязана говорить то же, что отчёт: «живых 0 из 1» в
		// прогоне без tor звучала как вердикт о сервисе, хотя обращения к нему
		// не было вовсе.
		if rep.Skipped > 0 {
			p.Log.Infof("probe: живых %d из %d за %s, пропущено %d (нужен tor или прокси)",
				rep.Live, rep.Total, rep.Elapsed, rep.Skipped)
		} else {
			p.Log.Infof("probe: живых %d из %d за %s", rep.Live, rep.Total, rep.Elapsed)
		}
	}
	return rep
}

func (p *Prober) probeOne(ctx context.Context, host string) ProbeResult {
	return p.probeOneWith(p.Client, ctx, host)
}

// probeOneWith пробует адрес указанным клиентом. Клиент вынесен в параметр,
// чтобы каждый воркер ходил своей цепью tor: с одним общим клиентом весь
// параллелизм фиктивен, воркеры стоят в очередь к единственной цепи.
func (p *Prober) probeOneWith(cl *httpc.Client, ctx context.Context, host string) ProbeResult {
	res := ProbeResult{URL: host}

	if cl == nil {
		res.Error = "клиент не задан"
		return res
	}
	if err := p.Limiter.Wait(ctx, host); err != nil {
		res.Error = "rate limit: " + err.Error()
		return res
	}

	pctx, cancel := context.WithTimeout(ctx, p.Cfg.Timeout)
	defer cancel()

	began := time.Now()
	resp, err := cl.Fetch(pctx, httpc.Request{
		URL:    "http://" + host + "/",
		Method: http.MethodGet,
	})
	res.LatencyMS = time.Since(began).Milliseconds()

	if err != nil {
		res.Error = trimErr(err)
		if errors.Is(err, httpc.ErrOnionWithoutTor) {
			// Вердикта нет: сервис не спрашивали. Запись в пул исказила бы
			// накопительную статистику живости, которую невозможно отличить от
			// настоящей смерти адреса.
			res.NoTransport = true
			return res
		}
		res.PoolStatus = p.record(ctx, host, false, res.LatencyMS)
		return res
	}
	res.Status = resp.Status
	res.OK = resp.Status > 0 && resp.Status < 500
	if !res.OK {
		res.Error = "HTTP " + http.StatusText(resp.Status)
	}
	res.PoolStatus = p.record(ctx, host, res.OK, res.LatencyMS)
	return res
}

// record пишет результат пробы в пул и возвращает итоговый статус адреса.
//
// Статус берётся из той же атомарной записи через store.RecordProbeStatus, а не
// повторным чтением строки: при ProbeConcurrency=16 и одном соединении к базе
// лишнее чтение встало бы в очередь, а главное - могло вернуть состояние,
// изменённое чужой конкурентной пробой между записью и чтением.
//
// Пустая строка означает, что записи не было (нет базы) или запись не удалась.
// Ошибка записи не делает пробу недействительной: исход обращения уже известен,
// а потерянная статистика хуже, чем прерванный прогон. Поэтому ошибка логируется,
// а не возвращается вызывающему.
func (p *Prober) record(ctx context.Context, host string, ok bool, latency int64) string {
	if p.Store == nil {
		return ""
	}
	status, err := p.Store.RecordProbeStatus(ctx, host, ok, latency)
	if err != nil {
		if p.Log != nil {
			p.Log.Infof("probe/%s: запись результата: %v", host, err)
		}
		return ""
	}
	return status
}

// ProbeWave берёт из пула очередь охвата (NextProbeWave) и прогоняет волну.
// Это основной режим прогрева: poolcheck показывает статистику, а здесь пул
// наполняется живыми записями - и освежается, когда непроверенный хвост
// иссякает.
//
// Этап 176: прежняя выборка (NextUnprobed, WHERE status='unknown') не
// трогала проверенные записи вовсе. Пока в пуле есть unknown - на живой
// базе это тысячи адресов, то есть месяцы волн - волна никогда не доходила
// до live и dead: мёртвый сервис оставался live навечно (порог смерти
// требует проб), латентность застывала на первой записи, а записи с
// легаси-тысячей из до-175 эпохи не перекрашивались ничем, кроме повторного
// discover-обхода. Живой BEFORE-факт на витрине (5 live 2024-года, 2 dead,
// 3 unknown): probe_pool limit=5 вернул total=3 - потолок не заполнен,
// семь записей пула волна не видит.
//
// Отбор идёт не через Known: тот сортирует по живости, у непроверенных
// адресов счёт одинаково нулевой, и ничья разрешалась по алфавиту. Пробы
// каждый раз брали один и тот же начало списка, а хвост пула в тысячи
// адресов не проверялся никогда - хотя живые есть и там.
//
// WithWaveLimits возвращает пробера с пределами волны конкретного вызова.
//
// Этап 178 (смоук-H): описание probe_pool обещало «timeout_ms и delay_ms -
// применённые пределы волны», а в схеме инструмента этих параметров не было
// вовсе: переданные 2000/100 молча глотались, волна шла на серверных
// 20000/2000, и смоук видел latency ~20083 при timeout_ms=2000. Пределы
// волны обязаны быть не только читаемыми, но и применимыми. Возвращается
// копия пробера: конфиг и лимитер - свои, клиент, база и лог - общие.
// Новый лимитер не наследует прогрева хостов родителя: задержка волны
// вызова отсчитывается с нуля, что и обещает «применённый предел волны».
// Незаданные (нулевые или отрицательные) пределы остаются серверными.
func (p *Prober) WithWaveLimits(timeoutMS, delayMS, concurrency int64) *Prober {
	cfg := p.Cfg
	if timeoutMS > 0 {
		cfg.Timeout = time.Duration(timeoutMS) * time.Millisecond
	}
	if delayMS > 0 {
		cfg.Delay = time.Duration(delayMS) * time.Millisecond
	}
	if concurrency > 0 {
		cfg.Concurrency = int(concurrency)
	}
	if cfg == p.Cfg {
		return p
	}
	cp := *p
	cp.Cfg = cfg
	cp.Limiter = NewRateLimiter(cfg.Delay)
	return &cp
}

// ProbeWave строит волну из очереди охвата и пробует её. Очередь сама
// решает, кто попадает в волну.
func (p *Prober) ProbeWave(ctx context.Context, limit int) (ProbeReport, error) {
	var rep ProbeReport
	if p.Store == nil {
		return rep, errNoStore
	}
	addrs, err := p.Store.NextProbeWave(ctx, limit)
	if err != nil {
		return rep, err
	}
	rep.RequestedLimit = limit
	if len(addrs) == 0 {
		// Этап 178 (смоук-K): пустая волна держит тот же контракт полей, что и
		// отброшенная выборка в Probe: применённые пределы и elapsed. До
		// правки «живых 0 из 0» приходило с concurrency=0, timeout_ms=0,
		// delay_ms=0 и elapsed="" - ответ выглядел незаполненным, хотя очередь
		// охвата честно пуста. Пределы берутся с копии пробера вызова
		// (WithWaveLimits уже применён вызывающим).
		applied := p.Cfg.withDefaults()
		rep.Concurrency = applied.Concurrency
		rep.TimeoutMS = applied.Timeout.Milliseconds()
		rep.DelayMS = applied.Delay.Milliseconds()
		rep.Elapsed = "0s"
		return rep, nil
	}
	wave := p.Probe(ctx, addrs)
	wave.RequestedLimit = limit
	return wave, nil
}
