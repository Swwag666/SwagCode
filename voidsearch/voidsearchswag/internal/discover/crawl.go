package discover

import (
	"context"
	"errors"
	"html"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"voidsearchswag/internal/filex"
	"voidsearchswag/internal/httpc"
)

type CrawlConfig struct {
	Depth        int
	MaxHosts     int
	PerHostDelay time.Duration
	PageTimeout  time.Duration
	Concurrency  int
}

func (c CrawlConfig) withDefaults() CrawlConfig {
	if c.Depth <= 0 {
		c.Depth = 2
	}
	if c.MaxHosts <= 0 {
		c.MaxHosts = 50
	}
	if c.PerHostDelay <= 0 {
		c.PerHostDelay = 2 * time.Second
	}
	if c.PageTimeout <= 0 {
		c.PageTimeout = 45 * time.Second
	}
	if c.Concurrency <= 0 {
		c.Concurrency = 4
	}
	return c
}

type Page struct {
	Host   string `json:"host"`
	URL    string `json:"url"`
	Status int    `json:"status"`
	Bytes  int    `json:"bytes"`
	Title  string `json:"title,omitempty"`
	// file_refs - файловые ССЫЛКИ, найденные на странице, а не файлы,
	// записанные в каталог: это разведка, а не доставка. До этапа 168
	// поле называлось files, и под-отчёт crawl противоречил верхнему
	// ответу discover: «files: 6» внутри при files=0 снаружи читалось
	// как потеря доставки, хотя запись и не должна была происходить
	// (повторный обход, всё уже в базе под прежней меткой).
	Files     []filex.Ref `json:"file_refs,omitempty"`
	Addresses int         `json:"addresses"`
	Depth     int         `json:"depth"`
	Error     string      `json:"error,omitempty"`
	// LatencyMS - фактическое время GET страницы. Этап 175: до этого
	// recordCrawl записывал живость по обходу с заглушкой ровно 1000ms,
	// потому что поле негде было взять - и вся обходная статистика пула
	// сходилась к «тысяче» (SQL по живой базе: у топовых live-записей
	// latency_avg=1000 у всех подряд). Замер несёт и неуспешные страницы:
	// время до отказа - тоже факт о сервисе, ноль остаётся только для
	// отменённых до запроса.
	LatencyMS int64 `json:"latency_ms,omitempty"`
	// NoTransport отделяет «запрос не мог уйти» от «сервис не ответил». Без tor
	// и без прокси onion-барьер httpc возвращает ErrOnionWithoutTor до обращения
	// к сети: это состояние окружения автора, а не факт о сервисе. Поле нужно
	// потому, что Error к этому моменту уже строка, и тип ошибки в ней не
	// прочитать: запись провала по одной строке уравнивала отсутствующий tor,
	// таймаут и настоящий отказ сервиса, и живой адрес пула становился мёртвым
	// за три обхода на машине без tor.
	NoTransport bool `json:"no_transport,omitempty"`
	// Cut (этап 178): страница срезана отменой контекста вызова ДО своего
	// предела - запрос не дожил до PageTimeout, потому что бюджет discover
	// кончился. Это не отказ сервиса, и живость за срез не пишется: до этапа
	// 178 такие страницы писали провал в пул (recordCrawl по Error!="") и
	// fail_streak рос за чужой счёт - смоук этапа 178 увидел отменённые
	// страницы, поднимавшие счётчик неудач у хостов, которых даже не спросили
	// до конца.
	Cut bool `json:"cut,omitempty"`
}

