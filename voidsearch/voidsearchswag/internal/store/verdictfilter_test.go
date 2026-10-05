package store

import (
	"context"
	"testing"

	"voidsearchswag/internal/filex"
)

func TestSplitVerdictList(t *testing.T) {
	cases := []struct {
		raw  string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"ebook", []string{"ebook"}},
		{"EBOOK", []string{"ebook"}},
		{"  Ebook  ", []string{"ebook"}},
		{"ebook,document", []string{"ebook", "document"}},
		{"ebook document", []string{"ebook", "document"}},
		{"ebook;document", []string{"ebook", "document"}},
		{"ebook,\tdocument", []string{"ebook", "document"}},
		{"ebook,,document", []string{"ebook", "document"}},
		{",,,", nil},
		{"ebook,ebook", []string{"ebook"}},
		{"EBOOK,ebook", []string{"ebook"}},
		{"executable,secret,archive", []string{"executable", "secret", "archive"}},
	}
	for _, c := range cases {
		got := splitVerdictList(c.raw)
		if len(got) != len(c.want) {
			t.Errorf("splitVerdictList(%q) = %v, ожидала %v", c.raw, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("splitVerdictList(%q)[%d] = %q, ожидала %q", c.raw, i, got[i], c.want[i])
			}
		}
	}
}

func TestSplitVerdictListKeepsOrder(t *testing.T) {
	// Порядок сохраняется: список уходит в IN, где порядок не важен для
	// результата, но важен для читаемости сообщения об ошибке, если фильтр
	// когда-нибудь начнёт проверять значения.
	got := splitVerdictList("document,ebook,archive")
	want := []string{"document", "ebook", "archive"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q, ожидала %q", i, got[i], want[i])
		}
	}
}

func TestSearchFilesVerdictFilter(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	entries := []FileEntry{
		{TaskID: "t", URL: "http://a.onion/book.epub", Filename: "book.epub", Ext: "epub", Size: 100, Verdict: "ebook"},
		{TaskID: "t", URL: "http://a.onion/doc.pdf", Filename: "doc.pdf", Ext: "pdf", Size: 200, Verdict: "document"},
		{TaskID: "t", URL: "http://a.onion/setup.exe", Filename: "setup.exe", Ext: "exe", Size: 300, Verdict: "executable"},
		{TaskID: "t", URL: "http://a.onion/pack.zip", Filename: "pack.zip", Ext: "zip", Size: 400, Verdict: "archive"},
		{TaskID: "t", URL: "http://a.onion/keys.kdbx", Filename: "keys.kdbx", Ext: "kdbx", Size: 50, Verdict: "secret"},
	}
	for _, f := range entries {
		if err := st.AddFile(ctx, f); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("одна метка", func(t *testing.T) {
		got, err := st.SearchFiles(ctx, FileQuery{Verdict: "ebook"})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Ext != "epub" {
			t.Errorf("найдено %d записей: %+v", len(got), got)
		}
	})

	t.Run("несколько меток", func(t *testing.T) {
		got, err := st.SearchFiles(ctx, FileQuery{Verdict: "ebook,document"})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Errorf("найдено %d, ожидала 2", len(got))
		}
	})

	t.Run("регистр не важен", func(t *testing.T) {
		got, err := st.SearchFiles(ctx, FileQuery{Verdict: "EBOOK"})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Errorf("фильтр в верхнем регистре нашёл %d, ожидала 1", len(got))
		}
	})

	t.Run("пустой фильтр не ограничивает", func(t *testing.T) {
		got, err := st.SearchFiles(ctx, FileQuery{Verdict: ""})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(entries) {
			t.Errorf("пустой фильтр вернул %d, ожидала %d", len(got), len(entries))
		}
	})

	t.Run("несуществующая метка", func(t *testing.T) {
		got, err := st.SearchFiles(ctx, FileQuery{Verdict: "нет-такой-метки"})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Errorf("найдено %d по несуществующей метке", len(got))
		}
	})

	t.Run("вместе с расширением", func(t *testing.T) {
		// Оба фильтра обязаны применяться одновременно, а не перезаписывать
		// друг друга.
		got, err := st.SearchFiles(ctx, FileQuery{Verdict: "ebook", Ext: "epub"})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Errorf("найдено %d, ожидала 1", len(got))
		}
		got, err = st.SearchFiles(ctx, FileQuery{Verdict: "ebook", Ext: "pdf"})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Errorf("противоречивые фильтры дали %d записей", len(got))
		}
	})

	t.Run("вместе с текстом", func(t *testing.T) {
		got, err := st.SearchFiles(ctx, FileQuery{Verdict: "document", Text: "doc"})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Filename != "doc.pdf" {
			t.Errorf("найдено %d: %+v", len(got), got)
		}
	})

	t.Run("вместе с размером", func(t *testing.T) {
		got, err := st.SearchFiles(ctx, FileQuery{Verdict: "archive", MinSize: 500})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Errorf("фильтр по размеру не применён: %d записей", len(got))
		}
		got, err = st.SearchFiles(ctx, FileQuery{Verdict: "archive", MinSize: 100})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Errorf("найдено %d, ожидала 1", len(got))
		}
	})
}

