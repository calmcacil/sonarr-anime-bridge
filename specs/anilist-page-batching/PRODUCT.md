# Product Spec: Batch AniList Year Fetches

Status: implemented and validated in [PR #144](https://github.com/calmcacil/sonarr-anime-bridge/pull/144),
including required hosted CI. Manual release review remains pending. This is required for the release batch tracked in
[TODO.md](../../TODO.md). Comparative evidence is recorded in [TECH.md](TECH.md).

## Summary

Retrieve the same complete AniList year data with fewer HTTP requests, while
respecting the current upstream rate limit. Preserve Sonarr list behavior and
require evidence that the change does not slow normal application use before
publishing the full build.

## Scope

Keep AniList as the metadata provider and retain the existing static mapping
source. This step covers year-fetch batching and shared upstream pacing. Changing
server-side filters, mapping refresh, Winter backfill, cache contents, or public
configuration is outside this step.

## Behavior

1. For identical upstream data, configuration, mappings, and evaluation time,
   `/list` returns the same ordered results, titles, duplicates, JSON shape,
   status codes, and headers for every supported season and category.

2. A successful year fetch returns every record and metadata field available
   through the existing year fetch, in the same order. All seasons and formats
   remain available from one stored year. Tags, relations, unknown values, and
   partial dates retain their meaning; batching does not discard or deduplicate
   records or introduce additional filters.

3. Multi-page years use fewer upstream HTTP requests. The default fetch can
   retrieve up to four consecutive pages per request. An empty year remains a
   successful empty dataset; years ending within a batch are complete without
   requiring an additional request solely to discover that they ended.

4. A year is available as newly fetched data only after the entire fetch and
   persistent write succeed. Missing, mismatched, malformed, or errored upstream
   responses never turn a partial fetch into a successful year. A failed refresh
   leaves previously valid cached data usable under the existing stale policy.

5. A primary cache miss continues to wait synchronously for its coordinated
   year fetch. A failed fetch still returns `[]` under the existing contract.
   Stale data returns immediately while refresh runs in the background. Warm
   cache hits do not contact AniList or wait for upstream rate-limit admission.

6. Existing 24-hour/current-year and seven-day/other-year freshness rules,
   pruning, cache statistics, clear behavior, restart persistence, and decoded
   data reuse remain. No cache migration or deployment change is required.

7. Winter retains its prior-year lookup, December merge, deduplication, and
   background backfill behavior. Both primary and prior-year fetches receive the
   improvement without introducing extra waits on available cached results.

8. Every AniList attempt in one service process, including retries, prewarm,
   user-triggered fetches, and scheduled refreshes, shares pacing that stays
   below 30 HTTP requests in any minute. A lower advertised upstream allowance
   further restricts pacing; a higher allowance does not increase this ceiling.
   Independent service processes do not coordinate their shared-IP usage.

9. When AniList reports an exhausted allowance or a rate-limit cooldown, later
   attempts in the same process respect that cooldown. A request already sent
   may finish. A canceled waiter does not reset the cooldown or prevent other
   callers from progressing. Upstream recovery never releases queued requests
   as an unpaced burst.

10. Retry attempts and waits remain finite and context-aware. Exhaustion,
    cancellation, shutdown, or the existing fetch deadline stops the fetch
    without publishing partial data. Existing same-year fetch coordination and
    failure cooldowns retain their behavior; distinct years share only the
    upstream budget, not their data or completion state.

11. Startup continues to listen before prewarm. Startup prewarm, cold `/list`
    requests, and background refresh remain within their existing timeout
    budgets. Larger upstream responses do not justify increasing these budgets
    or removing response-size and pagination bounds.

12. The change must preserve warm-request responsiveness and allocations and
    must not cause a meaningful slowdown of successful cold fetches or prewarm
    under the same upstream allowance and comparable conditions. Small years,
    multi-page years, and concurrent fetches are included. Performance criteria
    and their measurement
    method are defined in [TECH.md](TECH.md); fewer requests alone do not satisfy
    this requirement.

13. Operator diagnostics preserve their current task/outcome conventions and
    distinguish pages processed, HTTP attempts, retries, and pacing/cooldown
    waits. They must not expose full request/response bodies or sensitive URLs,
    and batching must not add expensive logging to warm requests.

14. The full build remains unreleased until this step is implemented, output
    equivalence and failure behavior are verified, and comparative performance
    evidence meets the release criteria. The tracker must not mark this step
    implemented on the strength of the exploratory live API test alone.
