# Product Spec: Reuse Decoded Year Data

Status: proposed; implementation is outside this change.

## Summary

Repeated Sonarr list requests should reuse previously decoded year data to reduce
CPU work and temporary allocations. Responses, persistent cache compatibility,
freshness rules, and operator controls must keep their existing behavior.

## Scope

This feature optimizes year-data reuse in the existing Go service. It does not
cache final list responses, change the mapping parser, introduce new settings,
or change the database format. SQLite remains the durable cache.

## Behavior

1. For identical stored data, configuration, mappings, and evaluation time,
   `/list` returns the same ordered results, titles, JSON shape, status codes,
   and headers as before. Methods, parameter validation, and resolver-unavailable
   responses retain their existing contracts.

2. After a year has been decoded and retained, subsequent requests using its
   unchanged stored data reuse that decoding. Retention is bounded; a year that
   was evicted can require decoding again without requiring an AniList fetch.

3. Reuse is independent of season and category. All supported season/category
   combinations use the full unfiltered year data. One request's filtering must
   never change the results of another request, including concurrent requests.

4. Each request applies current mappings, configured filters, and date-based
   eligibility rules. Reusing year data must not freeze TVDB resolution or future
   filtering at the time that data was first decoded.

5. Freshness retains the existing 24-hour threshold for the current year and
   seven-day threshold for other years. It is evaluated at access time, including
   across a change of calendar year. Reusing a decoding does not renew freshness.

6. A stale year remains immediately usable while its asynchronous refresh is
   scheduled. Failed fetches or failed persistent writes leave the previous
   valid stored data usable under the existing stale-data policy.

7. A missing primary year still triggers the existing synchronous, coordinated
   fetch. Fetch failure returns `[]` as before. Successfully fetched data becomes
   eligible for reuse only after it has been persisted successfully.

8. Winter requests retain the existing prior-year lookup and December merge.
   Available prior-year data is eligible for the same reuse. Missing or invalid
   prior-year data schedules background backfill without delaying the response.

9. Successful replacement of a stored year is visible to requests that start
   after the replacement finishes. A request already reading the earlier version
   may finish using that version; it must not combine partially replaced data
   within a single year's input.

10. A successful authorized `POST /cache/clear` removes eligibility to reuse all
    pre-clear year data. Requests starting after clear finishes follow the normal
    missing-data path unless a subsequent write has repopulated the year. Reads
    already underway may finish with their earlier data. Clear does not become a
    cancellation command for upstream fetches already underway.

11. Retention pruning removes reuse eligibility for deleted years. Access to
    retained decoded data still counts as user access for the existing 14-day
    pruning policy. Background maintenance and prewarm do not count as user hits.

12. Invalid stored JSON is never hidden by an earlier valid decoding. Recovery
    follows the existing primary-year and winter-backfill behavior. An empty
    array is a valid cached year; invalid, missing, and empty remain distinct.

13. Restart discards the memory optimization but retains the existing SQLite
    data. Startup still listens before prewarming. Configured years that are
    successfully prewarmed become eligible for reuse, subject to capacity; no
    additional upstream fetch is made solely to populate memory.

14. Cancellation while waiting for decoding must allow the canceled request to
    stop waiting. It must not corrupt retained data or prevent other requests
    from loading the year. Shutdown retains its existing bounded behavior.

15. `/cache/stats` retains its existing fields and persistent-entry meaning.
    Existing hit/miss counts, access tracking, debug authorization, request cache
    states, and health checks keep their meanings. A memory hit does not hide a
    SQLite read failure or make an unhealthy persistent cache appear healthy.

16. Retained decoded data has finite capacity. Exceeding capacity falls back to
    ordinary decoding and existing request behavior; it does not fail a request
    or discard persistent data. No deployment changes or new environment
    variables are required.
