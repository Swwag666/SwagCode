package main

import (
	"strings"
	"testing"
)

// Первая строка отчёта обязана называть источник живости, когда волна не
// состоялась. Живой замер на копии боевой базы дал «onion-поисковики: живых 3 из
// 7 (пропущено 7: нужен tor или прокси)»: три живых взяты из таблицы прошлых
// замеров, ни один движок в этом прогоне не спрошен, а строка утверждает
// текущее состояние пула. Таблица ниже усиливала впечатление - «tornet жив,
// успех 93%, проб 29» при нуле проб в этой волне. На свежей базе та же
// конструкция даёт «живых 0 из 4», где ноль означает отсутствие истории, а не
// смерть движков.
func TestCmdOnioncheckNamesPastMeasurements(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)

	// Единица - часть того же контракта: несостоявшаяся волна не должна выглядеть
	// успешным прогоном для обвязки, которая читает код возврата.
	out, code := runMain(t, "onioncheck", "--no-tor", "-timeout", "30s")
	if code != 1 {
		t.Fatalf("onioncheck: код %d, хочу 1, вывод %q", code, firstN(out, 400))
	}
	if !strings.Contains(out, "по прошлым замерам") {
		t.Errorf("строка не называет источник живости: %q", firstN(out, 300))
	}
	if !strings.Contains(out, "пропущено") {
		t.Errorf("пропущенные проверки не названы: %q", firstN(out, 300))
	}
	if !strings.Contains(out, "в этой волне не проверялось") {
		t.Errorf("таблица состояний не оговорена: %q", firstN(out, 600))
	}
}

// Обратная сторона: состоявшаяся волна не должна обрастать оговорками. Пинг
// уходит через прокси и падает на соединении - это проверка с результатом
// «мёртв», а не пропуск, поэтому живость в строке своя, а не историческая.
func TestCmdOnioncheckHeadlineWhenProbeHappened(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	t.Setenv("VOIDSEARCH_TRANSPORT", "static")
	t.Setenv("VOIDSEARCH_PROXIES", "127.0.0.1:1")
	t.Setenv("VOIDSEARCH_REQUEST_TIMEOUT", "2s")

	out, code := runMain(t, "onioncheck", "--no-tor", "-timeout", "60s")
	if code != 0 {
		t.Fatalf("onioncheck: код %d, вывод %q", code, firstN(out, 400))
	}
	if !strings.Contains(out, "onion-поисковики: живых ") {
		t.Errorf("заголовок потерян: %q", firstN(out, 300))
	}
	if strings.Contains(out, "пропущено") {
		t.Errorf("состоявшаяся проба названа пропуском: %q", firstN(out, 400))
	}
	if strings.Contains(out, "по прошлым замерам") {
		t.Errorf("оговорка об истории при состоявшейся волне: %q", firstN(out, 400))
	}
}

// Три состава волны разбираются напрямую: команда поднимает tor и конфиг, а
// формулировка строки обязана проверяться без сети.
func TestOnioncheckHeadline(t *testing.T) {
	for _, c := range []struct {
		name                     string
		live, total, skipped     int
		wantExact                string
		wantContains, wantAbsent []string
	}{
		{
			name: "волна состоялась",
			live: 3, total: 7, skipped: 0,
			wantExact: "onion-поисковики: живых 3 из 7",
		},
		{
			name: "вся волна пропущена",
			live: 3, total: 7, skipped: 7,
			wantExact: "onion-поисковики: живых 3 из 7 по прошлым замерам (пропущено 7: нужен tor или прокси)",
		},
		{
			name: "вся волна пропущена на свежей базе",
			live: 0, total: 4, skipped: 4,
			wantExact: "onion-поисковики: живых 0 из 4 по прошлым замерам (пропущено 4: нужен tor или прокси)",
		},
		{
			name: "часть пропущена",
			live: 2, total: 7, skipped: 3,
			wantContains: []string{
				"живых 2 из 7",
				"пропущено 3: нужен tor или прокси",
				"живость непроверенных по прошлым замерам",
			},
		},
		{
			name: "движков нет",
			live: 0, total: 0, skipped: 0,
			wantExact:  "onion-поисковики: живых 0 из 0",
			wantAbsent: []string{"пропущено", "по прошлым замерам"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := onioncheckHeadline(c.live, c.total, c.skipped)
			if c.wantExact != "" && got != c.wantExact {
				t.Errorf("строка = %q, хочу %q", got, c.wantExact)
			}
			for _, w := range c.wantContains {
				if !strings.Contains(got, w) {
					t.Errorf("в строке нет %q: %q", w, got)
				}
			}
			for _, w := range c.wantAbsent {
				if strings.Contains(got, w) {
					t.Errorf("в строке появилось %q: %q", w, got)
				}
			}
			// Существующий контракт отчёта: причина пропуска не обязана
			// выглядеть как ошибка движка, поэтому двоеточие после неё
			// запрещено - по нему тест TestCmdOnioncheckReportsSkippedProbes
			// отличает пропуск от записанной ошибки.
			if strings.Contains(got, "нужен tor или прокси:") {
				t.Errorf("причина пропуска стала похожа на ошибку движка: %q", got)
			}
		})
	}
}
