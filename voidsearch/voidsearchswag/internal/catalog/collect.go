package catalog

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"voidsearchswag/internal/discover"
	"voidsearchswag/internal/filex"
	"voidsearchswag/internal/httpc"
	"voidsearchswag/internal/store"
)

// Logger - минимальный интерфейс логирования. Пакет не заводит
// собственных зависимостей ради одного метода.
type Logger interface {
	Infof(format string, args ...any)
}

// Config настраивает сбор. Замеры по умолчанию рассчитаны на onion:
// HEAD к скрытому сервису часто не поддерживается, поэтому проверка идёт
// GET-ом с разбором только заголовков.
type Config struct {
	MaxHosts     int
	MaxFiles     int
	Depth        int
	PageLimit    int
	PerHostDelay time.Duration
	PageTimeout  time.Duration
	Concurrency  int
	MaxFileSize  int64
}

func (c Config) withDefaults() Config {
	if c.MaxHosts <= 0 {
		c.MaxHosts = 20
	}
	if c.MaxFiles <= 0 {
		c.MaxFiles = 500
	}
	if c.Depth <= 0 {
		c.Depth = 2
	}
	if c.PageLimit <= 0 {
		c.PageLimit = 12
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
	if c.MaxFileSize <= 0 {
		c.MaxFileSize = 256 << 20
	}
	return c
}

// Report - итог одного сбора.
//
// Saved - сколько ссылок записано в каталог. Skipped - сколько найдено, но не
// записано из-за ошибки базы. Failed - сколько хостов не ответили. Числа обязаны
// быть согласованы с текстовым отчётом команды collect: расхождение между ними
// означало, что текст показывает одну картину, а JSON - другую.
type Report struct {
	Hosts      int            `json:"hosts"`
	Pages      int            `json:"pages"`
	Links      int            `json:"links"`
	Saved      int            `json:"saved"`
	Skipped    int            `json:"skipped"`
	Failed     int            `json:"failed"`
	ByExt      map[string]int `json:"by_ext,omitempty"`
	Elapsed    string         `json:"elapsed"`
	LimitHit   string         `json:"limit_hit,omitempty"`
	SampleURLs []string       `json:"sample_urls,omitempty"`
	// Note объясняет, почему обхода не было: обходить нечего (пустой пул или
	// поисковики не дали файловых хостов). В состоявшемся прогоне поле пусто и
	// потому не появляется в ответе.
	Note string `json:"note,omitempty"`
	// Применённые потолки. Без них отчёт показывал только итог, и оператор не мог
	// отличить «нашли семь файлов, потому что больше нет» от «нашли семь, потому
	// что потолок семь»: оба выглядят как saved=7. Значения берутся уже после
	// withDefaults, то есть это те числа, с которыми шёл обход, включая подмену
	// неположительных.
	MaxFiles       int   `json:"max_files"`
	Concurrency    int   `json:"concurrency"`
	PerHostDelayMS int64 `json:"per_host_delay_ms"`
	// Revisited - файлы, которые задача увидела, но каталог уже знал их до
	// неё. Происхождение (task_id первой находки) не перезаписывается, и
	// без отдельного счётчика задача отчитывалась «сохранено 2», а её
	// task_id потом не находил ничего: файлы принадлежали прежнему прогону.
	// Жалоба смоук-агента этапа 165: collect_files saved=2,
	// file_search task_id -> 0 записей.
	Revisited int `json:"revisited"`
	// FailedHosts - причина отказа для каждого хоста из Failed. Без карты
	// смоук-этап 181 видел failed=1 за 31.6с и не мог отличить битый
	// адрес (отказ за миллисекунды) от молчащего хоста (PageTimeout):
	// elapsed - косвенный признак, а причина была только в служебном
	// логе, скрытом от клиента. Пустая карта не сериализуется.
	FailedHosts map[string]string `json:"failed_hosts,omitempty"`
}

// Fetcher - то, что нужно сборщику от HTTP-клиента. Интерфейс вместо
// конкретного типа позволяет гонять сбор на httptest-сервере без tor и
// без сетевой зависимости в тестах.
type Fetcher interface {
	Fetch(ctx context.Context, r httpc.Request) (*httpc.Response, error)
}

// Collector обходит хосты, вытаскивает ссылки на файлы и пишет их в
// каталог. Обход идёт по тем же правилам, что и разведка: пауза на хост,
// потолок хостов, детерминированный порядок.
type Collector struct {
	Client  Fetcher
	Store   *store.Store
	Limiter *discover.RateLimiter
	Log     Logger
	Cfg     Config
}

func NewCollector(client Fetcher, st *store.Store, limiter *discover.RateLimiter, log Logger, cfg Config) *Collector {
	cfg = cfg.withDefaults()
	if limiter == nil && cfg.PerHostDelay > 0 {
		limiter = discover.NewRateLimiter(cfg.PerHostDelay)
	}
	return &Collector{Client: client, Store: st, Limiter: limiter, Log: log, Cfg: cfg}
}

var errNoStore = fmt.Errorf("store не задан")

// Collect обходит хосты и собирает файлы. taskID связывает находки с
// задачей: по нему потом можно отобрать результаты конкретного прогона.
func (c *Collector) Collect(ctx context.Context, hosts []string, taskID string) (Report, error) {
	start := time.Now()
	var rep Report
	rep.ByExt = map[string]int{}

	if c.Store == nil {
		return rep, errNoStore
	}
	if c.Client == nil {
		return rep, fmt.Errorf("клиент не задан")
	}

	// Этап 174: каждый предел - свой флаг. Прежде обе причины писались в
	// одно и то же поле LimitHit, и потолок файлов, сработавший после
	// потолка хостов, ЗАТИРАЛ его: ответ терял одно из двух состоявшихся
	// событий. Строки собираются в конце, обе причины видны одновременно.
	hostsCeiling := false
	filesCeiling := false
	uniq := normaliseHosts(hosts)
	if len(uniq) == 0 {
		return rep, fmt.Errorf("хосты не заданы")
	}
	if len(uniq) > c.Cfg.MaxHosts {
		hostsCeiling = true
		uniq = uniq[:c.Cfg.MaxHosts]
	}
	rep.Hosts = len(uniq)
	rep.MaxFiles = c.Cfg.MaxFiles
	rep.Concurrency = c.Cfg.Concurrency
	rep.PerHostDelayMS = c.Cfg.PerHostDelay.Milliseconds()

	type hostResult struct {
		page  int
		links []filex.Ref
		err   error
	}
	results := make([]hostResult, len(uniq))

	sem := make(chan struct{}, c.Cfg.Concurrency)
	var wg sync.WaitGroup
	for i, host := range uniq {
		wg.Add(1)
		// Этап 178 (смоук-J): захват слота конкурирует с отменой контекста.
		// Прежний безусловный sem <- struct{}{} запускал хвост очереди и
		// после отмены: каждый хост дорабатывал свой goroutine-старт, чтобы
		// collectHost отклонил его уже отменённым контекстом. Сейчас
		// не стартовавший после отмены хост получает явную причину
		// «обход прерван» сразу в results, не запуская горутину. (Ранний
		// ctx-фильтр collectHost делает обе версии эквивалентными по
		// сети - ни одна проба после отмены не уходит - здесь выигрыш
		// в явности причины и отсутствие пустых стартов хвоста.)
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Done()
			results[i] = hostResult{err: fmt.Errorf("обход прерван: %w", ctx.Err())}
			continue
		}
		go func(i int, host string) {
			defer wg.Done()
			defer func() { <-sem }()
			page, links, err := c.collectHost(ctx, host)
			results[i] = hostResult{page: page, links: links, err: err}
		}(i, host)
	}
	wg.Wait()

	// Этап 163: запись собранного переживает отмену контекста. Обход - это
	// сеть, его честно обрывает таймаут вызова; но запись в базу - локальная
	// и быстрая, и если гасить её тем же отменённым контекстом, минуты
	// работы по tor пропадали в никуда: хосты обойдены, файлы вытащены,
	// AddFile не прошёл и счётчик ушёл в skipped. Отменяемость сохраняется
	// через отдельный запас на запись: зависшая база не держит вызов
	// вечно, а собранное доезжает до каталога.
	saveCtx, saveCancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer saveCancel()

	saved := 0
	skipped := 0
	revisited := 0
	// Причины отказов пишутся той же петлёй, что и Failed: карта и счётчик
	// обязаны сходиться по каждому хосту, иначе ответ снова требует
	// угадывания по elapsed.
	failedHosts := map[string]string{}
	for i, r := range results {
		rep.Pages += r.page
		rep.Links += len(r.links)
		if r.err != nil {
			rep.Failed++
			failedHosts[uniq[i]] = r.err.Error()
			continue
		}
		for _, f := range r.links {
			if saved >= c.Cfg.MaxFiles {
				filesCeiling = true
				break
			}
			// Повторная находка не становится новой записью и не съедает
			// потолок: задача видела файл, но каталог уже знал его с
			// прежним происхождением. Отдельный счётчик вместо saved
			// объясняет агенту, почему file_search по его task_id меньше,
			// чем найденного.
			known, kErr := c.Store.FileKnown(saveCtx, f.URL)
			if kErr != nil {
				c.logf("catalog: проверка %s: %v", f.URL, kErr)
			}
			if err := c.Store.AddFile(saveCtx, store.FileEntry{
				TaskID:     taskID,
				URL:        f.URL,
				Filename:   f.Filename,
				Ext:        f.Ext,
				Size:       f.Size,
				MIME:       f.MIME,
				SourcePage: f.SourcePage,
				// Вердикт заполняется при записи. Колонка существовала с
				// начала, но оставалась пустой у всех строк каталога, поэтому
				// фильтр по verdict работал только в тестах, которые вставляли
				// значения сами.
				Verdict: filex.ClassifyRef(f),
			}); err != nil {
				// Ошибка записи считается, а не только логируется: служебный лог
				// по умолчанию скрыт, и без счётчика отчёт показывал «ссылок N,
				// файлов 0», что читается как источник без файлов.
				c.logf("catalog: запись %s: %v", f.URL, err)
				skipped++
				continue
			}
			if known {
				revisited++
				rep.ByExt[f.Ext]++
				continue
			}
			// Этап 179 (смоук-C, Д2): параллельный сосед мог вставить файл
			// между FileKnown и AddFile - наша запись прошла как
			// ON CONFLICT-обновление без вставки, но старый код всё равно
			// считал её в saved. Происхождение после записи различает
			// вставку от обновления: файл наш, только если каталог несёт
			// наш task_id; сосед, выигравший гонку, оставил свой.
			origin, oOk, oErr := c.Store.FileOrigin(saveCtx, f.URL)
			if oErr != nil {
				c.logf("catalog: происхождение %s: %v", f.URL, oErr)
			}
			if oOk && origin != taskID {
				revisited++
				rep.ByExt[f.Ext]++
				continue
			}
			saved++
			rep.ByExt[f.Ext]++
			if len(rep.SampleURLs) < 20 {
				rep.SampleURLs = append(rep.SampleURLs, f.URL)
			}
		}
	}
	rep.Saved = saved
	// Пропуски отдаются в отчёт тем же числом, которое накапливалось при записи:
	// без этого поле оставалось нулевым всегда, хотя JSON его сериализует.
	rep.Skipped = skipped
	rep.Revisited = revisited
	if len(failedHosts) > 0 {
		rep.FailedHosts = failedHosts
	}
	// Этап 174: сборка limit_hit из флагов. Порядок фиксирован - хосты,
	// затем файлы: одна и та же пара событий всегда даёт одну и ту же
	// строку, и клиенту не нужно угадывать, что из перезаписей случилось.
	switch {
	case hostsCeiling && filesCeiling:
		rep.LimitHit = "достигнут потолок хостов; достигнут потолок файлов"
	case hostsCeiling:
		rep.LimitHit = "достигнут потолок хостов"
	case filesCeiling:
		rep.LimitHit = "достигнут потолок файлов"
	}
	rep.Elapsed = time.Since(start).Round(time.Millisecond).String()
	return rep, nil
}

