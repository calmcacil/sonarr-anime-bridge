import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import release_scope as release


ROOT = Path(__file__).resolve().parents[1]


class ReleaseScopeTests(unittest.TestCase):
    def test_application_and_image_inputs_not_automation_or_tests(self):
        for path in release.IMAGE_INPUTS | {"cmd/server/main.go", "internal/config/config.go"}:
            with self.subTest(path=path):
                self.assertTrue(release.runtime_path(path))
        for path in ("CHANGELOG.md", ".release-please-manifest.json", "release-please-config.json",
                     ".github/workflows/ci.yml", "Makefile", "README.md", "docs/CI_RELEASES.md",
                     "scripts/release_scope.py", "testdata/pipeline-year.json",
                     "cmd/server/main_test.go", "internal/filter/filter_test.go", "internal/testutil/testutil.go"):
            with self.subTest(path=path):
                self.assertFalse(release.runtime_path(path))
        self.assertEqual(release.changed_inputs({"a": "old", "removed": "data"}, {"a": "new", "added": "data"}),
                         ["a", "added", "removed"])
        self.assertEqual(release.changed_inputs({"a": "same"}, {"a": "same"}), [])

    def test_select_highest_published_stable_version(self):
        records = [{"tag_name": tag, "draft": False, "prerelease": False, "published_at": "2026-09-30"}
                   for tag in ("v2.9.0", "v2.10.0", "v2.14.7", "v2.14.8-rc.1")]
        records += [dict(records[-1], tag_name="v3.0.0", draft=True)]
        result = subprocess.CompletedProcess([], 0, "\n".join(json.dumps(row) for row in records), "")
        with patch.object(release, "command", return_value=result) as run:
            self.assertEqual(release.latest_published_tag(), "v2.14.7")
        self.assertIn("--paginate", run.call_args.args)
        self.assertNotIn("--slurp", run.call_args.args)
        with patch.object(release, "command", return_value=subprocess.CompletedProcess([], 0, "", "")):
            self.assertIsNone(release.latest_published_tag())
        with patch.object(release, "command", side_effect=RuntimeError("API unavailable")), self.assertRaises(RuntimeError):
            release.latest_published_tag()

    def test_git_inputs_ignore_metadata_and_detect_add_delete_revert(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            def git(*args):
                return subprocess.check_output(["git", *args], cwd=root).decode().strip()
            git("init", "-q")
            git("config", "user.name", "Test")
            git("config", "user.email", "test@example.invalid")
            (root / "cmd/server").mkdir(parents=True)
            source = root / "cmd/server/main.go"
            source.write_text("package main\n")
            (root / "Dockerfile").write_text("FROM scratch\n")
            git("add", "cmd/server/main.go", "Dockerfile")
            git("commit", "-qm", "base")
            base = git("rev-parse", "HEAD")
            def read_inputs(ref):
                with patch.object(release.subprocess, "check_output", side_effect=lambda args: subprocess.run(args, cwd=root, check=True, capture_output=True).stdout):
                    return release.inputs(ref)
            before = read_inputs(base)
            git("update-index", "--chmod=+x", "cmd/server/main.go")
            git("commit", "-qm", "change mode")
            self.assertEqual(release.changed_inputs(before, read_inputs("HEAD")), ["cmd/server/main.go"])
            git("update-index", "--chmod=-x", "cmd/server/main.go")
            git("commit", "-qm", "restore mode")
            self.assertEqual(before, read_inputs("HEAD"))
            (root / "CHANGELOG.md").write_text("new version\n")
            (root / "cmd/server/main_test.go").write_text("package main\n")
            git("add", "CHANGELOG.md", "cmd/server/main_test.go")
            git("commit", "-qm", "metadata and tests")
            self.assertEqual(before, read_inputs("HEAD"))
            source.write_text("package main\nfunc main() {}\n")
            git("add", "cmd/server/main.go")
            git("commit", "-qm", "runtime")
            self.assertEqual(release.changed_inputs(before, read_inputs("HEAD")), ["cmd/server/main.go"])
            source.write_text("package main\n")
            git("add", "cmd/server/main.go")
            git("commit", "-qm", "restore runtime")
            self.assertEqual(before, read_inputs("HEAD"))
            git("rm", "cmd/server/main.go")
            git("commit", "-qm", "delete entrypoint")
            with self.assertRaises(ValueError):
                read_inputs("HEAD")

    def test_coordination_guard_and_no_outputs_on_errors(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "output"
            output.touch()
            env = dict(GITHUB_REPOSITORY=release.CANONICAL_REPOSITORY, GITHUB_REF="refs/heads/main", GITHUB_OUTPUT=str(output))
            data = {"Dockerfile": "blob", "cmd/server/main.go": "source"}
            with patch.dict(os.environ, env), patch.object(release, "latest_published_tag", return_value="v2.14.7"), patch.object(release, "inputs", return_value=data):
                release.select_release()
            self.assertEqual(output.read_text(), "eligible=false\n")
            output.write_text("")
            with patch.dict(os.environ, env), patch.object(release, "latest_published_tag", return_value=None), patch.object(release, "inputs", return_value=data):
                release.select_release()
            self.assertEqual(output.read_text(), "eligible=true\n")
            for changes in ({"GITHUB_REPOSITORY": "fork/repo"}, {"GITHUB_REF": "refs/heads/feature"}):
                output.write_text("")
                with patch.dict(os.environ, dict(env, **changes)), self.assertRaises(ValueError):
                    release.select_release()
                self.assertEqual(output.read_text(), "")
            output.write_text("")
            with patch.dict(os.environ, env), patch.object(release, "latest_published_tag", side_effect=RuntimeError("API unavailable")), self.assertRaises(RuntimeError):
                release.select_release()
            self.assertEqual(output.read_text(), "")

    def test_workflow_guards_both_token_and_release_action(self):
        workflow = (ROOT / ".github/workflows/release.yml").read_text()
        self.assertIn("fetch-tags: true", workflow)
        self.assertIn("fetch-depth: 0", workflow)
        self.assertEqual(workflow.count("if: steps.eligibility.outputs.eligible == 'true'"), 2)
        self.assertLess(workflow.index("python3 scripts/release_scope.py"), workflow.index("Create release GitHub App token"))
        self.assertNotIn("packages: write", workflow)


if __name__ == "__main__":
    unittest.main()
