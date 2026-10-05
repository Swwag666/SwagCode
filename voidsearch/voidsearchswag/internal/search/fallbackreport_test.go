package search

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"voidsearchswag/internal/router"
	"voidsearchswag/internal/searchers"
)

// Живой замер до правки на HEAD d4c9f03: молчащий TCP-слушатель на 127.0.0.1:18999
// в роли socks-прокси, VOIDSEARCH_REQUEST_TIMEOUT=2s, транспорт static, копия
// боевой базы.
//
//	search test --limit 2 --no-cache
//	    rc=0, длилось 16184 мс
//	    stdout: «режим: fast | 0 результатов | 16.011s», два движка с
//	        elapsed 4.002s и ошибкой «ddg: context deadline exceeded»
//	search test --limit 2 --no-cache --json
//	    duration=16.011s, в report только engines из двух записей, ключей
//	    fallbacks нет
//
// Двенадцать секунд из шестнадцати не объясняло ничто: после пустой выдачи поиск
// ушёл в stealth и deep, оба отработали и исчезли из отчёта, потому что отчёт
// запасного режима не сохранялся.
func TestReportListsFallbacksWhenEveryModeEmpty(t *testing.T) {
	// Этап 162: сценарий фоллбэков переведён на авто-режим - явный mode с
	// этого этапа не подменяется. Роутер для «linux» выбирает fast, цепочка
	// запасных остаётся прежней: stealth, deep.
	empty := &fixedSearcher{name: "ddg", res: nil}
	e, _ := newEngine(t, empty)

	out, err := e.Search(context.Background(), Options{Query: "linux", Mode: "", Limit: 2, NoCache: true})
	if err != nil {
		t.Fatalf("поиск вернул ошибку: %v", err)
	}
	if len(out.Report.Fallbacks) != 2 {
		t.Fatalf("в отчёте %d записей о запасных режимах, хочу две (stealth и deep): %+v",
			len(out.Report.Fallbacks), out.Report.Fallbacks)
	}
	want := []string{"stealth", "deep"}
	for i, w := range want {
		got := out.Report.Fallbacks[i]
		if got.Mode != w {
			t.Errorf("запись %d: режим %q, хочу %q", i, got.Mode, w)
		}
		if got.Duration == "" {
			t.Errorf("запись %d: у запасного режима нет длительности", i)
		}
	}
	// Stealth опрашивает тот же clearnet-набор, deep без onion-каталога уходит в
	// clearnet сам, поэтому движков по одному в обоих режимах. Живыми считаются
	// ответившие движки, а не те, что дали результаты: пустая выдача при живом
	// движке - нормальный исход, и поле Live это отражает.
	if out.Report.Fallbacks[0].Engines != 1 {
		t.Errorf("stealth: движков %d, хочу 1", out.Report.Fallbacks[0].Engines)
	}
	if out.Report.Fallbacks[1].Engines != 1 {
		t.Errorf("deep: движков %d, хочу 1 через откат на clearnet", out.Report.Fallbacks[1].Engines)
	}
	if out.Report.Fallbacks[0].Live != 1 {
		t.Errorf("stealth: живых %d, хочу 1 - движок ответил, но пусто", out.Report.Fallbacks[0].Live)
	}
	if len(out.Results) != 0 {
		t.Errorf("выдача не пуста: %+v", out.Results)
	}
}

// Успешный запасной режим заменяет отчёт целиком, и список запасных режимов
// обязан при этом выжить: именно он объясняет, почему выдачу дали не с первой
// попытки и сколько стоили предыдущие.
//
// Браузерный движок отвечает только со второго вызова: первый приходится на
// исходный режим, второй - на запасной. Иначе fast и stealth неразличимы, оба
// опрашивают один и тот же clearnet-набор, и первый же режим дал бы выдачу.
func TestReportKeepsFallbacksAfterSuccessfulFallback(t *testing.T) {
	// Этап 162: авто-режим, роутер берёт fast и сам может передумать.
	empty := &fixedSearcher{name: "ddg", res: nil}
	e, _ := newEngine(t, empty)
	e.Browser = &nthSearcher{name: "browser", fromCall: 2, res: []searchers.Result{
		{URL: "https://b1.example/1", Title: "первый"},
		{URL: "https://b2.example/2", Title: "второй"},
	}}

	out, err := e.Search(context.Background(), Options{Query: "linux", Mode: "", Limit: 2, NoCache: true})
	if err != nil {
		t.Fatalf("поиск вернул ошибку: %v", err)
	}
	if out.Count == 0 {
		t.Fatal("запасной режим не дал выдачи, хотя браузерный движок отвечал со второго вызова")
	}
	if out.UsedMode != router.ModeStealth {
		t.Errorf("used_mode = %q, хочу %q: fast пуст, первый запасной - stealth", out.UsedMode, router.ModeStealth)
	}
	if len(out.Report.Fallbacks) != 1 {
		t.Fatalf("в отчёте %d записей о запасных режимах, хочу одну: %+v",
			len(out.Report.Fallbacks), out.Report.Fallbacks)
	}
	fb := out.Report.Fallbacks[0]
	if fb.Mode != string(router.ModeStealth) {
		t.Errorf("режим запасной записи %q, хочу %q", fb.Mode, router.ModeStealth)
	}
	if fb.Live == 0 {
		t.Errorf("у успешного запасного режима ноль живых движков: %+v", fb)
	}
	if fb.Duration == "" {
		t.Errorf("у успешного запасного режима нет длительности: %+v", fb)
	}
	if !out.Degraded {
		t.Error("выдача запасного режима не помечена деградацией")
	}
}

