package scheduler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/calmcacil/sonarr-anime-bridge/internal/anilist"

	"github.com/calmcacil/sonarr-anime-bridge/internal/cache"
	"github.com/calmcacil/sonarr-anime-bridge/internal/config"
	"github.com/calmcacil/sonarr-anime-bridge/internal/mapping"
	"github.com/calmcacil/sonarr-anime-bridge/internal/testutil"
)

type testFetcher struct{}

func (testFetcher) FetchYear(context.Context, int) ([]anilist.Show, error) {
	return []anilist.Show{}, nil
}

type sequenceFetcher struct {
	mu        sync.Mutex
	calls     int
	responses func(call int) ([]anilist.Show, error)
}

func (f *sequenceFetcher) FetchYear(_ context.Context, _ int) ([]anilist.Show, error) {
	f.mu.Lock()
	f.calls++
	call := f.calls
	f.mu.Unlock()
	return f.responses(call)
}

func (f *sequenceFetcher) CallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
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

// openFileCache opens a file-backed cache plus a raw SQL handle to the same
// database so tests can inspect or rewrite year_cache columns directly.
func openFileCache(t *testing.T) (*cache.Cache, *sql.DB) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "cache.db")
	c, err := cache.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	sqlDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	return c, sqlDB
}

// newTestScheduler builds a scheduler with defaults for nil arguments: an
// in-memory cache, an empty config, and testFetcher. A non-nil ids map is
// installed as the resolver mapping (MAL ID -> TVDB ID).
func newTestScheduler(t *testing.T, c *cache.Cache, cfg *config.Config, fetcher yearFetcher, ids map[int]int) *Scheduler {
	t.Helper()
	if c == nil {
		c = newTestCache(t)
	}
	if cfg == nil {
		cfg = &config.Config{}
	}
	if fetcher == nil {
		fetcher = testFetcher{}
	}
	s := NewWithFetcher(c, cfg, fetcher)
	if ids != nil {
		s.resolver.SetMapping(mapping.NewAnibridgeMapping(ids, nil))
	}
	return s
}

// startBackground starts background work and registers cancel+Wait cleanup.
func startBackground(t *testing.T, s *Scheduler) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s.StartBackground(ctx)
	t.Cleanup(func() {
		cancel()
		waitCtx, waitCancel := context.WithTimeout(context.Background(), time.Second)
		defer waitCancel()
		if err := s.Wait(waitCtx); err != nil {
			t.Errorf("Wait cleanup: %v", err)
		}
	})
	return cancel
}

func tvShow(id, mal int, title, season string) anilist.Show {
	s := anilist.Show{ID: id, Title: anilist.Title{English: testutil.Ptr(title)}, Format: "TV", Season: season}
	if mal != 0 {
		s.IDMal = testutil.Ptr(mal)
	}
	return s
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}

func assertSeenMappingCount(t *testing.T, c *cache.Cache, want int) {
	t.Helper()
	got, err := c.CountSeenMappings(context.Background())
	if err != nil {
		t.Fatalf("CountSeenMappings: %v", err)
	}
	if got != want {
		t.Fatalf("seen mappings = %d, want %d", got, want)
	}
}

func mappingLogs(records []testutil.LogRecord) (aggregates, details []testutil.LogRecord) {
	for _, r := range records {
		if r.Attrs["task"].String() != "mapping_discovery" || r.Attrs["outcome"].String() != "succeeded" {
			continue
		}
		if _, detail := r.Attrs["tvdbid"]; detail {
			details = append(details, r)
		} else {
			aggregates = append(aggregates, r)
		}
	}
	return aggregates, details
}

