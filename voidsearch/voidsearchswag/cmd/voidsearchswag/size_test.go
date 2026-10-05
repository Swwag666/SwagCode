package main

import (
	"strings"
	"testing"
)

func TestParseSize(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		// Число без суффикса остаётся байтами: прежнее поведение сохранено.
		{"0", 0},
		{"1024", 1024},
		{"500", 500},
		{"1b", 1},
		{"1B", 1},
		{"100 байт", 100},

		// Десятичные суффиксы: степень 1000.
		{"1kb", 1000},
		{"1KB", 1000},
		{"1k", 1000},
		{"1кб", 1000},
		{"1mb", 1000000},
		{"1MB", 1000000},
		{"1m", 1000000},
		{"1мб", 1000000},
		{"2gb", 2000000000},
		{"1g", 1000000000},
		{"1гб", 1000000000},
		{"1tb", 1000000000000},

		// Двоичные суффиксы: степень 1024. Каталог печатает размеры именно в
		// MiB, поэтому пользователь пишет MiB и ожидает 1024.
		{"1kib", 1024},
		{"1KiB", 1024},
		{"1ki", 1024},
		{"1mib", 1048576},
		{"1MiB", 1048576},
		{"1.5MiB", 1572864},
		{"1gib", 1073741824},
		{"1tib", 1099511627776},

		// Дробные значения и пробел между числом и суффиксом.
		{"0.5mb", 500000},
		{"2.5 mib", 2621440},
		{"  1 MB  ", 1000000},
	}
	for _, c := range cases {
		got, err := parseSize(c.in)
		if err != nil {
			t.Errorf("parseSize(%q) ошибка: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseSize(%q) = %d, ожидала %d", c.in, got, c.want)
		}
	}
}

func TestParseSizeRejectsBadInput(t *testing.T) {
	cases := []struct {
		in       string
		contains string
	}{
		{"", "пустое"},
		{"   ", "пустое"},
		{"bogus", "нет числа"},
		{"mb", "нет числа"},
		{"-5", "отрицательным"},
		{"-1MB", "отрицательным"},
		{"+5", "без плюса"},
		{"1XB", "неизвестный суффикс"},
		{"5 пингвинов", "неизвестный суффикс"},
		{"999999999999999999999TB", "превышает предел"},
	}
	for _, c := range cases {
		_, err := parseSize(c.in)
		if err == nil {
			t.Errorf("parseSize(%q) принято", c.in)
			continue
		}
		if !strings.Contains(err.Error(), c.contains) {
			t.Errorf("parseSize(%q): ошибка %q не содержит %q", c.in, err, c.contains)
		}
	}
}

func TestParseSizeErrorMessageNamesInput(t *testing.T) {
	// Сообщение обязано показывать исходное значение: пользователь набирает
	// флаг вручную и должен видеть, что именно не разобралось.
	_, err := parseSize("1XB")
	if err == nil || !strings.Contains(err.Error(), "1XB") {
		t.Errorf("в ошибке нет исходного значения: %v", err)
	}
	// И перечислять доступные суффиксы, иначе непонятно, что писать вместо.
	if err == nil || !strings.Contains(err.Error(), "mib") {
		t.Errorf("ошибка не перечисляет доступные суффиксы: %v", err)
	}
}

func TestParseSizeDecimalVsBinaryDiffer(t *testing.T) {
	// MB и MiB обязаны различаться: если дать им один множитель, фильтр
	// «до 2MiB» будет отсекать не то, что показывает каталог.
	dec, err := parseSize("1MB")
	if err != nil {
		t.Fatal(err)
	}
	bin, err := parseSize("1MiB")
	if err != nil {
		t.Fatal(err)
	}
	if dec == bin {
		t.Errorf("MB и MiB дали одно значение %d", dec)
	}
	if bin <= dec {
		t.Errorf("MiB=%d должен быть больше MB=%d", bin, dec)
	}
}

func TestParseSizeOverflowBoundary(t *testing.T) {
	// float64(math.MaxInt64) округляется ровно до 2^63, поэтому условие
	// out > float64(math.MaxInt64) пропускало значение, в точности равное
	// пределу: приведение int64(2^63) даёт -9223372036854775808, и фильтр
	// размера молча становился отрицательным, то есть переставал фильтровать.
	//
	// Значения на самой границе отвергаются целиком, потому что float64 не
	// различает MaxInt64 и 2^63: «9223372036854775807» приводится к тому же
	// float64, что и «9223372036854775808». Цена - отказ на значении, которое
	// на один байт меньше предела, то есть на восьми эбибайтах, и это безопасная
	// сторона ошибки.
	for _, in := range []string{
		"9223372036854775808",     // ровно 2^63 байт
		"9223372036854775807",     // MaxInt64: во float64 неотличим от 2^63
		"8388608tib",              // 2^23 * 2^40 = 2^63
		"999999999999999999999TB", // многократно за пределом
	} {
		v, err := parseSize(in)
		if err == nil {
			t.Errorf("parseSize(%q) = %d без ошибки, ожидала отказ на переполнении", in, v)
			continue
		}
		if !strings.Contains(err.Error(), "предел") {
			t.Errorf("parseSize(%q): ошибка %q не объясняет переполнение", in, err)
		}
	}

	// Значения ниже границы обязаны приниматься, иначе отказ начал бы ловить и
	// рабочие размеры.
	for _, c := range []struct {
		in   string
		want int64
	}{
		{"4611686018427387904", 4611686018427387904}, // 2^62
		{"4096tib", 4096 * 1099511627776},            // 2^42
		{"9223372036854774784", 9223372036854774784}, // наибольшее float64 ниже 2^63
	} {
		got, err := parseSize(c.in)
		if err != nil {
			t.Errorf("parseSize(%q) отвергнуто: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseSize(%q) = %d, ожидала %d", c.in, got, c.want)
		}
	}
}

func TestSizeFlagInterface(t *testing.T) {
	// sizeFlag реализует flag.Value, поэтому проверяется контракт: String до
	// Set не должен паниковать, Set обязан возвращать ошибку на мусоре и
	// сохранять значение на корректном вводе.
	var f sizeFlag
	if f.String() != "0" {
		t.Errorf("незаданный флаг = %q", f.String())
	}
	if f.set || f.val != 0 {
		t.Error("незаданный флаг уже заполнен")
	}
	if err := f.Set("bogus"); err == nil {
		t.Error("мусор принят")
	}
	if f.set {
		t.Error("мусор пометил флаг как заданный")
	}
	if err := f.Set("2MiB"); err != nil {
		t.Fatal(err)
	}
	if !f.set || f.val != 2097152 {
		t.Errorf("после Set: set=%v val=%d", f.set, f.val)
	}
	if f.String() != "2MiB" {
		t.Errorf("String вернул %q, ожидала исходное 2MiB", f.String())
	}
}
