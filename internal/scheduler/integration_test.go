package scheduler

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/calmcacil/sonarr-anime-bridge/internal/cache"
	"github.com/calmcacil/sonarr-anime-bridge/internal/config"
)

func skipUnlessIntegration(t *testing.T) {
	t.Helper()
	if os.Getenv("INTEGRATION") != "1" {
		t.Skip("set INTEGRATION=1 to run integration tests")
	}
}

func TestIntegration_DataPipeline(t *testing.T) {
	skipUnlessIntegration(t)

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "cache.db")

	cfg := &config.Config{
		PrewarmYears:         []int{time.Now().Year()},
		IncludeTypes:         []string{"TV", "ONA"},
		CacheDBPath:          dbPath,
		AnibridgeMappingPath: filepath.Join(dir, "mappings.json.zst"),
	}

	c, err := cache.Open(dbPath)
	if err != nil {
		t.Fatalf("cache.Open: %v", err)
	}
	defer c.Close()

	sched := New(c, cfg)
	sched.LoadResolver(t.Context())

	ctx := context.Background()
	year := time.Now().Year()

	if err := sched.FetchAndStore(ctx, year, "integration_test"); err != nil {
		t.Fatalf("FetchAndStore: %v", err)
	}

	data, fresh, ok := c.GetYear(year)
	if !ok {
		t.Fatal("expected cache hit after FetchAndStore")
	}
	if !fresh {
		t.Log("data is not fresh — acceptable if fetch was slow")
	}

	shows, err := sched.Process(data, "WINTER", year, "series")
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(shows) == 0 {
		t.Fatal("expected at least one resolved show")
	}

	baselinePath := filepath.Join("testdata", "baseline-series.json")
	compareOrSaveBaseline(t, baselinePath, shows)
}

func TestIntegration_Prewarm(t *testing.T) {
	skipUnlessIntegration(t)

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "cache.db")

	cfg := &config.Config{
		PrewarmYears:         []int{time.Now().Year()},
		IncludeTypes:         []string{"TV", "ONA"},
		CacheDBPath:          dbPath,
		AnibridgeMappingPath: filepath.Join(dir, "mappings.json.zst"),
	}

	c, err := cache.Open(dbPath)
	if err != nil {
		t.Fatalf("cache.Open: %v", err)
	}
	defer c.Close()

	sched := New(c, cfg)
	sched.LoadResolver(t.Context())

	ctx := context.Background()
	if err := sched.Prewarm(ctx); err != nil {
		t.Fatalf("Prewarm: %v", err)
	}

	year := time.Now().Year()
	for _, category := range []string{"series", "series-new"} {
		data, fresh, ok := c.GetYear(year)
		if !ok {
			t.Fatalf("expected cache hit for year %d", year)
		}
		if !fresh {
			t.Logf("%s data is not fresh — acceptable", category)
		}

		shows, err := sched.Process(data, "WINTER", year, category)
		if err != nil {
			t.Fatalf("Process %s: %v", category, err)
		}
		if len(shows) == 0 {
			t.Fatalf("expected at least one resolved show in %s", category)
		}
	}

	stats := c.Stats()
	if stats.Entries == 0 {
		t.Fatal("expected cache entries after prewarm")
	}
	t.Logf("cache entries: %d", stats.Entries)
}

func compareOrSaveBaseline(t *testing.T, path string, shows []Show) {
	t.Helper()

	tvdbIDs := make([]int, len(shows))
	for i, s := range shows {
		tvdbIDs[i] = s.TVDBID
	}
	sort.Ints(tvdbIDs)

	data, err := json.MarshalIndent(tvdbIDs, "", "  ")
	if err != nil {
		t.Fatalf("marshal baseline: %v", err)
	}

	existing, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			t.Fatalf("read baseline: %v", err)
		}
		t.Logf("no baseline at %s, saving current output", path)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create baseline dir: %v", err)
		}
		if err := os.WriteFile(path, data, 0644); err != nil {
			t.Fatalf("write baseline: %v", err)
		}
		return
	}

	var expected []int
	if err := json.Unmarshal(existing, &expected); err != nil {
		t.Fatalf("unmarshal existing baseline: %v", err)
	}

	if len(tvdbIDs) != len(expected) {
		t.Errorf("show count: got %d, want %d", len(tvdbIDs), len(expected))
	}
	for i := range tvdbIDs {
		if i >= len(expected) {
			break
		}
		if tvdbIDs[i] != expected[i] {
			t.Errorf("tvdbId mismatch at position %d: got %d, want %d", i, tvdbIDs[i], expected[i])
		}
	}
}
