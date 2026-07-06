package cache

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestOpenAndClose(t *testing.T) {
	t.Parallel()

	c, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("Close: %v", err)
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

func TestGetYear_Miss(t *testing.T) {
	t.Parallel()

	c, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	data, fresh, ok := c.GetYear(2026)
	if ok {
		t.Error("expected miss")
	}
	if data != nil {
		t.Error("expected nil data on miss")
	}
	if fresh {
		t.Error("expected not fresh on miss")
	}
}

func TestSetAndGetYear(t *testing.T) {
	t.Parallel()

	c, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	yearData := []byte(`[{"id":1,"title":{"romaji":"Test"},"format":"TV"}]`)
	if err := c.SetYear(2026, yearData); err != nil {
		t.Fatalf("SetYear: %v", err)
	}

	data, fresh, ok := c.GetYear(2026)
	if !ok {
		t.Fatal("expected hit after SetYear")
	}
	if string(data) != string(yearData) {
		t.Errorf("data = %s, want %s", data, yearData)
	}
	if !fresh {
		t.Error("expected fresh")
	}
}

func TestSetYear_OverwritesExisting(t *testing.T) {
	t.Parallel()

	c, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	c.SetYear(2026, []byte(`"old"`))
	c.SetYear(2026, []byte(`"new"`))

	data, _, ok := c.GetYear(2026)
	if !ok {
		t.Fatal("expected hit")
	}
	if string(data) != `"new"` {
		t.Errorf("data = %s, want \"new\"", data)
	}
}

func TestNeedsRefreshYears(t *testing.T) {
	t.Parallel()

	c, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	c.SetYear(2025, []byte(`[]`))
	c.SetYear(2026, []byte(`[]`))

	// Entries just created should NOT need refresh
	years, err := c.NeedsRefreshYears(2026, 1, 7)
	if err != nil {
		t.Fatalf("NeedsRefreshYears: %v", err)
	}
	if len(years) != 0 {
		t.Errorf("expected 0 stale years, got %d", len(years))
	}
}

func TestPruneStaleYears(t *testing.T) {
	t.Parallel()

	c, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	c.SetYear(2020, []byte(`[]`))
	c.SetYear(2021, []byte(`[]`))

	// Recently set, should not be pruned with short duration
	n, err := c.PruneStaleYears(30)
	if err != nil {
		t.Fatalf("PruneStaleYears: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 pruned from fresh entries, got %d", n)
	}

	// Manually push last_hit and fetched_at far in the past so both
	// prune fallbacks trigger correctly.
	c.db.Exec(`UPDATE year_cache SET last_hit = 0, fetched_at = 0`)
	n, err = c.PruneStaleYears(1)
	if err != nil {
		t.Fatalf("PruneStaleYears: %v", err)
	}
	if n != 2 {
		t.Errorf("expected 2 pruned with stale entries, got %d", n)
	}
}

func TestClear(t *testing.T) {
	c, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	c.SetYear(2026, []byte(`[]`))
	if err := c.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}

	stats := c.Stats()
	if stats.Entries != 0 {
		t.Errorf("expected 0 entries after clear, got %d", stats.Entries)
	}
}

func TestStats(t *testing.T) {
	c, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	c.SetYear(2025, []byte(`[]`))
	c.SetYear(2026, []byte(`[]`))
	c.GetYear(2026)

	stats := c.Stats()
	if stats.Entries != 2 {
		t.Errorf("entries = %d, want 2", stats.Entries)
	}
	if stats.Hits != 1 {
		t.Errorf("hits = %d, want 1", stats.Hits)
	}
	if stats.Misses != 0 {
		t.Errorf("misses = %d, want 0", stats.Misses)
	}
}

func TestConcurrentAccess(t *testing.T) {
	c, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.SetYear(2026, []byte(`[{"tvdbId":1}]`))
			c.GetYear(2026)
			c.SetYear(2025, []byte(`[]`))
			c.GetYear(2025)
			c.Stats()
		}()
	}
	wg.Wait()
}

