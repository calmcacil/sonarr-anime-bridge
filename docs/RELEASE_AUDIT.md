# Historical release audit

Snapshot: 2026-09-30. Compared 58 stable tags through `v2.14.7` against their immediate published predecessors using production input blobs and modes. This does not establish binary equality or describe later releases.

## Source-unchanged versions

These six releases have identical selected runtime/image inputs to their
predecessors. This is a source-input finding, not proof of identical binaries
or OCI artifacts.

| Release | Predecessor | Changes outside runtime/image inputs | GHCR references sharing its index |
|---|---|---|---|
| `v1.0.3` | `v1.0.2` | Publication workflow and changelog | No public exact-version image |
| `v2.7.2` | `v2.7.1` | CI/publication workflows, contributor instructions, docs, regression scripts, changelog | `v2.7.2`, `sha-b6b5774` |
| `v2.7.3` | `v2.7.2` | Publication workflow and changelog | `v2.7.3`, `sha-1d3d8d7` |
| `v2.13.2` | `v2.13.1` | Release manifest/configuration, changelog, docs | `v2.13.2`, `v2.13` |
| `v2.14.2` | `v2.14.1` | CI/security workflows, release manifest, changelog | `v2.14.2` |
| `v2.14.3` | `v2.14.2` | CI/security workflows, release manifest, changelog | `v2.14.3` |

All five available images have distinct index digests from their predecessors.
Each pair has the same Linux amd64/arm64 platform set, runtime user, entrypoint,
and first base layer per platform. Remaining layers differ. Version injection,
VCS/build metadata, OCI labels, and attestations can change artifacts without a
source-input change. Older Dockerfiles also used unpinned bases/toolchains, so
input equality alone cannot establish runtime-binary equality.

| Source-unchanged image | Observed OCI index digest |
|---|---|
| `v2.7.2` | `sha256:87544348fc320fabc38b9027aadbd3a9e3c4c305c58690f6f285d8ea9b4e398d` |
| `v2.7.3` | `sha256:013c67ecb2099843c5a415bf0755221bb4310be7451f50aebb09396b42403456` |
| `v2.13.2` | `sha256:a35f6de3915c133f84baebeb4534bbea96411ebd3158a8796f744b077ec95c19` |
| `v2.14.2` | `sha256:7d3faa61651c3366246406e6852b23da654b282335f29b61d39f85a2b55fadf2` |
| `v2.14.3` | `sha256:124c936c344d32193029c318e7c033379cd4bcb92505e8249f1a27fc3eea34d5` |

## Decision and limitations

Preserve historical GitHub releases, Git tags, GHCR images, and aliases. Deleting
these versions could break pinned installations, rollback references, shared
SHA tags, or the `v2.13` update track. None is established as a byte-identical
duplicate that can safely replace another artifact. No deletion or retagging
was performed.

The preventative release-eligibility gate stops future automation-only churn.
It does not retroactively change versions or remove non-runtime changelog entries
from a release that also contains application/image changes. Source comparison
is intentionally conservative: production comments/formatting still count,
and future embedded assets or new Docker inputs require policy updates.

The other 52 releases include the initial release and 51 versions with changed
selected inputs. In particular, `v2.14.6` is not a no-change build: `.dockerignore`,
`Dockerfile`, and `NOTICE` changed, including base/toolchain pinning and compliance
content. The `latest`, `v2`, and `v2.14` aliases all remained on `v2.14.7` at
`sha256:8e33004b3128761d6df7be122cffd256e229925ada07e1048e8511f8bb62fb19`.
