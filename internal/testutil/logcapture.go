package testutil

import (
	"context"
	"log/slog"
	"sync"
	"testing"
)

// LogRecord is a captured slog record with its attributes flattened by key.
type LogRecord struct {
	Level slog.Level
	Msg   string
	Attrs map[string]slog.Value
}

// LogCapture is a goroutine-safe slog.Handler that keeps every record at or
// above its minimum level. Handlers derived via WithAttrs share the records.
type LogCapture struct {
	min     slog.Level
	attrs   []slog.Attr
	mu      *sync.Mutex
	records *[]LogRecord
}

// CaptureLogs installs a LogCapture as the default slog logger until the test
// ends. Tests using it must not run in parallel.
func CaptureLogs(t testing.TB, min slog.Level) *LogCapture {
	t.Helper()
	h := &LogCapture{min: min, mu: &sync.Mutex{}, records: &[]LogRecord{}}
	previous := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return h
}

func (h *LogCapture) Enabled(_ context.Context, level slog.Level) bool { return level >= h.min }

func (h *LogCapture) Handle(_ context.Context, r slog.Record) error {
	attrs := make(map[string]slog.Value, len(h.attrs)+r.NumAttrs())
	for _, a := range h.attrs {
		attrs[a.Key] = a.Value
	}
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value
		return true
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	*h.records = append(*h.records, LogRecord{Level: r.Level, Msg: r.Message, Attrs: attrs})
	return nil
}

func (h *LogCapture) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := *h
	next.attrs = append(append([]slog.Attr(nil), h.attrs...), attrs...)
	return &next
}

func (h *LogCapture) WithGroup(string) slog.Handler { return h }

// Records returns a snapshot of the captured records.
func (h *LogCapture) Records() []LogRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]LogRecord(nil), *h.records...)
}

// Find returns the first captured record with the given message.
func (h *LogCapture) Find(msg string) (LogRecord, bool) {
	for _, r := range h.Records() {
		if r.Msg == msg {
			return r, true
		}
	}
	return LogRecord{}, false
}
