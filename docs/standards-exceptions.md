# Standards exceptions

**Image signing:** There is no supported consumer signature-verification path. Exact OCI digests, provenance and SBOMs provide the current integrity record, not a publisher signature. Reassess when consumers require signing.

**Independent review:** A solo maintainer cannot supply an independent approval. Use PRs and the `Required` check. When two maintainers are routinely available, require one independent approval and dismiss stale reviews; add CODEOWNERS only for real ownership boundaries. This policy is not evidence of live GitHub settings.
