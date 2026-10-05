package filex

import "testing"

// Правило приведения метки обязано совпадать с тем, что делает VerdictIsRisk,
// иначе решение «предупреждать ли» и объяснение «о чём именно» расходятся.
func TestNormalizeVerdict(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"executable", "executable"},
		{"EXECUTABLE", "executable"},
		{"Executable", "executable"},
		{"  secret ", "secret"},
		{"\tSecret\n", "secret"},
		{"", ""},
		{"   ", ""},
		{"other", "other"},
	} {
		if got := NormalizeVerdict(c.in); got != c.want {
			t.Errorf("NormalizeVerdict(%q) = %q, хочу %q", c.in, got, c.want)
		}
	}
}

// Нормализация не меняет смысл VerdictIsRisk: она по-прежнему признаёт рисковые
// метки в любой записи и отвергает остальные.
func TestVerdictIsRiskUsesNormalizeVerdict(t *testing.T) {
	for _, v := range RiskVerdicts {
		for _, form := range []string{v, " " + v + " ", NormalizeVerdict(v)} {
			if !VerdictIsRisk(form) {
				t.Errorf("VerdictIsRisk(%q) = false", form)
			}
		}
	}
	for _, v := range []string{VerdictArchive, VerdictEbook, VerdictUnknown, "", "  "} {
		if VerdictIsRisk(v) {
			t.Errorf("VerdictIsRisk(%q) = true", v)
		}
	}
}

// Каждая рисковая метка обязана отличаться от прочих: объяснение, которое
// одинаково для executable и secret, не помогает решить, открывать файл или нет.
func TestRiskVerdictsAreDistinct(t *testing.T) {
	seen := make(map[string]bool, len(RiskVerdicts))
	for _, v := range RiskVerdicts {
		n := NormalizeVerdict(v)
		if n != v {
			t.Errorf("RiskVerdicts содержит ненормализованную метку %q", v)
		}
		if seen[n] {
			t.Errorf("RiskVerdicts содержит дубль %q", n)
		}
		seen[n] = true
	}
	if len(RiskVerdicts) == 0 {
		t.Fatal("список рисковых меток пуст")
	}
}
