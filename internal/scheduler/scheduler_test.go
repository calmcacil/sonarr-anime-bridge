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
