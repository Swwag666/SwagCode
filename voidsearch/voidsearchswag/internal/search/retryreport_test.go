package search

import (
	"testing"
)

func TestMergeRetryReportsNoDuplicates(t *testing.T) {
	// Живой прогон deep-режима показывал «движки: ahmia:fail, ahmia:fail,
	// ahmia-clear:fail» - три записи там, где движков два. Прежняя версия
	// дописывала отчёты повторного опроса к отчётам первой попытки.
	first := []EngineReport{
		{Name: "ahmia", OK: false, Error: "ссылок не найдено", Elapsed: "1.2s"},
		{Name: "torch", OK: false, Error: "таймаут", Elapsed: "30s"},
	}
	retry := []EngineReport{
		{Name: "ahmia", OK: false, Error: "ссылок не найдено", Elapsed: "0.9s"},
		{Name: "torch", OK: true, Count: 5, Elapsed: "8s"},
	}

	got := mergeRetryReports(first, retry)
	if len(got) != 2 {
		t.Fatalf("записей %d, ожидала 2: дубли не слиты", len(got))
	}

	byName := map[string]EngineReport{}
	for _, r := range got {
		if _, dup := byName[r.Name]; dup {
			t.Errorf("движок %s встречается дважды", r.Name)
		}
		byName[r.Name] = r
	}

	// Результат берётся из повторной попытки: она свежее.
	if !byName["torch"].OK || byName["torch"].Count != 5 {
		t.Errorf("torch не обновлён повтором: %+v", byName["torch"])
	}
	if byName["torch"].Elapsed != "8s" {
		t.Errorf("Elapsed у torch из первой попытки: %q", byName["torch"].Elapsed)
	}
	// Факт повтора сохраняется в обоих случаях: и когда повтор удался, и когда
	// снова не удался. Он объясняет, почему поиск занял дольше ожидаемого.
	if !byName["torch"].Retried {
		t.Error("у torch не отмечен повтор")
	}
	if !byName["ahmia"].Retried {
		t.Error("у ahmia не отмечен повтор")
	}
	if byName["ahmia"].OK {
		t.Error("ahmia помечен живым, хотя повтор тоже не удался")
	}
}

func TestMergeRetryReportsTakesFresherError(t *testing.T) {
	// Первая попытка могла лечь из-за битой цепи, которую сменили, поэтому её
	// текст уже не объясняет текущее состояние. Ошибка обязана быть из повтора.
	first := []EngineReport{{Name: "ahmia", OK: false, Error: "битая цепь tor"}}
	retry := []EngineReport{{Name: "ahmia", OK: false, Error: "ссылок не найдено"}}

	got := mergeRetryReports(first, retry)
	if got[0].Error != "ссылок не найдено" {
		t.Errorf("ошибка из первой попытки: %q", got[0].Error)
	}
}

func TestMergeRetryReportsAddsNewEngine(t *testing.T) {
	// Между опросами каталог мог пополнить фоновый промоут, поэтому движок из
	// повтора может отсутствовать в первой попытке. Его отчёт добавляется, а не
	// теряется.
	first := []EngineReport{{Name: "ahmia", OK: false, Error: "fail"}}
	retry := []EngineReport{
		{Name: "ahmia", OK: false, Error: "fail"},
		{Name: "promoted-engine", OK: true, Count: 3},
	}

	got := mergeRetryReports(first, retry)
	if len(got) != 2 {
		t.Fatalf("записей %d, ожидала 2", len(got))
	}
	found := false
	for _, r := range got {
		if r.Name == "promoted-engine" {
			found = true
			if !r.OK || r.Count != 3 {
				t.Errorf("отчёт нового движка искажён: %+v", r)
			}
			if !r.Retried {
				t.Error("у нового движка не отмечен повтор")
			}
		}
	}
	if !found {
		t.Error("отчёт нового движка потерян при слиянии")
	}
}

func TestMergeRetryReportsDoesNotMutateInput(t *testing.T) {
	// Отчёты уходят в JSON и в кэш, поэтому мутирование на месте испортило бы
	// уже собранные данные.
	first := []EngineReport{{Name: "ahmia", OK: false, Error: "fail"}}
	retry := []EngineReport{{Name: "ahmia", OK: true, Count: 7}}

	got := mergeRetryReports(first, retry)

	if first[0].OK || first[0].Retried {
		t.Errorf("входной срез first изменён: %+v", first[0])
	}
	if !retry[0].OK || retry[0].Retried {
		t.Errorf("входной срез retry изменён: %+v", retry[0])
	}
	if !got[0].OK || !got[0].Retried {
		t.Errorf("результат не собран: %+v", got[0])
	}
}

