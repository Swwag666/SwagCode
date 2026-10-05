package main

import "testing"

func TestPoolStatusMatchesProbe(t *testing.T) {
	// Согласованные пары не печатаются отдельно: «live» рядом с «отвечает»
	// дублировало бы одно и то же двумя словами и растягивало строку без пользы.
	cases := []struct {
		status string
		ok     bool
		want   bool
	}{
		{"live", true, true},
		{"dead", false, true},
		// Расхождения: одна неудача нового адреса даёт «unknown», а успех
		// оживляет адрес, который считался мёртвым.
		{"unknown", false, false},
		{"unknown", true, false},
		{"dead", true, false},
		{"live", false, false},
		{"", true, false},
		{"", false, false},
		{"weird", true, false},
	}
	for _, c := range cases {
		if got := poolStatusMatchesProbe(c.status, c.ok); got != c.want {
			t.Errorf("poolStatusMatchesProbe(%q, %v) = %v, ожидала %v", c.status, c.ok, got, c.want)
		}
	}
}

func TestPoolStatusNoteExplainsDivergence(t *testing.T) {
	// Объяснение нужно именно потому, что расхождение выглядит как ошибка: адрес
	// не ответил, но мёртвым не объявлен. Без пояснения пользователь решил бы,
	// что статистика врёт или что порог смерти сломан.
	cases := []struct {
		status string
		ok     bool
		must   string
	}{
		{"unknown", false, "одной неудачи мало"},
		{"dead", true, "вернула его в работу"},
		{"dead", false, "третья неудача подряд"},
	}
	for _, c := range cases {
		note := poolStatusNote(c.status, c.ok)
		if note == "" {
			t.Errorf("poolStatusNote(%q, %v) вернул пустую строку", c.status, c.ok)
		}
		if !contains(note, c.must) {
			t.Errorf("poolStatusNote(%q, %v) = %q, ожидала упоминание %q", c.status, c.ok, note, c.must)
		}
	}
}

func TestPoolStatusNoteFallback(t *testing.T) {
	// Неизвестный статус не должен ронять вывод или печатать пустое объяснение:
	// пул может получить новое значение статуса в будущей миграции.
	note := poolStatusNote("quarantined", true)
	if note == "" {
		t.Fatal("пустое объяснение для неизвестного статуса")
	}
	if !contains(note, "quarantined") {
		t.Errorf("объяснение не содержит сам статус: %q", note)
	}
}

func TestPoolStatusNoteNeverEmpty(t *testing.T) {
	// Ни одна комбинация не должна давать пустую строку: вывод напечатал бы
	// «пул: » с висящим двоеточием и без содержания.
	for _, status := range []string{"live", "dead", "unknown", "", "прочее"} {
		for _, ok := range []bool{true, false} {
			if note := poolStatusNote(status, ok); note == "" {
				t.Errorf("poolStatusNote(%q, %v) вернул пустую строку", status, ok)
			}
		}
	}
}

// contains - локальная проверка подстроки без импорта strings ради одного
// вспомогательного теста.
func contains(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
