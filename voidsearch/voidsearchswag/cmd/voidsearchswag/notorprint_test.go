package main

import (
	"encoding/json"
	"strings"
	"testing"

	"voidsearchswag/internal/discover"
)

func TestPrintProbeReportMarksSkipped(t *testing.T) {
	// Волна на машине без tor: запросы не ушли в сеть, и вывод обязан сказать об
	// этом в итоговой строке, а не только в причине под адресом.
	rep := discover.ProbeReport{
		Total:   2,
		Live:    0,
		Dead:    0,
		Skipped: 2,
		Elapsed: "5ms",
		Results: []discover.ProbeResult{
			{URL: "abcdefghijklmnop.onion", NoTransport: true, Error: "onion-адрес недостижим напрямую: нужен tor или прокси"},
			{URL: "qrstuvwxyzabcdef.onion", NoTransport: true, Error: "onion-адрес недостижим напрямую: нужен tor или прокси"},
		},
	}

	out := captureStdout(t, func() { printProbeReport(rep) })
	for _, want := range []string{"пробы: живых 0 из 2", "пропущено 2: нужен tor или прокси", "за 5ms", "нет tor", "нужен tor или прокси"} {
		if !strings.Contains(out, want) {
			t.Errorf("в выводе нет %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "нет ответа") {
		t.Errorf("проба без tor названа отсутствием ответа:\n%s", out)
	}
}

func TestPrintProbeReportHealthyWaveUnchanged(t *testing.T) {
	// Обратная сторона: у здоровой волны формат прежний, никаких оговорок.
	rep := discover.ProbeReport{
		Total:   2,
		Live:    2,
		Elapsed: "1.2s",
		Results: []discover.ProbeResult{
			{URL: "abcdefghijklmnop.onion", OK: true, Status: 200, LatencyMS: 120},
			{URL: "qrstuvwxyzabcdef.onion", OK: true, Status: 200, LatencyMS: 340},
		},
	}

	out := captureStdout(t, func() { printProbeReport(rep) })
	if !strings.Contains(out, "пробы: живых 2 из 2 за 1.2s") {
		t.Errorf("итоговая строка изменилась:\n%s", out)
	}
	if strings.Contains(out, "пропущено") {
		t.Errorf("здоровая волна получила оговорку о пропусках:\n%s", out)
	}
	if strings.Count(out, "отвечает") != 2 {
		t.Errorf("состояний «отвечает» %d, ожидала 2:\n%s", strings.Count(out, "отвечает"), out)
	}
}

func TestPrintProbeReportDeadStillSaysNoAnswer(t *testing.T) {
	// Настоящий отказ сервиса обязан печататься как раньше: «нет ответа» и
	// расхождение со статусом пула.
	rep := discover.ProbeReport{
		Total:   1,
		Dead:    1,
		Elapsed: "30s",
		Results: []discover.ProbeResult{
			{URL: "abcdefghijklmnop.onion", Error: "таймаут", PoolStatus: "dead"},
		},
	}

	out := captureStdout(t, func() { printProbeReport(rep) })
	if !strings.Contains(out, "нет ответа") {
		t.Errorf("настоящий отказ не назван отсутствием ответа:\n%s", out)
	}
	if strings.Contains(out, "пропущено") || strings.Contains(out, "нет tor") {
		t.Errorf("настоящий отказ записан в пропуски:\n%s", out)
	}
	if !strings.Contains(out, "таймаут") {
		t.Errorf("причина отказа потеряна:\n%s", out)
	}
}

func TestPrintProbeReportKeepsLimitLine(t *testing.T) {
	rep := discover.ProbeReport{
		Total:    1,
		Live:     1,
		Elapsed:  "2s",
		LimitHit: "потолок выдачи 500",
		Results:  []discover.ProbeResult{{URL: "abcdefghijklmnop.onion", OK: true}},
	}

	out := captureStdout(t, func() { printProbeReport(rep) })
	if !strings.Contains(out, "предел: потолок выдачи 500") {
		t.Errorf("строка предела потеряна:\n%s", out)
	}
}

func TestProbeReportJSONCarriesSkipped(t *testing.T) {
	// Контракт потребителя - JSON: поля обязаны быть в нём, а не только в тексте.
	rep := discover.ProbeReport{
		Total:   1,
		Skipped: 1,
		Results: []discover.ProbeResult{{URL: "abcdefghijklmnop.onion", NoTransport: true, Error: "нужен tor или прокси"}},
	}
	blob, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(blob, &m); err != nil {
		t.Fatal(err)
	}
	if got, _ := m["skipped"].(float64); got != 1 {
		t.Errorf("skipped=%v, ожидала 1: %s", m["skipped"], blob)
	}
	res, _ := m["results"].([]any)
	if len(res) != 1 {
		t.Fatalf("результатов %d: %s", len(res), blob)
	}
	one, _ := res[0].(map[string]any)
	if nt, _ := one["no_transport"].(bool); !nt {
		t.Errorf("no_transport не попал в JSON: %s", blob)
	}
}

func TestPrintDiscoverSummaryMarksSkippedProbes(t *testing.T) {
	// Обход без tor: «живость: обновлено по обходу 0 хостов» не печатается вовсе,
	// и пользователь не узнаёт, что пробы не состоялись.
	res := discover.Result{
		Found:        3,
		New:          0,
		ProbeSkipped: 3,
		Elapsed:      "1s",
	}

	out := captureStdout(t, func() { printDiscoverSummary(res) })
	if !strings.Contains(out, "живость: пропущено 3 проб, нужен tor или прокси") {
		t.Errorf("пропущенные пробы не названы:\n%s", out)
	}
	if strings.Contains(out, "обновлено по обходу") {
		t.Errorf("обход без tor отчитался обновлением живости:\n%s", out)
	}
}

func TestPrintDiscoverSummaryCrawlLineNamesMissingTor(t *testing.T) {
	res := discover.Result{
		Found: 1,
		Crawl: &discover.CrawlReport{
			Pages: 2, Ok: 0, Failed: 0, NoTransport: 2, Depth: 2, Elapsed: "0s",
			MaxHosts: 50, PerHostDelayMS: 2000,
		},
	}

	out := captureStdout(t, func() { printDiscoverSummary(res) })
	want := "обход: страниц 2 (успешно 0, ошибок 0, без tor 2), глубина 2, потолок хостов 50, пауза 2s, за 0s"
	if !strings.Contains(out, want) {
		t.Errorf("нет строки %q:\n%s", want, out)
	}
}

func TestPrintDiscoverSummaryCrawlLineUnchangedWithTor(t *testing.T) {
	// Когда tor есть, строка обхода обязана остаться прежней: «без tor 0»
	// шумело бы в каждом нормальном прогоне.
	res := discover.Result{
		Found:  1,
		Probed: 4,
		Crawl: &discover.CrawlReport{
			Pages: 5, Ok: 4, Failed: 1, Depth: 2, Elapsed: "3s",
			MaxHosts: 50, PerHostDelayMS: 2000,
		},
	}

	out := captureStdout(t, func() { printDiscoverSummary(res) })
	want := "обход: страниц 5 (успешно 4, ошибок 1), глубина 2, потолок хостов 50, пауза 2s, за 3s"
	if !strings.Contains(out, want) {
		t.Errorf("формат строки обхода изменился: нет %q:\n%s", want, out)
	}
	if strings.Contains(out, "без tor") {
		t.Errorf("оговорка о tor появилась при живом tor:\n%s", out)
	}
	if !strings.Contains(out, "живость: обновлено по обходу 4 хостов") {
		t.Errorf("строка живости потеряна:\n%s", out)
	}
	if strings.Contains(out, "пропущено") {
		t.Errorf("оговорка о пропусках появилась при нуле пропусков:\n%s", out)
	}
}
