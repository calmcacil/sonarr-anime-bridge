package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/calmcacil/sonarr-anime-bridge/internal/anilist"
	"github.com/calmcacil/sonarr-anime-bridge/internal/cache"
	"github.com/calmcacil/sonarr-anime-bridge/internal/config"
	"github.com/calmcacil/sonarr-anime-bridge/internal/mapping"
	"github.com/calmcacil/sonarr-anime-bridge/internal/scheduler"
	"github.com/calmcacil/sonarr-anime-bridge/internal/testutil"
	"github.com/klauspost/compress/zstd"
)

var listCfg = &config.Config{IncludeTypes: []string{"TV", "ONA"}}

func newTestCache(t *testing.T) *cache.Cache {
	t.Helper()
	c, err := cache.Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// newTestScheduler builds a scheduler backed by the fixture mapping file. The
// resolver is not loaded; callers opt in with LoadResolverContext.
func newTestScheduler(t *testing.T, c *cache.Cache, fetcher ...fakeFetcher) *scheduler.Scheduler {
	t.Helper()
	dir := t.TempDir()
	writeTestMappingFile(t, dir)
	cfg := &config.Config{
		IncludeTypes:         []string{"TV", "ONA"},
		AnibridgeMappingPath: filepath.Join(dir, "mappings.json.zst"),
		AnibridgeURL:         "http://127.0.0.1:1/nonexistent",
	}
	var f fakeFetcher
	if len(fetcher) > 0 {
		f = fetcher[0]
	}
	return scheduler.NewWithFetcher(c, cfg, f)
}

func newReadyScheduler(t *testing.T, c *cache.Cache, fetcher ...fakeFetcher) *scheduler.Scheduler {
	t.Helper()
	s := newTestScheduler(t, c, fetcher...)
	s.LoadResolverContext(context.Background())
	return s
}

type fakeFetcher struct {
	shows []anilist.Show
	err   error
}

func (f fakeFetcher) FetchYear(context.Context, int) ([]anilist.Show, error) {
	return f.shows, f.err
}

func serve(t *testing.T, h http.Handler, method, url string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, url, nil))
	return w
}

func decodeJSON[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("invalid JSON %q: %v", w.Body.String(), err)
	}
	return v
}

func seedYear(t *testing.T, c *cache.Cache, year int, data string) {
	t.Helper()
	if err := c.SetYearContext(context.Background(), year, []byte(data)); err != nil {
		t.Fatal(err)
	}
}

func recordAttrsString(record testutil.LogRecord) string {
	var builder strings.Builder
	for key, value := range record.Attrs {
		builder.WriteString(key)
		builder.WriteByte('=')
		builder.WriteString(value.String())
		builder.WriteByte(' ')
	}
	return builder.String()
}

func assertNoForbidden(t *testing.T, record testutil.LogRecord, forbidden ...string) {
	t.Helper()
	if _, ok := record.Attrs["path"]; ok {
		t.Error("completion event must not include path")
	}
	for _, value := range append(forbidden, "?season=") {
		if strings.Contains(recordAttrsString(record), value) {
			t.Errorf("completion log contains forbidden value %q: %s", value, recordAttrsString(record))
		}
	}
}