func TestSearchFilesRiskOnly(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	entries := []FileEntry{
		{TaskID: "t", URL: "http://a.onion/setup.exe", Filename: "setup.exe", Ext: "exe", Size: 300, Verdict: "executable"},
		{TaskID: "t", URL: "http://a.onion/keys.kdbx", Filename: "keys.kdbx", Ext: "kdbx", Size: 50, Verdict: "secret"},
		{TaskID: "t", URL: "http://a.onion/book.epub", Filename: "book.epub", Ext: "epub", Size: 100, Verdict: "ebook"},
		{TaskID: "t", URL: "http://a.onion/doc.pdf", Filename: "doc.pdf", Ext: "pdf", Size: 200, Verdict: "document"},
		{TaskID: "t", URL: "http://a.onion/pack.zip", Filename: "pack.zip", Ext: "zip", Size: 400, Verdict: "archive"},
	}
	for _, f := range entries {
		if err := st.AddFile(ctx, f); err != nil {
			t.Fatal(err)
		}
	}

	got, err := st.SearchFiles(ctx, FileQuery{RiskOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("найдено %d, ожидала 2 (executable и secret)", len(got))
	}
	for _, f := range got {
		if !filex.VerdictIsRisk(f.Verdict) {
			t.Errorf("в рискованной выборке оказалась метка %q", f.Verdict)
		}
	}

	// Без фильтра возвращаются все записи.
	all, err := st.SearchFiles(ctx, FileQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != len(entries) {
		t.Errorf("без фильтра %d, ожидала %d", len(all), len(entries))
	}
}

func TestSearchFilesRiskOnlyMatchesFilexList(t *testing.T) {
	// Фильтр обязан брать список опасных меток из filex, а не из собственной
	// копии: иначе расширение категорий в одном месте не обновляло бы другое.
	st := newStore(t)
	ctx := context.Background()

	for i, v := range filex.AllVerdicts() {
		if err := st.AddFile(ctx, FileEntry{
			TaskID: "t", URL: "http://a.onion/f" + string(rune('a'+i)),
			Filename: "f", Ext: "bin", Size: int64(i + 1), Verdict: v,
		}); err != nil {
			t.Fatal(err)
		}
	}

	got, err := st.SearchFiles(ctx, FileQuery{RiskOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(filex.RiskVerdicts) {
		t.Errorf("найдено %d, ожидала %d по списку filex.RiskVerdicts", len(got), len(filex.RiskVerdicts))
	}
	for _, f := range got {
		if !filex.VerdictIsRisk(f.Verdict) {
			t.Errorf("метка %q не считается рискованной в filex", f.Verdict)
		}
	}
}

func TestSearchFilesVerdictEmptyColumnNotMatched(t *testing.T) {
	// Строки, записанные до появления вердикта, имеют пустую колонку. Фильтр не
	// должен их ни находить, ни ломать запрос.
	st := newStore(t)
	ctx := context.Background()

	if err := st.AddFile(ctx, FileEntry{
		TaskID: "t", URL: "http://a.onion/old.pdf", Filename: "old.pdf", Ext: "pdf", Size: 10,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddFile(ctx, FileEntry{
		TaskID: "t", URL: "http://a.onion/new.pdf", Filename: "new.pdf", Ext: "pdf", Size: 20, Verdict: "document",
	}); err != nil {
		t.Fatal(err)
	}

	got, err := st.SearchFiles(ctx, FileQuery{Verdict: "document"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Filename != "new.pdf" {
		t.Errorf("найдено %d: %+v", len(got), got)
	}

	// Пустой вердикт не должен находиться ни по одной метке.
	got, err = st.SearchFiles(ctx, FileQuery{Verdict: "unknown"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("строка с пустой колонкой найдена по метке unknown: %+v", got)
	}
}

func TestSearchFilesVerdictLimit(t *testing.T) {
	// Limit применяется после фильтра, а не до: иначе отбор по метке возвращал
	// бы меньше записей, чем есть.
	st := newStore(t)
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		if err := st.AddFile(ctx, FileEntry{
			TaskID: "t", URL: "http://a.onion/f" + string(rune('a'+i)),
			Filename: "f", Ext: "epub", Size: int64(i + 1), Verdict: "ebook",
		}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.SearchFiles(ctx, FileQuery{Verdict: "ebook", Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Errorf("найдено %d, ожидала 3", len(got))
	}
	got, err = st.SearchFiles(ctx, FileQuery{Verdict: "ebook"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 10 {
		t.Errorf("без limit найдено %d, ожидала 10", len(got))
	}
}
