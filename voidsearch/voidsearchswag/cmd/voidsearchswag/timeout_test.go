package main

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestCheckTimeoutAcceptsSaneValues(t *testing.T) {
	for _, d := range []time.Duration{time.Second, 30 * time.Second, 5 * time.Minute, time.Hour, 24 * time.Hour} {
		if err := checkTimeout("timeout", d); err != nil {
			t.Errorf("%s отклонено: %v", d, err)
		}
	}
}

func TestCheckTimeoutRejectsNonPositive(t *testing.T) {
	// Ноль или отрицательное значение создают уже истёкший контекст, поэтому
	// каждая операция падает немедленно, а сообщение выглядит как сбой сети
	// или базы, а не как опечатка во флаге.
	for _, d := range []time.Duration{0, -time.Second, -24 * time.Hour} {
		err := checkTimeout("timeout", d)
		if err == nil {
			t.Errorf("%s принято", d)
			continue
		}
		if !strings.Contains(err.Error(), "положительным") {
			t.Errorf("ошибка не объясняет причину: %v", err)
		}
		if !strings.Contains(err.Error(), "--timeout") {
			t.Errorf("в ошибке нет имени флага: %v", err)
		}
	}
}

func TestCheckTimeoutRejectsHugeValues(t *testing.T) {
	// --timeout 3000000h в сумме с добавочной минутой переполнял int64 в
	// наносекундах и уходил в отрицательную область: контекст снова оказывался
	// истёкшим, и hunt watch возвращался мгновенно с невнятной ошибкой.
	// Литерал 3000000 * time.Hour не компилируется - константа переполняет
	// int64 ещё на этапе сборки, поэтому крайний случай берётся через MaxInt64.
	for _, d := range []time.Duration{24*time.Hour + time.Second, 100000 * time.Hour, time.Duration(math.MaxInt64)} {
		err := checkTimeout("timeout", d)
		if err == nil {
			t.Errorf("%s принято", d)
			continue
		}
		if !strings.Contains(err.Error(), "слишком большой") {
			t.Errorf("ошибка не объясняет причину: %v", err)
		}
	}
}

func TestCheckTimeoutNamesFlag(t *testing.T) {
	// Имя флага обязано попадать в текст: у команд несколько длительностей,
	// и «--interval» в сообщении важнее общего «ошибка».
	err := checkTimeout("interval", 0)
	if err == nil || !strings.Contains(err.Error(), "--interval") {
		t.Errorf("имя флага не попало в ошибку: %v", err)
	}
}

func TestAddTimeoutNoOverflow(t *testing.T) {
	if got := addTimeout(math.MaxInt64, time.Minute); got != math.MaxInt64 {
		t.Errorf("сумма переполнилась: %d", got)
	}
	if got := addTimeout(math.MaxInt64, time.Minute); got <= 0 {
		t.Errorf("результат неположительный: %d", got)
	}
	// Обычное сложение не должно меняться.
	if got := addTimeout(time.Minute, time.Minute); got != 2*time.Minute {
		t.Errorf("обычная сумма %s, ожидала 2m", got)
	}
	// Отрицательное добавочное не должно ломать проверку.
	if got := addTimeout(time.Hour, -time.Minute); got != time.Hour-time.Minute {
		t.Errorf("вычитание дало %s", got)
	}
	// Граница: ровно максимум не переполняется.
	if got := addTimeout(math.MaxInt64-time.Minute, time.Minute); got != math.MaxInt64 {
		t.Errorf("граница обработана неверно: %d", got)
	}
}

func TestAddTimeoutResultAlwaysValidForContext(t *testing.T) {
	// Конечная цель: результат обязан быть пригодным для context.WithTimeout,
	// то есть строго положительным. Иначе контекст создан уже истёкшим.
	for _, d := range []time.Duration{time.Second, time.Hour, 24 * time.Hour, math.MaxInt64} {
		if got := addTimeout(d, time.Minute); got <= 0 {
			t.Errorf("%s дало неположительный результат %d", d, got)
		}
	}
}

func TestValidTimeoutPassesThrough(t *testing.T) {
	// validTimeout завершает процесс на ошибке, поэтому проверяется только
	// проход валидного значения: само правило покрыто через checkTimeout.
	if got := validTimeout("timeout", 5*time.Minute); got != 5*time.Minute {
		t.Errorf("validTimeout изменил валидное значение: %s", got)
	}
}
