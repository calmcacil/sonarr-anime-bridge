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

type yearFetcher interface {
	FetchYear(ctx context.Context, year int) ([]anilist.Show, error)
}

type mappingLoader func(ctx context.Context, path, url string) (*mapping.AnibridgeMapping, mapping.Metadata, error)

const (
	mappingRefreshInterval        = 24 * time.Hour
	unloadedResolverRetryInterval = time.Minute
)

type Scheduler struct {
	cache    *cache.Cache
	cfg      *config.Config
	client   yearFetcher
	resolver *mapping.Resolver
	appCtx   context.Context

	wg         sync.WaitGroup
	waitDone   chan struct{}
	waitOnce   sync.Once
	inflight   sync.Map
	lastVacuum atomic.Int64
	bgMu       sync.Mutex
	bgClosed   bool

	loadMapping           mappingLoader
	resolverRetryInterval time.Duration
}

type Show struct {
	TVDBID int    `json:"tvdbId"`
	Title  string `json:"title,omitempty"`
}

func New(c *cache.Cache, cfg *config.Config) *Scheduler {
	return NewWithFetcher(c, cfg, anilist.NewWithTimeout(30*time.Second))
}

func NewWithFetcher(c *cache.Cache, cfg *config.Config, fetcher yearFetcher) *Scheduler {
	return &Scheduler{
		cache:    c,
		cfg:      cfg,
		client:   fetcher,
		resolver: mapping.NewResolver(),
		appCtx:   context.Background(),
		waitDone: make(chan struct{}),

		loadMapping:           mapping.LoadOrFetch,
		resolverRetryInterval: unloadedResolverRetryInterval,
	}
}

func (s *Scheduler) ResolverLoaded() bool {
	return s.resolver.Mapping() != nil
}

func (s *Scheduler) LoadResolver() {
	s.LoadResolverContext(context.Background())
}

func (s *Scheduler) LoadResolverContext(ctx context.Context) {
	if err := s.loadResolver(ctx); err != nil {
		slog.Error("failed to load anibridge mapping", "type", "resolver", "error", err)
		return
	}
}

