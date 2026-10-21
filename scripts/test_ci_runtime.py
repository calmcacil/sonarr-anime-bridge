"""Exercise CI's actual parallel shell block with isolated engine stand-ins."""

import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import textwrap
import unittest

ROOT = Path(__file__).resolve().parents[1]


@unittest.skipUnless(os.name == "posix" and shutil.which("bash"), "requires POSIX bash")
class ParallelRuntimeTests(unittest.TestCase):
    def test_both_engines_overlap_finish_and_propagate_failures(self):
        workflow = (ROOT / ".github/workflows/ci.yml").read_text()
        block = workflow.split("      - name: Test the same candidate bytes under both rootless engines\n", 1)[1]
        block = textwrap.dedent(block.split("        run: |\n", 1)[1].split("      - name:", 1)[0])
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "testdata").mkdir()
            (root / "bin").mkdir()
            docker = root / "bin/docker"
            docker.write_text("#!/bin/sh\nexit 0\n")
            docker.chmod(0o755)
            # A barrier makes a sequential implementation fail, without timing assertions.
            (root / "testdata/rootless-smoke.sh").write_text('''#!/bin/sh
set -eu
test "$SAB_SMOKE_FIXTURE" = "$RUNNER_TEMP/runtime-fixture"
test "$SAB_SMOKE_OUTPUT" = "$RUNNER_TEMP/evidence/rootless-$1"
touch "$RUNNER_TEMP/$1-started"
case "$1" in docker) other=podman ;; podman) other=docker ;; esac
for _ in $(seq 1 100); do
    if [ -f "$RUNNER_TEMP/$other-started" ]; then
        touch "$RUNNER_TEMP/$1-finished"
        case "$1" in docker) exit "$DOCKER_STATUS" ;; podman) exit "$PODMAN_STATUS" ;; esac
    fi
    sleep 0.02
done
exit 99
''')
            # Keep even candidate transport paths inside this disposable test directory.
            block = block.replace("/tmp/sab-candidate", str(root / "candidate"))
            for docker_status, podman_status in ((0, 0), (7, 0), (0, 9), (7, 9)):
                with self.subTest(docker=docker_status, podman=podman_status):
                    for marker in root.glob("*-started"):
                        marker.unlink()
                    for marker in root.glob("*-finished"):
                        marker.unlink()
                    env = dict(os.environ, PATH=str(root / "bin") + os.pathsep + os.environ["PATH"],
                               RUNNER_TEMP=str(root), SMOKE_EVIDENCE_PATH="evidence",
                               DOCKER_STATUS=str(docker_status), PODMAN_STATUS=str(podman_status))
                    result = subprocess.run(["bash", "-e", "-o", "pipefail", "-c", block],
                                            cwd=root, env=env, capture_output=True, text=True, timeout=10)
                    self.assertEqual(result.returncode == 0, docker_status == podman_status == 0, result.stderr)
                    self.assertIn(f"docker={docker_status} podman={podman_status}", result.stdout)
                    for engine in ("docker", "podman"):
                        self.assertTrue((root / f"{engine}-finished").exists())


if __name__ == "__main__":
    unittest.main()
