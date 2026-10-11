# sonarr-anime-bridge Container Flow

## Startup Sequence

```text
1. Container runtime
   ├─ starts distroless image as a non-root user
   ├─ runs /server directly
   └─ expects /data to be readable and writable by the configured runtime UID/GID

2. main.go run()
   ├─ load config (environment variables)
   ├─ verify cache/mapping parent directories are readable and writable
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
       │   └─ if cached and fresh: hydrate decoded reuse without user hits
       │   └─ else FetchAndStore(year) → AniList API → cache → hydrate committed year
```

## Request Handling: `/list?season=X&year=Y`

```text
1. Parse params (season, year, category)
2. GetDecodedYear(year): read SQLite, then reuse matching decoded bytes
   ├─ HIT  → fresh/ok → process pipeline
   └─ MISS → synchronous FetchAndStore(year)
      ├─ Inflight deduplication (channel wait if same year already fetching)
      ├─ AniList GraphQL fetch (four aliased pages/request, 50 results/page)
      │   ├─ validate all aliases; assemble ordered pages through terminal flag
      │   ├─ shared 2.3s admission gap, restricted by lower upstream allowances
      │   ├─ shared Retry-After/reset cooldown + overlapping exponential backoff
      │   └─ exponential backoff has jitter (+/-25%)
      └─ Store in cache (year_cache table)
3. Winter overflow check (if season=WINTER)
   └─ If year-1 cached: read SQLite/reuse decoding and merge only December starts
   └─ If missing/invalid: ScheduleYearFetch(year-1), without waiting
4. Process pipeline (applied in order)
   ├─ Merge winter overflow shows (if WINTER)
   ├─ FilterBySeason (skipped if season=ALL)
   ├─ FilterByFormats (configured formats)
   ├─ Filter (duration >10min, exclude tags)
   ├─ FilterFuture (3 months ahead, if FILTER_FUTURE_ENABLED=true)
   ├─ FilterFirstSeason (if category=series-new)
   └─ Resolve TVDB IDs against one immutable mapping snapshot (MAL/AniList → TVDB)
5. If data was stale (!fresh) → ScheduleYearFetch(year), sharing one pending/running task per year
6. Return JSON: [{tvdbId, title}, ...]
```

## Data Flow

```text
AniList GraphQL API → JSON → SQLite cache → bounded decoded-year reuse → Filter pipeline → TVDB resolution → HTTP JSON response
```

## Cache

- **Backend**: SQLite (WAL mode) at configured `CACHE_DB_PATH`
- **Table**: `year_cache(year PK, data BLOB, fetched_at INT, last_hit INT)`
- **Freshness thresholds**:
  - Current year: 24 hours
  - Past years: 7 days
- **Operations**:
  - `GetDecodedYearContext` → one SQL read, then decoded reuse; returns shows/fresh/present/decode error
  - `PeekDecodedYearContext` → same reuse without user hit/access accounting
  - `GetYearContext` → raw compatibility read
  - `SetYearContext` → UPSERT; invalidate decoding after successful write
  - `HasYearsContext` → read-only multi-year readiness
  - `ClearContext` → delete year rows and invalidate all decoding; in-flight fetches may repopulate
  - `Stats` → entries, hits, misses

Decoded reuse retains up to eight years and 32 MiB of accounted payload per cache
instance, with LRU eviction and oversized bypass. This is not a process RSS limit.
Writes, clear, prune and close fence admissions with a mutation generation.
Request arrays are copied; nested objects remain read-only. Freshness and storage
failures remain visible on every access. Restart drops only the memory layer.

## Rate Limiting (AniList Client)

- **Global per-process**: Single `anilist.Client` with `sync.Mutex`
- **Proactive throttle**: 2.3s minimum admission gap; at most 27 attempts per rolling minute
- **Lower allowances**: gap increases to `60s / (0.9 * advertised limit)` when needed
- **Shared cooldown**: later valid positive Retry-After or future reset; one-minute fallback on 429 or exhausted allowance
- **Wait bounds**: waits over two minutes fail; the shared upstream deadline is retained
- **Queued callers**: recheck cooldown and pacing before admission; cancellation preserves shared state, recovery stays paced
- **Exponential backoff**: 2s → 4s → 8s → 16s (+/-25% jitter), 5 attempts max
- **Inflight deduplication**: `scheduler.FetchAndStore` uses per-year channels

Pacing, retry backoff and cooldown deadlines overlap. Warm cache hits do not use
the AniList gate. Complete-year publication still follows a successful SQLite write.

## Shutdown

```text
SIGTERM/INT → cancel context → wait prewarm goroutine → server.Shutdown(10s)
          → wait scheduler background workers (5s)
```
