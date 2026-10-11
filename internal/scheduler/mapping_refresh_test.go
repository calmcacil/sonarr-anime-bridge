package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/calmcacil/sonarr-anime-bridge/internal/anilist"
	"github.com/calmcacil/sonarr-anime-bridge/internal/mapping"
	"github.com/calmcacil/sonarr-anime-bridge/internal/testutil"
)

func TestMappingRefreshDispositions(t *testing.T) {
	for _, disposition := range []mapping.Disposition{mapping.Loaded, mapping.Reused, mapping.Fallback} {
		t.Run(string(disposition), func(t *testing.T) {
			s := newTestScheduler(t, nil, nil, nil, nil)
			previous := mapping.NewAnibridgeMapping(map[int]int{101: 1001}, nil)
			s.resolver.SetMapping(previous)
			candidate := previous
			if disposition == mapping.Loaded {
				candidate = mapping.NewAnibridgeMapping(map[int]int{101: 2001}, nil)
			}
			s.loadMapping = func(context.Context, string, string) (mapping.LoadResult, error) {
				return mapping.LoadResult{Mapping: candidate, Disposition: disposition}, nil
			}
			logs := testutil.CaptureLogs(t, slog.LevelInfo)
			s.refreshMapping(context.Background())
			if !s.ResolverLoaded() || s.resolver.Mapping() != candidate || s.nextMappingRefreshInterval() != 24*time.Hour {
				t.Fatal("refresh changed resolver availability or cadence")
			}
			terminals := 0
			for _, rec := range logs.Records() {
				if rec.Attrs["task"].String() != "mapping_refresh" || rec.Attrs["outcome"].String() == "started" {
					continue
				}
				terminals++
				wantOutcome, wantReason, wantLevel := "succeeded", "changed_content", slog.LevelInfo
				if disposition == mapping.Reused {
					wantReason = "unchanged_content"
				}
				if disposition == mapping.Fallback {
					wantOutcome, wantReason, wantLevel = "degraded", "cached_fallback", slog.LevelWarn
				}
				if rec.Attrs["outcome"].String() != wantOutcome || rec.Attrs["reason"].String() != wantReason || rec.Level != wantLevel || rec.Attrs["disposition"].String() != string(disposition) {
					t.Fatalf("incorrect refresh diagnostic: %+v", rec)
				}
			}
			if terminals != 1 {
				t.Fatalf("terminal records=%d", terminals)
			}
		})
	}
}

func TestMappingRefreshReadersAndCanceledWaiter(t *testing.T) {
	t.Parallel()
	s := newTestScheduler(t, nil, nil, nil, nil)
	previous := mapping.NewAnibridgeMapping(map[int]int{101: 1001}, nil)
	next := mapping.NewAnibridgeMapping(map[int]int{101: 2001}, nil)
	s.resolver.SetMapping(previous)
	entered, resume := make(chan struct{}), make(chan struct{})
	var loads atomic.Int32
	s.loadMapping = func(context.Context, string, string) (mapping.LoadResult, error) {
		loads.Add(1)
		close(entered)
		<-resume
		return mapping.LoadResult{Mapping: next, Disposition: mapping.Loaded}, nil
	}
	finished := make(chan error, 1)
	go func() { finished <- s.loadResolver(context.Background(), "mapping_refresh", "scheduled") }()
	<-entered
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for range 1000 {
			if result := s.resolver.ResolveBatch([]anilist.Show{{ID: 1, IDMal: testutil.Ptr(101)}}); result[1].TVDBID != 1001 {
				t.Error("reader saw incomplete mapping during refresh")
				return
			}
		}
	}()
	select {
	case <-readDone:
	case <-time.After(2 * time.Second):
		t.Fatal("refresh blocked resolver readers")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.loadResolver(ctx, "mapping_refresh", "scheduled"); !errors.Is(err, context.Canceled) || loads.Load() != 1 {
		t.Fatalf("canceled waiter entered loader: loads=%d err=%v", loads.Load(), err)
	}
	close(resume)
	if err := <-finished; err != nil || s.resolver.Mapping() != next {
		t.Fatalf("complete mapping was not published: %v", err)
	}
}

func TestMappingRefreshCancellationBeforePublication(t *testing.T) {
	t.Parallel()
	s := newTestScheduler(t, nil, nil, nil, nil)
	previous := mapping.NewAnibridgeMapping(map[int]int{101: 1001}, nil)
	next := mapping.NewAnibridgeMapping(map[int]int{101: 2001}, nil)
	s.resolver.SetMapping(previous)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.loadMapping = func(context.Context, string, string) (mapping.LoadResult, error) {
		cancel()
		return mapping.LoadResult{Mapping: next, Disposition: mapping.Loaded}, nil
	}
	if err := s.loadResolver(ctx, "mapping_refresh", "scheduled"); !errors.Is(err, context.Canceled) || s.resolver.Mapping() != previous {
		t.Fatal("canceled refresh replaced active resolver")
	}
	s.loadMapping = func(context.Context, string, string) (mapping.LoadResult, error) {
		return mapping.LoadResult{Mapping: next, Disposition: mapping.Loaded}, nil
	}
	if err := s.loadResolver(context.Background(), "mapping_refresh", "scheduled"); err != nil || s.resolver.Mapping() != next {
		t.Fatal("cancellation left publication gate stuck")
	}
}

func TestBackgroundMappingRefreshStopsOnShutdown(t *testing.T) {
	t.Parallel()
	s := newTestScheduler(t, nil, nil, nil, nil)
	s.resolverRetryInterval = time.Millisecond
	entered := make(chan struct{})
	s.loadMapping = func(ctx context.Context, _, _ string) (mapping.LoadResult, error) {
		close(entered)
		<-ctx.Done()
		return mapping.LoadResult{}, ctx.Err()
	}
	cancel := startBackground(t, s)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("unloaded resolver did not retry")
	}
	cancel()
	waitCtx, waitCancel := context.WithTimeout(context.Background(), time.Second)
	defer waitCancel()
	if err := s.Wait(waitCtx); err != nil {
		t.Fatalf("shutdown left mapping refresh blocked: %v", err)
	}
}
