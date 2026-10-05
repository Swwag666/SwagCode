package store

import (
	"context"
	"testing"
)

// Текстовый фильтр каталога обязан находить файл по странице, на которой он
// висел. Имена файлов в собранных каталогах машинные (0a1b.bin, f001.dat),
// поэтому адрес страницы нередко единственный осмысленный признак группы:
// «покажи всё с /leaks/db-dump». Команда files печатает строку
// «страница: ...» в каждой записи выдачи, но отобрать по ней не давала -
// Text сравнивался только с filename и url. Тот же параметр приходит из
// MCP-инструмента поиска по каталогу, поэтому чинится он в хранилище.
func TestSearchFilesMatchesSourcePage(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	add := func(url, name, ext, page string) {
		t.Helper()
		f := FileEntry{TaskID: "t", URL: url, Filename: name, Ext: ext, Size: 100, SourcePage: page}
		if err := st.AddFile(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	add("http://abc.onion/f/0a1b.bin", "0a1b.bin", "bin", "http://abc.onion/leaks/db-dump/")
	add("http://abc.onion/f/photo.jpg", "photo.jpg", "jpg", "http://abc.onion/gallery/")

	got, err := st.SearchFiles(ctx, FileQuery{Text: "leaks"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Filename != "0a1b.bin" {
		t.Fatalf("поиск по странице-источнику вернул %v, хочу одну запись 0a1b.bin", got)
	}

	// Поиск по имени и по адресу файла обязан работать как раньше: новое поле
	// добавляет совпадения, а не заменяет прежние.
	for _, text := range []string{"photo", "0a1b.bin", "gallery", "abc.onion"} {
		got, err := st.SearchFiles(ctx, FileQuery{Text: text})
		if err != nil {
			t.Fatal(err)
		}
		if text == "abc.onion" {
			if len(got) != 2 {
				t.Errorf("поиск %q вернул %d записей, хочу 2", text, len(got))
			}
			continue
		}
		if len(got) != 1 {
			t.Errorf("поиск %q вернул %d записей, хочу 1", text, len(got))
		}
	}
}

// Символы шаблона SQL остаются данными и в новом поле: запрос из одного «%» не
// должен находить всё подряд, иначе фильтр превращается в его отсутствие.
func TestSearchFilesSourcePageEscapesWildcards(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	for _, f := range []FileEntry{
		{TaskID: "t", URL: "http://abc.onion/f/a.bin", Filename: "a.bin", Ext: "bin", Size: 10, SourcePage: "http://abc.onion/100%pass/"},
		{TaskID: "t", URL: "http://abc.onion/f/b.bin", Filename: "b.bin", Ext: "bin", Size: 20, SourcePage: "http://abc.onion/plain/"},
	} {
		if err := st.AddFile(ctx, f); err != nil {
			t.Fatal(err)
		}
	}

	got, err := st.SearchFiles(ctx, FileQuery{Text: "%"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Filename != "a.bin" {
		t.Errorf("запрос «%%» вернул %v: символ шаблона в странице-источнике не экранирован", got)
	}

	got, err = st.SearchFiles(ctx, FileQuery{Text: "%pass"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Filename != "a.bin" {
		t.Errorf("буквальная подстрока страницы не найдена: %v", got)
	}
}
