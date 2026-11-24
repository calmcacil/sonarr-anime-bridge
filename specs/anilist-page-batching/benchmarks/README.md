# AniList batching measurements

Measured on 2026-10-11 using Go 1.27.2, Linux arm64, four ARM Neoverse-N1
vCPUs. [metadata.json](metadata.json) records the source and input fingerprints.
The candidate implementation is `9271eb8156f5f058be09967cf19d4d1af115d419`,
based on `89bfe4eae662e4564162760076f9cab067d2bb0f`.

The controlled transport compared original single-page pacing, compliant single-page pacing, and four-page batches. It used identical records and six samples per scenario; no live AniList requests were timed.

[Latency and request counts](latency-comparison.md) · [Warm benchstat](warm-benchstat.txt) · [Fetch benchstat](fetch-benchstat.txt) · [Native replay](native-regression.json).
