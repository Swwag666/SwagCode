package searchers

import (
	"errors"
	"strings"
	"testing"
)

func TestCombineSourceResultsFirstWins(t *testing.T) {
	// Первый источник дал выдачу - второй не нужен, и его ошибка не должна
	// портить результат.
	first := []Result{{URL: "http://a.onion", Title: "a"}}
	got, err := combineSourceResults(first, nil, nil, errors.New("bing упал"))
	if err != nil {
		t.Errorf("ошибка второго источника испортила успешный первый: %v", err)
	}
	if len(got) != 1 || got[0].URL != "http://a.onion" {
		t.Errorf("выдача первого источника потеряна: %+v", got)
	}
}

func TestCombineSourceResultsFirstErrorSecondEmptyIsError(t *testing.T) {
	// Ключевой дефект, который правка устраняет.
	//
	// Прежняя версия возвращала (пусто, nil) в ситуации «DDG упал, Bing ответил
	// успешно, но без результатов». Вызывающий видел «результатов нет» вместо
	// «источник не сработал». Для отчёта о движках это принципиально: fail
	// учитывается в статистике здоровья и запускает повтор со сменой цепи, а
	// пустой ответ считается нормальным исходом.
	got, err := combineSourceResults(nil, errors.New("ddg: таймаут"), nil, nil)
	if err == nil {
		t.Fatal("ошибка первого источника потеряна при пустом втором")
	}
	if got != nil {
		t.Errorf("при ошибке вернулась выдача: %+v", got)
	}
	if !strings.Contains(err.Error(), "ddg") {
		t.Errorf("ошибка не называет источник: %v", err)
	}
	if !strings.Contains(err.Error(), "таймаут") {
		t.Errorf("ошибка потеряла исходную причину: %v", err)
	}
	if !strings.Contains(err.Error(), "без результатов") {
		t.Errorf("ошибка не объясняет, что второй источник ответил пусто: %v", err)
	}
}

func TestCombineSourceResultsFirstErrorSecondHasResults(t *testing.T) {
	// Если второй источник дал настоящую выдачу, её нужно вернуть: пользователь
	// получает результаты, а сбой первого источника остаётся в логе.
	second := []Result{{URL: "http://b.onion", Title: "b"}}
	got, err := combineSourceResults(nil, errors.New("ddg упал"), second, nil)
	if err != nil {
		t.Errorf("выдача второго источника отброшена из-за ошибки первого: %v", err)
	}
	if len(got) != 1 || got[0].URL != "http://b.onion" {
		t.Errorf("выдача второго источника потеряна: %+v", got)
	}
}

func TestCombineSourceResultsBothErrors(t *testing.T) {
	// Обе причины обязаны попасть в ошибку: одна не объясняет, почему не
	// сработал второй источник.
	got, err := combineSourceResults(nil, errors.New("ddg сбой"), nil, errors.New("bing сбой"))
	if err == nil {
		t.Fatal("обе ошибки пропали")
	}
	if got != nil {
		t.Errorf("при ошибках вернулась выдача: %+v", got)
	}
	if !strings.Contains(err.Error(), "ddg сбой") {
		t.Errorf("первая причина потеряна: %v", err)
	}
	if !strings.Contains(err.Error(), "bing сбой") {
		t.Errorf("вторая причина потеряна: %v", err)
	}
}

func TestCombineSourceResultsOnlySecondError(t *testing.T) {
	// Первый ответил успешно и пусто, второй упал: ошибка второго - единственная
	// причина отсутствия результатов, и она обязана дойти до вызывающего.
	got, err := combineSourceResults(nil, nil, nil, errors.New("bing недоступен"))
	if err == nil {
		t.Fatal("ошибка второго источника потеряна")
	}
	if got != nil {
		t.Errorf("при ошибке вернулась выдача: %+v", got)
	}
	if !strings.Contains(err.Error(), "bing недоступен") {
		t.Errorf("причина искажена: %v", err)
	}
	// Первый источник не виноват, поэтому его имя в ошибке не нужно: иначе
	// сообщение намекало бы на сбой там, где его не было.
	if strings.Contains(err.Error(), "ddg") {
		t.Errorf("ошибка обвиняет первый источник, который ответил успешно: %v", err)
	}
}

