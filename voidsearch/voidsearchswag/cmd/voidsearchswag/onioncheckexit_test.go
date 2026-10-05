package main

import (
	"strings"
	"testing"
)

// Код возврата onioncheck обязан отличать несостоявшуюся проверку от результата.
// Живой замер на копии боевой базы дал «живых 3 из 7 по прошлым замерам
// (пропущено 7: нужен tor или прокси)» при rc=0: обвязка в cron видит успех
// прогона, в котором ни один движок не был спрошен, а состояние пула осталось
// неизвестным. probe и promote после этапов 105 и 104 в таком случае возвращают
// единицу, и три диагностические команды обязаны говорить на одном языке.
func TestCmdOnioncheckExitsNonZeroWhenAllProbesSkipped(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	out, _, code := runMainSplit(t, "onioncheck", "--no-tor", "-timeout", "30s")
	if code != 1 {
		t.Fatalf("код возврата %d, хочу 1: %s", code, firstN(out, 400))
	}
	if !strings.Contains(out, "по прошлым замерам") {
		t.Errorf("отчёт потерял источник живости: %q", firstN(out, 300))
	}
	if !strings.Contains(out, "пропущено") {
		t.Errorf("отчёт потерял число пропущенных проверок: %q", firstN(out, 300))
	}
	if !strings.Contains(out, "torch") {
		t.Errorf("таблица движков потеряна: %q", firstN(out, 600))
	}
}

// Обратная сторона: волна, в которой запрос ушёл и упал, остаётся нулём. Пинг
// через мёртвый прокси - это проверка с результатом «мёртв», а диагностика затем
// и запускается, чтобы находить мёртвые движки.
func TestCmdOnioncheckExitsZeroWhenProbeHappened(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	t.Setenv("VOIDSEARCH_TRANSPORT", "static")
	t.Setenv("VOIDSEARCH_PROXIES", "127.0.0.1:1")
	t.Setenv("VOIDSEARCH_REQUEST_TIMEOUT", "2s")

	out, _, code := runMainSplit(t, "onioncheck", "--no-tor", "-timeout", "60s")
	if code != 0 {
		t.Fatalf("код возврата %d при состоявшейся волне: %s", code, firstN(out, 400))
	}
	if !strings.Contains(out, "onion-поисковики: живых ") {
		t.Errorf("заголовок потерян: %q", firstN(out, 300))
	}
	if strings.Contains(out, "пропущено") {
		t.Errorf("состоявшаяся проба названа пропуском: %q", firstN(out, 400))
	}
}

// Правило вынесено в функцию и разбирается на границах: прогон команды стоит
// tor-клиента и конфига, а отличие «спрошено» от «пропущено» проверяется на пяти
// составах волны.
func TestOnioncheckExitCode(t *testing.T) {
	for _, c := range []struct {
		name           string
		total, skipped int
		want           int
	}{
		{"вся волна пропущена", 7, 7, 1},
		{"вся волна пропущена на свежей базе", 4, 4, 1},
		{"часть пропущена", 7, 2, 0},
		{"все спрошены и мертвы", 7, 0, 0},
		{"движков нет", 0, 0, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := onioncheckExitCode(c.total, c.skipped); got != c.want {
				t.Errorf("onioncheckExitCode(total %d, skipped %d) = %d, хочу %d",
					c.total, c.skipped, got, c.want)
			}
		})
	}
}
