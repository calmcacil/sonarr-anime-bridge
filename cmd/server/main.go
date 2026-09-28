package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/calmcacil/sonarr-anime-bridge/internal/cache"
	"github.com/calmcacil/sonarr-anime-bridge/internal/config"
	"github.com/calmcacil/sonarr-anime-bridge/internal/scheduler"
)

var version = "dev"

func main() {
	healthcheck := flag.Bool("healthcheck", false, "run container healthcheck")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	if *healthcheck {
		if err := runHealthcheck(); err != nil {
			fmt.Fprintln(os.Stderr, "healthcheck:", err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func runHealthcheck() error {
	port := config.LoadQuiet().Port
	client := &http.Client{Timeout: 4 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/health", port))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return nil
}

func run() error {
	cfg := config.LoadQuiet()

	setupLogging(cfg.LogLevel)
	config.Log(cfg)

	slog.Info("starting",
		"type", "system",
		"version", version,
		"port", cfg.Port,
		"prewarm_years", cfg.PrewarmYears,
	)

	if err := validateRuntimeDataDirs(cfg); err != nil {
		return fmt.Errorf("validate data directories: %w", err)
	}

	db, err := cache.Open(cfg.CacheDBPath)
	if err != nil {
		return fmt.Errorf("open cache: %w", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			slog.Warn("close cache failed", "type", "system", "error", err)
		}
	}()

	sched := scheduler.New(db, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sched.LoadResolverContext(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("/list", handleList(db, sched, cfg))
	mux.HandleFunc("/health", handleHealth(db, sched, cfg.PrewarmYears))
	mux.HandleFunc("/cache/stats", handleCacheStats(db, cfg))
	mux.HandleFunc("/cache/clear", handleCacheClear(db, cfg))

	server := &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.Port),
		Handler:      loggingMiddleware(recoveryMiddleware(mux)),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 120 * time.Second,
		IdleTimeout:  30 * time.Second,
	}

	sched.StartBackground(ctx)

	serverErrCh := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				serverErrCh <- fmt.Errorf("panic in HTTP server goroutine: %v", r)
			}
		}()
		slog.Info("listening", "type", "http", "addr", server.Addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serverErrCh <- err
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	prewarmDone := make(chan struct{})
	go func() {
		defer close(prewarmDone)
		slog.Info("prewarming cache", "type", "scheduler")
		if err := sched.Prewarm(ctx); err != nil {
			slog.Error("prewarm failed", "type", "scheduler", "error", err)
		}
		stats, statsErr := db.StatsContext(ctx)
		if statsErr != nil {
			slog.Warn("cache stats failed after prewarm", "type", "scheduler", "error", statsErr)
		} else {
			slog.Info("prewarm complete", "type", "scheduler", "entries", stats.Entries)
		}
	}()

	select {
	case sig := <-sigCh:
		slog.Info("shutting down", "type", "system", "signal", sig)
		cancel()
		<-prewarmDone
	case err := <-serverErrCh:
		cancel()
		<-prewarmDone
		return fmt.Errorf("server error: %w", err)
	case <-prewarmDone:
		select {
		case sig := <-sigCh:
			slog.Info("shutting down", "type", "system", "signal", sig)
		case err := <-serverErrCh:
			cancel()
			return fmt.Errorf("server error: %w", err)
		}
		cancel()
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	serverErr := server.Shutdown(shutdownCtx)

	waitCtx, waitCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer waitCancel()
	if err := sched.Wait(waitCtx); err != nil {
		slog.Warn("some background goroutines did not finish in time", "type", "system", "error", err)
	}

	return serverErr
}

func validateRuntimeDataDirs(cfg *config.Config) error {
	if cfg == nil {
		return nil
	}

	dirs := make(map[string][]string)
	if cfg.CacheDBPath != "" && cfg.CacheDBPath != ":memory:" {
		dir := filepath.Clean(filepath.Dir(cfg.CacheDBPath))
		dirs[dir] = append(dirs[dir], "CACHE_DB_PATH")
	}
	if cfg.AnibridgeMappingPath != "" {
		dir := filepath.Clean(filepath.Dir(cfg.AnibridgeMappingPath))
		dirs[dir] = append(dirs[dir], "MAPPING_PATH")
	}

	for dir, labels := range dirs {
		if err := validateRuntimeDataDir(dir); err != nil {
			return fmt.Errorf("%s directory %q must be readable and writable: %w", strings.Join(labels, "/"), dir, err)
		}
	}
	return nil
}

func validateRuntimeDataDir(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("stat: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("not a directory")
	}
	if _, err := os.ReadDir(dir); err != nil {
		return fmt.Errorf("read: %w", err)
	}

	probe, err := os.CreateTemp(dir, ".sonarr-anime-bridge-write-test-*")
	if err != nil {
		return fmt.Errorf("write probe: %w", err)
	}
	_, writeErr := probe.Write([]byte("ok"))
	closeErr := probe.Close()
	removeErr := os.Remove(probe.Name())
	switch {
	case writeErr != nil:
		return fmt.Errorf("write probe: %w", writeErr)
	case closeErr != nil:
		return fmt.Errorf("write probe close: %w", closeErr)
	case removeErr != nil:
		return fmt.Errorf("remove write probe: %w", removeErr)
	}
	return nil
}

var validSeasons = map[string]bool{"WINTER": true, "SPRING": true, "SUMMER": true, "FALL": true, "ALL": true}

func handleList(db *cache.Cache, sched *scheduler.Scheduler, cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			methodNotAllowed(w, "GET, HEAD")
			return
		}

		season := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("season")))
		if season == "" {
			season = "ALL"
		}

		if !validSeasons[season] {
			http.Error(w, "invalid season parameter", http.StatusBadRequest)
			return
		}

		if !sched.ResolverLoaded() {
			writeJSON(w, http.StatusServiceUnavailable, []byte(`{"status":"degraded","reason":"resolver not loaded"}`))
			return
		}

		yearStr := r.URL.Query().Get("year")
		year := time.Now().Year()
		if yearStr != "" {
			y, err := strconv.Atoi(yearStr)
			if err != nil || y <= 0 {
				http.Error(w, "invalid year parameter", http.StatusBadRequest)
				return
			}
			if y < year-10 || y > year+10 {
				http.Error(w, fmt.Sprintf("year %d out of range (must be within %d to %d)", y, year-10, year+10), http.StatusBadRequest)
				return
			}
			year = y
		}

		category := strings.TrimSpace(r.URL.Query().Get("category"))
		if category == "" {
			category = "series"
		} else if category != "series" && category != "series-new" {
			http.Error(w, fmt.Sprintf("invalid category: %q (valid values: series, series-new)", category), http.StatusBadRequest)
			return
		}

		metadata := requestMetadata(r.Context())
		metadata.season = season
		metadata.year = year
		metadata.category = category
		respondEmpty := func() {
			metadata.resultCount = 0
			metadata.resultCountSet = true
			writeJSON(w, http.StatusOK, []byte("[]"))
		}
		backgroundFetch := func(fetchYear int, trigger, failureMsg string) {
			sched.StartBackgroundFetch(90*time.Second, func(fetchCtx context.Context) {
				if err := sched.FetchAndStore(fetchCtx, fetchYear, trigger); err != nil {
					slog.Error(failureMsg,
						"type", "http",
						"year", fetchYear,
						"season", season,
						"category", category,
						"trigger", trigger,
						"error", err,
					)
				}
			})
		}

		data, fresh, ok, err := db.GetYearContext(r.Context(), year)
		if err == nil {
			switch {
			case !ok:
				metadata.cacheState = "miss"
			case fresh:
				metadata.cacheState = "hit"
			default:
				metadata.cacheState = "stale"
			}
		}
		if err != nil {
			slog.Error("cache read failed", "type", "http", "error", err, "year", year)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		fetchTrigger := "cache_miss"
		needsFetch := !ok
		if ok {
			if payloadErr := scheduler.ValidateYearData(data); payloadErr != nil {
				needsFetch = true
				fetchTrigger = "cache_recovery"
				slog.Warn("cached year data is invalid, fetching replacement",
					"type", "http",
					"year", year,
					"error", payloadErr,
				)
			}
		}
		if needsFetch {
			slog.Info("cache miss or invalid payload, fetching before response",
				"type", "http",
				"season", season,
				"year", year,
				"category", category,
			)

			fetchCtx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
			if err := sched.FetchAndStore(fetchCtx, year, fetchTrigger); err != nil {
				cancel()
				slog.Error("trigger backfill failed",
					"type", "http",
					"year", year,
					"season", season,
					"category", category,
					"trigger", fetchTrigger,
					"error", err,
				)
				respondEmpty()
				return
			}
			cancel()

			data, fresh, ok, err = db.GetYearContext(r.Context(), year)
			if err != nil {
				slog.Error("cache read after fetch failed", "type", "http", "error", err, "year", year)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			if !ok || scheduler.ValidateYearData(data) != nil {
				slog.Warn("fetch completed without valid cached data, returning empty",
					"type", "http",
					"year", year,
					"season", season,
					"category", category,
					"trigger", fetchTrigger,
				)
				respondEmpty()
				return
			}
		}

		if season == "WINTER" {
			priorData, _, hasPriorYear, err := db.GetYearContext(r.Context(), year-1)
			if err != nil {
				slog.Error("prior year cache check failed", "type", "http", "error", err, "year", year-1)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			priorNeedsFetch := !hasPriorYear
			if hasPriorYear && scheduler.ValidateYearData(priorData) != nil {
				priorNeedsFetch = true
			}
			if priorNeedsFetch {
				slog.Debug("winter overflow: prior year not cached, fetching in background",
					"type", "http",
					"prior_year", year-1,
				)
				backgroundFetch(year-1, "winter_overflow", "winter overflow backfill failed")
			}
		}

		shows, err := sched.ProcessContext(r.Context(), data, season, year, category)
		if err != nil {
			slog.Error("processing failed",
				"type", "http",
				"year", year,
				"season", season,
				"category", category,
				"trigger", "request",
				"error", err,
			)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		if !fresh {
			slog.Debug("serving stale data, refreshing in background",
				"type", "http",
				"season", season,
				"year", year,
				"category", category,
			)
			backgroundFetch(year, "stale_refresh", "stale refresh failed")
		}

		metadata.resultCount = len(shows)
		metadata.resultCountSet = true
		body, err := json.Marshal(shows)
		if err != nil {
			slog.Error("marshal result", "type", "http", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, body)
	}
}

type healthStatus string

const (
	healthStatusOK        healthStatus = "ok"
	healthStatusWarming   healthStatus = "warming"
	healthStatusDegraded  healthStatus = "degraded"
	healthStatusUnhealthy healthStatus = "unhealthy"
)

type healthCheck struct {
	Status healthStatus `json:"status"`
}

type healthChecks struct {
	Cache    healthCheck `json:"cache"`
	Resolver healthCheck `json:"resolver"`
}

type healthResponse struct {
	Status healthStatus `json:"status"`
	Reason string       `json:"reason,omitempty"`
	Checks healthChecks `json:"checks"`
}

func handleHealth(db *cache.Cache, sched *scheduler.Scheduler, years []int) http.HandlerFunc {
	prewarmYears := append([]int(nil), years...)
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			methodNotAllowed(w, "GET, HEAD")
			return
		}

		cacheReady, cacheErr := db.HasYearsContext(r.Context(), prewarmYears)
		cacheStatus := healthStatusWarming
		aggregateStatus := healthStatusUnhealthy
		statusCode := http.StatusServiceUnavailable
		if cacheErr != nil {
			slog.Error("health check failed", "type", "http", "error", cacheErr)
			cacheStatus = healthStatusUnhealthy
		} else {
			if cacheReady {
				cacheStatus = healthStatusOK
			}
			aggregateStatus = healthStatusOK
			statusCode = http.StatusOK
		}

		resolverStatus := healthStatusDegraded
		resolverLoaded := sched.ResolverLoaded()
		if resolverLoaded {
			resolverStatus = healthStatusOK
		} else if cacheErr == nil {
			aggregateStatus = healthStatusDegraded
			statusCode = http.StatusServiceUnavailable
		}

		response := healthResponse{
			Status: aggregateStatus,
			Checks: healthChecks{
				Cache:    healthCheck{Status: cacheStatus},
				Resolver: healthCheck{Status: resolverStatus},
			},
		}
		if !resolverLoaded && cacheErr == nil {
			response.Reason = "resolver not loaded"
		}
		body, err := json.Marshal(response)
		if err != nil {
			slog.Error("marshal health response", "type", "http", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, statusCode, body)
	}
}

func handleCacheStats(db *cache.Cache, cfg *config.Config) http.HandlerFunc {
	return debugHandler(cfg, "GET, HEAD", func(w http.ResponseWriter, r *http.Request) {
		stats, err := db.StatsContext(r.Context())
		if err != nil {
			slog.Error("cache stats failed", "type", "http", "error", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		data, err := json.Marshal(stats)
		if err != nil {
			slog.Error("marshal cache stats", "type", "http", "error", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, data)
	})
}

func handleCacheClear(db *cache.Cache, cfg *config.Config) http.HandlerFunc {
	return debugHandler(cfg, "POST", func(w http.ResponseWriter, r *http.Request) {
		slog.Warn("clearing all cache entries", "type", "http")
		if err := db.ClearContext(r.Context()); err != nil {
			slog.Error("cache clear failed", "type", "http", "error", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, []byte(`{"status":"ok"}`))
	})
}

// debugHandler enforces the allowed methods, then hides the endpoint (404)
// unless debug endpoints are enabled and the request is authorized.
func debugHandler(cfg *config.Config, allow string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !slices.Contains(strings.Split(allow, ", "), r.Method) {
			methodNotAllowed(w, allow)
			return
		}
		if !authorizedDebugRequest(r, cfg) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		next(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, data []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write(data); err != nil {
		slog.Warn("write response failed", "type", "http", "error", err)
	}
}

func methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

func authorizedDebugRequest(r *http.Request, cfg *config.Config) bool {
	if cfg == nil || !cfg.DebugEndpointsEnabled {
		return false
	}
	if cfg.AdminToken == "" {
		return true
	}
	return r.Header.Get("Authorization") == "Bearer "+cfg.AdminToken
}

type requestMetadataKey struct{}

type requestLogMetadata struct {
	season         string
	year           int
	category       string
	resultCount    int
	resultCountSet bool
	cacheState     string
}

// requestMetadata returns the logging middleware's per-request metadata, or a
// throwaway value when the handler runs without the middleware.
func requestMetadata(ctx context.Context) *requestLogMetadata {
	if metadata, ok := ctx.Value(requestMetadataKey{}).(*requestLogMetadata); ok {
		return metadata
	}
	return &requestLogMetadata{}
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		srw := &statusResponseWriter{ResponseWriter: w, status: http.StatusOK}
		metadata := &requestLogMetadata{}
		request := r.WithContext(context.WithValue(r.Context(), requestMetadataKey{}, metadata))
		next.ServeHTTP(srw, request)

		route := request.Pattern

		if route == "" {
			route = "unknown"
		}
		logger := slog.With("type", "http")
		attrs := []any{
			"method", r.Method,
			"route", route,
			"status", srw.status,
			"duration_ms", time.Since(start).Milliseconds(),
		}
		if metadata.season != "" {
			attrs = append(attrs, "season", metadata.season)
		}
		if metadata.year != 0 {
			attrs = append(attrs, "year", metadata.year)
		}
		if metadata.category != "" {
			attrs = append(attrs, "category", metadata.category)
		}
		if metadata.resultCountSet {
			attrs = append(attrs, "result_count", metadata.resultCount)
		}
		if metadata.cacheState != "" {
			attrs = append(attrs, "cache_state", metadata.cacheState)
		}

		if srw.status >= http.StatusBadRequest {
			logger.Warn("request completed", attrs...)
		} else if route != "/health" {
			logger.Info("request completed", attrs...)
		}
	})
}

func recoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rc := recover(); rc != nil {
				slog.Error("panic recovered",
					"type", "http",
					"path", r.URL.Path,
					"error", rc,
				)
				if srw, ok := w.(*statusResponseWriter); !ok || !srw.wroteHeader {
					w.WriteHeader(http.StatusInternalServerError)
				}
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusResponseWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (srw *statusResponseWriter) Unwrap() http.ResponseWriter {
	return srw.ResponseWriter
}

func (srw *statusResponseWriter) WriteHeader(code int) {
	if srw.wroteHeader {
		return
	}
	srw.status = code
	srw.wroteHeader = true
	srw.ResponseWriter.WriteHeader(code)
}

func (srw *statusResponseWriter) Write(data []byte) (int, error) {
	if !srw.wroteHeader {
		srw.WriteHeader(http.StatusOK)
	}
	return srw.ResponseWriter.Write(data)
}

func setupLogging(level string) {
	var l slog.Level
	switch strings.ToLower(level) {
	case "debug":
		l = slog.LevelDebug
	case "warn", "warning":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l})
	slog.SetDefault(slog.New(handler))
}
