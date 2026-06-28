package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const (
	lastHitDebounceInterval = 5 * time.Minute
)

type Cache struct {
	db                   *sql.DB
	currentYearFreshness time.Duration
	pastYearFreshness    time.Duration
	hits                 atomic.Int64
	misses               atomic.Int64
	lastHitTimes         sync.Map // map[int]int64 — unix ts of last db write per year
	lastHitDebounce      atomic.Int64
	lastHitFailed        sync.Map // map[int]bool — set when UPDATE fails after retries
	retryHook            func()
}

type CacheStats struct {
	Entries int   `json:"entries"`
	Hits    int64 `json:"hits"`
	Misses  int64 `json:"misses"`
}

func Open(path string) (*Cache, error) {
	validatedPath, err := validateDBPath(path)
	if err != nil {
		return nil, err
	}
	db, err := openDB(validatedPath)
	if err != nil {
		// A BUSY error on startup suggests the database is stuck from a
		// previous crash. Since cache data is re-fetchable from AniList,
		// we remove the database and sidecar files and recreate fresh.
		if validatedPath != ":memory:" && isBusy(err) {
			slog.Warn("database appears stuck, recreating",
				"path", validatedPath,
				"error", err,
			)
			for _, p := range []string{validatedPath, validatedPath + "-wal", validatedPath + "-shm"} {
				if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
					slog.Warn("failed to remove file during recovery",
						"path", p, "error", err,
					)
				}
			}
			db, err = openDB(validatedPath)
			if err != nil {
				return nil, fmt.Errorf("reopen after recovery: %w", err)
			}
		} else {
			return nil, err
		}
	}

	c := &Cache{
		db:                   db,
		currentYearFreshness: 24 * time.Hour,
		pastYearFreshness:    7 * 24 * time.Hour,
	}
	c.lastHitDebounce.Store(int64(lastHitDebounceInterval))
	return c, nil
}

func validateDBPath(path string) (string, error) {
	if path == ":memory:" {
		return path, nil
	}
	if strings.ContainsAny(path, "?&") || strings.Contains(path, "://") {
		return "", fmt.Errorf("cache path must be a plain filesystem path: %s", path)
	}
	cleaned := filepath.Clean(path)
	if !filepath.IsAbs(cleaned) {
		return "", fmt.Errorf("cache path must be absolute: %s", path)
	}
	for _, base := range []string{"/data", os.TempDir()} {
		rel, err := filepath.Rel(base, cleaned)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
			return cleaned, nil
		}
	}
	return "", fmt.Errorf("cache path must be under /data: %s", path)
}

// openDB opens the sqlite database file, applies connection pool settings and
// performance/recovery PRAGMAs, creates the schema, and runs a diagnostic read
// to trigger WAL auto-recovery after a crash.
func openDB(path string) (*sql.DB, error) {
	validatedPath, err := validateDBPath(path)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", validatedPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	if validatedPath == ":memory:" {
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
		db.SetConnMaxLifetime(0)
	} else {
		db.SetMaxOpenConns(5)
		db.SetMaxIdleConns(1)
		db.SetConnMaxLifetime(5 * time.Minute)
	}

	if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		db.Close() //nolint:errcheck // cleanup on error path
		return nil, fmt.Errorf("enable WAL: %w", err)
	}

	if _, err := db.Exec(`PRAGMA busy_timeout=5000`); err != nil {
		db.Close() //nolint:errcheck // cleanup on error path
		return nil, fmt.Errorf("set busy_timeout: %w", err)
	}

	if _, err := db.Exec(`PRAGMA synchronous=NORMAL`); err != nil {
		db.Close() //nolint:errcheck // cleanup on error path
		return nil, fmt.Errorf("set synchronous: %w", err)
	}

	if _, err := db.Exec(`PRAGMA wal_autocheckpoint=1000`); err != nil {
		// Non-critical — log and continue.
		slog.Warn("set wal_autocheckpoint failed", "error", err)
	}

	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS year_cache (
			year       INTEGER NOT NULL PRIMARY KEY,
			data       BLOB NOT NULL,
			fetched_at INTEGER NOT NULL,
			last_hit   INTEGER NOT NULL DEFAULT 0
		)
	`); err != nil {
		db.Close() //nolint:errcheck // cleanup on error path
		return nil, fmt.Errorf("create year_cache table: %w", err)
	}

	// Diagnostic read: triggers SQLite WAL auto-recovery (if the database
	// was left in an inconsistent state by a prior crash) and verifies the
	// database is accessible.
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM year_cache`).Scan(&count); err != nil {
		db.Close() //nolint:errcheck // cleanup on error path
		return nil, fmt.Errorf("diagnostic read: %w", err)
	}

	// Force a WAL checkpoint to finalise any pending frames and shrink
	// the WAL file. Succeeds trivially on a fresh or clean database.
	if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		slog.Warn("startup WAL checkpoint failed", "error", err)
	}

	if err := db.Ping(); err != nil {
		db.Close() //nolint:errcheck // cleanup on error path
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}

	return db, nil
}

