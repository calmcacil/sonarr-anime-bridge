package cache

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

func newMemCache(t *testing.T) *Cache {
	t.Helper()
	c, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return c
}

func newFileCache(t *testing.T) (*Cache, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "cache.db")
	c, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return c, dbPath
}

func holdWriteLock(t *testing.T, dbPath, stmt string, args ...any) func() {
	t.Helper()
	blocker, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.Exec(`PRAGMA busy_timeout=1`); err != nil {
		blocker.Close()
		t.Fatal(err)
	}
	if _, err := blocker.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		blocker.Close()
		t.Fatal(err)
	}
	tx, err := blocker.Begin()
	if err != nil {
		blocker.Close()
		t.Fatal(err)
	}
	if _, err := tx.Exec(stmt, args...); err != nil {
		tx.Rollback()
		blocker.Close()
		t.Fatal(err)
	}
	released := false
	release := func() {
		if !released {
			tx.Rollback()
			blocker.Close()
			released = true
		}
	}
	t.Cleanup(release)
	return release
}

func observeRetry(c *Cache) <-chan struct{} {
	ch := make(chan struct{})
	var once sync.Once
	c.retryHook = func() {
		once.Do(func() { close(ch) })
	}
	return ch
}

func seedYear(t *testing.T, c *Cache, year int, fetchedAt, lastHit int64) {
	t.Helper()
	if err := c.SetYearContext(context.Background(), year, []byte(`[]`)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.Exec(`UPDATE year_cache SET fetched_at=?, last_hit=? WHERE year=?`, fetchedAt, lastHit, year); err != nil {
		t.Fatal(err)
	}
}

func readLastHit(t *testing.T, c *Cache, year int) int64 {
	t.Helper()
	var lastHit int64
	if err := c.db.QueryRow(`SELECT last_hit FROM year_cache WHERE year=?`, year).Scan(&lastHit); err != nil {
		t.Fatal(err)
	}
	return lastHit
}

func eventually(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal(msg)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestOpenBusyDoesNotRemoveDatabaseOrSidecars(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "cache.db")
	wantFiles := map[string][]byte{
		dbPath:          []byte("healthy sqlite database"),
		dbPath + "-wal": []byte("healthy wal"),
		dbPath + "-shm": []byte("healthy shm"),
	}
	for path, contents := range wantFiles {
		if err := os.WriteFile(path, contents, 0600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	_, err := openCacheDB(dbPath, func(string) (*sql.DB, error) {
		return nil, errors.New("SQLITE_BUSY: database is locked")
	})
	if err == nil || !strings.Contains(err.Error(), "SQLITE_BUSY") {
		t.Fatalf("openCacheDB error = %v, want SQLITE_BUSY", err)
	}
	for path, want := range wantFiles {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s after busy open: %v", path, err)
		}
		if string(got) != string(want) {
			t.Errorf("%s changed after busy open: got %q, want %q", path, got, want)
		}
	}
}

func TestSQLiteOpenNameAppliesPragmasPerConnection(t *testing.T) {
	t.Parallel()

	got := sqliteOpenName("/tmp/cache with space.db")
	if !strings.HasPrefix(got, "file:///tmp/cache%20with%20space.db?") {
		t.Fatalf("sqliteOpenName prefix = %q", got)
	}
	for _, want := range []string{
		"_pragma=busy_timeout%285000%29",
		"_pragma=journal_mode%28WAL%29",
		"_pragma=synchronous%28NORMAL%29",
		"_pragma=wal_autocheckpoint%281000%29",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("sqliteOpenName = %q, missing %q", got, want)
		}
	}

	if got := sqliteOpenName(":memory:"); got != ":memory:" {
		t.Fatalf("sqliteOpenName(:memory:) = %q", got)
	}
}

func TestGetSetYearAndStats(t *testing.T) {
	t.Parallel()
	c := newMemCache(t)

	// Miss
	data, fresh, ok, err := c.GetYearContext(context.Background(), 2026)
	if err != nil {
		t.Fatalf("GetYearContext miss: %v", err)
	}
	if ok || data != nil || fresh {
		t.Errorf("miss = (%q, %v, %v), want (nil, false, false)", data, fresh, ok)
	}

	// Set + Get
	yearData := []byte(`[{"id":1,"title":{"romaji":"Test"},"format":"TV"}]`)
	if err := c.SetYearContext(context.Background(), 2026, yearData); err != nil {
		t.Fatalf("SetYear: %v", err)
	}
	data, fresh, ok, err = c.GetYearContext(context.Background(), 2026)
	if err != nil {
		t.Fatalf("GetYearContext after set: %v", err)
	}
	if !ok || string(data) != string(yearData) || !fresh {
		t.Errorf("after set = (%s, %v, %v), want (%s, true, true)", data, fresh, ok, yearData)
	}

	// Stats
	stats, err := c.StatsContext(context.Background())
	if err != nil {
		t.Fatalf("StatsContext: %v", err)
	}
	if stats.Entries != 1 || stats.Hits != 1 || stats.Misses != 1 {
		t.Errorf("stats = %+v, want {Entries:1 Hits:1 Misses:1}", stats)
	}

	// Clear resets stats and entries
	if err := c.ClearContext(context.Background()); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	stats, err = c.StatsContext(context.Background())
	if err != nil {
		t.Fatalf("StatsContext after clear: %v", err)
	}
	if stats.Entries != 0 || stats.Hits != 0 || stats.Misses != 0 {
		t.Errorf("stats after clear = %+v, want all zeros", stats)
	}
}

func TestSetYearOverwrite(t *testing.T) {
	t.Parallel()
	c := newMemCache(t)

	// Overwrite replaces data
	if err := c.SetYearContext(context.Background(), 2026, []byte(`"old"`)); err != nil {
		t.Fatal(err)
	}
	if err := c.SetYearContext(context.Background(), 2026, []byte(`"new"`)); err != nil {
		t.Fatal(err)
	}
	data, _, ok, err := c.GetYearContext(context.Background(), 2026)
	if err != nil || !ok || string(data) != `"new"` {
		t.Errorf("after overwrite = (%s, %v, %v), want new data", data, ok, err)
	}

	// Overwrite preserves last_hit
	const previousLastHit = int64(1000000)
	if _, err := c.db.Exec(`UPDATE year_cache SET last_hit=? WHERE year=2026`, previousLastHit); err != nil {
		t.Fatal(err)
	}
	if err := c.SetYearContext(context.Background(), 2026, []byte(`[{"tvdbId":9}]`)); err != nil {
		t.Fatal(err)
	}
	if got := readLastHit(t, c, 2026); got != previousLastHit {
		t.Errorf("last_hit changed on overwrite: got %d, want %d", got, previousLastHit)
	}

	// Fallback to fetched_at when last_hit=0
	if err := c.SetYearContext(context.Background(), 2027, []byte(`[]`)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.Exec(`UPDATE year_cache SET last_hit=0 WHERE year=2027`); err != nil {
		t.Fatal(err)
	}
	if err := c.SetYearContext(context.Background(), 2027, []byte(`[1]`)); err != nil {
		t.Fatal(err)
	}
	var fetchedAt, lastHit int64
	if err := c.db.QueryRow(`SELECT fetched_at, last_hit FROM year_cache WHERE year=2027`).Scan(&fetchedAt, &lastHit); err != nil {
		t.Fatal(err)
	}
	if lastHit != fetchedAt {
		t.Errorf("last_hit fallback: got %d, want fetched_at %d", lastHit, fetchedAt)
	}
}

func TestHasYearsContext(t *testing.T) {
	t.Parallel()
	c := newMemCache(t)

	for _, year := range []int{2024, 2025} {
		if err := c.SetYearContext(context.Background(), year, []byte(`[]`)); err != nil {
			t.Fatalf("SetYear(%d): %v", year, err)
		}
	}
	if _, err := c.db.Exec(`UPDATE year_cache SET last_hit=111 WHERE year=2024`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.Exec(`UPDATE year_cache SET last_hit=222 WHERE year=2025`); err != nil {
		t.Fatal(err)
	}

	closed, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	errAny := errors.New("any error")
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	bg := context.Background()
	tests := []struct {
		name  string
		c     *Cache
		ctx   context.Context
		years []int
		want  bool
		err   error
	}{
		{"all present", c, bg, []int{2024, 2025}, true, nil},
		{"partially present", c, bg, []int{2024, 2026}, false, nil},
		{"none present", c, bg, []int{2026, 2027}, false, nil},
		{"empty", c, bg, nil, true, nil},
		{"duplicates", c, bg, []int{2024, 2024, 2025, 2025}, true, nil},
		{"canceled empty", c, canceled, nil, false, context.Canceled},
		{"canceled nonempty", c, canceled, []int{2024}, false, context.Canceled},
		{"closed empty", closed, bg, nil, false, errAny},
	}

	beforeStats, err := c.StatsContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	beforeLastHits := map[int]int64{2024: readLastHit(t, c, 2024), 2025: readLastHit(t, c, 2025)}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.c.HasYearsContext(tt.ctx, tt.years)
			if tt.err == errAny {
				if err == nil {
					t.Fatal("HasYearsContext error = nil, want error")
				}
			} else if !errors.Is(err, tt.err) {
				t.Fatalf("HasYearsContext error = %v, want %v", err, tt.err)
			}
			if got != tt.want {
				t.Errorf("HasYearsContext(%v) = %v, want %v", tt.years, got, tt.want)
			}
		})
	}

	// Verify stats and last_hit unchanged
	c.flushLastHits(context.Background())
	afterStats, err := c.StatsContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if afterStats != beforeStats {
		t.Errorf("cache stats changed: before %+v, after %+v", beforeStats, afterStats)
	}
	for year, before := range beforeLastHits {
		if after := readLastHit(t, c, year); after != before {
			t.Errorf("last_hit for %d changed: before %d, after %d", year, before, after)
		}
	}
}

