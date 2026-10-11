# Product Spec: Rootless Deployment

Status: proposed; implementation is pending.

## Summary

Provide explicit deployment recipes for rootless Podman, rootless Docker, and
conventional Docker Compose. Each recipe runs the service as a non-root container
user, preserves data, applies a narrow writable-filesystem policy, and has
verified restart and shutdown behavior.

## Scope

Rootless Podman/Quadlet is the primary deployment target inferred from the
existing media stack; Docker Compose remains supported. This spec groups identity
mapping, hardening, lifecycle, and actual rootless runtime validation.
[Container builds](../container-builds/PRODUCT.md) owns build inputs and caching.

The application repository owns portable examples, documentation, and tests.
The separate media-stack repository owns its production Quadlet and activation.
Writing or implementing this spec does not authorize production deployment.

## Behavior

1. Documentation distinguishes an unprivileged container process from a
   rootless container engine. It identifies the engine, host identity, container
   identity, and writable-storage setup for each supported recipe.

2. The published image retains its non-root default UID/GID 65532:65532 and
   starts the application directly. It does not acquire privileges, invoke a
   shell, change data ownership, or download a replacement application at startup.

3. The rootless Podman recipe maps the invoking host account to container
   UID/GID 65532:65532. A private host-owned bind directory is writable without
   container UID 0 or a recursive ownership change.

4. The rootless Docker recipe works without assuming container UID 1000 maps to
   host UID 1000. Its default uses an explicitly named persistent volume and the
   image identity; any bind-mount alternative explains the actual namespace
   mapping and verifies access before use.

5. Conventional Docker Compose retains a host bind-mount recipe with an explicit
   runtime UID/GID and pre-existing writable directory. PUID/PGID remain Compose
   substitutions, not application environment settings. The recipe does not
   create a missing bind directory with an unintended owner.

6. All supported recipes drop capabilities, prevent privilege escalation, and
   use a read-only container root. Persistent writes are confined to /data;
   scratch space, when required, uses bounded nonpersistent storage.
   The service requires no engine socket, privileged mode, or host networking.

7. Healthy operation includes SQLite database/sidecar writes, mapping and
   metadata persistence, and normal list responses under those restrictions.
   A missing or unwritable data directory fails before listening or creating
   persistent application files, with an actionable diagnostic.

8. Recreating or upgrading a container with the same data mount retains usable
   cache and mapping data. Deployment changes do not delete state, change host
   ownership recursively, silently switch to empty storage, or run two writers
   against the same data directory.

9. The supported recipes recover from an unexpected process exit. An explicit
   operator stop remains stopped. An unhealthy healthcheck alone does not imply
   that Docker restarts the container; the documented behavior distinguishes
   health reporting from process restart.

10. Containers receive SIGTERM directly and have a 30-second stop grace period.
    Startup work, prewarm, active requests, and background work respond to
    cancellation so ordinary stops finish before that grace expires. Completed
    shutdown closes application storage; forced termination is reported as a
    failed lifecycle check.

11. Startup during an upstream outage retains current degraded-health and retry
    behavior. Restart policies must not restart solely because mappings are
    temporarily unavailable or prewarm is still running.

12. Runtime acceptance verifies the engine is actually rootless and checks the
    container identity and host-side mount access. A non-root user on a rootful
    engine is separate evidence and cannot satisfy rootless acceptance.

13. Native amd64 and arm64 checks exercise hardened rootless Podman and rootless
    Docker deployments, including mount rejection, persistence, restart, stop,
    and a controlled list response. Tests use disposable storage and controlled
    upstreams, not production data or live AniList/mapping availability.

14. Release promotion requires successful rootless validation of the exact
    candidate platform images. Both engines test the candidate bytes, not a
    rebuild; existing source validation, vulnerability scans, SBOM/provenance,
    and immutable digest promotion remain in force.

15. Migration instructions name the existing storage location, old/new identities,
    required runtime settings, verification, and rollback. Image rollback and
    data recovery are distinguished. Existing production auto-update and
    internal-network policy are preserved unless separately reviewed.