func TestCombineSourceResultsBothEmptyIsHonestEmpty(t *testing.T) {
	// Оба источника ответили успешно и оба пусты. Это честный пустой результат,
	// а не ошибка: браузер сработал, выдача действительно пуста. Превращение
	// такого исхода в ошибку означало бы, что поисковик «падает» на запросе,
	// которого в индексе нет.
	got, err := combineSourceResults(nil, nil, nil, nil)
	if err != nil {
		t.Errorf("честный пустой результат превращён в ошибку: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ожидала пустую выдачу, получила %+v", got)
	}
}

func TestCombineSourceResultsEmptySlicesTreatedAsNoResults(t *testing.T) {
	// Пустой срез и nil обязаны трактоваться одинаково: parseDDG и parseBing
	// возвращают пустой срез, когда выдача не найдена, и различение nil от
	// пустого среза дало бы разное поведение на одном и том же исходе.
	empty := []Result{}
	got, err := combineSourceResults(empty, errors.New("ddg сбой"), empty, nil)
	if err == nil {
		t.Error("пустой срез второго источника принят за выдачу, ошибка первого потеряна")
	}
	if len(got) != 0 {
		t.Errorf("ожидала пустую выдачу, получила %+v", got)
	}
}

func TestCombineSourceResultsDoesNotMutateInputs(t *testing.T) {
	// Результаты кэшируются выше по стеку, поэтому функция не имеет права
	// менять входные срезы.
	first := []Result{{URL: "http://a.onion", Title: "a"}}
	second := []Result{{URL: "http://b.onion", Title: "b"}}
	firstCopy := append([]Result(nil), first...)
	secondCopy := append([]Result(nil), second...)

	combineSourceResults(first, nil, second, nil)

	for i := range first {
		if first[i] != firstCopy[i] {
			t.Errorf("первый вход изменён на позиции %d", i)
		}
	}
	for i := range second {
		if second[i] != secondCopy[i] {
			t.Errorf("второй вход изменён на позиции %d", i)
		}
	}
}

func TestCombineSourceResultsErrorIsWrapped(t *testing.T) {
	// Ошибка второго источника обязана быть обёрнута через %w, чтобы вызывающий
	// мог проверить её через errors.Is: по тексту ошибки тип сбоя не определить.
	sentinel := errors.New("маркер")
	_, err := combineSourceResults(nil, nil, nil, sentinel)
	if !errors.Is(err, sentinel) {
		t.Errorf("ошибка второго источника не распаковывается через errors.Is: %v", err)
	}
}

func TestCombineSourceResultsAllCombinationsNeverPanics(t *testing.T) {
	// Перебор всех комбинаций: функция обязана вернуть либо выдачу, либо ошибку,
	// и никогда не паниковать на nil-срезах.
	errA := errors.New("a")
	res := []Result{{URL: "http://x.onion"}}

	errs := []error{nil, errA}
	results := [][]Result{nil, res}

	for _, e1 := range errs {
		for _, r1 := range results {
			for _, e2 := range errs {
				for _, r2 := range results {
					got, err := combineSourceResults(r1, e1, r2, e2)
					// Инвариант: успешная выдача и ошибка не возвращаются
					// одновременно - вызывающий не должен угадывать, чему верить.
					if err == nil && len(got) == 0 && (len(r1) > 0 || len(r2) > 0) {
						t.Errorf("выдача потеряна: r1=%d r2=%d e1=%v e2=%v",
							len(r1), len(r2), e1, e2)
					}
					if err != nil && len(got) > 0 {
						t.Errorf("одновременно ошибка и выдача: %+v / %v", got, err)
					}
				}
			}
		}
	}
}
