package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	err     error
	fetched bool
	done    chan struct{}
}

type yearFetcher interface {
	FetchYear(ctx context.Context, year int) ([]anilist.Show, error)
}

type mappingLoader func(ctx context.Context, path, url string) (*mapping.AnibridgeMapping, mapping.Metadata, error)

const (
	mappingRefreshInterval        = 24 * time.Hour
	unloadedResolverRetryInterval = time.Minute
	fetchRetryBase                = 15 * time.Second
	fetchRetryMax                 = 5 * time.Minute
	maxTrackedFetchFailures       = 256
)

type fetchFailure struct {
	retryAt time.Time
	delay   time.Duration
}

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
	failureMu  sync.Mutex
	failures   map[int]fetchFailure
	lastVacuum atomic.Int64
	tracking   atomic.Bool
	bgMu       sync.Mutex
	bgClosed   bool

	loadMapping           mappingLoader
	resolverRetryInterval time.Duration
	fetchRetryBase        time.Duration
	fetchRetryMax         time.Duration
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
		failures: make(map[int]fetchFailure),

		loadMapping:           mapping.LoadOrFetch,
		resolverRetryInterval: unloadedResolverRetryInterval,
		fetchRetryBase:        fetchRetryBase,
		fetchRetryMax:         fetchRetryMax,
	}
}

func (s *Scheduler) ResolverLoaded() bool {
	return s.resolver.Mapping() != nil
}

func (s *Scheduler) LoadResolverContext(ctx context.Context) {
	_ = s.loadResolver(ctx, "mapping_load", "startup")
}

