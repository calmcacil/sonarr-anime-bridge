package cache

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
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
	lastHitDebounce      time.Duration
	lastHitFailed        sync.Map // map[int]bool — set when UPDATE fails after retries
}

type CacheStats struct {
	Entries int
	Hits    int64
	Misses  int64
}

func Open(path string) (*Cache, error) {
	db, err := openDB(path)
	if err != nil {
		if path != ":memory:" && isBusy(err) {
			slog.Warn("database busy on startup", "path", path, "error", err)
		}
		return nil, err
	}

	return &Cache{
		db:                   db,
		currentYearFreshness: 24 * time.Hour,
		pastYearFreshness:    7 * 24 * time.Hour,
		lastHitDebounce:      lastHitDebounceInterval,
	}, nil
}

// openDB opens the sqlite database file, applies connection pool settings and
// performance/recovery PRAGMAs, creates the schema, and runs a diagnostic read
// to trigger WAL auto-recovery after a crash.
func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(5 * time.Minute)

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
func (c *Cache) execWithRetry(query string, args ...any) error {
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		_, err = c.db.Exec(query, args...)
		if err == nil {
			return nil
		}
		if !isBusy(err) {
			return err
		}
		backoff := time.Duration(50*(1<<attempt)) * time.Millisecond
		jitter := time.Duration(rand.Int64N(int64(backoff / 2)))
		time.Sleep(backoff + jitter)
	}
	return err
}

func (c *Cache) Close() error {
	return c.db.Close()
}

func (c *Cache) GetYear(year int) (data []byte, fresh bool, ok bool) {
	var raw []byte
	var fetchedAt int64

	err := c.db.QueryRow(
		`SELECT data, fetched_at FROM year_cache WHERE year=?`,
		year,
	).Scan(&raw, &fetchedAt)

	if err != nil {
		c.misses.Add(1)
		return nil, false, false
	}

	c.hits.Add(1)

	// Debounced last_hit update: only write to the database if enough time
	// has passed since the last write for this year. This drastically
	// reduces write contention from concurrent HTTP requests.
	now := time.Now().Unix()
	if last, loaded := c.lastHitTimes.Load(year); !loaded || now-last.(int64) >= int64(c.lastHitDebounce.Seconds()) {
		if err := c.execWithRetry(
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
	return raw, fresh, true
}

func (c *Cache) SetYear(year int, data []byte) error {
	now := time.Now().Unix()
	return c.execWithRetry(
		`INSERT OR REPLACE INTO year_cache (year, data, fetched_at, last_hit) VALUES (?, ?, ?, ?)`,
		year, data, now, now,
	)
}

func (c *Cache) Clear() error {
	if err := c.execWithRetry(`DELETE FROM year_cache`); err != nil {
		return err
	}
	c.hits.Store(0)
	c.misses.Store(0)
	c.lastHitTimes.Clear()
	c.lastHitFailed.Clear()
	return nil
}

func (c *Cache) HasYear(year int) bool {
	var exists bool
	_ = c.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM year_cache WHERE year=?)`, year).Scan(&exists)
	return exists
}

func (c *Cache) Vacuum() error {
	return c.execWithRetry("VACUUM")
}

func (c *Cache) NeedsRefreshYears(currentYear int, currentRefreshDays, pastRefreshDays int) ([]int, error) {
	rows, err := c.db.Query(`SELECT year, fetched_at FROM year_cache`)
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
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour).Unix()
	// Use fetched_at as a fallback when last_hit is 0 (e.g. entries created
	// before the column existed or after a failed last_hit UPDATE).
	result, err := c.db.Exec(
		`DELETE FROM year_cache WHERE CASE WHEN last_hit > 0 THEN last_hit ELSE fetched_at END < ?`,
		cutoff,
	)
	if err != nil {
		return 0, err
	}
	n, _ := result.RowsAffected()
	return int(n), nil
}

func (c *Cache) Stats() CacheStats {
	stats := CacheStats{Hits: c.hits.Load(), Misses: c.misses.Load()}
	_ = c.db.QueryRow(`SELECT COUNT(*) FROM year_cache`).Scan(&stats.Entries)
	return stats
}

func (c *Cache) Ping() error {
	return c.db.Ping()
}

// SetLastHitDebounce sets the debounce interval for last_hit updates.
// Used in tests to control the debounce window.
func (c *Cache) SetLastHitDebounce(d time.Duration) {
	c.lastHitDebounce = d
}