func (s *Scheduler) StartBackground(ctx context.Context) {
	s.appCtx = ctx
	s.bgMu.Lock()
	s.bgClosed = false
	s.bgMu.Unlock()
	s.wg.Add(2)
	s.waitOnce.Do(func() {
		go func() {
			s.wg.Wait()
			close(s.waitDone)
		}()
	})
	go func() {
		defer s.wg.Done()
		defer func() {
			if r := recover(); r != nil {
				slog.Error("panic in stale refresh background worker", "type", "scheduler", "recover", r)
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

	go func() {
		defer s.wg.Done()
		defer func() {
			if r := recover(); r != nil {
				slog.Error("panic in mapping refresh background worker", "type", "scheduler", "recover", r)
			}
		}()
		timer := time.NewTimer(s.nextMappingRefreshInterval())
		defer timer.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				s.refreshMapping(ctx)
				timer.Reset(s.nextMappingRefreshInterval())
			}
		}
	}()
}

func (s *Scheduler) nextMappingRefreshInterval() time.Duration {
	if s.ResolverLoaded() {
		return mappingRefreshInterval
	}
	if s.resolverRetryInterval <= 0 {
		return unloadedResolverRetryInterval
	}
	return s.resolverRetryInterval
}

func (s *Scheduler) loadResolver(ctx context.Context) error {
	path := s.cfg.AnibridgeMappingPath
	upstream := s.cfg.AnibridgeURL
	loader := s.loadMapping
	if loader == nil {
		loader = mapping.LoadOrFetch
	}
	m, _, err := loader(ctx, path, upstream)
	if err != nil {
		return err
	}
	s.resolver.SetMapping(m)
	return nil
}

func (s *Scheduler) refreshMapping(ctx context.Context) {
	start := time.Now()
	if err := s.loadResolver(ctx); err != nil {
		slog.Warn("anibridge mapping refresh failed, keeping current mapping",
			"type", "resolver",
			"error", err,
			"duration_ms", time.Since(start).Milliseconds(),
		)
		return
	}
	slog.Info("anibridge mapping refresh loaded",
		"type", "resolver",
		"duration_ms", time.Since(start).Milliseconds(),
	)
}

func (s *Scheduler) Prewarm(ctx context.Context) error {
	var firstErr error
	for _, year := range s.cfg.PrewarmYears {
		start := time.Now()
		if err := ctx.Err(); err != nil {
			return err
		}
		if data, fresh, ok, err := s.cache.GetYearContext(ctx, year); err != nil {
			slog.Warn("prewarm cache read failed", "type", "scheduler", "year", year, "error", err)
		} else if ok && fresh {
			var shows []anilist.Show
			if err := json.Unmarshal(data, &shows); err == nil {
				slog.Info("prewarm skipped, cache is fresh",
					"type", "scheduler",
					"year", year,
					"shows", len(shows),
					"duration_ms", time.Since(start).Milliseconds(),
				)
				continue
			} else {
				slog.Warn("fresh cache data is corrupt, refetching", "type", "scheduler", "year", year, "error", err)
			}
		}
		slog.Info("prewarming", "type", "scheduler", "year", year)
		if err := s.FetchAndStore(ctx, year, "prewarm"); err != nil {
			slog.Error("prewarm failed",
				"type", "scheduler",
				"year", year,
				"error", err,
				"duration_ms", time.Since(start).Milliseconds(),
			)
			if firstErr == nil {
				firstErr = err
			}
		} else {
			slog.Info("prewarm fetched",
				"type", "scheduler",
				"year", year,
				"duration_ms", time.Since(start).Milliseconds(),
			)
		}
	}
	return firstErr
}

func (s *Scheduler) Process(rawData []byte, season string, year int, category string) ([]Show, error) {
	return s.ProcessContext(context.Background(), rawData, season, year, category)
}

func (s *Scheduler) ProcessContext(ctx context.Context, rawData []byte, season string, year int, category string) ([]Show, error) {
	start := time.Now()
	var shows []anilist.Show
	if err := json.Unmarshal(rawData, &shows); err != nil {
		return nil, fmt.Errorf("unmarshal year data: %w", err)
	}
	input := len(shows)
	afterWinterOverflow := input

	if season == "WINTER" {
		prevData, _, ok, err := s.cache.GetYearContext(ctx, year-1)
		if err != nil {
			slog.Warn("winter overflow cache read failed", "type", "scheduler", "year", year-1, "error", err)
		} else if ok {
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
			} else {
				slog.Warn("winter overflow cache data is corrupt", "type", "scheduler", "year", year-1, "error", err)
			}
		}
		afterWinterOverflow = len(shows)
	}

	if season != "ALL" {
		shows = filter.FilterBySeason(shows, season)
	}
	afterSeason := len(shows)

	shows = filter.FilterByFormats(shows, s.cfg.IncludeTypes)
	afterFormat := len(shows)

	shows, filterStats := filter.FilterWithStats(shows, filter.Config{
		ExcludeTags: s.cfg.ExcludeTags,
	})

	var futureStats filter.FutureStats
	if s.cfg.FilterFutureEnabled {
		shows, futureStats = filter.FilterFutureWithStats(shows, 3)
	} else {
		futureStats = filter.FutureStats{Input: len(shows), Output: len(shows)}
	}

	beforeFirstSeason := len(shows)
	if category == "series-new" {
		shows = filter.FilterFirstSeason(shows)
	}
	skippedFirstSeason := beforeFirstSeason - len(shows)

	batch := s.resolveBatch(shows)
	resolved := responseShows(shows, batch)
	unresolved := len(shows) - len(resolved)
	if unresolved < 0 {
		unresolved = 0
	}
	slog.Debug("processed filters",
		"type", "filter",
		"year", year,
		"season", season,
		"category", category,
		"input", input,
		"after_winter_overflow", afterWinterOverflow,
		"after_season", afterSeason,
		"after_format", afterFormat,
		"skipped_duration", filterStats.SkippedDuration,
		"skipped_tags", filterStats.SkippedTags,
		"skipped_future", futureStats.SkippedFuture,
		"skipped_first_season", skippedFirstSeason,
		"resolved", len(resolved),
		"unresolved", unresolved,
		"duration_ms", time.Since(start).Milliseconds(),
	)
	s.trackNewMappings(ctx, shows, batch, season, year)
	return resolved, nil
}