func TestProcessContext_PriorYearWinterOverflow(t *testing.T) {
	tests := []struct {
		season      string
		current     anilist.Show
		wantTVDBIDs []int
	}{
		{season: "ALL", current: tvShow(1, 101, "Current", "SPRING"), wantTVDBIDs: []int{1001}},
		{season: "WINTER", current: tvShow(1, 101, "Current Winter", "WINTER"), wantTVDBIDs: []int{1001, 1002}},
	}
	for _, tt := range tests {
		t.Run(tt.season, func(t *testing.T) {
			s := newTestScheduler(t, nil, &config.Config{IncludeTypes: []string{"TV"}}, nil, map[int]int{101: 1001, 102: 1002})
			if tt.current.Season == "WINTER" {
				tt.current.StartDate = anilist.FuzzyDate{Month: testutil.Ptr(1)}
			}
			priorDecember := tvShow(2, 102, "Prior December", "WINTER")
			priorDecember.StartDate = anilist.FuzzyDate{Month: testutil.Ptr(12)}
			if err := s.cache.SetYearContext(context.Background(), 2025, mustMarshal(t, []anilist.Show{priorDecember})); err != nil {
				t.Fatalf("set prior year: %v", err)
			}

			shows, err := s.ProcessContext(context.Background(), mustMarshal(t, []anilist.Show{tt.current}), tt.season, 2026, "series")
			if err != nil {
				t.Fatalf("ProcessContext: %v", err)
			}
			got := make(map[int]bool, len(shows))
			for _, show := range shows {
				got[show.TVDBID] = true
			}
			if len(shows) != len(tt.wantTVDBIDs) {
				t.Fatalf("len(shows) = %d, want %d: %#v", len(shows), len(tt.wantTVDBIDs), shows)
			}
			for _, id := range tt.wantTVDBIDs {
				if !got[id] {
					t.Fatalf("missing TVDBID %d in %#v", id, shows)
				}
			}
		})
	}
}

