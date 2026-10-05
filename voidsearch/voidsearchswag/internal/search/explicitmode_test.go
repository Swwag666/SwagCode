package search

import (
	"context"
	"errors"
	"testing"

	"voidsearchswag/internal/router"
	"voidsearchswag/internal/searchers"
)

var errDeadEngine = errors.New("движок мёртв")

// Этап 162. Живой замер ДО (смоук трёх независимых агентов, сервер
// v0.1.0 c00d86b): юзер явно просил mode=fast, движки clearnet лежали -
// сервер тихо подменил режим на deep (tor) с used_mode=deep,
// degraded=true и reason «режим задан явно -> фоллбэк на deep».
// Просящий «не ходить в tor» получил запрос в tor. Явный режим -
// закон: пустая выдача или мёртвые движки дают честный пустой ответ,
// а не смену транспорта.
func TestExplicitModeIsLawWhenEnginesDead(t *testing.T) {
	dead := &fixedSearcher{name: "ddg", res: nil, err: errDeadEngine}
	e, _ := newEngine(t, dead)
	// Браузерный движок отвечает только с третьего вызова: первый - запасной
	// браузер внутри fast, второй - stealth, третий - deep, который без
	// onion-каталога откатывается на clearnet. Именно так старый код
	// дотягивался до deep-выдачи при явном fast: мёртвый ddg, browser молчит
	// дважды, а на третьем вызове мусорная ссылка выдавалась за ответ fast.
	e.Browser = &nthSearcher{name: "browser", fromCall: 3, res: []searchers.Result{
		{URL: "https://deep.example/1", Title: "мусор"},
	}}

	out, err := e.Search(context.Background(), Options{Query: "x", Mode: router.ModeFast, Limit: 5, NoCache: true})
	if err != nil {
		t.Fatalf("поиск вернул ошибку: %v", err)
	}
	if out.UsedMode != router.ModeFast {
		t.Fatalf("used_mode = %q: явный fast подменён, а режим - закон", out.UsedMode)
	}
	if len(out.Report.Fallbacks) != 0 {
		t.Fatalf("запасные режимы запущены при явном fast: %+v", out.Report.Fallbacks)
	}
	if out.Degraded {
		t.Error("пустой ответ явного режима помечен деградацией: подмены нет - деградации нет")
	}
	if out.Count != 0 {
		t.Errorf("выдача явного режима не пуста: %+v", out.Results)
	}
	if out.Report.Note == "" {
		t.Error("пустой ответ явного режима без пояснения: юзер должен видеть, что режим не подменялся")
	}
}

// Живые движки с пустой выдачей - тот же закон: «не нашлось» не повод
// менять транспорт. До правки пустая выдача явного fast запускала
// цепочку stealth -> deep.
func TestExplicitModeIsLawWhenEnginesEmpty(t *testing.T) {
	empty := &fixedSearcher{name: "ddg", res: nil}
	e, _ := newEngine(t, empty)
	e.Browser = &nthSearcher{name: "browser", fromCall: 3, res: []searchers.Result{
		{URL: "https://deep.example/1", Title: "мусор"},
	}}

	out, err := e.Search(context.Background(), Options{Query: "x", Mode: router.ModeFast, Limit: 5, NoCache: true})
	if err != nil {
		t.Fatalf("поиск вернул ошибку: %v", err)
	}
	if out.UsedMode != router.ModeFast {
		t.Fatalf("used_mode = %q: пустая выдача не повод менять режим", out.UsedMode)
	}
	if len(out.Report.Fallbacks) != 0 {
		t.Fatalf("запасные режимы запущены при пустой выдаче явного fast: %+v", out.Report.Fallbacks)
	}
}

// Авто-режим фоллбэки сохраняет: там их и место, роутер сам выбрал и
// сам может передумать. До правки это поведение принадлежало и явным
// режимам.
func TestAutoModeKeepsFallbacks(t *testing.T) {
	empty := &fixedSearcher{name: "ddg", res: nil}
	e, _ := newEngine(t, empty)
	e.Browser = &nthSearcher{name: "browser", fromCall: 2, res: []searchers.Result{
		{URL: "https://b1.example/1", Title: "первый"},
	}}

	out, err := e.Search(context.Background(), Options{Query: "linux", Mode: "", Limit: 2, NoCache: true})
	if err != nil {
		t.Fatalf("поиск вернул ошибку: %v", err)
	}
	if out.UsedMode != router.ModeStealth {
		t.Fatalf("авто-режим не дошёл до запасного stealth: used_mode=%q", out.UsedMode)
	}
	if !out.Degraded {
		t.Error("выдача запасного режима не помечена деградацией")
	}
}