// ---------------------------------------------------------------------------
// New tests: last_hit correctness
// ---------------------------------------------------------------------------

func TestLastHit_UpdatedOnGet(t *testing.T) {
	t.Parallel()

	c, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetLastHitDebounce(0) // no debounce — every GetYear must attempt UPDATE

	if err := c.SetYear(2026, []byte(`[]`)); err != nil {
		t.Fatal(err)
	}

	var initialLastHit int64
	if err := c.db.QueryRow(`SELECT last_hit FROM year_cache WHERE year=2026`).Scan(&initialLastHit); err != nil {
		t.Fatal(err)
	}

	// GetYear triggers the UPDATE (value may be the same within the same second).
	c.GetYear(2026)

	var updatedLastHit int64
	if err := c.db.QueryRow(`SELECT last_hit FROM year_cache WHERE year=2026`).Scan(&updatedLastHit); err != nil {
		t.Fatal(err)
	}

	if updatedLastHit < initialLastHit {
		t.Errorf("last_hit decreased: was %d, now %d", initialLastHit, updatedLastHit)
	}

	// Verify the in-memory debounce tracker was populated.
	if _, ok := c.lastHitTimes.Load(2026); !ok {
		t.Error("debounce tracker not set after GetYear")
	}

	// Verify the failure flag is clean — a successful UPDATE clears
	// any prior failure state.
	if _, ok := c.lastHitFailed.Load(2026); ok {
		t.Error("lastHitFailed should be unset after successful GetYear")
	}
}

func TestLastHit_FailureTracking(t *testing.T) {
	t.Parallel()

	c, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetLastHitDebounce(0)

	if err := c.SetYear(2026, []byte(`[]`)); err != nil {
		t.Fatal(err)
	}

	// Simulate a prior failure by directly setting the flag.
	c.lastHitFailed.Store(2026, true)

	// A successful GetYear should clear the flag and log recovery.
	data, fresh, ok := c.GetYear(2026)
	if !ok {
		t.Fatal("expected hit")
	}
	if !fresh {
		t.Error("expected fresh data")
	}
	_ = data // content verified by other tests

	if _, ok := c.lastHitFailed.Load(2026); ok {
		t.Error("lastHitFailed should be cleared after successful GetYear")
	}
}

func TestLastHit_Debounced(t *testing.T) {
	t.Parallel()

	c, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetLastHitDebounce(5 * time.Minute) // debounce active

	if err := c.SetYear(2026, []byte(`[]`)); err != nil {
		t.Fatal(err)
	}

	// First GetYear — triggers the UPDATE
	c.GetYear(2026)

	var afterFirst int64
	if err := c.db.QueryRow(`SELECT last_hit FROM year_cache WHERE year=2026`).Scan(&afterFirst); err != nil {
		t.Fatal(err)
	}

	time.Sleep(10 * time.Millisecond)

	// Second GetYear within debounce window — should NOT update last_hit
	c.GetYear(2026)

	var afterSecond int64
	if err := c.db.QueryRow(`SELECT last_hit FROM year_cache WHERE year=2026`).Scan(&afterSecond); err != nil {
		t.Fatal(err)
	}

	if afterSecond != afterFirst {
		t.Errorf("last_hit changed within debounce window: %d -> %d", afterFirst, afterSecond)
	}
}

func TestSetYear_SetsLastHit(t *testing.T) {
	t.Parallel()

	c, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err := c.SetYear(2026, []byte(`[]`)); err != nil {
		t.Fatal(err)
	}

	var lastHit int64
	if err := c.db.QueryRow(`SELECT last_hit FROM year_cache WHERE year=2026`).Scan(&lastHit); err != nil {
		t.Fatal(err)
	}

	if lastHit == 0 {
		t.Fatal("last_hit is 0 after SetYear, expected a recent timestamp")
	}
	now := time.Now().Unix()
	if lastHit < now-5 {
		t.Errorf("last_hit = %d, expected within 5s of now (%d)", lastHit, now)
	}
}

