# Tech Spec: Container Builds

Status: implemented; validated in [PR #145](https://github.com/calmcacil/sonarr-anime-bridge/pull/145).

## Context

Inspected application baseline: `8ceaa50ea63104ad0f5cf1013933a53ff97a06ae`.

- [Dockerfile:5](https://github.com/calmcacil/sonarr-anime-bridge/blob/8ceaa50ea63104ad0f5cf1013933a53ff97a06ae/Dockerfile#L5)
  downloads modules before `COPY . .`, then compiles without `-trimpath`.
- [.dockerignore:1](https://github.com/calmcacil/sonarr-anime-bridge/blob/8ceaa50ea63104ad0f5cf1013933a53ff97a06ae/.dockerignore#L1)
  uses a denylist that omits local credentials, appdata, and benchmark directories.
  The inspected developer checkout contains about 1.57 GiB of benchmark files
  not covered by those rules. These files are not committed application inputs.
- [Makefile:46](https://github.com/calmcacil/sonarr-anime-bridge/blob/8ceaa50ea63104ad0f5cf1013933a53ff97a06ae/Makefile#L46)
  builds host, amd64, and arm64 images, duplicating the supported host target.
- [Publisher:115](https://github.com/calmcacil/sonarr-anime-bridge/blob/8ceaa50ea63104ad0f5cf1013933a53ff97a06ae/.github/workflows/publish.yml#L115)
  already cross-compiles without QEMU, exports GHA layer caches, and preserves
  the exact tested multi-platform index through promotion.
- [Release inputs:10](https://github.com/calmcacil/sonarr-anime-bridge/blob/8ceaa50ea63104ad0f5cf1013933a53ff97a06ae/scripts/release_scope.py#L10)
  identifies runtime Go files and explicit image inputs. Keep this inventory
  aligned with the context; no embedded application assets currently exist.

## Proposed changes

### Explicit input inventory

Change `.dockerignore` to exclude everything by default. Re-include only
`Dockerfile`, `.dockerignore`, `go.mod`, `go.sum`, `LICENSE`, `NOTICE`,
and production `*.go` files under `cmd/` and `internal/`. Re-include parent
directories so nested source is traversable; exclude `*_test.go` and
`internal/testutil/` after source inclusion. Do not allow entire source
directories without filename restrictions: a local key inside `internal/`
must remain excluded.

Replace `COPY . .` with explicit manifest, source-directory, and notice copies.
The two controls have different roles: context filtering prevents transfer,
while explicit COPY statements describe compiler/package inputs. Keep dependency
download before source copies so ordinary edits do not invalidate that layer.
Changing files inside the allowed source set still invalidates compilation.

Add a meaningful context regression harness under `testdata/`. It exports a
scratch audit image containing the filtered context from a disposable checkout,
using a separate temporary audit Dockerfile. Assert the complete exported file
inventory, not merely the final runtime image. Add harmless sentinel credentials,
databases, binaries, fixtures, and benchmark files both at the root and inside a
source directory; assert none appear. Never inspect or copy actual private files.
Assert nested production Go files are included.

Update `scripts/release_scope.py` and its tests only if the declared runtime
inventory changes. Add the new regression harness to `scripts/ci_scope.py`'s
registry-sensitive inputs and to workflow/ShellCheck validation. Future embedded
assets require an explicit allowlist and release-input change.

### Incremental compilation and consistent binaries

Keep the pinned native-platform builder and `CGO_ENABLED=0` cross-compilation.
Add a BuildKit cache mount for `/root/.cache/go-build` to the compile RUN, with
an explicit target-aware cache ID and concurrency policy. Go's own cache keys
also include compiler inputs; never cache `/server` as the output to be reused.
Retain `go mod download` as a regular cached layer, rather than putting its
only downloaded module contents in a non-exported mount.

Compile with `-trimpath` and `-buildvcs=false`, preserving stripped linker
flags and version injection. Explicit VCS behavior matches the filtered context,
which intentionally has no Git metadata. Use the same flags in the local binary
build target. No change to the distroless base, UID, entrypoint, /data volume,
healthcheck, notices, or supported image platforms is needed.

GHA's layer-cache export does not export cache-mount contents. Initially retain
the existing per-workload/platform GHA layer caches and use compilation mounts
for retained local builders. Record this limit in build documentation. Do not
add a cache-export helper/action merely to claim CI savings. If measurements
justify CI mount persistence later, specify its pinned tooling, architecture and
trust-separated keys, restore/save order, permissions, and cold-cache fallback
before enabling it. Such persistence is optional for this work.

### Local checks and CI integration

Refactor `docker-build` to require host architecture amd64 or arm64, build each
target exactly once with VERSION=local, and run `--version` from the existing
native target tag. Use explicit platform arguments and BuildKit. A host outside
the supported set gets a clear diagnostic rather than a mislabeled executable.
Keep the current public `make check` source gate and explicit `make check-images` container gate, both cross-builds, and the native version check.

Add the context audit to the existing native container validation; do not build
a second application candidate to test it. Continue using the current candidate
for smoke, scans, and runtime checks. Release validation remains by exact digest.
Document fresh-builder and retained-builder commands in README and preflight
docs, including VERSION and platform selection.

## Testing and validation

| Product invariants | Required evidence |
| --- | --- |
| 1-4 | Audit export has exactly declared inputs; dirty-checkout sentinels are absent, including inside source directories; nested runtime Go files are present. Excluded-file edits leave COPY/compile layers cached. |
| 4, 11 | Production source and module edits invalidate the relevant build steps; compiler/download failures return nonzero. Release-input tests cover additions if the inventory changes. |
| 5-7 | Same-builder cold, unchanged, and source-edited builds; compiler-cache reuse with correct new output; clean-builder build with no restored cache; both target architectures; consistent flags and correct version. |
| 8 | Local command transcript proves one application build per architecture and a successful native version run; unsupported hosts fail explicitly. |
| 9, 10 | Existing native image checks and final-image/config inspection; license files and UID/entrypoint/volume/healthcheck remain; disposable-registry promotion retains digest and attestations. |
| 12 | Record filtered context bytes and comparable cold/warm/edit timings, builder/tool versions, cache hit evidence, and baseline revision. Do not impose noisy elapsed-time thresholds in CI. |

Run the context audit with BuildKit, focused automation tests, `make docs`,
workflow validation, and `make check`. Update
[rootless deployment](../rootless-deployment/TECH.md) in the same change if
runtime validation entrypoints change. Preserve the ignored local benchmark
artifacts; correcting the context does not require deleting them.

## Risks and rollout

An overly narrow allowlist can omit future assets; review the input inventory
with every new embedded/generated runtime input. Previously exported dirty
builder caches can retain excluded data: document disposable-builder replacement
or targeted cache removal after reviewing cache ownership. Do not prune shared
builders or delete local benchmark evidence automatically.

These changes do not require a runtime identity or storage migration. Keep release
version/changelog changes under Release Please and existing release policy.

## Parallelization

Use one implementation agent for this spec. Context rules, COPY operations,
cache behavior, and local/CI checks should be reviewed together. This work may
land before rootless deployment; avoid concurrent edits to the shared Dockerfile,
Makefile, and CI workflow without an explicit integration owner.

## Implementation Evidence

Implementation baseline: `e02c4a7` (current remote main). The baseline now has
separate `make check` and `make check-images` gates; this change preserves that
contract. `testdata/container-build-audit.sh` exports and verifies the exact
BuildKit context, then records cold/warm/edit measurements and compiler reuse
on a dedicated builder. Native CI runs it for both architectures and writes
measurements into the job summary. Local Docker socket access is denied, so
container evidence comes from the native CI run below; it is not a successful local skip.

No new runtime input class was introduced; the existing release policy already
selects the declared production inventory. New automation is registry-sensitive.

### Native acceptance (2026-10-11)

[CI run 38148388157](https://github.com/calmcacil/sonarr-anime-bridge/actions/runs/38148388157)
passes all 13 checks, including Required, both native images, runtime smoke,
scans, and disposable-registry digest/attestation promotion. Validated source
head: `2f5f6d7`; CI synthetic merge: `1a6bd69f5f47fa0c57a11e8c1fced02142b35b71`.
The exact audit inventory is 149,914 bytes, including its synthetic nested Go
file; all undeclared sentinel files and tracked test fixtures are absent.
The baseline developer checkout had approximately 1.57 GiB of undeclared local
benchmark inputs. No baseline build timing was inferred from that size.

| Native target | Cold | Unchanged | Excluded edits | Source edit | Module edit | Cold/edit compiler invocations |
| --- | --- | --- | --- | --- | --- | --- |
| amd64 | 30.277 s | 0.453 s | 0.210 s | 1.634 s | 5.021 s | 238 / 1 |
| arm64 | 24.755 s | 0.216 s | 0.199 s | 1.175 s | 3.806 s | 236 / 1 |

Each row uses one dedicated initially empty builder and the same pinned Go
1.27.2 toolchain. Unchanged/excluded builds reuse the compilation layer; real
source changes execute compilation, produce the expected changed version, and
reuse dependency compilation. Module edits re-execute download and compile
steps. Deliberately invalid Go and module inputs return nonzero on both targets.
These are host-specific observations, not comparisons across architectures or
machine-independent speed promises. The local command regression verifies the
public image entrypoint's two builds and native reuse independently.
