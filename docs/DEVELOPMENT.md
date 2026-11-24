# Development

Install [mise](https://mise.jdx.dev/installing-mise.html) and GNU Make. Review `mise.toml`, then run `mise trust mise.toml` and `mise install` from the repository root. The file pins Go, Python, ShellCheck and GitHub CLI.

## Commands

| Command | Checks |
| --- | --- |
| `mise run check` | Formatting, modules, vet, lint, Linux builds, vulnerabilities, workflows, shell scripts, Python tooling checks, and documentation links. |
| `mise run check-images` | The source gate plus image builds, native version validation, and the build-context audit. |
| `mise run regression` | Release/working comparison with identical captured inputs. |
| `mise run docs` | Local documentation link validation. |

Docker with Buildx is needed for image checks, not the source gate. See [preflight](PREFLIGHT_TEST.md) for native replay. Preserve ignored benchmark evidence when switching worktrees.

Dependabot manages Go modules, Actions and Docker images. Renovate manages `mise.toml`. For a Go update, align the mise pin, `go.mod` directive and Docker builder tag/digest. The Makefile pins Go-based validation tools separately.