func (c *Collector) collectHost(ctx context.Context, host string) (int, []filex.Ref, error) {
	if err := ctx.Err(); err != nil {
		return 0, nil, err
	}
	if c.Limiter != nil {
		if err := c.Limiter.Wait(ctx, host); err != nil {
			return 0, nil, err
		}
	}

	root := "http://" + host + "/"
	pctx, cancel := context.WithTimeout(ctx, c.Cfg.PageTimeout)
	defer cancel()

	resp, err := c.Client.Fetch(pctx, httpc.Request{URL: root, Method: http.MethodGet})
	if err != nil {
		return 0, nil, err
	}
	if resp.Status >= 400 {
		return 1, nil, fmt.Errorf("%s: HTTP %d", host, resp.Status)
	}

	pages := 1
	refs := c.refsFrom(ctx, string(resp.Body), root, host)

	// Главная почти всегда отдаёт только разметку: скрипты, стили и
	// картинки. Файлы содержимого лежат глубже, поэтому идём по внутренним
	// ссылкам - но только по тем, что похожи на страницы, и не дальше
	// потолка страниц.
	visited := map[string]bool{root: true}
	queue := c.innerPages(string(resp.Body), root, host)

	for depth := 1; depth < c.Cfg.Depth && len(queue) > 0; depth++ {
		layer := queue
		if rest := c.Cfg.PageLimit - pages; rest < len(layer) {
			if rest <= 0 {
				break
			}
			layer = layer[:rest]
		}
		queue = nil

		for _, pageURL := range layer {
			if err := ctx.Err(); err != nil {
				break
			}
			if visited[pageURL] {
				continue
			}
			visited[pageURL] = true

			if c.Limiter != nil {
				if err := c.Limiter.Wait(ctx, host); err != nil {
					break
				}
			}
			ictx, icancel := context.WithTimeout(ctx, c.Cfg.PageTimeout)
			iresp, err := c.Client.Fetch(ictx, httpc.Request{URL: pageURL, Method: http.MethodGet})
			icancel()
			if err != nil || iresp.Status >= 400 {
				continue
			}
			pages++
			ibody := string(iresp.Body)
			refs = append(refs, c.refsFrom(ctx, ibody, pageURL, host)...)
			if depth+1 < c.Cfg.Depth {
				queue = append(queue, c.innerPages(ibody, pageURL, host)...)
			}
		}
	}

	// Порядок файлов задаётся сортировкой, а не разметкой страницы: при
	// потолке файлов обрезка должна быть предсказуемой.
	refs = dedupeRefs(refs)
	sort.Slice(refs, func(i, j int) bool { return refs[i].URL < refs[j].URL })
	return pages, refs, nil
}

