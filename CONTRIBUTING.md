# Contributing

Use a short-lived branch from `main` and open a pull request with a Conventional
Commit title, such as `fix(cache): retry busy reads`. Pull requests are squash
merged, so the title determines semantic-release intent. Use `!` for breaking
changes and explain migration impact in the pull request body.

Run the local CI equivalent before submitting changes:

```bash
make check
```

This checks formatting and module consistency, runs vet, golangci-lint, race
tests, supported Linux builds, govulncheck, actionlint, and Docker builds for
`amd64` and `arm64`. It requires Go, Git, and Docker; tools invoked through Go
are version-pinned in the Makefile. Python 3 is used for the dependency-free
internal documentation-link check.

Run `./testdata/native-regression.sh` for filtering, season splitting, winter
overflow, resolution, sorting, or pipeline changes. Run the integration tests
documented in `docs/PREFLIGHT_TEST.md` for data-pipeline changes.

Release Please owns routine version and changelog updates. Do not manually edit
release versions or add routine changelog entries. See `docs/CI_RELEASES.md` for
the release, recovery, and rollback process.
