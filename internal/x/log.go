package x

type Logger interface {
	Log(args ...any)
	Logf(format string, args ...any)
}

type NopLogger struct{}

func (NopLogger) Log(args ...any)                 {}
func (NopLogger) Logf(format string, args ...any) {}