// Ключ fallbacks не должен появляться в обычном отчёте: запись кэша и JSON
// обычного прогона остаются прежней схемы, иначе каждый потребитель JSON увидел бы
// новое поле там, где запасных режимов не было вовсе.
func TestReportOmitsEmptyFallbacksFromJSON(t *testing.T) {
	filled := &fixedSearcher{name: "ddg", res: []searchers.Result{{URL: "https://f1.example/1"}}}
	e, _ := newEngine(t, filled)

	out, err := e.Search(context.Background(), Options{Query: "linux", Mode: router.ModeFast, Limit: 2, NoCache: true})
	if err != nil {
		t.Fatalf("поиск вернул ошибку: %v", err)
	}
	if len(out.Report.Fallbacks) != 0 {
		t.Fatalf("при непустой выдаче появились записи о запасных режимах: %+v", out.Report.Fallbacks)
	}
	raw, err := json.Marshal(out.Report)
	if err != nil {
		t.Fatalf("отчёт не сериализуется: %v", err)
	}
	if strings.Contains(string(raw), "fallbacks") {
		t.Errorf("пустой список запасных режимов попал в JSON: %s", raw)
	}
}

// Одна и та же выдача, собранная запасным режимом, обязана нести в JSON и ключ
// engines нового отчёта, и ключ fallbacks прежних попыток.
func TestReportJSONCarriesFallbacksAndEngines(t *testing.T) {
	// Этап 162: авто-режим, запасной stealth при пустом fast.
	empty := &fixedSearcher{name: "ddg", res: nil}
	e, _ := newEngine(t, empty)
	e.Browser = &nthSearcher{name: "browser", fromCall: 2, res: []searchers.Result{
		{URL: "https://b1.example/1", Title: "первый"},
	}}

	out, err := e.Search(context.Background(), Options{Query: "linux", Mode: "", Limit: 2, NoCache: true})
	if err != nil {
		t.Fatalf("поиск вернул ошибку: %v", err)
	}
	raw, err := json.Marshal(out.Report)
	if err != nil {
		t.Fatalf("отчёт не сериализуется: %v", err)
	}
	var decoded struct {
		Engines   []EngineReport   `json:"engines"`
		Fallbacks []FallbackReport `json:"fallbacks"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("отчёт не разбирается: %v, JSON: %s", err, raw)
	}
	if len(decoded.Engines) == 0 {
		t.Errorf("в JSON нет движков: %s", raw)
	}
	if len(decoded.Fallbacks) != 1 {
		t.Errorf("в JSON %d записей о запасных режимах, хочу одну: %s", len(decoded.Fallbacks), raw)
	}
	if len(decoded.Fallbacks) == 1 && decoded.Fallbacks[0].Mode != string(router.ModeStealth) {
		t.Errorf("в JSON режим запасной записи %q, хочу %q", decoded.Fallbacks[0].Mode, router.ModeStealth)
	}
	if len(decoded.Fallbacks) == 1 && decoded.Fallbacks[0].Duration == "" {
		t.Errorf("в JSON у запасной записи нет длительности: %s", raw)
	}
}

// nthSearcher отвечает только начиная с вызова номер fromCall, а до него отдаёт
// пусто. Нужен, чтобы различить исходный режим и запасной: оба опрашивают один и
// тот же clearnet-набор, и движок, который отвечает всегда, закрывает вопрос на
// первом же режиме.
type nthSearcher struct {
	name     string
	fromCall int
	res      []searchers.Result
	calls    int
}

func (n *nthSearcher) Name() string { return n.name }

func (n *nthSearcher) Search(context.Context, string, int) ([]searchers.Result, error) {
	n.calls++
	if n.calls < n.fromCall {
		return nil, nil
	}
	return n.res, nil
}
