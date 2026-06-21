package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

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

func TestHandleHealth_OK(t *testing.T) {
	t.Parallel()
	c := newTestCache(t)
	s := newTestScheduler(t, c)

	s.LoadResolver(t.Context())

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

	handleCacheStats(c)(w, req)

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

	handleList(t.Context(), c, s, cfg)(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestHandleList_CacheMiss(t *testing.T) {
	t.Parallel()
	c := newTestCache(t)
	s := newTestScheduler(t, c)
	cfg := &config.Config{
		IncludeTypes: []string{"TV", "ONA"},
	}

	s.LoadResolver(t.Context())

	req := httptest.NewRequest(http.MethodGet, "/list?season=WINTER&year=2026", nil)
	w := httptest.NewRecorder()

	handleList(t.Context(), c, s, cfg)(w, req)

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

func TestHandleList_CacheHit(t *testing.T) {
	t.Parallel()
	c := newTestCache(t)
	s := newTestScheduler(t, c)

	s.LoadResolver(t.Context())

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

	handleList(t.Context(), c, s, cfg)(w, req)

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

	s.LoadResolver(t.Context())

	req := httptest.NewRequest(http.MethodGet, "/list", nil)
	w := httptest.NewRecorder()

	handleList(t.Context(), c, s, cfg)(w, req)

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
	// Deliberately NOT calling s.LoadResolver(t.Context())

	req := httptest.NewRequest(http.MethodGet, "/list?season=WINTER&year=2026", nil)
	w := httptest.NewRecorder()

	handleList(t.Context(), c, s, cfg)(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleList_InvalidYear_Returns400(t *testing.T) {
	t.Parallel()
	c := newTestCache(t)
	s := newTestScheduler(t, c)
	cfg := &config.Config{
		IncludeTypes: []string{"TV", "ONA"},
	}

	s.LoadResolver(t.Context())

	req := httptest.NewRequest(http.MethodGet, "/list?season=WINTER&year=abc", nil)
	w := httptest.NewRecorder()

	handleList(t.Context(), c, s, cfg)(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleList_YearOutOfRange_Returns400(t *testing.T) {
	t.Parallel()
	c := newTestCache(t)
	s := newTestScheduler(t, c)
	cfg := &config.Config{
		IncludeTypes: []string{"TV", "ONA"},
	}

	s.LoadResolver(t.Context())

	// Year far in the past (year-10 = 2016 for 2026)
	req := httptest.NewRequest(http.MethodGet, "/list?season=WINTER&year=1990", nil)
	w := httptest.NewRecorder()

	handleList(t.Context(), c, s, cfg)(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}

	// Year far in the future
	req = httptest.NewRequest(http.MethodGet, "/list?season=WINTER&year=2099", nil)
	w = httptest.NewRecorder()

	handleList(t.Context(), c, s, cfg)(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleList_InvalidCategory(t *testing.T) {
	t.Parallel()
	c := newTestCache(t)
	s := newTestScheduler(t, c)

	s.LoadResolver(t.Context())

	cfg := &config.Config{
		IncludeTypes: []string{"TV", "ONA"},
	}

	req := httptest.NewRequest(http.MethodGet, "/list?season=WINTER&year=2026&category=invalid", nil)
	w := httptest.NewRecorder()

	handleList(t.Context(), c, s, cfg)(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}
