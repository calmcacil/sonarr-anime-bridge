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

func newTestCache(t *testing.T) *cache.Cache {
	t.Helper()
	c, err := cache.Open(":memory:")
	if err != nil {
		t.Fatalf("cache.Open: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestProcess_AllDoesNotIncludeWinterOverflow(t *testing.T) {
	c := newTestCache(t)
	cfg := &config.Config{IncludeTypes: []string{"TV", "ONA"}, FilterFutureEnabled: false}
	s := New(c, cfg)
	s.resolver.SetMapping(mapping.NewAnibridgeMapping(map[int]int{101: 1001, 102: 1002}, nil))

	current := []anilist.Show{{
		ID: 1, IDMal: ptr(101), Title: anilist.Title{English: ptr("Current")}, Format: "TV", Duration: ptr(24),
		Season: "FALL", StartDate: anilist.FuzzyDate{Year: ptr(2026), Month: ptr(10)},
	}}
	prior := []anilist.Show{{
		ID: 2, IDMal: ptr(102), Title: anilist.Title{English: ptr("Prior December")}, Format: "TV", Duration: ptr(24),
		Season: "WINTER", StartDate: anilist.FuzzyDate{Year: ptr(2025), Month: ptr(12)},
	}}
	currentData, err := json.Marshal(current)
	if err != nil {
		t.Fatal(err)
	}
	priorData, err := json.Marshal(prior)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SetYear(2025, priorData); err != nil {
		t.Fatal(err)
	}

	shows, err := s.Process(currentData, "ALL", 2026, "series")
	if err != nil {
		t.Fatal(err)
	}
	if len(shows) != 1 {
		t.Fatalf("expected 1 show, got %d: %+v", len(shows), shows)
	}
	if shows[0].TVDBID != 1001 {
		t.Fatalf("expected current year show, got %+v", shows[0])
	}
}

func ptr[T any](v T) *T {
	return &v
}

func TestFetchAndStore_InflightErrorPropagation(t *testing.T) {
	c := newTestCache(t)
	cfg := &config.Config{
		IncludeTypes: []string{"TV", "ONA"},
	}
	s := New(c, cfg)

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

	// Give the waiter time to reach the select on result.done.
	time.Sleep(50 * time.Millisecond)

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
