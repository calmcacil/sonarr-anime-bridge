package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/calmcacil/sonarr-anime-bridge/internal/datapath"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const (
	lastHitDebounceInterval = 5 * time.Minute
	lastHitWriteTimeout     = 1 * time.Second
	lastHitRetryInterval    = 5 * time.Second
	lastHitBusyTimeoutMS    = 100
	busyRetryAttempts       = 5
)

type Cache struct {
	db                   *sql.DB
	currentYearFreshness time.Duration
	pastYearFreshness    time.Duration
	hits                 atomic.Int64
	misses               atomic.Int64
	lastHitTimes         sync.Map // map[int]int64 — unix ts of last scheduled write per year
	lastHitDebounce      atomic.Int64
	lastHitFailed        sync.Map // map[int]bool — set when an asynchronous UPDATE fails
	lastHitMu            sync.Mutex
	pendingLastHits      map[int]int64
	lastHitWake          chan struct{}
	lastHitCtx           context.Context
	lastHitCancel        context.CancelFunc
	lastHitDone          chan struct{}
	closeOnce            sync.Once
	retryHook            func()
}

type CacheStats struct {
	Entries int   `json:"entries"`
	Hits    int64 `json:"hits"`
	Misses  int64 `json:"misses"`
}

// SeenMapping tracks a resolved TVDB mapping that has been recorded so
// that new mappings can be detected and logged on subsequent cycles.
type SeenMapping struct {
	TVDBID      int    `json:"tvdbId"`
	AniListID   int    `json:"anilistId"`
	Title       string `json:"title"`
	Season      string `json:"season"`
	Year        int    `json:"year"`
	StartsAt    string `json:"startsAt,omitempty"`
	FirstSeenAt int64  `json:"firstSeenAt,omitempty"`
}

func Open(path string) (*Cache, error) {
	validatedPath, err := validateDBPath(path)
	if err != nil {
		return nil, err
	}
	return openCacheDB(validatedPath, openDB)
}

func openCacheDB(path string, open func(string) (*sql.DB, error)) (*Cache, error) {
	db, err := open(path)
	if err != nil {
		return nil, fmt.Errorf("open cache database: %w", err)
	}

	lastHitCtx, lastHitCancel := context.WithCancel(context.Background())
	c := &Cache{
		db:                   db,
		currentYearFreshness: 24 * time.Hour,
		pastYearFreshness:    7 * 24 * time.Hour,
		pendingLastHits:      make(map[int]int64),
		lastHitWake:          make(chan struct{}, 1),
		lastHitCtx:           lastHitCtx,
		lastHitCancel:        lastHitCancel,
		lastHitDone:          make(chan struct{}),
	}
	c.lastHitDebounce.Store(int64(lastHitDebounceInterval))
	go c.runLastHitWorker()
	return c, nil
}

func validateDBPath(path string) (string, error) {
	if path == ":memory:" {
		return path, nil
	}
	cleaned, err := datapath.Validate(path)
	if err != nil {
		return "", fmt.Errorf("cache %w", err)
	}
	return cleaned, nil
}

