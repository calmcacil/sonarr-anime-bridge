package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/calmcacil/sonarr-anime-bridge/internal/anilist"

	"github.com/calmcacil/sonarr-anime-bridge/internal/cache"
	"github.com/calmcacil/sonarr-anime-bridge/internal/config"
	"github.com/calmcacil/sonarr-anime-bridge/internal/mapping"
)

type testFetcher struct{}

func (testFetcher) FetchYear(context.Context, int) ([]anilist.Show, error) {
	return []anilist.Show{}, nil
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
	resolved := []Show{
		{TVDBID: 1001, Title: "Show One"},
		{TVDBID: 1002, Title: "Show Two"},
	}

	// First run: should seed silently (no logs), verify by checking DB
	s.trackNewMappings(ctx, anilistShows, resolved, "SUMMER", 2026)

	count, err := c.CountSeenMappings(ctx)
	if err != nil {
		t.Fatalf("CountSeenMappings: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 seen mappings after first run, got %d", count)
	}

	// Second run with same shows: should not add any new mappings
	s.trackNewMappings(ctx, anilistShows, resolved, "SUMMER", 2026)

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
	firstResolved := []Show{
		{TVDBID: 1001, Title: "Show One"},
		{TVDBID: 1002, Title: "Show Two"},
	}
	s.trackNewMappings(ctx, firstBatch, firstResolved, "SUMMER", 2026)

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
	secondResolved := []Show{
		{TVDBID: 1001, Title: "Show One"},
		{TVDBID: 1002, Title: "Show Two"},
		{TVDBID: 1003, Title: "Show Three"},
	}
	s.trackNewMappings(ctx, secondBatch, secondResolved, "SUMMER", 2026)

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
	resolved := []Show{
		{TVDBID: 1001, Title: "Show One"},
	}

	// First run with summer 2026
	s.trackNewMappings(ctx, shows, resolved, "SUMMER", 2026)

	// Same TVDB ID but different season should be tracked separately
	s.trackNewMappings(ctx, shows, resolved, "FALL", 2026)

	// Same TVDB ID but different year should be tracked separately
	s.trackNewMappings(ctx, shows, resolved, "SUMMER", 2027)

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
	resolved := []Show{
		{TVDBID: 1001, Title: "Show One"},
	}

	// Should not panic or error
	s.trackNewMappings(ctx, shows, resolved, "SUMMER", 2026)

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
