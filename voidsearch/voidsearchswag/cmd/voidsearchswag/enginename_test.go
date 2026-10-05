package main

import (
	"strings"
	"testing"
)

func TestDisplayEngineNameShortensOnionLabel(t *testing.T) {
	// Живой прогон deep-режима напечатал имя движка целиком: одна такая запись
	// длиннее всех остальных вместе взятых и ломала строку отчёта на перенос.
	full := "2222222o2mfta3jpjvymjr6jiwmmuyvouq2huakwcxu5tjdwj3egovad"
	got := displayEngineName(full)

	if got == full {
		t.Error("v3 onion-имя не укорочено")
	}
	if want := "onion-2222222o"; got != want {
		t.Errorf("укороченное имя = %q, ожидала %q", got, want)
	}
	if len(got) > 16 {
		t.Errorf("укороченное имя слишком длинное: %d символов", len(got))
	}
}

func TestDisplayEngineNameKeepsReadableNames(t *testing.T) {
	// Осмысленные имена и так короткие, поэтому трогать их нельзя: укорачивание
	// сделало бы отчёт менее понятным, а не более.
	for _, name := range []string{
		"torch", "tornet", "tor66", "ahmia", "ahmia-clear", "ddg-html", "ddg-lite",
	} {
		if got := displayEngineName(name); got != name {
			t.Errorf("читаемое имя %q изменено на %q", name, got)
		}
	}
}

func TestDisplayEngineNameDeterministic(t *testing.T) {
	// Имя обязано оставаться детерминированным, иначе отчёты двух прогонов
	// нельзя было бы сравнивать между собой.
	full := "2222222o2mfta3jpjvymjr6jiwmmuyvouq2huakwcxu5tjdwj3egovad"
	first := displayEngineName(full)
	for i := 0; i < 5; i++ {
		if got := displayEngineName(full); got != first {
			t.Errorf("имя нестабильно: %q против %q", got, first)
		}
	}
}

func TestDisplayEngineNameDistinctOnionsStayDistinct(t *testing.T) {
	// Смысл укорачивания - читаемость, но разные движки обязаны оставаться
	// различимыми в списке. Префикса в 8 символов для этого достаточно.
	names := []string{
		"2222222o2mfta3jpjvymjr6jiwmmuyvouq2huakwcxu5tjdwj3egovad",
		"3333333p3nguubkqwznxkzs7axjvxpzipl3iwbnzdyt4exfhmnkfae",
		"4444444q4ohvvclyxoaylat8bykwzqmjcm4jxoaeuz5fyigolmgfb",
	}
	seen := map[string]string{}
	for _, n := range names {
		got := displayEngineName(n)
		if prev, dup := seen[got]; dup {
			t.Errorf("коллизия: %q и %q дали %q", prev, n, got)
		}
		seen[got] = n
	}
}

func TestLooksLikeOnionLabel(t *testing.T) {
	cases := []struct {
		s    string
		want bool
	}{
		// Настоящая метка v3 onion: 56 символов base32.
		{"2222222o2mfta3jpjvymjr6jiwmmuyvouq2huakwcxu5tjdwj3egovad", true},
		{"abcdefghijklmnopqrstuvwxyz234567abcdefghijklmnopqrstuvwxyz", false}, // 57 символов
		// Короткие и длинные строки не подходят.
		{"torch", false},
		{"ahmia-clear", false},
		{"", false},
		// 56 символов, но не base32: в алфавите нет 0, 1, 8, 9.
		{"2222222000000000000000000000000000000000000000000000000", false},
		// 56 символов с недопустимым символом.
		{"2222222o2mfta3jpjvymjr6jiwmmuyvouq2huakwcxu5tjdwj3egova!", false},
		// Заглавные буквы: base32 в onion-адресах строчный.
		{"2222222O2MFTA3JPJVYMJR6JIWMMUYVOUQ2HUAKWCXU5TJDWJ3EGOVAD", false},
	}
	for _, c := range cases {
		if got := looksLikeOnionLabel(c.s); got != c.want {
			t.Errorf("looksLikeOnionLabel(%q) = %v, ожидала %v (длина %d)", c.s, got, c.want, len(c.s))
		}
	}
}

func TestDisplayEngineNameEmptyAndWhitespace(t *testing.T) {
	if got := displayEngineName(""); got != "" {
		t.Errorf("пустое имя дало %q", got)
	}
	// Пробелы по краям убираются: имя могло прийти из базы с мусором.
	if got := displayEngineName("  torch  "); got != "torch" {
		t.Errorf("пробелы не убраны: %q", got)
	}
}

func TestDisplayEngineNameFitsReportLine(t *testing.T) {
	// Конечный смысл правки: строка отчёта обязана оставаться в одну линию при
	// реалистичном числе движков. Живой прогон дал 5 записей.
	names := []string{"torch", "tornet", "tor66",
		"2222222o2mfta3jpjvymjr6jiwmmuyvouq2huakwcxu5tjdwj3egovad",
		"ahmia-clear"}

	var b strings.Builder
	b.WriteString("движки: ")
	for i, n := range names {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(displayEngineName(n) + ":281")
	}
	line := b.String()

	if len(line) > 100 {
		t.Errorf("строка отчёта слишком длинная (%d символов): %s", len(line), line)
	}
	if strings.Contains(line, "2222222o2mfta3jpjvymjr6jiwmmuyvouq2huakwcxu5tjdwj3egovad") {
		t.Errorf("полное onion-имя осталось в строке: %s", line)
	}
	if !strings.Contains(line, "onion-2222222o") {
		t.Errorf("укороченное имя не попало в строку: %s", line)
	}
}