func writeTestMappingFile(t *testing.T, dir string) {
	t.Helper()
	fixture := `{ "mal:16498": { "tvdb_show:12345:s1": { "1-12": "1-12" } }, "anilist:42": { "tvdb_show:77777:s1": { "1": "1" } } }`

	f, err := os.Create(filepath.Join(dir, "mappings.json.zst"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w, err := zstd.NewWriter(f)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(fixture)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := mapping.WriteMetadata(filepath.Join(dir, "mappings.json.zst.meta.json"), mapping.Metadata{
		ETag: `"test-fixture"`,
		URL:  "http://127.0.0.1:1/nonexistent",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestLoggingMiddlewareListCompletion(t *testing.T) {
	c := newTestCache(t)
	s := newReadyScheduler(t, c)
	year := time.Now().Year()
	seedYear(t, c, year, `[]`)
	seedYear(t, c, year-1, `[]`) // prevents an async WINTER backfill from logging mid-capture
	mux := http.NewServeMux()
	mux.HandleFunc("/list", handleList(c, s, listCfg))

	logs := testutil.CaptureLogs(t, slog.LevelInfo)
	url := fmt.Sprintf("/list?season=WINTER&year=%d&category=series&title=secret&tvdb_id=9876", year)
	if w := serve(t, loggingMiddleware(mux), http.MethodGet, url); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	records := logs.Records()
	if len(records) != 1 {
		t.Fatalf("captured %d records, want one completion event", len(records))
	}
	record := records[0]
	if record.Level != slog.LevelInfo {
		t.Fatalf("completion level = %s, want INFO", record.Level)
	}
	attrs := record.Attrs
	for key, want := range map[string]string{
		"type": "http", "task": "request", "outcome": "succeeded", "method": "GET", "route": "/list", "cache_state": "hit", "season": "WINTER", "category": "series",
	} {
		if got := attrs[key].String(); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	for key, want := range map[string]int64{"year": int64(year), "status": http.StatusOK, "result_count": 0} {
		if got := attrs[key].Int64(); got != want {
			t.Errorf("%s = %d, want %d", key, got, want)
		}
	}
	if duration, ok := attrs["duration_ms"]; !ok || duration.Int64() < 0 {
		t.Errorf("duration_ms = %v, want a nonnegative value", duration)
	}
	assertNoForbidden(t, record, "title", "secret", "tvdb_id", "9876")
}

func TestLoggingMiddlewareCompletionLevels(t *testing.T) {
	healthMux := func(loaded bool) func(t *testing.T) http.Handler {
		return func(t *testing.T) http.Handler {
			c := newTestCache(t)
			s := newTestScheduler(t, c)
			if loaded {
				s.LoadResolverContext(context.Background())
			}
			seedYear(t, c, 2026, `[]`)
			mux := http.NewServeMux()
			mux.HandleFunc("/health", handleHealth(c, s, []int{2026}))
			return mux
		}
	}

	tests := []struct {
		name       string
		handler    func(t *testing.T) http.Handler
		url        string
		wantStatus int
		wantRoute  string // empty means no completion record is expected
		forbidden  []string
	}{
		{
			name: "failed request uses stable route",
			handler: func(*testing.T) http.Handler {
				mux := http.NewServeMux()
				mux.HandleFunc("/list", func(w http.ResponseWriter, _ *http.Request) {
					http.Error(w, "bad request", http.StatusBadRequest)
				})
				return mux
			},
			url:        "/list?season=INVALID&query=private&token=secret",
			wantStatus: http.StatusBadRequest,
			wantRoute:  "/list",
			forbidden:  []string{"INVALID", "private", "secret"},
		},
		{
			name:       "unmatched route is bounded",
			handler:    func(*testing.T) http.Handler { return http.NewServeMux() },
			url:        "/private/value?token=secret",
			wantStatus: http.StatusNotFound,
			wantRoute:  "unknown",
			forbidden:  []string{"private", "value", "token", "secret"},
		},
		{
			name:       "failed health is warning",
			handler:    healthMux(false),
			url:        "/health",
			wantStatus: http.StatusServiceUnavailable,
			wantRoute:  "/health",
		},
		{
			name:       "successful health is suppressed",
			handler:    healthMux(true),
			url:        "/health",
			wantStatus: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := loggingMiddleware(tt.handler(t))
			logs := testutil.CaptureLogs(t, slog.LevelInfo)
			if w := serve(t, h, http.MethodGet, tt.url); w.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", w.Code, tt.wantStatus)
			}
			records := logs.Records()
			if tt.wantRoute == "" {
				if len(records) != 0 {
					t.Fatalf("captured %d records, want none", len(records))
				}
				return
			}
			if len(records) != 1 {
				t.Fatalf("captured %d records, want one completion event", len(records))
			}
			record := records[0]
			if record.Level != slog.LevelWarn || record.Attrs["outcome"].String() != "failed" {
				t.Fatalf("completion = %v, want WARN failed", record)
			}
			if got := record.Attrs["route"].String(); got != tt.wantRoute {
				t.Errorf("route = %q, want %q", got, tt.wantRoute)
			}
			if got := record.Attrs["status"].Int64(); got != int64(tt.wantStatus) {
				t.Errorf("status attribute = %d, want %d", got, tt.wantStatus)
			}
			assertNoForbidden(t, record, tt.forbidden...)
		})
	}
}

type failingResponseWriter struct {
	header http.Header
}

func (w *failingResponseWriter) Header() http.Header { return w.header }
func (*failingResponseWriter) WriteHeader(int)       {}
func (*failingResponseWriter) Write([]byte) (int, error) {
	return 0, errors.New("connection closed")
}

func TestRequestCompletionDoesNotClaimSuccessAfterWriteOrPanic(t *testing.T) {
	for _, panicAfterWrite := range []bool{false, true} {
		t.Run(fmt.Sprintf("panic=%v", panicAfterWrite), func(t *testing.T) {
			logs := testutil.CaptureLogs(t, slog.LevelInfo)
			mux := http.NewServeMux()
			mux.HandleFunc("/list", func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusOK, []byte("[]"))
				if panicAfterWrite {
					panic("handler failed")
				}
			})
			var writer http.ResponseWriter = httptest.NewRecorder()
			if !panicAfterWrite {
				writer = &failingResponseWriter{header: make(http.Header)}
			}
			loggingMiddleware(recoveryMiddleware(mux)).ServeHTTP(writer, httptest.NewRequest(http.MethodGet, "/list?token=private", nil))
			completions := 0
			for _, record := range logs.Records() {
				assertNoForbidden(t, record, "token", "private")
				if _, ok := record.Attrs["status"]; !ok {
					continue
				}
				completions++
				if record.Level != slog.LevelWarn || record.Attrs["outcome"].String() != "failed" || record.Attrs["status"].Int64() != http.StatusOK {
					t.Errorf("completion = %v, want failed WARN with already-sent HTTP 200", record)
				}
			}
			if completions != 1 {
				t.Errorf("completion count = %d, want 1", completions)
			}
		})
	}
}

func TestValidateRuntimeDataDirs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		subdir    string
		cachePath func(dir string) string
		wantErr   string
	}{
		{name: "ok", cachePath: func(dir string) string { return filepath.Join(dir, "cache.db") }},
		{name: "memory cache still checks mapping", cachePath: func(string) string { return ":memory:" }},
		{
			name:      "missing dir",
			subdir:    "missing",
			cachePath: func(dir string) string { return filepath.Join(dir, "cache.db") },
			wantErr:   "CACHE_DB_PATH/MAPPING_PATH directory",
		},
		{
			name:      "memory cache with missing mapping dir",
			subdir:    "missing",
			cachePath: func(string) string { return ":memory:" },
			wantErr:   `MAPPING_PATH directory`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := filepath.Join(t.TempDir(), tt.subdir)
			err := validateRuntimeDataDirs(&config.Config{
				CacheDBPath:          tt.cachePath(dir),
				AnibridgeMappingPath: filepath.Join(dir, "mappings.json.zst"),
			})
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateRuntimeDataDirsReadOnlyDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write to read-only directories")
	}
	t.Parallel()

	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Logf("restore permissions: %v", err)
		}
	})

	err := validateRuntimeDataDirs(&config.Config{
		CacheDBPath:          filepath.Join(dir, "cache.db"),
		AnibridgeMappingPath: filepath.Join(dir, "mappings.json.zst"),
	})
	if err == nil || !strings.Contains(err.Error(), "must be readable and writable") {
		t.Fatalf("error = %v, want readable/writable error", err)
	}
}