type CrawlReport struct {
	Pages int `json:"pages"`
	Ok    int `json:"ok"`
	// Failed считает страницы, которые не удалось получить, хотя запрос мог
	// уйти: таймаут, отказ соединения, HTTP 4xx/5xx.
	Failed int `json:"failed"`
	// NoTransport считает страницы, запрос которых не ушёл в сеть вовсе: нет
	// ни tor, ни прокси. В Failed они не входят намеренно - иначе строка
	// «ошибок N» утверждала бы, что сервисы отказали, хотя их никто не спросил.
	NoTransport int `json:"no_transport"`
	// Cut (этап 178) считает страницы, срезанные отменой вызова до их
	// собственного предела: запрос убит бюджетом discover, а не отказался
	// сервисом. В Failed не входит по той же логике, что и NoTransport -
	// вердикта о сервисе нет, живость за срез не пишется.
	Cut   int `json:"cut"`
	Found int `json:"found"`
	// file_refs_found - сколько файловых ССЫЛОК нашёл обход (len(file_refs)),
	// а не сколько файлов записано в каталог: запись - работа discover после
	// возврата, и она честно отражается его полем files. До этапа 168
	// под-отчёт тоже называл это files, и ответ discover противоречил сам
	// себе: «crawl.files: 6» при files=0 (повторный обход: всё уже под
	// прежней меткой) читалось как «нашли и потеряли». Имя file_refs_found
	// снимает противоречие: ссылки найдены, доставка считается отдельно.
	Files   int    `json:"file_refs_found"`
	Depth   int    `json:"depth"`
	Elapsed string `json:"elapsed"`
	// LimitHit называет ТОЛЬКО потолок хостов. Этап 172: раньше то же
	// поле несло и «контекст отменён», и проверка отмены стояла первой
	// на входе в слой - она затирала уже поставленный предел. Живой
	// BEFORE-факт на HEAD 7d82914: max_hosts=1000, timeout=60 - обошли
	// ровно 1000 (потолок достигнут), а limit_hit ответил «контекст
	// отменён», и событие предела исчезло из ответа. Отмена теперь
	// живёт в отдельном поле Cancelled, оба события видны одновременно.
	LimitHit   string            `json:"limit_hit,omitempty"`
	Cancelled  bool              `json:"cancelled,omitempty"`
	NewHosts   []string          `json:"new_hosts,omitempty"`
	Titles     map[string]string `json:"titles,omitempty"`
	FileRefs   []filex.Ref       `json:"file_refs,omitempty"`
	PageDetail []Page            `json:"pages_detail,omitempty"`
	// Применённые потолки. До этих полей отчёт называл только глубину, и
	// оператор не мог отличить «нашли три хоста, потому что больше нет» от
	// «нашли три, потому что потолок три». Значения берутся после withDefaults,
	// то есть это те числа, с которыми шёл обход.
	MaxHosts       int   `json:"max_hosts"`
	PerHostDelayMS int64 `json:"per_host_delay_ms"`
	// SeedsCut - сколько кандидатов не стало сидами из-за потолка. Этап
	// 174: срез входа молчал. Живой BEFORE-факт смоука этапа 172 (агент C,
	// max_hosts=3): пул нёс 14217 кандидатов, seedHosts отдал 3 сидов,
	// обход честно обошёл эти 3, очередь ссылок исчерпалась - и ни одно
	// поле не назвало 14214 отрезанных адресов, а «почему обошли 3 из
	// 14217» не имело ответа. Поле заполняет Pool.Run после обхода: сам
	// Crawl о числе несостоявшихся сидов не знает. Это НЕ событие
	// limit_hit: потолок, отрезавший вход, не останавливал обход, и
	// тест-контракт maxhosts («очередь исчерпана - limit_hit молчит»)
	// сохранён.
	SeedsCut int `json:"seeds_cut,omitempty"`
	// FilesMeasured/FilesUnmeasured/MeasureCut вскрывают фазу замера
	// размеров. Этап 177: замер - единственная сетевая фаза файла, и до
	// этих полей ответ никак не называл её итог: file_refs с size=0 мог
	// значить и «сервер не дал Content-Length», и «окно замера истекло»,
	// и «ссылка не влезла в потолок 200 замеров». FilesMeasured несёт
	// ссылки с известным размером после фазы (мера или каталог прошлых
	// прогонов), FilesUnmeasured - оставшиеся с size=0, MeasureCut - окно
	// срезано (бюджет вызова или собственные 60s). Заполняет Pool.Run
	// после MeasureSizes: сам Crawl о бюджете фазы не знает.
	FilesMeasured   int  `json:"file_refs_measured"`
	FilesUnmeasured int  `json:"file_refs_unmeasured"`
	MeasureCut      bool `json:"measure_cutoff,omitempty"`
}

// RateLimiter держит паузу между запросами к одному хосту. Без этого
// рекурсивный обход забивает единственный onion-сервис запросами и
// получает бан вместо данных.
type RateLimiter struct {
	mu    sync.Mutex
	next  map[string]time.Time
	delay time.Duration
}

