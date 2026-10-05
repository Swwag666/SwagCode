package main

import (
	"errors"
	"strings"
	"testing"

	"voidsearchswag/internal/parser"
)

func parseValues() map[string]parser.Result {
	return map[string]parser.Result{
		"title": {Value: "", Strategy: "none", Confidence: 0, Healed: true},
		"price": {Value: "", Strategy: "none", Confidence: 0, Healed: true},
	}
}

func TestParseJSONPayloadCarriesError(t *testing.T) {
	// Главная проверка: ошибка разбора обязана быть в машинном выводе. Parse
	// возвращает непустую карту вместе с ошибкой «ни одно поле не извлечено»,
	// поэтому fatalf в cmdParse не срабатывает, и до правки JSON отдавал пустые
	// поля с healed=true и кодом возврата 0 при пустом stderr.
	payload := parseJSONPayload(parseValues(), true, "", errors.New("ни одно поле не извлечено"))

	msg, ok := payload["error"].(string)
	if !ok {
		t.Fatalf("в JSON нет поля error: %+v", payload)
	}
	if !strings.Contains(msg, "ни одно поле не извлечено") {
		t.Errorf("error = %q, ожидала причину разбора", msg)
	}
	if payload["fields"] == nil {
		t.Error("поля потеряны вместе с добавлением ошибки")
	}
	if payload["healed"] != true {
		t.Errorf("healed = %v, ожидала true", payload["healed"])
	}
}

func TestParseJSONPayloadNoErrorKeyOnSuccess(t *testing.T) {
	// Обратная сторона: при успешном разборе ключа error быть не должно, иначе
	// потребитель начнёт проверять его на пустоту вместо наличия.
	values := map[string]parser.Result{"title": {Value: "заголовок", Strategy: "css", Confidence: 1}}
	payload := parseJSONPayload(values, false, "", nil)

	if _, ok := payload["error"]; ok {
		t.Errorf("ключ error присутствует при успешном разборе: %+v", payload)
	}
	if payload["healed"] != false {
		t.Errorf("healed = %v, ожидала false", payload["healed"])
	}
}

func TestPrintParseOutcomeMarksEmptyFields(t *testing.T) {
	// Пустое значение печаталось как результат: «title [none 0.0]: » выглядело
	// извлечённым полем с пустым содержимым.
	out := captureStdout(t, func() {
		printParseOutcome([]string{"title", "price"}, parseValues(), true, "", errors.New("ни одно поле не извлечено"))
	})

	if !strings.Contains(out, "title: не извлечено") {
		t.Errorf("пустое поле не помечено:\n%s", out)
	}
	if !strings.Contains(out, "price: не извлечено") {
		t.Errorf("второе пустое поле не помечено:\n%s", out)
	}
	if strings.Contains(out, "[none 0.0]") {
		t.Errorf("пустое значение напечатано как результат со стратегией:\n%s", out)
	}
}

func TestPrintParseOutcomeHealedAdviceNeedsResults(t *testing.T) {
	// Совет перегенерировать селекторы при полном провале уводил от причины:
	// извлечено не было ничего, и селекторы здесь ни при чём.
	out := captureStdout(t, func() {
		printParseOutcome([]string{"title", "price"}, parseValues(), true, "", errors.New("ни одно поле не извлечено"))
	})
	if strings.Contains(out, "healed=true") {
		t.Errorf("совет про селекторы напечатан при нуле извлечённых полей:\n%s", out)
	}

	// Когда запасная стратегия хоть что-то дала, совет остаётся.
	values := map[string]parser.Result{
		"title": {Value: "заголовок", Strategy: "fuzzy", Confidence: 0.4, Healed: true},
		"price": {Value: "", Strategy: "none", Confidence: 0, Healed: true},
	}
	out = captureStdout(t, func() {
		printParseOutcome([]string{"title", "price"}, values, true, "", nil)
	})
	if !strings.Contains(out, "healed=true: часть полей взята запасной стратегией") {
		t.Errorf("совет про селекторы потерян при частичном успехе:\n%s", out)
	}
	if !strings.Contains(out, "title [fuzzy 0.4]: заголовок") {
		t.Errorf("извлечённое поле напечатано не в прежнем формате:\n%s", out)
	}
	if !strings.Contains(out, "price: не извлечено") {
		t.Errorf("пустое поле рядом с извлечённым не помечено:\n%s", out)
	}
}

func TestPrintParseOutcomeReportsErrorOnStderr(t *testing.T) {
	// Причина уходит в stderr и больше не называется предупреждением: разбор не
	// состоялся, и код возврата команды ненулевой.
	text := captureStderr(t, func() {
		captureStdout(t, func() {
			printParseOutcome([]string{"title"}, parseValues(), true, "", errors.New("ни одно поле не извлечено"))
		})
	})
	if !strings.Contains(text, "ERROR: parse: ни одно поле не извлечено") {
		t.Errorf("в stderr нет причины: %q", text)
	}
	if strings.Contains(text, "WARN") {
		t.Errorf("прежнее предупреждение осталось: %q", text)
	}
}