// openDB opens the sqlite database file, applies connection pool settings and
// performance/recovery PRAGMAs, creates the schema, and runs a diagnostic read
// to trigger WAL auto-recovery after a crash.
func openDB(validatedPath string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", sqliteOpenName(validatedPath))
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

	if err := execDBWithRetry(context.Background(), db, `PRAGMA journal_mode=WAL`); err != nil {
		closeDBOnOpenError(db)
		return nil, fmt.Errorf("enable WAL: %w", err)
	}

	if err := execDBWithRetry(context.Background(), db, `PRAGMA busy_timeout=5000`); err != nil {
		closeDBOnOpenError(db)
		return nil, fmt.Errorf("set busy_timeout: %w", err)
	}

	if err := execDBWithRetry(context.Background(), db, `PRAGMA synchronous=NORMAL`); err != nil {
		closeDBOnOpenError(db)
		return nil, fmt.Errorf("set synchronous: %w", err)
	}

	if err := execDBWithRetry(context.Background(), db, `PRAGMA wal_autocheckpoint=1000`); err != nil {
		if isBusy(err) {
			closeDBOnOpenError(db)
			return nil, fmt.Errorf("set wal_autocheckpoint: %w", err)
		}
		// Non-critical — log and continue.
		slog.Warn("set wal_autocheckpoint failed", "type", "cache", "error", err)
	}

	if err := execDBWithRetry(context.Background(), db, `
		CREATE TABLE IF NOT EXISTS year_cache (
			year       INTEGER NOT NULL PRIMARY KEY,
			data       BLOB NOT NULL,
			fetched_at INTEGER NOT NULL,
			last_hit   INTEGER NOT NULL DEFAULT 0
		)
	`); err != nil {
		closeDBOnOpenError(db)
		return nil, fmt.Errorf("create year_cache table: %w", err)
	}

	if err := execDBWithRetry(context.Background(), db, `
		CREATE TABLE IF NOT EXISTS seen_mappings (
			tvdb_id      INTEGER NOT NULL,
			anilist_id   INTEGER NOT NULL,
			title        TEXT    NOT NULL,
			season       TEXT    NOT NULL,
			year         INTEGER NOT NULL,
			first_seen_at INTEGER NOT NULL,
			starts_at    TEXT    NOT NULL DEFAULT '',
			PRIMARY KEY (tvdb_id, season, year)
		)
	`); err != nil {
		closeDBOnOpenError(db)
		return nil, fmt.Errorf("create seen_mappings table: %w", err)
	}

	// Diagnostic read: triggers SQLite WAL auto-recovery (if the database
	// was left in an inconsistent state by a prior crash) and verifies the
	// database is accessible.
	var count int
	if err := retryBusy(context.Background(), nil, func() error {
		return db.QueryRow(`SELECT COUNT(*) FROM year_cache`).Scan(&count)
	}); err != nil {
		closeDBOnOpenError(db)
		return nil, fmt.Errorf("diagnostic read: %w", err)
	}

	// Force a WAL checkpoint to finalise any pending frames and shrink
	// the WAL file. Succeeds trivially on a fresh or clean database.
	if err := execDBWithRetry(context.Background(), db, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		if isBusy(err) {
			closeDBOnOpenError(db)
			return nil, fmt.Errorf("startup WAL checkpoint: %w", err)
		}
		slog.Warn("startup WAL checkpoint failed", "type", "cache", "error", err)
	}

	if err := db.Ping(); err != nil {
		closeDBOnOpenError(db)
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}

	return db, nil
}

func closeDBOnOpenError(db *sql.DB) {
	if err := db.Close(); err != nil {
		slog.Debug("close sqlite after open failure failed", "type", "cache", "error", err)
	}
}

func sqliteOpenName(path string) string {
	if path == ":memory:" {
		return path
	}
	u := url.URL{Scheme: "file", Path: path}
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "wal_autocheckpoint(1000)")
	u.RawQuery = q.Encode()
	return u.String()
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
// Each attempt also waits up to busy_timeout (5s) inside SQLite.
func (c *Cache) execWithRetry(ctx context.Context, query string, args ...any) error {
	_, err := c.execResultWithRetry(ctx, query, args...)
	return err
}

func (c *Cache) execResultWithRetry(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return retryBusyValue(ctx, c.retryHook, func() (sql.Result, error) {
		return c.db.ExecContext(ctx, query, args...)
	})
}

func execDBWithRetry(ctx context.Context, db *sql.DB, query string, args ...any) error {
	_, err := retryBusyValue(ctx, nil, func() (sql.Result, error) {
		return db.ExecContext(ctx, query, args...)
	})
	return err
}

func (c *Cache) queryRowWithRetry(ctx context.Context, scan func() error) error {
	return retryBusy(ctx, c.retryHook, scan)
}

func retryBusy(ctx context.Context, retryHook func(), fn func() error) error {
	_, err := retryBusyValue(ctx, retryHook, func() (struct{}, error) {
		return struct{}{}, fn()
	})
	return err
}

