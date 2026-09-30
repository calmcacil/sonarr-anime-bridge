package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/calmcacil/sonarr-anime-bridge/internal/anilist"
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

func run() (runErr error) {
	setupLogging(os.Getenv("LOG_LEVEL"))
	cfg := config.Load()
	start := time.Now()
	startupComplete := false
	defer func() {
		if runErr != nil {
			task := "service"
			if !startupComplete {
				task = "startup"
			}
			slog.Error("bridge stopped with an error", "type", "system", "task", task, "outcome", "failed", "error", runErr, "duration_ms", time.Since(start).Milliseconds())
		}
	}()

	slog.Info("starting bridge",
		"type", "system", "task", "startup", "outcome", "started",
		"version", version,
		"port", cfg.Port,
		"prewarm_years", cfg.PrewarmYears,
	)

	if err := validateRuntimeDataDirs(cfg); err != nil {
		return fmt.Errorf("validate data directories: %w", err)
	}

	slog.Info("opening year cache", "type", "cache", "task", "cache_open", "outcome", "started")
	cacheStart := time.Now()
	db, err := cache.Open(cfg.CacheDBPath)
	if err != nil {
		return fmt.Errorf("open cache: %w", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			slog.Warn("year cache close failed", "type", "cache", "task", "cache_close", "outcome", "failed", "error", err)
		}
	}()
	slog.Info("year cache opened", "type", "cache", "task", "cache_open", "outcome", "succeeded", "duration_ms", time.Since(cacheStart).Milliseconds())

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

	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", server.Addr, err)
	}
	defer func() { _ = listener.Close() }()
	slog.Info("HTTP listener ready", "type", "http", "task", "http_listen", "outcome", "succeeded", "addr", listener.Addr().String(), "duration_ms", time.Since(start).Milliseconds(), "resolver_loaded", sched.ResolverLoaded())
	startupComplete = true
	startupOutcome := "succeeded"
	if !sched.ResolverLoaded() {
		startupOutcome = "degraded"
	}
	slog.Info("bridge startup completed", "type", "system", "task", "startup", "outcome", startupOutcome, "duration_ms", time.Since(start).Milliseconds(), "resolver_loaded", sched.ResolverLoaded())
	sched.StartBackground(ctx)

	serverErrCh := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				serverErrCh <- fmt.Errorf("panic in HTTP server goroutine: %v", r)
			}
		}()
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			serverErrCh <- err
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	prewarmDone := make(chan struct{})
	go func() {
		defer close(prewarmDone)
		// Prewarm owns its aggregate outcome; year fetches own individual failures.
		_ = sched.Prewarm(ctx)
	}()

	select {
	case sig := <-sigCh:
		slog.Info("stopping bridge", "type", "system", "task", "shutdown", "outcome", "started", "signal", sig.String())
		cancel()
		<-prewarmDone
	case err := <-serverErrCh:
		cancel()
		<-prewarmDone
		return fmt.Errorf("server error: %w", err)
	case <-prewarmDone:
		select {
		case sig := <-sigCh:
			slog.Info("stopping bridge", "type", "system", "task", "shutdown", "outcome", "started", "signal", sig.String())
		case err := <-serverErrCh:
			cancel()
			return fmt.Errorf("server error: %w", err)
		}
		cancel()
	}

	shutdownStart := time.Now()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	serverErr := server.Shutdown(shutdownCtx)

	waitCtx, waitCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer waitCancel()
	if err := sched.Wait(waitCtx); err != nil {
		slog.Warn("background tasks did not stop before shutdown deadline", "type", "system", "task", "shutdown", "outcome", "degraded", "error", err, "duration_ms", time.Since(shutdownStart).Milliseconds())
		if serverErr == nil {
			serverErr = err
		}
	}

	if serverErr == nil {
		slog.Info("bridge stopped", "type", "system", "task", "shutdown", "outcome", "succeeded", "duration_ms", time.Since(shutdownStart).Milliseconds())
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

		params := r.URL.Query()
		season := strings.ToUpper(strings.TrimSpace(params.Get("season")))
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

		yearStr := params.Get("year")
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

		category := strings.TrimSpace(params.Get("category"))
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
		backgroundFetch := func(fetchYear int, trigger string) {
			sched.StartBackgroundFetch(90*time.Second, func(fetchCtx context.Context) {
				_ = sched.FetchAndStore(fetchCtx, fetchYear, trigger)
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
			slog.Error("list cache read failed", "type", "http", "task", "list", "outcome", "failed", "stage", "cache_read", "error", err, "year", year)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		fetchTrigger := "cache_miss"
		needsFetch := !ok
		var anime []anilist.Show
		if ok {
			anime, err = scheduler.DecodeYearData(data)
			if err != nil {
				needsFetch = true
				fetchTrigger = "cache_recovery"
				metadata.cacheState = "invalid"
			}
		}
		if needsFetch {
			slog.Debug("list waiting for year fetch", "type", "http", "task", "list", "outcome", "waiting", "year", year, "trigger", fetchTrigger)

			fetchCtx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
			if err := sched.FetchAndStore(fetchCtx, year, fetchTrigger); err != nil {
				cancel()
				metadata.outcome = "degraded"
				metadata.reason = "fetch_failed"
				respondEmpty()
				return
			}
			cancel()

			data, fresh, ok, err = db.GetYearContext(r.Context(), year)
			if err != nil {
				slog.Error("list cache read after fetch failed", "type", "http", "task", "list", "outcome", "failed", "stage", "cache_read", "error", err, "year", year)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			anime, err = scheduler.DecodeYearData(data)
			if !ok || err != nil {
				metadata.outcome = "degraded"
				metadata.reason = "cache_unavailable_after_fetch"
				respondEmpty()
				return
			}
		}

		var priorShows []anilist.Show
		if season == "WINTER" {
			priorData, _, hasPriorYear, err := db.GetYearContext(r.Context(), year-1)
			if err != nil {
				slog.Error("winter backfill cache check failed", "type", "http", "task", "list", "outcome", "failed", "stage", "prior_year_read", "error", err, "year", year-1)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			if hasPriorYear {
				priorShows, err = scheduler.DecodeYearData(priorData)
				if err != nil {
					slog.Warn("winter backfill cache payload invalid; scheduling replacement", "type", "http", "task", "winter_overflow", "outcome", "degraded", "trigger", "request", "year", year-1, "reason", "invalid_cache", "error", err)
				}
			}
			if !hasPriorYear || err != nil {
				priorShows = nil
				metadata.winterBackfill = true
				backgroundFetch(year-1, "winter_overflow")
			}
		}

		shows := sched.Process(anime, priorShows, season, year, category)

		if !fresh {
			metadata.refreshScheduled = true
			backgroundFetch(year, "stale_refresh")
		}

		metadata.resultCount = len(shows)
		metadata.resultCountSet = true
		body, err := json.Marshal(shows)
		if err != nil {
			slog.Error("list response encoding failed", "type", "http", "task", "list", "outcome", "failed", "stage", "encode", "error", err)
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
			slog.Error("health cache check failed", "type", "http", "task", "health", "outcome", "failed", "error", cacheErr)
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
			slog.Error("health response encoding failed", "type", "http", "task", "health", "outcome", "failed", "stage", "encode", "error", err)
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
			slog.Error("cache statistics query failed", "type", "http", "task", "cache_stats", "outcome", "failed", "error", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		data, err := json.Marshal(stats)
		if err != nil {
			slog.Error("cache statistics encoding failed", "type", "http", "task", "cache_stats", "outcome", "failed", "stage", "encode", "error", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, data)
	})
}

func handleCacheClear(db *cache.Cache, cfg *config.Config) http.HandlerFunc {
	return debugHandler(cfg, "POST", func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		slog.Info("clearing year cache", "type", "cache", "task", "cache_clear", "outcome", "started", "trigger", "admin")
		if err := db.ClearContext(r.Context()); err != nil {
			slog.Error("year cache clear failed", "type", "cache", "task", "cache_clear", "outcome", "failed", "error", err, "duration_ms", time.Since(start).Milliseconds())
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		slog.Info("year cache cleared", "type", "cache", "task", "cache_clear", "outcome", "succeeded", "duration_ms", time.Since(start).Milliseconds())
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
		if _, tracked := w.(*statusResponseWriter); !tracked {
			slog.Warn("HTTP response write failed", "type", "http", "task", "request", "outcome", "failed", "stage", "write", "error", err)
		}
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
	season           string
	year             int
	category         string
	resultCount      int
	resultCountSet   bool
	cacheState       string
	outcome          string
	reason           string
	refreshScheduled bool
	winterBackfill   bool
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
		logger := slog.Default()
		attrs := []any{
			"type", "http", "task", "request",
			"method", r.Method,
			"route", route,
			"status", srw.status,
			"duration_ms", time.Since(start).Milliseconds(),
		}
		outcome := metadata.outcome
		if outcome == "" {
			outcome = "succeeded"
		}
		if srw.status >= http.StatusBadRequest || srw.writeErr != nil {
			outcome = "failed"
		}
		attrs = append(attrs, "outcome", outcome)
		if srw.writeErr != nil {
			attrs = append(attrs, "reason", "response_write_failed", "error", srw.writeErr)
		} else if metadata.reason != "" {
			attrs = append(attrs, "reason", metadata.reason)
		}
		if metadata.refreshScheduled {
			attrs = append(attrs, "refresh_scheduled", true)
		}
		if metadata.winterBackfill {
			attrs = append(attrs, "winter_backfill_scheduled", true)
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

		if outcome != "succeeded" {
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
				requestMetadata(r.Context()).outcome = "failed"
				requestMetadata(r.Context()).reason = "panic"
				slog.Error("HTTP handler panic recovered", "type", "http", "task", "request", "outcome", "failed", "error", rc)
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
	writeErr    error
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
	n, err := srw.ResponseWriter.Write(data)
	if err != nil {
		srw.writeErr = err
	}
	return n, err
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
	handler := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: l})
	slog.SetDefault(slog.New(handler))
}
