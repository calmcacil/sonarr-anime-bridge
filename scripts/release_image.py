#!/usr/bin/env python3
"""Fail-closed release selection and digest-preserving image promotion."""

import argparse
import json
import os
from pathlib import Path
import re
import subprocess
import sys


CANONICAL_REPOSITORY = "calmcacil/sonarr-anime-bridge"
STABLE_TAG = re.compile(r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)")
DIGEST = re.compile(r"sha256:[0-9a-f]{64}")
ARCHITECTURES = ("amd64", "arm64")


def version(tag):
    match = STABLE_TAG.fullmatch(tag)
    if not match:
        raise ValueError(f"Expected stable vMAJOR.MINOR.PATCH, got {tag!r}")
    return tuple(int(part) for part in match.groups())


def digest(value):
    if not isinstance(value, str) or not DIGEST.fullmatch(value):
        raise ValueError(f"Invalid image digest: {value!r}")
    return value


def command(*args, check=True):
    result = subprocess.run(args, capture_output=True, text=True, timeout=300)
    if check and result.returncode:
        raise RuntimeError(result.stderr.strip() or f"{args[0]} failed")
    return result


def output(**values):
    text = "".join(f"{key}={value}\n" for key, value in values.items())
    if os.environ.get("GITHUB_OUTPUT"):
        with Path(os.environ["GITHUB_OUTPUT"]).open("a") as stream:
            stream.write(text)
    print(text, end="")


def stable_releases(releases):
    return sorted({
        release["tag_name"] for release in releases
        if release.get("draft") is False and release.get("prerelease") is False
        and release.get("published_at") and STABLE_TAG.fullmatch(release["tag_name"])
    }, key=version)


def published_tags(repository):
    pages = json.loads(command(
        "gh", "api", "--paginate", "--slurp",
        f"repos/{repository}/releases?per_page=100",
    ).stdout)
    tags = stable_releases([release for page in pages for release in page])
    if not tags:
        raise ValueError("No published stable releases found")
    return tags


def validate_release(repository, event, ref, tag, release, event_id=""):
    if repository != CANONICAL_REPOSITORY:
        raise ValueError("Publication is limited to the canonical repository")
    if event not in ("release", "workflow_dispatch"):
        raise ValueError(f"Unsupported publication event: {event}")
    if event == "workflow_dispatch" and ref != "refs/heads/main":
        raise ValueError("Manual recovery must run from main")
    version(tag)
    if stable_releases([release]) != [tag]:
        raise ValueError("Tag must identify an existing published stable release")
    if event == "release" and str(release["id"]) != event_id:
        raise ValueError("Published release does not match the event")


def manifest(ref, absent_ok=False):
    result = command(
        "docker", "buildx", "imagetools", "inspect", ref,
        "--format", "{{json .Manifest}}", check=False,
    )
    if result.returncode:
        # Do not confuse authorization, DNS, or generic HTTP failures with absence.
        missing = re.search(
            r"(?:^|: )not found\s*$|\b(?:MANIFEST_UNKNOWN|NAME_UNKNOWN|manifest unknown|name unknown)\b",
            result.stderr,
        )
        if absent_ok and missing:
            return None
        raise RuntimeError(result.stderr.strip() or "Registry inspection failed")
    data = json.loads(result.stdout)
    digest(data.get("digest"))
    return data


