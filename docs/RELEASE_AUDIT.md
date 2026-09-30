# Historical release audit

Snapshot: 2026-09-30. Repository: `calmcacil/sonarr-anime-bridge`.

## Scope and method

- All 58 published stable GitHub releases, from `v1.0.0` through `v2.14.7`, were compared in numeric version order. Each comparison uses the immediate published predecessor, not the release date or changelog claims.
- Public GHCR contained 109 tag references, including 54 exact stable-version image tags. The first available exact image was `v1.0.4`; `v1.0.0` through `v1.0.3` had no public exact-version image tag at this snapshot.
- The comparison uses `scripts/release_scope.py`: Git blobs and file modes for production Go source, modules, Dockerfile/build-context exclusions, licenses/notices, and the historical `entrypoint.sh`. Tests, automation, documentation, and release version metadata are excluded.
- All public tag-to-index digests were inspected for shared references. The five available source-unchanged images and their predecessors were also inspected for platform manifests, layers, runtime user, entrypoint, and version/revision labels. No historical rebuild was performed.

The GitHub release inventory was paginated. The public GHCR tag response had no
next page. Package-management REST APIs were unavailable with the current token's
permissions; public registry reads required no additional account scopes.

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

## Complete version comparison

Counts represent changed selected paths, including additions, deletions, and
mode changes, relative to the preceding row. They do not measure semantic changes
or image-layer differences.

| Version | Changed runtime/image inputs |
|---|---:|
| v1.0.0 | Initial release |
| v1.0.1 | 2 |
| v1.0.2 | 1 |
| v1.0.3 | 0 |
| v1.0.4 | 5 |
| v1.0.5 | 8 |
| v1.0.6 | 10 |
| v1.0.7 | 5 |
| v1.0.8 | 7 |
| v1.1.0 | 1 |
| v2.0.0 | 2 |
| v2.1.0 | 1 |
| v2.2.0 | 3 |
| v2.3.0 | 2 |
| v2.4.0 | 2 |
| v2.4.1 | 4 |
| v2.4.2 | 2 |
| v2.5.0 | 2 |
| v2.6.0 | 7 |
| v2.6.1 | 2 |
| v2.6.2 | 1 |
| v2.6.3 | 2 |
| v2.7.0 | 1 |
| v2.7.1 | 1 |
| v2.7.2 | 0 |
| v2.7.3 | 0 |
| v2.7.4 | 1 |
| v2.7.5 | 7 |
| v2.7.6 | 1 |
| v2.8.0 | 2 |
| v2.9.0 | 1 |
| v2.9.1 | 7 |
| v2.9.2 | 8 |
| v2.9.3 | 3 |
| v2.9.4 | 3 |
| v2.10.0 | 2 |
| v2.11.0 | 7 |
| v2.11.1 | 2 |
| v2.11.2 | 2 |
| v2.11.3 | 3 |
| v2.11.4 | 1 |
| v2.11.5 | 1 |
| v2.12.0 | 5 |
| v2.12.1 | 6 |
| v2.12.2 | 5 |
| v2.12.3 | 4 |
| v2.12.4 | 4 |
| v2.13.0 | 6 |
| v2.13.1 | 3 |
| v2.13.2 | 0 |
| v2.14.0 | 2 |
| v2.14.1 | 3 |
| v2.14.2 | 0 |
| v2.14.3 | 0 |
| v2.14.4 | 3 |
| v2.14.5 | 9 |
| v2.14.6 | 3 |
| v2.14.7 | 6 |

## Reproduce the source comparison

Fetch the published tags, then run from the repository root with authenticated
read access through `gh`:

```bash
git fetch origin --tags
PYTHONDONTWRITEBYTECODE=1 python3 - <<'PY'
import sys
sys.path.insert(0, "scripts")
from release_image import CANONICAL_REPOSITORY, command, stable_releases
from release_scope import inputs, changed_inputs
import json

records = command("gh", "api", "--paginate", "--jq", ".[] | @json",
                  f"repos/{CANONICAL_REPOSITORY}/releases?per_page=100").stdout
tags = stable_releases([json.loads(line) for line in records.splitlines()])
before = {}
for tag in tags:
    after = inputs(f"refs/tags/{tag}^{{commit}}")
    print(tag, changed_inputs(before, after))
    before = after
PY
```

Future releases will extend this output; the tables above remain a dated
snapshot. See [CI and releases](CI_RELEASES.md) for the active eligibility and
publication policy.
