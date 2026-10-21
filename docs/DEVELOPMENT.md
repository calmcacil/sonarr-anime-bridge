# Development Setup

Install [mise](https://mise.jdx.dev/installing-mise.html) using its official
instructions. From the repository root, review `mise.toml`, then run:

```sh
command -v mise
mise --version
mise trust mise.toml
mise install
mise run check
```

The project pins Go, Python, ShellCheck, and GitHub CLI to exact versions.
Go matches the version in `go.mod`; Python meets the native regression runner's
3.12+ requirement. GitHub CLI is used to resolve releases for native regression.
The Makefile separately pins golangci-lint, actionlint, and govulncheck and runs
them through Go. CI continues using its existing setup actions and Makefile gates.

GNU Make and a C compiler for Go race tests must be available on the host.
Docker with Buildx is required only for explicit image checks; the ordinary
source gate does not need a container engine. mise does not provision engines,
host namespace mappings, or privileged rootless prerequisites.

## Commands

| Command | Checks |
| --- | --- |
| `mise run check` | Formatting, modules, vet, lint, race tests, Linux builds, vulnerabilities, workflows, shell scripts, Python regressions, and documentation links. |
| `mise run check-images` | The source gate plus image builds, native version validation, and the build-context audit. |
| `mise run regression` | Release/working comparison with identical captured inputs. |
| `mise run docs` | Local documentation link validation. |

Tasks delegate to the existing Makefile. For individual targets or CLI options,
use `mise exec -- make test` or, for example:

```sh
mise exec -- ./testdata/native-regression.sh --ref-tag v3.0.0
```

See [preflight tests](PREFLIGHT_TEST.md) for validation selection and regression
snapshot reuse. A fresh worktree does not contain ignored benchmark notes or
artifacts from another checkout. Preserve those files and consult their original
location before continuing benchmark work.

## Dependency Updates

[Dependabot](../.github/dependabot.yml) owns Go module, GitHub Actions, and
Docker image updates. Dependabot does not support mise configuration files;
[Renovate](https://docs.renovatebot.com/modules/manager/mise/) owns the tool
versions in `mise.toml` through [renovate.json](../renovate.json). Its enabled
manager and included paths restrict it to that file.

Renovate proposes updates on Mondays between 05:00 and 09:00 Europe/Oslo.
Minor and patch updates are grouped; major updates remain separate. Automatic
merging is disabled, so each proposal requires review and the normal CI checks.
The schedule defines an eligibility window, not a guaranteed job start time.
Dependabot's Docker image check is also weekly, on Monday at 06:00 Europe/Oslo.
A Go release or image published after that check waits for the next scheduled
check; an absent PR alone does not mean the updater failed. Inspect the latest
Dependabot Updates job log for the detected `golang` version and registry errors.

A standard-library security fix requires a Go toolchain update, not just a Go
module dependency bump. Coordinate `mise.toml`, the `go` directive in `go.mod`,
and the Docker builder tag and digest. The local check gate verifies these three
versions agree. For urgent fixes, update all three manually and run the security
scan rather than waiting for either updater's weekly schedule.

To activate updates, install the [Renovate GitHub App](https://github.com/apps/renovate)
and select this repository. The hosted Community plan is free for public and
private repositories; see [the current plans](https://docs.renovatebot.com/mend-hosted/overview/).
Configuration alone does not install or authorize the app. After merging this
configuration, verify the app's job logs and Dependency Dashboard show the four
mise tools and no duplicate module, Actions, or Docker proposals.

For every tool update, review release notes, run `mise install`, verify selected
versions with `mise ls`, and run `mise run check`. A Go update must also align
`go.mod` and the Docker builder version/digest; review related Dependabot PRs
together. The Makefile's three Go CLI pins and the CI golangci-lint version
remain separate maintenance inputs and must be updated together where applicable.

For a manual pin change, use `mise use TOOL@EXACT_VERSION` and review the diff.
To roll back a tool update, revert its pin change and reinstall the recorded
versions. This does not change application storage or deployed containers.
