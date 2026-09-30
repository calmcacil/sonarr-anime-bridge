"""Select CI work from the actual Git diff; validate intentional job skips."""

import json
import os
from pathlib import Path
import re
import subprocess
import sys

from release_image import CANONICAL_REPOSITORY, version


RELEASE_FILES = {"CHANGELOG.md", ".release-please-manifest.json"}
REGISTRY_FILES = {
    "Dockerfile", ".dockerignore", "Makefile", "release-please-config.json",
    ".release-please-manifest.json", "scripts/release_image.py",
    "scripts/test_release_image.py", "scripts/ci_scope.py", "scripts/test_ci_scope.py",
    "scripts/release_scope.py", "scripts/test_release_scope.py",
    "testdata/release-registry.sh", "testdata/runtime-smoke.sh",
}
FULL_JOBS = {"quality", "tests", "vulnerabilities", "dependency-review", "container"}


def classify(event, repository, payload, paths):
    if event not in {"pull_request", "merge_group", "workflow_dispatch"}:
        raise ValueError(f"Unsupported CI event: {event}")
    pr = payload.get("pull_request", {})
    release = (
        event == "pull_request" and repository == CANONICAL_REPOSITORY
        and pr.get("user", {}).get("login") == "calm-package-releaser[bot]"
        and pr.get("head", {}).get("repo", {}).get("full_name") == repository
        and pr.get("head", {}).get("ref") == "release-please--branches--main--components--sonarr-anime-bridge"
        and set(paths) == RELEASE_FILES
    )
    registry = event == "workflow_dispatch" or any(
        path in REGISTRY_FILES or path.startswith((".github/workflows/", ".github/actions/"))
        for path in paths
    )
    return {"mode": "release" if release else "full", "registry": str(not release and registry).lower()}


def validate_metadata(before, after, changelog):
    before, after = json.loads(before), json.loads(after)
    if not isinstance(before, dict) or not isinstance(after, dict) or set(before) != {"."} or set(after) != {"."}:
        raise ValueError("Release manifest must contain only the root package")
    if not isinstance(before["."], str) or not isinstance(after["."], str):
        raise ValueError("Release versions must be strings")
    if version("v" + after["."]) <= version("v" + before["."]):
        raise ValueError("Release version must increase")
    headings = re.findall(r"^## \[([^\]]+)\]", changelog, re.MULTILINE)
    if not headings or headings[0] != after["."]:
        raise ValueError("First changelog release must match the manifest version")


def validate_required(event, needs):
    if set(needs) != FULL_JOBS | {"scope", "codeql"}:
        raise ValueError("Required jobs are missing or unexpected")
    outputs = needs["scope"].get("outputs", {})
    mode = outputs.get("mode")
    if event not in {"pull_request", "merge_group", "workflow_dispatch"} or mode not in {"full", "release"}:
        raise ValueError("Invalid validation mode or event")
    if (outputs.get("registry") not in {"true", "false"}
            or (mode == "release" and (event != "pull_request" or outputs["registry"] != "false"))):
        raise ValueError("Invalid validation scope")
    if event == "workflow_dispatch" and outputs["registry"] != "true":
        raise ValueError("Manual validation must exercise registry promotion")
    expected = dict.fromkeys(FULL_JOBS, "success" if mode == "full" else "skipped")
    expected.update(scope="success", codeql="success")
    if mode == "full" and event != "pull_request":
        expected["dependency-review"] = "skipped"
    for job, result in expected.items():
        if needs[job].get("result") != result:
            raise ValueError(f"{job}: expected {result}, got {needs[job].get('result')}")


def git(*args):
    return subprocess.check_output(["git", *args])


def select_scope():
    event = os.environ["GITHUB_EVENT_NAME"]
    payload = json.loads(Path(os.environ["GITHUB_EVENT_PATH"]).read_text())
    base = ""
    paths = []
    if event != "workflow_dispatch":
        base = (payload["pull_request"]["base"]["sha"] if event == "pull_request"
                else payload["merge_group"]["base_sha"])
        if not re.fullmatch(r"[0-9a-f]{40}", base):
            raise ValueError("Invalid diff base commit")
        # No rename detection: moving a release script still selects registry tests.
        paths = git("diff", "--name-only", "--no-renames", "-z", base, "HEAD").decode().split("\0")[:-1]
    scope = classify(event, os.environ["GITHUB_REPOSITORY"], payload, paths)
    if scope["mode"] == "release":
        validate_metadata(git("show", f"{base}:.release-please-manifest.json"),
                          git("show", "HEAD:.release-please-manifest.json"),
                          git("show", "HEAD:CHANGELOG.md").decode())
    with open(os.environ["GITHUB_OUTPUT"], "a") as output:
        for name, value in scope.items():
            output.write(f"{name}={value}\n")
    print(f"Validation scope: {scope['mode']}; registry regression: {scope['registry']}")


if __name__ == "__main__":
    if sys.argv[1:] == ["required"]:
        event = os.environ["GITHUB_EVENT_NAME"]
        validate_required(event, json.loads(os.environ["JOB_RESULTS"]))
        print("All required checks match the validated scope")
    elif not sys.argv[1:]:
        select_scope()
    else:
        raise SystemExit("usage: ci_scope.py [required]")
