package main

import (
	"reflect"
	"testing"
)

func TestRankHostsOrdersByScore(t *testing.T) {
	score := map[string]int{"a.onion": 1, "b.onion": 5, "c.onion": 3}
	got := rankHosts(score, 10)
	want := []string{"b.onion", "c.onion", "a.onion"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("порядок %v, ожидала %v", got, want)
	}
}

func TestRankHostsTruncatesToLimit(t *testing.T) {
	score := map[string]int{"a.onion": 1, "b.onion": 5, "c.onion": 3, "d.onion": 2}
	got := rankHosts(score, 2)
	want := []string{"b.onion", "c.onion"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("обрезка дала %v, ожидала %v", got, want)
	}
}

func TestRankHostsTruncationKeepsTopScores(t *testing.T) {
	// Обрезка обязана отбрасывать хвост, а не случайные записи: иначе в обход
	// попали бы хосты, упомянутые один раз, а вероятные файловые - нет.
	score := map[string]int{}
	for i := 0; i < 50; i++ {
		score[string(rune('a'+i%26))+string(rune('0'+i/26))+".onion"] = i + 1
	}
	got := rankHosts(score, 5)
	if len(got) != 5 {
		t.Fatalf("вернулось %d хостов", len(got))
	}
	for _, h := range got {
		if score[h] < 46 {
			t.Errorf("в топ-5 попал хост со счётом %d: %s", score[h], h)
		}
	}
}

func TestRankHostsDeterministicOnTies(t *testing.T) {
	// Обход карты в Go рандомизирован. Без детерминированной развилки два
	// одинаковых прогона дали бы разный набор кандидатов, и воспроизвести
	// найденное было бы нельзя.
	score := map[string]int{"z.onion": 2, "a.onion": 2, "m.onion": 2, "b.onion": 2}
	first := rankHosts(score, 10)
	want := []string{"a.onion", "b.onion", "m.onion", "z.onion"}
	if !reflect.DeepEqual(first, want) {
		t.Fatalf("порядок %v, ожидала %v", first, want)
	}
	for i := 0; i < 20; i++ {
		if got := rankHosts(score, 10); !reflect.DeepEqual(got, first) {
			t.Fatalf("итерация %d дала другой порядок: %v", i, got)
		}
	}
}

func TestRankHostsEmptyAndZeroLimit(t *testing.T) {
	if got := rankHosts(nil, 5); got != nil {
		t.Errorf("пустая карта дала %v", got)
	}
	if got := rankHosts(map[string]int{}, 5); got != nil {
		t.Errorf("пустая карта дала %v", got)
	}
	if got := rankHosts(map[string]int{"a.onion": 3}, 0); got != nil {
		t.Errorf("нулевой limit дал %v", got)
	}
	if got := rankHosts(map[string]int{"a.onion": 3}, -1); got != nil {
		t.Errorf("отрицательный limit дал %v", got)
	}
}

func TestRankHostsSkipsJunk(t *testing.T) {
	// Пустой хост и нулевой счёт не должны занимать место в топе: пустая
	// строка ушла бы в обход как адрес, а ноль означает «не упоминался».
	score := map[string]int{"": 9, "  ": 8, "a.onion": 1, "b.onion": 0}
	got := rankHosts(score, 10)
	want := []string{"a.onion"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("получено %v, ожидала %v", got, want)
	}
}

func TestRankHostsSingleEntry(t *testing.T) {
	got := rankHosts(map[string]int{"only.onion": 1}, 5)
	if !reflect.DeepEqual(got, []string{"only.onion"}) {
		t.Errorf("получено %v", got)
	}
}

func TestRankHostsNoDuplicates(t *testing.T) {
	// Карта по построению не даёт дублей, но проверка нужна как страховка:
	// дубль означал бы двойной обход одного хоста и лишнюю нагрузку на tor.
	score := map[string]int{"a.onion": 3, "b.onion": 2, "c.onion": 1}
	got := rankHosts(score, 10)
	seen := map[string]bool{}
	for _, h := range got {
		if seen[h] {
			t.Fatalf("дубль %s в %v", h, got)
		}
		seen[h] = true
	}
}

func TestFileHostQueriesNotEmpty(t *testing.T) {
	if len(fileHostQueries) == 0 {
		t.Fatal("список файловых запросов пуст")
	}
	seen := map[string]bool{}
	for _, q := range fileHostQueries {
		if q == "" {
			t.Error("пустой запрос в списке")
		}
		if seen[q] {
			t.Errorf("дубль запроса %q", q)
		}
		seen[q] = true
	}
}
