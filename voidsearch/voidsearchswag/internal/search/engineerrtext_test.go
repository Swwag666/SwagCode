package search

import (
	"errors"
	"strings"
	"testing"
)

// engineErrText снимает из текста ошибки имя движка, которое движок сам же и
// поставил: отчёт, печать и лог добавляют имя ещё раз, и без снятия получался
// дубль «ahmia-clear: ahmia-clear: ссылок не найдено».
func TestEngineErrTextStripsOwnNamePrefix(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"ahmia-clear", "ahmia-clear: ссылок не найдено", "ссылок не найдено"},
		{"ahmia-clear", "ahmia-clear: HTTP 503", "HTTP 503"},
		{"ahmia-clear", "ahmia-clear:   лишние пробелы", "лишние пробелы"},
		{"torch", "torch: таймаут", "таймаут"},
	}
	for _, c := range cases {
		got := engineErrText(c.name, errors.New(c.in))
		if got != c.want {
			t.Errorf("engineErrText(%q, %q) = %q, хочу %q", c.name, c.in, got, c.want)
		}
	}
}

// Чужой префикс не срезается: ошибка транспорта начинается словами про транспорт,
// и резать её по имени движка значило бы терять часть причины.
func TestEngineErrTextKeepsForeignPrefix(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"rod-browser", "браузер: ddg: таймаут"},
		{"ahmia-clear", "Get \"https://ahmia.fi/search/\": context deadline exceeded"},
		{"torch", "onion-адрес недостижим напрямую: нужен tor или прокси"},
	}
	for _, c := range cases {
		got := engineErrText(c.name, errors.New(c.in))
		if got != c.in {
			t.Errorf("engineErrText(%q, %q) = %q, хочу текст без изменений", c.name, c.in, got)
		}
	}
}

// Совпадение обязано быть по границе слова, а не по подстроке: имя «ahmia» не
// может резать сообщение про «ahmia-clear».
func TestEngineErrTextNeedsExactNameBoundary(t *testing.T) {
	in := "ahmia-clear: ссылок не найдено"
	if got := engineErrText("ahmia", errors.New(in)); got != in {
		t.Errorf("имя %q срезало чужой префикс: %q", "ahmia", got)
	}
	if got := engineErrText("ahmia-clea", errors.New(in)); got != in {
		t.Errorf("неполное имя срезало префикс: %q", got)
	}
}

// Без ошибки текста нет: пустая строка означает «движок ответил», и это отличает
// успешный отчёт от отказа, у которого причина потерялась.
func TestEngineErrTextNilErrorIsEmpty(t *testing.T) {
	if got := engineErrText("ahmia-clear", nil); got != "" {
		t.Errorf("engineErrText при nil = %q, хочу пустую строку", got)
	}
}

// Без имени резать нечем: сообщение возвращается как есть, иначе функция
// срезала бы первый попавшийся префикс и портила чужие ошибки.
func TestEngineErrTextWithoutNameKeepsMessage(t *testing.T) {
	in := "ссылок не найдено"
	if got := engineErrText("", errors.New(in)); got != in {
		t.Errorf("engineErrText с пустым именем = %q, хочу %q", got, in)
	}
	prefixed := "ahmia-clear: ссылок не найдено"
	if got := engineErrText("", errors.New(prefixed)); got != prefixed {
		t.Errorf("пустое имя срезало префикс: %q", got)
	}
}

// Пустое сообщение остаётся пустым: «без описания» подставляет печать, а не
// эта функция, иначе отчёт в JSON нёс бы текст, которого движок не говорил.
func TestEngineErrTextEmptyMessage(t *testing.T) {
	if got := engineErrText("ahmia-clear", errors.New("")); got != "" {
		t.Errorf("engineErrText при пустой ошибке = %q, хочу пустую строку", got)
	}
	if got := engineErrText("ahmia-clear", errors.New("   ")); strings.TrimSpace(got) != "" {
		t.Errorf("engineErrText при ошибке из пробелов = %q", got)
	}
}