func TestHandleHealth(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		years           []int
		cachedYears     []int
		resolverLoaded  bool
		closeCache      bool
		wantCode        int
		wantStatus      healthStatus
		wantCacheStatus healthStatus
		wantResolver    healthStatus
		wantReason      string
		wantBody        string
	}{
		{
			name:            "ready",
			years:           []int{2025, 2026},
			cachedYears:     []int{2025, 2026},
			resolverLoaded:  true,
			wantCode:        http.StatusOK,
			wantStatus:      healthStatusOK,
			wantCacheStatus: healthStatusOK,
			wantResolver:    healthStatusOK,
			wantBody:        `{"status":"ok","checks":{"cache":{"status":"ok"},"resolver":{"status":"ok"}}}`,
		},
		{
			name:            "warming",
			years:           []int{2026},
			resolverLoaded:  true,
			wantCode:        http.StatusOK,
			wantStatus:      healthStatusOK,
			wantCacheStatus: healthStatusWarming,
			wantResolver:    healthStatusOK,
		},
		{
			name:            "resolver degraded",
			years:           []int{2026},
			cachedYears:     []int{2026},
			wantCode:        http.StatusServiceUnavailable,
			wantStatus:      healthStatusDegraded,
			wantCacheStatus: healthStatusOK,
			wantResolver:    healthStatusDegraded,
			wantReason:      "resolver not loaded",
		},
		{
			name:            "cache failure",
			years:           []int{2026},
			resolverLoaded:  true,
			closeCache:      true,
			wantCode:        http.StatusServiceUnavailable,
			wantStatus:      healthStatusUnhealthy,
			wantCacheStatus: healthStatusUnhealthy,
			wantResolver:    healthStatusOK,
		},
		{
			name:            "empty prewarm years cache failure",
			years:           []int{},
			resolverLoaded:  true,
			closeCache:      true,
			wantCode:        http.StatusServiceUnavailable,
			wantStatus:      healthStatusUnhealthy,
			wantCacheStatus: healthStatusUnhealthy,
			wantResolver:    healthStatusOK,
		},
		{
			name:            "combined failure",
			years:           []int{2026},
			closeCache:      true,
			wantCode:        http.StatusServiceUnavailable,
			wantStatus:      healthStatusUnhealthy,
			wantCacheStatus: healthStatusUnhealthy,
			wantResolver:    healthStatusDegraded,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestCache(t)
			s := newTestScheduler(t, c)
			if tt.resolverLoaded {
				s.LoadResolverContext(context.Background())
			}
			for _, year := range tt.cachedYears {
				seedYear(t, c, year, `[]`)
			}
			if tt.closeCache {
				if err := c.Close(); err != nil {
					t.Fatal(err)
				}
			}

			w := serve(t, handleHealth(c, s, tt.years), http.MethodGet, "/health")
			if w.Code != tt.wantCode {
				t.Fatalf("expected %d, got %d", tt.wantCode, w.Code)
			}
			if tt.wantBody != "" && w.Body.String() != tt.wantBody {
				t.Errorf("body = %s, want %s", w.Body.String(), tt.wantBody)
			}
			response := decodeJSON[healthResponse](t, w)
			if response.Status != tt.wantStatus {
				t.Errorf("status = %q, want %q", response.Status, tt.wantStatus)
			}
			if response.Checks.Cache.Status != tt.wantCacheStatus {
				t.Errorf("cache status = %q, want %q", response.Checks.Cache.Status, tt.wantCacheStatus)
			}
			if response.Checks.Resolver.Status != tt.wantResolver {
				t.Errorf("resolver status = %q, want %q", response.Checks.Resolver.Status, tt.wantResolver)
			}
			if response.Reason != tt.wantReason {
				t.Errorf("reason = %q, want %q", response.Reason, tt.wantReason)
			}
		})
	}
}

