package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func TestHandleHealth_OK(t *testing.T) {
	t.Parallel()
	c := newTestCache(t)
	s := newTestScheduler(t, c)

	s.LoadResolver()

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()

	handleHealth(c, s)(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if resp["status"] != "ok" {
		t.Errorf("expected status ok, got %q", resp["status"])
	}
}

func TestHandleHealth_Degraded(t *testing.T) {
	t.Parallel()
	c := newTestCache(t)
	s := newTestScheduler(t, c)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()

	handleHealth(c, s)(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}

	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if resp["status"] != "degraded" {
		t.Errorf("expected status degraded, got %q", resp["status"])
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
	handleHealth(c, s)(w, httptest.NewRequest(http.MethodPost, "/health", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /health: expected 405, got %d", w.Code)
	}

	w = httptest.NewRecorder()
	handleCacheStats(c, cfg)(w, httptest.NewRequest(http.MethodPost, "/cache/stats", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /cache/stats: expected 405, got %d", w.Code)
	}

	if err := c.SetYear(2026, []byte(`[]`)); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	handleCacheClear(c, cfg)(w, httptest.NewRequest(http.MethodGet, "/cache/clear", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /cache/clear: expected 405, got %d", w.Code)
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
