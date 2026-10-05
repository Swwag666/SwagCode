package netx

import (
	"strings"
	"time"
)

func orStr(v, d string) string {
	if s := strings.TrimSpace(v); s != "" {
		return s
	}
	return d
}

func orInt(v, d int) int {
	if v > 0 {
		return v
	}
	return d
}

func orDur(d time.Duration, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func clipReason(s string) string { return clip(s, 70) }

func MaskSpec(spec string) string { return maskProxy(spec) }

func maskProxy(spec string) string {
	if spec == "" {
		return "direct"
	}
	i := strings.Index(spec, "://")
	if i < 0 {
		return "proxy"
	}
	scheme, rest := spec[:i], spec[i+3:]
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		rest = rest[at+1:]
	}
	return scheme + "://" + rest
}

func splitList(s string) []string {
	fs := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ';' || r == '\n' || r == '\r' || r == ' ' || r == '\t'
	})
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}
