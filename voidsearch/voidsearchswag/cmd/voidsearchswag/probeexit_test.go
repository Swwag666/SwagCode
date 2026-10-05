package main

import (
	"encoding/json"
	"strings"
	"testing"

	"voidsearchswag/internal/discover"
)

// Волна проб, в которой ни один запрос не ушёл в сеть, обязана завершаться
// ненулевым кодом. Отчёт при этом честный: «живых 0 из 1 (пропущено 1: нужен tor
// или прокси)», - но обвязка в cron читает код, а не текст, и до правки получала
// ноль. Для регулярной проверки живости это худший вариант: tor не поднят,
// состояние пула неизвестно, а прогон признан успешным.
func TestCmdProbeExitsNonZeroWhenAllProbesSkipped(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	t.Setenv("VOIDSEARCH_TOR", "off")
	t.Setenv("VOIDSEARCH_TRANSPORT", "direct")
	t.Setenv("VOIDSEARCH_REQUEST_TIMEOUT", "2s")

	out, code := runMain(t, "probe", "--addr", "abcdefghijklmnop.onion", "--timeout", "20s")
	if code != 1 {
		t.Errorf("код возврата %d, хочу 1:\n%s", code, firstN(out, 400))
	}
	// Отчёт сохраняется: код возврата добавляет сигнал, а не заменяет
	// объяснение, из-за которого волна признана несостоявшейся.
	if !strings.Contains(out, "живых 0 из 1") {
		t.Errorf("итоговая строка потеряна:\n%s", firstN(out, 300))
	}
	if !strings.Contains(out, "пропущено 1: нужен tor или прокси") {
		t.Errorf("оговорка о пропуске потеряна:\n%s", firstN(out, 300))
	}
}

// Машинный режим подчиняется тому же правилу: JSON печатается целиком, и только
// потом процесс завершается единицей.
func TestCmdProbeJSONExitsNonZeroWhenAllProbesSkipped(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	t.Setenv("VOIDSEARCH_TOR", "off")
	t.Setenv("VOIDSEARCH_TRANSPORT", "direct")
	t.Setenv("VOIDSEARCH_REQUEST_TIMEOUT", "2s")

	out, _, code := runMainSplit(t, "probe", "--addr", "abcdefghijklmnop.onion", "--json", "--timeout", "20s")
	if code != 1 {
		t.Errorf("код возврата %d, хочу 1: %s", code, firstN(out, 300))
	}
	var rep map[string]any
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("JSON не разобран: %v (%s)", err, firstN(out, 200))
	}
	if rep["skipped"] != float64(1) {
		t.Errorf("skipped = %+v, хочу 1", rep["skipped"])
	}
	if rep["total"] != float64(1) {
		t.Errorf("total = %+v, хочу 1", rep["total"])
	}
}

// Обратная сторона: адрес, который спросили и который не ответил, это результат
// диагностики, а не провал команды. Мёртвый прокси означает, что запрос ушёл в
// сеть и упал на соединении, поэтому волна состоялась и код обязан остаться
// нулевым. Без этого отличия регулярная проверка начала бы сообщать о поломке
// каждый раз, когда пул действительно плох.
func TestCmdProbeExitsZeroWhenProbeReachedNetwork(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	t.Setenv("VOIDSEARCH_TOR", "off")
	t.Setenv("VOIDSEARCH_TRANSPORT", "static")
	t.Setenv("VOIDSEARCH_PROXIES", "127.0.0.1:1")
	t.Setenv("VOIDSEARCH_REQUEST_TIMEOUT", "2s")

	out, code := runMain(t, "probe", "--addr", "abcdefghijklmnop.onion", "--timeout", "20s")
	if code != 0 {
		t.Errorf("код возврата %d при состоявшейся пробе:\n%s", code, firstN(out, 400))
	}
	if strings.Contains(out, "пропущено") {
		t.Errorf("настоящий отказ записан в пропуски:\n%s", firstN(out, 300))
	}
	if !strings.Contains(out, "живых 0 из 1") {
		t.Errorf("итоговая строка потеряна:\n%s", firstN(out, 300))
	}
}

// Пустой пул - не отказ: проверять нечего, волна нулевая, и команда обязана
// остаться успешной, иначе обвязка сообщит о поломке на свежей базе.
func TestCmdProbeExitsZeroWhenNothingToProbe(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	t.Setenv("VOIDSEARCH_TOR", "off")
	t.Setenv("VOIDSEARCH_TRANSPORT", "direct")
	t.Setenv("VOIDSEARCH_REQUEST_TIMEOUT", "2s")

	out, code := runMain(t, "probe", "--limit", "5", "--timeout", "20s")
	if code != 0 {
		t.Errorf("код возврата %d при пустом пуле:\n%s", code, firstN(out, 300))
	}
	if !strings.Contains(out, "живых 0 из 0") {
		t.Errorf("отчёт на пустом пуле потеряна:\n%s", firstN(out, 300))
	}
}

// Правило вынесено в функцию и разбирается на границах: прогон команды стоит
// подпроцесса и тор-клиента, а отличие «спрошено» от «пропущено» проверяется на
// шести составах волны.
func TestProbeExitCode(t *testing.T) {
	for _, c := range []struct {
		name string
		rep  discover.ProbeReport
		want int
	}{
		{"одна проба пропущена", discover.ProbeReport{Total: 1, Skipped: 1}, 1},
		{"вся волна пропущена", discover.ProbeReport{Total: 7, Skipped: 7}, 1},
		{"все спрошены и мертвы", discover.ProbeReport{Total: 2, Dead: 2}, 0},
		{"есть живой", discover.ProbeReport{Total: 3, Live: 1, Dead: 2}, 0},
		{"часть пропущена, один спрошен", discover.ProbeReport{Total: 3, Dead: 1, Skipped: 2}, 0},
		{"пустая волна", discover.ProbeReport{}, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := probeExitCode(c.rep); got != c.want {
				t.Errorf("probeExitCode(total %d, live %d, dead %d, skipped %d) = %d, хочу %d",
					c.rep.Total, c.rep.Live, c.rep.Dead, c.rep.Skipped, got, c.want)
			}
		})
	}
}