func (s *Scheduler) BackgroundFetchContext(timeout time.Duration) (context.Context, context.CancelFunc) {
	base := s.appCtx
	if base == nil {
		base = context.Background()
	}
	if timeout <= 0 {
		return context.WithCancel(base)
	}
	return context.WithTimeout(base, timeout)
}

func (s *Scheduler) StartBackgroundFetch(timeout time.Duration, fn func(context.Context)) {
	s.bgMu.Lock()
	if s.bgClosed {
		s.bgMu.Unlock()
		return
	}
	s.wg.Add(1)
	s.bgMu.Unlock()

	go func() {
		defer s.wg.Done()
		fetchCtx, cancel := s.BackgroundFetchContext(timeout)
		defer cancel()
		fn(fetchCtx)
	}()
}

func (s *Scheduler) closeBackgroundFetches() {
	s.bgMu.Lock()
	s.bgClosed = true
	s.bgMu.Unlock()
}

func (s *Scheduler) FetchAndStore(ctx context.Context, year int, trigger string) (err error) {
	start := time.Now()
	result := &inflightResult{done: make(chan struct{})}
	actual, loaded := s.inflight.LoadOrStore(year, result)
	if loaded {
		slog.Debug("year fetch already in-flight, waiting", "type", "fetch", "year", year)
		res := actual.(*inflightResult)
		select {
		case <-res.done:
			return res.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("fetch year %d panic: %v", year, r)
		}
		result.err = err
		close(result.done)
		s.inflight.Delete(year)
	}()

	fetchCtx, cancel := s.fetchContext(ctx)
	defer cancel()

	shows, fetchErr := s.client.FetchYear(fetchCtx, year)
	if fetchErr != nil {
		err = fmt.Errorf("fetch year %d: %w", year, fetchErr)
		return
	}

	data, marshalErr := json.Marshal(shows)
	if marshalErr != nil {
		err = fmt.Errorf("marshal year %d: %w", year, marshalErr)
		return
	}

	if cacheErr := s.cache.SetYearContext(fetchCtx, year, data); cacheErr != nil {
		err = fmt.Errorf("cache set year %d: %w", year, cacheErr)
		return
	}

	slog.Info("year_cached",
		"type", "fetch",
		"year", year,
		"shows", len(shows),
		"trigger", trigger,
		"duration_ms", time.Since(start).Milliseconds(),
	)
	return nil
}

func (s *Scheduler) fetchContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	base := s.appCtx
	if base == nil {
		return context.WithTimeout(ctx, 2*time.Minute)
	}
	fetchCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	done := context.AfterFunc(base, cancel)
	return fetchCtx, func() {
		done()
		cancel()
	}
}

// resolveBatch resolves AniList shows to TVDB IDs using the anibridge mapping.
func (s *Scheduler) resolveBatch(shows []anilist.Show) map[int]mapping.ResolvedShow {
	m := s.resolver.Mapping()
	if m == nil {
		slog.Warn("resolver not yet loaded, skipping resolution", "type", "resolver", "resolver_loaded", false)
		return nil
	}
	return s.resolver.ResolveBatch(shows)
}

// responseShows converts resolved mapping results into the public response shape.
func responseShows(shows []anilist.Show, resolved map[int]mapping.ResolvedShow) []Show {
	out := make([]Show, 0, len(shows))
	for _, show := range shows {
		if r, ok := resolved[show.ID]; ok && r.Resolved {
			out = append(out, Show{TVDBID: r.TVDBID, Title: r.Title})
		}
	}
	return out
}

