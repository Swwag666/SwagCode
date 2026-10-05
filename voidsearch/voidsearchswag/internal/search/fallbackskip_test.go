package search

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"voidsearchswag/internal/router"
	"voidsearchswag/internal/searchers"
)

// Живой замер до правки на HEAD 49e5da6, молчащий TCP-слушатель на 127.0.0.1:18999
// в роли socks-прокси, транспорт static, VOIDSEARCH_REQUEST_TIMEOUT=2s, копия
// боевой базы:
//
//	search test --limit 2 --no-cache --json
//	    rc=0, прогон длился 17167 мс
//	    duration=16.029s, count=0, used_mode=fast, note пусто
//	    engine ddg-html: ok=false elapsed=4.004s error=ddg: context deadline exceeded
//	    engine ddg-lite: ok=false elapsed=4.002s error=ddg: context deadline exceeded
//	    fallbacks: stealth 4.001s engines=2 live=0 | deep 8.018s engines=6 live=0
//
// Двенадцать секунд из шестнадцати ушли на повтор того же отказа: выход в сеть не
// работал, и ни stealth, ни deep не имели шанса.
func TestFallbacksSkippedWhenAllEnginesTimedOut(t *testing.T) {
	// Этап 162: сценарии этого файла переведены на авто-режим (Mode ""),
	// потому что явный mode с этого этапа не подменяется вовсе. Роутер для
	// «linux» выбирает fast, поведение фоллбэков прежнее.
	timedOut := &fixedSearcher{name: "ddg", err: context.DeadlineExceeded}
	e, _ := newEngine(t, timedOut)

	start := time.Now()
	out, err := e.Search(context.Background(), Options{Query: "linux", Mode: "", Limit: 2, NoCache: true})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("поиск вернул ошибку: %v", err)
	}
	if len(out.Report.Fallbacks) != 0 {
		t.Errorf("запасные режимы запустились при мёртвом транспорте: %+v", out.Report.Fallbacks)
	}
	if !strings.Contains(out.Report.Note, "запасные режимы не запускались") {
		t.Errorf("примечание не объясняет пропуск запасных режимов: %q", out.Report.Note)
	}
	if !out.Report.Degraded {
		t.Error("пропуск запасных режимов не помечен деградацией")
	}
	if out.UsedMode != router.ModeFast {
		t.Errorf("used_mode = %q, хочу %q: запасные режимы не запускались", out.UsedMode, router.ModeFast)
	}
	if elapsed > 5*time.Second {
		t.Errorf("поиск с мёртвым транспортом длился %s", elapsed)
	}
	if len(out.Report.Engines) != 1 {
		t.Fatalf("в отчёте %d движков, хочу один", len(out.Report.Engines))
	}
	if !out.Report.Engines[0].Timeout {
		t.Errorf("движок не помечен легшим по сроку: %+v", out.Report.Engines[0])
	}
}

// Отказ по другой причине обязан оставлять запасные режимы в силе: движок мог
// отказать из-за блокировки или поломки разметки, а другой режим при этом работает.
func TestFallbacksRunWhenEnginesFailForOtherReason(t *testing.T) {
	refused := &fixedSearcher{name: "ddg", err: errors.New("отказ соединения")}
	e, _ := newEngine(t, refused)

	out, err := e.Search(context.Background(), Options{Query: "linux", Mode: "", Limit: 2, NoCache: true})
	if err != nil {
		t.Fatalf("поиск вернул ошибку: %v", err)
	}
	if len(out.Report.Fallbacks) != 2 {
		t.Errorf("в отчёте %d записей о запасных режимах, хочу две (stealth и deep): %+v",
			len(out.Report.Fallbacks), out.Report.Fallbacks)
	}
	if out.Report.Engines[0].Timeout {
		t.Errorf("обычный отказ помечен сроком: %+v", out.Report.Engines[0])
	}
	if strings.Contains(out.Report.Note, "запасные режимы не запускались") {
		t.Errorf("примечание объявило пропуск запасных режимов при живом транспорте: %q", out.Report.Note)
	}
}