def platforms(index):
    runtime = {}
    attestations = []
    seen = set()
    for entry in index.get("manifests", []):
        platform = entry.get("platform", {})
        os_name, arch = platform.get("os"), platform.get("architecture")
        entry_digest = digest(entry.get("digest"))
        if entry_digest in seen:
            raise ValueError("Duplicate manifest digest in image index")
        seen.add(entry_digest)
        if os_name == "linux" and arch in ARCHITECTURES:
            if arch in runtime:
                raise ValueError(f"Duplicate Linux {arch} manifest")
            if platform.get("variant", "") not in ("", "v8" if arch == "arm64" else ""):
                raise ValueError(f"Unexpected {arch} variant")
            runtime[arch] = entry_digest
        elif os_name == "unknown" and arch == "unknown":
            annotations = entry.get("annotations", {})
            if annotations.get("vnd.docker.reference.type") != "attestation-manifest":
                raise ValueError("Unknown platform is not an attestation")
            attestations.append((entry_digest, digest(annotations.get("vnd.docker.reference.digest"))))
        else:
            raise ValueError(f"Unexpected runtime platform: {platform}")
    if set(runtime) != set(ARCHITECTURES) or len(set(runtime.values())) != 2:
        raise ValueError("Expected exactly one distinct Linux amd64 and arm64 manifest")
    if any(subject not in runtime.values() for _, subject in attestations):
        raise ValueError("Attestation references an unknown runtime manifest")
    return runtime, attestations


def image_config(ref):
    return json.loads(command(
        "docker", "buildx", "imagetools", "inspect", ref,
        "--format", "{{json .Image}}",
    ).stdout)


def image_versions(image, index):
    runtime, _ = platforms(index)
    versions = {
        image_config(f"{image}@{platform_digest}").get("config", {}).get("Labels", {}).get(
            "org.opencontainers.image.version", ""
        ) for platform_digest in runtime.values()
    }
    if len(versions) != 1:
        raise ValueError("Platform version labels disagree")
    tag = versions.pop()
    version(tag)
    return tag


def validate_image(image, image_digest, revision, tag):
    digest(image_digest)
    version(tag)
    if not re.fullmatch(r"[0-9a-f]{40}", revision):
        raise ValueError("Expected a verified Git source commit")
    index = manifest(f"{image}@{image_digest}")
    if index["digest"] != image_digest:
        raise ValueError("Registry returned a different index digest")
    runtime, attestations = platforms(index)
    predicates = {subject: set() for subject in runtime.values()}
    for attestation_digest, subject in attestations:
        attestation = json.loads(command(
            "docker", "buildx", "imagetools", "inspect", f"{image}@{attestation_digest}", "--raw",
        ).stdout)
        if attestation.get("subject") and attestation["subject"].get("digest") != subject:
            raise ValueError("Attestation subject does not match its index descriptor")
        for layer in attestation.get("layers", []):
            if layer.get("mediaType") == "application/vnd.in-toto+json":
                predicates[subject].add(layer.get("annotations", {}).get("in-toto.io/predicate-type"))
    for arch, platform_digest in runtime.items():
        types = predicates[platform_digest]
        if "https://spdx.dev/Document" not in types or not types.intersection({
            "https://slsa.dev/provenance/v0.2", "https://slsa.dev/provenance/v1",
        }):
            raise ValueError(f"Missing SBOM or provenance for {arch}")
        config = image_config(f"{image}@{platform_digest}")
        settings = config.get("config", {})
        labels = settings.get("Labels", {})
        if config.get("os") != "linux" or config.get("architecture") != arch:
            raise ValueError(f"Incorrect runtime architecture for {arch}")
        if labels.get("org.opencontainers.image.revision") != revision:
            raise ValueError(f"{arch} revision does not match the verified release source")
        if labels.get("org.opencontainers.image.version") != tag:
            raise ValueError(f"{arch} version does not match the release tag")
        if settings.get("User") not in ("65532", "65532:65532"):
            raise ValueError(f"{arch} image must default to runtime UID 65532")
        if settings.get("Entrypoint") != ["/server"] or "/data" not in settings.get("Volumes", {}):
            raise ValueError(f"{arch} image violates the /server and /data runtime contract")
    return runtime


def eligible_aliases(tag, tags):
    candidate = version(tag)
    if tag not in tags:
        raise ValueError("Candidate is not a published stable release")
    major, minor, _ = candidate
    tracks = {
        f"v{major}.{minor}": [item for item in tags if version(item)[:2] == candidate[:2]],
        f"v{major}": [item for item in tags if version(item)[0] == major],
        "latest": tags,
    }
    return [alias for alias, releases in tracks.items() if max(releases, key=version) == tag]