func (s *Scheduler) StartBackground(ctx context.Context) {
	s.appCtx = ctx
	s.bgMu.Lock()
	s.bgClosed = false
	s.bgMu.Unlock()
	s.wg.Add(2)
	go func() {
		defer s.wg.Done()
		start := time.Now()
		defer func() {
			if r := recover(); r != nil {
				slog.Error("cache maintenance worker failed", "type", "scheduler", "task", "cache_maintenance", "outcome", "failed",
					"trigger", "scheduled", "recover", r, "duration_ms", time.Since(start).Milliseconds())
			}
		}()
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.prune(ctx)
				s.refreshStaleYears(ctx)
				s.logCacheStats(ctx)
			}
		}
	}()

	go func() {
		defer s.wg.Done()
		start := time.Now()
		defer func() {
			if r := recover(); r != nil {
				slog.Error("mapping refresh worker failed", "type", "scheduler", "task", "mapping_refresh", "outcome", "failed",
					"trigger", "scheduled", "recover", r, "duration_ms", time.Since(start).Milliseconds())
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

func (s *Scheduler) loadResolver(ctx context.Context, task, trigger string) error {
	start := time.Now()
	slog.Info("loading mapping", "type", "resolver", "task", task, "outcome", "started", "trigger", trigger)
	loader := s.loadMapping
	if loader == nil {
		loader = mapping.LoadOrFetch
	}
	m, _, err := loader(ctx, s.cfg.AnibridgeMappingPath, s.cfg.AnibridgeURL)
	if err != nil {
		loaded := s.ResolverLoaded()
		outcome, reason, level := "failed", "keeping_current_mapping", slog.LevelWarn
		if !loaded {
			outcome, reason = "degraded", "resolver_unavailable"
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			outcome, reason = "canceled", "context_done"
			if errors.Is(err, context.Canceled) {
				level = slog.LevelInfo
			}
		}
		slog.Log(ctx, level, "mapping load ended", "type", "resolver", "task", task,
			"outcome", outcome, "trigger", trigger, "reason", reason, "error", err,
			"resolver_loaded", loaded, "retry_in_ms", s.nextMappingRefreshInterval().Milliseconds(),
			"duration_ms", time.Since(start).Milliseconds())
		return err
	}
	s.resolver.SetMapping(m)
	malEntries, aniListEntries := m.Stats()
	slog.Info("mapping loaded", "type", "resolver", "task", task, "outcome", "succeeded",
		"trigger", trigger, "resolver_loaded", true, "mal_entries", malEntries,
		"anilist_entries", aniListEntries, "total_entries", malEntries+aniListEntries,
		"duration_ms", time.Since(start).Milliseconds())
	return nil
}

// DecodeYearData validates persisted JSON and returns a request-owned slice.
func DecodeYearData(data []byte) ([]anilist.Show, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, fmt.Errorf("year data must be a JSON array")
	}
	var shows []anilist.Show
	if err := json.Unmarshal(trimmed, &shows); err != nil {
		return nil, err
	}
	return shows, nil
}

func (s *Scheduler) refreshMapping(ctx context.Context) {
	trigger := "scheduled"
	if !s.ResolverLoaded() {
		trigger = "recovery"
	}
	_ = s.loadResolver(ctx, "mapping_refresh", trigger)
}

func (s *Scheduler) Prewarm(ctx context.Context) (err error) {
	start := time.Now()
	if len(s.cfg.PrewarmYears) == 0 {
		slog.Debug("prewarm skipped", "type", "scheduler", "task", "prewarm", "outcome", "skipped", "trigger", "startup",
			"reason", "no_configured_years", "configured", 0, "cached", 0, "fetched", 0, "failed", 0,
			"duration_ms", time.Since(start).Milliseconds())
		return nil
	}
	cached, fetched, failed := 0, 0, 0
	slog.Info("prewarming years", "type", "scheduler", "task", "prewarm", "outcome", "started",
		"trigger", "startup", "configured", len(s.cfg.PrewarmYears))
	defer func() {
		outcome, level := "succeeded", slog.LevelInfo
		if err != nil {
			outcome, level = "degraded", slog.LevelWarn
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				outcome = "canceled"
				if errors.Is(err, context.Canceled) {
					level = slog.LevelInfo
				}
			}
		}
		slog.Log(ctx, level, "prewarm ended", "type", "scheduler", "task", "prewarm", "outcome", outcome,
			"trigger", "startup", "configured", len(s.cfg.PrewarmYears), "cached", cached,
			"fetched", fetched, "failed", failed, "duration_ms", time.Since(start).Milliseconds())
	}()
	for _, year := range s.cfg.PrewarmYears {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if data, fresh, ok, cacheErr := s.cache.PeekYearContext(ctx, year); cacheErr == nil && ok && fresh {
			if _, payloadErr := DecodeYearData(data); payloadErr == nil {
				cached++
				slog.Debug("prewarm year skipped", "type", "scheduler", "task", "prewarm", "outcome", "skipped",
					"trigger", "startup", "year", year, "reason", "fresh_cache")
				continue
			}
		}
		didFetch, fetchErr := s.fetchAndStore(ctx, year, "prewarm")
		switch {
		case fetchErr != nil:
			failed++
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			if err == nil {
				err = fetchErr
			}
		case didFetch:
			fetched++
		default:
			cached++
		}
	}
	return err
}

// Process consumes request-owned slices; callers must not reuse them.
func (s *Scheduler) Process(shows, prevShows []anilist.Show, season string, year int, category string) []Show {
	start := time.Now()
	input := len(shows)
	afterWinterOverflow := input

	if season == "WINTER" && len(prevShows) > 0 {
		prevShows = filter.FilterBySeason(prevShows, "WINTER")
		seen := make(map[int]bool, len(shows))
		for _, sh := range shows {
			seen[sh.ID] = true
		}
		for _, sh := range prevShows {
			if !seen[sh.ID] && sh.StartDate.Month != nil && *sh.StartDate.Month == 12 {
				shows = append(shows, sh)
				seen[sh.ID] = true
			}
		}
		afterWinterOverflow = len(shows)
	}

	shows = filter.FilterBySeason(shows, season)
	afterSeason := len(shows)

	shows = filter.FilterByFormats(shows, s.cfg.IncludeTypes)
	afterFormat := len(shows)

	shows, filterStats := filter.FilterWithStats(shows, filter.Config{
		ExcludeTags: s.cfg.ExcludeTags,
	})

	var futureStats filter.FutureStats
	if s.cfg.FilterFutureEnabled {
		shows, futureStats = filter.FilterFutureWithStats(shows, 3)
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
		"task", "show_process",
		"outcome", "succeeded",
		"trigger", "request",
		"resolver_loaded", s.ResolverLoaded(),
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
	// Discovery is optional: coalesce busy requests instead of queuing writers.
	if len(resolved) > 0 && s.tracking.CompareAndSwap(false, true) {
		s.StartBackgroundFetch(5*time.Second, func(ctx context.Context) {
			defer s.tracking.Store(false)
			s.trackNewMappings(ctx, shows, batch, season, year)
		})
	}
	return resolved
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
		defer func() {
			if r := recover(); r != nil {
				slog.Error("panic in background task", "type", "scheduler", "recover", r)
			}
		}()
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

func (s *Scheduler) FetchAndStore(ctx context.Context, year int, trigger string) error {
	_, err := s.fetchAndStore(ctx, year, trigger)
	return err
}

// fetchAndStore reports whether this call used an upstream result, including a
// coalesced result, so prewarm can distinguish fetched years from fresh skips.
func (s *Scheduler) fetchAndStore(ctx context.Context, year int, trigger string) (fetched bool, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	start := time.Now()
	result := &inflightResult{done: make(chan struct{})}
	actual, loaded := s.inflight.LoadOrStore(year, result)
	if loaded {
		slog.Debug("waiting for year fetch", "type", "fetch", "task", "year_fetch", "outcome", "waiting",
			"year", year, "trigger", trigger, "reason", "inflight")
		res := actual.(*inflightResult)
		select {
		case <-res.done:
			return res.fetched, res.err
		case <-ctx.Done():
			slog.Debug("year fetch wait canceled", "type", "fetch", "task", "year_fetch", "outcome", "canceled",
				"year", year, "trigger", trigger, "reason", "wait_context_done", "duration_ms", time.Since(start).Milliseconds())
			return false, ctx.Err()
		}
	}
	attemptedFetch := false
	stage, showCount := "cache_check", 0
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("fetch year %d panic: %v", year, r)
		}
		if err == nil {
			s.clearFetchFailure(year)
			if attemptedFetch {
				fetched = true
				slog.Info("year cached", "type", "fetch", "task", "year_fetch", "outcome", "succeeded",
					"year", year, "trigger", trigger, "stage", stage, "shows", showCount,
					"duration_ms", time.Since(start).Milliseconds())
			}
		} else {
			if attemptedFetch && !errors.Is(err, context.Canceled) {
				s.recordFetchFailure(year)
			}
			// Cooldown rejections are skips, not a second failure of the attempt.
			if stage != "cooldown" {
				outcome, level := "failed", slog.LevelError
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					outcome, level = "canceled", slog.LevelWarn
					if errors.Is(err, context.Canceled) {
						level = slog.LevelInfo
					}
				}
				slog.Log(ctx, level, "year fetch ended", "type", "fetch", "task", "year_fetch", "outcome", outcome,
					"year", year, "trigger", trigger, "stage", stage, "error", err,
					"retry_in_ms", s.fetchCooldownRemaining(year).Milliseconds(), "duration_ms", time.Since(start).Milliseconds())
			}
		}
		result.err, result.fetched = err, fetched
		close(result.done)
		s.inflight.Delete(year)
	}()

	cachedData, fresh, present, cacheErr := s.cache.PeekYearContext(ctx, year)
	if cacheErr != nil {
		err = fmt.Errorf("check cached year %d before fetch: %w", year, cacheErr)
		return
	}
	if present && fresh {
		if _, payloadErr := DecodeYearData(cachedData); payloadErr == nil {
			slog.Debug("year fetch skipped", "type", "fetch", "task", "year_fetch", "outcome", "skipped",
				"year", year, "trigger", trigger, "reason", "fresh_cache", "duration_ms", time.Since(start).Milliseconds())
			return
		} else {
			slog.Warn("cached year data is invalid, refetching", "type", "fetch", "task", "year_fetch", "outcome", "degraded",
				"year", year, "trigger", trigger, "stage", stage, "reason", "invalid_cache", "error", payloadErr)
		}
	}
	if remaining := s.fetchCooldownRemaining(year); remaining > 0 {
		stage = "cooldown"
		err = fmt.Errorf("fetch year %d is cooling down for %s", year, remaining.Round(time.Second))
		slog.Debug("year fetch skipped", "type", "fetch", "task", "year_fetch", "outcome", "skipped",
			"year", year, "trigger", trigger, "reason", "cooldown", "retry_in_ms", remaining.Milliseconds(),
			"duration_ms", time.Since(start).Milliseconds())
		return
	}

	attemptedFetch = true
	fetchCtx, cancel := s.fetchContext(ctx)
	defer cancel()
	stage = "upstream"
	slog.Info("fetching year", "type", "fetch", "task", "year_fetch", "outcome", "started",
		"year", year, "trigger", trigger, "stage", stage)
	shows, fetchErr := s.client.FetchYear(fetchCtx, year)
	if fetchErr != nil {
		err = fmt.Errorf("fetch year %d: %w", year, fetchErr)
		return
	}
	if shows == nil {
		shows = []anilist.Show{}
	}
	showCount = len(shows)
	stage = "encode"
	data, marshalErr := json.Marshal(shows)
	if marshalErr != nil {
		err = fmt.Errorf("marshal year %d: %w", year, marshalErr)
		return
	}
	stage = "cache_write"
	if cacheErr := s.cache.SetYearContext(fetchCtx, year, data); cacheErr != nil {
		err = fmt.Errorf("cache set year %d: %w", year, cacheErr)
		return
	}
	return
}

