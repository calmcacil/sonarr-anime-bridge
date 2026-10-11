"""Regress the local build entrypoint's command count and error propagation."""

import os
from pathlib import Path
import subprocess
import tempfile
import unittest

from container_build_audit import compile_step, production
from release_scope import runtime_path


ROOT = Path(__file__).resolve().parents[1]


class ContainerBuildTests(unittest.TestCase):
    def test_inventory_agrees_with_release_policy(self):
        for name in ("Dockerfile", ".dockerignore", "go.mod", "LICENSE", "NOTICE", "cmd/server/main.go",
                     "internal/nested/source.go", "internal/cache/cache_test.go", "internal/testutil/a.go",
                     "cmd/server/key.pem", "internal/mapping/cache.db", "scripts/container_build_audit.py"):
            with self.subTest(path=name):
                self.assertEqual(production(name), runtime_path(name))

    def test_compile_cache_parser(self):
        log = "#7 [builder 8/9] RUN --mount=type=cache,id=go-build-linux-arm64 go build\n#7 CACHED\n"
        compile_step(log, True)
        with self.assertRaises(AssertionError):
            compile_step(log, False)
        with self.assertRaises(AssertionError):
            compile_step("no compilation step", True)

    def test_local_build_count_native_reuse_and_failures(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            docker = root / "docker"
            docker.write_text('#!/bin/sh\nprintf "%s\\n" "$*" >> "$AUDIT_COMMANDS"\n'
                              'if [ "$1" = build ] && [ "$AUDIT_FAIL" = build ]; then exit 7; fi\n'
                              'if [ "$1" = run ]; then [ "$AUDIT_FAIL" != run ] || exit 9; echo local; fi\n')
            docker.chmod(0o755)
            sh = root / "sh"
            sh.write_text('#!/bin/sh\nexit 0\n')
            sh.chmod(0o755)
            commands = root / "commands"
            env = dict(os.environ, PATH=f"{root}:{os.environ['PATH']}", AUDIT_COMMANDS=str(commands), AUDIT_FAIL="")
            for arch in ("amd64", "arm64"):
                commands.write_text("")
                result = subprocess.run(["make", "docker-build", f"HOST_ARCH={arch}"], cwd=ROOT, env=env, capture_output=True)
                self.assertEqual(result.returncode, 0, result.stderr)
                calls = commands.read_text().splitlines()
                self.assertEqual(sum(line.startswith("build ") for line in calls), 2)
                self.assertEqual(sum(line.startswith("run ") for line in calls), 1)
                self.assertIn(f"sonarr-anime-bridge:check-{arch}", calls[-1])
            commands.write_text("")
            result = subprocess.run(["make", "docker-build", "HOST_ARCH=386"], cwd=ROOT, env=env, capture_output=True)
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual(commands.read_text(), "")
            for failure in ("build", "run"):
                result = subprocess.run(["make", "docker-build", "HOST_ARCH=amd64"], cwd=ROOT,
                                        env=dict(env, AUDIT_FAIL=failure), capture_output=True)
                self.assertNotEqual(result.returncode, 0)


if __name__ == "__main__":
    unittest.main()
