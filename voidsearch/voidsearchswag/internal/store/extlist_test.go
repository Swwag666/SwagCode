package store

import (
	"context"
	"testing"
)

func TestSplitExtList(t *testing.T) {
	cases := []struct {
		raw  string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"epub", []string{"epub"}},
		{"epub,pdf", []string{"epub", "pdf"}},
		{"epub pdf", []string{"epub", "pdf"}},
		{"epub;pdf", []string{"epub", "pdf"}},
		{"epub, pdf, mp4", []string{"epub", "pdf", "mp4"}},
		{".EPUB, Pdf", []string{"epub", "pdf"}},
		{"epub,,pdf", []string{"epub", "pdf"}},
		{"epub,epub,pdf", []string{"epub", "pdf"}},
		{",,,", nil},
		{"...", nil},
		{"\tepub\n", []string{"epub"}},
	}
	for _, c := range cases {
		got := splitExtList(c.raw)
		if len(got) != len(c.want) {
			t.Errorf("splitExtList(%q) = %v, ожидала %v", c.raw, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("splitExtList(%q)[%d] = %q, ожидала %q", c.raw, i, got[i], c.want[i])
			}
		}
	}
}

// TestSearchFilesMultipleExtensions закрывает дефект, из-за которого
// -ext "epub,pdf" молча возвращал пустой результат.
//
// Прежняя версия сравнивала ext = ? дословно, а в каталоге нет строки с
// расширением, в точности равным «epub,pdf». Пустой результат при этом
// неотличим от «таких файлов нет», и пользователь не получал ни ошибки, ни
// намёка на неверный синтаксис.
func TestSearchFilesMultipleExtensions(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	for _, f := range []FileEntry{
		{TaskID: "t", URL: "http://a.onion/book.epub", Filename: "book.epub", Ext: "epub", Size: 100},
		{TaskID: "t", URL: "http://a.onion/report.pdf", Filename: "report.pdf", Ext: "pdf", Size: 200},
		{TaskID: "t", URL: "http://a.onion/video.mp4", Filename: "video.mp4", Ext: "mp4", Size: 300},
		{TaskID: "t", URL: "http://a.onion/dump.sql", Filename: "dump.sql", Ext: "sql", Size: 400},
	} {
		if err := st.AddFile(ctx, f); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		ext  string
		want int
	}{
		{"epub", 1},
		{"pdf", 1},
		{"epub,pdf", 2},
		{"epub pdf", 2},
		{".EPUB, Pdf", 2},
		{"epub,pdf,mp4", 3},
		{"epub,pdf,mp4,sql", 4},
		{"epub,epub", 1},
		{"", 4},
		{"zip", 0},
		{"epub,zip", 1},
	}
	for _, c := range cases {
		got, err := st.SearchFiles(ctx, FileQuery{Ext: c.ext, Limit: 100})
		if err != nil {
			t.Errorf("ext=%q: %v", c.ext, err)
			continue
		}
		if len(got) != c.want {
			t.Errorf("ext=%q: получено %d, ожидала %d", c.ext, len(got), c.want)
		}
	}
}

func TestSearchFilesMultipleExtCombinesWithOtherFilters(t *testing.T) {
	// Список расширений обязан сочетаться с прочими условиями, а не заменять
	// их: иначе фильтр по размеру молча переставал работать.
	st := newStore(t)
	ctx := context.Background()
	for _, f := range []FileEntry{
		{TaskID: "t1", URL: "http://a.onion/small.epub", Filename: "small.epub", Ext: "epub", Size: 100},
		{TaskID: "t1", URL: "http://a.onion/big.epub", Filename: "big.epub", Ext: "epub", Size: 5000},
		{TaskID: "t2", URL: "http://a.onion/small.pdf", Filename: "small.pdf", Ext: "pdf", Size: 150},
	} {
		if err := st.AddFile(ctx, f); err != nil {
			t.Fatal(err)
		}
	}

	got, err := st.SearchFiles(ctx, FileQuery{Ext: "epub,pdf", MaxSize: 1000, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("получено %d, ожидала 2 (small.epub и small.pdf)", len(got))
	}

	got, err = st.SearchFiles(ctx, FileQuery{Ext: "epub,pdf", TaskID: "t1", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("с задачей t1 получено %d, ожидала 2", len(got))
	}

	got, err = st.SearchFiles(ctx, FileQuery{Ext: "epub,pdf", Text: "big", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Filename != "big.epub" {
		t.Errorf("с текстом big получено %+v", got)
	}
}
