"""Execute the real rootless wrapper against isolated fake engine commands."""

import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[1]
SHELL = shutil.which("sh")


@unittest.skipUnless(os.name == "posix" and SHELL, "rootless shell wrapper requires POSIX sh")
class RootlessWrapperTests(unittest.TestCase):
    def run_wrapper(self, engine, scenario, archive=False, explicit_output=True):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        work = Path(temporary.name)
        binaries = work / "bin"
        binaries.mkdir()
        state = work / "state"
        state.mkdir()
        output = work / "evidence"

        def executable(name, source):
            path = binaries / name
            path.write_text('#!/bin/sh\nexec "$SAB_TEST_PYTHON" "$0.py" "$@"\n',
                            encoding="utf-8", newline="\n")
            Path(str(path) + ".py").write_text(source, encoding="utf-8", newline="\n")
            path.chmod(0o755)

        executable("id", "print(1000)\n")
        executable("dockerd-rootless.sh", """import os, time
print('actual daemon startup diagnostic password=private-value', flush=True)
if os.environ['SCENARIO'] != 'daemon-failure':
    time.sleep(60)
""")
        fake_engine = """import json, os, sys, time
args = sys.argv[1:]
scenario = os.environ['SCENARIO']
if 'info' in args and scenario == 'daemon-failure':
    time.sleep(0.1)
    sys.exit(1)
if 'pull' in args or 'load' in args:
    print('actual engine startup diagnostic token=private-value', file=sys.stderr)
    if scenario == 'pull-failure':
        sys.exit(37)
if 'inspect' in args:
    print(json.dumps([{'Id': 'sha256:' + 'a' * 64, 'RootFS': {'Layers': ['sha256:one']}}]))
"""
        executable("docker", fake_engine)
        executable("podman", fake_engine)
        executable("python3", f"""import json, os, pathlib, sys
if pathlib.Path(sys.argv[1]).name == 'runtime_smoke.py':
    args = sys.argv[2:]
    output = pathlib.Path(args[args.index('--output') + 1])
    (output / 'runtime-call.json').write_text(json.dumps(args))
    sys.exit(43 if os.environ['SCENARIO'] == 'runtime-failure' else 0)
os.execv({sys.executable!r}, [{sys.executable!r}, *sys.argv[1:]])
""")
        env = dict(os.environ, PATH=str(binaries) + os.pathsep + os.environ["PATH"],
                   TMPDIR=state.as_posix(), SCENARIO=scenario, SAB_ROOTLESS_DOCKER_BIN="",
                   SAB_TEST_PYTHON=Path(sys.executable).as_posix())
        env.pop("SAB_SMOKE_OUTPUT", None)
        if explicit_output:
            env["SAB_SMOKE_OUTPUT"] = output.as_posix()
        args = [SHELL, (ROOT / "testdata/rootless-smoke.sh").as_posix(), engine, "candidate"]
        if archive:
            expected = work / "expected.json"
            expected.write_text(json.dumps([{"Id": "sha256:" + "a" * 64,
                                             "RootFS": {"Layers": ["sha256:one"]}}]))
            args += [(work / "candidate.tar").as_posix(), expected.as_posix()]
        result = subprocess.run(args, env=env, text=True, capture_output=True, timeout=15)
        if not explicit_output:
            output = next(state.glob("sab-rootless-*/evidence"))
        return result, output

    def test_daemon_failure_keeps_actual_log_and_original_status(self):
        result, output = self.run_wrapper("docker", "daemon-failure")
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertIn("actual daemon startup diagnostic", (output / "daemon.log").read_text())
        self.assertNotIn("private-value", (output / "daemon.log").read_text())
        self.assertEqual(json.loads((output / "rootless.json").read_text())["exit_code"], 1)
        self.assertFalse((output / "runtime-call.json").exists())

    def test_engine_pull_failure_preserves_status_and_diagnostics(self):
        for engine in ("docker", "podman"):
            with self.subTest(engine=engine):
                result, output = self.run_wrapper(engine, "pull-failure")
                self.assertEqual(result.returncode, 37, result.stderr)
                log = (output / "engine.log").read_text()
                self.assertIn("actual engine startup diagnostic", log)
                self.assertNotIn("private-value", log)
                self.assertEqual(json.loads((output / "rootless.json").read_text())["exit_code"], 37)
                self.assertFalse((output / "candidate.json").exists())

    def test_runtime_failure_and_existing_archive_interface_pass_output_through(self):
        for engine in ("docker", "podman"):
            with self.subTest(engine=engine):
                result, output = self.run_wrapper(engine, "runtime-failure", archive=True)
                self.assertEqual(result.returncode, 43, result.stderr)
                args = json.loads((output / "runtime-call.json").read_text())
                self.assertEqual(args[:4], ["--engine", engine, "--rootless", "--output"])
                self.assertEqual(Path(args[4]), output)
                self.assertEqual(args[5:], ["candidate"])
                self.assertEqual(json.loads((output / "rootless.json").read_text())["exit_code"], 43)
                self.assertIn("Exact candidate config and layers preserved", (output / "engine.log").read_text())

    def test_default_output_still_works_with_two_arguments(self):
        result, output = self.run_wrapper("podman", "success", explicit_output=False)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads((output / "rootless.json").read_text())["status"], "passed")


if __name__ == "__main__":
    unittest.main()