func (s *Scheduler) fetchCooldownRemaining(year int) time.Duration {
	s.failureMu.Lock()
	defer s.failureMu.Unlock()
	if failure, ok := s.failures[year]; ok {
		if remaining := time.Until(failure.retryAt); remaining > 0 {
			return remaining
		}
	}
	return 0
}

func (s *Scheduler) recordFetchFailure(year int) {
	now := time.Now()
	s.failureMu.Lock()
	defer s.failureMu.Unlock()

	base := s.fetchRetryBase
	if base <= 0 {
		base = fetchRetryBase
	}
	maxDelay := s.fetchRetryMax
	if maxDelay < base {
		maxDelay = fetchRetryMax
	}
	delay := base
	if previous, ok := s.failures[year]; ok && previous.delay > 0 {
		delay = previous.delay * 2
	}
	if delay > maxDelay {
		delay = maxDelay
	}

	for trackedYear, failure := range s.failures {
		if now.Sub(failure.retryAt) > maxDelay {
			delete(s.failures, trackedYear)
		}
	}
	if _, exists := s.failures[year]; !exists && len(s.failures) >= maxTrackedFetchFailures {
		var oldestYear int
		var oldest time.Time
		for trackedYear, failure := range s.failures {
			if oldest.IsZero() || failure.retryAt.Before(oldest) {
				oldestYear, oldest = trackedYear, failure.retryAt
			}
		}
		delete(s.failures, oldestYear)
	}
	s.failures[year] = fetchFailure{retryAt: now.Add(delay), delay: delay}
}

