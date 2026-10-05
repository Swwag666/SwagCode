package discover

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"voidsearchswag/internal/httpc"
	"voidsearchswag/internal/store"
)

// perURLHead отдаёт Content-Length только тем путям, что названы в sizes.
// Так деление measured/unmeasured проверяется на живых ответах, а не на
// глобальном заголовке фейка: у реального сервера бывает и отдача, и
// молчание Content-Length в одном наборе ссылок.
type perURLHead struct {
	inner *stubCrawl
	sizes map[string]string
	heads int32
}

func (p *perURLHead) Fetch(ctx context.Context, r httpc.Request) (*httpc.Response, error) {
	if r.Method == http.MethodHead {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		atomic.AddInt32(&p.heads, 1)
		h := http.Header{}
		if v, ok := p.sizes[r.URL]; ok {
			h.Set("Content-Length", v)
		}
		return &httpc.Response{Status: 200, Header: h}, nil
	}
	return p.inner.Fetch(ctx, r)
}

// Этап 177, D3. Живой BEFORE (стенд vss177b, 9348844): discover с
// crawl=true отдал file_refs_found=1 и ссылку с size=19256501 - а в
// под-отчёте не было НИ ОДНОГО ключа о фазе замера. size=0 мог значить
// «нет Content-Length», «окно 60s истекло», «потолок 200 замеров»,
// «бюджет вызова обрезал» - ответ молчал, и смоук-агент 176-го трактовал
// недомерянные ссылки как «размер потерян». Подсчёт по факту (мера или
// каталог) выносит итог фазы в ответ.
func TestDiscoverReportsMeasureSplit(t *testing.T) {
	st := newStore(t)
	page := "http://" + cHost + "/"
	body := `<html><body><a href="/files/dump.zip">дамп</a><a href="/files/keys.7z">ключи</a></body></html>`
	head := &perURLHead{
		inner: &stubCrawl{pages: map[string]string{page: body}},
		sizes: map[string]string{page + "files/dump.zip": "12345"},
	}
	cr := NewCrawler(head, silentLog{}, fastCrawl(CrawlConfig{Depth: 1}))
	p := &Pool{
		Store:       st,
		Finder:      &Finder{Client: &brokenWriteSource{n: 1}, Log: silentLog{}, Attempts: 1},
		Crawl:       cr,
		Log:         silentLog{},
		sizesBudget: 2 * time.Second,
	}

	res, err := p.Run(context.Background(), Options{WithCrawl: true, Seeds: []string{cHost}, MaxHosts: 2})
	if err != nil {
		t.Fatalf("Run вернул ошибку: %v", err)
	}
	if res.Crawl.FilesMeasured != 1 {
		t.Errorf("file_refs_measured = %d, ожидала 1 (только dump.zip несёт Content-Length)", res.Crawl.FilesMeasured)
	}
	if res.Crawl.FilesUnmeasured != 1 {
		t.Errorf("file_refs_unmeasured = %d, ожидала 1 (keys.7z без Content-Length)", res.Crawl.FilesUnmeasured)
	}
	if res.Crawl.MeasureCut {
		t.Error("measure_cutoff взведён при живом окне 2s и двух замерах")
	}
	for _, f := range res.Crawl.FileRefs {
		if f.URL == page+"files/dump.zip" && f.Size != 12345 {
			t.Errorf("dump.zip size = %d, ожидала 12345", f.Size)
		}
		if f.URL == page+"files/keys.7z" && f.Size != 0 {
			t.Errorf("keys.7z size = %d, ожидала 0", f.Size)
		}
	}
}

