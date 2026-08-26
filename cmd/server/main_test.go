package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/calmcacil/sonarr-anime-bridge/internal/anilist"
	"github.com/calmcacil/sonarr-anime-bridge/internal/cache"
	"github.com/calmcacil/sonarr-anime-bridge/internal/config"
	"github.com/calmcacil/sonarr-anime-bridge/internal/mapping"
	"github.com/calmcacil/sonarr-anime-bridge/internal/scheduler"
	"github.com/klauspost/compress/zstd"
)

func newTestCache(t *testing.T) *cache.Cache {
	t.Helper()
	f, err := os.CreateTemp("", "cache-test-*.db")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })

	c, err := cache.Open(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func newTestScheduler(t *testing.T, c *cache.Cache) *scheduler.Scheduler {
	t.Helper()
	dir := t.TempDir()

	writeTestMappingFile(t, dir)

	cfg := &config.Config{
		IncludeTypes:         []string{"TV", "ONA"},
		AnibridgeMappingPath: filepath.Join(dir, "mappings.json.zst"),
		AnibridgeURL:         "http://127.0.0.1:1/nonexistent",
	}
	return scheduler.New(c, cfg)
}

type fakeFetcher struct {
	shows []anilist.Show
	err   error
}

func (f fakeFetcher) FetchYear(context.Context, int) ([]anilist.Show, error) {
	return f.shows, f.err
}

type capturedLogHandler struct {
	records []slog.Record
	attrs   []slog.Attr
}

func (h *capturedLogHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelInfo
}

func (h *capturedLogHandler) Handle(_ context.Context, record slog.Record) error {
	record.AddAttrs(h.attrs...)
	h.records = append(h.records, record.Clone())
	return nil
}

func (h *capturedLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	h.attrs = append(h.attrs, attrs...)
	return h
}

func (h *capturedLogHandler) WithGroup(string) slog.Handler { return h }

func capturedAttrs(record slog.Record) map[string]slog.Value {
	attrs := make(map[string]slog.Value)
	record.Attrs(func(attr slog.Attr) bool {
		attrs[attr.Key] = attr.Value
		return true
	})
	return attrs
}

func installCapturedLogs(t *testing.T) *capturedLogHandler {
	t.Helper()
	logs := &capturedLogHandler{}
	previous := slog.Default()
	slog.SetDefault(slog.New(logs))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return logs
}

func TestLoggingMiddlewareListCompletion(t *testing.T) {
	c := newTestCache(t)
	s := newTestScheduler(t, c)
	s.LoadResolver()
	year := time.Now().Year()
	if err := c.SetYear(year, []byte(`[]`)); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/list", handleList(c, s, &config.Config{IncludeTypes: []string{"TV", "ONA"}}))
	logs := installCapturedLogs(t)
	request := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/list?season=WINTER&year=%d&category=series&title=secret&tvdb_id=9876", year), nil)
	response := httptest.NewRecorder()
	loggingMiddleware(mux).ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	if len(logs.records) != 1 {
		t.Fatalf("captured %d records, want one completion event", len(logs.records))
	}
	record := logs.records[0]
	if record.Message != "request completed" || record.Level != slog.LevelInfo {
		t.Fatalf("completion = (%s, %s), want (request completed, INFO)", record.Message, record.Level)
	}
	attrs := capturedAttrs(record)
	for key, want := range map[string]string{
		"type": "http", "method": "GET", "route": "/list", "cache_state": "hit", "season": "WINTER", "category": "series",
	} {
		if got := attrs[key].String(); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if got := attrs["year"].Int64(); got != int64(year) {
		t.Errorf("year = %d, want %d", got, year)
	}
	if got := attrs["status"].Int64(); got != http.StatusOK {
		t.Errorf("status attribute = %d, want 200", got)
	}
	if got := attrs["result_count"].Int64(); got != 0 {
		t.Errorf("result_count = %d, want 0", got)
	}
	if duration, ok := attrs["duration_ms"]; !ok || duration.Int64() < 0 {
		t.Errorf("duration_ms = %v, want a nonnegative value", duration)
	}
	for _, forbidden := range []string{"title", "secret", "tvdb_id", "9876", "?season="} {
		if strings.Contains(recordAttrsString(record), forbidden) {
			t.Errorf("completion log contains forbidden value %q: %s", forbidden, recordAttrsString(record))
		}
	}
}

func TestLoggingMiddlewareFailedRequestUsesStableRouteAndWarn(t *testing.T) {
	logs := installCapturedLogs(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/list", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad request", http.StatusBadRequest)
	})
	request := httptest.NewRequest(http.MethodGet, "/list?season=INVALID&query=private&token=secret", nil)
	loggingMiddleware(mux).ServeHTTP(httptest.NewRecorder(), request)

	if len(logs.records) != 1 {
		t.Fatalf("captured %d records, want one completion event", len(logs.records))
	}
	record := logs.records[0]
	if record.Message != "request completed" || record.Level != slog.LevelWarn {
		t.Fatalf("completion = (%s, %s), want (request completed, WARN)", record.Message, record.Level)
	}
	attrs := capturedAttrs(record)
	if got := attrs["route"].String(); got != "/list" {
		t.Fatalf("route = %q, want /list", got)
	}
	if _, ok := attrs["path"]; ok {
		t.Fatal("completion event must not include path")
	}
	if got := attrs["status"].Int64(); got != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", got)
	}
	for _, forbidden := range []string{"INVALID", "private", "secret", "?season="} {
		if strings.Contains(recordAttrsString(record), forbidden) {
			t.Errorf("completion log contains forbidden value %q: %s", forbidden, recordAttrsString(record))
		}
	}
}

