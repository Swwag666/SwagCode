package netx

import (
	"testing"
)

func TestBootstrapPctParsing(t *testing.T) {
	cases := []struct {
		phase string
		want  string
	}{
		{"NOTICE BOOTSTRAP PROGRESS=100 TAG=done SUMMARY=\"Done\"", "100%"},
		{"NOTICE BOOTSTRAP PROGRESS=45 TAG=requesting_descriptors", "45%"},
		{"NOTICE BOOTSTRAP PROGRESS=0", "0%"},
		{"NOTICE BOOTSTRAP", ""},
		{"", ""},
		{"PROGRESS=", ""},
	}
	for _, c := range cases {
		if got := bootstrapPct(c.phase); got != c.want {
			t.Errorf("bootstrapPct(%q)=%q, ожидала %q", c.phase, got, c.want)
		}
	}
}

func TestBootstrapPctNoPanicOnLongInput(t *testing.T) {
	long := "PROGRESS=99 " + string(make([]byte, 500))
	if got := bootstrapPct(long); got != "99%" {
		t.Errorf("bootstrapPct на длинном вводе=%q, ожидала 99%%", got)
	}
}

func TestBootstrapCompleteString(t *testing.T) {
	complete := "NOTICE BOOTSTRAP PROGRESS=100 TAG=done SUMMARY=\"Done\""
	if !containsProgress100(complete) {
		t.Error("строка завершения bootstrap не распознана")
	}
	if containsProgress100("PROGRESS=99 TAG=circuit_create") {
		t.Error("неполный bootstrap распознан как завершённый")
	}
}

func containsProgress100(s string) bool {
	for i := 0; i+len("PROGRESS=100") <= len(s); i++ {
		if s[i:i+len("PROGRESS=100")] == "PROGRESS=100" {
			return true
		}
	}
	return false
}
