package catalog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"voidsearchswag/internal/httpc"
	"voidsearchswag/internal/store"
)

const v2a = "abcdefghijklmnop"
const v2b = "qrstuvwxyz234567"

// fakeClient отдаёт заранее заданное тело на любой GET и Content-Length на
// HEAD. Сети не касается: сбор проверяется детерминированно.
type fakeClient struct {
	body  string
	size  string
	head  bool
	delay time.Duration
	err   error
}

func (f *fakeClient) Fetch(ctx context.Context, r httpc.Request) (*httpc.Response, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	h := http.Header{}
	if r.Method == http.MethodHead {
		f.head = true
		if f.size != "" {
			h.Set("Content-Length", f.size)
		}
		return &httpc.Response{Status: 200, Header: h}, nil
	}
	return &httpc.Response{Status: 200, Header: h, Body: []byte(f.body)}, nil
}

// newRealClient заводит настоящий клиент для проверки на httptest-сервере:
// так тест ловит ошибки разбора ответа, которые фейк не воспроизводит.
func newRealClient(t *testing.T) *httpc.Client {
	t.Helper()
	c, err := httpc.NewClient(context.Background(), httpc.Options{
		Transport: "direct",
		Timeout:   10 * time.Second,
		Logger:    silentLogger{},
	})
	if err != nil {
		t.Fatalf("клиент: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// silentLogger глушит вывод клиента: логи здесь не проверяются, а в
// консоль они сыпятся на каждом запросе.
type silentLogger struct{}

func (silentLogger) Infof(string, ...any) {}
func (silentLogger) Warnf(string, ...any) {}

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestConfigDefaults(t *testing.T) {
	c := Config{}.withDefaults()
	if c.MaxHosts != 20 {
		t.Errorf("MaxHosts=%d, ожидала 20", c.MaxHosts)
	}
	if c.MaxFiles != 500 {
		t.Errorf("MaxFiles=%d, ожидала 500", c.MaxFiles)
	}
	if c.PerHostDelay != 2*time.Second {
		t.Errorf("PerHostDelay=%v", c.PerHostDelay)
	}
	if c.PageTimeout != 45*time.Second {
		t.Errorf("PageTimeout=%v", c.PageTimeout)
	}
	if c.Concurrency != 4 {
		t.Errorf("Concurrency=%d", c.Concurrency)
	}
	if c.MaxFileSize != 256<<20 {
		t.Errorf("MaxFileSize=%d", c.MaxFileSize)
	}
}

func TestConfigKeepsExplicit(t *testing.T) {
	in := Config{MaxHosts: 3, MaxFiles: 7, Depth: 4, PageLimit: 9,
		PerHostDelay: time.Second, PageTimeout: 5 * time.Second,
		Concurrency: 2, MaxFileSize: 128}
	if got := in.withDefaults(); got != in {
		t.Errorf("явные значения затёрты: %+v", got)
	}
}

func TestConfigFillsDepthAndPageLimit(t *testing.T) {
	c := Config{}.withDefaults()
	if c.Depth != 2 {
		t.Errorf("Depth=%d, ожидала 2", c.Depth)
	}
	if c.PageLimit != 12 {
		t.Errorf("PageLimit=%d, ожидала 12", c.PageLimit)
	}
}

func TestContentLength(t *testing.T) {
	h := http.Header{}
	h.Set("Content-Length", "12345")
	if n, ok := contentLength(h); !ok || n != 12345 {
		t.Errorf("contentLength = %d, %v", n, ok)
	}
	h.Set("Content-Length", "abc")
	if _, ok := contentLength(h); ok {
		t.Error("нечисловое значение принято")
	}
	h.Set("Content-Length", "")
	if _, ok := contentLength(h); ok {
		t.Error("пустое значение принято")
	}
	if _, ok := contentLength(nil); ok {
		t.Error("nil-заголовки приняты")
	}
	h2 := http.Header{}
	h2.Set("Content-Length", "99999999999999")
	if _, ok := contentLength(h2); ok {
		t.Error("абсурдный размер принят")
	}
}

func TestNormaliseHosts(t *testing.T) {
	got := normaliseHosts([]string{
		"b.onion", "a.onion", "a.onion", " http://b.onion/ ", "", "http://c.onion/x",
	})
	want := []string{"a.onion", "b.onion", "c.onion"}
	if len(got) != len(want) {
		t.Fatalf("получено %v, ожидала %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("порядок или содержимое: %v, ожидала %v", got, want)
		}
	}
}

func TestNormaliseHostsURLCleans(t *testing.T) {
	got := normaliseHosts([]string{"http://" + v2a + ".onion/"})
	if len(got) != 1 || got[0] != v2a+".onion" {
		t.Errorf("URL не приведён к хосту: %v", got)
	}
}

func TestInnerPagesFiltersByExtension(t *testing.T) {
	st := newStore(t)
	f := &fakeClient{}
	c := NewCollector(f, st, nil, nil, fastCfg(Config{}))
	body := `<a href="/files/">dir</a>
<a href="/about">about</a>
<a href="/x.zip">zip</a>
<a href="/style.css">css</a>
<a href="http://other.onion/page">other</a>`
	got := c.innerPages(body, "http://"+v2a+".onion/", v2a+".onion")
	want := []string{"http://" + v2a + ".onion/about", "http://" + v2a + ".onion/files/"}
	if len(got) != len(want) {
		t.Fatalf("получено %v, ожидала %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("элемент %d: %q, ожидала %q", i, got[i], want[i])
		}
	}
}

func TestInnerPagesStripsQuery(t *testing.T) {
	st := newStore(t)
	c := NewCollector(&fakeClient{}, st, nil, nil, fastCfg(Config{}))
	body := `<a href="/list?page=1">p1</a><a href="/list?page=2">p2</a>`
	got := c.innerPages(body, "http://"+v2a+".onion/", v2a+".onion")
	if len(got) != 1 {
		t.Fatalf("страницы с разным query не схлопнуты: %v", got)
	}
	if strings.Contains(got[0], "?") {
		t.Errorf("query остался: %q", got[0])
	}
}

func TestCollectorGoesDeeper(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.Write([]byte(`<a href="/files">files</a><a href="/style.css">css</a>`))
		case "/files":
			w.Write([]byte(`<a href="/data/dump.sql">d</a>`))
		default:
			if r.Method == http.MethodHead {
				w.Header().Set("Content-Length", "100")
				w.WriteHeader(http.StatusOK)
				return
			}
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	st := newStore(t)
	c := NewCollector(newRealClient(t), st, nil, nil,
		fastCfg(Config{Depth: 2, PageLimit: 5}))
	rep, err := c.Collect(context.Background(), []string{host}, "t")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Pages < 2 {
		t.Errorf("обход не пошёл вглубь: страниц %d", rep.Pages)
	}
	if rep.Saved != 1 {
		t.Fatalf("сохранено %d, ожидала 1 файл с внутренней страницы", rep.Saved)
	}
	files, err := st.SearchFiles(context.Background(), store.FileQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Ext != "sql" {
		t.Fatalf("в каталоге %+v", files)
	}
	if !strings.Contains(files[0].SourcePage, "/files") {
		t.Errorf("страница-источник не записана: %q", files[0].SourcePage)
	}
}

func TestCollectorDepthOneStaysOnRoot(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Write([]byte(`<a href="/files">files</a>`))
			return
		}
		w.Write([]byte(`<a href="/data/dump.sql">d</a>`))
	}))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	st := newStore(t)
	c := NewCollector(newRealClient(t), st, nil, nil, fastCfg(Config{Depth: 1}))
	rep, err := c.Collect(context.Background(), []string{host}, "t")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Pages != 1 {
		t.Errorf("при Depth=1 пройдено страниц %d, ожидала 1", rep.Pages)
	}
	if rep.Saved != 0 {
		t.Errorf("при Depth=1 найдено файлов %d, ожидала 0", rep.Saved)
	}
}

