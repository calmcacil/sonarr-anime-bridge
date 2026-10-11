package mapping

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/calmcacil/sonarr-anime-bridge/internal/anilist"
)

type swappingLogHandler struct {
	once  sync.Once
	swap  func()
	calls atomic.Int64
}

func (h *swappingLogHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *swappingLogHandler) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *swappingLogHandler) WithGroup(string) slog.Handler            { return h }
func (h *swappingLogHandler) Handle(_ context.Context, record slog.Record) error {
	if record.Message == "resolved via anibridge (MAL)" || record.Message == "resolved via anibridge (AniList fallback)" {
		h.calls.Add(1)
		h.once.Do(h.swap)
	}
	return nil
}

func TestResolveBatchCapturesOneSnapshot(t *testing.T) {
	r := NewResolver()
	r.SetMapping(NewAnibridgeMapping(map[int]int{1: 101, 2: 102}, map[int]int{3: 103}))
	// Swap at the first lookup's debug event, before its result is returned.
	h := &swappingLogHandler{swap: func() { r.SetMapping(NewAnibridgeMapping(map[int]int{1: 201, 2: 202}, map[int]int{3: 203})) }}
	previous := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(previous) })
	input := []anilist.Show{showWithMAL(1, 1, "First"), showWithMAL(2, 2, "Second"), showWithMAL(3, 999, "Fallback"), showAnilistOnly(4, "Unresolved")}
	first := r.ResolveBatch(input)
	next := r.ResolveBatch(input)
	for id := 1; id <= 3; id++ {
		if first[id].TVDBID != 100+id || next[id].TVDBID != 200+id || !first[id].Resolved || first[id].Title != input[id-1].DisplayTitle() {
			t.Fatalf("id %d: first=%+v, next=%+v", id, first[id], next[id])
		}
	}
	if first[4].Resolved || next[4].Resolved || h.calls.Load() != 6 {
		t.Fatalf("omission/debug behavior changed: first=%+v, calls=%d", first, h.calls.Load())
	}
}