func (s *Scheduler) clearFetchFailure(year int) {
	s.failureMu.Lock()
	delete(s.failures, year)
	s.failureMu.Unlock()
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
		slog.Debug("resolution skipped", "type", "resolver", "task", "show_resolution", "outcome", "skipped",
			"trigger", "request", "reason", "resolver_unavailable", "resolver_loaded", false)
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
	start := time.Now()
	m := s.resolver.Mapping()
	if m == nil || len(batch) == 0 {
		return
	}

	firstRun := false
	count, err := s.cache.CountSeenMappings(ctx)
	if err != nil {
		outcome, level := "failed", slog.LevelWarn
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			outcome = "canceled"
			if errors.Is(err, context.Canceled) {
				level = slog.LevelInfo
			}
		}
		slog.Log(ctx, level, "mapping discovery ended", "type", "mapping", "task", "mapping_discovery", "outcome", outcome,
			"trigger", "request", "stage", "count_seen", "year", year, "season", season, "error", err,
			"duration_ms", time.Since(start).Milliseconds())
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
		outcome, level := "failed", slog.LevelWarn
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			outcome = "canceled"
			if errors.Is(err, context.Canceled) {
				level = slog.LevelInfo
			}
		}
		slog.Log(ctx, level, "mapping discovery ended", "type", "mapping", "task", "mapping_discovery", "outcome", outcome,
			"trigger", "request", "stage", "record_seen", "year", year, "season", season, "error", err,
			"duration_ms", time.Since(start).Milliseconds())
		return
	}

	if firstRun || len(newMappings) == 0 {
		return
	}

	slog.Info("new mappings discovered",
		"type", "mapping",
		"task", "mapping_discovery",
		"outcome", "succeeded",
		"trigger", "request",
		"duration_ms", time.Since(start).Milliseconds(),
		"count", len(newMappings),
		"season", season,
		"year", year,
	)
	for _, m := range newMappings {
		slog.Debug("mapping added",
			"type", "mapping",
			"task", "mapping_discovery",
			"outcome", "succeeded",
			"trigger", "request",
			"tvdbid", m.TVDBID,
			"title", m.Title,
			"starts_at", m.StartsAt,
			"season", m.Season,
			"year", m.Year,
		)
	}
}