func TestMergeRetryReportsEmptyInputs(t *testing.T) {
	if got := mergeRetryReports(nil, nil); len(got) != 0 {
		t.Errorf("nil/nil дал %d записей", len(got))
	}
	if got := mergeRetryReports([]EngineReport{}, []EngineReport{}); len(got) != 0 {
		t.Errorf("пустые срезы дали %d записей", len(got))
	}

	// Пустой повтор не должен потерять отчёты первой попытки.
	first := []EngineReport{{Name: "ahmia", OK: false}}
	if got := mergeRetryReports(first, nil); len(got) != 1 || got[0].Retried {
		t.Errorf("пустой повтор исказил отчёт: %+v", got)
	}

	// Пустая первая попытка: отчёты повтора добавляются целиком.
	retry := []EngineReport{{Name: "torch", OK: true, Count: 2}}
	got := mergeRetryReports(nil, retry)
	if len(got) != 1 || !got[0].Retried || got[0].Count != 2 {
		t.Errorf("слияние с пустой первой попыткой: %+v", got)
	}
}

func TestMergeRetryReportsPreservesOrder(t *testing.T) {
	// Порядок первой попытки сохраняется: отчёт читается человеком, и
	// перестановка движков между прогонами мешала бы сравнивать результаты.
	first := []EngineReport{
		{Name: "ahmia", OK: false},
		{Name: "torch", OK: false},
		{Name: "tor66", OK: true, Count: 4},
	}
	retry := []EngineReport{
		{Name: "tor66", OK: true, Count: 6},
		{Name: "ahmia", OK: true, Count: 2},
		{Name: "torch", OK: false, Error: "fail"},
	}

	got := mergeRetryReports(first, retry)
	want := []string{"ahmia", "torch", "tor66"}
	for i, w := range want {
		if got[i].Name != w {
			t.Errorf("[%d] = %s, ожидала %s", i, got[i].Name, w)
		}
	}
	// Значения при этом берутся из повтора, несмотря на другой порядок.
	if got[2].Count != 6 {
		t.Errorf("tor66 Count = %d, ожидала 6 из повтора", got[2].Count)
	}
	if got[0].Count != 2 || !got[0].OK {
		t.Errorf("ahmia не обновлён повтором: %+v", got[0])
	}
}

func TestMergeRetryReportsFirstAttemptNotRetried(t *testing.T) {
	// Движки, которых не было в повторе, не должны получить флаг Retried:
	// иначе отчёт утверждал бы, что повтор был, когда его не было.
	first := []EngineReport{
		{Name: "ahmia", OK: false},
		{Name: "only-first", OK: true, Count: 1},
	}
	retry := []EngineReport{{Name: "ahmia", OK: false}}

	got := mergeRetryReports(first, retry)
	for _, r := range got {
		if r.Name == "only-first" && r.Retried {
			t.Error("движок без повтора помечен Retried")
		}
		if r.Name == "ahmia" && !r.Retried {
			t.Error("движок с повтором не помечен Retried")
		}
	}
}

func TestCountOKAfterMerge(t *testing.T) {
	// rep.Live считается по слитому отчёту, поэтому дубли не завышали бы его и
	// после слияния счётчик остаётся честным.
	first := []EngineReport{
		{Name: "ahmia", OK: false},
		{Name: "torch", OK: false},
	}
	retry := []EngineReport{
		{Name: "ahmia", OK: true, Count: 3},
		{Name: "torch", OK: true, Count: 2},
	}

	merged := mergeRetryReports(first, retry)
	if got := countOK(merged); got != 2 {
		t.Errorf("countOK = %d, ожидала 2", got)
	}
	// До слияния прежняя версия считала бы по списку из четырёх записей, где
	// две живые и две мёртвые, и rep.Live совпал бы случайно. Проверяю, что
	// слияние даёт именно два движка, а не четыре записи.
	if len(merged) != 2 {
		t.Errorf("записей после слияния %d, ожидала 2", len(merged))
	}
}