func TestHandleEndpointMethodsAndAuth(t *testing.T) {
	t.Parallel()
	enabled := &config.Config{DebugEndpointsEnabled: true}
	tokenCfg := &config.Config{DebugEndpointsEnabled: true, AdminToken: "tok"}
	type handlerFor func(*cache.Cache, *scheduler.Scheduler) http.HandlerFunc
	health := func(c *cache.Cache, s *scheduler.Scheduler) http.HandlerFunc { return handleHealth(c, s, nil) }
	stats := func(cfg *config.Config) handlerFor {
		return func(c *cache.Cache, _ *scheduler.Scheduler) http.HandlerFunc { return handleCacheStats(c, cfg) }
	}
	clearH := func(cfg *config.Config) handlerFor {
		return func(c *cache.Cache, _ *scheduler.Scheduler) http.HandlerFunc { return handleCacheClear(c, cfg) }
	}

	tests := []struct {
		name        string
		handler     handlerFor
		method      string
		auth        string
		wantCode    int
		wantAllow   string
		wantEntries int // checked when >= 0
	}{
		{name: "POST /health", handler: health, method: http.MethodPost, wantCode: http.StatusMethodNotAllowed, wantAllow: "GET, HEAD", wantEntries: -1},
		{name: "HEAD /health", handler: health, method: http.MethodHead, wantCode: http.StatusServiceUnavailable, wantEntries: -1},
		{name: "POST /cache/stats", handler: stats(enabled), method: http.MethodPost, wantCode: http.StatusMethodNotAllowed, wantAllow: "GET, HEAD", wantEntries: -1},
		{name: "GET /cache/stats", handler: stats(enabled), method: http.MethodGet, wantCode: http.StatusOK, wantEntries: 1},
		{name: "GET /cache/stats disabled", handler: stats(&config.Config{}), method: http.MethodGet, wantCode: http.StatusNotFound, wantEntries: -1},
		{name: "GET /cache/stats wrong token", handler: stats(tokenCfg), method: http.MethodGet, auth: "Bearer nope", wantCode: http.StatusNotFound, wantEntries: -1},
		{name: "GET /cache/stats token", handler: stats(tokenCfg), method: http.MethodGet, auth: "Bearer tok", wantCode: http.StatusOK, wantEntries: 1},
		{name: "GET /cache/clear", handler: clearH(enabled), method: http.MethodGet, wantCode: http.StatusMethodNotAllowed, wantAllow: "POST", wantEntries: 1},
		{name: "POST /cache/clear", handler: clearH(enabled), method: http.MethodPost, wantCode: http.StatusOK, wantEntries: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newTestCache(t)
			seedYear(t, c, 2026, `[]`)
			req := httptest.NewRequest(tt.method, "/", nil)
			if tt.auth != "" {
				req.Header.Set("Authorization", tt.auth)
			}
			w := httptest.NewRecorder()
			tt.handler(c, newTestScheduler(t, c))(w, req)
			if w.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d", w.Code, tt.wantCode)
			}
			if got := w.Header().Get("Allow"); got != tt.wantAllow {
				t.Errorf("Allow = %q, want %q", got, tt.wantAllow)
			}
			if tt.method == http.MethodGet && tt.wantCode == http.StatusOK {
				if got := decodeJSON[cache.CacheStats](t, w).Entries; got != tt.wantEntries {
					t.Errorf("stats entries = %d, want %d", got, tt.wantEntries)
				}
			}
			if tt.wantEntries >= 0 {
				stats, err := c.StatsContext(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if stats.Entries != tt.wantEntries {
					t.Errorf("cache entries = %d, want %d", stats.Entries, tt.wantEntries)
				}
			}
		})
	}
}

