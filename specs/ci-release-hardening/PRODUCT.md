# CI and release hardening

## Summary

Fix the reliability, security, and repeated work identified in the GitHub Actions
review. Preserve the release trust boundary and the stable `Required` check.
Service behavior, versioning, changelog generation, and manual merge ownership
do not change.

## Behavior

1. Every pull request, merge-group candidate, and main push receives the complete
   validation gate. Failed, cancelled, missing, or unexpectedly skipped checks
   cannot produce a successful `Required` result.
2. PR title changes receive an independent Conventional Commit check, including
   title-only edits. Title validation cannot replace the full validation gate.
   All commit types retained by Release Please are accepted.
3. Security analysis covers production Go code without repeating unrelated
   tests, linters, or container builds. High/critical CodeQL security findings
   and error-level findings block merging, not just failed analysis uploads.
4. Validation builds each supported container once, then tests and scans that
   same image. Both Linux amd64 and arm64 retain the non-root runtime, correct
   version, healthy writable-data startup, and early unwritable-data failure.
5. Publication accepts only an existing published stable release in the canonical
   repository whose exact source commit is merged into main. Manual recovery
   identifies the released source even when main has advanced. Release App
   credentials remain confined to the coordinator.
6. A new release's exact image index and both platform images pass source,
   version, runtime, and vulnerability validation before receiving full-version
   or convenience tags. Provenance and SBOMs survive promotion unchanged.
7. Existing full-version images are never replaced. Recovery validates the
   existing digest again. Registry authentication, network, or malformed-image
   failures must not be treated as absence.
8. Mutable aliases only advance within their own stable version track: latest
   selects the highest stable release, major the highest in that major, and
   minor the highest patch in that major/minor. Recovering an older release
   cannot downgrade another track. Publications are serialized and pending
   publications queue rather than replacing each other.
9. Weekly security checks explicitly scan current Go dependencies and the exact
   distributed current stable image on both architectures. They do not rebuild
   main as a substitute for scanning the release.
10. Build caches retain workload-specific artifacts and refresh with source
    changes. Shared dependency downloads do not make one job's build cache
    replace the race-test, cross-build, or security-analysis cache.
11. Release tags cannot be updated or deleted, and new GitHub Releases are
    immutable. Existing main protections and no-bypass policy remain in place.

## Scope

The current highest stable version is the security-maintained release; older
exact versions remain rollback references, not automatically rebuilt artifacts.
Verification uses disposable registry resources and a review PR. This change
does not merge the PR, create a release, run the production publisher, or alter
existing GHCR release digests.