func TestLoggingMiddlewareUnmatchedRouteIsBounded(t *testing.T) {
	logs := installCapturedLogs(t)
	request := httptest.NewRequest(http.MethodGet, "/private/value?token=secret", nil)
	response := httptest.NewRecorder()

	loggingMiddleware(http.NewServeMux()).ServeHTTP(response, request)

	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.Code)
	}
	if len(logs.records) != 1 {
		t.Fatalf("captured %d records, want one completion event", len(logs.records))
	}
	record := logs.records[0]
	attrs := capturedAttrs(record)
	if got := attrs["route"].String(); got != "unknown" {
		t.Fatalf("route = %q, want unknown", got)
	}
	for _, forbidden := range []string{"private", "value", "token", "secret"} {
		if strings.Contains(recordAttrsString(record), forbidden) {
			t.Errorf("completion log contains forbidden value %q: %s", forbidden, recordAttrsString(record))
		}
	}
}

func recordAttrsString(record slog.Record) string {
	var builder strings.Builder
	record.Attrs(func(attr slog.Attr) bool {
		builder.WriteString(attr.Key)
		builder.WriteByte('=')
		builder.WriteString(attr.Value.String())
		builder.WriteByte(' ')
		return true
	})
	return builder.String()
}

func TestLoggingMiddlewareHealthLevels(t *testing.T) {
	t.Run("successful health is suppressed", func(t *testing.T) {
		c := newTestCache(t)
		s := newTestScheduler(t, c)
		s.LoadResolver()
		if err := c.SetYear(2026, []byte(`[]`)); err != nil {
			t.Fatal(err)
		}
		mux := http.NewServeMux()
		mux.HandleFunc("/health", handleHealth(c, s, []int{2026}))
		logs := installCapturedLogs(t)
		loggingMiddleware(mux).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/health", nil))
		if len(logs.records) != 0 {
			t.Fatalf("captured %d records for successful health, want none", len(logs.records))
		}
	})

	t.Run("failed health is warning", func(t *testing.T) {
		c := newTestCache(t)
		s := newTestScheduler(t, c)
		mux := http.NewServeMux()
		mux.HandleFunc("/health", handleHealth(c, s, []int{2026}))
		logs := installCapturedLogs(t)
		response := httptest.NewRecorder()
		loggingMiddleware(mux).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health", nil))
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", response.Code)
		}
		if len(logs.records) != 1 {
			t.Fatalf("captured %d records, want one completion event", len(logs.records))
		}
		record := logs.records[0]
		if record.Message != "request completed" || record.Level != slog.LevelWarn {
			t.Fatalf("completion = (%s, %s), want (request completed, WARN)", record.Message, record.Level)
		}
		attrs := capturedAttrs(record)
		if got := attrs["route"].String(); got != "/health" {
			t.Fatalf("route = %q, want /health", got)
		}
		if got := attrs["status"].Int64(); got != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", got)
		}
	})
}