func TestHandleList_BadRequests(t *testing.T) {
	t.Parallel()
	c := newTestCache(t)
	ready := newReadyScheduler(t, c)
	unloaded := newTestScheduler(t, c)
	now := time.Now().Year()

	tests := []struct {
		name     string
		url      string
		unloaded bool
		wantCode int
		wantBody string
	}{
		{"invalid season", "/list?season=INVALID&year=2026", false, http.StatusBadRequest, "invalid season parameter"},
		{"invalid season before resolver check", "/list?season=INVALID", true, http.StatusBadRequest, "invalid season parameter"},
		{"resolver not loaded", "/list?season=WINTER&year=2026", true, http.StatusServiceUnavailable, "resolver not loaded"},
		{"year too far past", fmt.Sprintf("/list?season=WINTER&year=%d", now-11), false, http.StatusBadRequest, "out of range"},
		{"year too far future", fmt.Sprintf("/list?season=WINTER&year=%d", now+11), false, http.StatusBadRequest, "out of range"},
		{"non-numeric year", "/list?season=WINTER&year=abc", false, http.StatusBadRequest, "invalid year parameter"},
		{"zero year", "/list?season=WINTER&year=0", false, http.StatusBadRequest, "invalid year parameter"},
		{"negative year", "/list?season=WINTER&year=-1", false, http.StatusBadRequest, "invalid year parameter"},
		{"invalid category", "/list?season=WINTER&year=2026&category=invalid", false, http.StatusBadRequest, "invalid category"},
		{"method not allowed", "/list", false, http.StatusMethodNotAllowed, "method not allowed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := ready
			if tt.unloaded {
				s = unloaded
			}
			method := http.MethodGet
			if tt.wantCode == http.StatusMethodNotAllowed {
				method = http.MethodPost
			}
			w := serve(t, handleList(c, s, listCfg), method, tt.url)
			if w.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d: %s", w.Code, tt.wantCode, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tt.wantBody) {
				t.Errorf("body = %q, want containing %q", w.Body.String(), tt.wantBody)
			}
		})
	}
}

