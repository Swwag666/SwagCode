package main

import (
	"strings"
	"testing"
	"time"

	"voidsearchswag/internal/router"
	"voidsearchswag/internal/search"
	"voidsearchswag/internal/searchers"
)

// partialFailureOutcome собирает выдачу, где один движок ответил, а второй лёг с
// внятной причиной. Это штатная картина deep-режима без tor и штатная картина
// любого режима, когда один из эндпоинтов отвалился по таймауту.
func partialFailureOutcome(cached bool) *search.Outcome {
	return &search.Outcome{
		Query:    "leak database",
		Mode:     router.ModeDeep,
		UsedMode: router.ModeDeep,
		Count:    2,
		Cached:   cached,
		Duration: "12.4s",
		Limit:    5,
		Decision: router.Decision{Mode: router.ModeDeep, Reason: "запрос про утечки"},
		Report: search.Report{Engines: []search.EngineReport{
			{Name: "ddg-html", OK: true, Count: 2},
			{Name: "ahmia", OK: false, Error: "таймаут после 30s"},
			{Name: "ddg-lite", OK: false, Error: ""},
		}},
		Results: []searchers.Result{
			{Rank: 1, Title: "первый", URL: "https://one.example/1", Snippet: "коротко"},
			{Rank: 2, Title: "второй", URL: "https://two.example/2"},
		},
	}
}

// Замер до правки на HEAD 0f1a08b: при непустой выдаче упавший движок печатался
// как «ahmia:fail», и причина из отчёта не доходила до текста вовсе, хотя поле
// EngineReport.Error было заполнено. Причины печатались только при нулевой
// выдаче, поэтому частичный отказ - самый частый случай - оставался необъяснённым:
// пользователь видел fail и не знал, таймаут это, блокировка или отсутствие tor.
func TestPrintSearchOutcomeExplainsPartialFailure(t *testing.T) {
	out := partialFailureOutcome(false)
	text := captureStdout(t, func() { printSearchOutcome(out, 5, time.Hour) })

	if !strings.Contains(text, "движки: ") {
		t.Fatalf("в тексте нет строки движков:\n%s", text)
	}
	if !strings.Contains(text, "ddg-html:2") {
		t.Errorf("в тексте нет счёта живого движка:\n%s", text)
	}
	if !strings.Contains(text, "ahmia:fail") {
		t.Errorf("в тексте нет отметки отказа:\n%s", text)
	}

	if !strings.Contains(text, "таймаут после 30s") {
		t.Errorf("причина отказа движка не напечатана:\n%s", text)
	}
	if !strings.Contains(text, "без описания") {
		t.Errorf("движок без причины не отмечен как необъяснённый:\n%s", text)
	}
}

// Причины обязаны идти построчно и называть движок: строка «движки: ...»
// компактная, и причина, приклеенная к ней через запятую, сделала бы её
// нечитаемой на четырёх-пяти движках.
func TestPrintSearchOutcomeFailuresArePerLine(t *testing.T) {
	out := partialFailureOutcome(false)
	text := captureStdout(t, func() { printSearchOutcome(out, 5, time.Hour) })

	var reasons []string
	for _, ln := range strings.Split(text, "\n") {
		if strings.HasPrefix(ln, "  ahmia: ") || strings.HasPrefix(ln, "  ddg-lite: ") {
			reasons = append(reasons, strings.TrimSpace(ln))
		}
	}
	if len(reasons) != 2 {
		t.Fatalf("найдено %d строк причин, хочу 2:\n%s", len(reasons), text)
	}
	if !strings.Contains(reasons[0], "таймаут после 30s") {
		t.Errorf("первая строка причины: %q", reasons[0])
	}
	if !strings.Contains(reasons[1], "без описания") {
		t.Errorf("вторая строка причины: %q", reasons[1])
	}
}

// Отказ в кэшированной выдаче описывает прошлый прогон, поэтому строка движков
// помечена как запись кэша; причины обязаны печататься и там, иначе отметка
// есть, а объяснения нет.
func TestPrintSearchOutcomeExplainsCachedPartialFailure(t *testing.T) {
	out := partialFailureOutcome(true)
	text := captureStdout(t, func() { printSearchOutcome(out, 5, time.Hour) })

	if !strings.Contains(text, "движки (запись кэша): ") {
		t.Fatalf("в тексте нет отметки записи кэша:\n%s", text)
	}
	if !strings.Contains(text, "таймаут после 30s") {
		t.Errorf("причина отказа в кэшированной выдаче не напечатана:\n%s", text)
	}
}

// Контроль: при нулевой выдаче причины печатались и до правки. Они обязаны
// остаться, и ровно один раз - дубль на двух движках выглядел бы как четыре
// разных отказа.
func TestPrintSearchOutcomeEmptyFailureReasonsPrintedOnce(t *testing.T) {
	out := partialFailureOutcome(false)
	out.Count = 0
	out.Results = nil
	text := captureStdout(t, func() { printSearchOutcome(out, 5, time.Hour) })

	if !strings.Contains(text, "ничего не найдено") {
		t.Fatalf("в тексте нет объяснения пустой выдачи:\n%s", text)
	}
	if got := strings.Count(text, "таймаут после 30s"); got != 1 {
		t.Errorf("причина напечатана %d раз, хочу 1:\n%s", got, text)
	}
	if got := strings.Count(text, "без описания"); got != 1 {
		t.Errorf("«без описания» напечатано %d раз, хочу 1:\n%s", got, text)
	}
}