// trackNewMappings records newly-resolved TVDB IDs in the seen_mappings
// database and logs those that are genuinely new (not seen before for the
// same season/year context). On first-ever run with an empty tracking
// table, it seeds silently per the issue #52 spec.
func (s *Scheduler) trackNewMappings(ctx context.Context, anilistShows []anilist.Show, batch map[int]mapping.ResolvedShow, season string, year int) {
	m := s.resolver.Mapping()
	if m == nil || len(batch) == 0 {
		return
	}

	firstRun := false
	count, err := s.cache.CountSeenMappings(ctx)
	if err != nil {
		slog.Warn("failed to count seen mappings", "type", "mapping", "error", err)
		return
	}
	if count == 0 {
		firstRun = true
	}

	entries := make([]cache.SeenMapping, 0, len(anilistShows))
	for _, as := range anilistShows {
		r, ok := batch[as.ID]
		if !ok || !r.Resolved {
			continue
		}

		startsAt := ""
		if as.StartDate.Year != nil && as.StartDate.Month != nil && as.StartDate.Day != nil {
			startsAt = fmt.Sprintf("%02d.%02d.%02d", *as.StartDate.Day, *as.StartDate.Month, *as.StartDate.Year%100)
		}

		entries = append(entries, cache.SeenMapping{
			TVDBID:    r.TVDBID,
			AniListID: as.ID,
			Title:     r.Title,
			Season:    season,
			Year:      year,
			StartsAt:  startsAt,
		})
	}

	if len(entries) == 0 {
		return
	}

	newMappings, err := s.cache.MarkSeenMappings(ctx, entries)
	if err != nil {
		slog.Warn("failed to record seen mappings", "type", "mapping", "error", err)
		return
	}

	if firstRun || len(newMappings) == 0 {
		return
	}

	slog.Info("new mappings discovered",
		"type", "mapping",
		"count", len(newMappings),
		"season", season,
		"year", year,
	)
	for _, m := range newMappings {
		slog.Debug("mapping added",
			"type", "mapping",
			"tvdbid", m.TVDBID,
			"title", m.Title,
			"starts_at", m.StartsAt,
			"season", m.Season,
			"year", m.Year,
		)
	}
}

func (s *Scheduler) refreshStaleYears(ctx context.Context) {
	currentYear := time.Now().Year()
	years, err := s.cache.NeedsRefreshYearsContext(ctx, currentYear, 1, 7)
	if err != nil {
		slog.Error("needs refresh query failed", "type", "scheduler", "error", err)
		return
	}
	for _, year := range years {
		if err := ctx.Err(); err != nil {
			return
		}
		slog.Info("refreshing stale year", "type", "scheduler", "year", year)
		start := time.Now()
		yearCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		err := s.FetchAndStore(yearCtx, year, "stale_refresh")
		cancel()
		if err != nil {
			slog.Error("stale year refresh failed",
				"type", "scheduler",
				"year", year,
				"error", err,
				"duration_ms", time.Since(start).Milliseconds(),
			)
		} else {
			slog.Info("stale year refresh complete",
				"type", "scheduler",
				"year", year,
				"duration_ms", time.Since(start).Milliseconds(),
			)
		}
	}
}

func (s *Scheduler) prune(ctx context.Context) {
	if err := ctx.Err(); err != nil {
		return
	}
	start := time.Now()
	n, err := s.cache.PruneStaleYearsContext(ctx, 14)
	if err != nil {
		slog.Error("prune failed", "type", "scheduler", "error", err, "duration_ms", time.Since(start).Milliseconds())
		return
	}
	if n > 0 {
		slog.Info("pruned cache entries", "type", "scheduler", "count", n, "duration_ms", time.Since(start).Milliseconds())
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
			slog.Debug("running VACUUM on year_cache", "type", "scheduler")
			start := time.Now()
			if err := s.cache.VacuumContext(ctx); err != nil {
				slog.Error("vacuum failed", "type", "scheduler", "error", err, "duration_ms", time.Since(start).Milliseconds())
			} else {
				slog.Debug("vacuum complete", "type", "scheduler", "duration_ms", time.Since(start).Milliseconds())
			}
		}
	}
}

func (s *Scheduler) logCacheStats(ctx context.Context) {
	if err := ctx.Err(); err != nil {
		return
	}
	stats, err := s.cache.StatsContext(ctx)
	if err != nil {
		slog.Warn("cache stats failed", "type", "scheduler", "error", err)
		return
	}
	slog.Debug("cache stats",
		"type", "scheduler",
		"entries", stats.Entries,
		"hits", stats.Hits,
		"misses", stats.Misses,
	)
}

func (s *Scheduler) Wait(ctx context.Context) error {
	s.closeBackgroundFetches()
	select {
	case <-s.waitDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
