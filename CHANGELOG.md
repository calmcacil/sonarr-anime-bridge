# Changelog
All notable changes to this project are documented here.
The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
Releases are automated via the `publish.yml` workflow: each push to `main`
with conventional commits (`feat:`, `fix:`, `perf:`, etc.) triggers a
version bump and GitHub Release with auto-generated notes.


















## [2.11.4] — 2026-07-04

### Fixed
harden sqlite cache writes (#65)

## [2.11.3] — 2026-07-04

### Fixed
prevent stale publish releases (#63)

### Miscellaneous
bump the go-modules group with 2 updates (#62)
bump alpine from 3.21 to 3.24 in the docker-images group (#60)
bump the github-actions group with 4 updates (#61)

### Changed
harden workflow dependency management (#59)

## [2.11.2] — 2026-06-29

### Fixed
defer config logging until after logger setup (#57)

## [2.11.1] — 2026-06-29

### Fixed
add missing type fields to startup and config logs (#56)

## [2.11.0] — 2026-06-29

### Added
standardize structured log types across all packages (#55)

## [2.10.0] — 2026-06-29

### Added
log newly mapped shows per season/year context (#52) (#54)

### Documentation
align markdown with current service behavior (#49)

### Changed
optimize CI performance — concurrency, fast PR path, trusted publish, Docker caching (#50)

## [2.9.4] — 2026-06-28

### Fixed
address audit correctness findings (#48)

## [2.9.3] — 2026-06-28

### Fixed
resolve audit findings (#47)

## [2.9.2] — 2026-06-27

### Fixed
global refactor audit remediation — context, lifecycle, release, and test hardening (#44)

## [2.9.1] — 2026-06-21

### Fixed
harden cache lifecycle and request handling

### Documentation
update specs to match implementation
update specs to match implementation

## [2.9.0] — 2026-06-10

### Added
add pre-commit hooks with lint, build, test, markdownlint, and conventional commit enforcement
add pre-commit hooks with lint, build, test, markdownlint, and conventional commit enforcement

## [2.8.0] — 2026-06-10

### Added
add trigger context to year_cached log entries
add trigger context to year_cached log entries

## [2.7.6] — 2026-06-09

### Fixed
eliminate SQLITE_BUSY errors with debounced last_hit, retry, and startup recovery

## [2.7.5] — 2026-06-09

### Fixed
resolve verified issues from code review (issue #31)

## [2.7.4] — 2026-06-09

### Fixed
set PRAGMA busy_timeout to prevent SQLITE_BUSY on last_hit updates (#34)

## [2.7.3] — 2026-06-09

### Fixed
compute semver Docker tags from version output instead of tag event

## [2.7.2] — 2026-06-09

### Fixed
add major-version Docker tags and delete v0.1.0 GHCR image (#33)

### Documentation
fix issues found in review
add PREFLIGHT_TEST.md and REGRESSION_TESTS.md, update AGENTS.md (#32)
add container flow documentation (#30)

### Miscellaneous
clean up blank lines in CHANGELOG header

## [2.7.1] — 2026-06-08
### Fixed
start HTTP listener before prewarm (#29)
## [2.7.0] — 2026-06-08
### Added
skip prewarm when cache is fresh (#28)
## [2.6.3] — 2026-06-08
### Fixed
wait on in-flight fetch instead of returning immediately (#27)
## [2.6.2] — 2026-06-08
### Fixed
wait for backfill instead of returning empty response (#26)
skip CI publish for docs-only changes (#24)
## [2.6.1] — 2026-06-08
### Fixed
include winter overflow shows in season=ALL responses (#25)
### Documentation
sync specs to current year-cache architecture, simplify, fix stale references
## [2.6.0] — 2026-06-08
### Changed
year-cache with on-the-fly filtering and resolution (#22)
simplify cache API, remove dead code, add native regression test
## [2.5.0] — 2026-06-08
### Added
richer pipeline logging with per-stage counts and timing (#20)
## [2.4.2] — 2026-06-08
### Fixed
also check filter_future_enabled in GetWithVersion handler path (#19)
## [2.4.1] — 2026-06-08
### Fixed
refresh cached entries when filter_future_enabled changes (#18)
### Miscellaneous
fix stale sonarr-anilist-bridge references in User-Agent header
## [2.4.0] — 2026-06-08
### Added
add FILTER_FUTURE_ENABLED toggle to control 3-month-ahead filtering (#17)
## [2.3.0] — 2026-06-08
### Added
add cache robustness fixes — db.Ping, index, placeholder/JSON validation, and VACUUM (#16)
## [2.2.0] — 2026-06-08
### Added
cache uplift — AniList raw cache, mapping versioning, periodic stats, integration tests (#15)
## [2.1.0] — 2026-06-08
### Added
add cache entry count to startup logs (#12)
## [2.0.0] — 2026-06-08
### Changed
simplify config env vars, remove 8 rarely-needed flags (#11)
## [1.1.0] — 2026-06-08
### Added
enable ONA format by default (#9)
## [1.0.8] — 2026-06-08
### Miscellaneous
improve CI, testing, Docker, and code organization (#7)
## [1.0.7] — 2026-06-06
### Fixed
apply architecture review findings — concurrency hardening, shutdown lifecycle, config validation, entrypoint safety
## [1.0.6] — 2026-06-05
### Fixed
propagate fetch errors, add HEALTHCHECK, harden input validation
## [1.0.5] — 2026-06-05
### Fixed
resolve all code review findings (#4)
### Documentation
sync README and specs to current architecture
add project AGENTS.md with PR regression check instructions
## [1.0.4] — 2026-06-05
### Fixed
strip git log prefix and tags in CHANGELOG generator
### Miscellaneous
rename repo to sonarr-anime-bridge
## [1.0.3] — 2026-06-05
## [1.0.2] — 2026-06-05
## [1.0.0] — 2026-06-05
### Added
- **AniBridge data source** (`internal/mapping/anibridge.go`):
  Replaced `shinkro/community-mapping` (YAML, ~3,387 MAL→TVDB entries) with
  `anibridge/anibridge-mappings` (zstd-compressed JSON, ~8,900 MAL→TVDB +
  ~9,100 AniList→TVDB entries). Shows without a MAL cross-reference are now
  resolved via AniList fallback — recovering ~47 previously-unresolvable
  shows across 18 years of data.
- **Conditional HTTP caching**: The anibridge mapping is persisted in a
  sidecar metadata file with ETag/MD5 tracking. On startup or refresh, a
  HEAD request checks the upstream ETag — a `304 Not Modified` skips the
  ~8 MB download entirely.
- **Background mapping refresh**: The in-memory mapping is checked for
  updates every hour. When changes are detected, the mapping is swapped
  atomically (`sync/atomic.Pointer`) without blocking lookups. Each refresh
  logs a diff: `Updated anibridge database, 12 new, 3 removals, 18091 total`.
- **PUID/PGID support** (`entrypoint.sh`): Drops privileges at runtime
  using `su-exec`. The entrypoint creates a user/group matching the
  provided UID/GID (default `1000:1000`), chowns `/data`, and runs the
  server as that user.
- **Clean `docker-compose.yml`**: Removed hardcoded default overrides
  that would silently break in future years (e.g. `PREWARM_YEARS=2026`).
  Documented available env vars as commented examples.
### Changed
- **Go dependency**: `gopkg.in/yaml.v3` removed (no longer needed),
  `github.com/klauspost/compress/zstd` added for mapping decompression.
- **Config env vars**: New `ALG_ANIBRIDGE_MAPPING_PATH`,
  `ALG_ANIBRIDGE_REFRESH_DAYS`, `ALG_ANIBRIDGE_URL`.
- **NOTICE**: Updated attribution from `shinkro/community-mapping` to
  `anibridge/anibridge-mappings`.
### Removed
- `internal/mapping/community.go` — old shinkro YAML loader.
[1.0.0]: https://github.com/calmcacil/sonarr-anime-bridge/releases/tag/v1.0.0
[1.0.2]: https://github.com/calmcacil/sonarr-anime-bridge/releases/tag/v1.0.2
[1.0.3]: https://github.com/calmcacil/sonarr-anime-bridge/releases/tag/v1.0.3
[1.0.4]: https://github.com/calmcacil/sonarr-anime-bridge/releases/tag/v1.0.4
[1.0.5]: https://github.com/calmcacil/sonarr-anime-bridge/releases/tag/v1.0.5
[1.0.6]: https://github.com/calmcacil/sonarr-anime-bridge/releases/tag/v1.0.6
[1.0.7]: https://github.com/calmcacil/sonarr-anime-bridge/releases/tag/v1.0.7
[1.0.8]: https://github.com/calmcacil/sonarr-anime-bridge/releases/tag/v1.0.8
[1.1.0]: https://github.com/calmcacil/sonarr-anime-bridge/releases/tag/v1.1.0
[2.0.0]: https://github.com/calmcacil/sonarr-anime-bridge/releases/tag/v2.0.0
[2.1.0]: https://github.com/calmcacil/sonarr-anime-bridge/releases/tag/v2.1.0
[2.2.0]: https://github.com/calmcacil/sonarr-anime-bridge/releases/tag/v2.2.0
[2.3.0]: https://github.com/calmcacil/sonarr-anime-bridge/releases/tag/v2.3.0
[2.4.0]: https://github.com/calmcacil/sonarr-anime-bridge/releases/tag/v2.4.0
[2.4.1]: https://github.com/calmcacil/sonarr-anime-bridge/releases/tag/v2.4.1
[2.4.2]: https://github.com/calmcacil/sonarr-anime-bridge/releases/tag/v2.4.2
[2.5.0]: https://github.com/calmcacil/sonarr-anime-bridge/releases/tag/v2.5.0
[2.6.0]: https://github.com/calmcacil/sonarr-anime-bridge/releases/tag/v2.6.0
[2.6.1]: https://github.com/calmcacil/sonarr-anime-bridge/releases/tag/v2.6.1
[2.6.2]: https://github.com/calmcacil/sonarr-anime-bridge/releases/tag/v2.6.2
[2.6.3]: https://github.com/calmcacil/sonarr-anime-bridge/releases/tag/v2.6.3
[2.7.0]: https://github.com/calmcacil/sonarr-anime-bridge/releases/tag/v2.7.0
[2.7.1]: https://github.com/calmcacil/sonarr-anime-bridge/releases/tag/v2.7.1
[2.7.2]: https://github.com/calmcacil/sonarr-anime-bridge/releases/tag/v2.7.2
[2.7.3]: https://github.com/calmcacil/sonarr-anime-bridge/releases/tag/v2.7.3
[2.7.4]: https://github.com/calmcacil/sonarr-anime-bridge/releases/tag/v2.7.4
[2.7.5]: https://github.com/calmcacil/sonarr-anime-bridge/releases/tag/v2.7.5
[2.7.6]: https://github.com/calmcacil/sonarr-anime-bridge/releases/tag/v2.7.6
[2.8.0]: https://github.com/calmcacil/sonarr-anime-bridge/releases/tag/v2.8.0
[2.9.0]: https://github.com/calmcacil/sonarr-anime-bridge/releases/tag/v2.9.0
[2.9.1]: https://github.com/calmcacil/sonarr-anime-bridge/releases/tag/v2.9.1
[2.9.2]: https://github.com/calmcacil/sonarr-anime-bridge/releases/tag/v2.9.2
[2.9.3]: https://github.com/calmcacil/sonarr-anime-bridge/releases/tag/v2.9.3
[2.9.4]: https://github.com/calmcacil/sonarr-anime-bridge/releases/tag/v2.9.4
[2.10.0]: https://github.com/calmcacil/sonarr-anime-bridge/releases/tag/v2.10.0
[2.11.0]: https://github.com/calmcacil/sonarr-anime-bridge/releases/tag/v2.11.0
[2.11.1]: https://github.com/calmcacil/sonarr-anime-bridge/releases/tag/v2.11.1
[2.11.2]: https://github.com/calmcacil/sonarr-anime-bridge/releases/tag/v2.11.2
[2.11.3]: https://github.com/calmcacil/sonarr-anime-bridge/releases/tag/v2.11.3
[2.11.4]: https://github.com/calmcacil/sonarr-anime-bridge/releases/tag/v2.11.4