func retryBusyValue[T any](ctx context.Context, retryHook func(), fn func() (T, error)) (T, error) {
	var zero T
	var err error
	for attempt := 0; attempt < busyRetryAttempts; attempt++ {
		var value T
		value, err = fn()
		if err == nil {
			return value, nil
		}
		if !isBusy(err) {
			return zero, err
		}
		if retryHook != nil {
			retryHook()
		}
		if err := waitBeforeRetry(ctx, attempt); err != nil {
			return zero, err
		}
	}
	slog.Warn("sqlite busy retries exhausted", "type", "cache", "attempts", busyRetryAttempts, "error", err)
	return zero, err
}

func waitBeforeRetry(ctx context.Context, attempt int) error {
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
		return nil
	}
}

func (c *Cache) Close() error {
	c.closeOnce.Do(c.lastHitCancel)
	<-c.lastHitDone
	return c.db.Close()
}

func (c *Cache) GetYearContext(ctx context.Context, year int) (data []byte, fresh bool, ok bool, err error) {
	return c.readYearContext(ctx, year, true)
}

// PeekYearContext reads cached data and freshness without counting a hit or
// updating last_hit. Refresh coordination uses it to avoid treating background
// maintenance as user access.
func (c *Cache) PeekYearContext(ctx context.Context, year int) (data []byte, fresh bool, ok bool, err error) {
	return c.readYearContext(ctx, year, false)
}

func (c *Cache) readYearContext(ctx context.Context, year int, recordAccess bool) (data []byte, fresh bool, ok bool, err error) {
	var raw []byte
	var fetchedAt int64
	var lastHit int64

	err = c.queryRowWithRetry(ctx, func() error {
		return c.db.QueryRowContext(ctx,
			`SELECT data, fetched_at, last_hit FROM year_cache WHERE year=?`, year,
		).Scan(&raw, &fetchedAt, &lastHit)
	})

	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			if recordAccess {
				c.misses.Add(1)
			}
			return nil, false, false, nil
		}
		return nil, false, false, err
	}

	if recordAccess {
		c.hits.Add(1)
		c.queueLastHit(year, lastHit, time.Now().Unix())
	}

	freshnessThreshold := c.pastYearFreshness
	if year == time.Now().Year() {
		freshnessThreshold = c.currentYearFreshness
	}
	fresh = time.Since(time.Unix(fetchedAt, 0)) < freshnessThreshold
	return raw, fresh, true, nil
}

// SetYearContext starts an entry's retention window on insertion. Refreshing
// an existing year advances fetched_at but preserves its original last_hit.
func (c *Cache) SetYearContext(ctx context.Context, year int, data []byte) error {
	now := time.Now().Unix()
	return c.execWithRetry(ctx,
		`INSERT INTO year_cache (year, data, fetched_at, last_hit) VALUES (?, ?, ?, ?)
		 ON CONFLICT(year) DO UPDATE SET
			data = excluded.data,
			fetched_at = excluded.fetched_at,
			last_hit = CASE WHEN year_cache.last_hit > 0 THEN year_cache.last_hit ELSE year_cache.fetched_at END`,
		year, data, now, now,
	)
}

func (c *Cache) ClearContext(ctx context.Context) error {
	if err := c.execWithRetry(ctx, `DELETE FROM year_cache`); err != nil {
		return err
	}
	c.hits.Store(0)
	c.misses.Store(0)
	c.lastHitMu.Lock()
	c.lastHitTimes.Clear()
	clear(c.pendingLastHits)
	c.lastHitMu.Unlock()
	c.lastHitFailed.Clear()
	return nil
}

// HasYearsContext reports whether every requested year is present in the cache.
// It performs one read-only query and does not affect cache hit/miss statistics
// or last_hit timestamps.
func (c *Cache) HasYearsContext(ctx context.Context, years []int) (bool, error) {
	if len(years) == 0 {
		if err := c.db.PingContext(ctx); err != nil {
			return false, err
		}
		return true, nil
	}

	unique := make(map[int]struct{}, len(years))
	for _, year := range years {
		unique[year] = struct{}{}
	}

	placeholders := make([]string, 0, len(unique))
	args := make([]any, 0, len(unique))
	for year := range unique {
		placeholders = append(placeholders, "?")
		args = append(args, year)
	}

	var count int
	query := `SELECT COUNT(DISTINCT year) FROM year_cache WHERE year IN (` + strings.Join(placeholders, ",") + `)`
	if err := c.queryRowWithRetry(ctx, func() error {
		return c.db.QueryRowContext(ctx, query, args...).Scan(&count)
	}); err != nil {
		return false, err
	}
	return count == len(unique), nil
}

