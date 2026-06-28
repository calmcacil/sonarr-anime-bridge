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
	flag.Parse()
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
	port := config.Load().Port
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sched.LoadResolverContext(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("/list", handleList(db, sched, cfg))
	mux.HandleFunc("/health", handleHealth(db, sched))
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
		slog.Info("listening", "addr", server.Addr)
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
		slog.Info("prewarming cache")
		if err := sched.Prewarm(ctx); err != nil {
			slog.Error("prewarm failed", "error", err)
		}
		stats, statsErr := db.StatsContext(ctx)
		if statsErr != nil {
			slog.Warn("cache stats failed after prewarm", "error", statsErr)
		} else {
			slog.Info("prewarm complete", "entries", stats.Entries)
		}
	}()

	select {
	case sig := <-sigCh:
		slog.Info("shutting down", "signal", sig)
		cancel()
		<-prewarmDone
	case err := <-serverErrCh:
		cancel()
		<-prewarmDone
		return fmt.Errorf("server error: %w", err)
	case <-prewarmDone:
		select {
		case sig := <-sigCh:
			slog.Info("shutting down", "signal", sig)
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
		slog.Warn("some background goroutines did not finish in time", "error", err)
	}

	return serverErr
}

func handleList(db *cache.Cache, sched *scheduler.Scheduler, cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

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
			if err := writeJSONStatus(w, http.StatusServiceUnavailable, []byte(`{"status":"degraded","reason":"resolver not loaded"}`)); err != nil {
				slog.Warn("write response failed", "error", err)
			}
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

		data, fresh, ok, err := db.GetYearContext(r.Context(), year)
		if err != nil {
			slog.Error("cache read failed", "error", err, "year", year)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if !ok {
			slog.Info("cache miss, fetching before response",
				"season", season,
				"year", year,
				"category", category,
			)

			fetchCtx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
			if err := sched.FetchAndStore(fetchCtx, year, "cache_miss"); err != nil {
				cancel()
				slog.Error("trigger backfill failed", "error", err)
				if writeErr := writeJSON(w, []byte("[]")); writeErr != nil {
					slog.Warn("write response failed", "error", writeErr)
				}
				return
			}
			cancel()

			data, fresh, ok, err = db.GetYearContext(r.Context(), year)
			if err != nil {
				slog.Error("cache read after fetch failed", "error", err, "year", year)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			if !ok {
				slog.Warn("fetch completed but data still missing, returning empty", "year", year)
				if writeErr := writeJSON(w, []byte("[]")); writeErr != nil {
					slog.Warn("write response failed", "error", writeErr)
				}
				return
			}
		}

		if season == "WINTER" {
			hasPriorYear, err := db.HasYearContext(r.Context(), year-1)
			if err != nil {
				slog.Error("prior year cache check failed", "error", err, "year", year-1)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			if !hasPriorYear {
				slog.Debug("winter overflow: prior year not cached, fetching in background",
					"prior_year", year-1,
				)
				go func(priorYear int) {
					fetchCtx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
					defer cancel()
					if err := sched.FetchAndStore(fetchCtx, priorYear, "winter_overflow"); err != nil {
						slog.Error("winter overflow backfill failed", "error", err)
					}
				}(year - 1)
			}
		}

		shows, err := sched.ProcessContext(r.Context(), data, season, year, category)
		if err != nil {
			slog.Error("processing failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		if !fresh {
			slog.Debug("serving stale data, refreshing in background",
				"season", season,
				"year", year,
				"category", category,
			)
			go func(refreshYear int) {
				fetchCtx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
				defer cancel()
				if err := sched.FetchAndStore(fetchCtx, refreshYear, "stale_refresh"); err != nil {
					slog.Error("stale refresh failed", "error", err)
				}
			}(year)
		}

		body, err := json.Marshal(shows)
		if err != nil {
			slog.Error("marshal result", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if err := writeJSON(w, body); err != nil {
			slog.Warn("write response failed", "error", err)
		}
	}
}

func handleHealth(db *cache.Cache, sched *scheduler.Scheduler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		healthy := true
		if err := db.PingContext(r.Context()); err != nil {
			slog.Error("health check failed", "error", err)
			healthy = false
		}
		resolverOK := sched.ResolverLoaded()
		switch {
		case healthy && resolverOK:
			if err := writeJSON(w, []byte(`{"status":"ok"}`)); err != nil {
				slog.Warn("write response failed", "error", err)
			}
		case healthy:
			if err := writeJSONStatus(w, http.StatusServiceUnavailable, []byte(`{"status":"degraded","reason":"resolver not loaded"}`)); err != nil {
				slog.Warn("write response failed", "error", err)
			}
		default:
			if err := writeJSONStatus(w, http.StatusServiceUnavailable, []byte(`{"status":"unhealthy"}`)); err != nil {
				slog.Warn("write response failed", "error", err)
			}
		}
	}
}

func handleCacheStats(db *cache.Cache, cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !authorizedDebugRequest(r, cfg) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		stats, err := db.StatsContext(r.Context())
		if err != nil {
			slog.Error("cache stats failed", "error", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		data, err := json.Marshal(stats)
		if err != nil {
			slog.Error("marshal cache stats", "error", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if err := writeJSON(w, data); err != nil {
			slog.Warn("write response failed", "error", err)
		}
	}
}

func handleCacheClear(db *cache.Cache, cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !authorizedDebugRequest(r, cfg) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}

		slog.Warn("clearing all cache entries")
		if err := db.ClearContext(r.Context()); err != nil {
			slog.Error("cache clear failed", "error", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		if err := writeJSON(w, []byte(`{"status":"ok"}`)); err != nil {
			slog.Warn("write response failed", "error", err)
		}
	}
}

func writeJSON(w http.ResponseWriter, data []byte) error {
	w.Header().Set("Content-Type", "application/json")
	_, err := w.Write(data)
	return err
}

func writeJSONStatus(w http.ResponseWriter, status int, data []byte) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, err := w.Write(data)
	return err
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