func TestNeedsRefreshYears(t *testing.T) {
	t.Parallel()

	// currentYear=2026, currentRefreshDays=1, pastRefreshDays=7.
	now := time.Now()
	tests := []struct {
		name      string
		year      int
		fetchedAt time.Time
		wantStale bool
	}{
		{"current year fresh", 2026, now, false},
		{"current year stale", 2026, now.Add(-25 * time.Hour), true},
		{"past year fresh", 2025, now.Add(-6 * 24 * time.Hour), false},
		{"past year stale", 2025, now.Add(-8 * 24 * time.Hour), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newMemCache(t)
			seedYear(t, c, tt.year, tt.fetchedAt.Unix(), tt.fetchedAt.Unix())
			years, err := c.NeedsRefreshYearsContext(context.Background(), 2026, 1, 7)
			if err != nil {
				t.Fatalf("NeedsRefreshYears: %v", err)
			}
			var want []int
			if tt.wantStale {
				want = []int{tt.year}
			}
			if !slices.Equal(years, want) {
				t.Errorf("NeedsRefreshYears = %v, want stale=%v for %d", years, tt.wantStale, tt.year)
			}
		})
	}
}

func TestPruneStaleYears(t *testing.T) {
	t.Parallel()

	now := time.Now().Unix()
	old := time.Now().Add(-40 * 24 * time.Hour).Unix()
	tests := []struct {
		name       string
		fetchedAt  int64
		lastHit    int64
		days       int
		wantPruned int // per seeded year (two years are seeded)
	}{
		{"fresh entries kept", now, now, 30, 0},
		{"stale last_hit pruned", old, old, 30, 1},
		{"recent last_hit keeps old fetched_at", 0, now, 1, 0},
		{"fetched_at fallback when last_hit=0", 0, 0, 1, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newMemCache(t)
			seedYear(t, c, 2020, tt.fetchedAt, tt.lastHit)
			seedYear(t, c, 2021, tt.fetchedAt, tt.lastHit)
			n, err := c.PruneStaleYearsContext(context.Background(), tt.days)
			if err != nil {
				t.Fatal(err)
			}
			if n != 2*tt.wantPruned {
				t.Errorf("pruned = %d, want %d", n, 2*tt.wantPruned)
			}
			stats, err := c.StatsContext(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if stats.Entries != 2-n {
				t.Errorf("remaining entries = %d, want %d", stats.Entries, 2-n)
			}
		})
	}
}

