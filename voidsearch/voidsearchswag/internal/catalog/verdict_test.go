package catalog

import (
	"context"
	"testing"

	"voidsearchswag/internal/filex"
	"voidsearchswag/internal/store"
)

// TestCollectStoresVerdict проверяет сквозное заполнение колонки verdict.
//
// Тест нужен именно как сквозной, а не как проверка ClassifyFile отдельно:
// колонка verdict существовала с самого начала, но оба продакшн-места записи
// опускали поле при вставке, поэтому у всех строк каталога оно было пустым.
// Классификатор мог быть идеальным и при этом никак не влиять на базу. Без
// сквозной проверки потеря заполнения осталась бы невидимой.
func TestCollectStoresVerdict(t *testing.T) {
	st := newStore(t)
	f := &fakeClient{body: `<a href="/book.epub">e</a><a href="/doc.pdf">d</a>` +
		`<a href="/setup.exe">x</a><a href="/pack.zip">z</a>`}
	c := NewCollector(f, st, nil, nil, fastCfg(Config{}))

	rep, err := c.Collect(context.Background(), []string{v2a + ".onion"}, "task-verdict")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Saved != 4 {
		t.Fatalf("сохранено %d, ожидала 4", rep.Saved)
	}

	files, err := st.SearchFiles(context.Background(), store.FileQuery{TaskID: "task-verdict"})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 4 {
		t.Fatalf("в базе %d файлов, ожидала 4", len(files))
	}

	byExt := map[string]string{}
	for _, fl := range files {
		// Главный дефект: пустая колонка у всех строк.
		if fl.Verdict == "" {
			t.Errorf("у файла %s вердикт пустой", fl.Filename)
		}
		byExt[fl.Ext] = fl.Verdict
	}

	want := map[string]string{
		"epub": filex.VerdictEbook,
		"pdf":  filex.VerdictDocument,
		"exe":  filex.VerdictExecutable,
		"zip":  filex.VerdictArchive,
	}
	for ext, w := range want {
		if got := byExt[ext]; got != w {
			t.Errorf("у %s вердикт %q, ожидала %q", ext, got, w)
		}
	}
}

func TestCollectVerdictIsFilterable(t *testing.T) {
	// Конечный смысл заполнения колонки: по ней можно отбирать. Фильтр
	//RiskOnly и фильтр по метке обязаны работать на настоящих данных сбора, а
	// не только на вставленных вручную.
	st := newStore(t)
	f := &fakeClient{body: `<a href="/book.epub">e</a><a href="/setup.exe">x</a>` +
		`<a href="/keys.kdbx">k</a><a href="/doc.pdf">d</a>`}
	c := NewCollector(f, st, nil, nil, fastCfg(Config{}))

	if _, err := c.Collect(context.Background(), []string{v2a + ".onion"}, "task-filter"); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()

	ebooks, err := st.SearchFiles(ctx, store.FileQuery{TaskID: "task-filter", Verdict: "ebook"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ebooks) != 1 || ebooks[0].Ext != "epub" {
		t.Errorf("фильтр по метке ebook дал %d записей: %+v", len(ebooks), ebooks)
	}

	risky, err := st.SearchFiles(ctx, store.FileQuery{TaskID: "task-filter", RiskOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(risky) != 2 {
		t.Fatalf("фильтр риска дал %d записей, ожидала 2 (exe и kdbx)", len(risky))
	}
	for _, r := range risky {
		if !filex.VerdictIsRisk(r.Verdict) {
			t.Errorf("в рискованной выборке метка %q", r.Verdict)
		}
	}
}

// Фолбэк вердикта на имя файла, когда Ext пуст, покрыт юнит-тестом
// filex.TestClassifyRefFallsBackToFilename. На уровне каталога он не проверяется
// намеренно: fakeClient не умеет отдавать Content-Disposition, а расширять общий
// помощник ради одного теста значило бы задеть остальные.
