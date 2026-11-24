# Repository guide

`main` is protected. Start a topic branch from the latest `origin/main`, commit and push only to that branch, and open a pull request against `main`. All changes, including docs, must land through a PR. Never commit or push directly to `main`, even if global instructions say otherwise.

Use the pinned `mise` tasks in the [development guide](docs/DEVELOPMENT.md). See the [request lifecycle](docs/flow.md) before changing fetch or cache behavior.

Release Please owns versions and `CHANGELOG.md`. When production build inputs change, update the Docker context and `scripts/release_scope.py` together. Keep release credentials out of PR workflows.
