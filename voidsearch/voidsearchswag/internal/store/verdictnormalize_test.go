package store

import (
	"context"
	"fmt"
	"testing"
)

// Метка записи приходила в AddFile как есть, а фильтр вердиктов сравнивает
// нормализованный ввод с колонкой точно, чтобы пользоваться индексом
// idx_file_verdict. Замер до правки: запись с Verdict «EXECUTABLE» не находилась
// запросом Verdict: «executable», и пустой ответ был неотличим от «таких файлов
// нет». Приведение на стороне записи оставляет запрос индексным и убирает
// расхождение в корне.
func TestAddFileNormalizesVerdict(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"верхний регистр", "EXECUTABLE", "executable"},
		{"смешанный регистр", "Secret", "secret"},
		{"пробелы по краям", "  ebook\t", "ebook"},
		{"уже нормализовано", "document", "document"},
		{"пустая метка", "", ""},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			url := fmt.Sprintf("http://a.onion/f/%d.bin", i)
			if err := st.AddFile(ctx, FileEntry{
				TaskID: "t", URL: url, Filename: "f.bin", Ext: "bin", Size: 10, Verdict: c.in,
			}); err != nil {
				t.Fatalf("AddFile(%q): %v", c.in, err)
			}
			var got string
			if err := st.db.QueryRowContext(ctx,
				`SELECT verdict FROM file_catalog WHERE url = ?`, url).Scan(&got); err != nil {
				t.Fatalf("чтение метки: %v", err)
			}
			if got != c.want {
				t.Errorf("в базе verdict = %q, хочу %q", got, c.want)
			}
		})
	}
}

// Обе стороны фильтра обязаны встретиться: ввод в любом регистре находит запись,
// записанную в любом регистре.
func TestSearchFilesFindsVerdictWrittenInAnyCase(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	if err := st.AddFile(ctx, FileEntry{
		TaskID: "t", URL: "http://a.onion/f/setup.exe", Filename: "setup.exe",
		Ext: "exe", Size: 300, Verdict: "EXECUTABLE",
	}); err != nil {
		t.Fatalf("AddFile: %v", err)
	}
	if err := st.AddFile(ctx, FileEntry{
		TaskID: "t", URL: "http://a.onion/f/book.epub", Filename: "book.epub",
		Ext: "epub", Size: 100, Verdict: "ebook",
	}); err != nil {
		t.Fatalf("AddFile: %v", err)
	}

	for _, filter := range []string{"executable", "EXECUTABLE", " Executable "} {
		got, err := st.SearchFiles(ctx, FileQuery{Verdict: filter})
		if err != nil {
			t.Fatalf("SearchFiles(%q): %v", filter, err)
		}
		if len(got) != 1 {
			t.Errorf("фильтр %q нашёл %d записей, хочу 1", filter, len(got))
			continue
		}
		if got[0].Verdict != "executable" {
			t.Errorf("фильтр %q вернул запись с меткой %q", filter, got[0].Verdict)
		}
	}
}

// Перезапись существующего адреса нормализует метку так же, как первая запись:
// ON CONFLICT обновляет колонку значением из excluded.
func TestAddFileNormalizesVerdictOnConflict(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	url := "http://a.onion/f/keys.kdbx"
	for _, verdict := range []string{"secret", "SECRET"} {
		if err := st.AddFile(ctx, FileEntry{
			TaskID: "t", URL: url, Filename: "keys.kdbx", Ext: "kdbx", Size: 50, Verdict: verdict,
		}); err != nil {
			t.Fatalf("AddFile(%q): %v", verdict, err)
		}
	}

	got, err := st.SearchFiles(ctx, FileQuery{Verdict: "secret"})
	if err != nil {
		t.Fatalf("SearchFiles: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("найдено %d записей, хочу 1", len(got))
	}
	if got[0].Verdict != "secret" {
		t.Errorf("после перезаписи метка %q, хочу secret", got[0].Verdict)
	}
}
