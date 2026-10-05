package store

import (
	"context"
	"fmt"
	"testing"
)

// Потолок выдачи каталога обязан быть открытой константой: CLI проверяет --limit
// до запроса и печатает предел в сообщении об отказе, поэтому число не может
// жить только в теле SearchFiles. Замер до правки на HEAD 36e4cae, проба на
// временной базе из 1200 файлов: Limit=5000 вернул 1000 строк, Limit=1500 - 1000,
// Limit=1001 - 1000, Limit=1000 - 1000, Limit=999 - 999.
func TestSearchFilesCeilingIsMaxFileSearchLimit(t *testing.T) {
	if MaxFileSearchLimit <= 0 {
		t.Fatalf("MaxFileSearchLimit = %d, хочу положительное число", MaxFileSearchLimit)
	}
	st := newStore(t)
	ctx := context.Background()

	total := MaxFileSearchLimit + 200
	for i := 0; i < total; i++ {
		f := FileEntry{
			URL:      fmt.Sprintf("http://abcdefghijklmnop.onion/ceiling/%04d.bin", i),
			Filename: fmt.Sprintf("c%04d.bin", i),
			Ext:      "bin",
			Size:     10,
		}
		if err := st.AddFile(ctx, f); err != nil {
			t.Fatal(err)
		}
	}

	for _, limit := range []int{total * 2, MaxFileSearchLimit + 1, MaxFileSearchLimit} {
		got, err := st.SearchFiles(ctx, FileQuery{Limit: limit})
		if err != nil {
			t.Fatalf("SearchFiles(limit=%d): %v", limit, err)
		}
		if len(got) != MaxFileSearchLimit {
			t.Errorf("SearchFiles(limit=%d) вернул %d строк, хочу ровно %d",
				limit, len(got), MaxFileSearchLimit)
		}
	}

	below, err := st.SearchFiles(ctx, FileQuery{Limit: MaxFileSearchLimit - 1})
	if err != nil {
		t.Fatalf("SearchFiles(limit=%d): %v", MaxFileSearchLimit-1, err)
	}
	if len(below) != MaxFileSearchLimit-1 {
		t.Errorf("значение на единицу ниже потолка вернуло %d строк, хочу %d",
			len(below), MaxFileSearchLimit-1)
	}
}

// Значение под потолок не теряет строки: обрезка должна срабатывать только сверх
// предела, иначе потолок молча съел бы часть законной выборки.
func TestSearchFilesKeepsRowsBelowCeiling(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		f := FileEntry{
			URL:      fmt.Sprintf("http://abcdefghijklmnop.onion/small/%04d.bin", i),
			Filename: fmt.Sprintf("s%04d.bin", i),
			Ext:      "bin",
			Size:     10,
		}
		if err := st.AddFile(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	for _, limit := range []int{5, 6, MaxFileSearchLimit} {
		got, err := st.SearchFiles(ctx, FileQuery{Limit: limit})
		if err != nil {
			t.Fatalf("SearchFiles(limit=%d): %v", limit, err)
		}
		if len(got) != 5 {
			t.Errorf("SearchFiles(limit=%d) вернул %d строк, хочу все 5", limit, len(got))
		}
	}
}