def copy_index(image, image_digest, tag):
    command("docker", "buildx", "imagetools", "create", "--tag", f"{image}:{tag}", f"{image}@{image_digest}")
    if manifest(f"{image}:{tag}")["digest"] != image_digest:
        raise ValueError(f"Promotion changed the index digest for {tag}")


def promote(image, image_digest, revision, tag, tags):
    validate_image(image, image_digest, revision, tag)
    existing = manifest(f"{image}:{tag}", absent_ok=True)
    if existing and existing["digest"] != image_digest:
        raise ValueError("Refusing to overwrite an existing immutable full-version tag")
    aliases = []
    # Complete all preflight reads before making any public tag visible.
    for alias in eligible_aliases(tag, tags):
        current = manifest(f"{image}:{alias}", absent_ok=True)
        if current:
            current_tag = image_versions(image, current)
            current_version = version(current_tag)
            candidate = version(tag)
            if alias != "latest":
                width = 2 if alias.count(".") == 1 else 1
                if current_version[:width] != candidate[:width]:
                    raise ValueError(f"Existing {alias} points outside its version track")
            if current_version > candidate:
                print(f"Preserving newer alias {alias}: {current_tag}")
                continue
        aliases.append(alias)
    if not existing:
        copy_index(image, image_digest, tag)
    for alias in aliases:
        copy_index(image, image_digest, alias)
    return aliases


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("operation", choices=("release", "existing", "validate", "promote", "scan-current"))
    parser.add_argument("--image", default=f"ghcr.io/{CANONICAL_REPOSITORY}")
    parser.add_argument("--repository", default=CANONICAL_REPOSITORY)
    parser.add_argument("--tag")
    parser.add_argument("--revision")
    parser.add_argument("--digest")
    parser.add_argument("--architecture", choices=ARCHITECTURES)
    args = parser.parse_args()
    if args.operation == "release":
        event = os.environ["GITHUB_EVENT_NAME"]
        tag = os.environ.get("EVENT_TAG" if event == "release" else "INPUT_TAG", "")
        version(tag)
        release = json.loads(command("gh", "api", f"repos/{args.repository}/releases/tags/{tag}").stdout)
        validate_release(os.environ["GITHUB_REPOSITORY"], event, os.environ["GITHUB_REF"], tag,
                         release, os.environ.get("EVENT_RELEASE_ID", ""))
        output(tag=tag)
    elif args.operation == "existing":
        version(args.tag)
        index = manifest(f"{args.image}:{args.tag}", absent_ok=True)
        if index:
            validate_image(args.image, index["digest"], args.revision, args.tag)
            output(exists="true", digest=index["digest"])
        else:
            output(exists="false")
    elif args.operation == "validate":
        runtime = validate_image(args.image, args.digest, args.revision, args.tag)
        output(platform_ref=f"{args.image}@{runtime[args.architecture]}")
    elif args.operation == "promote":
        aliases = promote(args.image, args.digest, args.revision, args.tag, published_tags(args.repository))
        message = f"Verified {args.image}:{args.tag} at {args.digest}; promoted aliases: {', '.join(aliases) or 'none'}.\n"
        print(message, end="")
        if os.environ.get("GITHUB_STEP_SUMMARY"):
            with Path(os.environ["GITHUB_STEP_SUMMARY"]).open("a") as stream:
                stream.write(message)
    else:
        tag = published_tags(args.repository)[-1]
        index = manifest(f"{args.image}:{tag}")
        runtime, _ = platforms(index)
        matrix = {"include": [
            {"architecture": arch, "image": f"{args.image}@{runtime[arch]}", "tag": tag,
             "index_digest": index["digest"]} for arch in ARCHITECTURES
        ]}
        output(matrix=json.dumps(matrix, separators=(",", ":")))


if __name__ == "__main__":
    try:
        main()
    except (ValueError, RuntimeError, KeyError, TypeError, subprocess.TimeoutExpired) as error:
        print(f"Release image validation failed: {error}", file=sys.stderr)
        sys.exit(1)
