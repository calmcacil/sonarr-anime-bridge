package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
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
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := config.Load()

	setupLogging(cfg.LogLevel)

	slog.Info("starting",
		"version", version,
		"port", cfg.Port,
		"prewarm_years", cfg.PrewarmYears,
	)

	db, err := cache.Open(cfg.CacheDBPath)
	if err != nil {
		return fmt.Errorf("open cache: %w", err)
	}
	defer db.Close() //nolint:errcheck // cleanup on exit

	sched := scheduler.New(db, cfg)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	sched.LoadResolver(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("/list", handleList(ctx, db, sched, cfg))
	mux.HandleFunc("/health", handleHealth(db, sched))
	mux.HandleFunc("/cache/stats", handleCacheStats(db))
	mux.HandleFunc("/cache/clear", handleCacheClear(db))

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
		slog.Info("listening", "addr", server.Addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serverErrCh <- err
		}
	}()

	prewarmDone := make(chan struct{})
	go func() {
		defer close(prewarmDone)
		slog.Info("prewarming cache")
		if err := sched.Prewarm(ctx); err != nil {
			slog.Error("prewarm failed", "error", err)
		}
		slog.Info("prewarm complete", "entries", db.Stats().Entries)
	}()

	var serverErr error
	select {
	case <-prewarmDone:
	case <-ctx.Done():
		slog.Info("shutting down", "signal", ctx.Err())
	case serverErr = <-serverErrCh:
		slog.Error("server error", "error", serverErr)
		stop()
	}

	if serverErr == nil && ctx.Err() == nil {
		select {
		case <-ctx.Done():
			slog.Info("shutting down", "signal", ctx.Err())
		case serverErr = <-serverErrCh:
			slog.Error("server error", "error", serverErr)
			stop()
		}
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	shutdownErr := server.Shutdown(shutdownCtx)

	waitCtx, waitCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer waitCancel()
	select {
	case <-prewarmDone:
	case <-waitCtx.Done():
		slog.Warn("prewarm did not finish in time", "error", waitCtx.Err())
	}
	if err := sched.Wait(waitCtx); err != nil {
		slog.Warn("some background goroutines did not finish in time", "error", err)
	}

	if serverErr != nil {
		return serverErr
	}
	return shutdownErr
}

func handleList(ctx context.Context, db *cache.Cache, sched *scheduler.Scheduler, cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		season := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("season")))
		if season == "" {
			season = "ALL"
		}

		validSeasons := map[string]bool{"WINTER": true, "SPRING": true, "SUMMER": true, "FALL": true, "ALL": true}
		if !validSeasons[season] {
			http.Error(w, "invalid season parameter", http.StatusBadRequest)
			return
		}

		if !sched.ResolverLoaded() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"status":"degraded","reason":"resolver not loaded"}`))
			return
		}

		yearStr := strings.TrimSpace(r.URL.Query().Get("year"))
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

		data, fresh, ok := db.GetYear(year)
		if !ok {
			slog.Info("cache miss, triggering backfill",
				"season", season,
				"year", year,
				"category", category,
			)

			if err := sched.FetchAndStore(r.Context(), year, "cache_miss"); err != nil {
				slog.Error("trigger backfill failed", "error", err)
				writeJSON(w, []byte("[]"))
				return
			}

			data, fresh, ok = db.GetYear(year)
			if !ok {
				slog.Warn("fetch completed but data still missing, returning empty", "year", year)
				writeJSON(w, []byte("[]"))
				return
			}
		}

		if season == "WINTER" && !db.HasYear(year-1) {
			slog.Debug("winter overflow: prior year not cached, triggering backfill",
				"prior_year", year-1,
			)
			sched.FetchAndStoreAsync(ctx, year-1, "winter_overflow")
		}

		shows, err := sched.Process(data, season, year, category)
		if err != nil {
			slog.Error("processing failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		if !fresh {
			slog.Debug("serving stale data, triggering refresh",
				"season", season,
				"year", year,
				"category", category,
			)
			sched.FetchAndStoreAsync(ctx, year, "stale_refresh")
		}

		body, err := json.Marshal(shows)
		if err != nil {
			slog.Error("marshal result", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, body)
	}
}

func handleHealth(db *cache.Cache, sched *scheduler.Scheduler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		healthy := true
		if err := db.Ping(); err != nil {
			slog.Error("health check failed", "error", err)
			healthy = false
		}
		resolverOK := sched.ResolverLoaded()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case healthy && resolverOK:
			w.Write([]byte(`{"status":"ok"}`))
		case healthy:
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"status":"degraded","reason":"resolver not loaded"}`))
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"status":"unhealthy"}`))
		}
	}
}

func handleCacheStats(db *cache.Cache) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		stats := db.Stats()
		data, err := json.Marshal(stats)
		if err != nil {
			slog.Error("marshal cache stats", "error", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		writeJSON(w, data)
	}
}

func handleCacheClear(db *cache.Cache) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		slog.Warn("clearing all cache entries")
		if err := db.Clear(); err != nil {
			slog.Error("cache clear failed", "error", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	}
}

func writeJSON(w http.ResponseWriter, data []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		srw := &statusResponseWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(srw, r)
		slog.Debug("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", srw.status,
			"duration", time.Since(start),
		)
	})
}

func recoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rc := recover(); rc != nil {
				slog.Error("panic recovered",
					"path", r.URL.Path,
					"error", rc,
				)
				w.WriteHeader(http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusResponseWriter struct {
	http.ResponseWriter
	status int
}

func (srw *statusResponseWriter) WriteHeader(code int) {
	srw.status = code
	srw.ResponseWriter.WriteHeader(code)
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
