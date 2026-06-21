package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/calmcacil/sonarr-anime-bridge/internal/anilist"
	"github.com/calmcacil/sonarr-anime-bridge/internal/cache"
	"github.com/calmcacil/sonarr-anime-bridge/internal/config"
	"github.com/calmcacil/sonarr-anime-bridge/internal/filter"
	"github.com/calmcacil/sonarr-anime-bridge/internal/mapping"
)

// inflightResult carries the outcome of an in-flight year fetch so that
// concurrent waiters receive the same error (if any) as the original caller.
type inflightResult struct {
	err  error
	done chan struct{}
}

type Scheduler struct {
	cache    *cache.Cache
	cfg      *config.Config
	client   *anilist.Client
	resolver *mapping.Resolver

	wg         sync.WaitGroup
	inflight   sync.Map
	lastVacuum atomic.Int64
}

type Show struct {
	TVDBID int    `json:"tvdbId"`
	Title  string `json:"title,omitempty"`
}

func New(c *cache.Cache, cfg *config.Config) *Scheduler {
	return &Scheduler{
		cache:    c,
		cfg:      cfg,
		client:   anilist.NewWithTimeout(30 * time.Second),
		resolver: mapping.NewResolver(),
	}
}

func (s *Scheduler) ResolverLoaded() bool {
	return s.resolver.Mapping() != nil
}

func (s *Scheduler) LoadResolver(ctx context.Context) {
	path := s.cfg.AnibridgeMappingPath
	upstream := s.cfg.AnibridgeURL
	m, _, err := mapping.LoadOrFetch(ctx, path, upstream)
	if err != nil {
		slog.Error("failed to load anibridge mapping", "error", err)
		return
	}
	s.resolver.SetMapping(m)
}

func (s *Scheduler) StartBackground(ctx context.Context) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			if r := recover(); r != nil {
				slog.Error("panic in stale refresh background worker", "recover", r)
			}
		}()
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.refreshStaleYears(ctx)
				s.prune(ctx)
				s.logCacheStats(ctx)
			}
		}
	}()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			if r := recover(); r != nil {
				slog.Error("panic in mapping refresh background worker", "recover", r)
			}
		}()
		mapTicker := time.NewTicker(24 * time.Hour)
		defer mapTicker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-mapTicker.C:
				s.refreshMapping(ctx)
			}
		}
	}()
}

func (s *Scheduler) refreshMapping(ctx context.Context) {
	m, _, err := mapping.LoadOrFetch(ctx, s.cfg.AnibridgeMappingPath, s.cfg.AnibridgeURL)
	if err != nil {
		slog.Warn("anibridge mapping refresh failed, keeping current mapping", "error", err)
		return
	}
	s.resolver.SetMapping(m)
}

