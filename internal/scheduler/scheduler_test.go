package scheduler

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/calmcacil/sonarr-anime-bridge/internal/anilist"

	"github.com/calmcacil/sonarr-anime-bridge/internal/cache"
	"github.com/calmcacil/sonarr-anime-bridge/internal/config"
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
