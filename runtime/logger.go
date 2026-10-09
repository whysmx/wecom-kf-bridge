// Package runtime contains process lifecycle, diagnostics and HTTP health
// primitives shared by the gateway entrypoint.  It intentionally has no
// dependency on the protocol adapters so it can be tested in isolation.
package runtime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Logger is the small logging boundary used by runtime components. Fields are
// copied and redacted before they are written; callers may safely reuse maps.
type Logger interface {
	Log(event string, fields map[string]any)
}

// StructuredLogger writes one JSON object per line. It never writes customer
// content or credentials, even when they are nested in a field value.
type StructuredLogger struct {
	Writer   io.Writer
	Clock    func() time.Time
	MinLevel string
	mu       sync.Mutex
}

// JSONLogger is kept as an alias for callers that prefer the format name.
type JSONLogger = StructuredLogger

func NewLogger(w io.Writer) *StructuredLogger {
	if w == nil {
		w = io.Discard
	}
	return &StructuredLogger{Writer: w, Clock: time.Now}
}
func NewStructuredLogger(w io.Writer) *StructuredLogger { return NewLogger(w) }

func (l *StructuredLogger) Log(event string, fields map[string]any) {
	l.write("INFO", event, fields)
}
func (l *StructuredLogger) Info(event string, fields map[string]any) { l.write("INFO", event, fields) }
func (l *StructuredLogger) Error(event string, err error, fields map[string]any) {
	cp := cloneFields(fields)
	if cp == nil {
		cp = map[string]any{}
	}
	if err != nil {
		cp["error_category"] = classifyError(err)
		cp["error"] = sanitizeText(err.Error())
	}
	l.write("ERROR", event, cp)
}
func (l *StructuredLogger) write(level, event string, fields map[string]any) {
	if l == nil || l.Writer == nil {
		return
	}
	if strings.TrimSpace(event) == "" {
		event = "event"
	}
	event = sanitizeText(event)
	ts := time.Now().UTC()
	if l.Clock != nil {
		ts = l.Clock().UTC()
	}
	record := map[string]any{"ts": ts.Format(time.RFC3339Nano), "level": level, "event": event}
	for k, v := range SanitizeFields(fields) {
		record[k] = v
	}
	data, err := json.Marshal(record)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = l.Writer.Write(append(data, '\n'))
}

// SanitizeFields removes or masks credentials and customer payloads. It is
// exported so HTTP middleware and adapter errors use exactly the same policy.
func SanitizeFields(fields map[string]any) map[string]any {
	if fields == nil {
		return nil
	}
	out := make(map[string]any, len(fields))
	for k, v := range fields {
		out[k] = sanitizeValue(k, v)
	}
	return out
}

var secretKey = regexp.MustCompile(`(?i)(token|secret|password|passwd|authorization|cookie|credential|api[_-]?key|private[_-]?key|access[_-]?token|aes[_-]?key|corpsecret|signature|echostr)`)
var bodyKey = regexp.MustCompile(`(?i)^(body|content|payload|raw|rawxml|xml|message|answer|prompt|response|text|media|attachment)$`)

func sanitizeValue(key string, v any) any {
	if secretKey.MatchString(key) {
		return "[REDACTED]"
	}
	if bodyKey.MatchString(key) {
		return "[REDACTED]"
	}
	switch x := v.(type) {
	case error:
		return sanitizeText(x.Error())
	case string:
		return sanitizeText(x)
	case []byte:
		return fmt.Sprintf("[bytes:%d]", len(x))
	case map[string]any:
		return SanitizeFields(x)
	case map[string]string:
		m := make(map[string]any, len(x))
		for k, v := range x {
			m[k] = sanitizeValue(k, v)
		}
		return m
	case []any:
		a := make([]any, len(x))
		for i, v := range x {
			a[i] = sanitizeValue(key, v)
		}
		return a
	case time.Time:
		return x.UTC().Format(time.RFC3339Nano)
	}
	rv := reflect.ValueOf(v)
	if rv.IsValid() && rv.Kind() == reflect.Pointer && !rv.IsNil() {
		return sanitizeValue(key, rv.Elem().Interface())
	}
	return v
}

func sanitizeText(s string) string {
	// Redact query parameters even when an error wraps an URL. Keep the path
	// and non-sensitive diagnostics useful for operations.
	if u, err := url.Parse(s); err == nil && u.RawQuery != "" {
		q := u.Query()
		for k := range q {
			if secretKey.MatchString(k) {
				q.Set(k, "[REDACTED]")
			}
		}
		u.RawQuery = q.Encode()
		s = u.String()
	}
	return regexp.MustCompile(`(?i)(token|secret|password|authorization|cookie|api[_-]?key|access[_-]?token)=([^&\s,]+)`).ReplaceAllString(s, "$1=[REDACTED]")
}
func classifyError(err error) string {
	if err == nil {
		return ""
	}
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "timeout"):
		return "timeout"
	case strings.Contains(s, "denied") || strings.Contains(s, "forbidden"):
		return "permission"
	case strings.Contains(s, "not found"):
		return "not_found"
	case strings.Contains(s, "invalid"):
		return "invalid"
	default:
		return "internal"
	}
}
func cloneFields(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// Fields returns a deterministic shallow field map useful for tests and
// adapters that add common identifiers to an event.
func Fields(kv ...any) map[string]any {
	out := map[string]any{}
	for i := 0; i+1 < len(kv); i += 2 {
		k, ok := kv[i].(string)
		if ok && k != "" {
			out[k] = kv[i+1]
		}
	}
	return out
}

// FormatJSON is a small helper for diagnostics and tests; unlike fmt it emits
// stable key order and never exposes redacted values.
func FormatJSON(fields map[string]any) string {
	b := bytes.Buffer{}
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(true)
	_ = enc.Encode(SanitizeFields(fields))
	return strings.TrimSpace(b.String())
}

var _ Logger = (*StructuredLogger)(nil)
var _ = sort.Strings
