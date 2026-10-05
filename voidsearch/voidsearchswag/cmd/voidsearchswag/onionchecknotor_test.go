package main

import (
	"strconv"
	"strings"
	"testing"
)

func TestCmdOnioncheckReportsSkippedProbes(t *testing.T) {
	// Без tor onion-адрес недостижим, и прежний вывод «onion-поисковики: живых
	// 0 из 7» читался как массовая смерть движков. Живой замер показывал хуже:
	// за четыре вызова успех tornet падал 90% -> 87% -> 84% -> 82%, а после
	// третьего вызова все движки получали состояние «отключён» навсегда.
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	// Единица здесь часть контракта: ни одна проверка не состоялась, и обвязка
	// обязана видеть это по коду возврата, а не по чтению отчёта.
	out, code := runMain(t, "onioncheck", "--no-tor", "-timeout", "30s")
	if code != 1 {
		t.Fatalf("onioncheck: код %d, хочу 1, вывод %q", code, firstN(out, 400))
	}
	if !strings.Contains(out, "живых ") {
		t.Errorf("в выводе нет счётчика живых: %q", firstN(out, 300))
	}
	if !strings.Contains(out, "пропущено") {
		t.Errorf("в выводе нет числа пропущенных проверок: %q", firstN(out, 300))
	}
	if !strings.Contains(out, "нужен tor или прокси") {
		t.Errorf("в выводе нет причины пропуска: %q", firstN(out, 300))
	}
	if strings.Contains(out, "нужен tor или прокси:") {
		t.Errorf("несостоявшаяся проверка записана как ошибка движка: %q", firstN(out, 800))
	}
	if strings.Contains(out, "отключён") {
		t.Errorf("движок отключен несостоявшейся проверкой: %q", firstN(out, 800))
	}
}

func TestCmdOnioncheckRepeatDoesNotDegrade(t *testing.T) {
	// Повторные вызовы не должны копить ущерб: состояние движков переживает
	// перезапуск, поэтому испорченные счётчики остались бы в базе навсегда.
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	for i := 0; i < 4; i++ {
		// Все четыре вызова пропускают волну целиком, поэтому единица обязана
		// повторяться: код возврата не должен «изнашиваться» вместе со
		// счётчиками здоровья.
		out, code := runMain(t, "onioncheck", "--no-tor", "-timeout", "30s")
		if code != 1 {
			t.Fatalf("вызов %d: код %d, хочу 1, вывод %q", i+1, code, firstN(out, 300))
		}
		lines := probeLines(out)
		if len(lines) == 0 {
			t.Fatalf("вызов %d: таблица движков пуста, проверять нечего: %q", i+1, firstN(out, 300))
		}
		if strings.Contains(out, "отключён") {
			t.Fatalf("вызов %d отключил движок: %q", i+1, firstN(out, 800))
		}
		if n := probesTotal(lines); n != 0 {
			t.Fatalf("вызов %d: суммарно проб %d, ожидала 0 - несостоявшиеся проверки попали в счётчик", i+1, n)
		}
		if !strings.Contains(out, "пропущено") {
			t.Fatalf("вызов %d: нет пояснения о пропуске: %q", i+1, firstN(out, 300))
		}
	}
}

// probeLines возвращает строки таблицы движков. Порядок их нестабилен: пул
// хранит записи в map, поэтому сравнивать между вызовами первую строку
// бессмысленно - суммировать счётчики можно, а искать «ту же строку» нет.
func probeLines(out string) []string {
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "проб ") {
			lines = append(lines, line)
		}
	}
	return lines
}

// probesTotal суммирует счётчики проб по строкам таблицы.
func probesTotal(lines []string) int {
	total := 0
	for _, line := range lines {
		i := strings.Index(line, "проб ")
		fields := strings.Fields(line[i+len("проб "):])
		if len(fields) == 0 {
			continue
		}
		n, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		total += n
	}
	return total
}