func TestPrunePreservesHitPendingAfterBusyWrite(t *testing.T) {
	c, dbPath := newFileCache(t)
	stale := time.Now().Add(-15 * 24 * time.Hour).Unix()
	for _, year := range []int{2020, 2021} {
		seedYear(t, c, year, stale, stale)
	}

	release := holdWriteLock(t, dbPath, `UPDATE year_cache SET data='[]' WHERE year=2020`)
	data, _, ok, err := c.GetYearContext(context.Background(), 2020)
	if err != nil || !ok || string(data) != `[]` {
		t.Fatalf("cache hit = (%q, %v, %v), want available data", data, ok, err)
	}
	eventually(t, 0, func() bool {
		_, failed := c.lastHitFailed.Load(2020)
		return failed
	}, "last_hit worker did not attempt the busy write")
	release()

	pruned, err := c.PruneStaleYearsContext(context.Background(), 14)
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 1 {
		t.Fatalf("pruned %d entries, want only the inactive year", pruned)
	}
	for year, want := range map[int]bool{2020: true, 2021: false} {
		got, err := c.HasYearsContext(context.Background(), []int{year})
		if err != nil {
			t.Fatalf("HasYearsContext(%d): %v", year, err)
		}
		if got != want {
			t.Errorf("year %d present = %v, want %v", year, got, want)
		}
	}

	// The failed write recovers on the next flush (the worker ticker may beat us to it).
	c.flushLastHits(context.Background())
	if _, failed := c.lastHitFailed.Load(2020); failed {
		t.Error("lastHitFailed not cleared after successful flush")
	}
	if got := readLastHit(t, c, 2020); got <= stale {
		t.Errorf("last_hit = %d, want advanced past %d", got, stale)
	}
}