func TestPrintParseOutcomeSilentOnSuccess(t *testing.T) {
	values := map[string]parser.Result{"title": {Value: "заголовок", Strategy: "css", Confidence: 1}}
	text := captureStderr(t, func() {
		captureStdout(t, func() {
			printParseOutcome([]string{"title"}, values, false, "", nil)
		})
	})
	if text != "" {
		t.Errorf("при успешном разборе в stderr что-то напечатано: %q", text)
	}
}

const selectorWarn = "сохранённые селекторы хоста example.com не прочитаны, " +
	"разбор без них: database is closed"

func TestParseJSONPayloadCarriesWarning(t *testing.T) {
	// Деградация разбора не является ошибкой: значения извлекаются запасными
	// стратегиями, err остаётся nil, и до правки вызывающий не имел способа
	// узнать, что точные селекторы не прочитаны.
	values := map[string]parser.Result{
		"price": {Value: "1 990 ₽", Strategy: "structural", Confidence: 0.5, Healed: true},
	}
	payload := parseJSONPayload(values, true, selectorWarn, nil)

	warn, ok := payload["warning"].(string)
	if !ok {
		t.Fatalf("в JSON нет поля warning: %+v", payload)
	}
	if !strings.Contains(warn, "example.com") {
		t.Errorf("warning = %q, в нём нет хоста", warn)
	}
	if _, has := payload["error"]; has {
		t.Errorf("предупреждение превратилось в ошибку: %+v", payload)
	}
	if payload["healed"] != true {
		t.Errorf("healed = %v, ожидала true", payload["healed"])
	}
}

func TestParseJSONPayloadNoWarningKeyWhenClean(t *testing.T) {
	// Обратная сторона: ключа warning быть не должно, иначе потребитель начнёт
	// видеть деградацию в каждом ответе.
	values := map[string]parser.Result{"title": {Value: "заголовок", Strategy: "css", Confidence: 1}}
	payload := parseJSONPayload(values, false, "", nil)

	if _, ok := payload["warning"]; ok {
		t.Errorf("ключ warning присутствует при чистом разборе: %+v", payload)
	}
}

func TestParseJSONPayloadCarriesWarningAndError(t *testing.T) {
	// Предупреждение и ошибка независимы: разбор может не извлечь ни одного поля
	// и при этом сообщить, что база селекторов недоступна.
	payload := parseJSONPayload(parseValues(), true, selectorWarn, errors.New("ни одно поле не извлечено"))

	if _, ok := payload["warning"].(string); !ok {
		t.Errorf("warning потерян рядом с ошибкой: %+v", payload)
	}
	if _, ok := payload["error"].(string); !ok {
		t.Errorf("error потерян рядом с предупреждением: %+v", payload)
	}
}

func TestPrintParseOutcomeShowsWarningBeforeHealedAdvice(t *testing.T) {
	// Порядок обязателен: совет перегенерировать селекторы при недоступной базе
	// уводит от причины, поэтому объяснение идёт первым.
	values := map[string]parser.Result{
		"price": {Value: "1 990 ₽", Strategy: "structural", Confidence: 0.5, Healed: true},
	}
	out := captureStdout(t, func() {
		printParseOutcome([]string{"price"}, values, true, selectorWarn, nil)
	})

	if !strings.Contains(out, "внимание: "+selectorWarn) {
		t.Fatalf("предупреждение не напечатано:\n%s", out)
	}
	warnAt := strings.Index(out, "внимание:")
	adviceAt := strings.Index(out, "healed=true")
	if adviceAt < 0 {
		t.Fatalf("совет о селекторах потерян:\n%s", out)
	}
	if warnAt > adviceAt {
		t.Errorf("предупреждение напечатано после совета (позиции %d и %d):\n%s", warnAt, adviceAt, out)
	}
	if !strings.Contains(out, "price [structural 0.5]: 1 990 ₽") {
		t.Errorf("извлечённое поле напечатано не в прежнем формате:\n%s", out)
	}
}

func TestPrintParseOutcomeSilentWithoutWarning(t *testing.T) {
	values := map[string]parser.Result{"title": {Value: "заголовок", Strategy: "css", Confidence: 1}}
	out := captureStdout(t, func() {
		printParseOutcome([]string{"title"}, values, false, "", nil)
	})
	if strings.Contains(out, "внимание:") {
		t.Errorf("предупреждение напечатано при пустом warning:\n%s", out)
	}
}