func TestCollectorPageLimit(t *testing.T) {
	var root []byte
	for i := 0; i < 10; i++ {
		root = append(root, []byte(`<a href="/p`+string(rune('a'+i))+`">p</a>`)...)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Write(root)
			return
		}
		w.Write([]byte(`<a href="/x.zip">x</a>`))
	}))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	st := newStore(t)
	c := NewCollector(newRealClient(t), st, nil, nil,
		fastCfg(Config{Depth: 2, PageLimit: 3}))
	rep, err := c.Collect(context.Background(), []string{host}, "t")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Pages > 3 {
		t.Errorf("потолок страниц не сработал: %d", rep.Pages)
	}
}

func TestCollectorDeduplicatesFilesAcrossPages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Write([]byte(`<a href="/a">a</a><a href="/b">b</a>`))
			return
		}
		if r.Method == http.MethodHead {
			return
		}
		w.Write([]byte(`<a href="/same.zip">s</a>`))
	}))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	st := newStore(t)
	c := NewCollector(newRealClient(t), st, nil, nil,
		fastCfg(Config{Depth: 2, PageLimit: 5}))
	rep, err := c.Collect(context.Background(), []string{host}, "t")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Saved != 1 {
		t.Errorf("дубль не схлопнут: сохранено %d", rep.Saved)
	}
}

