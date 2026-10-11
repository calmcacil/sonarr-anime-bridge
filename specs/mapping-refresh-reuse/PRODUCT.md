# Product Spec: Reuse Unchanged Mappings

Status: proposed; implementation is pending.

## Summary

When a mapping refresh confirms that the source has not changed, keep using the
already parsed mapping. Avoid repeated decompression and JSON parsing while
preserving update detection, startup recovery, and the existing health contract.

## Scope

This feature changes mapping refresh reuse in the Go service. It does not change
TVDB selection, the mapping format, refresh cadence, request filtering, or the
parser itself. It requires no new settings or persistent metadata migration.

## Behavior

1. Startup with no loaded mapping still validates and parses a usable source
   before reporting the resolver as loaded. A persisted ETag alone cannot make
   an unloaded resolver ready. Restarts do not retain parsed mappings in memory.

2. A refresh that confirms the same source content retains the loaded mapping
   without decompressing or parsing it again. Checking the source may still
   require reading compressed bytes or downloading them when freshness metadata
   is unavailable. Reuse does not imply an absence of network or disk work.

3. Reuse applies only to the configured mapping path and URL that supplied the
   loaded mapping. A changed URL does not reuse the old source as a successful
   refresh, even if its cached ETag resembles the new source's ETag.

4. Local mapping changes are checked even when the remote ETag is unchanged.
   Replacing the file, including with same-size content and preserved timestamps,
   cannot silently reuse the previous decoding. A changed valid local file is
   parsed according to the existing cache-selection policy. A concurrent file
   change can be observed on the next refresh, but partial data is never published.

5. A missing, unreadable, or corrupt local cache follows existing download and
   fallback behavior. A loaded mapping may remain usable after a failed refresh,
   but the failure is not reported as successful verification of an unchanged
   source. Missing or invalid sidecar metadata does not prove content identity.

6. A valid changed download is parsed completely and persisted successfully
   before replacing the active mapping. Invalid downloads and failed persistent
   writes leave the previous active mapping usable. Invalid downloads do not
   overwrite a previously usable cached mapping.

7. Successfully downloaded bytes identical to the verified loaded source can
   reuse its parsing, including when no remote checksum header is present.
   Existing upstream checksum verification still occurs when a header is present.

8. Network failures, HEAD-to-GET fallback, local-cache fallback, URL/redirect
   restrictions, download limits, and request timeouts retain their existing
   policies. Memory reuse must not conceal a failure or weaken validation.

9. Reuse does not renew AniList cache freshness or change `/list`, `/health`, or
   debug endpoint schemas. An existing loaded resolver remains healthy through
   refresh failures; an unloaded resolver remains degraded until loading succeeds.

10. Mapping readers continue using a complete immutable mapping during checks
    and replacement. Refresh does not block lookups. Cancellation and shutdown
    do not publish a partial mapping or leave future refresh attempts blocked.

11. Existing daily refresh and one-minute unloaded-resolver retry cadences remain.
    Logs distinguish verified reuse, changed mapping load, and failure/fallback
    while retaining existing task and outcome conventions and safe URL handling.

12. Only one current source's parsed state is retained for reuse per loader.
    Changing sources must not accumulate a historical mapping cache. Old mappings
    remain alive only as long as existing readers or the active resolver need them.