// isBusy reports whether err is a SQLITE_BUSY result (primary code 5),
// including its extended variants (SQLITE_BUSY_RECOVERY, etc.).
func isBusy(err error) bool {
	var se *sqlite.Error
	if errors.As(err, &se) {
		return se.Code()&0xff == sqlite3.SQLITE_BUSY
	}
	return false
}

// execWithRetry executes a write SQL statement, retrying up to 5 times with
// exponential backoff and jitter when the database returns SQLITE_BUSY.
// The cumulative backoff across all retries is ~17s, which combined with
// busy_timeout=5000 provides ~42s of total contention tolerance.
func (c *Cache) execWithRetry(ctx context.Context, query string, args ...any) error {
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		_, err = c.db.ExecContext(ctx, query, args...)
		if err == nil {
			return nil
		}
		if !isBusy(err) {
			return err
		}
		if c.retryHook != nil {
			c.retryHook()
		}
		backoff := time.Duration(50*(1<<attempt)) * time.Millisecond
		jitter := time.Duration(rand.Int64N(int64(backoff / 2)))
		timer := time.NewTimer(backoff + jitter)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
	return err
}

func (c *Cache) Close() error {
	return c.db.Close()
}

func (c *Cache) GetYear(year int) (data []byte, fresh bool, ok bool) {
	data, fresh, ok, err := c.GetYearContext(context.Background(), year)
	if err != nil {
		slog.Warn("cache get failed", "error", err, "year", year)
	}
	return data, fresh, ok
}

func (c *Cache) GetYearContext(ctx context.Context, year int) (data []byte, fresh bool, ok bool, err error) {
	var raw []byte
	var fetchedAt int64

	err = c.db.QueryRowContext(ctx,
		`SELECT data, fetched_at FROM year_cache WHERE year=?`,
		year,
	).Scan(&raw, &fetchedAt)

	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.misses.Add(1)
			return nil, false, false, nil
		}
		return nil, false, false, err
	}

	c.hits.Add(1)

	// Debounced last_hit update: only write to the database if enough time
	// has passed since the last write for this year. This drastically
	// reduces write contention from concurrent HTTP requests.
	now := time.Now().Unix()
	debounce := time.Duration(c.lastHitDebounce.Load())
	if last, loaded := c.lastHitTimes.Load(year); !loaded || now-last.(int64) >= int64(debounce.Seconds()) {
		if err := c.execWithRetry(ctx,
			`UPDATE year_cache SET last_hit=? WHERE year=?`,
			now, year,
		); err != nil {
			slog.Warn("failed to update last_hit", "error", err, "year", year)
			c.lastHitFailed.Store(year, true)
		} else {
			if _, wasFailed := c.lastHitFailed.LoadAndDelete(year); wasFailed {
				slog.Info("last_hit update recovered", "year", year)
			}
			c.lastHitTimes.Store(year, now)
		}
	}

	freshnessThreshold := c.pastYearFreshness
	if year == time.Now().Year() {
		freshnessThreshold = c.currentYearFreshness
	}
	fresh = time.Since(time.Unix(fetchedAt, 0)) < freshnessThreshold
	return raw, fresh, true, nil
}