func writeTestMappingFile(t *testing.T, dir string) {
	t.Helper()
	fixture := `{ "mal:16498": { "tvdb_show:12345:s1": { "1-12": "1-12" } }, "anilist:42": { "tvdb_show:77777:s1": { "1": "1" } } }`

	path := filepath.Join(dir, "mappings.json.zst")
	f, err := os.Create(path)
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

func TestValidateRuntimeDataDirsOK(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := &config.Config{
		CacheDBPath:          filepath.Join(dir, "cache.db"),
		AnibridgeMappingPath: filepath.Join(dir, "mappings.json.zst"),
	}

	if err := validateRuntimeDataDirs(cfg); err != nil {
		t.Fatalf("expected valid data dirs, got %v", err)
	}
}

func TestValidateRuntimeDataDirsMissingDir(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "missing")
	cfg := &config.Config{
		CacheDBPath:          filepath.Join(dir, "cache.db"),
		AnibridgeMappingPath: filepath.Join(dir, "mappings.json.zst"),
	}

	err := validateRuntimeDataDirs(cfg)
	if err == nil {
		t.Fatal("expected missing directory error")
	}
	if !strings.Contains(err.Error(), "CACHE_DB_PATH/MAPPING_PATH directory") {
		t.Fatalf("expected cache and mapping path context, got %v", err)
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

	cfg := &config.Config{
		CacheDBPath:          filepath.Join(dir, "cache.db"),
		AnibridgeMappingPath: filepath.Join(dir, "mappings.json.zst"),
	}

	err := validateRuntimeDataDirs(cfg)
	if err == nil {
		t.Fatal("expected read-only directory error")
	}
	if !strings.Contains(err.Error(), "must be readable and writable") {
		t.Fatalf("expected readable/writable error, got %v", err)
	}
}

func TestValidateRuntimeDataDirsMemoryCacheStillChecksMapping(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := &config.Config{
		CacheDBPath:          ":memory:",
		AnibridgeMappingPath: filepath.Join(dir, "mappings.json.zst"),
	}

	if err := validateRuntimeDataDirs(cfg); err != nil {
		t.Fatalf("expected mapping dir to validate with memory cache, got %v", err)
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
				s.LoadResolver()
			}
			for _, year := range tt.cachedYears {
				if err := c.SetYear(year, []byte(`[]`)); err != nil {
					t.Fatal(err)
				}
			}
			if tt.closeCache {
				if err := c.Close(); err != nil {
					t.Fatal(err)
				}
			}

			req := httptest.NewRequest(http.MethodGet, "/health", nil)
			w := httptest.NewRecorder()
			handleHealth(c, s, tt.years)(w, req)

			if w.Code != tt.wantCode {
				t.Fatalf("expected %d, got %d", tt.wantCode, w.Code)
			}
			var response healthResponse
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatalf("invalid JSON: %v", err)
			}
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

func TestHandleHealthSafeJSONShape(t *testing.T) {
	t.Parallel()
	c := newTestCache(t)
	s := newTestScheduler(t, c)
	s.LoadResolver()
	if err := c.SetYear(2026, []byte(`[]`)); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	handleHealth(c, s, []int{2026})(w, httptest.NewRequest(http.MethodGet, "/health", nil))
	want := `{"status":"ok","checks":{"cache":{"status":"ok"},"resolver":{"status":"ok"}}}`
	if got := w.Body.String(); got != want {
		t.Fatalf("health JSON = %s, want %s", got, want)
	}
}

func TestHandleHealthHEADServerSuppressesBody(t *testing.T) {
	c := newTestCache(t)
	s := newTestScheduler(t, c)
	s.LoadResolver()
	if err := c.SetYear(2026, []byte(`[]`)); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", handleHealth(c, s, []int{2026}))
	server := httptest.NewServer(mux)
	defer server.Close()

	getResp, err := http.Get(server.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer getResp.Body.Close()
	headReq, err := http.NewRequest(http.MethodHead, server.URL+"/health", nil)
	if err != nil {
		t.Fatal(err)
	}
	headResp, err := http.DefaultClient.Do(headReq)
	if err != nil {
		t.Fatal(err)
	}
	defer headResp.Body.Close()
	if headResp.StatusCode != getResp.StatusCode {
		t.Fatalf("HEAD status = %d, GET status = %d", headResp.StatusCode, getResp.StatusCode)
	}
	if headResp.Header.Get("Content-Type") != getResp.Header.Get("Content-Type") {
		t.Fatalf("HEAD Content-Type = %q, GET Content-Type = %q", headResp.Header.Get("Content-Type"), getResp.Header.Get("Content-Type"))
	}
	body, err := io.ReadAll(headResp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != 0 {
		t.Fatalf("HEAD response body = %q, want empty", body)
	}
}

func TestHandleCacheStats(t *testing.T) {
	t.Parallel()
	c := newTestCache(t)

	req := httptest.NewRequest(http.MethodGet, "/cache/stats", nil)
	w := httptest.NewRecorder()

	handleCacheStats(c, &config.Config{DebugEndpointsEnabled: true})(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var stats cache.CacheStats
	if err := json.Unmarshal(w.Body.Bytes(), &stats); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if stats.Entries != 0 {
		t.Errorf("expected 0 entries, got %d", stats.Entries)
	}
}

func TestHandleList_InvalidSeason(t *testing.T) {
	t.Parallel()
	c := newTestCache(t)
	s := newTestScheduler(t, c)
	cfg := &config.Config{
		IncludeTypes: []string{"TV", "ONA"},
	}

	req := httptest.NewRequest(http.MethodGet, "/list?season=INVALID&year=2026", nil)
	w := httptest.NewRecorder()

	handleList(c, s, cfg)(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestHandleList_CacheMiss(t *testing.T) {
	t.Parallel()
	c := newTestCache(t)
	dir := t.TempDir()
	writeTestMappingFile(t, dir)
	cfg := &config.Config{
		IncludeTypes:         []string{"TV", "ONA"},
		AnibridgeMappingPath: filepath.Join(dir, "mappings.json.zst"),
		AnibridgeURL:         "http://127.0.0.1:1/nonexistent",
	}
	s := scheduler.NewWithFetcher(c, cfg, fakeFetcher{})
	s.LoadResolver()

	req := httptest.NewRequest(http.MethodGet, "/list?season=WINTER&year=2026", nil)
	w := httptest.NewRecorder()

	handleList(c, s, cfg)(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var shows []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &shows); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(shows) != 0 {
		t.Errorf("expected empty list on cache miss, got %d shows", len(shows))
	}
}

func TestHandleList_CacheMissFetchFailureLogsContext(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() {
		slog.SetDefault(old)
	})

	c := newTestCache(t)
	dir := t.TempDir()
	writeTestMappingFile(t, dir)
	cfg := &config.Config{
		IncludeTypes:         []string{"TV", "ONA"},
		AnibridgeMappingPath: filepath.Join(dir, "mappings.json.zst"),
		AnibridgeURL:         "http://127.0.0.1:1/nonexistent",
	}
	s := scheduler.NewWithFetcher(c, cfg, fakeFetcher{err: errors.New("fetch failed")})
	s.LoadResolver()

	year := time.Now().Year()
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/list?season=FALL&year=%d&category=series-new", year), nil)
	w := httptest.NewRecorder()

	handleList(c, s, cfg)(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if got := strings.TrimSpace(w.Body.String()); got != "[]" {
		t.Fatalf("body = %q, want []", got)
	}

	out := logs.String()
	for _, want := range []string{
		"msg=\"trigger backfill failed\"",
		fmt.Sprintf("year=%d", year),
		"season=FALL",
		"category=series-new",
		"trigger=cache_miss",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("log missing %q:\n%s", want, out)
		}
	}
}

func TestHandleList_CacheHit(t *testing.T) {
	t.Parallel()
	c := newTestCache(t)
	s := newTestScheduler(t, c)

	s.LoadResolver()

	cfg := &config.Config{
		IncludeTypes: []string{"TV", "ONA"},
	}

	yearlyData := []byte(`[
		{"id":1,"idMal":16498,"title":{"english":"Test Show"},"format":"TV","startDate":{"year":2026,"month":1},"tags":[],"episodes":12,"duration":24,"status":"FINISHED"}
	]`)
	if err := c.SetYear(2026, yearlyData); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/list?season=WINTER&year=2026", nil)
	w := httptest.NewRecorder()

	handleList(c, s, cfg)(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var shows []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &shows); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(shows) > 0 {
		t.Logf("got %d shows (resolved via anibridge mapping)", len(shows))
	}
}

func TestHandleList_DefaultParams(t *testing.T) {
	t.Parallel()
	c := newTestCache(t)
	s := newTestScheduler(t, c)
	cfg := &config.Config{
		IncludeTypes: []string{"TV", "ONA"},
	}

	s.LoadResolver()

	req := httptest.NewRequest(http.MethodGet, "/list", nil)
	w := httptest.NewRecorder()

	handleList(c, s, cfg)(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestHandleList_ResolverNotLoaded_Returns503(t *testing.T) {
	t.Parallel()
	c := newTestCache(t)
	s := newTestScheduler(t, c)
	cfg := &config.Config{
		IncludeTypes: []string{"TV", "ONA"},
	}
	// Deliberately NOT calling s.LoadResolver()

	req := httptest.NewRequest(http.MethodGet, "/list?season=WINTER&year=2026", nil)
	w := httptest.NewRecorder()

	handleList(c, s, cfg)(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleList_YearOutOfRange_Returns400(t *testing.T) {
	t.Parallel()
	c := newTestCache(t)
	s := newTestScheduler(t, c)
	cfg := &config.Config{
		IncludeTypes: []string{"TV", "ONA"},
	}

	s.LoadResolver()

	// Year far in the past (year-10 = 2016 for 2026)
	req := httptest.NewRequest(http.MethodGet, "/list?season=WINTER&year=1990", nil)
	w := httptest.NewRecorder()

	handleList(c, s, cfg)(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}

	// Year far in the future
	req = httptest.NewRequest(http.MethodGet, "/list?season=WINTER&year=2099", nil)
	w = httptest.NewRecorder()

	handleList(c, s, cfg)(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleList_InvalidYearValues(t *testing.T) {
	t.Parallel()
	c := newTestCache(t)
	s := newTestScheduler(t, c)
	s.LoadResolver()
	cfg := &config.Config{IncludeTypes: []string{"TV", "ONA"}}

	for _, rawYear := range []string{"abc", "0", "-1"} {
		t.Run(rawYear, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/list?season=WINTER&year="+rawYear, nil)
			w := httptest.NewRecorder()

			handleList(c, s, cfg)(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestHandleDebugEndpointMethods(t *testing.T) {
	t.Parallel()
	c := newTestCache(t)
	s := newTestScheduler(t, c)
	cfg := &config.Config{DebugEndpointsEnabled: true}

	w := httptest.NewRecorder()
	handleHealth(c, s, nil)(w, httptest.NewRequest(http.MethodPost, "/health", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /health: expected 405, got %d", w.Code)
	}
	if got := w.Header().Get("Allow"); got != "GET, HEAD" {
		t.Fatalf("POST /health: Allow = %q, want GET, HEAD", got)
	}

	w = httptest.NewRecorder()
	handleCacheStats(c, cfg)(w, httptest.NewRequest(http.MethodPost, "/cache/stats", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /cache/stats: expected 405, got %d", w.Code)
	}
	if got := w.Header().Get("Allow"); got != "GET, HEAD" {
		t.Fatalf("POST /cache/stats: Allow = %q, want GET, HEAD", got)
	}

	if err := c.SetYear(2026, []byte(`[]`)); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	handleCacheClear(c, cfg)(w, httptest.NewRequest(http.MethodGet, "/cache/clear", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /cache/clear: expected 405, got %d", w.Code)
	}
	if got := w.Header().Get("Allow"); got != "POST" {
		t.Fatalf("GET /cache/clear: Allow = %q, want POST", got)
	}

	w = httptest.NewRecorder()
	handleCacheClear(c, cfg)(w, httptest.NewRequest(http.MethodPost, "/cache/clear", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("POST /cache/clear: expected 200, got %d", w.Code)
	}
	if stats := c.Stats(); stats.Entries != 0 {
		t.Fatalf("expected cache clear to remove entries, got %d", stats.Entries)
	}
}

func TestHandleList_InvalidCategory(t *testing.T) {
	t.Parallel()
	c := newTestCache(t)
	s := newTestScheduler(t, c)

	s.LoadResolver()

	cfg := &config.Config{
		IncludeTypes: []string{"TV", "ONA"},
	}

	req := httptest.NewRequest(http.MethodGet, "/list?season=WINTER&year=2026&category=invalid", nil)
	w := httptest.NewRecorder()

	handleList(c, s, cfg)(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}