// refsFrom классифицирует ссылки одной страницы.
func (c *Collector) refsFrom(ctx context.Context, body, pageURL, host string) []filex.Ref {
	var out []filex.Ref
	for _, l := range filex.ExtractLinks(body, pageURL) {
		if err := ctx.Err(); err != nil {
			break
		}
		ref, ok := c.classify(ctx, l, pageURL, host)
		if !ok {
			continue
		}
		out = append(out, ref)
	}
	return out
}

// dedupeRefs схлопывает файлы, найденные на нескольких страницах: один и
// тот же файл обычно соседствует в навигации и в списке содержимого, и без
// дедупа каталог раздувается копиями.
func dedupeRefs(refs []filex.Ref) []filex.Ref {
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

// innerPages отбирает ссылки, похожие на страницы самого сервиса: без
// файлового расширения, на том же хосте, не якоря. Такие ссылки ведут в
// каталоги файлов и на страницы категорий, где и лежит содержимое.
func (c *Collector) innerPages(body, pageURL, host string) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range filex.ExtractLinks(body, pageURL) {
		abs := filex.Absolutise(l, pageURL)
		if abs == "" {
			continue
		}
		u, err := url.Parse(abs)
		if err != nil {
			continue
		}
		if u.Host != host {
			continue
		}
		if e := filex.ExtOf(path.Base(u.Path)); e != "" {
			continue
		}
		abs = filex.StripQuery(abs)
		if abs == "" || seen[abs] {
			continue
		}
		seen[abs] = true
		out = append(out, abs)
	}
	sort.Strings(out)
	return out
}