func (c *Cache) SetYear(year int, data []byte) error {
	return c.SetYearContext(context.Background(), year, data)
}

func (c *Cache) SetYearContext(ctx context.Context, year int, data []byte) error {
	now := time.Now().Unix()
	return c.execWithRetry(ctx,
		`INSERT OR REPLACE INTO year_cache (year, data, fetched_at, last_hit) VALUES (?, ?, ?, ?)`,
		year, data, now, now,
	)
}

func (c *Cache) Clear() error {
	return c.ClearContext(context.Background())
}

func (c *Cache) ClearContext(ctx context.Context) error {
	if err := c.execWithRetry(ctx, `DELETE FROM year_cache`); err != nil {
		return err
	}
	c.hits.Store(0)
	c.misses.Store(0)
	c.lastHitTimes.Clear()
	c.lastHitFailed.Clear()
	return nil
}

func (c *Cache) HasYear(year int) bool {
	ok, err := c.HasYearContext(context.Background(), year)
	if err != nil {
		slog.Warn("cache has year failed", "error", err, "year", year)
	}
	return ok
}

func (c *Cache) HasYearContext(ctx context.Context, year int) (bool, error) {
	var count int
	if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM year_cache WHERE year=?`, year).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

func (c *Cache) Vacuum() error {
	return c.VacuumContext(context.Background())
}

func (c *Cache) VacuumContext(ctx context.Context) error {
	_, err := c.db.ExecContext(ctx, "VACUUM")
	return err
}

func (c *Cache) NeedsRefreshYears(currentYear int, currentRefreshDays, pastRefreshDays int) ([]int, error) {
	return c.NeedsRefreshYearsContext(context.Background(), currentYear, currentRefreshDays, pastRefreshDays)
}

func (c *Cache) NeedsRefreshYearsContext(ctx context.Context, currentYear int, currentRefreshDays, pastRefreshDays int) ([]int, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT year, fetched_at FROM year_cache`)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // rows.Err() captures iteration errors

	var years []int
	now := time.Now()

	for rows.Next() {
		var year int
		var fetchedAt int64
		if err := rows.Scan(&year, &fetchedAt); err != nil {
			return nil, err
		}

		ttl := time.Duration(pastRefreshDays) * 24 * time.Hour
		if year == currentYear {
			ttl = time.Duration(currentRefreshDays) * 24 * time.Hour
		}

		if now.Sub(time.Unix(fetchedAt, 0)) > ttl {
			years = append(years, year)
		}
	}

	return years, rows.Err()
}

func (c *Cache) PruneStaleYears(days int) (int, error) {
	return c.PruneStaleYearsContext(context.Background(), days)
}

func (c *Cache) PruneStaleYearsContext(ctx context.Context, days int) (int, error) {
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour).Unix()
	// Use fetched_at as a fallback when last_hit is 0 (e.g. entries created
	// before the column existed or after a failed last_hit UPDATE).
	result, err := c.db.ExecContext(ctx,
		`DELETE FROM year_cache WHERE CASE WHEN last_hit > 0 THEN last_hit ELSE fetched_at END < ?`,
		cutoff,
	)
	if err != nil {
		return 0, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

func (c *Cache) Stats() CacheStats {
	stats, err := c.StatsContext(context.Background())
	if err != nil {
		slog.Warn("cache stats failed", "error", err)
	}
	return stats
}

func (c *Cache) StatsContext(ctx context.Context) (CacheStats, error) {
	stats := CacheStats{Hits: c.hits.Load(), Misses: c.misses.Load()}
	if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM year_cache`).Scan(&stats.Entries); err != nil {
		return stats, err
	}
	return stats, nil
}

func (c *Cache) Ping() error {
	return c.PingContext(context.Background())
}

func (c *Cache) PingContext(ctx context.Context) error {
	return c.db.PingContext(ctx)
}

// SetLastHitDebounce sets the debounce interval for last_hit updates.
// Used in tests to control the debounce window.
func (c *Cache) SetLastHitDebounce(d time.Duration) {
	c.lastHitDebounce.Store(int64(d))
}
