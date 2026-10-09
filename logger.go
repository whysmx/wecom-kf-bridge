package bridge

// Logger is deliberately small so callers can bridge it to slog, zap, or a
// structured logger without making protocol code depend on one implementation.
type Logger interface {
	Log(event string, fields map[string]any)
}

type LoggerFunc func(string, map[string]any)

func (f LoggerFunc) Log(event string, fields map[string]any) {
	if f != nil {
		f(event, fields)
	}
}

type nopLogger struct{}

func (nopLogger) Log(string, map[string]any) {}
