"""Release only when version-independent application or image inputs change."""

import json
import os
from pathlib import Path
import subprocess

from release_image import CANONICAL_REPOSITORY, command, stable_releases


IMAGE_INPUTS = {"Dockerfile", ".dockerignore", "go.mod", "go.sum", "LICENSE", "NOTICE", "entrypoint.sh"}


def runtime_path(path):
    if path in IMAGE_INPUTS:
        return True
    return (path.startswith(("cmd/", "internal/")) and path.endswith(".go")
            and not path.endswith("_test.go") and not path.startswith("internal/testutil/"))


def inputs(ref):
    records = subprocess.check_output(["git", "ls-tree", "-r", "-z", ref]).decode().split("\0")
    selected = {}
    for record in filter(None, records):
        entry, path = record.split("\t", 1)
        if runtime_path(path):
            selected[path] = entry
    if "Dockerfile" not in selected or not any(path.startswith("cmd/") for path in selected):
        raise ValueError(f"Cannot identify application build inputs at {ref}")
    return selected


def changed_inputs(before, after):
    return sorted(path for path in before.keys() | after.keys() if before.get(path) != after.get(path))


def latest_published_tag():
    # Compact JSON records work with old gh versions as well as CI's current CLI.
    records = command("gh", "api", "--paginate", "--jq", ".[] | @json",
                      f"repos/{CANONICAL_REPOSITORY}/releases?per_page=100").stdout
    tags = stable_releases([json.loads(line) for line in records.splitlines() if line.strip()])
    return tags[-1] if tags else None


def select_release():
    if os.environ["GITHUB_REPOSITORY"] != CANONICAL_REPOSITORY or os.environ["GITHUB_REF"] != "refs/heads/main":
        raise ValueError("Release coordination requires the canonical main branch")
    previous = latest_published_tag()
    current = inputs("HEAD")
    changed = changed_inputs(inputs(f"refs/tags/{previous}^{{commit}}"), current) if previous else sorted(current)
    eligible = str(bool(changed)).lower()
    with Path(os.environ["GITHUB_OUTPUT"]).open("a") as output:
        output.write(f"eligible={eligible}\n")
    print(f"Release eligible: {eligible}; previous published tag: {previous or 'none'}")
    if changed:
        print("Changed application/image inputs: " + ", ".join(changed))


if __name__ == "__main__":
    select_release()