func TestSetYear_OverwriteUpdatesLastHit(t *testing.T) {
	t.Parallel()

	c, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err := c.SetYear(2026, []byte(`[]`)); err != nil {
		t.Fatal(err)
	}

	// Age last_hit to a very old timestamp
	if _, err := c.db.Exec(`UPDATE year_cache SET last_hit = 1000000 WHERE year = 2026`); err != nil {
		t.Fatal(err)
	}

	// Overwrite with different data
	if err := c.SetYear(2026, []byte(`[{"tvdbId":9}]`)); err != nil {
		t.Fatal(err)
	}

	var lastHit int64
	if err := c.db.QueryRow(`SELECT last_hit FROM year_cache WHERE year=2026`).Scan(&lastHit); err != nil {
		t.Fatal(err)
	}

	now := time.Now().Unix()
	if lastHit < now-5 {
		t.Errorf("last_hit not refreshed on overwrite: got %d, expected ~%d", lastHit, now)
	}
}

func TestPrune_UsesLastHitWhenAvailable(t *testing.T) {
	t.Parallel()

	c, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err := c.SetYear(2020, []byte(`[]`)); err != nil {
		t.Fatal(err)
	}

	// Set fetched_at to a very old value but last_hit to "now".
	// Prune should keep the entry because last_hit is recent.
	now := time.Now().Unix()
	if _, err := c.db.Exec(`UPDATE year_cache SET fetched_at = 0, last_hit = ? WHERE year = 2020`, now); err != nil {
		t.Fatal(err)
	}

	n, err := c.PruneStaleYears(1)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("recently-hit entry pruned: got %d, want 0", n)
	}

	stats := c.Stats()
	if stats.Entries != 1 {
		t.Errorf("entries = %d, want 1", stats.Entries)
	}
}

func TestStartupRecovery_StuckDatabase(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	// Create and seed
	c1, err := Open(dbPath)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := c1.SetYear(2026, []byte(`[{"tvdbId":1}]`)); err != nil {
		t.Fatal(err)
	}
	c1.Close()

	// Clean re-open — data should be intact
	c2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("re-open after clean close: %v", err)
	}
	defer c2.Close()

	data, _, ok := c2.GetYear(2026)
	if !ok {
		t.Fatal("data not found after clean re-open")
	}
	if string(data) != `[{"tvdbId":1}]` {
		t.Errorf("data = %s, want [{\"tvdbId\":1}]", data)
	}

	// Corrupt the database file to simulate a stuck/unreadable DB
	c2.Close()

	if err := os.WriteFile(dbPath, []byte("this is not a valid sqlite database"), 0644); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(dbPath+"-wal", []byte("garbage"), 0644)

	_, err = Open(dbPath)
	if err == nil {
		t.Error("expected error opening corrupt database, got nil")
	} else {
		t.Logf("corrupt database correctly rejected: %v", err)
	}
}