func (s *Scheduler) refreshStaleYears(ctx context.Context) {
	start := time.Now()
	currentYear := time.Now().Year()
	years, err := s.cache.NeedsRefreshYearsContext(ctx, currentYear, 1, 7)
	if err != nil {
		outcome, level := "failed", slog.LevelWarn
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			outcome = "canceled"
			if errors.Is(err, context.Canceled) {
				level = slog.LevelInfo
			}
		}
		slog.Log(ctx, level, "stale year scan ended", "type", "scheduler", "task", "stale_year_scan", "outcome", outcome,
			"trigger", "scheduled", "error", err, "duration_ms", time.Since(start).Milliseconds())
		return
	}
	slog.Debug("stale year scan complete", "type", "scheduler", "task", "stale_year_scan", "outcome", "succeeded",
		"trigger", "scheduled", "count", len(years), "duration_ms", time.Since(start).Milliseconds())
	for _, year := range years {
		if ctx.Err() != nil {
			return
		}
		yearCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		_ = s.FetchAndStore(yearCtx, year, "stale_refresh")
		cancel()
	}
}

func (s *Scheduler) prune(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	start := time.Now()
	slog.Debug("checking stale cache entries", "type", "scheduler", "task", "cache_prune", "outcome", "started", "trigger", "scheduled")
	n, err := s.cache.PruneStaleYearsContext(ctx, 14)
	if err != nil {
		outcome, level := "failed", slog.LevelWarn
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			outcome = "canceled"
			if errors.Is(err, context.Canceled) {
				level = slog.LevelInfo
			}
		}
		slog.Log(ctx, level, "cache prune ended", "type", "scheduler", "task", "cache_prune", "outcome", outcome,
			"trigger", "scheduled", "error", err, "duration_ms", time.Since(start).Milliseconds())
		return
	}
	level := slog.LevelDebug
	if n > 0 {
		level = slog.LevelInfo
	}
	slog.Log(ctx, level, "cache prune complete", "type", "scheduler", "task", "cache_prune", "outcome", "succeeded",
		"trigger", "scheduled", "count", n, "duration_ms", time.Since(start).Milliseconds())
	if n > 0 {
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
	if !time.Unix(last, 0).Add(vacuumInterval).Before(time.Now()) || !s.lastVacuum.CompareAndSwap(last, now) {
		slog.Debug("cache vacuum skipped", "type", "scheduler", "task", "cache_vacuum", "outcome", "skipped",
			"trigger", "prune", "reason", "interval_or_inflight")
		return
	}
	start := time.Now()
	slog.Info("vacuuming cache", "type", "scheduler", "task", "cache_vacuum", "outcome", "started", "trigger", "prune")
	if err := s.cache.VacuumContext(ctx); err != nil {
		outcome, level := "failed", slog.LevelWarn
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			outcome = "canceled"
			if errors.Is(err, context.Canceled) {
				level = slog.LevelInfo
			}
		}
		slog.Log(ctx, level, "cache vacuum ended", "type", "scheduler", "task", "cache_vacuum", "outcome", outcome,
			"trigger", "prune", "error", err, "duration_ms", time.Since(start).Milliseconds())
		return
	}
	slog.Info("cache vacuum complete", "type", "scheduler", "task", "cache_vacuum", "outcome", "succeeded",
		"trigger", "prune", "duration_ms", time.Since(start).Milliseconds())
}

func (s *Scheduler) logCacheStats(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	start := time.Now()
	stats, err := s.cache.StatsContext(ctx)
	if err != nil {
		outcome, level := "failed", slog.LevelWarn
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			outcome = "canceled"
			if errors.Is(err, context.Canceled) {
				level = slog.LevelInfo
			}
		}
		slog.Log(ctx, level, "cache stats ended", "type", "scheduler", "task", "cache_stats", "outcome", outcome,
			"trigger", "scheduled", "error", err, "duration_ms", time.Since(start).Milliseconds())
		return
	}
	slog.Debug("cache stats", "type", "scheduler", "task", "cache_stats", "outcome", "succeeded", "trigger", "scheduled",
		"entries", stats.Entries, "hits", stats.Hits, "misses", stats.Misses, "duration_ms", time.Since(start).Milliseconds())
}

func (s *Scheduler) Wait(ctx context.Context) error {
	s.closeBackgroundFetches()
	s.waitOnce.Do(func() {
		go func() {
			s.wg.Wait()
			close(s.waitDone)
		}()
	})
	select {
	case <-s.waitDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
