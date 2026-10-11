# Tech Spec: Rootless Deployment

Status: implemented; validated in [PR #147](https://github.com/calmcacil/sonarr-anime-bridge/pull/147).

## Context

Inspected application baseline: `8ceaa50ea63104ad0f5cf1013933a53ff97a06ae`.
Inspected media-stack baseline: `e962a65c0525d99b1a5ecda32d3cef2a04af6c47`.

- [Image:18](https://github.com/calmcacil/sonarr-anime-bridge/blob/8ceaa50ea63104ad0f5cf1013933a53ff97a06ae/Dockerfile#L18)
  defaults to distroless UID 65532 and a 65532-owned /data directory.
- [Compose:5](https://github.com/calmcacil/sonarr-anime-bridge/blob/8ceaa50ea63104ad0f5cf1013933a53ff97a06ae/docker-compose.yml#L5)
  overrides identity to PUID/PGID 1000, uses a short bind mount, and omits
  filesystem hardening, restart policy, and an explicit stop grace period.
- [Runtime smoke:122](https://github.com/calmcacil/sonarr-anime-bridge/blob/8ceaa50ea63104ad0f5cf1013933a53ff97a06ae/testdata/runtime-smoke.sh#L122)
  chowns to 65532 with sudo fallback and uses host networking. This verifies
  non-root application startup, but does not establish rootless-engine support.
- [Lifecycle:151](https://github.com/calmcacil/sonarr-anime-bridge/blob/8ceaa50ea63104ad0f5cf1013933a53ff97a06ae/cmd/server/main.go#L151)
  registers signals after initial resolver load, waits for prewarm before a
  10-second HTTP drain, then waits up to five seconds for scheduler workers.
  [Cache close:309](https://github.com/calmcacil/sonarr-anime-bridge/blob/8ceaa50ea63104ad0f5cf1013933a53ff97a06ae/internal/cache/cache.go#L309)
  also joins its last-hit worker before closing SQLite.
- [Production Quadlet:19](https://github.com/calmcacil/compose/blob/e962a65c0525d99b1a5ecda32d3cef2a04af6c47/quadlet/sonarr-anime-bridge.container#L19)
  sets User=0:0 under rootless Podman, already drops capabilities and enables
  no-new-privileges, uses internal networking and registry auto-updates, and
  has systemd Restart=always. Container root maps to host media, not host root.

## Proposed changes

### Recipes and ownership

Keep the image default at 65532:65532 and preserve its /data volume declaration.
Do not add root startup, chown, a shell, or PUID parsing to the application.

Maintain three explicitly labeled recipes:

| Recipe | Identity and storage |
| --- | --- |
| Existing docker-compose.yml, conventional Docker | Retain PUID/PGID substitution and existing APPDATA_DIR default. Use a long bind mount with create_host_path=false; prepare a new directory with the chosen ownership before startup. |
| examples/docker-compose.rootless.yml | Use a specifically named Docker volume, omit user overrides, and retain image UID 65532. Verify first-use volume initialization and subsequent reuse under a real rootless daemon. |
| examples/sonarr-anime-bridge.container | Use User=65532:65532 and UserNS=keep-id:uid=65532,gid=65532 with a private host-owned /data bind. Provide a portable Quadlet example with documented network and installation requirements. |

Do not change the existing Compose mount defaults to a named volume: that would
silently replace existing storage with an empty dataset. Named-volume conversion
is an explicit operator migration. Docker namespace mappings differ from Podman
keep-id; do not copy host UID values or Podman arguments into Docker instructions.

The production media-stack follow-up changes its native Quadlet through that
repository's policy, preserving host media ownership, GroupAdd=keep-groups,
internal network, environment-file inventory, target membership, and AutoUpdate.
Host UID/GID 1002 maps to container 65532:65532 with keep-id. No common PUID/PGID
environment-file change is necessary because the bridge ignores those keys.
The portable example is reference material, not a second production unit tree.

### Filesystem restrictions and lifecycle settings

Both Compose recipes set read_only=true, cap_drop=[ALL],
security_opt=[no-new-privileges:true], restart=unless-stopped, and
stop_grace_period=30s. Add a bounded /tmp tmpfs only where runtime tests establish
scratch requirements; use nosuid,nodev and mode 1777. /data remains writable.
Quadlet uses ReadOnly=true, DropCapability=ALL, NoNewPrivileges=true, a 30-second
container stop timeout (portable `PodmanArgs=--stop-timeout=30`), and a larger systemd TimeoutStopSec (45 seconds) so the
supervisor does not kill Podman before the container stop completes.
Retain production Restart=always; explicit systemd stop suppresses restart.

Keep default network exposure unchanged for existing Compose users. Rootless
tests use private bridge networks and ephemeral loopback-published ports. No
recipe adds an engine socket or host networking. Health uses /server --healthcheck;
retain existing health timing and degraded-resolver behavior. Docker restart
policies act on process exit, not unhealthy health state.

Audit shutdown before relying on the longer grace. Register cancellation before
initial resolver loading, propagate it through startup and prewarm, and account
for HTTP drain, scheduler waits, and cache close. Use controlled cancellation
tests to find unbounded waits; make the smallest lifecycle changes needed to stop
within 30 seconds, including during startup. Preserve the 10-second HTTP drain
and five-second scheduler limits where they fit. A deadline failure must be
logged and return failure, not be reported as clean shutdown. Do not detach
storage cleanup and then claim persistence succeeded.

### Deterministic runtime validation

Refactor testdata/runtime-smoke.sh into an engine-selectable harness plus
testdata/rootless-smoke.sh for actual engine setup/verification. Keep support for
the existing rootful Docker baseline; its host chown fixture may remain confined
to that explicit scenario. Rootless runtime scenarios require neither host sudo
nor recursive ownership changes. CI may use runner privileges to install engine
packages and configure subuid/subgid, but must run the tested daemon/containers
as the ordinary runner account and assert rootless state.

Provision pinned/documented Docker-rootless and Podman tooling on both native
runner architectures. Use isolated sockets, engine state, storage, names, and
networks. Do not reset, prune, stop, or change the host's default engine. Cleanup
removes only test-owned resources; collect redacted logs and exit status on failure.
Verify Docker's rootless SecurityOptions and Podman's rootless flag, mapped
identity, mount ownership/access, read-only root, no-new-privileges, and dropped
capabilities.

CI builds the application candidate once per architecture. Import those same
bytes into rootless Docker and Podman (verify config/layer identity when archive
transport loses the original registry reference). Publication tests pull the
actual platform digest into each engine and verify its digest; never rebuild it.
Run the two engine scenarios sequentially inside the existing per-architecture
container/validate jobs so the Required aggregation and promotion dependency
already fail closed. Increase documented job timeouts if measured setup needs it.

Use the original candidate plus a mounted test-only Go helper in a disposable
container owning a private network namespace and ephemeral published application
port. Serve mapping fixtures on loopback
inside that namespace, then join the application with container-network sharing.
The existing ALLOW_INSECURE_MAPPING_URL=1 test switch permits loopback only;
do not broaden the allowlist to arbitrary fixture hostnames or add insecure
settings to production recipes. A sidecar shares only the isolated test namespace,
not host networking.

Seed disposable /data with a fresh year-cache fixture through a small test helper
using the actual cache package; exclude the helper from production image inputs.
Match its year and mapping IDs to a fixed /list result and configured prewarm,
so no live AniList call is needed for the successful runtime scenario. Initial
mapping download still exercises writable /data and metadata. Recreate without
the fixture server to prove persisted-mapping fallback. Test cache writes and
reopening as well as file existence; do not compare changing metadata bytewise.

Use Go tests with existing injectable AniList clients and controlled handlers for
stalled mapping/prewarm/request cancellation, rather than adding a production
AniList endpoint override. A separate runtime upstream-outage scenario proves
degraded startup and bounded stop. Test negative mounts, crash restart using a
signal supported without added capabilities, explicit stop without restart,
same-mount recreation, and SIGTERM exit without SIGKILL.

### Integration and migration documentation

Update README, docs/OPERATIONS.md, docs/PREFLIGHT_TEST.md, docs/CI_RELEASES.md,
AGENTS.md, and affected service-baseline specs to distinguish supported recipes
and evidence. Replace recursive-chown guidance with new-directory provisioning
and explicit review of existing identity/mount access. Existing /data schema,
image UID, image tags, URLs, port defaults, and application env contracts remain.

Add new harnesses/helpers to Makefile checks and the CI scope policy's
registry-sensitive paths; ensure release eligibility excludes test-only helpers.
Production changes are a separate media-stack commit/PR and its controlled
delivery process, with old/new image references and storage recorded before
activation. Do not mark live deployment accepted from disposable CI evidence.

## Testing and validation

| Product invariants | Required evidence |
| --- | --- |
| 1-5, 12 | Engine rootless assertions and mapped UID/GID evidence for both architectures; Podman writes to host-owned bind with unchanged owner; Docker named-volume initialization/reuse; conventional bind recipe and missing-source refusal. |
| 6, 7, 13 | Runtime config inspection plus healthy SQLite/mapping writes and fixed /list output under hardening; unwritable mount exits nonzero before listener/files; no live upstream dependency. |
| 8, 15 | Same-mount recreation reopens SQLite and cached mappings; ownership remains; documented migration/rollback preserves original storage. Review production changes separately in the media-stack repository. |
| 9, 11 | Unexpected exit restarts; explicit stop stays stopped; controlled mapping outage remains degraded without health-triggered restart or storage replacement. |
| 10 | Go cancellation tests and container SIGTERM during startup, prewarm, idle, and activity; exit before 30 seconds without SIGKILL; successful close/reopen for normal completed shutdown; lifecycle failure remains observable. |
| 14 | PR/native matrix uses identical candidate config/layers; publication checks exact platform digests under both rootless engines; promotion depends on both passing plus existing scans and attestation checks. |

Run focused Go race tests when lifecycle code changes, automation policy tests,
ShellCheck/actionlint, Compose config checks, Podman's generator on the example,
make docs, and make check. Record native rootless results with engine versions
and namespaces on both architectures. An unavailable rootless engine is a failed
required acceptance check, not a successful skip.

## Risks and rollout

Rootless setup requires subordinate-ID ranges and kernel/runtime support.
Verify both native CI runner images before wiring release gates; do not fall back
silently to rootful operation. UID mapping changes require reviewed storage
access, not recursive chown or Podman :U.

Recovering an old immutable image may fail new hardening/lifecycle checks.
Keep trusted automation separate from released source and preserve historical
digests; do not overwrite an immutable image to make it pass. A blocked recovery
must be reported with the failing contract. A new reviewed release fixes image
behavior; production activation remains governed by the media-stack repository.

## Parallelization

Use one implementation agent for identity, recipes, runtime fixtures, and
lifecycle integration because they share storage and acceptance contracts.
Implement after container-builds to avoid shared-file conflicts. Treat the
production media-stack change as a separate reviewed follow-up after disposable
acceptance, with its own branch and production validation.

## Implementation And Evidence

Container-builds merged in [PR #145](https://github.com/calmcacil/sonarr-anime-bridge/pull/145).
The current baseline preserves separate source/image gates. Runtime acceptance
now uses the original candidate image plus a test-only Go helper mounted read-only;
this removes the need for a separately pinned fixture image or a rebuilt app.
The helper seeds through the actual cache package, serves loopback mappings in a
private container namespace, and disables only the mapping listener for persisted
fallback. No production AniList override or URL allowlist change was introduced.
Verified successful paths need no scratch tmpfs.

`make check` passes locally; the final automation suite has 47 regressions. Controlled race tests
cover mapping startup, prewarm, and an active list request; each stop closes and
reopens SQLite. Five repeated cancellation runs pass. The request scenario uses
an explicit HTTP/scheduler budget; HTTP drain retains its ten-second bound.
Podman 5.4.2's generator accepts the portable example and emits keep-id 65532,
read-only root, capability drop, no-new-privileges, 30-second container stop, and
45-second systemd stop timeout.

Local native arm64 rootless Podman 5.4.2 passes missing/unwritable mounts, fixed
list output, image healthcheck execution, mapped identity, zero effective
capabilities, no-new-privileges, unchanged bind ownership, unexpected SIGABRT
exit/restart, same-storage recreation without the mapping listener, reopened
SQLite with persisted mapping discovery, degraded outage, and explicit stop.
Normal idle/active/degraded/stalled-startup stops exit zero in 0.051/0.057/0.048/
0.048 seconds, with cache-close and terminal shutdown evidence. This local image
packages the locally compiled binary in the pinned runtime base; the temporary
VFS compiler-image build exhausted storage. It is supplemental evidence, not a
claim of CI candidate/digest equivalence. CI must validate the original Dockerfile
candidate under both engines on both architectures before merge.

CI provisioning uses Ubuntu 24.04 distro Podman/uidmap/network-helper packages
and checksum-pinned Docker 29.9.0/rootless static archives in private storage,
reporting installed versions. The host Docker daemon is unchanged.
The distro versions are documented and recorded, rather than called immutable
pins. Provisioning can use runner privileges; tested engine processes and runtime
scenarios cannot. Namespace mappings shown inside Podman are relative to the
parent rootless namespace; evidence also records the real host account and bind
owner before/after. Docker archive imports must preserve config ID and rootfs
layers; published candidates must retain the exact requested platform digest.
Both engines run inside the existing native jobs and fail Required/promotion.

Docker 29's containerd image store reports manifest IDs through image inspection.
The isolated daemon explicitly uses the classic image store so config IDs have
the same meaning as Docker-save/Podman; all config and layer comparisons remain
mandatory. Mutation tests reject changed configs, changed/reordered layers, and
missing inspection data. Both isolated engines use cgroupfs rather than depending
on a host systemd user-manager socket. Resource-limit enforcement is not claimed.

Native acceptance completed in [CI run 38150646080](https://github.com/calmcacil/sonarr-anime-bridge/actions/runs/38150646080)
at implementation commit `81f3077`. All source, security, image, context-audit,
disposable-registry and Required checks pass. Each original Dockerfile candidate
was imported unchanged into rootless Docker 29.9.0 and Podman 4.9.3. Host runner
UID/GID is 1001:1001; container UID/GID is 65532:65532, capabilities are zero,
no-new-privileges is enabled, and Podman bind ownership stays 1001:1001.

| Architecture | Engine | Idle / active / degraded / startup stop (seconds) | Result |
| --- | --- | --- | --- |
| amd64 | Docker 29.9.0 | 0.047 / 0.050 / 0.046 / 0.046 | All scenarios pass; exit 0 |
| amd64 | Podman 4.9.3 | 0.051 / 0.058 / 0.052 / 0.050 | All scenarios pass; exit 0 |
| arm64 | Docker 29.9.0 | 0.037 / 0.043 / 0.038 / 0.038 | All scenarios pass; exit 0 |
| arm64 | Podman 4.9.3 | 0.045 / 0.079 / 0.254 / 0.054 | All scenarios pass; exit 0 |

The matrix verifies negative mounts, fixed lists, root-mount hardening, persistent
SQLite/mappings/metadata, same-storage recreation without the mapping listener,
unexpected exit/restart, explicit stop, degraded outage, stalled-startup SIGTERM,
and completed cache close. Native replay against v2.16.0 also passes ordered
responses, endpoints, persistence, clear, and shutdown. Supplemental local arm64
Docker 29.9.0 acceptance passes the same scenarios with exact archive identity
and 0.043-0.053 second clean stops. No release was published to exercise the live
promotion gate; workflow regressions cover both required engines and exact digest
selection. Actual publication and historical recovery remain independently gated.

The separate [media-stack PR #2](https://github.com/calmcacil/compose/pull/2)
prepares the production keep-id identity, read-only root and native 30/45-second
stop settings, preserving appdata, networking, inputs, target and auto-updates.
Its source validator, mutation coverage, all five regression suites, shell syntax
and Podman 5.4.2 generator inspection pass locally.
[Exact-head source CI](https://github.com/calmcacil/compose/actions/runs/38151086975)
passes on `1b327f4`. Production activation and live acceptance remain
pending under the media-stack delivery contract.

Production follow-up and live activation remain separate. An old immutable image
that fails the new lifecycle contract blocks recovery; preserve its digest and
report the failure rather than overwriting it or weakening validation.

Ubuntu 24.04 ships Podman 4.9.3, whose Quadlet generator rejects the newer
`StopTimeout` key. The portable example uses `PodmanArgs=--stop-timeout=30`,
which preserves the exact 30-second runtime contract on both 4.9 and 5.4.
The generated command is validated; production may use its supported native key.
