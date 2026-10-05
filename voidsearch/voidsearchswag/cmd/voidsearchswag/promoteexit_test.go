package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// Прогон промоута, в котором ни одна проверка не состоялась, обязан
// завершаться ненулевым кодом. Обвязка в cron или скрипте не читает текст
// отчёта и отличает провал только по коду возврата, а команда parse уже
// возвращает единицу при неудачном разборе - promote до правки возвращал ноль
// всегда, и регулярный прогон «tor лёг, ничего не проверено» выглядел успехом.
func TestPromoteCLIExitsNonZeroWhenNoCheckSucceeded(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	promoteBase(t, dir, 3)

	out, code := runMain(t, "promote", "--no-tor", "-limit", "3")
	if code != 1 {
		t.Errorf("код возврата %d, хочу 1:\n%s", code, firstN(out, 400))
	}
	// Отчёт при этом сохраняется: код возврата добавляет сигнал, а не заменяет
	// объяснение, из-за которого прогон и признаётся несостоявшимся.
	if !strings.Contains(out, "проверено 3, поднято 0") {
		t.Errorf("итоговая строка потеряна:\n%s", firstN(out, 300))
	}
	if !strings.Contains(out, "отказов 3") {
		t.Errorf("оговорка об отказах потеряна:\n%s", firstN(out, 400))
	}
}

// Машинный режим подчиняется тому же правилу: JSON печатается целиком, и уже
// потом процесс завершается единицей. Обратный порядок лишил бы потребителя
// и отчёта, и сигнала.
func TestPromoteCLIJSONExitsNonZeroWhenNoCheckSucceeded(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	promoteBase(t, dir, 2)

	out, code := runMain(t, "promote", "--no-tor", "-limit", "2", "--json")
	if code != 1 {
		t.Errorf("код возврата %d, хочу 1: %s", code, firstN(out, 300))
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("JSON не разобран: %v (%s)", err, firstN(out, 200))
	}
	if res["failed"] != float64(2) {
		t.Errorf("failed = %+v, хочу 2", res["failed"])
	}
	if promoted, ok := res["promoted"].([]any); !ok || len(promoted) != 0 {
		t.Errorf("promoted = %#v, хочу пустой список", res["promoted"])
	}
}

// Пустой пул - не отказ: проверять нечего, и команда обязана остаться
// успешной, иначе обвязка начнёт сообщать о поломке на свежей базе.
func TestPromoteCLIExitsZeroWhenNothingChecked(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	promoteBase(t, dir, 0)

	out, code := runMain(t, "promote", "--no-tor", "-limit", "3")
	if code != 0 {
		t.Errorf("код возврата %d при пустом пуле: %s", code, firstN(out, 300))
	}
}

// Решение о коде возврата вынесено в функцию: правило «работа не выполнена»
// обязано быть одним и тем же для текстового и машинного режима, а проверить его
// на границах проще, чем через три прогона команды.
func TestPromoteExitCode(t *testing.T) {
	for _, c := range []struct {
		name                             string
		checked, promoted, failed, saved int
		want                             int
	}{
		{"все проверки упали", 3, 0, 3, 0, 1},
		{"найдено, но не сохранено", 2, 0, 0, 2, 1},
		{"отказ и несохранённый сид", 4, 0, 3, 1, 1},
		{"поднят хотя бы один при отказах", 5, 1, 4, 0, 0},
		{"проверки состоялись, движков нет", 6, 0, 0, 0, 0},
		{"пустой пул", 0, 0, 0, 0, 0},
		{"один отказ из шести при нуле подъёма", 6, 0, 1, 0, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := promoteExitCode(c.checked, c.promoted, c.failed, c.saved); got != c.want {
				t.Errorf("promoteExitCode(%d, %d, %d, %d) = %d, хочу %d",
					c.checked, c.promoted, c.failed, c.saved, got, c.want)
			}
		})
	}
}