func NewRateLimiter(delay time.Duration) *RateLimiter {
	if delay <= 0 {
		delay = 2 * time.Second
	}
	return &RateLimiter{next: map[string]time.Time{}, delay: delay}
}

func (r *RateLimiter) Delay() time.Duration { return r.delay }

// Wait блокирует до момента, когда хосту снова можно слать запрос.
// Уважает отмену контекста и не спит после неё.
func (r *RateLimiter) Wait(ctx context.Context, host string) error {
	r.mu.Lock()
	now := time.Now()
	at, ok := r.next[host]
	if !ok || at.Before(now) {
		at = now
	}
	r.next[host] = at.Add(r.delay)
	r.mu.Unlock()

	wait := time.Until(at)
	if wait <= 0 {
		return nil
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Fetcher - то, что нужно обходу от HTTP-клиента. Интерфейс вместо
// конкретного типа позволяет прогонять обход на подставном клиенте: обход
// валидирует хосты как onion, поэтому httptest-сервер здесь не подходит.
type Fetcher interface {
	Fetch(ctx context.Context, r httpc.Request) (*httpc.Response, error)
}

type Crawler struct {
	Client  Fetcher
	Limiter *RateLimiter
	Log     Logger
	Cfg     CrawlConfig
}

func NewCrawler(client Fetcher, log Logger, cfg CrawlConfig) *Crawler {
	cfg = cfg.withDefaults()
	return &Crawler{
		Client:  client,
		Limiter: NewRateLimiter(cfg.PerHostDelay),
		Log:     log,
		Cfg:     cfg,
	}
}

// WithDepth возвращает обходчика с иной глубиной, не трогая исходный.
// Глубина - единственное поле, которое вызывающий обязан менять на лету:
// discover по MCP принимает depth на каждый вызов, а обходчик один на
// процесс и собирается из серверного конфига. Живой BEFORE этапа 170
// (стенд vss170): depth=1 в запросе давал crawl.depth=2 в ответе -
// Options.Depth молча игнорировался, и обход всегда шёл на серверную
// глубину. Неположительная глубина возвращает исходного обходчика без
// копии: это договорённость с CLI, где флаг 0 значит «глубина из
// конфига». Клиент, лимитер и лог переиспользуются по указателю: пауза
// между хостами остаётся серверной, копируется только число слоёв.
func (c *Crawler) WithDepth(depth int) *Crawler {
	if depth <= 0 || depth == c.Cfg.Depth {
		return c
	}
	cfg := c.Cfg
	cfg.Depth = depth
	return &Crawler{
		Client:  c.Client,
		Limiter: c.Limiter,
		Log:     c.Log,
		Cfg:     cfg,
	}
}

// WithMaxHosts возвращает обходчика с иным потолком хостов, не трогая
// исходного. Живой BEFORE этапа 171 (стенд vss171, чистая база):
// discover с max_hosts=15 отвечал crawl.max_hosts=50 и обходит 50
// страниц (15 сидов + 35 ссылок) - Options.MaxHosts резал только срез
// сидов, а сам обход жил на серверной настройке 50, и «достигнут
// потолок хостов» срабатывал по конфигу, не по запросу. Все три слепых
// смоук-агента этапа 170 поймали это независимо. Неположительный
// потолок возвращает исходного обходчика без копии: это договорённость
// с CLI, где флаг 0 значит «потолок из конфига». Клиент, лимитер и лог
// переиспользуются по указателю: пауза между хостами остаётся
// серверной, копируется только число.
func (c *Crawler) WithMaxHosts(maxHosts int) *Crawler {
	if maxHosts <= 0 || maxHosts == c.Cfg.MaxHosts {
		return c
	}
	cfg := c.Cfg
	cfg.MaxHosts = maxHosts
	return &Crawler{
		Client:  c.Client,
		Limiter: c.Limiter,
		Log:     c.Log,
		Cfg:     cfg,
	}
}

type crawlJob struct {
	addr  string
	depth int
}

// Crawl обходит onion-адреса в ширину до заданной глубины, собирая все
// встреченные onion-ссылки. Каждый хост обходится один раз: повторный
// заход на тот же сервис бессмысленен и заметен для него. Порядок
// результата детерминирован — слои обходятся последовательно, внутри
// слоя страницы и очередь сортируются.
func (c *Crawler) Crawl(ctx context.Context, seeds []string) ([]Candidate, CrawlReport) {
	start := time.Now()
	cfg := c.Cfg.withDefaults()
	var rep CrawlReport
	rep.Depth = cfg.Depth
	rep.MaxHosts = cfg.MaxHosts
	rep.PerHostDelayMS = cfg.PerHostDelay.Milliseconds()

	visited := map[string]bool{}
	discovered := map[string]Candidate{}
	meta := map[string]string{}
	var mu sync.Mutex

	queue := make([]crawlJob, 0, len(seeds))
	for _, s := range seeds {
		h := HostOf(s)
		if h == "" || visited[h] {
			continue
		}
		visited[h] = true
		queue = append(queue, crawlJob{addr: h, depth: 0})
	}

	hostsDone := 0

	for layer := 0; layer <= cfg.Depth && len(queue) > 0; layer++ {
		// Этап 172: отмена контекста - отдельное событие, а не значение
		// limit_hit. Проверка стоит первой на входе в слой, поэтому до
		// этапа 172 она ЗАТИРАЛА уже поставленный срезом предел: обход
		// успевал упереться в потолок, а отчёт терял это событие.
		if ctx.Err() != nil {
			rep.Cancelled = true
			break
		}

		batch := queue
		if rest := cfg.MaxHosts - hostsDone; rest < len(batch) {
			if rest <= 0 {
				rep.LimitHit = "достигнут потолок хостов"
				break
			}
			batch = batch[:rest]
			rep.LimitHit = "достигнут потолок хостов"
		}
		queue = nil

		pages := make([]Page, 0, len(batch))
		var pagesMu sync.Mutex
		var nextMu sync.Mutex
		var next []crawlJob

		sem := make(chan struct{}, cfg.Concurrency)
		var wg sync.WaitGroup

		for _, j := range batch {
			wg.Add(1)
			sem <- struct{}{}
			go func(j crawlJob) {
				defer wg.Done()
				defer func() { <-sem }()

				page, addrs := c.crawlPage(ctx, j.addr, j.depth)

				pagesMu.Lock()
				pages = append(pages, page)
				pagesMu.Unlock()

				mu.Lock()
				for _, a := range addrs {
					if _, ok := discovered[a]; !ok {
						discovered[a] = Candidate{
							Address: a,
							Source:  "crawl",
							Via:     "tor",
							Depth:   j.depth + 1,
						}
					}
				}
				if page.Title != "" && page.Error == "" {
					meta[page.Host] = page.Title
				}
				mu.Unlock()

				if j.depth < cfg.Depth {
					nextMu.Lock()
					for _, a := range addrs {
						if !visited[a] {
							visited[a] = true
							next = append(next, crawlJob{addr: a, depth: j.depth + 1})
						}
					}
					nextMu.Unlock()
				}
			}(j)
		}
		wg.Wait()

		hostsDone += len(batch)
		sort.Slice(pages, func(a, b int) bool { return pages[a].Host < pages[b].Host })
		for _, p := range pages {
			rep.Pages++
			switch {
			case p.NoTransport:
				rep.NoTransport++
			case p.Cut:
				rep.Cut++
			case p.Error != "":
				rep.Failed++
			default:
				rep.Ok++
			}
		}
		if c.Log != nil {
			c.Log.Infof("crawl: слой %d, страниц %d, хостов в пуле %d", layer, len(pages), len(discovered))
		}
		rep.PageDetail = append(rep.PageDetail, pages...)
		for _, p := range pages {
			for _, f := range p.Files {
				rep.FileRefs = append(rep.FileRefs, f)
			}
		}

		sort.Slice(next, func(a, b int) bool { return next[a].addr < next[b].addr })
		queue = next
	}

	// Этап 172: развязка «ровно потолок + отмена». Срез batch ставит
	// limit_hit, только когда очередь ШИРЕ потолка на входе слоя. Если
	// последний слой вместился целиком (hostsDone == потолок) и отмена
	// пришла раньше следующего слоя, событие предела не ставилось вовсе,
	// хотя очередь непуста и хостов обошли ровно столько, сколько можно.
	// Живой BEFORE-факт: max_hosts=50, timeout=30 - crawled=50 из 50,
	// limit_hit «контекст отменён», ссылки в очереди не тронуты.
	if rep.Cancelled && rep.LimitHit == "" && len(queue) > 0 && hostsDone >= cfg.MaxHosts {
		rep.LimitHit = "достигнут потолок хостов"
	}

	// Этап 174: отмена, встретившая слой, за которым очередь пуста.
	// Единственный путь поставить Cancelled раньше был входом в следующий
	// слой, а цикл умеет заканчиваться и по пустой очереди. Живой
	// BEFORE-факт смоука этапа 172 (агент B, timeout=5): crawl.elapsed
	// 4.32s, 4 страницы несут «отменено» - отмена реально случилась внутри
	// обхода, - очередь ссылок пуста, цикл вышел сам, и crawl.cancelled
	// исчез из ответа: единственный обрезанный вызов серии потерял
	// единственное событие стопа. Проверка после цикла: контекст мёртв на
	// выходе из Crawl значит отмену, случившуюся ДО завершения обхода
	// (отмена записи приходит позже возврата и сюда не попадает), поэтому
	// флаг ставится безусловно, а не по текстам ошибок страниц, которые
	// лимитер маскирует под «rate limit: ...».
	if !rep.Cancelled && ctx.Err() != nil {
		rep.Cancelled = true
	}

	rep.Found = len(discovered)
	rep.NewHosts = make([]string, 0, len(discovered))
	for h := range discovered {
		rep.NewHosts = append(rep.NewHosts, h)
	}
	sort.Strings(rep.NewHosts)
	rep.Titles = meta
	rep.FileRefs = dedupeFileRefs(rep.FileRefs)
	sort.Slice(rep.FileRefs, func(i, j int) bool { return rep.FileRefs[i].URL < rep.FileRefs[j].URL })
	rep.Files = len(rep.FileRefs)
	rep.Elapsed = time.Since(start).Round(time.Millisecond).String()
	return Merge(candidatesFrom(discovered)), rep
}

func (c *Crawler) crawlPage(ctx context.Context, host string, depth int) (Page, []string) {
	p := Page{Host: host, URL: "http://" + host + "/", Depth: depth}

	if c.Client == nil {
		p.Error = "клиент не задан"
		return p, nil
	}
	if err := ctx.Err(); err != nil {
		p.Error = "отменено"
		// Этап 178: запрос не начинался - вердикта о сервисе нет, и живость
		// за это не пишется (раньше такие страницы писали провал).
		p.Cut = true
		return p, nil
	}
	if err := c.Limiter.Wait(ctx, host); err != nil {
		p.Error = "rate limit: " + err.Error()
		// Этап 178: Wait падает только по отмене контекста - запрос не ушёл.
		if ctx.Err() != nil {
			p.Cut = true
		}
		return p, nil
	}

	pctx, cancel := context.WithTimeout(ctx, c.Cfg.PageTimeout)
	defer cancel()

	// Этап 175: замер обязан обнимать сам Fetch, а не весь crawlPage:
	// Limiter.Wait съедает до PerHostDelay, и замер с паузой утверждал бы
	// латентность сервиса там, где была очередь вежливости.
	began := time.Now()
	resp, err := c.Client.Fetch(pctx, httpc.Request{URL: p.URL, Method: http.MethodGet})
	p.LatencyMS = time.Since(began).Milliseconds()
	if err != nil {
		p.Error = trimErr(err)
		// Признак ставится здесь, а не по тексту ошибки: строка может быть
		// обрезана trimErr, а тип ошибки после обрезки не восстановить.
		p.NoTransport = errors.Is(err, httpc.ErrOnionWithoutTor)
		// Этап 178: родительский контекст мёртв - запрос убит бюджетом
		// вызова, а не страницным пределом (pctx): это срез, живость за него
		// не пишется. Своё PageTimeout страница честно отрабатывает при
		// живом ctx: тогда Error несёт настоящий отказ.
		if ctx.Err() != nil && !p.NoTransport {
			p.Cut = true
		}
		return p, nil
	}
	p.Status = resp.Status
	p.Bytes = len(resp.Body)
	if resp.Status >= 400 {
		p.Error = "HTTP " + http.StatusText(resp.Status)
		return p, nil
	}

	body := string(resp.Body)
	p.Title = pageTitle(body)
	addrs := Extract(body)
	p.Addresses = len(addrs)

	// Файловые ссылки ищутся тем же проходом, что уже скачал страницу:
	// отдельный обход ради каталога означал бы второй заход по тем же
	// адресам и удвоенную нагрузку на сервис.
	//
	// Здесь принимаются только ссылки с известным файловым расширением.
	// Проверка выдач без расширения (через Content-Disposition) требует
	// отдельного запроса на каждую ссылку и делается сборщиком каталога,
	// у которого для этого есть клиент и пауза на хост. Иначе в каталог
	// попадают картинки разметки: путь uploads/ намекает на скачивание, но
	// расширение при этом картиночное.
	for _, l := range filex.ExtractLinks(body, p.URL) {
		abs := filex.Absolutise(l, p.URL)
		if abs == "" {
			continue
		}
		u, err := url.Parse(abs)
		if err != nil || u.Host != p.Host {
			continue
		}
		ref, ok := filex.FromURL(abs, p.URL, p.Host)
		if !ok {
			continue
		}
		p.Files = append(p.Files, ref)
	}
	p.Files = dedupeFileRefs(p.Files)
	sort.Slice(p.Files, func(i, j int) bool { return p.Files[i].URL < p.Files[j].URL })
	return p, addrs
}

func dedupeFileRefs(refs []filex.Ref) []filex.Ref {
	seen := map[string]bool{}
	out := refs[:0]
	for _, r := range refs {
		if seen[r.URL] {
			continue
		}
		seen[r.URL] = true
		out = append(out, r)
	}
	return out
}

// MeasureSizes уточняет размеры найденных файлов по заголовкам. Список
// обрезается до limit: замер - это отдельный запрос на файл, и на большом
// прогоне он может занять больше времени, чем сам обход. Запросы идут
// параллельно с паузой на хост, тело не читается.
func (c *Crawler) MeasureSizes(ctx context.Context, refs []filex.Ref, limit int) []filex.Ref {
	if c.Client == nil || len(refs) == 0 {
		return refs
	}
	if limit <= 0 {
		limit = defaultSizeProbes
	}
	if len(refs) > limit {
		refs = refs[:limit]
	}

	cfg := c.Cfg.withDefaults()
	sem := make(chan struct{}, cfg.Concurrency)
	var wg sync.WaitGroup
	for i := range refs {
		if refs[i].Size > 0 {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			if c.Limiter != nil {
				if err := c.Limiter.Wait(ctx, refs[i].Host); err != nil {
					return
				}
			}
			if size, ok := c.sizeOf(ctx, refs[i].URL, cfg.PageTimeout); ok {
				refs[i].Size = size
			}
		}(i)
	}
	wg.Wait()
	return refs
}

// defaultSizeProbes ограничивает число замеров за прогон: каталог получает
// размеры самых релевантных файлов, а не тысяч картинок.
const defaultSizeProbes = 200

func (c *Crawler) sizeOf(ctx context.Context, fileURL string, timeout time.Duration) (int64, bool) {
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	resp, err := c.Client.Fetch(pctx, httpc.Request{URL: fileURL, Method: http.MethodHead})
	if err != nil || resp.Status >= 400 {
		return 0, false
	}
	return headerSize(resp.Header)
}

func headerSize(h http.Header) (int64, bool) {
	if h == nil {
		return 0, false
	}
	v := strings.TrimSpace(h.Get("Content-Length"))
	if v == "" {
		return 0, false
	}
	var n int64
	for _, ch := range v {
		if ch < '0' || ch > '9' {
			return 0, false
		}
		n = n*10 + int64(ch-'0')
		if n > 1<<40 {
			return 0, false
		}
	}
	return n, true
}

func pageTitle(body string) string {
	lower := strings.ToLower(body)
	i := strings.Index(lower, "<title>")
	if i < 0 {
		return ""
	}
	rest := body[i+len("<title>"):]
	if j := strings.Index(strings.ToLower(rest), "</title>"); j >= 0 {
		rest = rest[:j]
	}
	// Этап 178: HTML-сущности декодируются до записи: сырой &amp; в title
	// выглядел двусмысленной записью о сервисе, хотя это просто экранирование
	// разметки. UnescapeString после схлопывания пробелов - порядок не важен,
	// сущности не содержат их самих.
	t := html.UnescapeString(strings.Join(strings.Fields(rest), " "))
	r := []rune(t)
	if len(r) > 120 {
		return string(r[:120])
	}
	return t
}

func candidatesFrom(m map[string]Candidate) []Candidate {
	out := make([]Candidate, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}
