package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/calmcacil/sonarr-anime-bridge/internal/anilist"

	"github.com/calmcacil/sonarr-anime-bridge/internal/cache"
	"github.com/calmcacil/sonarr-anime-bridge/internal/config"
	"github.com/calmcacil/sonarr-anime-bridge/internal/mapping"
)

type capturedLog struct {
	level slog.Level
	msg   string
	attrs map[string]any
}

type captureHandler struct {
	mu      sync.Mutex
	records []capturedLog
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool {
	return true
}

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	attrs := make(map[string]any)
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.Any()
		return true
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, capturedLog{level: r.Level, msg: r.Message, attrs: attrs})
	return nil
}

func (h *captureHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := &captureHandler{}
	next.records = h.records
	return next
}

func (h *captureHandler) WithGroup(string) slog.Handler {
	return h
}

func captureSchedulerLogs(t *testing.T, fn func()) []capturedLog {
	t.Helper()
	handler := &captureHandler{}
	old := slog.Default()
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() {
		slog.SetDefault(old)
	})

	fn()

	handler.mu.Lock()
	defer handler.mu.Unlock()
	out := make([]capturedLog, len(handler.records))
	copy(out, handler.records)
	return out
}

type testFetcher struct{}

func (testFetcher) FetchYear(context.Context, int) ([]anilist.Show, error) {
	return []anilist.Show{}, nil
}

type cancelAwareFetcher struct {
	started chan struct{}
	once    sync.Once
}

func (f *cancelAwareFetcher) FetchYear(ctx context.Context, _ int) ([]anilist.Show, error) {
	f.once.Do(func() { close(f.started) })
	<-ctx.Done()
	return nil, ctx.Err()
}

