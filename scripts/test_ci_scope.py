import copy
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
import unittest

import ci_scope as ci


ROOT = Path(__file__).resolve().parents[1]
RELEASE_EVENT = {"pull_request": {
    "user": {"login": "calm-package-releaser[bot]"},
    "head": {"ref": "release-please--branches--main--components--sonarr-anime-bridge",
             "repo": {"full_name": ci.CANONICAL_REPOSITORY}},
}}


class ScopeTests(unittest.TestCase):
    def classify(self, paths, event="pull_request", repository=ci.CANONICAL_REPOSITORY, payload=None):
        return ci.classify(event, repository, RELEASE_EVENT if payload is None else payload, paths)

    def test_release_requires_exact_files_bot_and_same_repository(self):
        self.assertEqual(self.classify(ci.RELEASE_FILES), {"mode": "release", "registry": "false"})
        payloads = []
        for key, value in (("user", {"login": "maintainer"}),
                           ("head", {"ref": "feature", "repo": {"full_name": ci.CANONICAL_REPOSITORY}}),
                           ("head", {"ref": RELEASE_EVENT["pull_request"]["head"]["ref"], "repo": {"full_name": "fork/repo"}})):
            payload = copy.deepcopy(RELEASE_EVENT)
            payload["pull_request"][key] = value
            payloads.append(payload)
        for payload in payloads + [{}]:
            with self.subTest(payload=payload):
                self.assertEqual(self.classify(ci.RELEASE_FILES, payload=payload)["mode"], "full")
        self.assertEqual(self.classify(ci.RELEASE_FILES, repository="fork/repo")["mode"], "full")
        for paths in ([], ["CHANGELOG.md"], ci.RELEASE_FILES | {"cmd/server/main.go"},
                      ci.RELEASE_FILES | {"scripts/ci_scope.py"}, ci.RELEASE_FILES | {".github/workflows/ci.yml"}):
            with self.subTest(paths=paths):
                self.assertEqual(self.classify(paths)["mode"], "full")

    def test_registry_scope_and_non_pr_events(self):
        for path in ci.REGISTRY_FILES | {".github/workflows/publish.yml", ".github/actions/setup-go/action.yml"}:
            with self.subTest(path=path):
                self.assertEqual(self.classify([path]), {"mode": "full", "registry": "true"})
        for path in ("cmd/server/main.go", "internal/cache/cache.go", "go.mod", "go.sum", "docs/CI_RELEASES.md", "scripts/check-doc-links.py"):
            with self.subTest(path=path):
                self.assertEqual(self.classify([path]), {"mode": "full", "registry": "false"})
        self.assertEqual(self.classify([], event="workflow_dispatch"), {"mode": "full", "registry": "true"})
        self.assertEqual(self.classify(ci.RELEASE_FILES, event="merge_group")["mode"], "full")
        with self.assertRaises(ValueError):
            self.classify([], event="push")

    def test_release_metadata_version_and_changelog(self):
        before = '{".":"2.14.7"}'
        after = '{".":"2.14.8"}'
        changelog = "# Changelog\n\n## [2.14.8](url) (2026-09-30)\n\n## [2.14.7](url)\n"
        ci.validate_metadata(before, after, changelog)
        for invalid in ('{".":"2.14.7"}', '{".":"2.14.6"}', '{".":"02.14.8"}',
                        '{".":"2.14.8-rc.1"}', '{".":2}', '{".":"2.14.8","extra":"1.0.0"}', '[]', '{'):
            with self.subTest(manifest=invalid), self.assertRaises(ValueError):
                ci.validate_metadata(before, invalid, changelog)
        for invalid in ("", "# Changelog", "## [2.14.7](url)\n## [2.14.8](url)"):
            with self.subTest(changelog=invalid), self.assertRaises(ValueError):
                ci.validate_metadata(before, after, invalid)

    def test_required_result_truth_table(self):
        for event, mode in (("pull_request", "full"), ("pull_request", "release"),
                            ("merge_group", "full"), ("workflow_dispatch", "full")):
            needs = {name: {"result": "success" if mode == "full" else "skipped"} for name in ci.FULL_JOBS}
            needs.update(scope={"result": "success", "outputs": {"mode": mode, "registry": "true" if event == "workflow_dispatch" else "false"}},
                         codeql={"result": "success"})
            if mode == "full" and event != "pull_request":
                needs["dependency-review"]["result"] = "skipped"
            ci.validate_required(event, needs)
            for name in needs:
                for status in ("success", "failure", "cancelled", "skipped", ""):
                    if status == needs[name]["result"]:
                        continue
                    invalid = copy.deepcopy(needs)
                    invalid[name]["result"] = status
                    with self.subTest(event=event, mode=mode, job=name, status=status), self.assertRaises(ValueError):
                        ci.validate_required(event, invalid)
                invalid = copy.deepcopy(needs)
                del invalid[name]
                with self.subTest(missing=name), self.assertRaises(ValueError):
                    ci.validate_required(event, invalid)
            for outputs in ({}, {"mode": "unknown", "registry": "false"}, {"mode": mode, "registry": "unknown"}):
                invalid = copy.deepcopy(needs)
                invalid["scope"]["outputs"] = outputs
                with self.assertRaises(ValueError):
                    ci.validate_required(event, invalid)
            if mode == "release":
                invalid = copy.deepcopy(needs)
                invalid["scope"]["outputs"]["registry"] = "true"
                with self.assertRaises(ValueError):
                    ci.validate_required(event, invalid)
                for invalid_event in ("push", "merge_group", "workflow_dispatch"):
                    with self.assertRaises(ValueError):
                        ci.validate_required(invalid_event, needs)

    def test_cli_uses_git_diff_and_fails_without_valid_history(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            def git(*args):
                return subprocess.check_output(["git", *args], cwd=root).decode().strip()
            git("init", "-q")
            git("config", "user.name", "Test")
            git("config", "user.email", "test@example.invalid")
            (root / ".release-please-manifest.json").write_text('{".":"2.14.7"}')
            (root / "CHANGELOG.md").write_text("## [2.14.7](url)\n")
            (root / "Dockerfile").write_text("FROM scratch\n")
            git("add", ".release-please-manifest.json", "CHANGELOG.md", "Dockerfile")
            git("commit", "-qm", "base")
            base = git("rev-parse", "HEAD")
            payload = copy.deepcopy(RELEASE_EVENT)
            payload["pull_request"]["base"] = {"sha": base}
            event_file, output = root / "event.json", root / "output"
            event_file.write_text(json.dumps(payload))
            env = dict(os.environ, GITHUB_EVENT_NAME="pull_request", GITHUB_REPOSITORY=ci.CANONICAL_REPOSITORY,
                       GITHUB_EVENT_PATH=str(event_file), GITHUB_OUTPUT=str(output), PYTHONDONTWRITEBYTECODE="1")
            def run():
                output.write_text("")
                return subprocess.run(["python3", str(ROOT / "scripts/ci_scope.py")], cwd=root, env=env, capture_output=True)
            (root / ".release-please-manifest.json").write_text('{".":"2.14.8"}')
            (root / "CHANGELOG.md").write_text("## [2.14.8](url)\n## [2.14.7](url)\n")
            git("add", ".release-please-manifest.json", "CHANGELOG.md")
            git("commit", "-qm", "release")
            result = run()
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(output.read_text(), "mode=release\nregistry=false\n")
            # A release-like branch with a renamed build file must run full CI.
            git("mv", "Dockerfile", "moved-build-file")
            git("commit", "-qm", "rename")
            result = run()
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(output.read_text(), "mode=full\nregistry=true\n")
            payload["pull_request"]["base"]["sha"] = "0" * 40
            event_file.write_text(json.dumps(payload))
            self.assertNotEqual(run().returncode, 0)
            self.assertEqual(output.read_text(), "")

    def test_workflow_wiring_and_default_branch_baseline(self):
        workflow = (ROOT / ".github/workflows/ci.yml").read_text()
        triggers = workflow.split("permissions:")[0]
        self.assertNotIn("push:", triggers)
        self.assertNotIn("paths-ignore", workflow)
        self.assertNotIn("  builds:", workflow)
        self.assertIn("fetch-depth: 0", workflow)
        self.assertIn("python3 scripts/ci_scope.py required", workflow)
        self.assertIn("JOB_RESULTS: ${{ toJSON(needs) }}", workflow)
        self.assertIn("needs: [scope, quality, tests, vulnerabilities, dependency-review, codeql, container]", workflow)
        self.assertIn("matrix.architecture == 'amd64' && needs.scope.outputs.registry == 'true'", workflow)
        for job in ("quality", "tests", "vulnerabilities", "dependency-review", "container"):
            block = re.split(r"\n  [\w-]+:\n", workflow.split(f"  {job}:\n")[1])[0]
            self.assertIn("needs: scope", block)
            self.assertIn("needs.scope.outputs.mode == 'full'", block)
        codeql = (ROOT / ".github/workflows/codeql.yml").read_text()
        self.assertIn("push:\n    branches: [main]", codeql)
        self.assertIn("workflow_call:", codeql)
        self.assertIn("github.workflow", codeql)


if __name__ == "__main__":
    unittest.main()
