# sonarr-anime-bridge Container Flow

## Startup Sequence

```text
1. Entrypoint (entrypoint.sh)
   ├─ validates PUID/PGID are numeric
   ├─ rejects PUID/PGID 0 unless ALLOW_ROOT=1
   ├─ resolves/creates configured group and user only when missing
   ├─ validates and `chown -h` user-owned cache/mapping paths + parent directories under /data
   └─ exec su-exec "$PUID:$PGID" /server

2. main.go run()
   ├─ load config (environment variables)
   ├─ open SQLite cache
   ├─ create scheduler (holds anilist.Client + Cache)
   ├─ load anibridge mappings (/data/anibridge_mappings.json.zst)
   │   └─ downloaded from GitHub if missing/changed
   ├─ start background workers (stale refresh + mapping refresh)
   ├─ start HTTP server (LISTENS IMMEDIATELY)
   │   ├─ GET/HEAD /list        → handleList
   │   ├─ GET/HEAD /health      → handleHealth
   │   ├─ GET/HEAD /cache/stats → handleCacheStats
   │   └─ POST /cache/clear     → handleCacheClear
   └─ prewarm configured years in goroutine (runs after listener starts)
       ├─ for each PREWARM_YEARS
       │   └─ if cached and fresh: SKIP
       │   └─ else FetchAndStore(year) → AniList API → cache
```

## Request Handling: `/list?season=X&year=Y`

```text
1. Parse params (season, year, category)
2. GetYear(year) from cache
   ├─ HIT  → fresh/ok → process pipeline
   └─ MISS → synchronous FetchAndStore(year)
      ├─ Inflight deduplication (channel wait if same year already fetching)
      ├─ AniList GraphQL fetch (paginated, 50 results/page)
      │   ├─ 700ms throttle between pages
      │   ├─ Retry-After + exponential backoff on 429
      │   └─ exponential backoff has jitter (+/-25%)
      └─ Store in cache (year_cache table)
3. Winter overflow check (if season=WINTER)
   └─ If year-1 cached: read cache and merge only December starts
4. Process pipeline (applied in order)
   ├─ Merge winter overflow shows (if WINTER)
   ├─ FilterBySeason (skipped if season=ALL)
   ├─ FilterByFormats (configured formats)
   ├─ Filter (duration >10min, exclude tags)
   ├─ FilterFuture (3 months ahead, if FILTER_FUTURE_ENABLED=true)
   ├─ FilterFirstSeason (if category=series-new)
   └─ Resolve TVDB IDs via anibridge mapping (MAL/AniList → TVDB)
5. If data was stale (!fresh) → trigger async FetchAndStore(year)
6. Return JSON: [{tvdbId, title}, ...]
```

## Data Flow

```text
AniList GraphQL API → JSON → SQLite cache → Filter pipeline → TVDB resolution → HTTP JSON response
```

## Cache

- **Backend**: SQLite (WAL mode) at configured `CACHE_DB_PATH`
- **Table**: `year_cache(year PK, data BLOB, fetched_at INT, last_hit INT)`
- **Freshness thresholds**:
  - Current year: 24 hours
  - Past years: 7 days
- **Operations**:
  - `GetYear` → returns (data, fresh, ok)
  - `SetYear` → INSERT OR REPLACE
  - `HasYear` → existence check
  - `Clear` → truncate table
  - `Stats` → entries, hits, misses

## Rate Limiting (AniList Client)

- **Global per-process**: Single `anilist.Client` with `sync.Mutex`
- **Proactive throttle**: 700ms minimum gap between requests
- **Post-429 backoff**: 5s minimum gap for up to 30 seconds after any 429
- **Retry-After header**: respected (clamped when required)
- **Exponential backoff**: 2s → 4s → 8s → 16s (+/-25% jitter), 5 attempts max
- **Inflight deduplication**: `scheduler.FetchAndStore` uses per-year channels

## Shutdown

```text
SIGTERM/INT → cancel context → wait prewarm goroutine → server.Shutdown(10s)
          → wait scheduler background workers (5s)
```