// classify решает, является ли ссылка файлом. Файлы с известным
// расширением принимаются по адресу; остальные проверяются запросом, но
// только если путь намекает на скачивание - иначе проверка пойдёт по
// каждой ссылке страницы.
func (c *Collector) classify(ctx context.Context, rawURL, sourcePage, host string) (filex.Ref, bool) {
	abs := filex.Absolutise(rawURL, sourcePage)
	if abs == "" {
		return filex.Ref{}, false
	}
	u, err := url.Parse(abs)
	if err != nil {
		return filex.Ref{}, false
	}
	if u.Host != host && !strings.HasSuffix(u.Host, "."+host) {
		return filex.Ref{}, false
	}

	ref, ok := filex.FromURL(abs, sourcePage, host)
	if !ok {
		if !filex.DownloadHint(u.Path) {
			return filex.Ref{}, false
		}
		return c.probeFile(ctx, abs, sourcePage, host)
	}

	if size, ok := c.headSize(ctx, abs, host); ok {
		if size > c.Cfg.MaxFileSize {
			return filex.Ref{}, false
		}
		ref.Size = size
	}
	return ref, true
}

// probeFile проверяет ссылку запросом и признаёт её файлом, если сервер сам
// сообщает о вложении: так ловятся выдачи вида /download?id=17, где ни
// имени, ни расширения в адресе нет.
func (c *Collector) probeFile(ctx context.Context, fileURL, sourcePage, host string) (filex.Ref, bool) {
	size, hdr, ok := c.probeHeaders(ctx, fileURL, host)
	if !ok {
		return filex.Ref{}, false
	}
	name, attached := filex.AttachmentName(hdr.Get("Content-Disposition"))
	if !attached {
		return filex.Ref{}, false
	}
	ext := filex.ExtOf(name)
	if size > c.Cfg.MaxFileSize {
		return filex.Ref{}, false
	}
	safe := filex.SafeName(name)
	if safe == "" {
		return filex.Ref{}, false
	}
	// Ворота - та же функция, что и в filex.FromURL, а не вторая копия
	// проверок. Расхождение между двумя путями добавления было живым
	// дефектом, а не стилистикой: FromURL отклонял служебные расширения (asc,
	// sig, pem, key, sha256), а probeFile принимал всё, что есть в KnownExts,
	// поэтому подпись, отданная сервером с Content-Disposition, в каталог
	// попадала. CleanFileCatalog её тут же удалял, и одна и та же запись
	// бесконечно добавлялась и вычищалась: счётчик файлов между прогонами
	// показывал то, чего в каталоге быть не должно. В реальной базе это видно -
	// key.asc при том, что IsContent("asc") возвращает false.
	//
	// Сервер, отдающий manifest.json, графику оформления или ответ эндпоинта
	// API с Content-Disposition, тоже не превращает их в содержимое: иначе путь
	// через пробу оставался лазейкой вокруг фильтра.
	//
	// Путь передаётся уже разобранным, чтобы не разбирать адрес второй раз.
	p := ""
	if u, err := url.Parse(fileURL); err == nil {
		p = u.Path
	}
	if filex.CatalogJunkReasonPath(ext, safe, p) != "" {
		return filex.Ref{}, false
	}
	mime := filex.MimeForExt(ext)
	if ct := filex.MimeOf(hdr.Get("Content-Type")); ct != "" && !strings.HasPrefix(ct, "text/html") {
		mime = ct
	}
	return filex.Ref{
		URL:        fileURL,
		Filename:   safe,
		Ext:        ext,
		Size:       size,
		MIME:       mime,
		SourcePage: sourcePage,
		Host:       host,
	}, true
}