// TestHandleList_Results covers cache hit, cache miss, invalid cached payload
// recovery, and fetch failures. Not parallel: rows capture the default logger.
func TestHandleList_Results(t *testing.T) {
	year := time.Now().Year()
	recoveredTitle := "Recovered"
	recovered := anilist.Show{ID: 42, Title: anilist.Title{English: &recoveredTitle}, Format: "TV"}
	fetchErr := fakeFetcher{err: errors.New("fetch failed")}
	hitData := fmt.Sprintf(`[{"id":1,"idMal":16498,"title":{"english":"Test Show"},"format":"TV","startDate":{"year":%d,"month":1},"tags":[],"episodes":12,"duration":24,"status":"FINISHED"}]`, year)

	tests := []struct {
		name        string
		fetcher     fakeFetcher
		seed        string // cached payload for the current year; empty means none
		url         string
		wantTVDB    []int
		wantTrigger string // failed fetch origin; empty means successful processing
		wantValid   bool   // cached payload must be valid afterwards
	}{
		{name: "default params", url: "/list"},
		{name: "cache miss", url: fmt.Sprintf("/list?season=WINTER&year=%d", year)},
		// A failing fetcher proves a fresh hit is served without fetching.
		{name: "cache hit", fetcher: fetchErr, seed: hitData, url: fmt.Sprintf("/list?season=WINTER&year=%d", year), wantTVDB: []int{12345}},
		{
			name:        "cache miss fetch failure",
			fetcher:     fetchErr,
			url:         fmt.Sprintf("/list?season=FALL&year=%d&category=series-new", year),
			wantTrigger: "cache_miss",
		},
		{
			name:      "invalid payload recovered",
			fetcher:   fakeFetcher{shows: []anilist.Show{recovered}},
			seed:      `not-json`,
			url:       "/list?season=ALL",
			wantTVDB:  []int{77777},
			wantValid: true,
		},
		{
			name:        "invalid payload fetch failure",
			fetcher:     fetchErr,
			seed:        `not-json`,
			url:         fmt.Sprintf("/list?season=FALL&year=%d&category=series-new", year),
			wantTrigger: "cache_recovery",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestCache(t)
			s := newReadyScheduler(t, c, tt.fetcher)
			seedYear(t, c, year-1, `[]`) // Keep prior-year work outside this request-outcome regression.
			if tt.seed != "" {
				seedYear(t, c, year, tt.seed)
			}
			logs := testutil.CaptureLogs(t, slog.LevelDebug)

			mux := http.NewServeMux()
			mux.HandleFunc("/list", handleList(c, s, listCfg))
			w := serve(t, loggingMiddleware(mux), http.MethodGet, tt.url)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
			}
			var got []int
			for _, show := range decodeJSON[[]scheduler.Show](t, w) {
				got = append(got, show.TVDBID)
			}
			if !slices.Equal(got, tt.wantTVDB) {
				t.Errorf("tvdbIds = %v, want %v (body %s)", got, tt.wantTVDB, w.Body.String())
			}

			var completion testutil.LogRecord
			failures := 0
			for _, record := range logs.Records() {
				if record.Attrs["task"].String() == "request" {
					completion = record
				}
				if record.Attrs["task"].String() == "year_fetch" && record.Attrs["outcome"].String() == "failed" {
					failures++
					if record.Attrs["trigger"].String() != tt.wantTrigger {
						t.Errorf("fetch trigger = %s, want %s", record.Attrs["trigger"], tt.wantTrigger)
					}
				}
			}
			if tt.wantTrigger != "" {
				if failures != 1 || completion.Level != slog.LevelWarn || completion.Attrs["outcome"].String() != "degraded" || completion.Attrs["reason"].String() != "fetch_failed" {
					t.Errorf("failed fetch logs: failures=%d completion=%v", failures, completion)
				}
			} else if failures != 0 || completion.Attrs["outcome"].String() != "succeeded" {
				t.Errorf("successful list logs: failures=%d completion=%v", failures, completion)
			}

			if tt.wantValid {
				data, _, ok, err := c.PeekYearContext(context.Background(), year)
				if err != nil || !ok {
					t.Fatalf("recovered cache entry: ok=%v err=%v", ok, err)
				}
				if err := scheduler.ValidateYearData(data); err != nil {
					t.Fatalf("recovered cache payload is invalid: %v", err)
				}
			}
		})
	}
}