func TestLastHitBehavior(t *testing.T) {
	t.Parallel()
	c := newMemCache(t)
	c.SetLastHitDebounce(0)

	if err := c.SetYearContext(context.Background(), 2026, []byte(`[]`)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.Exec(`UPDATE year_cache SET last_hit=1000 WHERE year=2026`); err != nil {
		t.Fatal(err)
	}

	// GetYear updates last_hit
	c.GetYearContext(context.Background(), 2026)
	eventually(t, 0, func() bool { return readLastHit(t, c, 2026) > 1000 }, "last_hit did not advance")
	updatedLastHit := readLastHit(t, c, 2026)
	if updatedLastHit < time.Now().Unix()-5 {
		t.Errorf("last_hit = %d, expected recent timestamp", updatedLastHit)
	}

	// Debounce prevents update
	c.SetLastHitDebounce(5 * time.Minute)
	if err := c.SetYearContext(context.Background(), 2027, []byte(`[]`)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.Exec(`UPDATE year_cache SET last_hit=0 WHERE year=2027`); err != nil {
		t.Fatal(err)
	}
	c.GetYearContext(context.Background(), 2027)
	eventually(t, 0, func() bool { return readLastHit(t, c, 2027) > 0 }, "first last_hit write did not occur")
	afterFirst := readLastHit(t, c, 2027)
	c.GetYearContext(context.Background(), 2027)
	c.flushLastHits(context.Background())
	if afterSecond := readLastHit(t, c, 2027); afterSecond != afterFirst {
		t.Errorf("last_hit changed within debounce window: %d -> %d", afterFirst, afterSecond)
	}
}

func TestLastHitWriteDoesNotBlockCacheHitAndCloseIsBounded(t *testing.T) {
	c, dbPath := newFileCache(t)
	seedYear(t, c, 2026, time.Now().Unix(), 1000)
	c.SetLastHitDebounce(0)
	c.db.SetMaxOpenConns(1)
	if _, err := c.db.Exec(`PRAGMA busy_timeout=10`); err != nil {
		t.Fatal(err)
	}

	release := holdWriteLock(t, dbPath, `UPDATE year_cache SET data='[]' WHERE year=2026`)

	// Cache hit doesn't block
	start := time.Now()
	data, fresh, ok, err := c.GetYearContext(context.Background(), 2026)
	if err != nil {
		t.Fatalf("GetYearContext during write contention: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("cache hit blocked for %s on last_hit bookkeeping", elapsed)
	}
	if !ok || !fresh || string(data) != `[]` {
		t.Fatalf("GetYearContext = (%q, %v, %v), want fresh cache hit", data, fresh, ok)
	}

	// Close is bounded by lastHitWriteTimeout (1s)
	start = time.Now()
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("Close took %s with pending busy last_hit write, want at most 3s", elapsed)
	}
	release()
}

func TestPeekYearContextDoesNotRecordAccess(t *testing.T) {
	c := newMemCache(t)
	if err := c.SetYearContext(context.Background(), 2026, []byte(`[]`)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.Exec(`UPDATE year_cache SET last_hit=123 WHERE year=2026`); err != nil {
		t.Fatal(err)
	}
	before, err := c.StatsContext(context.Background())
	if err != nil {
		t.Fatalf("StatsContext before PeekYearContext: %v", err)
	}

	data, fresh, ok, err := c.PeekYearContext(context.Background(), 2026)
	if err != nil {
		t.Fatalf("PeekYearContext: %v", err)
	}
	if !ok || !fresh || string(data) != `[]` {
		t.Fatalf("PeekYearContext = (%q, %v, %v), want fresh cache hit", data, fresh, ok)
	}
	after, err := c.StatsContext(context.Background())
	if err != nil {
		t.Fatalf("StatsContext after PeekYearContext: %v", err)
	}
	if after != before {
		t.Errorf("PeekYearContext changed stats: before %+v, after %+v", before, after)
	}
	if lastHit := readLastHit(t, c, 2026); lastHit != 123 {
		t.Errorf("PeekYearContext changed last_hit to %d, want 123", lastHit)
	}
}

func TestFreshnessSurvivesRestart(t *testing.T) {
	c, dbPath := newFileCache(t)
	if err := c.SetYearContext(context.Background(), 2020, []byte(`[]`)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.Exec(`UPDATE year_cache SET fetched_at=?, last_hit=? WHERE year=2020`, time.Now().Add(-10*24*time.Hour).Unix(), time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	data, fresh, ok, err := reopened.PeekYearContext(context.Background(), 2020)
	if err != nil {
		t.Fatalf("PeekYearContext after restart: %v", err)
	}
	if !ok || fresh || string(data) != `[]` {
		t.Fatalf("reopened year = (%q, %v, %v), want stale persisted data", data, fresh, ok)
	}
}

func TestStartupRecoveryCorruptDatabase(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	if err := os.WriteFile(dbPath, []byte("this is not a valid sqlite database"), 0644); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(dbPath+"-wal", []byte("garbage"), 0644)

	_, err := Open(dbPath)
	if err == nil {
		t.Error("expected error opening corrupt database, got nil")
	}
}

func TestRecoversFromBusy(t *testing.T) {
	var newMappings []SeenMapping
	tests := []struct {
		name   string
		setup  func(t *testing.T, c *Cache)
		write  func(c *Cache) error
		verify func(t *testing.T, c *Cache)
	}{
		{
			name: "SetYear",
			setup: func(t *testing.T, c *Cache) {
				seedYear(t, c, 2026, time.Now().Unix(), time.Now().Unix())
			},
			write: func(c *Cache) error {
				return c.SetYearContext(context.Background(), 2026, []byte(`[{"tvdbId":2}]`))
			},
			verify: func(t *testing.T, c *Cache) {
				var data []byte
				if err := c.db.QueryRow(`SELECT data FROM year_cache WHERE year=2026`).Scan(&data); err != nil {
					t.Fatal(err)
				}
				if string(data) != `[{"tvdbId":2}]` {
					t.Errorf("data = %s, want new payload", data)
				}
				if lastHit := readLastHit(t, c, 2026); lastHit == 0 {
					t.Error("last_hit not set after successful SetYear")
				}
			},
		},
		{
			name:  "MarkSeenMappings",
			setup: func(t *testing.T, c *Cache) {},
			write: func(c *Cache) error {
				mappings := []SeenMapping{
					{TVDBID: 1001, AniListID: 1, Title: "Show A", Season: "SUMMER", Year: 2026},
					{TVDBID: 1002, AniListID: 2, Title: "Show B", Season: "SUMMER", Year: 2026},
				}
				var err error
				newMappings, err = c.MarkSeenMappings(context.Background(), mappings)
				return err
			},
			verify: func(t *testing.T, c *Cache) {
				if len(newMappings) != 2 {
					t.Fatalf("expected 2 new mappings after retry, got %d", len(newMappings))
				}
				count, err := c.CountSeenMappings(context.Background())
				if err != nil {
					t.Fatalf("CountSeenMappings: %v", err)
				}
				if count != 2 {
					t.Fatalf("expected 2 committed seen mappings, got %d", count)
				}
			},
		},
		{
			name: "PruneStaleYears",
			setup: func(t *testing.T, c *Cache) {
				seedYear(t, c, 2020, 0, 0)
			},
			write: func(c *Cache) error {
				_, err := c.PruneStaleYearsContext(context.Background(), 1)
				return err
			},
			verify: func(t *testing.T, c *Cache) {
				stats, err := c.StatsContext(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if stats.Entries != 0 {
					t.Fatalf("entries = %d, want 0 after prune", stats.Entries)
				}
			},
		},
		{
			name: "Vacuum",
			setup: func(t *testing.T, c *Cache) {
				seedYear(t, c, 2026, time.Now().Unix(), time.Now().Unix())
			},
			write: func(c *Cache) error {
				return c.VacuumContext(context.Background())
			},
			verify: func(t *testing.T, c *Cache) {},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, dbPath := newFileCache(t)
			c.SetLastHitDebounce(0)
			tt.setup(t, c)

			c.db.SetMaxOpenConns(1)
			if _, err := c.db.Exec(`PRAGMA busy_timeout=10`); err != nil {
				t.Fatal(err)
			}

			release := holdWriteLock(t, dbPath, `INSERT OR IGNORE INTO year_cache (year, data, fetched_at, last_hit) VALUES (9999, '[]', 0, 0)`)
			retryCh := observeRetry(c)

			errCh := make(chan error, 1)
			go func() {
				errCh <- tt.write(c)
			}()

			select {
			case <-retryCh:
			case <-time.After(5 * time.Second):
				t.Fatal("operation did not observe a busy retry within 5s")
			}

			release()

			select {
			case err := <-errCh:
				if err != nil {
					t.Errorf("operation after lock release: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("operation did not complete within 5s")
			}

			tt.verify(t, c)
		})
	}
}

func TestConcurrentAccess(t *testing.T) {
	n := 10
	if os.Getenv("STRESS") == "1" {
		n = 50
	}

	tests := []struct {
		name    string
		newFunc func(t *testing.T) *Cache
	}{
		{name: "memory", newFunc: func(t *testing.T) *Cache { return newMemCache(t) }},
		{name: "file", newFunc: func(t *testing.T) *Cache { c, _ := newFileCache(t); return c }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := tt.newFunc(t)
			c.SetLastHitDebounce(0) // every hit schedules a write, maximising contention

			var errCount atomic.Int64
			var wg sync.WaitGroup
			for range n {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if err := c.SetYearContext(context.Background(), 2026, []byte(`[{"tvdbId":1}]`)); err != nil {
						errCount.Add(1)
						t.Logf("SetYear(2026) error: %v", err)
					}
					c.GetYearContext(context.Background(), 2026)
					if err := c.SetYearContext(context.Background(), 2025, []byte(`[]`)); err != nil {
						errCount.Add(1)
						t.Logf("SetYear(2025) error: %v", err)
					}
					c.GetYearContext(context.Background(), 2025)
					c.StatsContext(context.Background())
				}()
			}
			wg.Wait()

			if n := errCount.Load(); n > 0 {
				t.Errorf("SetYear returned %d errors under concurrent load", n)
			}

			stats, err := c.StatsContext(context.Background())
			if err != nil {
				t.Fatalf("StatsContext: %v", err)
			}
			if stats.Entries == 0 {
				t.Error("no cache entries after concurrent SetYear calls")
			}
		})
	}
}

func TestSeenMappings(t *testing.T) {
	t.Parallel()
	c := newMemCache(t)
	ctx := context.Background()

	// Empty batch is no-op
	newMappings, err := c.MarkSeenMappings(ctx, nil)
	if err != nil || len(newMappings) != 0 {
		t.Fatalf("MarkSeenMappings(nil) = (%d, %v), want (0, nil)", len(newMappings), err)
	}
	newMappings, err = c.MarkSeenMappings(ctx, []SeenMapping{})
	if err != nil || len(newMappings) != 0 {
		t.Fatalf("MarkSeenMappings(empty) = (%d, %v), want (0, nil)", len(newMappings), err)
	}

	// First run: all new
	count, err := c.CountSeenMappings(ctx)
	if err != nil || count != 0 {
		t.Fatalf("CountSeenMappings = (%d, %v), want (0, nil)", count, err)
	}
	mappings := []SeenMapping{
		{TVDBID: 1001, AniListID: 1, Title: "Show A", Season: "SUMMER", Year: 2026, StartsAt: "15.06.26"},
		{TVDBID: 1002, AniListID: 2, Title: "Show B", Season: "SUMMER", Year: 2026, StartsAt: ""},
	}
	newMappings, err = c.MarkSeenMappings(ctx, mappings)
	if err != nil || len(newMappings) != 2 {
		t.Fatalf("first insert = (%d, %v), want (2, nil)", len(newMappings), err)
	}
	count, err = c.CountSeenMappings(ctx)
	if err != nil || count != 2 {
		t.Fatalf("CountSeenMappings = (%d, %v), want (2, nil)", count, err)
	}

	// Duplicate: same (tvdb_id, season, year)
	newMappings, err = c.MarkSeenMappings(ctx, []SeenMapping{{TVDBID: 1001, AniListID: 1, Title: "Show A", Season: "SUMMER", Year: 2026}})
	if err != nil || len(newMappings) != 0 {
		t.Fatalf("duplicate = (%d, %v), want (0, nil)", len(newMappings), err)
	}

	// Different season: new
	newMappings, err = c.MarkSeenMappings(ctx, []SeenMapping{{TVDBID: 1001, AniListID: 1, Title: "Show A", Season: "FALL", Year: 2026}})
	if err != nil || len(newMappings) != 1 {
		t.Fatalf("different season = (%d, %v), want (1, nil)", len(newMappings), err)
	}

	// Different year: new
	newMappings, err = c.MarkSeenMappings(ctx, []SeenMapping{{TVDBID: 1001, AniListID: 1, Title: "Show A", Season: "SUMMER", Year: 2027}})
	if err != nil || len(newMappings) != 1 {
		t.Fatalf("different year = (%d, %v), want (1, nil)", len(newMappings), err)
	}

	// Clear
	if err := c.execWithRetry(ctx, `DELETE FROM seen_mappings`); err != nil {
		t.Fatalf("clear: %v", err)
	}
	count, err = c.CountSeenMappings(ctx)
	if err != nil || count != 0 {
		t.Fatalf("CountSeenMappings after clear = (%d, %v), want (0, nil)", count, err)
	}
}
