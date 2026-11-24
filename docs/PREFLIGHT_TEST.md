# Preflight checks

Use `mise run docs` for documentation links. `mise run check` runs the source gate without containers; `mise run check-images` adds Docker builds and a build-context audit.

## Native release comparison

Run `mise run regression` on demand on the machine that submitted the PR, from a clean checkout of its current head. Compare against the latest published stable release before merging the candidate PR or approving the Release Please PR. Check the release tag and working commit in `results.json`; a result for an older head does not cover a new commit. This is a local procedure, not a CI check.

Both builds replay the previous calendar year, the preceding December and the same mapping bytes. The runner reuses a validated snapshot for 30 days across worktrees under `${XDG_CACHE_HOME:-$HOME/.cache}/sonarr-anime-bridge/native-regression/v1/`. Only a missing, expired or invalid snapshot requires a new capture. Each run retains its own results and replay inputs.

To replay offline, supply a snapshot and both binaries:

```sh
mise exec -- ./testdata/native-regression.sh --ref-tag v3.1.0 \
  --ref-binary /path/to/released-server --work-binary /path/to/working-server \
  --snapshot /path/to/snapshot --output /path/to/new-results
```

Inspect differences in ordered results, readiness, persistence and shutdown. For an intentional change from release behavior, exercise the new path directly and record its response and logs.

## Rootless runtime acceptance

`testdata/rootless-smoke.sh ENGINE IMAGE` exercises disposable rootless Docker or Podman. Both engines must accept the same candidate on amd64 and arm64 before promotion. Do not run acceptance against production storage.
