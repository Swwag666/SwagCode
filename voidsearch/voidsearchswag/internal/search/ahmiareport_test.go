package search

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"voidsearchswag/internal/router"
	"voidsearchswag/internal/searchers"
)

// recordingLogger собирает строки лога движка. Часть проверки идёт по логу: отказ
// моста виден и там, и в отчёте, и дубль имени портил оба канала.
type recordingLogger struct {
	mu    sync.Mutex
	lines []string
}

func (r *recordingLogger) Infof(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, fmt.Sprintf(format, args...))
}

func (r *recordingLogger) Warnf(format string, args ...any) { r.Infof(format, args...) }

func (r *recordingLogger) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.lines))
	copy(out, r.lines)
	return out
}

// ahmiaStub поднимает стенд моста: на главной отдаёт форму со скрытым anti-spam
// полем, потому что Search читает токен каждый раз, а на /search/ отвечает
// заданным статусом и телом.
func ahmiaStub(t *testing.T, status int, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if strings.HasPrefix(r.URL.Path, "/search/") {
			w.WriteHeader(status)
			w.Write([]byte(body))
			return
		}
		w.Write([]byte(`<html><body><form action="/search/" method="get">` +
			`<input type="hidden" name="s" value="tok"></form></body></html>`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// deepEngineWithAhmia собирает движок deep-режима, у которого onion-каталога нет,
// а мост смотрит на стенд. Кэш выключен: каждый прогон обязан опрашивать мост
// заново, иначе тест измерил бы запись прошлого прогона.
func deepEngineWithAhmia(t *testing.T, base string) (*Engine, *recordingLogger) {
	t.Helper()
	lg := &recordingLogger{}
	e, _ := newEngine(t)
	e.Ahmia = &searchers.AhmiaClear{Client: testDirectClient(t), BaseURL: base, Log: lg}
	e.Log = lg
	e.CacheTTL = 0
	e.DefaultN = 10
	return e, lg
}

// ahmiaReport достаёт запись моста из отчёта.
func ahmiaReport(t *testing.T, out *Outcome) EngineReport {
	t.Helper()
	for _, er := range out.Report.Engines {
		if er.Name == "ahmia-clear" {
			return er
		}
	}
	t.Fatalf("в отчёте нет движка ahmia-clear: %+v", out.Report.Engines)
	return EngineReport{}
}

// Этап 182: пустая выдача моста - не отказ. Прежний parseOnion возвращал
// «ссылок не найдено», и живой мост, честно не нашедший ничего по запросу,
// репортился упавшим (смоуки 179-181 стабильно: ok=false «ссылок не
// найдено» за 350мс). Прежние версии этого теста и
// TestDeepAhmiaFailureLoggedOnce держали обратный контракт - «пусто =
// фейл» - и переписаны: пустой ответ обязан приходить как ok=true,
// count=0, без причины отказа. Ошибкой остаются только транспорт и HTTP.
func TestDeepAhmiaNoLinksFailureHasNoDuplicatedName(t *testing.T) {
	base := ahmiaStub(t, http.StatusOK,
		`<html><body><div class="result">текст без единой ссылки</div></body></html>`)
	e, _ := deepEngineWithAhmia(t, base)

	out, err := e.Search(context.Background(), Options{Query: "leak database", Mode: router.ModeDeep})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	er := ahmiaReport(t, out)
	if !er.OK {
		t.Fatalf("мост отчитался отказом на пустой выдаче: %+v", er)
	}
	if er.Count != 0 {
		t.Errorf("count=%d, ожидала 0 на пустой выдаче", er.Count)
	}
	if er.Error != "" {
		t.Errorf("у пустого ответа заполнена причина отказа: %q", er.Error)
	}
}

// Вторая форма того же дефекта: отказ по статусу. Здесь имя в сообщении ставит
// сам движок, поэтому дубль появляется так же.
func TestDeepAhmiaHTTPFailureHasNoDuplicatedName(t *testing.T) {
	base := ahmiaStub(t, http.StatusServiceUnavailable, `<html><body>мост лежит</body></html>`)
	e, _ := deepEngineWithAhmia(t, base)

	out, err := e.Search(context.Background(), Options{Query: "leak database", Mode: router.ModeDeep})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	er := ahmiaReport(t, out)
	if er.OK {
		t.Fatalf("мост отчитался успехом при ответе 503: %+v", er)
	}
	if strings.HasPrefix(er.Error, "ahmia-clear:") {
		t.Errorf("имя движка продублировано в причине отказа: %q", er.Error)
	}
	if er.Error == "" {
		t.Error("причина отказа пустая: по отчёту не понять, что случилось")
	}
}

// Лог печатает имя движка сам, поэтому в строке лога оно обязано встречаться
// ровно один раз. Дубль в логе мешал сильнее, чем в отчёте: deep-прогон с
// --verbose и так печатает по строке на движок.
//
// Этап 182: прогон на пустой выдаче больше не пишет строку отказа вовсе -
// мост отработал успешно. Контроль дубля имени идёт на настоящем отказе
// (503), где строка отказа обязана существовать.
func TestDeepAhmiaFailureLoggedOnce(t *testing.T) {
	base := ahmiaStub(t, http.StatusServiceUnavailable, `<html><body>мост лежит</body></html>`)
	e, lg := deepEngineWithAhmia(t, base)

	if _, err := e.Search(context.Background(), Options{Query: "leak database", Mode: router.ModeDeep}); err != nil {
		t.Fatalf("search: %v", err)
	}

	lines := lg.snapshot()
	mentioned := 0
	for _, ln := range lines {
		if !strings.Contains(ln, "ahmia-clear") {
			continue
		}
		mentioned++
		if got := strings.Count(ln, "ahmia-clear"); got != 1 {
			t.Errorf("имя движка встречается %d раз в строке лога: %q", got, ln)
		}
		if !strings.Contains(ln, "503") {
			t.Errorf("в строке лога про отказ моста нет причины: %q", ln)
		}
	}
	if mentioned == 0 {
		t.Errorf("в логе нет ни одной строки про отказ моста: %v", lines)
	}
}

// Контроль: успешный прогон моста не обрастает объяснениями. Поле причины
// обязано остаться пустым, иначе пустая строка в отчёте выглядит как
// потерянное сообщение.
func TestDeepAhmiaSuccessKeepsErrorEmpty(t *testing.T) {
	base := ahmiaStub(t, http.StatusOK, `<html><body><ol>`+
		`<li class="result"><h4><a href="http://abcdefabcdefabcd.onion/docs">Onion docs</a></h4></li>`+
		`</ol></body></html>`)
	e, _ := deepEngineWithAhmia(t, base)

	out, err := e.Search(context.Background(), Options{Query: "leak database", Mode: router.ModeDeep})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	er := ahmiaReport(t, out)
	if !er.OK {
		t.Fatalf("мост не отработал на живой выдаче: %+v", er)
	}
	if er.Error != "" {
		t.Errorf("у успешного движка заполнена причина отказа: %q", er.Error)
	}
	if er.Count != 1 {
		t.Errorf("count=%d, ожидала 1", er.Count)
	}
}

// Контроль против починки ценой потери причины: engineErrText снимает только
// префикс имени, поэтому остальные ошибки моста обязаны доходить до отчёта
// дословно. Здесь мост смотрит на адрес, где никто не слушает, и отказывает
// транспорт.
func TestDeepAhmiaTransportFailureKeepsReason(t *testing.T) {
	e, _ := deepEngineWithAhmia(t, "http://127.0.0.1:1")

	out, err := e.Search(context.Background(), Options{Query: "leak database", Mode: router.ModeDeep})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	er := ahmiaReport(t, out)
	if er.OK {
		t.Fatalf("мост отчитался успехом на недоступном адресе: %+v", er)
	}
	if er.Error == "" {
		t.Fatal("причина отказа на недоступном адресе потеряна")
	}
	if strings.Count(er.Error, "ahmia-clear") > 1 {
		t.Errorf("имя движка продублировано: %q", er.Error)
	}
}