func (c *Cache) VacuumContext(ctx context.Context) error {
	return c.execWithRetry(ctx, "VACUUM")
}

func (c *Cache) NeedsRefreshYearsContext(ctx context.Context, currentYear int, currentRefreshDays, pastRefreshDays int) ([]int, error) {
	return retryBusyValue(ctx, c.retryHook, func() ([]int, error) {
		rows, err := c.db.QueryContext(ctx, `SELECT year, fetched_at FROM year_cache`)
		if err != nil {
			return nil, err
		}
		defer func() {
			if err := rows.Close(); err != nil {
				slog.Debug("close refresh rows failed", "type", "cache", "error", err)
			}
		}()

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
	})
}

func (c *Cache) PruneStaleYearsContext(ctx context.Context, days int) (int, error) {
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour).Unix()
	// Use fetched_at as a fallback when last_hit is 0 (e.g. entries created
	// before the column existed or after a failed last_hit UPDATE).
	// A cache hit may have returned before its optional last_hit write commits.
	// Keep those years until the worker persists the access (or the next prune).
	c.lastHitMu.Lock()
	var pendingYears []int
	for year, hit := range c.pendingLastHits {
		if hit >= cutoff {
			pendingYears = append(pendingYears, year)
		}
	}
	c.lastHitMu.Unlock()
	query := `DELETE FROM year_cache WHERE CASE WHEN last_hit > 0 THEN last_hit ELSE fetched_at END < ?`
	args := make([]any, 0, len(pendingYears)+1)
	args = append(args, cutoff)
	if len(pendingYears) > 0 {
		query += ` AND year NOT IN (` + strings.TrimSuffix(strings.Repeat("?,", len(pendingYears)), ",") + `)`
		for _, year := range pendingYears {
			args = append(args, year)
		}
	}
	result, err := c.execResultWithRetry(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

func (c *Cache) StatsContext(ctx context.Context) (CacheStats, error) {
	stats := CacheStats{Hits: c.hits.Load(), Misses: c.misses.Load()}
	if err := c.queryRowWithRetry(ctx, func() error {
		return c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM year_cache`).Scan(&stats.Entries)
	}); err != nil {
		return stats, err
	}
	return stats, nil
}

// SetLastHitDebounce sets the debounce interval for last_hit updates.
// Used in tests to control the debounce window.
func (c *Cache) SetLastHitDebounce(d time.Duration) {
	c.lastHitDebounce.Store(int64(d))
}

func (c *Cache) queueLastHit(year int, persisted, now int64) {
	c.lastHitMu.Lock()
	last := persisted
	if scheduled, ok := c.lastHitTimes.Load(year); ok {
		last = scheduled.(int64)
	} else {
		c.lastHitTimes.Store(year, persisted)
	}
	debounceSeconds := int64(time.Duration(c.lastHitDebounce.Load()).Seconds())
	if now-last < debounceSeconds {
		c.lastHitMu.Unlock()
		return
	}
	c.lastHitTimes.Store(year, now)
	c.pendingLastHits[year] = now
	c.lastHitMu.Unlock()

	select {
	case c.lastHitWake <- struct{}{}:
	default:
	}
}

func (c *Cache) runLastHitWorker() {
	defer close(c.lastHitDone)
	ticker := time.NewTicker(lastHitRetryInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.lastHitWake:
			c.flushLastHits(c.lastHitCtx)
		case <-ticker.C:
			c.flushLastHits(c.lastHitCtx)
		case <-c.lastHitCtx.Done():
			ctx, cancel := context.WithTimeout(context.Background(), lastHitWriteTimeout)
			c.flushLastHits(ctx)
			cancel()
			return
		}
	}
}

func (c *Cache) flushLastHits(parent context.Context) {
	c.lastHitMu.Lock()
	batch := make(map[int]int64, len(c.pendingLastHits))
	for year, timestamp := range c.pendingLastHits {
		batch[year] = timestamp
	}
	c.lastHitMu.Unlock()
	if len(batch) == 0 {
		return
	}

	var query strings.Builder
	query.WriteString(`UPDATE year_cache SET last_hit = MAX(last_hit, CASE year `)
	args := make([]any, 0, len(batch)*3)
	years := make([]int, 0, len(batch))
	for year, timestamp := range batch {
		query.WriteString(`WHEN ? THEN ? `)
		args = append(args, year, timestamp)
		years = append(years, year)
	}
	query.WriteString(`ELSE last_hit END) WHERE year IN (`)
	for i, year := range years {
		if i > 0 {
			query.WriteByte(',')
		}
		query.WriteByte('?')
		args = append(args, year)
	}
	query.WriteByte(')')

	ctx, cancel := context.WithTimeout(parent, lastHitWriteTimeout)
	// Keep this optional write's SQLite lock wait short, then restore the
	// normal timeout before returning the connection to the shared pool.
	conn, err := c.db.Conn(ctx)
	if err == nil {
		_, err = conn.ExecContext(ctx, fmt.Sprintf(`PRAGMA busy_timeout=%d`, lastHitBusyTimeoutMS))
		if err == nil {
			_, err = conn.ExecContext(ctx, query.String(), args...)
		}
		if _, resetErr := conn.ExecContext(context.Background(), `PRAGMA busy_timeout=5000`); err == nil {
			err = resetErr
		}
		if closeErr := conn.Close(); err == nil {
			err = closeErr
		}
	}
	cancel()
	if err != nil {
		for year := range batch {
			c.lastHitFailed.Store(year, true)
		}
		slog.Warn("failed to update last_hit", "type", "cache", "years", len(batch), "error", err)
		return
	}

	c.lastHitMu.Lock()
	for year, timestamp := range batch {
		if pending, ok := c.pendingLastHits[year]; ok && pending <= timestamp {
			delete(c.pendingLastHits, year)
		}
	}
	c.lastHitMu.Unlock()
	for year := range batch {
		if _, wasFailed := c.lastHitFailed.LoadAndDelete(year); wasFailed {
			slog.Info("last_hit update recovered", "type", "cache", "year", year)
		}
	}
}

// MarkSeenMappings records new resolved mappings in the seen_mappings
// table using INSERT OR IGNORE. Returns the subset of mappings that were
// actually inserted (i.e. were not already tracked).
func (c *Cache) MarkSeenMappings(ctx context.Context, mappings []SeenMapping) ([]SeenMapping, error) {
	if len(mappings) == 0 {
		return nil, nil
	}
	now := time.Now().Unix()
	newMappings, err := retryBusyValue(ctx, c.retryHook, func() ([]SeenMapping, error) {
		return c.markSeenMappingsOnce(ctx, mappings, now)
	})
	if err != nil {
		return nil, fmt.Errorf("mark seen mapping: %w", err)
	}
	return newMappings, nil
}

func (c *Cache) markSeenMappingsOnce(ctx context.Context, mappings []SeenMapping, now int64) ([]SeenMapping, error) {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	stmt, err := tx.PrepareContext(ctx,
		`INSERT OR IGNORE INTO seen_mappings (tvdb_id, anilist_id, title, season, year, first_seen_at, starts_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = stmt.Close() }()
	newMappings := make([]SeenMapping, 0, len(mappings))
	for _, m := range mappings {
		res, err := stmt.ExecContext(ctx,
			m.TVDBID, m.AniListID, m.Title, m.Season, m.Year, now, m.StartsAt,
		)
		if err != nil {
			return nil, err
		}
		n, _ := res.RowsAffected()
		if n > 0 {
			m.FirstSeenAt = now
			newMappings = append(newMappings, m)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	committed = true
	return newMappings, nil
}

// CountSeenMappings returns the total number of entries in the
// seen_mappings table. Used to detect whether tracking has ever been
// seeded (0 = first run).
func (c *Cache) CountSeenMappings(ctx context.Context) (int, error) {
	var count int
	if err := c.queryRowWithRetry(ctx, func() error {
		return c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM seen_mappings`).Scan(&count)
	}); err != nil {
		return 0, err
	}
	return count, nil
}
