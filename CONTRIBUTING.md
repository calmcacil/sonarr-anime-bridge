# Contributing

Branch from `main` and open a pull request with a Conventional Commit title. Squash merges use the title for release notes; mark breaking changes with `!` and explain the migration.

Follow [development setup](docs/DEVELOPMENT.md). Run `mise run check` for the source gate; use [native comparison or image checks](docs/PREFLIGHT_TEST.md) when the change affects those paths. Include the commands and results in the PR.

Release Please owns version numbers and the changelog. Documentation or automation changes alone do not trigger a release.