func TestCollectorFindsAttachmentWithoutExt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.Write([]byte(`<a href="/download?id=7">dl</a><a href="/about">a</a>`))
		case "/download":
			if r.Method == http.MethodHead {
				w.Header().Set("Content-Disposition", `attachment; filename="backup.zip"`)
				w.Header().Set("Content-Length", "777")
				w.WriteHeader(http.StatusOK)
				return
			}
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	st := newStore(t)
	c := NewCollector(newRealClient(t), st, nil, nil, fastCfg(Config{Depth: 1}))
	rep, err := c.Collect(context.Background(), []string{host}, "t")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Saved != 1 {
		t.Fatalf("файл по Content-Disposition не найден: сохранено %d", rep.Saved)
	}
	files, err := st.SearchFiles(context.Background(), store.FileQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("в каталоге %d записей", len(files))
	}
	if files[0].Filename != "backup.zip" {
		t.Errorf("имя из заголовка не взято: %q", files[0].Filename)
	}
	if files[0].Size != 777 {
		t.Errorf("размер %d, ожидала 777", files[0].Size)
	}
}

func TestCollectorIgnoresInlineDisposition(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Write([]byte(`<a href="/download/x">dl</a>`))
			return
		}
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Disposition", `inline; filename="page.html"`)
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	st := newStore(t)
	c := NewCollector(newRealClient(t), st, nil, nil, fastCfg(Config{Depth: 1}))
	rep, err := c.Collect(context.Background(), []string{host}, "t")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Saved != 0 {
		t.Errorf("inline-выдача принята за файл: %d", rep.Saved)
	}
}

func TestCollectorSkipsNonDownloadPaths(t *testing.T) {
	var heads int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Write([]byte(`<a href="/about">a</a><a href="/contact">c</a>`))
			return
		}
		if r.Method == http.MethodHead {
			heads++
			w.Header().Set("Content-Disposition", `attachment; filename="x.zip"`)
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	st := newStore(t)
	c := NewCollector(newRealClient(t), st, nil, nil, fastCfg(Config{Depth: 1}))
	if _, err := c.Collect(context.Background(), []string{host}, "t"); err != nil {
		t.Fatal(err)
	}
	if heads != 0 {
		t.Errorf("сделано %d проверочных запросов по обычным ссылкам, ожидала 0", heads)
	}
}

