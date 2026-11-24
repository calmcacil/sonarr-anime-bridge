# Preflight checks

Use `mise run docs` for documentation links. `mise run check` runs the source gate without containers; `mise run check-images` adds Docker builds and a build-context audit.

## Native release comparison

Run `mise run regression` for list, cache or mapping changes. Both binaries replay the same captured year, preceding December and mapping bytes. The default reference is the latest stable release; the default year is the previous calendar year. The runner retains the snapshot and comparison results. Use distinct output directories.

To replay offline, supply a snapshot and both binaries:

```sh
mise exec -- ./testdata/native-regression.sh --ref-tag v3.1.0 \
  --ref-binary /path/to/released-server --work-binary /path/to/working-server \
  --snapshot /path/to/snapshot --output /path/to/new-results
```

Inspect differences in ordered results, readiness, persistence and shutdown. For an intentional change from release behavior, exercise the new path directly and record its response and logs.

## Rootless runtime acceptance

`testdata/rootless-smoke.sh ENGINE IMAGE` exercises disposable rootless Docker or Podman. Both engines must accept the same candidate on amd64 and arm64 before promotion. Do not run acceptance against production storage.