// probeHeaders запрашивает заголовки файла. Сначала HEAD, а если сервис его
// не поддерживает - GET: тело не читается, для каталога важен только размер
// и Content-Disposition.
func (c *Collector) probeHeaders(ctx context.Context, fileURL, host string) (int64, http.Header, bool) {
	if c.Limiter != nil {
		if err := c.Limiter.Wait(ctx, host); err != nil {
			return 0, nil, false
		}
	}
	hctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	resp, err := c.Client.Fetch(hctx, httpc.Request{URL: fileURL, Method: http.MethodHead})
	if err == nil && resp.Status < 400 {
		size, _ := contentLength(resp.Header)
		return size, resp.Header, true
	}

	gctx, gcancel := context.WithTimeout(ctx, 20*time.Second)
	defer gcancel()
	resp, err = c.Client.Fetch(gctx, httpc.Request{URL: fileURL, Method: http.MethodGet})
	if err != nil || resp.Status >= 400 {
		return 0, nil, false
	}
	size, _ := contentLength(resp.Header)
	return size, resp.Header, true
}

func (c *Collector) headSize(ctx context.Context, fileURL, host string) (int64, bool) {
	size, _, ok := c.probeHeaders(ctx, fileURL, host)
	if !ok || size == 0 {
		return 0, false
	}
	return size, true
}

func contentLength(h http.Header) (int64, bool) {
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

func (c *Collector) logf(format string, args ...any) {
	if c.Log != nil {
		c.Log.Infof(format, args...)
	}
}

func normaliseHosts(hosts []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(hosts))
	for _, h := range hosts {
		h = strings.TrimSpace(h)
		if h == "" {
			continue
		}
		if strings.Contains(h, "://") {
			if u, err := url.Parse(h); err == nil && u.Host != "" {
				h = u.Host
			}
		}
		h = strings.TrimSuffix(h, "/")
		if h == "" || seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}