// Срез окна: механика этапа 173 (отмена бюджета на первом GET) - замер
// не ходит в сеть, все ссылки остаются size=0. До этапа 177 этот исход
// не был виден в ответе: строка «доставка жива, размеры молчат» не
// позволяла отличить срез окна от сервера без Content-Length.
func TestDiscoverReportsMeasureCutoff(t *testing.T) {
	st := newStore(t)
	page := "http://" + cHost + "/"
	body := `<html><body><a href="/files/dump.zip">дамп</a><a href="/files/keys.7z">ключи</a></body></html>`
	head := &perURLHead{
		inner: &stubCrawl{pages: map[string]string{page: body}},
		sizes: map[string]string{
			page + "files/dump.zip": "12345",
			page + "files/keys.7z":  "54321",
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cr := NewCrawler(&cancelOnGet{inner: head, cancel: cancel}, silentLog{}, fastCrawl(CrawlConfig{Depth: 1}))
	p := &Pool{
		Store:       st,
		Finder:      &Finder{Client: &brokenWriteSource{n: 1}, Log: silentLog{}, Attempts: 1},
		Crawl:       cr,
		Log:         silentLog{},
		sizesBudget: 2 * time.Second,
	}

	res, err := p.Run(ctx, Options{WithCrawl: true, Seeds: []string{cHost}, MaxHosts: 2})
	if err != nil {
		t.Fatalf("Run вернул ошибку: %v", err)
	}
	if got := atomic.LoadInt32(&head.heads); got != 0 {
		t.Fatalf("после обрезки замер сходил в сеть %d раз", got)
	}
	if !res.Crawl.MeasureCut {
		t.Error("measure_cutoff не взведён при мёртвом окне замера")
	}
	if res.Crawl.FilesMeasured != 0 {
		t.Errorf("file_refs_measured = %d, ожидала 0: сеть для размеров мертва", res.Crawl.FilesMeasured)
	}
	if res.Crawl.FilesUnmeasured != 2 {
		t.Errorf("file_refs_unmeasured = %d, ожидала 2", res.Crawl.FilesUnmeasured)
	}
	if res.Files != 2 {
		t.Fatalf("files = %d: срез окна замера не имеет права рвать доставку", res.Files)
	}
}

// Размер из каталога прошлых прогонов тоже measured: до этапа 177 ссылка
// с размером из каталога не отличалась в ответе от только что замеренную.
func TestDiscoverReportsCatalogSizesAsMeasured(t *testing.T) {
	st := newStore(t)
	page := "http://" + cHost + "/"
	body := `<html><body><a href="/files/dump.zip">дамп</a><a href="/files/keys.7z">ключи</a></body></html>`
	if err := st.AddFile(context.Background(), store.FileEntry{
		TaskID:   "old-task",
		URL:      page + "files/keys.7z",
		Filename: "keys.7z",
		Ext:      "7z",
		Size:     555,
	}); err != nil {
		t.Fatal(err)
	}
	head := &perURLHead{
		inner: &stubCrawl{pages: map[string]string{page: body}},
		sizes: map[string]string{page + "files/dump.zip": "12345"},
	}
	cr := NewCrawler(head, silentLog{}, fastCrawl(CrawlConfig{Depth: 1}))
	p := &Pool{
		Store:       st,
		Finder:      &Finder{Client: &brokenWriteSource{n: 1}, Log: silentLog{}, Attempts: 1},
		Crawl:       cr,
		Log:         silentLog{},
		sizesBudget: 2 * time.Second,
	}

	res, err := p.Run(context.Background(), Options{WithCrawl: true, Seeds: []string{cHost}, MaxHosts: 2})
	if err != nil {
		t.Fatalf("Run вернул ошибку: %v", err)
	}
	if res.Crawl.FilesMeasured != 2 {
		t.Errorf("file_refs_measured = %d, ожидала 2: мера dump.zip + каталог keys.7z", res.Crawl.FilesMeasured)
	}
	if res.Crawl.FilesUnmeasured != 0 {
		t.Errorf("file_refs_unmeasured = %d, ожидала 0", res.Crawl.FilesUnmeasured)
	}
	if res.Crawl.MeasureCut {
		t.Error("measure_cutoff взведён при живом окне")
	}
	found := false
	for _, f := range res.Crawl.FileRefs {
		if f.URL == page+"files/keys.7z" {
			found = true
			if f.Size != 555 {
				t.Errorf("keys.7z size = %d, ожидала 555 из каталога", f.Size)
			}
		}
	}
	if !found {
		t.Fatal("keys.7z не попала в file_refs")
	}
}

// Пустой обход: file_refs_found=0 не должен оставлять счётчики
// неинициализированными - агент обязан видеть 0/0, а не гадать об
// отсутствии ключей. omitempty у чисел не стоит именно поэтому.
func TestDiscoverReportsMeasureZeroOnEmpty(t *testing.T) {
	st := newStore(t)
	page := "http://" + cHost + "/"
	cr := NewCrawler(&stubCrawl{pages: map[string]string{page: "<html><body>пусто</body></html>"}}, silentLog{}, fastCrawl(CrawlConfig{Depth: 1}))
	p := &Pool{
		Store:       st,
		Finder:      &Finder{Client: &brokenWriteSource{n: 1}, Log: silentLog{}, Attempts: 1},
		Crawl:       cr,
		Log:         silentLog{},
		sizesBudget: 2 * time.Second,
	}

	res, err := p.Run(context.Background(), Options{WithCrawl: true, Seeds: []string{cHost}, MaxHosts: 2})
	if err != nil {
		t.Fatalf("Run вернул ошибку: %v", err)
	}
	if res.Crawl.FilesMeasured != 0 || res.Crawl.FilesUnmeasured != 0 {
		t.Errorf("пустой обход: measured=%d unmeasured=%d, ожидала 0/0", res.Crawl.FilesMeasured, res.Crawl.FilesUnmeasured)
	}
	if res.Crawl.MeasureCut {
		t.Error("measure_cutoff взведён без ссылок и сети")
	}
}