func TestCollectorIgnoresOversizedAttachment(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Write([]byte(`<a href="/download/big">b</a>`))
			return
		}
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Disposition", `attachment; filename="big.iso"`)
			w.Header().Set("Content-Length", "999999999")
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	st := newStore(t)
	c := NewCollector(newRealClient(t), st, nil, nil,
		fastCfg(Config{Depth: 1, MaxFileSize: 1024}))
	rep, err := c.Collect(context.Background(), []string{host}, "t")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Saved != 0 {
		t.Errorf("слишком большой файл сохранён: %d", rep.Saved)
	}
}

func TestCollectFilesWithoutStore(t *testing.T) {
	c := NewCollector(nil, nil, nil, nil, Config{})
	if _, err := c.Collect(context.Background(), []string{"a.onion"}, ""); err == nil {
		t.Error("без базы сбор должен вернуть ошибку")
	}
}

func TestCollectWithoutClient(t *testing.T) {
	st := newStore(t)
	c := NewCollector(nil, st, nil, nil, Config{})
	if _, err := c.Collect(context.Background(), []string{"a.onion"}, ""); err == nil {
		t.Error("без клиента сбор должен вернуть ошибку")
	}
}

func TestCollectWithoutHosts(t *testing.T) {
	st := newStore(t)
	c := NewCollector(&fakeClient{}, st, nil, nil, Config{})
	if _, err := c.Collect(context.Background(), nil, ""); err == nil {
		t.Error("без хостов сбор должен вернуть ошибку")
	}
}