// Смешанный исход: один движок лёг по сроку, другой отказал по своей причине.
// Транспорт в этом случае не считается мёртвым, и запасные режимы обязаны
// отработать - иначе один зависший движок навсегда отключал бы попытку сменить
// режим.
func TestFallbacksRunWhenOnlyPartOfEnginesTimedOut(t *testing.T) {
	timedOut := &fixedSearcher{name: "ddg-html", err: context.DeadlineExceeded}
	refused := &fixedSearcher{name: "ddg-lite", err: errors.New("отказ соединения")}
	e, _ := newEngine(t, timedOut, refused)

	out, err := e.Search(context.Background(), Options{Query: "linux", Mode: "", Limit: 2, NoCache: true})
	if err != nil {
		t.Fatalf("поиск вернул ошибку: %v", err)
	}
	if len(out.Report.Fallbacks) != 2 {
		t.Errorf("в отчёте %d записей о запасных режимах, хочу две: %+v",
			len(out.Report.Fallbacks), out.Report.Fallbacks)
	}
	var timed, refusedCount int
	for _, er := range out.Report.Engines {
		if er.Timeout {
			timed++
		} else {
			refusedCount++
		}
	}
	if timed != 1 || refusedCount != 1 {
		t.Errorf("признак срока расставлен неверно: по сроку %d, по другой причине %d, отчёт %+v",
			timed, refusedCount, out.Report.Engines)
	}
}

// Пустая выдача без единого отказа - это ответ «ничего не нашлось», а не поломка
// транспорта: запасные режимы обязаны отработать.
func TestFallbacksRunWhenEnginesAnsweredEmpty(t *testing.T) {
	empty := &fixedSearcher{name: "ddg", res: nil}
	e, _ := newEngine(t, empty)

	out, err := e.Search(context.Background(), Options{Query: "linux", Mode: "", Limit: 2, NoCache: true})
	if err != nil {
		t.Fatalf("поиск вернул ошибку: %v", err)
	}
	if len(out.Report.Fallbacks) != 2 {
		t.Errorf("в отчёте %d записей о запасных режимах, хочу две: %+v",
			len(out.Report.Fallbacks), out.Report.Fallbacks)
	}
	if out.Report.Engines[0].Timeout {
		t.Errorf("движок, ответивший пустотой, помечен сроком: %+v", out.Report.Engines[0])
	}
}

func TestAllEnginesTimedOut(t *testing.T) {
	cases := []struct {
		name    string
		engines []EngineReport
		want    bool
	}{
		{"пустой отчёт", nil, false},
		{"все по сроку", []EngineReport{{Name: "a", Timeout: true}, {Name: "b", Timeout: true}}, true},
		{"один не по сроку", []EngineReport{{Name: "a", Timeout: true}, {Name: "b"}}, false},
		{"никто не по сроку", []EngineReport{{Name: "a"}, {Name: "b"}}, false},
		{"живой движок среди лёгших", []EngineReport{{Name: "a", Timeout: true}, {Name: "b", OK: true}}, false},
	}
	for _, c := range cases {
		if got := allEnginesTimedOut(c.engines); got != c.want {
			t.Errorf("%s: allEnginesTimedOut = %v, хочу %v", c.name, got, c.want)
		}
	}
}

// Примечание deep-режима уже несёт объяснение деградации, и пропуск запасных
// режимов обязан добавиться к нему, а не затереть.
func TestJoinNoteKeepsBothParts(t *testing.T) {
	if got := joinNote("", "вторая"); got != "вторая" {
		t.Errorf("пустое примечание: %q, хочу %q", got, "вторая")
	}
	got := joinNote("onion-выдача пуста, отработал clearnet", "запасные режимы не запускались")
	want := "onion-выдача пуста, отработал clearnet; запасные режимы не запускались"
	if got != want {
		t.Errorf("склейка примечаний: %q, хочу %q", got, want)
	}
	if joinCacheNote("кэш не прочитан", "запись не удалась") != "кэш не прочитан; запись не удалась" {
		t.Error("оговорки о кэше разошлись с общим правилом склейки")
	}
}

// Движок, не успевший ответить к истечению срока вызывающего, тоже обязан нести
// признак срока: иначе один такой движок снял бы блокировку запасных режимов и
// поиск снова ждал бы мёртвый транспорт.
func TestUnansweredEngineMarkedTimeout(t *testing.T) {
	hung := &blockingSearcher{name: "hung", delay: 5 * time.Second}
	e, _ := newEngine(t, hung)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, reports := e.querySearchersParallel(ctx, []searchers.Searcher{hung}, "q", 2)
	if len(reports) != 1 {
		t.Fatalf("в отчёте %d движков, хочу один", len(reports))
	}
	if !reports[0].Timeout {
		t.Errorf("не успевший движок не помечен сроком: %+v", reports[0])
	}
	if reports[0].Error == "" {
		t.Errorf("у не успевшего движка нет причины: %+v", reports[0])
	}
}