func TestFetchAndStore_InflightErrorPropagation(t *testing.T) {
	s := newTestScheduler(t, nil, &config.Config{IncludeTypes: []string{"TV", "ONA"}}, nil, nil)

	// Pre-populate an inflight result to simulate an in-flight year fetch.
	// This avoids the timing race where the fetcher completes before
	// waiters call LoadOrStore.
	result := &inflightResult{done: make(chan struct{})}
	s.inflight.Store(2026, result)

	waiterErr := make(chan error, 1)
	go func() {
		waiterErr <- s.FetchAndStore(context.Background(), 2026, "test")
	}()

	result.err = errors.New("simulated fetch failure")
	close(result.done)

	select {
	case err := <-waiterErr:
		if err == nil || !strings.Contains(err.Error(), "simulated fetch failure") {
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

func TestFetchAndStoreCacheBehavior(t *testing.T) {
	tests := []struct {
		name     string
		seed     []byte
		result   []anilist.Show
		triggers []string
		wantIDs  []int
	}{
		{
			name:     "rechecks fresh cache before fetching",
			result:   []anilist.Show{{ID: 42}},
			triggers: []string{"stale_refresh", "delayed_stale_scan"},
			wantIDs:  []int{42},
		},
		{
			name:     "normalizes successful empty result",
			triggers: []string{"cache_miss"},
		},
		{
			name:     "recovers invalid fresh payload",
			seed:     []byte(`not-json`),
			result:   []anilist.Show{{ID: 42}},
			triggers: []string{"cache_recovery"},
			wantIDs:  []int{42},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			c := newTestCache(t)
			if tt.seed != nil {
				if err := c.SetYearContext(ctx, 2026, tt.seed); err != nil {
					t.Fatal(err)
				}
			}
			fetcher := &sequenceFetcher{responses: func(int) ([]anilist.Show, error) { return tt.result, nil }}
			s := newTestScheduler(t, c, nil, fetcher, nil)
			for _, trigger := range tt.triggers {
				if err := s.FetchAndStore(ctx, 2026, trigger); err != nil {
					t.Fatalf("FetchAndStore(%s): %v", trigger, err)
				}
			}
			if got := fetcher.CallCount(); got != 1 {
				t.Fatalf("fetch calls = %d, want 1", got)
			}

			data, fresh, ok, err := c.PeekYearContext(ctx, 2026)
			if err != nil {
				t.Fatal(err)
			}
			if !ok || !fresh {
				t.Fatalf("cache entry = (fresh %v, present %v), want fresh and present", fresh, ok)
			}
			if len(tt.wantIDs) == 0 && string(data) != `[]` {
				t.Fatalf("cached empty result = %q, want []", data)
			}
			var shows []anilist.Show
			if err := json.Unmarshal(data, &shows); err != nil {
				t.Fatalf("cached payload is invalid: %v", err)
			}
			if len(shows) != len(tt.wantIDs) {
				t.Fatalf("cached shows = %#v, want IDs %v", shows, tt.wantIDs)
			}
			for i, id := range tt.wantIDs {
				if shows[i].ID != id {
					t.Fatalf("cached shows = %#v, want IDs %v", shows, tt.wantIDs)
				}
			}
		})
	}
}

func TestFetchFailureCooldownPreservesStaleDataAndRetries(t *testing.T) {
	ctx := context.Background()
	c, sqlDB := openFileCache(t)
	if err := c.SetYearContext(ctx, 2020, []byte(`[]`)); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.Exec(`UPDATE year_cache SET fetched_at=0 WHERE year=2020`); err != nil {
		t.Fatal(err)
	}
	fetcher := &sequenceFetcher{responses: func(call int) ([]anilist.Show, error) {
		if call == 1 {
			return nil, errors.New("temporary AniList failure")
		}
		return []anilist.Show{{ID: 99}}, nil
	}}
	s := newTestScheduler(t, c, nil, fetcher, nil)
	s.fetchRetryBase = 250 * time.Millisecond
	s.fetchRetryMax = 500 * time.Millisecond

	if err := s.FetchAndStore(ctx, 2020, "stale_refresh"); err == nil {
		t.Fatal("first stale refresh unexpectedly succeeded")
	}
	if err := s.FetchAndStore(ctx, 2020, "winter_overflow"); err == nil || !strings.Contains(err.Error(), "cooling down") {
		t.Fatalf("second trigger error = %v, want cooldown", err)
	}
	if got := fetcher.CallCount(); got != 1 {
		t.Fatalf("fetch calls during cooldown = %d, want 1", got)
	}

	data, fresh, ok, err := c.PeekYearContext(ctx, 2020)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || fresh || string(data) != `[]` {
		t.Fatalf("stale cache after failed refresh = (%q, fresh %v, present %v)", data, fresh, ok)
	}
	var fetchedAt int64
	if err := sqlDB.QueryRow(`SELECT fetched_at FROM year_cache WHERE year=2020`).Scan(&fetchedAt); err != nil {
		t.Fatal(err)
	}
	if fetchedAt != 0 {
		t.Fatalf("failed fetch advanced fetched_at to %d, want 0", fetchedAt)
	}

	s.failureMu.Lock()
	failure := s.failures[2020]
	failure.retryAt = time.Now().Add(-time.Second)
	s.failures[2020] = failure
	s.failureMu.Unlock()
	if err := s.FetchAndStore(ctx, 2020, "stale_refresh"); err != nil {
		t.Fatalf("retry after cooldown: %v", err)
	}
	if got := fetcher.CallCount(); got != 2 {
		t.Fatalf("fetch calls after cooldown = %d, want 2", got)
	}
	data, fresh, ok, err = c.PeekYearContext(ctx, 2020)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || !fresh || !strings.Contains(string(data), `"id":99`) {
		t.Fatalf("cache after successful retry = (%q, fresh %v, present %v)", data, fresh, ok)
	}
}

func TestPrewarmFreshYearDoesNotExtendRetention(t *testing.T) {
	c, sqlDB := openFileCache(t)
	year := time.Now().Year()
	if err := c.SetYearContext(context.Background(), year, []byte(`[]`)); err != nil {
		t.Fatal(err)
	}
	const oldHit = int64(1000)
	if _, err := sqlDB.Exec(`UPDATE year_cache SET last_hit=? WHERE year=?`, oldHit, year); err != nil {
		t.Fatal(err)
	}
	c.SetLastHitDebounce(0)
	fetcher := &sequenceFetcher{responses: func(int) ([]anilist.Show, error) {
		return nil, errors.New("fresh year should not be fetched")
	}}
	s := newTestScheduler(t, c, &config.Config{PrewarmYears: []int{year}}, fetcher, nil)
	if err := s.Prewarm(context.Background()); err != nil {
		t.Fatalf("prewarm fresh year: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if got := fetcher.CallCount(); got != 0 {
		t.Fatalf("fetch calls = %d, want 0", got)
	}
	var lastHit int64
	if err := sqlDB.QueryRow(`SELECT last_hit FROM year_cache WHERE year=?`, year).Scan(&lastHit); err != nil {
		t.Fatal(err)
	}
	if lastHit != oldHit {
		t.Fatalf("prewarm advanced last_hit to %d, want %d", lastHit, oldHit)
	}
}

func TestStartBackgroundRetriesResolverLoadWhileUnloaded(t *testing.T) {
	s := newTestScheduler(t, nil, nil, nil, nil)
	s.resolverRetryInterval = 10 * time.Millisecond

	var attempts atomic.Int32
	s.loadMapping = func(context.Context, string, string) (*mapping.AnibridgeMapping, mapping.Metadata, error) {
		if attempts.Add(1) == 1 {
			return nil, mapping.Metadata{}, errors.New("temporary mapping failure")
		}
		return mapping.NewAnibridgeMapping(map[int]int{101: 1001}, nil), mapping.Metadata{}, nil
	}

	s.LoadResolverContext(context.Background())
	if s.ResolverLoaded() {
		t.Fatal("resolver loaded after initial failing attempt")
	}

	startBackground(t, s)
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
	if attempts.Load() < 2 {
		t.Fatalf("attempts = %d, want at least 2", attempts.Load())
	}
}

func TestBackgroundFetchContextCanceledWhenAppContextCanceled(t *testing.T) {
	fetcher := &cancelAwareFetcher{started: make(chan struct{})}
	s := newTestScheduler(t, nil, &config.Config{IncludeTypes: []string{"TV"}}, fetcher, nil)
	appCancel := startBackground(t, s)
	logs := testutil.CaptureLogs(t, slog.LevelDebug)

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
	terminals := 0
	for _, record := range logs.Records() {
		if record.Attrs["task"].String() != "year_fetch" || record.Attrs["outcome"].String() == "started" {
			continue
		}
		terminals++
		if record.Level != slog.LevelInfo || record.Attrs["outcome"].String() != "canceled" || record.Attrs["trigger"].String() != "stale_refresh" {
			t.Fatalf("shutdown fetch logged incorrectly: %v", record)
		}
		if record.Attrs["retry_in_ms"].Int64() != 0 {
			t.Fatal("shutdown cancellation introduced cooldown")
		}
	}
	if terminals != 1 {
		t.Fatalf("shutdown fetch terminals=%d, want 1", terminals)
	}
}

func TestBackgroundFetchContextPreservesPerFetchTimeout(t *testing.T) {
	fetcher := &cancelAwareFetcher{started: make(chan struct{})}
	s := newTestScheduler(t, nil, &config.Config{IncludeTypes: []string{"TV"}}, fetcher, nil)
	startBackground(t, s)

	fetchCtx, cancel := s.BackgroundFetchContext(time.Millisecond)
	defer cancel()
	if err := s.FetchAndStore(fetchCtx, 2026, "winter_overflow"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("FetchAndStore error = %v, want context.DeadlineExceeded", err)
	}
}

func TestStartBackgroundFetchIsWaitedOn(t *testing.T) {
	s := newTestScheduler(t, nil, &config.Config{IncludeTypes: []string{"TV"}}, nil, nil)
	appCancel := startBackground(t, s)

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

func TestTrackNewMappings(t *testing.T) {
	ctx := context.Background()
	cfg := &config.Config{IncludeTypes: []string{"TV"}}
	s := newTestScheduler(t, nil, cfg, nil, nil)
	c := s.cache
	one, two, three := tvShow(1, 101, "Show One", ""), tvShow(2, 102, "Show Two", ""), tvShow(3, 103, "Show Three", "")

	// Without a resolver mapping, tracking is a no-op.
	s.trackNewMappings(ctx, []anilist.Show{one}, nil, "SUMMER", 2026)
	assertSeenMappingCount(t, c, 0)

	s.resolver.SetMapping(mapping.NewAnibridgeMapping(map[int]int{101: 1001, 102: 1002, 103: 1003}, nil))
	track := func(shows []anilist.Show, season string, year int) []testutil.LogRecord {
		t.Helper()
		logs := testutil.CaptureLogs(t, slog.LevelDebug)
		s.trackNewMappings(ctx, shows, s.resolver.ResolveBatch(shows), season, year)
		return logs.Records()
	}
	assertSilent := func(step string, records []testutil.LogRecord) {
		t.Helper()
		if aggregates, details := mappingLogs(records); len(aggregates)+len(details) != 0 {
			t.Fatalf("%s unexpectedly logged mapping events: %v %v", step, aggregates, details)
		}
	}

	// First run seeds silently; repeating it adds nothing.
	assertSilent("first run", track([]anilist.Show{one, two}, "SUMMER", 2026))
	assertSeenMappingCount(t, c, 2)
	assertSilent("duplicate run", track([]anilist.Show{one, two}, "SUMMER", 2026))
	assertSeenMappingCount(t, c, 2)

	// A new show logs one INFO aggregate without titles plus DEBUG detail.
	aggregates, details := mappingLogs(track([]anilist.Show{one, two, three}, "SUMMER", 2026))
	assertSeenMappingCount(t, c, 3)
	if len(aggregates) != 1 || aggregates[0].Level != slog.LevelInfo {
		t.Fatalf("aggregate logs = %v, want one INFO record", aggregates)
	}
	for key, want := range map[string]any{"type": "mapping", "count": int64(1), "season": "SUMMER", "year": int64(2026)} {
		if got := aggregates[0].Attrs[key].Any(); got != want {
			t.Fatalf("aggregate %s = %#v, want %#v", key, got, want)
		}
	}
	for _, key := range []string{"title", "tvdbid"} {
		if _, ok := aggregates[0].Attrs[key]; ok {
			t.Fatalf("aggregate unexpectedly includes %q", key)
		}
	}
	if len(details) != 1 || details[0].Level != slog.LevelDebug {
		t.Fatalf("mapping detail logs = %v, want one DEBUG record", details)
	}
	if got := details[0].Attrs["tvdbid"].Any(); got != int64(1003) {
		t.Fatalf("mapping detail tvdbid = %#v, want 1003", got)
	}
	if got := details[0].Attrs["title"].Any(); got != "Show Three" {
		t.Fatalf("mapping detail title = %#v, want Show Three", got)
	}

	// ProcessContext tracks internally; a different season is tracked separately
	// and repeated calls do not duplicate.
	fallData := mustMarshal(t, []anilist.Show{tvShow(1, 101, "Show One", "FALL")})
	for range 2 {
		shows, err := s.ProcessContext(ctx, fallData, "FALL", 2026, "series")
		if err != nil {
			t.Fatalf("ProcessContext: %v", err)
		}
		if len(shows) != 1 {
			t.Fatalf("len(shows) = %d, want 1", len(shows))
		}
		assertSeenMappingCount(t, c, 4)
	}

	// A different year is tracked separately.
	track([]anilist.Show{one}, "SUMMER", 2027)
	assertSeenMappingCount(t, c, 5)
}

func TestProcessContext_LogsAggregateFilterStats(t *testing.T) {
	cfg := &config.Config{
		IncludeTypes:         []string{"TV"},
		ExcludeTags:          []string{"Hentai"},
		FilterFutureEnabled:  true,
		AnibridgeMappingPath: "/tmp/unused",
	}
	s := newTestScheduler(t, nil, cfg, nil, map[int]int{101: 1001})

	input := []anilist.Show{
		tvShow(1, 101, "Resolved", "SUMMER"),
		tvShow(2, 0, "Short", "SUMMER"),
		tvShow(3, 0, "Tagged", "SUMMER"),
		tvShow(4, 0, "Future", "SUMMER"),
		tvShow(5, 0, "Movie", "SUMMER"),
		tvShow(6, 0, "Prequel", "SUMMER"),
		tvShow(7, 0, "Unresolved", "SUMMER"),
	}
	for i := range input {
		input[i].Duration = testutil.Ptr(24)
		input[i].StartDate = anilist.FuzzyDate{Year: testutil.Ptr(2020), Month: testutil.Ptr(7)}
	}
	input[1].Duration = testutil.Ptr(10)
	input[2].Tags = []anilist.Tag{{Name: "Hentai"}}
	input[3].StartDate.Year = testutil.Ptr(2099)
	input[4].Format = "MOVIE"
	input[5].Relations = &anilist.RelationBlock{Edges: []anilist.RelationEdge{{RelationType: "PREQUEL"}}}

	logCapture := testutil.CaptureLogs(t, slog.LevelDebug)
	shows, err := s.ProcessContext(context.Background(), mustMarshal(t, input), "SUMMER", 2026, "series-new")
	if err != nil {
		t.Fatalf("ProcessContext: %v", err)
	}
	if len(shows) != 1 {
		t.Fatalf("len(shows) = %d, want 1", len(shows))
	}

	var aggregates []testutil.LogRecord
	for _, r := range logCapture.Records() {
		if r.Attrs["task"].String() == "show_process" {
			aggregates = append(aggregates, r)
		}
	}
	if len(aggregates) != 1 {
		t.Fatalf("aggregate filter logs = %d, want 1", len(aggregates))
	}
	want := map[string]int64{
		"input": 7, "after_format": 6, "skipped_duration": 1, "skipped_tags": 1,
		"skipped_future": 1, "skipped_first_season": 1, "resolved": 1, "unresolved": 1,
	}
	for key, value := range want {
		if got := aggregates[0].Attrs[key].Any(); got != value {
			t.Fatalf("%s = %#v, want %d", key, got, value)
		}
	}
}

func TestFetchAndStoreFailureLogsOwnedOnce(t *testing.T) {
	for _, panicFetch := range []bool{false, true} {
		t.Run(map[bool]string{false: "upstream error", true: "upstream panic"}[panicFetch], func(t *testing.T) {
			fetcher := &sequenceFetcher{responses: func(int) ([]anilist.Show, error) {
				if panicFetch {
					panic("upstream panic")
				}
				return nil, errors.New("upstream unavailable")
			}}
			s := newTestScheduler(t, nil, nil, fetcher, nil)
			logs := testutil.CaptureLogs(t, slog.LevelDebug)
			for _, trigger := range []string{"cache_miss", "stale_refresh"} {
				if err := s.FetchAndStore(context.Background(), 2026, trigger); err == nil {
					t.Fatal("failed attempt or cooldown returned success")
				}
			}
			starts, failures, skips := 0, 0, 0
			for _, record := range logs.Records() {
				if record.Attrs["task"].String() != "year_fetch" {
					continue
				}
				if record.Attrs["year"].Int64() != 2026 {
					t.Fatalf("wrong year: %v", record)
				}
				switch record.Attrs["outcome"].String() {
				case "started":
					starts++
				case "failed":
					failures++
					if record.Level != slog.LevelError || record.Attrs["stage"].String() != "upstream" || record.Attrs["retry_in_ms"].Int64() <= 0 {
						t.Fatalf("failure missing severity, stage or retry delay: %v", record)
					}
					if _, ok := record.Attrs["duration_ms"]; !ok {
						t.Fatal("failure missing duration")
					}
				case "skipped":
					skips++
					if record.Level != slog.LevelDebug || record.Attrs["reason"].String() != "cooldown" || record.Attrs["trigger"].String() != "stale_refresh" {
						t.Fatalf("cooldown logged as an operation failure: %v", record)
					}
				case "succeeded":
					t.Fatal("failed fetch logged success")
				}
			}
			if starts != 1 || failures != 1 || skips != 1 || fetcher.CallCount() != 1 {
				t.Fatalf("starts=%d failures=%d skips=%d calls=%d", starts, failures, skips, fetcher.CallCount())
			}
		})
	}
}

func TestMappingLoadFailureOutcomes(t *testing.T) {
	for _, loaded := range []bool{false, true} {
		for _, canceled := range []bool{false, true} {
			t.Run(fmt.Sprintf("loaded=%t/canceled=%t", loaded, canceled), func(t *testing.T) {
				s := newTestScheduler(t, nil, nil, nil, nil)
				previous := mapping.NewAnibridgeMapping(map[int]int{101: 1001}, nil)
				if loaded {
					s.resolver.SetMapping(previous)
				}
				s.loadMapping = func(context.Context, string, string) (*mapping.AnibridgeMapping, mapping.Metadata, error) {
					if canceled {
						return nil, mapping.Metadata{}, context.Canceled
					}
					return nil, mapping.Metadata{}, errors.New("unavailable")
				}
				logs := testutil.CaptureLogs(t, slog.LevelDebug)
				s.refreshMapping(context.Background())
				wantOutcome, wantReason, wantLevel, wantTrigger := "degraded", "resolver_unavailable", slog.LevelWarn, "recovery"
				if loaded {
					wantOutcome, wantReason, wantTrigger = "failed", "keeping_current_mapping", "scheduled"
				}
				if canceled {
					wantOutcome, wantReason, wantLevel = "canceled", "context_done", slog.LevelInfo
				}
				terminals := 0
				for _, record := range logs.Records() {
					if record.Attrs["task"].String() != "mapping_refresh" || record.Attrs["outcome"].String() == "started" {
						continue
					}
					terminals++
					if record.Attrs["outcome"].String() != wantOutcome || record.Attrs["reason"].String() != wantReason || record.Level != wantLevel || record.Attrs["trigger"].String() != wantTrigger || record.Attrs["resolver_loaded"].Bool() != loaded {
						t.Fatalf("incorrect mapping failure record: %v", record)
					}
					if record.Attrs["retry_in_ms"].Int64() <= 0 {
						t.Fatal("mapping failure missing retry delay")
					}
				}
				if terminals != 1 || s.ResolverLoaded() != loaded || loaded && s.resolver.Mapping() != previous {
					t.Fatalf("terminals=%d resolver_loaded=%t; prior mapping must remain", terminals, s.ResolverLoaded())
				}
			})
		}
	}
}

func TestPrewarmPartialAndCanceledOutcomes(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprintf("canceled=%t", canceled), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fetcher := &sequenceFetcher{responses: func(call int) ([]anilist.Show, error) {
				if call == 1 {
					return []anilist.Show{{ID: 42}}, nil
				}
				if canceled {
					cancel()
					return nil, context.Canceled
				}
				return nil, errors.New("upstream unavailable")
			}}
			s := newTestScheduler(t, nil, &config.Config{PrewarmYears: []int{2024, 2025, 2026}}, fetcher, nil)
			if err := s.cache.SetYearContext(ctx, 2024, []byte(`[]`)); err != nil {
				t.Fatal(err)
			}
			logs := testutil.CaptureLogs(t, slog.LevelDebug)
			if err := s.Prewarm(ctx); err == nil {
				t.Fatal("partial prewarm returned success")
			}
			wantOutcome, wantLevel := "degraded", slog.LevelWarn
			if canceled {
				wantOutcome, wantLevel = "canceled", slog.LevelInfo
			}
			terminals := 0
			for _, record := range logs.Records() {
				if record.Attrs["task"].String() != "prewarm" || record.Attrs["outcome"].String() == "started" || record.Attrs["outcome"].String() == "skipped" {
					continue
				}
				terminals++
				if record.Attrs["outcome"].String() != wantOutcome || record.Level != wantLevel {
					t.Fatalf("incorrect partial prewarm outcome: %v", record)
				}
				for key, want := range map[string]int64{"configured": 3, "cached": 1, "fetched": 1, "failed": 1} {
					if record.Attrs[key].Int64() != want {
						t.Fatalf("prewarm %s=%v, want %d", key, record.Attrs[key], want)
					}
				}
			}
			if terminals != 1 {
				t.Fatalf("prewarm terminal records=%d, want 1", terminals)
			}
		})
	}
}
