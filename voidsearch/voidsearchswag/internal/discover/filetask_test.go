package discover

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"voidsearchswag/internal/httpc"
	"voidsearchswag/internal/store"
)

// runFilesPool поднимает пул с живой базой, обходчиком на подставном клиенте
// и страницей из двух файловых ссылок. Finder нужен Run: источники выдают
// один адрес, но сиды задаются явно, поэтому на результат он не влияет.
func runFilesPool(t *testing.T, st *store.Store) *Pool {
	t.Helper()
	page := "http://" + cHost + "/"
	body := `<html><body><a href="/files/dump.zip">дамп</a><a href="/files/keys.7z">ключи</a></body></html>`
	cr := NewCrawler(&stubCrawl{pages: map[string]string{page: body}}, silentLog{}, fastCrawl(CrawlConfig{Depth: 1}))
	return &Pool{
		Store:  st,
		Finder: &Finder{Client: &brokenWriteSource{n: 1}, Log: silentLog{}, Attempts: 1},
		Crawl:  cr,
		Log:    silentLog{},
	}
}

func TestRunReportsFileTaskIDForCrawledFiles(t *testing.T) {
	// Замер этапа 167: обход с crawl писал файлы под task_id «crawl-...»,
	// которого в ответе не было, и file_search не мог их найти по метке -
	// юзер видел рост каталога и пустой поиск. Ответ обязан называть метку
	// записанных файлов, и метка обязана совпадать с базой.
	st := newStore(t)
	p := runFilesPool(t, st)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := p.Run(ctx, Options{WithCrawl: true, Seeds: []string{cHost}, MaxHosts: 2})
	if err != nil {
		t.Fatalf("Run вернул ошибку: %v", err)
	}
	if res.FileTaskID == "" {
		t.Fatal("file_task_id не назван при записанных файлах")
	}
	if !strings.HasPrefix(res.FileTaskID, "crawl-") || len(res.FileTaskID) != len("crawl-20060102-150405.000") {
		t.Errorf("file_task_id не похож на метку задачи с миллисекундами: %q", res.FileTaskID)
	}
	if res.Files != 2 {
		t.Fatalf("files = %d, ожидала 2 (метрика та же, что до правки)", res.Files)
	}
	got, err := st.SearchFiles(ctx, store.FileQuery{TaskID: res.FileTaskID, Limit: 100})
	if err != nil {
		t.Fatalf("SearchFiles: %v", err)
	}
	if len(got) != res.Files {
		t.Errorf("по метке из ответа найдено %d записей при files=%d: file_search вновь не видит файлы задачи", len(got), res.Files)
	}
	for _, f := range got {
		if f.TaskID != res.FileTaskID {
			t.Errorf("файл %s записан под %q, а ответ назвал %q", f.URL, f.TaskID, res.FileTaskID)
		}
	}
}