func TestExecWithRetry_RecoversFromBusy(t *testing.T) {
	// NOT parallel — timing-sensitive test

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	// Open the Cache with standard settings
	c, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetLastHitDebounce(0)

	// Seed initial data
	if err := c.SetYear(2026, []byte(`[{"tvdbId":1}]`)); err != nil {
		t.Fatal(err)
	}

	// Constrain the Cache pool to one connection so the PRAGMA below is
	// guaranteed to be set on the connection that SetYear will use.
	c.db.SetMaxOpenConns(1)

	// Set a very short busy_timeout so SQLite-level retry returns BUSY
	// quickly and our Go-level retry loop is exercised.
	if _, err := c.db.Exec(`PRAGMA busy_timeout=10`); err != nil {
		t.Fatal(err)
	}

	// Open a separate connection that will hold a write lock.
	blocker, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	if _, err := blocker.Exec(`PRAGMA busy_timeout=1`); err != nil {
		t.Fatal(err)
	}
	// Ensure WAL journaling so both pools are on the same page
	if _, err := blocker.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		t.Fatal(err)
	}

	// Start a transaction and execute a write to acquire the SQLite write lock.
	tx, err := blocker.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE year_cache SET data='[]' WHERE year=2026`); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}

	retryObserved := make(chan struct{})
	var retryOnce sync.Once
	c.retryHook = func() {
		retryOnce.Do(func() {
			close(retryObserved)
		})
	}

	// Launch SetYear on the Cache — it will hit BUSY, then our retry fires.
	errCh := make(chan error, 1)
	go func() {
		errCh <- c.SetYear(2026, []byte(`[{"tvdbId":2}]`))
	}()

	select {
	case <-retryObserved:
	case <-time.After(5 * time.Second):
		t.Fatal("SetYear did not observe a busy retry within 5s")
	}

	// Release the write lock so the retry can succeed.
	tx.Rollback()

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("SetYear after lock release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SetYear did not complete within 5s — retry may be broken")
	}

	// Verify the write was actually applied.
	var lastHit int64
	if err := c.db.QueryRow(`SELECT last_hit FROM year_cache WHERE year=2026`).Scan(&lastHit); err != nil {
		t.Fatal(err)
	}
	if lastHit == 0 {
		t.Error("last_hit not set after successful SetYear")
	}
}

func TestMarkSeenMappings_RecoversFromBusy(t *testing.T) {
	// NOT parallel — timing-sensitive test

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	c, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	c.db.SetMaxOpenConns(1)
	if _, err := c.db.Exec(`PRAGMA busy_timeout=10`); err != nil {
		t.Fatal(err)
	}

	blocker, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	if _, err := blocker.Exec(`PRAGMA busy_timeout=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		t.Fatal(err)
	}

	tx, err := blocker.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(
		`INSERT OR IGNORE INTO seen_mappings (tvdb_id, anilist_id, title, season, year, first_seen_at, starts_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		9999, 9999, "Lock Holder", "SUMMER", 2026, time.Now().Unix(), "",
	); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}

	retryObserved := make(chan struct{})
	var retryOnce sync.Once
	c.retryHook = func() {
		retryOnce.Do(func() {
			close(retryObserved)
		})
	}

	errCh := make(chan error, 1)
	resultCh := make(chan []SeenMapping, 1)
	go func() {
		mappings := []SeenMapping{
			{TVDBID: 1001, AniListID: 1, Title: "Show A", Season: "SUMMER", Year: 2026},
			{TVDBID: 1002, AniListID: 2, Title: "Show B", Season: "SUMMER", Year: 2026},
		}
		newMappings, err := c.MarkSeenMappings(context.Background(), mappings)
		resultCh <- newMappings
		errCh <- err
	}()

	select {
	case <-retryObserved:
	case <-time.After(5 * time.Second):
		t.Fatal("MarkSeenMappings did not observe a busy retry within 5s")
	}

	tx.Rollback()

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("MarkSeenMappings after lock release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("MarkSeenMappings did not complete within 5s")
	}

	newMappings := <-resultCh
	if len(newMappings) != 2 {
		t.Fatalf("expected 2 new mappings after retry, got %d", len(newMappings))
	}

	count, err := c.CountSeenMappings(context.Background())
	if err != nil {
		t.Fatalf("CountSeenMappings: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 committed seen mappings after rolled-back blocker, got %d", count)
	}
}

func TestPruneStaleYears_RecoversFromBusy(t *testing.T) {
	// NOT parallel — timing-sensitive test

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	c, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err := c.SetYear(2020, []byte(`[]`)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.Exec(`UPDATE year_cache SET fetched_at = 0, last_hit = 0 WHERE year = 2020`); err != nil {
		t.Fatal(err)
	}

	c.db.SetMaxOpenConns(1)
	if _, err := c.db.Exec(`PRAGMA busy_timeout=10`); err != nil {
		t.Fatal(err)
	}

	blocker, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	if _, err := blocker.Exec(`PRAGMA busy_timeout=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		t.Fatal(err)
	}

	tx, err := blocker.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE year_cache SET data='[]' WHERE year=2020`); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}

	retryObserved := make(chan struct{})
	var retryOnce sync.Once
	c.retryHook = func() {
		retryOnce.Do(func() {
			close(retryObserved)
		})
	}

	errCh := make(chan error, 1)
	resultCh := make(chan int, 1)
	go func() {
		n, err := c.PruneStaleYearsContext(context.Background(), 1)
		resultCh <- n
		errCh <- err
	}()

	select {
	case <-retryObserved:
	case <-time.After(5 * time.Second):
		t.Fatal("PruneStaleYears did not observe a busy retry within 5s")
	}

	tx.Rollback()

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("PruneStaleYears after lock release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("PruneStaleYears did not complete within 5s")
	}

	n := <-resultCh
	if n != 1 {
		t.Fatalf("expected 1 pruned entry after retry, got %d", n)
	}
}

func TestVacuum_RecoversFromBusy(t *testing.T) {
	// NOT parallel — timing-sensitive test

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	c, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err := c.SetYear(2026, []byte(`[]`)); err != nil {
		t.Fatal(err)
	}

	c.db.SetMaxOpenConns(1)
	if _, err := c.db.Exec(`PRAGMA busy_timeout=10`); err != nil {
		t.Fatal(err)
	}

	blocker, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	if _, err := blocker.Exec(`PRAGMA busy_timeout=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		t.Fatal(err)
	}

	tx, err := blocker.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE year_cache SET data='[]' WHERE year=2026`); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}

	retryObserved := make(chan struct{})
	var retryOnce sync.Once
	c.retryHook = func() {
		retryOnce.Do(func() {
			close(retryObserved)
		})
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- c.VacuumContext(context.Background())
	}()

	select {
	case <-retryObserved:
	case <-time.After(5 * time.Second):
		t.Fatal("Vacuum did not observe a busy retry within 5s")
	}

	tx.Rollback()

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Vacuum after lock release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Vacuum did not complete within 5s")
	}
}

func TestConcurrentAccess_NoBusyErrors(t *testing.T) {
	if os.Getenv("STRESS") != "1" {
		t.Skip("set STRESS=1 to run SQLite contention stress test")
	}
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	c, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Disable debounce so every GetYear tries to write, maximising contention.
	c.SetLastHitDebounce(0)

	var errCount atomic.Int64
	var wg sync.WaitGroup

	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.SetYear(2026, []byte(`[{"tvdbId":1}]`)); err != nil {
				errCount.Add(1)
				t.Logf("SetYear(2026) error: %v", err)
			}
			c.GetYear(2026)
			if err := c.SetYear(2025, []byte(`[]`)); err != nil {
				errCount.Add(1)
				t.Logf("SetYear(2025) error: %v", err)
			}
			c.GetYear(2025)
			c.Stats()
		}()
	}

	wg.Wait()

	if n := errCount.Load(); n > 0 {
		t.Errorf("SetYear returned %d errors under concurrent load (busy_timeout + retry should have handled them)", n)
	}

	// Sanity-check that data survived.
	stats := c.Stats()
	if stats.Entries == 0 {
		t.Error("no cache entries after concurrent SetYear calls")
	} else {
		t.Logf("concurrent stress: %d entries, %d hits, %d misses",
			stats.Entries, stats.Hits, stats.Misses,
		)
	}
}

func TestSeenMapping_FirstRunSilent(t *testing.T) {
	t.Parallel()

	c, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	ctx := context.Background()

	count, err := c.CountSeenMappings(ctx)
	if err != nil {
		t.Fatalf("CountSeenMappings: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 seen mappings on fresh cache, got %d", count)
	}

	// First batch: should mark all as new
	mappings := []SeenMapping{
		{TVDBID: 1001, AniListID: 1, Title: "Show A", Season: "SUMMER", Year: 2026, StartsAt: "15.06.26"},
		{TVDBID: 1002, AniListID: 2, Title: "Show B", Season: "SUMMER", Year: 2026, StartsAt: ""},
	}
	newMappings, err := c.MarkSeenMappings(ctx, mappings)
	if err != nil {
		t.Fatalf("MarkSeenMappings: %v", err)
	}
	if len(newMappings) != 2 {
		t.Fatalf("expected 2 new mappings, got %d", len(newMappings))
	}

	count, err = c.CountSeenMappings(ctx)
	if err != nil {
		t.Fatalf("CountSeenMappings: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 seen mappings after insert, got %d", count)
	}
}

func TestSeenMapping_DeduplicatesByTVDBSeasonYear(t *testing.T) {
	t.Parallel()

	c, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	ctx := context.Background()

	mappings := []SeenMapping{
		{TVDBID: 1001, AniListID: 1, Title: "Show A", Season: "SUMMER", Year: 2026},
	}
	newMappings, err := c.MarkSeenMappings(ctx, mappings)
	if err != nil {
		t.Fatalf("MarkSeenMappings: %v", err)
	}
	if len(newMappings) != 1 {
		t.Fatalf("expected 1 new mapping on first insert, got %d", len(newMappings))
	}

	// Same (tvdb_id, season, year) again: should be deduplicated
	newMappings, err = c.MarkSeenMappings(ctx, mappings)
	if err != nil {
		t.Fatalf("MarkSeenMappings (dup): %v", err)
	}
	if len(newMappings) != 0 {
		t.Fatalf("expected 0 new mappings on duplicate, got %d", len(newMappings))
	}

	// Same tvdb_id, different season: should be new
	mappings[0].Season = "FALL"
	newMappings, err = c.MarkSeenMappings(ctx, mappings)
	if err != nil {
		t.Fatalf("MarkSeenMappings (new season): %v", err)
	}
	if len(newMappings) != 1 {
		t.Fatalf("expected 1 new mapping for different season, got %d", len(newMappings))
	}

	// Same tvdb_id, different year: should be new
	mappings[0].Season = "SUMMER"
	mappings[0].Year = 2027
	newMappings, err = c.MarkSeenMappings(ctx, mappings)
	if err != nil {
		t.Fatalf("MarkSeenMappings (new year): %v", err)
	}
	if len(newMappings) != 1 {
		t.Fatalf("expected 1 new mapping for different year, got %d", len(newMappings))
	}
}

func TestSeenMapping_ClearRemovesAll(t *testing.T) {
	t.Parallel()

	c, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	ctx := context.Background()

	mappings := []SeenMapping{
		{TVDBID: 1001, AniListID: 1, Title: "Show A", Season: "SUMMER", Year: 2026},
	}
	if _, err := c.MarkSeenMappings(ctx, mappings); err != nil {
		t.Fatalf("MarkSeenMappings: %v", err)
	}

	if err := c.ClearSeenMappings(ctx); err != nil {
		t.Fatalf("ClearSeenMappings: %v", err)
	}

	count, err := c.CountSeenMappings(ctx)
	if err != nil {
		t.Fatalf("CountSeenMappings: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 seen mappings after clear, got %d", count)
	}
}

func TestSeenMapping_EmptyBatchNoOp(t *testing.T) {
	t.Parallel()

	c, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	newMappings, err := c.MarkSeenMappings(context.Background(), nil)
	if err != nil {
		t.Fatalf("MarkSeenMappings(nil): %v", err)
	}
	if len(newMappings) != 0 {
		t.Fatalf("expected 0 new mappings for nil batch, got %d", len(newMappings))
	}

	newMappings, err = c.MarkSeenMappings(context.Background(), []SeenMapping{})
	if err != nil {
		t.Fatalf("MarkSeenMappings(empty): %v", err)
	}
	if len(newMappings) != 0 {
		t.Fatalf("expected 0 new mappings for empty batch, got %d", len(newMappings))
	}
}
