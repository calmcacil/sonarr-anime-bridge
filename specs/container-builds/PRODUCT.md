# Product Spec: Container Builds

Status: implemented; validated in [PR #145](https://github.com/calmcacil/sonarr-anime-bridge/pull/145).

## Summary

Build the supported amd64 and arm64 images from an explicit set of application
inputs, with useful incremental caching and consistent local and release builds.
Local benchmarks, credentials, databases, and editor files must never become
build inputs or exported builder-cache contents.

## Scope

This spec groups build-context hygiene and incremental-build improvements.
[Rootless deployment](../rootless-deployment/PRODUCT.md) owns runtime identity,
hardening, lifecycle, and runtime acceptance. Existing release trust, immutable
digests, scanning, SBOMs, provenance, and promotion remain requirements.

## Behavior

1. A build accepts only the declared application source, dependency manifests,
   license notices, and build recipe. Undeclared files remain excluded even when
   they are ignored by Git, placed alongside source, or added after checkout.

2. Adding or changing local benchmark artifacts, appdata, credentials, binaries,
   test fixtures, documentation, or editor metadata does not change the source
   copied into the builder or invalidate application compilation.

3. Excluded content is absent from the transferred context, intermediate layers,
   final runtime image, and newly exported build caches. The contract covers
   clean CI checkouts and developer checkouts with local files.

4. Source changes and dependency changes still rebuild the affected artifacts.
   An allowlist must not silently omit application packages or embedded assets.
   A new kind of build input requires updating its declared inventory and the
   release-change policy in the same change.

5. Repeated builds on a retained builder reuse Go compilation work. Changing
   application source preserves reusable compilation results for unaffected
   dependencies; changing the toolchain or architecture cannot reuse an
   incompatible compiled artifact.

6. Builds remain correct with empty caches, restored caches, or no cache service.
   Caches accelerate work; they do not decide which source or version is built.
   Documentation distinguishes retained-builder caching from ephemeral CI
   caching and makes no persistence claim without verification.

7. Local and release image builds use consistent binary build options, including
   removal of checkout-specific filesystem paths. Version injection, static
   linking, and the supported Linux amd64/arm64 targets remain available.
   This does not promise byte-identical OCI indexes across separate publications.

8. The local image check builds each supported architecture once and runs the
   native image's version check. A supported host's native image is reused rather
   than rebuilt under a second tag. Executing a foreign binary is not required
   for the local cross-build check.

9. CI continues testing actual native amd64 and arm64 images. Release validation
   continues testing the exact candidate platform digests before promotion;
   build optimization cannot introduce a second untested release build.

10. Runtime packaging retains the pinned distroless base, non-root default user,
    direct application entrypoint, writable data-directory contract, healthcheck,
    license notices, version reporting, and minimal runtime contents.

11. Build failures are reported as failures, including dependency-download,
    unsupported architecture, compilation, and native version-check failures.
    An unavailable optional cache does not select an older application binary.

12. Build-context size and cold, unchanged, and source-edited build measurements
    are recorded using the same toolchain and builder. Measurements identify the
    baseline and cache state; no machine-independent timing percentage is promised.