func (s *Scheduler) Prewarm(ctx context.Context) error {
	var firstErr error
	for _, year := range s.cfg.PrewarmYears {
		if data, fresh, ok := s.cache.GetYear(year); ok && fresh {
			var shows []anilist.Show
			if err := json.Unmarshal(data, &shows); err == nil {
				slog.Info("prewarm skipped, cache is fresh", "year", year, "shows", len(shows))
				continue
			}
		}
		slog.Info("prewarming", "year", year)
		if err := s.FetchAndStore(ctx, year, "prewarm"); err != nil {
			slog.Error("prewarm failed", "year", year, "error", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func (s *Scheduler) Process(rawData []byte, season string, year int, category string) ([]Show, error) {
	var shows []anilist.Show
	if err := json.Unmarshal(rawData, &shows); err != nil {
		return nil, fmt.Errorf("unmarshal year data: %w", err)
	}

	if season == "WINTER" {
		prevData, _, ok := s.cache.GetYear(year - 1)
		if ok {
			var prevShows []anilist.Show
			if err := json.Unmarshal(prevData, &prevShows); err == nil {
				prevShows = filter.FilterBySeason(prevShows, "WINTER")
				seen := make(map[int]bool, len(shows))
				for _, sh := range shows {
					seen[sh.ID] = true
				}
				for _, sh := range prevShows {
					if seen[sh.ID] {
						continue
					}
					if sh.StartDate.Month == nil || *sh.StartDate.Month != 12 {
						continue
					}
					shows = append(shows, sh)
					seen[sh.ID] = true
				}
			}
		}
	}

	if season != "ALL" {
		shows = filter.FilterBySeason(shows, season)
	}

	shows = filter.FilterByFormats(shows, s.cfg.IncludeTypes)

	shows = filter.Filter(shows, filter.Config{
		ExcludeTags: s.cfg.ExcludeTags,
	})

	if s.cfg.FilterFutureEnabled {
		shows = filter.FilterFuture(shows, 3)
	}

	if category == "series-new" {
		shows = filter.FilterFirstSeason(shows)
	}

	return s.resolveShows(shows), nil
}

func (s *Scheduler) FetchAndStoreAsync(ctx context.Context, year int, trigger string) {
	if ctx.Err() != nil {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			if r := recover(); r != nil {
				slog.Error("panic in year fetch background worker", "recover", r, "year", year, "trigger", trigger)
			}
		}()
		if err := s.FetchAndStore(ctx, year, trigger); err != nil {
			slog.Error("background year fetch failed", "year", year, "trigger", trigger, "error", err)
		}
	}()
}

func (s *Scheduler) FetchAndStore(ctx context.Context, year int, trigger string) (err error) {
	result := &inflightResult{done: make(chan struct{})}
	actual, loaded := s.inflight.LoadOrStore(year, result)
	if loaded {
		slog.Debug("year fetch already in-flight, waiting", "year", year)
		res := actual.(*inflightResult)
		select {
		case <-res.done:
			return res.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	defer func() {
		result.err = err
		close(result.done)
		s.inflight.Delete(year)
	}()

	shows, fetchErr := s.client.FetchYear(ctx, year)
	if fetchErr != nil {
		err = fmt.Errorf("fetch year %d: %w", year, fetchErr)
		return
	}

	data, marshalErr := json.Marshal(shows)
	if marshalErr != nil {
		err = fmt.Errorf("marshal year %d: %w", year, marshalErr)
		return
	}

	if cacheErr := s.cache.SetYear(year, data); cacheErr != nil {
		err = fmt.Errorf("cache set year %d: %w", year, cacheErr)
		return
	}

	slog.Info("year_cached", "year", year, "shows", len(shows), "trigger", trigger)
	return nil
}

func (s *Scheduler) resolveShows(shows []anilist.Show) []Show {
	m := s.resolver.Mapping()
	if m == nil {
		slog.Warn("resolver not yet loaded, skipping resolution")
		return make([]Show, 0)
	}
	out := make([]Show, 0, len(shows))
	for _, show := range shows {
		if tvdbID, ok := s.resolver.Resolve(show); ok {
			out = append(out, Show{TVDBID: tvdbID, Title: show.DisplayTitle()})
		}
	}
	return out
}

func (s *Scheduler) refreshStaleYears(ctx context.Context) {
	currentYear := time.Now().Year()
	years, err := s.cache.NeedsRefreshYears(currentYear, 1, 7)
	if err != nil {
		slog.Error("needs refresh query failed", "error", err)
		return
	}
	for _, year := range years {
		slog.Info("refreshing stale year", "year", year)
		if err := s.FetchAndStore(ctx, year, "stale_refresh"); err != nil {
			slog.Error("stale year refresh failed", "year", year, "error", err)
		}
	}
}

func (s *Scheduler) prune(ctx context.Context) {
	if err := ctx.Err(); err != nil {
		return
	}
	start := time.Now()
	n, err := s.cache.PruneStaleYears(14)
	if err != nil {
		slog.Error("prune failed", "error", err)
		return
	}
	if n > 0 {
		slog.Info("pruned cache entries", "count", n, "duration", time.Since(start))
		s.vacuumMaybe(ctx)
	}
}

// vacuumMaybe runs VACUUM at most once per 24 hours to avoid blocking cache
// operations on large databases too frequently.
func (s *Scheduler) vacuumMaybe(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	const vacuumInterval = 24 * time.Hour
	now := time.Now().Unix()
	last := s.lastVacuum.Load()
	if time.Unix(last, 0).Add(vacuumInterval).Before(time.Now()) {
		if s.lastVacuum.CompareAndSwap(last, now) {
			slog.Debug("running VACUUM on year_cache")
			if err := s.cache.Vacuum(); err != nil {
				slog.Error("vacuum failed", "error", err)
			}
		}
	}
}

func (s *Scheduler) logCacheStats(ctx context.Context) {
	if err := ctx.Err(); err != nil {
		return
	}
	stats := s.cache.Stats()
	slog.Debug("cache stats",
		"entries", stats.Entries,
		"hits", stats.Hits,
		"misses", stats.Misses,
	)
}

func (s *Scheduler) Wait(ctx context.Context) error {
	ch := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(ch)
	}()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