func TestRunRepeatCrawlCountsRevisitedNotSaved(t *testing.T) {
	// Повторный обход тех же ссылок: AddFile держит происхождение первой
	// записи, поэтому вторая задача не увидит эти файлы под своей меткой.
	// Без отдельного счётчика «files: 2» при пустом file_search читалось
	// как «записано 2», и невидимость возвращалась уже с меткой в ответе.
	st := newStore(t)
	p := runFilesPool(t, st)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	first, err := p.Run(ctx, Options{WithCrawl: true, Seeds: []string{cHost}, MaxHosts: 2})
	if err != nil {
		t.Fatalf("первый Run: %v", err)
	}
	if first.Files != 2 || first.Revisited != 0 {
		t.Fatalf("первый прогон: files=%d revisited=%d, ожидала 2/0", first.Files, first.Revisited)
	}

	// Пауза - страховка от грубого системного тика: формат метки с этапа 168
	// несёт миллисекунды, но на Windows часы могут стоять на месте до ~15ms,
	// и два прогона мгновенного теста иногда получают один тик. Проверяется
	// семантика повтора, а не гранулярность часов, поэтому прогоны
	// разделены паузой.
	time.Sleep(1100 * time.Millisecond)

	second, err := p.Run(ctx, Options{WithCrawl: true, Seeds: []string{cHost}, MaxHosts: 2})
	if err != nil {
		t.Fatalf("второй Run: %v", err)
	}
	if second.Files != 0 {
		t.Errorf("files = %d при сплошь известных файлах: происхождение не переписывается, метке нечего показать", second.Files)
	}
	if second.Revisited != 2 {
		t.Errorf("revisited = %d, ожидала 2", second.Revisited)
	}
	if second.FileTaskID == "" {
		t.Error("метка задачи не названа, хотя обход находил файлы")
	}
	if second.FileTaskID == first.FileTaskID {
		t.Errorf("метки прогонов совпали (%q): повторный прогон неотличим от первого", second.FileTaskID)
	}
	got, err := st.SearchFiles(ctx, store.FileQuery{TaskID: second.FileTaskID, Limit: 100})
	if err != nil {
		t.Fatalf("SearchFiles: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("под меткой повтора %d записей: повторные находки обязаны оставаться под прежним происхождением", len(got))
	}
	older, err := st.SearchFiles(ctx, store.FileQuery{TaskID: first.FileTaskID, Limit: 100})
	if err != nil {
		t.Fatalf("SearchFiles: %v", err)
	}
	if len(older) != 2 {
		t.Errorf("под меткой первого прогона %d записей, ожидала 2: повтор не сместил их", len(older))
	}
}

func TestRunWithoutCrawlOmitsFileTaskID(t *testing.T) {
	// Метка без файлов - шум: без обхода поля file_task_id в отчёте
	// быть не должно, и проверка идёт по JSON, а не по полю структуры.
	st := newStore(t)
	p := runFilesPool(t, st)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := p.Run(ctx, Options{WithCrawl: false})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	m := decodeResult(t, res)
	if _, ok := m["file_task_id"]; ok {
		t.Errorf("file_task_id в ответе без обхода: %s", mustJSON(t, m))
	}
}

func TestRunReportsFileTaskIDWhenWritesFail(t *testing.T) {
	// Мёртвая база: файлы не записались, но метка обязана быть названа -
	// иначе счётчик отказов file_save_failed не привязать к задаче, и
	// юзер не может даже сформулировать, что именно потерялось.
	st, err := store.Open(t.TempDir() + "/nofiles.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	p := runFilesPool(t, st)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := p.Run(ctx, Options{WithCrawl: true, Seeds: []string{cHost}, MaxHosts: 2})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.FileTaskID == "" {
		t.Fatal("file_task_id не назван при найденных файлах, пусть и не записанных")
	}
	if res.Files != 0 {
		t.Errorf("files = %d при мёртвой базе", res.Files)
	}
	if res.FileSaveFailed == 0 {
		t.Error("отказы записи файлов не учтены")
	}
}

// cancelingCrawl отменяет контекст прогона в момент первого запроса
// страницы: страница успевает вернуться со файловыми ссылками, а весь
// остальной прогон - уже «после обрезки». Так воспроизводится живой
// сценарий этапа 167: crawl собрал 313..315 ссылок, rctx истёк, и
// запись не должна зависеть от отменённого контекста.
type cancelingCrawl struct {
	inner  *stubCrawl
	cancel context.CancelFunc
	fired  bool
}

func (c *cancelingCrawl) Fetch(ctx context.Context, r httpc.Request) (*httpc.Response, error) {
	if !c.fired {
		c.fired = true
		c.cancel()
	}
	return c.inner.Fetch(ctx, r)
}

func TestRunSavesCrawledFilesAfterContextCancel(t *testing.T) {
	// Живой замер этапа 167 (смоук-агенты A3 и C4, стенд agenthunt):
	// discover с timeout 420..480 собирал 313..315 файловых ссылок, но
	// запись шла под уже отменённым контекстом и обрывалась на первой
	// итерации - files=0, file_task_id светился, file_search по метке
	// возвращал пустоту. Обещание «найденное при crawl пишется в каталог»
	// обязано выполняться и на обрезке: собранное доезжает до базы, как
	// в collect_files (этап 163).
	st := newStore(t)
	page := "http://" + cHost + "/"
	body := `<html><body><a href="/files/dump.zip">дамп</a><a href="/files/keys.7z">ключи</a></body></html>`
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cr := NewCrawler(&cancelingCrawl{inner: &stubCrawl{pages: map[string]string{page: body}}, cancel: cancel}, silentLog{}, fastCrawl(CrawlConfig{Depth: 2}))
	p := &Pool{
		Store:  st,
		Finder: &Finder{Client: &brokenWriteSource{n: 1}, Log: silentLog{}, Attempts: 1},
		Crawl:  cr,
		Log:    silentLog{},
	}

	res, err := p.Run(ctx, Options{WithCrawl: true, Seeds: []string{cHost}, MaxHosts: 2})
	if err != nil {
		t.Fatalf("Run вернул ошибку: %v", err)
	}
	if ctx.Err() == nil {
		t.Fatal("тест не воспроизвёл отмену: контекст жив")
	}
	if res.FileTaskID == "" {
		t.Fatal("file_task_id не назван при собранных файлах")
	}
	if res.Files != 2 {
		t.Fatalf("files = %d после отмены: собранное до обрезки обязано доезжать до базы (FileRefs=%d)", res.Files, len(res.Crawl.FileRefs))
	}
	// Чтение по живому контексту: отменённый ctx прогона не должен
	// мешать юзеру проверить, что доехало.
	got, err := st.SearchFiles(context.Background(), store.FileQuery{TaskID: res.FileTaskID, Limit: 100})
	if err != nil {
		t.Fatalf("SearchFiles: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("по метке из ответа найдено %d записей при files=%d", len(got), res.Files)
	}
}

func TestRunTaskLabelCarriesMilliseconds(t *testing.T) {
	// Секундная гранулярность метки - коллизия: повторный discover в ту же
	// секунду наследует task_id первого, и file_search по метке смешивает
	// задачи. Живой случай этапа 168: повторный прогон стенда after167b
	// получил метку, неотличимую от секундной коллизии. Миллисекунды
	// снимают коллизию; формат остаётся лексикографически сортируемым.
	st := newStore(t)
	p := runFilesPool(t, st)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := p.Run(ctx, Options{WithCrawl: true, Seeds: []string{cHost}, MaxHosts: 2})
	if err != nil {
		t.Fatalf("Run вернул ошибку: %v", err)
	}
	re := regexp.MustCompile(`^crawl-\d{8}-\d{6}\.\d{3}$`)
	if !re.MatchString(res.FileTaskID) {
		t.Fatalf("file_task_id %q не несёт миллисекунд: коллизия секунды жива", res.FileTaskID)
	}
}

func TestRunFilesSurvivePoolBudgetExhaustion(t *testing.T) {
	// Живой замер этапа 168 (стенд after167b): discover нашёл 14385
	// адресов, пул почти весь съел общий бюджет записи, и файловая фаза
	// вышла на его остаток - 200 найденных ссылок при files=0,
	// revisited=0 и нулевом file_save_failed: файлы пропали молча, как до
	// этапа 167, только причиной стал потолок пула, а не отмена rctx.
	// Файловая фаза обязана ехать под собственным бюджетом, который пул
	// съесть не может - иначе «собранное доезжает» держится только пока
	// пул маленький.
	st := newStore(t)
	page := "http://" + cHost + "/"
	body := `<html><head><title>Сборник</title></head><body>` +
		`<a href="/files/dump.zip">дамп</a><a href="/files/keys.7z">ключи</a>` +
		`</body></html>`
	// HEAD отвечает размером, а пауза на хост заметная: второй замер
	// обязан пройти через Limiter.Wait. Если замер поедет под чужим
	// мёртвым контекстом, Wait вернёт ошибку отмены и размер не доедет -
	// по потерянным размерам это и ловится.
	cfg := fastCrawl(CrawlConfig{Depth: 1})
	cfg.PerHostDelay = 40 * time.Millisecond
	cr := NewCrawler(&stubCrawl{
		pages: map[string]string{page: body},
		head:  http.Header{"Content-Length": []string{"12345"}},
	}, silentLog{}, cfg)
	// Мгновенное исчерпание бюджета - отрицательный, а не нано-бюджет
	// (этап 174): тик нано-таймера асинхронен, и батч-запись из синхронного
	// sqlite успевает пройти до первого тика - тест ловил гонку, а не
	// контракт. Отрицательный таймаут помечает saveCtx мёртвым синхронно
	// в момент создания: батч обязан отказаться, фолбэк обязан честно
	// посчитать отказ, файловая фаза обязана ехать под собственным бюджетом.
	p := &Pool{
		Store:      st,
		Finder:     &Finder{Client: &brokenWriteSource{n: 1}, Log: silentLog{}, Attempts: 1},
		Crawl:      cr,
		Log:        silentLog{},
		saveBudget: -time.Nanosecond,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := p.Run(ctx, Options{WithCrawl: true, Seeds: []string{cHost}, MaxHosts: 2})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.New != 0 || res.Updated != 0 {
		t.Fatalf("new=%d updated=%d при мёртвом бюджете пула: пул обязан падать, не писать", res.New, res.Updated)
	}
	if res.SaveFailed == 0 {
		t.Fatalf("save_failed = 0: мёртвый бюджет пула не оставил следов - сценарий не воспроизведён")
	}
	if res.Files != 2 {
		t.Fatalf("files = %d при исчерпанном бюджете пула: файловая фаза обязана ехать под собственным бюджетом (FileRefs=%d)", res.Files, len(res.Crawl.FileRefs))
	}
	for i, f := range res.Crawl.FileRefs {
		if f.Size != 12345 {
			t.Errorf("ref[%d] %s: size=%d, ожидала 12345 - замер потерял размеры под чужим контекстом", i, f.URL, f.Size)
		}
	}
	got, err := st.SearchFiles(context.Background(), store.FileQuery{TaskID: res.FileTaskID, Limit: 100})
	if err != nil {
		t.Fatalf("SearchFiles: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("по метке из ответа найдено %d записей при files=%d", len(got), res.Files)
	}
	for _, f := range got {
		if f.Size != 12345 {
			t.Errorf("файл %s записан с size=%d: замер не доехал до записи", f.URL, f.Size)
		}
	}
}

func TestRunSavesPoolMetaAndLivenessAfterContextCancel(t *testing.T) {
	// Этап 167 закрыл «собранное доезжает» только для файлов. Живой замер
	// C4 (agenthunt): 43 отказа «set onion meta: context deadline exceeded»
	// - это остальная фаза записи, шедшая под rctx обрезки: новые адреса,
	// заголовки Titles и живость обхода. Этот тест - тот же сценарий, но
	// проверяются не файлы, а пул: после отмены всё обязано доехать.
	st := newStore(t)
	// Сид известен пулу заранее - как в живом прогоне: discover с seeds
	// берёт адреса, которые уже лежат в пуле от прошлых прогонов.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := st.UpsertOnion(ctx, store.Onion{URL: cHost, Status: "unknown"}); err != nil {
		t.Fatalf("подготовка сида: %v", err)
	}
	page := "http://" + cHost + "/"
	body := `<html><head><title>Сборник</title></head><body>` +
		`<a href="/files/dump.zip">дамп</a>` +
		`<a href="http://aaaaaaaaaaaaaaaa.onion/">сосед</a>` +
		`</body></html>`
	cr := NewCrawler(&cancelingCrawl{inner: &stubCrawl{pages: map[string]string{page: body}}, cancel: cancel}, silentLog{}, fastCrawl(CrawlConfig{Depth: 1}))
	p := &Pool{
		Store:  st,
		Finder: &Finder{Client: &brokenWriteSource{n: 1}, Log: silentLog{}, Attempts: 1},
		Crawl:  cr,
		Log:    silentLog{},
	}

	res, err := p.Run(ctx, Options{WithCrawl: true, Seeds: []string{cHost}, MaxHosts: 5})
	if err != nil {
		t.Fatalf("Run вернул ошибку: %v", err)
	}
	if ctx.Err() == nil {
		t.Fatal("тест не воспроизвёл отмену: контекст жив")
	}
	if res.Files != 1 {
		t.Fatalf("files = %d после отмены, ожидала 1", res.Files)
	}
	if res.New < 1 {
		t.Errorf("new = %d: сосед со страницы обязан заводиться в пул и после обрезки (found=%d)", res.New, res.Found)
	}
	if res.SaveFailed != 0 {
		t.Errorf("save_failed = %d: мета и адреса обязаны доезжать под несократимым бюджетом", res.SaveFailed)
	}
	probe := context.Background()
	o, err := st.GetOnion(probe, cHost)
	if err != nil {
		t.Fatalf("GetOnion сида: %v", err)
	}
	if o.Title != "Сборник" {
		t.Errorf("заголовок Titles потерян при обрезке: %q", o.Title)
	}
	if _, err := st.GetOnion(probe, "aaaaaaaaaaaaaaaa.onion"); err != nil {
		t.Errorf("сосед не заведён: %v", err)
	}
	if res.Probed < 1 || res.ProbeFailed != 0 {
		t.Errorf("живость обхода: probed=%d probe_failed=%d, ожидала probed>=1 при probe_failed=0", res.Probed, res.ProbeFailed)
	}
}

// Живой прогон этапа 168 (смоук-агент F4): file_refs_found=284, а
// files+revisited упирались ровно в 200 - потолок замера defaultSizeProbes
// подменял список записи, и хвост ссылок терялся молча: не записывался и не
// считался повторным. Тест ставит ссылок больше потолка и гоняет прогон
// дважды: первый записывает всё, второй весь список находит повторным.
func TestRunFilesWritePastMeasureCeiling(t *testing.T) {
	st := newStore(t)
	page := "http://" + cHost + "/"
	const n = 205
	var sb strings.Builder
	sb.WriteString(`<html><head><title>Сборник</title></head><body>`)
	for i := 0; i < n; i++ {
		fmt.Fprintf(&sb, `<a href="/files/f%03d.zip">файл</a>`, i)
	}
	sb.WriteString(`</body></html>`)
	body := sb.String()
	cfg := fastCrawl(CrawlConfig{Depth: 1})
	cr := NewCrawler(&stubCrawl{
		pages: map[string]string{page: body},
		head:  http.Header{"Content-Length": []string{"777"}},
	}, silentLog{}, cfg)
	p := &Pool{
		Store:  st,
		Finder: &Finder{Client: &brokenWriteSource{n: 1}, Log: silentLog{}, Attempts: 1},
		Crawl:  cr,
		Log:    silentLog{},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	first, err := p.Run(ctx, Options{WithCrawl: true, Seeds: []string{cHost}, MaxHosts: 2})
	if err != nil {
		t.Fatalf("первый Run: %v", err)
	}
	if first.Crawl == nil {
		t.Fatal("crawl-отчёта нет")
	}
	if got := first.Crawl.Files; got != n {
		t.Fatalf("file_refs_found = %d, хочу %d: обход обязан насчитать все ссылки", got, n)
	}
	if got := len(first.Crawl.FileRefs); got != n {
		t.Fatalf("file_refs в ответе длиной %d при found=%d: обрезок замера не имеет права подменять список", got, n)
	}
	if first.Files != n {
		t.Fatalf("первый прогон: files = %d при %d ссылках: запись обязана идти по полному списку, не по потолку замера", first.Files, n)
	}
	if first.FileSaveFailed != 0 {
		t.Fatalf("file_save_failed = %d на живом каталоге", first.FileSaveFailed)
	}
	if first.Revisited != 0 {
		t.Fatalf("первый прогон: revisited = %d при чистом каталоге", first.Revisited)
	}

	// Повторный прогон: каталог уже несёт все ссылки - весь список обязан
	// отчитаться повторным, а не упасть в потолок 200.
	second, err := p.Run(ctx, Options{WithCrawl: true, Seeds: []string{cHost}, MaxHosts: 2})
	if err != nil {
		t.Fatalf("второй Run: %v", err)
	}
	if second.Files != 0 {
		t.Fatalf("повторный прогон записал %d новых файлов при полном каталоге", second.Files)
	}
	if second.Revisited != n {
		t.Fatalf("повторный прогон: revisited = %d, хочу %d - хвост за потолком замера терялся молча", second.Revisited, n)
	}
	if second.FileSaveFailed != 0 {
		t.Fatalf("повторный прогон: file_save_failed = %d", second.FileSaveFailed)
	}
}

// Живой BEFORE этапа 169 (стенд vss169): на повторном прогоне file_refs в
// ответе discover терял размеры, которые каталог знал с первой записи - все
// повторные находки шли с size=0, пока их снова не замеряли. Тест гоняет
// прогон дважды с разными HEAD-ответами: первый записывает файлы с размером
// 777, второй отвечает 888 на HEAD. Повторная находка обязана нести размер
// каталога (777): ни ноль (размер потерян), ни 888 (перевод замера) - значит,
// enrichment идёт из базы, а не из новой сети.
func TestRunRevisitedRefsCarryCatalogSizes(t *testing.T) {
	st := newStore(t)
	page := "http://" + cHost + "/"
	body := `<html><body><a href="/files/dump.zip">дамп</a><a href="/files/keys.7z">ключи</a></body></html>`
	mkPool := func(head string) *Pool {
		cr := NewCrawler(&stubCrawl{
			pages: map[string]string{page: body},
			head:  http.Header{"Content-Length": []string{head}},
		}, silentLog{}, fastCrawl(CrawlConfig{Depth: 1}))
		return &Pool{
			Store:  st,
			Finder: &Finder{Client: &brokenWriteSource{n: 1}, Log: silentLog{}, Attempts: 1},
			Crawl:  cr,
			Log:    silentLog{},
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	first, err := mkPool("777").Run(ctx, Options{WithCrawl: true, Seeds: []string{cHost}, MaxHosts: 2})
	if err != nil {
		t.Fatalf("первый Run: %v", err)
	}
	if first.Files != 2 {
		t.Fatalf("первый прогон: files = %d, хочу 2", first.Files)
	}

	time.Sleep(1100 * time.Millisecond)

	second, err := mkPool("888").Run(ctx, Options{WithCrawl: true, Seeds: []string{cHost}, MaxHosts: 2})
	if err != nil {
		t.Fatalf("второй Run: %v", err)
	}
	if second.Revisited != 2 {
		t.Fatalf("повторный прогон: revisited = %d, хочу 2", second.Revisited)
	}
	if len(second.Crawl.FileRefs) != 2 {
		t.Fatalf("file_refs в ответе длиной %d, хочу 2", len(second.Crawl.FileRefs))
	}
	for _, f := range second.Crawl.FileRefs {
		if f.Size != 777 {
			t.Errorf("повторная находка %s: size = %d, хочу 777 из каталога - ни 0 (размер потерян), ни 888 (перевод замера)", f.URL, f.Size)
		}
	}
}

// Живой BEFORE этапа 169 (стенд vss169): окно 60s и пер-хостовая пауза дают
// ~35 замеров из 275, а порядок замера был алфавитный по URL - новые ссылки
// не имели приоритета. Тест воспроизводит ловушку потолком defaultSizeProbes:
// каталог уже несёт 205 записей, страница к ним добавляет 3 новые, всего 208 -
// за потолком 200. Алфавитно имена новых ссылок идут ПОСЛЕ старых, поэтому
// без приоритета свежести они не попадают в замер и ложатся в каталог с
// size=0. Свежие обязаны меряться первыми: 3 новые с размером, а 205
// повторных - с размером из каталога.
func TestRunMeasuresFreshRefsBeforeStaleCeiling(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	const stale, fresh = 205, 3
	for i := 0; i < stale; i++ {
		err := st.AddFile(ctx, store.FileEntry{
			TaskID:   "crawl-preseed",
			URL:      fmt.Sprintf("http://%s/files/old%03d.zip", cHost, i),
			Filename: fmt.Sprintf("old%03d.zip", i),
			Ext:      "zip",
			Size:     555,
		})
		if err != nil {
			t.Fatalf("preseed %d: %v", i, err)
		}
	}

	page := "http://" + cHost + "/"
	var sb strings.Builder
	sb.WriteString(`<html><head><title>Сборник</title></head><body>`)
	for i := 0; i < stale; i++ {
		fmt.Fprintf(&sb, `<a href="/files/old%03d.zip">файл</a>`, i)
	}
	for i := 0; i < fresh; i++ {
		fmt.Fprintf(&sb, `<a href="/files/new%03d.zip">файл</a>`, i)
	}
	sb.WriteString(`</body></html>`)
	cr := NewCrawler(&stubCrawl{
		pages: map[string]string{page: sb.String()},
		head:  http.Header{"Content-Length": []string{"888"}},
	}, silentLog{}, fastCrawl(CrawlConfig{Depth: 1}))
	p := &Pool{
		Store:  st,
		Finder: &Finder{Client: &brokenWriteSource{n: 1}, Log: silentLog{}, Attempts: 1},
		Crawl:  cr,
		Log:    silentLog{},
	}

	runCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := p.Run(runCtx, Options{WithCrawl: true, Seeds: []string{cHost}, MaxHosts: 2})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Files != fresh {
		t.Fatalf("files = %d, хочу %d: записываются только новые ссылки", res.Files, fresh)
	}
	if res.Revisited != stale {
		t.Fatalf("revisited = %d, хочу %d", res.Revisited, stale)
	}
	if len(res.Crawl.FileRefs) != stale+fresh {
		t.Fatalf("file_refs в ответе длиной %d, хочу %d", len(res.Crawl.FileRefs), stale+fresh)
	}
	// Свежие: попали в потолок замера и несут свежий HEAD (888), не 0 и не
	// каталожный 555 (его у них нет).
	// Повторные: несут размер из каталога (555), не 0 и не перевод замера.
	for _, f := range res.Crawl.FileRefs {
		if strings.Contains(f.URL, "/new") {
			if f.Size != 888 {
				t.Errorf("новая ссылка %s: size = %d, хочу 888 - свежие обязаны меряться первыми при потолке 200 из %d", f.URL, f.Size, stale+fresh)
			}
			continue
		}
		if f.Size != 555 {
			t.Errorf("повторная ссылка %s: size = %d, хочу 555 из каталога - ни 0, ни перевод замера 888", f.URL, f.Size)
		}
	}
	got, err := st.SearchFiles(ctx, store.FileQuery{TaskID: res.FileTaskID, Limit: 100})
	if err != nil {
		t.Fatalf("SearchFiles: %v", err)
	}
	if len(got) != fresh {
		t.Fatalf("по метке задачи %d записей, хочу %d", len(got), fresh)
	}
	for _, f := range got {
		if f.Size != 888 {
			t.Errorf("новая запись %s легла с size = %d, хочу 888: замер свежести не доехал до записи", f.URL, f.Size)
		}
	}
}
