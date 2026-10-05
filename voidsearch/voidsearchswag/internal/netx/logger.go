package netx

type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
}
