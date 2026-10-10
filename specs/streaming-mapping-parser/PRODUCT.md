# Product Spec: Streamline Mapping Parsing

Status: proposed; implementation is pending.

## Summary

Load changed mapping data with fewer temporary objects while producing the same
MAL/AniList-to-TVDB lookups. Improve startup and changed-mapping refresh costs
without changing list membership, mapping selection, or recovery behavior.

## Scope

This feature optimizes mapping JSON parsing in Go. The download/cache protocol,
mapping format, resolver publication, and refresh cadence remain unchanged.
It works independently of [mapping refresh reuse](../mapping-refresh-reuse/PRODUCT.md).

## Behavior

1. Every successfully parsed dataset produces the same MAL and AniList lookup
   tables as the existing parser. Namespaces remain separate, even when numeric
   IDs coincide. Lookup statistics and sorted key snapshots retain their meaning.

2. Valid positive `mal:` and `anilist:` source IDs are processed. Unrelated
   namespaces and invalid source IDs are ignored using existing rules. Ignored
   JSON is still consumed correctly so subsequent mapping entries remain readable.

3. For each source entry, only valid positive `tvdb_show:` targets with a scope
   participate. Season-one scope wins over other scopes; within the same
   preference group, the highest source-episode count wins, then the lowest
   TVDB ID. Input property order does not change this selection.

4. Episode-count semantics retain existing treatment of single episodes,
   inclusive closed ranges, open-ended ranges, null values, and duplicate keys.
   Invalid relevant ranges and unexpected relevant value types retain their
   existing failure behavior, including candidates that do not ultimately win.

5. Duplicate source and target descriptors preserve the existing parser's
   results and acceptance rules. The optimization does not add deduplication
   policy or make JSON property order affect otherwise tied candidates.

6. Empty mapping objects and entries with no usable TVDB target remain valid.
   Unsupported root shapes, malformed JSON, relevant malformed entries, and
   truncated compressed data keep their existing success/failure behavior.
   Any deliberate change in accepted inputs requires a separate behavior change.

7. Dataset metadata retains its existing informational role and logging.
   A metadata problem that currently allows usable entries to load must not
   become a fatal mapping error merely because parsing was optimized.

8. Compressed-download and decoded-data size limits remain enforced, including
   data in ignored fields. The parser processes the input stream without keeping
   the complete decompressed document or building a document-wide JSON tree.

9. Cancellation and reader errors do not publish partial lookup tables. Large
   ignored values can be traversed with cancellation checks. Existing loaded
   mappings and valid cached files remain available under current fallback rules.

10. The parser retains only final lookup tables and bounded per-entry work,
    subject to the existing input-size limit. It releases temporary entry data
    before proceeding to the next entry and preserves immutable publication.

11. No new environment variables, database changes, HTTP response fields,
    third-party parser requirements, or deployment changes are needed.
