package store

import (
	"context"
	"testing"
)

// TestCleanFileCatalogRemovesSiteMetadata проверяет, что чистка убирает уже
// записанные служебные файлы, а не только перестаёт собирать новые.
//
// Без этого строки, попавшие в базу до введения фильтра, остались бы там
// навсегда: в реальной базе так лежали manifest.json, opensearch_html.xml,
// publicpgp.txt, canary.txt и LICENSE.txt.
func TestCleanFileCatalogRemovesSiteMetadata(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	junk := []FileEntry{
		{TaskID: "t", URL: "http://a.onion/manifest.json", Filename: "manifest.json", Ext: "json"},
		{TaskID: "t", URL: "http://a.onion/static/opensearch_html.xml", Filename: "opensearch_html.xml", Ext: "xml"},
		{TaskID: "t", URL: "http://a.onion/publicpgp.txt", Filename: "publicpgp.txt", Ext: "txt"},
		{TaskID: "t", URL: "http://a.onion/canary.txt", Filename: "canary.txt", Ext: "txt"},
		{TaskID: "t", URL: "http://a.onion/LICENSE.txt", Filename: "LICENSE.txt", Ext: "txt"},
	}
	good := []FileEntry{
		{TaskID: "t", URL: "http://a.onion/book.epub", Filename: "book.epub", Ext: "epub", Size: 1000},
		{TaskID: "t", URL: "http://a.onion/video.mp4", Filename: "video.mp4", Ext: "mp4", Size: 2000},
		{TaskID: "t", URL: "http://a.onion/report.pdf", Filename: "report.pdf", Ext: "pdf", Size: 300},
	}
	for _, f := range append(append([]FileEntry{}, junk...), good...) {
		if err := st.AddFile(ctx, f); err != nil {
			t.Fatal(err)
		}
	}

	removed, err := st.CleanFileCatalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if removed != int64(len(junk)) {
		t.Errorf("удалено %d, ожидала %d", removed, len(junk))
	}

	left, err := st.ListFiles(ctx, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != len(good) {
		t.Errorf("осталось %d файлов, ожидала %d", len(left), len(good))
	}
	for _, f := range left {
		switch f.Filename {
		case "book.epub", "video.mp4", "report.pdf":
		default:
			t.Errorf("в каталоге остался служебный файл %q", f.Filename)
		}
	}
}

// TestCleanFileCatalogRemovesAPIEndpoints закрывает второй класс мусора:
// ответы эндпоинтов API имеют «контентное» расширение .json, поэтому фильтр
// по расширению их не ловит и путь нужно проверять отдельно.
func TestCleanFileCatalogRemovesAPIEndpoints(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	// Оба URL взяты из реальной базы.
	for _, f := range []FileEntry{
		{TaskID: "t", URL: "http://a.onion/wp-json/oembed/1.0/embedc427.json?url=https%3A%2F%2Fsite%2F", Filename: "embedc427.json", Ext: "json"},
		{TaskID: "t", URL: "http://a.onion/wp-json/wp/v2/pages/7105.json", Filename: "7105.json", Ext: "json"},
	} {
		if err := st.AddFile(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	// Обычный .json остаётся: фильтр по пути, а не по расширению.
	if err := st.AddFile(ctx, FileEntry{
		TaskID: "t", URL: "http://a.onion/data/dataset.json", Filename: "dataset.json", Ext: "json", Size: 50,
	}); err != nil {
		t.Fatal(err)
	}

	removed, err := st.CleanFileCatalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Errorf("удалено %d, ожидала 2", removed)
	}
	left, err := st.ListFiles(ctx, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[0].Filename != "dataset.json" {
		t.Errorf("осталось %+v, ожидала только dataset.json", left)
	}
}

func TestCleanFileCatalogIdempotent(t *testing.T) {
	// Повторная чистка не должна ничего находить: иначе мусор возвращается или
	// счётчик врёт.
	st := newStore(t)
	ctx := context.Background()
	if err := st.AddFile(ctx, FileEntry{TaskID: "t", URL: "http://a.onion/robots.txt", Filename: "robots.txt", Ext: "txt"}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddFile(ctx, FileEntry{TaskID: "t", URL: "http://a.onion/book.epub", Filename: "book.epub", Ext: "epub", Size: 10}); err != nil {
		t.Fatal(err)
	}
	if n, err := st.CleanFileCatalog(ctx); err != nil || n != 1 {
		t.Fatalf("первая чистка: n=%d err=%v", n, err)
	}
	if n, err := st.CleanFileCatalog(ctx); err != nil || n != 0 {
		t.Errorf("вторая чистка нашла %d, err=%v", n, err)
	}
}