func newTestCache(t *testing.T) *cache.Cache {
	t.Helper()
	c, err := cache.Open(":memory:")
	if err != nil {
		t.Fatalf("cache.Open: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func ptr[T any](v T) *T {
	return &v
}

func TestProcessContext_AllDoesNotMergePriorYearWinterOverflow(t *testing.T) {
	c := newTestCache(t)
	cfg := &config.Config{
		IncludeTypes:        []string{"TV"},
		FilterFutureEnabled: false,
	}
	currentShow := anilist.Show{
		ID:     1,
		IDMal:  ptr(101),
		Title:  anilist.Title{English: ptr("Current")},
		Format: "TV",
		Season: "SPRING",
	}
	priorDecemberShow := anilist.Show{
		ID:        2,
		IDMal:     ptr(102),
		Title:     anilist.Title{English: ptr("Prior December")},
		Format:    "TV",
		Season:    "WINTER",
		StartDate: anilist.FuzzyDate{Month: ptr(12)},
	}
	priorData, err := json.Marshal([]anilist.Show{priorDecemberShow})
	if err != nil {
		t.Fatalf("marshal prior data: %v", err)
	}
	if err := c.SetYear(2025, priorData); err != nil {
		t.Fatalf("set prior year: %v", err)
	}
	currentData, err := json.Marshal([]anilist.Show{currentShow})
	if err != nil {
		t.Fatalf("marshal current data: %v", err)
	}
	s := NewWithFetcher(c, cfg, testFetcher{})
	s.resolver.SetMapping(mapping.NewAnibridgeMapping(map[int]int{101: 1001, 102: 1002}, nil))

	shows, err := s.ProcessContext(context.Background(), currentData, "ALL", 2026, "series")
	if err != nil {
		t.Fatalf("ProcessContext: %v", err)
	}
	if len(shows) != 1 {
		t.Fatalf("len(shows) = %d, want 1: %#v", len(shows), shows)
	}
	if shows[0].TVDBID != 1001 {
		t.Fatalf("TVDBID = %d, want 1001", shows[0].TVDBID)
	}
}

func TestProcessContext_WinterMergesPriorYearDecemberOverflow(t *testing.T) {
	c := newTestCache(t)
	cfg := &config.Config{
		IncludeTypes:        []string{"TV"},
		FilterFutureEnabled: false,
	}
	currentShow := anilist.Show{
		ID:        1,
		IDMal:     ptr(101),
		Title:     anilist.Title{English: ptr("Current Winter")},
		Format:    "TV",
		Season:    "WINTER",
		StartDate: anilist.FuzzyDate{Month: ptr(1)},
	}
	priorDecemberShow := anilist.Show{
		ID:        2,
		IDMal:     ptr(102),
		Title:     anilist.Title{English: ptr("Prior December")},
		Format:    "TV",
		Season:    "WINTER",
		StartDate: anilist.FuzzyDate{Month: ptr(12)},
	}
	priorData, err := json.Marshal([]anilist.Show{priorDecemberShow})
	if err != nil {
		t.Fatalf("marshal prior data: %v", err)
	}
	if err := c.SetYear(2025, priorData); err != nil {
		t.Fatalf("set prior year: %v", err)
	}
	currentData, err := json.Marshal([]anilist.Show{currentShow})
	if err != nil {
		t.Fatalf("marshal current data: %v", err)
	}
	s := NewWithFetcher(c, cfg, testFetcher{})
	s.resolver.SetMapping(mapping.NewAnibridgeMapping(map[int]int{101: 1001, 102: 1002}, nil))

	shows, err := s.ProcessContext(context.Background(), currentData, "WINTER", 2026, "series")
	if err != nil {
		t.Fatalf("ProcessContext: %v", err)
	}
	if len(shows) != 2 {
		t.Fatalf("len(shows) = %d, want 2: %#v", len(shows), shows)
	}
}

func TestFetchAndStore_InflightErrorPropagation(t *testing.T) {
	c := newTestCache(t)
	cfg := &config.Config{
		IncludeTypes: []string{"TV", "ONA"},
	}
	s := NewWithFetcher(c, cfg, testFetcher{})

	// Pre-populate an inflight result to simulate an in-flight year fetch.
	// This avoids the timing race where the fetcher completes before
	// waiters call LoadOrStore.
	result := &inflightResult{done: make(chan struct{})}
	s.inflight.Store(2026, result)

	// Waiter calls FetchAndStore — should find the inflight entry and block.
	waiterErr := make(chan error, 1)
	go func() {
		waiterErr <- s.FetchAndStore(context.Background(), 2026, "test")
	}()

	// Signal the waiter with a simulated fetch error.
	testErr := errors.New("simulated fetch failure")
	result.err = testErr
	close(result.done)

	select {
	case err := <-waiterErr:
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "simulated fetch failure") {
			t.Errorf("error = %v, want simulated fetch failure", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter did not return within 5s")
	}

	// Also verify that concurrent callers don't panic (regression test).
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.FetchAndStore(context.Background(), 2025, "test")
		}()
	}
	wg.Wait()
}

func TestStartBackgroundRetriesResolverLoadWhileUnloaded(t *testing.T) {
	c := newTestCache(t)
	cfg := &config.Config{}
	s := NewWithFetcher(c, cfg, testFetcher{})
	s.resolverRetryInterval = 10 * time.Millisecond

	var attempts atomic.Int32
	s.loadMapping = func(context.Context, string, string) (*mapping.AnibridgeMapping, mapping.Metadata, error) {
		if attempts.Add(1) == 1 {
			return nil, mapping.Metadata{}, errors.New("temporary mapping failure")
		}
		return mapping.NewAnibridgeMapping(map[int]int{101: 1001}, nil), mapping.Metadata{}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.LoadResolverContext(ctx)
	if s.ResolverLoaded() {
		t.Fatal("resolver loaded after initial failing attempt")
	}

	s.StartBackground(ctx)
	deadline := time.After(2 * time.Second)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for !s.ResolverLoaded() {
		select {
		case <-deadline:
			t.Fatalf("resolver did not load after retry; attempts=%d", attempts.Load())
		case <-tick.C:
		}
	}

	cancel()
	waitCtx, waitCancel := context.WithTimeout(context.Background(), time.Second)
	defer waitCancel()
	if err := s.Wait(waitCtx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if attempts.Load() < 2 {
		t.Fatalf("attempts = %d, want at least 2", attempts.Load())
	}
}

func TestBackgroundFetchContextCanceledWhenAppContextCanceled(t *testing.T) {
	c := newTestCache(t)
	cfg := &config.Config{IncludeTypes: []string{"TV"}}
	fetcher := &cancelAwareFetcher{started: make(chan struct{})}
	s := NewWithFetcher(c, cfg, fetcher)

	appCtx, appCancel := context.WithCancel(context.Background())
	s.StartBackground(appCtx)
	t.Cleanup(func() {
		appCancel()
		waitCtx, waitCancel := context.WithTimeout(context.Background(), time.Second)
		defer waitCancel()
		if err := s.Wait(waitCtx); err != nil {
			t.Fatalf("Wait: %v", err)
		}
	})

	fetchCtx, cancel := s.BackgroundFetchContext(90 * time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- s.FetchAndStore(fetchCtx, 2026, "stale_refresh")
	}()

	select {
	case <-fetcher.started:
	case <-time.After(time.Second):
		t.Fatal("fetch did not start")
	}

	appCancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("FetchAndStore error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("fetch was not canceled by app context")
	}
}

func TestBackgroundFetchContextPreservesPerFetchTimeout(t *testing.T) {
	c := newTestCache(t)
	cfg := &config.Config{IncludeTypes: []string{"TV"}}
	fetcher := &cancelAwareFetcher{started: make(chan struct{})}
	s := NewWithFetcher(c, cfg, fetcher)

	appCtx, appCancel := context.WithCancel(context.Background())
	defer appCancel()
	s.StartBackground(appCtx)
	t.Cleanup(func() {
		appCancel()
		waitCtx, waitCancel := context.WithTimeout(context.Background(), time.Second)
		defer waitCancel()
		if err := s.Wait(waitCtx); err != nil {
			t.Fatalf("Wait: %v", err)
		}
	})

	fetchCtx, cancel := s.BackgroundFetchContext(time.Millisecond)
	defer cancel()
	err := s.FetchAndStore(fetchCtx, 2026, "winter_overflow")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("FetchAndStore error = %v, want context.DeadlineExceeded", err)
	}
}

func TestStartBackgroundFetchIsWaitedOn(t *testing.T) {
	c := newTestCache(t)
	cfg := &config.Config{IncludeTypes: []string{"TV"}}
	s := NewWithFetcher(c, cfg, testFetcher{})

	appCtx, appCancel := context.WithCancel(context.Background())
	s.StartBackground(appCtx)
	t.Cleanup(func() {
		appCancel()
		waitCtx, waitCancel := context.WithTimeout(context.Background(), time.Second)
		defer waitCancel()
		if err := s.Wait(waitCtx); err != nil {
			t.Fatalf("Wait cleanup: %v", err)
		}
	})

	started := make(chan struct{})
	release := make(chan struct{})
	s.StartBackgroundFetch(time.Second, func(context.Context) {
		close(started)
		<-release
	})

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("background fetch did not start")
	}

	appCancel()
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer waitCancel()
	if err := s.Wait(waitCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait before release = %v, want context deadline exceeded", err)
	}

	close(release)
	waitCtx, waitCancel = context.WithTimeout(context.Background(), time.Second)
	defer waitCancel()
	if err := s.Wait(waitCtx); err != nil {
		t.Fatalf("Wait after release: %v", err)
	}
}

func TestTrackNewMappings_FirstRunSeedsSilently(t *testing.T) {
	c := newTestCache(t)
	cfg := &config.Config{
		IncludeTypes:        []string{"TV"},
		FilterFutureEnabled: false,
	}
	s := NewWithFetcher(c, cfg, testFetcher{})
	s.resolver.SetMapping(mapping.NewAnibridgeMapping(
		map[int]int{101: 1001, 102: 1002},
		nil,
	))

	ctx := context.Background()
	anilistShows := []anilist.Show{
		{ID: 1, IDMal: ptr(101), Title: anilist.Title{English: ptr("Show One")}, Format: "TV"},
		{ID: 2, IDMal: ptr(102), Title: anilist.Title{English: ptr("Show Two")}, Format: "TV"},
	}
	batch := s.resolver.ResolveBatch(anilistShows)

	// First run: should seed silently (no logs), verify by checking DB
	firstLogs := captureSchedulerLogs(t, func() {
		s.trackNewMappings(ctx, anilistShows, batch, "SUMMER", 2026)
	})
	for _, log := range firstLogs {
		if log.msg == "new mappings discovered" || log.msg == "mapping added" {
			t.Fatalf("first run unexpectedly logged mapping event %q", log.msg)
		}
	}
	// Second run with same shows: should not add any new mappings or log events
	duplicateLogs := captureSchedulerLogs(t, func() {
		s.trackNewMappings(ctx, anilistShows, batch, "SUMMER", 2026)
	})
	for _, log := range duplicateLogs {
		if log.msg == "new mappings discovered" || log.msg == "mapping added" {
			t.Fatalf("duplicate run unexpectedly logged mapping event %q", log.msg)
		}
	}

	count, err := c.CountSeenMappings(ctx)
	if err != nil {
		t.Fatalf("CountSeenMappings: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 seen mappings after first run, got %d", count)
	}

	count, err = c.CountSeenMappings(ctx)
	if err != nil {
		t.Fatalf("CountSeenMappings: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 seen mappings after duplicate, got %d", count)
	}
}

func TestTrackNewMappings_AddsNewOnSubsequentRuns(t *testing.T) {
	c := newTestCache(t)
	cfg := &config.Config{
		IncludeTypes:        []string{"TV"},
		FilterFutureEnabled: false,
	}
	s := NewWithFetcher(c, cfg, testFetcher{})
	s.resolver.SetMapping(mapping.NewAnibridgeMapping(
		map[int]int{101: 1001, 102: 1002, 103: 1003},
		nil,
	))

	ctx := context.Background()

	// First run: seed 2 shows silently
	firstBatch := []anilist.Show{
		{ID: 1, IDMal: ptr(101), Title: anilist.Title{English: ptr("Show One")}, Format: "TV"},
		{ID: 2, IDMal: ptr(102), Title: anilist.Title{English: ptr("Show Two")}, Format: "TV"},
	}
	firstBatchResolved := s.resolver.ResolveBatch(firstBatch)
	s.trackNewMappings(ctx, firstBatch, firstBatchResolved, "SUMMER", 2026)

	count, err := c.CountSeenMappings(ctx)
	if err != nil {
		t.Fatalf("CountSeenMappings: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 seen mappings after first run, got %d", count)
	}

	// Second run: add 1 new show
	secondBatch := []anilist.Show{
		{ID: 1, IDMal: ptr(101), Title: anilist.Title{English: ptr("Show One")}, Format: "TV"},
		{ID: 2, IDMal: ptr(102), Title: anilist.Title{English: ptr("Show Two")}, Format: "TV"},
		{ID: 3, IDMal: ptr(103), Title: anilist.Title{English: ptr("Show Three")}, Format: "TV"},
	}
	secondBatchResolved := s.resolver.ResolveBatch(secondBatch)
	logs := captureSchedulerLogs(t, func() {
		s.trackNewMappings(ctx, secondBatch, secondBatchResolved, "SUMMER", 2026)
	})

	var aggregate *capturedLog
	var details []capturedLog
	for i := range logs {
		switch logs[i].msg {
		case "new mappings discovered":
			if logs[i].level != slog.LevelInfo {
				t.Fatalf("aggregate level = %v, want INFO", logs[i].level)
			}
			if aggregate != nil {
				t.Fatal("saw multiple aggregate mapping logs")
			}
			aggregate = &logs[i]
		case "mapping added":
			if logs[i].level != slog.LevelDebug {
				t.Fatalf("mapping detail level = %v, want DEBUG", logs[i].level)
			}
			details = append(details, logs[i])
		}
	}
	if aggregate == nil {
		t.Fatal("missing aggregate mapping log")
	}
	wantAggregate := map[string]any{
		"type":   "mapping",
		"count":  int64(1),
		"season": "SUMMER",
		"year":   int64(2026),
	}
	for key, want := range wantAggregate {
		if got := aggregate.attrs[key]; got != want {
			t.Fatalf("aggregate %s = %#v, want %#v", key, got, want)
		}
	}
	for _, key := range []string{"title", "tvdbid"} {
		if _, ok := aggregate.attrs[key]; ok {
			t.Fatalf("aggregate unexpectedly includes %q", key)
		}
	}
	if len(details) != 1 {
		t.Fatalf("mapping detail count = %d, want 1", len(details))
	}
	if got := details[0].attrs["tvdbid"]; got != int64(1003) {
		t.Fatalf("mapping detail tvdbid = %#v, want 1003", got)
	}
	if got := details[0].attrs["title"]; got != "Show Three" {
		t.Fatalf("mapping detail title = %#v, want Show Three", got)
	}

	count, err = c.CountSeenMappings(ctx)
	if err != nil {
		t.Fatalf("CountSeenMappings: %v", err)
	}
	if count != 3 {
		t.Fatalf("expected 3 seen mappings after adding new show, got %d", count)
	}
}

func TestTrackNewMappings_DifferentSeasonIsSeparate(t *testing.T) {
	c := newTestCache(t)
	cfg := &config.Config{
		IncludeTypes:        []string{"TV"},
		FilterFutureEnabled: false,
	}
	s := NewWithFetcher(c, cfg, testFetcher{})
	s.resolver.SetMapping(mapping.NewAnibridgeMapping(
		map[int]int{101: 1001},
		nil,
	))

	ctx := context.Background()
	shows := []anilist.Show{
		{ID: 1, IDMal: ptr(101), Title: anilist.Title{English: ptr("Show One")}, Format: "TV"},
	}
	batch := s.resolver.ResolveBatch(shows)

	// First run with summer 2026
	s.trackNewMappings(ctx, shows, batch, "SUMMER", 2026)

	// Same TVDB ID but different season should be tracked separately
	s.trackNewMappings(ctx, shows, batch, "FALL", 2026)

	// Same TVDB ID but different year should be tracked separately
	s.trackNewMappings(ctx, shows, batch, "SUMMER", 2027)

	count, err := c.CountSeenMappings(ctx)
	if err != nil {
		t.Fatalf("CountSeenMappings: %v", err)
	}
	if count != 3 {
		t.Fatalf("expected 3 distinct season/year entries, got %d", count)
	}
}

func TestTrackNewMappings_NoResolverSkipsGracefully(t *testing.T) {
	c := newTestCache(t)
	cfg := &config.Config{
		IncludeTypes:        []string{"TV"},
		FilterFutureEnabled: false,
	}
	s := NewWithFetcher(c, cfg, testFetcher{})
	// No resolver mapping set

	ctx := context.Background()
	shows := []anilist.Show{
		{ID: 1, IDMal: ptr(101), Title: anilist.Title{English: ptr("Show One")}, Format: "TV"},
	}
	// Should not panic or error
	s.trackNewMappings(ctx, shows, nil, "SUMMER", 2026)

	count, err := c.CountSeenMappings(ctx)
	if err != nil {
		t.Fatalf("CountSeenMappings: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 seen mappings with no resolver, got %d", count)
	}
}

func TestProcessContext_WithTracking(t *testing.T) {
	c := newTestCache(t)
	cfg := &config.Config{
		IncludeTypes:        []string{"TV"},
		FilterFutureEnabled: false,
	}
	s := NewWithFetcher(c, cfg, testFetcher{})
	s.resolver.SetMapping(mapping.NewAnibridgeMapping(
		map[int]int{101: 1001, 102: 1002},
		nil,
	))

	ctx := context.Background()

	data, err := json.Marshal([]anilist.Show{
		{ID: 1, IDMal: ptr(101), Title: anilist.Title{English: ptr("Show One")}, Format: "TV", Season: "SUMMER"},
		{ID: 2, IDMal: ptr(102), Title: anilist.Title{English: ptr("Show Two")}, Format: "TV", Season: "SUMMER"},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// ProcessContext should trigger tracking internally
	shows, err := s.ProcessContext(ctx, data, "SUMMER", 2026, "series")
	if err != nil {
		t.Fatalf("ProcessContext: %v", err)
	}
	if len(shows) != 2 {
		t.Fatalf("expected 2 shows, got %d", len(shows))
	}

	// Verify tracking was recorded
	count, err := c.CountSeenMappings(ctx)
	if err != nil {
		t.Fatalf("CountSeenMappings: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 seen mappings after ProcessContext, got %d", count)
	}

	// Second call should not add duplicates
	shows, err = s.ProcessContext(ctx, data, "SUMMER", 2026, "series")
	if err != nil {
		t.Fatalf("ProcessContext (second): %v", err)
	}
	if len(shows) != 2 {
		t.Fatalf("expected 2 shows on second call, got %d", len(shows))
	}

	count, err = c.CountSeenMappings(ctx)
	if err != nil {
		t.Fatalf("CountSeenMappings: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected still 2 seen mappings after second call, got %d", count)
	}
}

func TestProcessContext_LogsAggregateFilterStats(t *testing.T) {
	c := newTestCache(t)
	cfg := &config.Config{
		IncludeTypes:         []string{"TV"},
		ExcludeTags:          []string{"Hentai"},
		FilterFutureEnabled:  true,
		AnibridgeMappingPath: "/tmp/unused",
	}
	s := NewWithFetcher(c, cfg, testFetcher{})
	s.resolver.SetMapping(mapping.NewAnibridgeMapping(map[int]int{101: 1001}, nil))

	data, err := json.Marshal([]anilist.Show{
		{ID: 1, IDMal: ptr(101), Title: anilist.Title{English: ptr("Resolved")}, Format: "TV", Season: "SUMMER", Duration: ptr(24), StartDate: anilist.FuzzyDate{Year: ptr(2020), Month: ptr(7)}},
		{ID: 2, Title: anilist.Title{English: ptr("Short")}, Format: "TV", Season: "SUMMER", Duration: ptr(10), StartDate: anilist.FuzzyDate{Year: ptr(2020), Month: ptr(7)}},
		{ID: 3, Title: anilist.Title{English: ptr("Tagged")}, Format: "TV", Season: "SUMMER", Duration: ptr(24), Tags: []anilist.Tag{{Name: "Hentai"}}, StartDate: anilist.FuzzyDate{Year: ptr(2020), Month: ptr(7)}},
		{ID: 4, Title: anilist.Title{English: ptr("Future")}, Format: "TV", Season: "SUMMER", Duration: ptr(24), StartDate: anilist.FuzzyDate{Year: ptr(2099), Month: ptr(7)}},
		{ID: 5, Title: anilist.Title{English: ptr("Movie")}, Format: "MOVIE", Season: "SUMMER", Duration: ptr(24), StartDate: anilist.FuzzyDate{Year: ptr(2020), Month: ptr(7)}},
		{ID: 6, Title: anilist.Title{English: ptr("Prequel")}, Format: "TV", Season: "SUMMER", Duration: ptr(24), StartDate: anilist.FuzzyDate{Year: ptr(2020), Month: ptr(7)}, Relations: &anilist.RelationBlock{Edges: []anilist.RelationEdge{{RelationType: "PREQUEL"}}}},
		{ID: 7, Title: anilist.Title{English: ptr("Unresolved")}, Format: "TV", Season: "SUMMER", Duration: ptr(24), StartDate: anilist.FuzzyDate{Year: ptr(2020), Month: ptr(7)}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	logs := captureSchedulerLogs(t, func() {
		shows, err := s.ProcessContext(context.Background(), data, "SUMMER", 2026, "series-new")
		if err != nil {
			t.Fatalf("ProcessContext: %v", err)
		}
		if len(shows) != 1 {
			t.Fatalf("len(shows) = %d, want 1", len(shows))
		}
	})

	var aggregate *capturedLog
	for i := range logs {
		if logs[i].msg == "processed filters" {
			if aggregate != nil {
				t.Fatalf("saw multiple aggregate filter logs")
			}
			aggregate = &logs[i]
		}
		if strings.HasPrefix(logs[i].msg, "skipped show") {
			t.Fatalf("unexpected per-show filter log: %q", logs[i].msg)
		}
	}
	if aggregate == nil {
		t.Fatal("missing aggregate filter log")
	}
	for _, key := range []string{"title", "tags"} {
		if _, ok := aggregate.attrs[key]; ok {
			t.Fatalf("aggregate log unexpectedly includes %q", key)
		}
	}

	want := map[string]any{
		"type":                  "filter",
		"year":                  int64(2026),
		"season":                "SUMMER",
		"category":              "series-new",
		"input":                 int64(7),
		"after_winter_overflow": int64(7),
		"after_season":          int64(7),
		"after_format":          int64(6),
		"skipped_duration":      int64(1),
		"skipped_tags":          int64(1),
		"skipped_future":        int64(1),
		"skipped_first_season":  int64(1),
		"resolved":              int64(1),
		"unresolved":            int64(1),
	}
	for key, value := range want {
		if got := aggregate.attrs[key]; got != value {
			t.Fatalf("%s = %#v, want %#v", key, got, value)
		}
	}
	if _, ok := aggregate.attrs["duration_ms"]; !ok {
		t.Fatal("aggregate log missing duration_ms")
	}
}