func TestCollectCancelled(t *testing.T) {
	st := newStore(t)
	c := NewCollector(&fakeClient{}, st, nil, nil, Config{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rep, err := c.Collect(ctx, []string{"a.onion"}, "")
	if err != nil {
		t.Fatalf("отмена не должна давать ошибку: %v", err)
	}
	if rep.Saved != 0 {
		t.Errorf("при отмене сохранено %d", rep.Saved)
	}
}

// cancelCountClient держит ответ дольше отмены и считает запросы: после
// поголовной отмены контекста новые хосты не должны даже стартовать.
type cancelCountClient struct {
	hits int32
}

func (c *cancelCountClient) Fetch(ctx context.Context, r httpc.Request) (*httpc.Response, error) {
	atomic.AddInt32(&c.hits, 1)
	select {
	case <-time.After(200 * time.Millisecond):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	h := http.Header{}
	if r.Method == http.MethodHead {
		return &httpc.Response{Status: 200, Header: h}, nil
	}
	return &httpc.Response{Status: 200, Header: h, Body: []byte("")}, nil
}

// Смоук-J Д1 (хвост этапа): после отмены контекста вызова обход обязан
// перестать ЗАПУСКАТЬ новые хосты, а не только позволять текущим упасть.
// До правки семафор хост-цикла не смотрел на ctx, и хвост выборки молотил
// запросы по уже мёртвому соединению. Здесь concurrency=1: первый хост
// держит семафор 200мс, отмена приходит на 20мс - хвост обязан быть
// остановлен по ctx, а не по исчерпанию выборки.
func TestCollectStopsLaunchingAfterCancel(t *testing.T) {
	st := newStore(t)
	f := &cancelCountClient{}
	c := NewCollector(f, st, nil, nil, fastCfg(Config{Concurrency: 1}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(20*time.Millisecond, cancel)
	rep, err := c.Collect(ctx, []string{"a.onion", "b.onion", "c.onion"}, "")
	if err != nil {
		t.Fatalf("отмена не должна давать ошибку: %v", err)
	}
	if hits := atomic.LoadInt32(&f.hits); hits != 1 {
		t.Errorf("после отмены стартовало %d запросов, хочу ровно 1: хвост выборки обязан останавливаться по контексту, а не дорабатывать очередь", hits)
	}
	if rep.Failed != 3 {
		t.Errorf("вся выборка обязана отчитаться в Failed (хвост с явной причиной): failed=%d, хочу 3", rep.Failed)
	}
}

func TestCollectMaxHosts(t *testing.T) {
	st := newStore(t)
	f := &fakeClient{}
	c := NewCollector(f, st, nil, nil, fastCfg(Config{MaxHosts: 2}))
	rep, err := c.Collect(context.Background(), []string{"a.onion", "b.onion", "c.onion"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Hosts != 2 {
		t.Errorf("обработано хостов %d, ожидала 2", rep.Hosts)
	}
	if rep.LimitHit == "" {
		t.Error("потолок хостов не отмечен")
	}
}

func TestCollectStoresFiles(t *testing.T) {
	st := newStore(t)
	f := &fakeClient{body: `<a href="/a.zip">a</a><a href="/b.pdf">b</a><a href="/c.css">c</a>`}
	c := NewCollector(f, st, nil, nil, fastCfg(Config{}))
	rep, err := c.Collect(context.Background(), []string{v2a + ".onion"}, "task1")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Saved != 2 {
		t.Fatalf("сохранено %d, ожидала 2 (css не файл содержимого)", rep.Saved)
	}
	if rep.ByExt["zip"] != 1 || rep.ByExt["pdf"] != 1 {
		t.Errorf("разбивка неверна: %v", rep.ByExt)
	}
	files, err := st.SearchFiles(context.Background(), store.FileQuery{TaskID: "task1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("в базе %d файлов, ожидала 2", len(files))
	}
}

func TestCollectIgnoresOtherHosts(t *testing.T) {
	st := newStore(t)
	f := &fakeClient{body: `<a href="http://` + v2b + `.onion/x.zip">other</a>`}
	c := NewCollector(f, st, nil, nil, fastCfg(Config{}))
	rep, err := c.Collect(context.Background(), []string{v2a + ".onion"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Saved != 0 {
		t.Errorf("чужой хост принят: сохранено %d", rep.Saved)
	}
}

func TestCollectMaxFiles(t *testing.T) {
	st := newStore(t)
	var b strings.Builder
	for i := 0; i < 10; i++ {
		b.WriteString(`<a href="/f`)
		b.WriteByte(byte('0' + i))
		b.WriteString(`.zip">x</a>`)
	}
	f := &fakeClient{body: b.String()}
	c := NewCollector(f, st, nil, nil, fastCfg(Config{MaxFiles: 3}))
	rep, err := c.Collect(context.Background(), []string{v2a + ".onion"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Saved != 3 {
		t.Errorf("потолок файлов не сработал: %d", rep.Saved)
	}
	if rep.LimitHit == "" {
		t.Error("потолок файлов не отмечен")
	}
}

func TestCollectSortedSample(t *testing.T) {
	st := newStore(t)
	f := &fakeClient{body: `<a href="/z.zip">z</a><a href="/a.zip">a</a>`}
	c := NewCollector(f, st, nil, nil, fastCfg(Config{}))
	rep, err := c.Collect(context.Background(), []string{v2a + ".onion"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.SampleURLs) == 0 {
		t.Fatal("нет примеров")
	}
	if !sort.StringsAreSorted(rep.SampleURLs) {
		t.Errorf("примеры не упорядочены: %v", rep.SampleURLs)
	}
}

func TestCollectorRealServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte(`<a href="/data/dump.sql">d</a>`))
			return
		}
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", "4242")
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	st := newStore(t)
	fc := newRealClient(t)
	c := NewCollector(fc, st, nil, nil, Config{PerHostDelay: time.Millisecond})
	rep, err := c.Collect(context.Background(), []string{host}, "t")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Saved != 1 {
		t.Fatalf("сохранено %d, ожидала 1", rep.Saved)
	}
	files, err := st.SearchFiles(context.Background(), store.FileQuery{Ext: "sql"})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("в каталоге %d записей", len(files))
	}
	if files[0].Size != 4242 {
		t.Errorf("размер %d, ожидала 4242", files[0].Size)
	}
	if files[0].MIME != "application/sql" {
		t.Errorf("mime=%q", files[0].MIME)
	}
}

// fastCfg убирает паузу между запросами к хосту: в тестах она только
// тянет время и ничего не проверяет.
func fastCfg(c Config) Config {
	c.PerHostDelay = time.Millisecond
	return c
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