// Обратная сторона: когда все движки ответили, объяснений быть не должно, иначе
// строка «без описания» потеряет смысл и начнёт пугать на здоровом прогоне.
func TestPrintSearchOutcomeSilentWhenAllEnginesOK(t *testing.T) {
	out := partialFailureOutcome(false)
	out.Report.Engines = []search.EngineReport{
		{Name: "ddg-html", OK: true, Count: 2},
		{Name: "ddg-lite", OK: true, Count: 1},
	}
	text := captureStdout(t, func() { printSearchOutcome(out, 5, time.Hour) })

	if strings.Contains(text, "без описания") {
		t.Errorf("в здоровом прогоне напечатано «без описания»:\n%s", text)
	}
	if strings.Contains(text, ":fail") {
		t.Errorf("в здоровом прогоне напечатан отказ:\n%s", text)
	}
	if !strings.Contains(text, "ddg-html:2, ddg-lite:1") {
		t.Errorf("строка движков изменилась:\n%s", text)
	}
}

// Повторный опрос после смены цепи остаётся в компактной строке: это факт про
// попытку, а не про причину отказа.
func TestPrintSearchOutcomeKeepsRetryMark(t *testing.T) {
	out := partialFailureOutcome(false)
	out.Report.Engines[1].Retried = true
	text := captureStdout(t, func() { printSearchOutcome(out, 5, time.Hour) })

	if !strings.Contains(text, "(повтор после смены цепи)") {
		t.Errorf("в тексте нет отметки повтора:\n%s", text)
	}
	if !strings.Contains(text, "таймаут после 30s") {
		t.Errorf("причина отказа пропала рядом с отметкой повтора:\n%s", text)
	}
}

// Причина отказа усекается до ширины сниппета: сообщение про недоступный
// onion-адрес заканчивается самим адресом и в полном виде длиннее строки
// терминала, из-за чего список причин переносился и терял выравнивание.
func TestPrintEngineFailuresClipsLongReason(t *testing.T) {
	long := strings.Repeat("длинная причина отказа ", 20)
	engines := []search.EngineReport{{Name: "ahmia", OK: false, Error: long}}
	text := captureStdout(t, func() { printEngineFailures(engines) })

	line := strings.TrimRight(text, "\n")
	if !strings.HasSuffix(line, "...") {
		t.Errorf("длинная причина не усечена: %q", line)
	}
	if strings.Contains(line, long) {
		t.Error("длинная причина напечатана целиком")
	}
	// Два пробела отступа, имя движка, разделитель и не больше 163 символов
	// самой причины вместе с маркером усечения.
	if r := []rune(line); len(r) > 2+20+163 {
		t.Errorf("строка причины длиннее допуска: %d рун", len(r))
	}
}

// Короткая причина печатается дословно: усечение не должно добавлять многоточие
// там, где текст помещается целиком.
func TestPrintEngineFailuresKeepsShortReason(t *testing.T) {
	engines := []search.EngineReport{{Name: "ddg-lite", OK: false, Error: "таймаут"}}
	text := captureStdout(t, func() { printEngineFailures(engines) })

	if strings.Contains(text, "...") {
		t.Errorf("короткая причина усечена: %q", text)
	}
	if !strings.Contains(text, "ddg-lite: таймаут") {
		t.Errorf("причина напечатана не дословно: %q", text)
	}
}

// Здоровые движки в список причин не попадают: иначе строка «без описания»
// потеряла бы смысл и пугала на исправном прогоне.
func TestPrintEngineFailuresSkipsHealthyEngines(t *testing.T) {
	engines := []search.EngineReport{
		{Name: "ddg-html", OK: true, Count: 3},
		{Name: "ahmia", OK: false, Error: "таймаут"},
	}
	text := captureStdout(t, func() { printEngineFailures(engines) })

	if strings.Contains(text, "ddg-html") {
		t.Errorf("здоровый движок напечатан в причинах отказа: %q", text)
	}
	if got := strings.Count(text, "\n"); got != 1 {
		t.Errorf("напечатано %d строк, хочу одну: %q", got, text)
	}
	if !strings.Contains(text, "ahmia: таймаут") {
		t.Errorf("причина упавшего движка пропала: %q", text)
	}
}

// Пустой список движков не печатает ничего: функция вызывается и на выдаче без
// отчёта, и лишняя пустая строка сдвинула бы список результатов.
func TestPrintEngineFailuresSilentWithoutFailures(t *testing.T) {
	for name, engines := range map[string][]search.EngineReport{
		"nil":         nil,
		"пустой срез": {},
		"все здоровы": {{Name: "ddg-html", OK: true, Count: 3}, {Name: "ahmia", OK: true, Count: 1}},
	} {
		text := captureStdout(t, func() { printEngineFailures(engines) })
		if text != "" {
			t.Errorf("%s: напечатан текст %q", name, text)
		}
	}
}
