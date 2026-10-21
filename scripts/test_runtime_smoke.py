"""Regress engine identity and candidate transport/promotion guardrails."""

import contextlib
import io
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import unittest
from unittest import mock
import urllib.error

import ci_scope
import runtime_smoke
from release_scope import runtime_path
from runtime_smoke import assert_archive_identity, assert_identity, assert_rootless


ROOT = Path(__file__).resolve().parents[1]


class RuntimeSmokeTests(unittest.TestCase):
    def test_archive_transport_requires_config_and_every_layer(self):
        before = {"Id": "sha256:" + "a" * 64, "RootFS": {"Layers": ["sha256:one", "sha256:two"]}}
        after = dict(before, Id="a" * 64)
        assert_archive_identity(before, after)
        for changed in (dict(after, Id="b" * 64),
                        dict(after, RootFS={"Layers": ["sha256:one", "sha256:changed"]}),
                        dict(after, RootFS={"Layers": ["sha256:two", "sha256:one"]}),
                        dict(after, Id=""), dict(after, RootFS={})):
            with self.subTest(changed=changed), self.assertRaises(ValueError):
                assert_archive_identity(before, changed)

    def test_rootless_is_engine_state_not_process_uid(self):
        assert_rootless("docker", {"SecurityOptions": ["name=rootless", "name=seccomp,profile=builtin"]})
        assert_rootless("podman", {"host": {"security": {"rootless": True}}})
        for engine, info in (("docker", {}), ("docker", {"SecurityOptions": ["name=seccomp"]}),
                             ("podman", {}), ("podman", {"host": {"security": {"rootless": False}}})):
            with self.subTest(engine=engine, info=info), self.assertRaises(ValueError):
                assert_rootless(engine, info)

    def test_nonroot_capabilities_and_escalation_are_all_required(self):
        identity = dict(uid=65532, gid=65532, CapEff="0000000000000000", NoNewPrivs="1")
        assert_identity(identity)
        for fields in ({"uid": 0}, {"gid": 0}, {"CapEff": "0000000000000001"}, {"NoNewPrivs": "0"}):
            with self.subTest(fields=fields), self.assertRaises(ValueError):
                assert_identity(dict(identity, **fields))

    def test_rootless_recipes_require_reviewed_v3_while_conventional_stays_v2(self):
        for path, field in (("examples/docker-compose.rootless.yml", "image:"),
                            ("examples/sonarr-anime-bridge.container", "Image=")):
            recipe = (ROOT / path).read_text()
            match = re.search(rf"^\s*{field}\s*(\S+)", recipe, re.MULTILINE)
            self.assertIsNotNone(match, path)
            self.assertIn(match[1], {"ghcr.io/calmcacil/sonarr-anime-bridge:v3",
                                     "ghcr.io/calmcacil/sonarr-anime-bridge:3.0.0",
                                     "ghcr.io/calmcacil/sonarr-anime-bridge:v3.0.0"}, path)
        self.assertIn("image: ghcr.io/calmcacil/sonarr-anime-bridge:v2",
                      (ROOT / "docker-compose.yml").read_text())

    def test_startup_failure_retains_actual_streams_and_survives_cleanup_timeout(self):
        for engine in ("docker", "podman"):
            with self.subTest(engine=engine), tempfile.TemporaryDirectory() as temporary:
                output = Path(temporary) / "evidence"
                calls = []

                def fake_engine(argv, **kwargs):
                    calls.append(argv)
                    action = argv[1]
                    stdout, stderr, status = "", "", 0
                    if action == "info":
                        stdout = json.dumps({"SecurityOptions": ["name=rootless"],
                                             "host": {"security": {"rootless": True}}})
                    elif action == "inspect":
                        stdout = json.dumps([{"Config": {"User": "65532"}, "Id": "sha256:" + "a" * 64}])
                    elif action == "port":
                        stdout = "127.0.0.1:18080"
                    elif action == "run":
                        if "--mount" in argv:
                            status, stderr = 125, "missing source"
                        elif argv[-1] == "identity":
                            stdout = json.dumps(dict(uid=65532, gid=65532, CapEff="0", NoNewPrivs="1"))
                        elif "--restart" in argv:
                            status, stderr = 37, "original startup failure token=runner-secret"
                    elif action == "logs":
                        # Engines can return partial logs even when the log request fails.
                        stdout, stderr, status = "actual application stdout\n", "actual application stderr\n", 1
                    elif action == "rm":
                        raise subprocess.TimeoutExpired(argv, 120, output=b"cleanup partial output")
                    return subprocess.CompletedProcess(argv, status, stdout, stderr)

                outage = urllib.error.HTTPError("http://fixture/outage", 503, "outage", {},
                                                io.BytesIO(b'{}'))
                with mock.patch.object(sys, "argv", ["runtime_smoke.py", "candidate", "--engine", engine,
                                                     "--rootless", "--fixture", temporary, "--output", str(output)]), \
                     mock.patch.dict(os.environ, {"SAB_PODMAN_ROOT": "", "TEST_TOKEN": "runner-secret"}), \
                     mock.patch.object(runtime_smoke.subprocess, "run", side_effect=fake_engine), \
                     mock.patch.object(runtime_smoke.urllib.request, "urlopen", side_effect=outage), \
                     contextlib.redirect_stdout(io.StringIO()), \
                     self.assertRaisesRegex(RuntimeError, "original startup failure"):
                    runtime_smoke.main()
                evidence = json.loads((output / "evidence.json").read_text())
                self.assertEqual(evidence["status"], "failed")
                self.assertTrue(evidence["scenarios"]["missing_bind_source"])
                self.assertIn("original startup failure", evidence["error"])
                self.assertTrue(evidence["cleanup_errors"])
                app_logs = list(output.glob("*-app.log"))
                self.assertEqual(len(app_logs), 1)
                self.assertEqual(app_logs[0].read_text(), "actual application stdout\nactual application stderr\n")
                self.assertIn("cleanup partial output", (output / "command-timeout.log").read_text())
                all_evidence = "".join(path.read_text() for path in output.iterdir())
                self.assertNotIn("runner-secret", all_evidence)
                self.assertIn("[REDACTED]", all_evidence)
                self.assertTrue(any(call[1:3] == ["volume", "rm"] for call in calls) or engine == "podman")

    def test_fixture_build_failure_keeps_diagnostics_and_partial_json(self):
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary) / "evidence"
            failed_build = subprocess.CompletedProcess(["go", "build"], 17, "compiler stdout\n", "compiler error\n")
            with mock.patch.object(sys, "argv", ["runtime_smoke.py", "candidate", "--output", str(output)]), \
                 mock.patch.object(runtime_smoke.subprocess, "run", return_value=failed_build), \
                 contextlib.redirect_stdout(io.StringIO()), \
                 self.assertRaises(subprocess.CalledProcessError) as caught:
                runtime_smoke.main()
            self.assertEqual(caught.exception.returncode, 17)
            self.assertEqual((output / "fixture-build.log").read_text(), "compiler stdout\ncompiler error\n")
            self.assertEqual(json.loads((output / "evidence.json").read_text())["status"], "failed")

    def test_evidence_write_failure_does_not_replace_original_error(self):
        with tempfile.TemporaryDirectory() as temporary:
            failed_build = subprocess.CompletedProcess(["go", "build"], 17, "", "compiler error")
            with mock.patch.object(sys, "argv", ["runtime_smoke.py", "candidate", "--output", temporary]), \
                 mock.patch.object(runtime_smoke.subprocess, "run", return_value=failed_build), \
                 mock.patch.object(runtime_smoke, "save_json", side_effect=OSError("disk full")), \
                 contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()) as errors, \
                 self.assertRaises(subprocess.CalledProcessError) as caught:
                runtime_smoke.main()
            self.assertEqual(caught.exception.returncode, 17)
            self.assertIn("Could not retain runtime evidence: disk full", errors.getvalue())

    def test_sanitization_keeps_diagnostics_not_credentials(self):
        with mock.patch.dict(os.environ, {"TEST_SECRET": "sensitive-value"}):
            sanitized = runtime_smoke.sanitize_log(
                'startup error sensitive-value https://user:pass@example.test/path '
                'Authorization: Bearer abc123 password=unsafe {"auth":"encoded-value"}')
        self.assertIn("startup error", sanitized)
        self.assertIn("example.test/path", sanitized)
        for secret in ("sensitive-value", "user:pass", "abc123", "unsafe", "encoded-value"):
            self.assertNotIn(secret, sanitized)

    def test_url_authority_tokens_and_uppercase_schemes_are_redacted(self):
        self.assertEqual(runtime_smoke.sanitize_log("https://opaque-token@registry.example/path@kept"),
                         "https://[REDACTED]@registry.example/path@kept")
        self.assertEqual(runtime_smoke.sanitize_log("HTTPS://opaque-token@registry.example/blob?sig=case-secret"),
                         "HTTPS://[REDACTED]@registry.example/blob?[REDACTED]")

    def test_signed_urls_and_escaped_secrets_are_redacted_in_parseable_json(self):
        urls = ["https://registry.example/blob?access_token=unknown-secret",
                "https://registry.example/blob?X-Amz-Signature=signed-secret&X-Amz-Credential=credential",
                "https://registry.example/blob?sig=another-secret",
                "https://registry.example/blob#token=fragment-secret"]
        with tempfile.TemporaryDirectory() as temporary, \
             mock.patch.dict(os.environ, {"TEST_TOKEN": 'secret-with-"quotes"\\and-newline\n'}):
            data = {"logs": urls, "nested": {"error": "registry failed " + os.environ["TEST_TOKEN"]}}
            path = Path(temporary) / "evidence.json"
            runtime_smoke.save_json(path, data)
            saved = json.loads(path.read_text())
            self.assertEqual(saved["logs"], ["https://registry.example/blob?[REDACTED]"] * len(urls))
            self.assertEqual(saved["nested"]["error"], "registry failed [REDACTED]")

    def test_workflows_always_upload_only_scoped_logs_and_json(self):
        for workflow in ("ci", "publish"):
            text = (ROOT / f".github/workflows/{workflow}.yml").read_text()
            self.assertIn("SMOKE_EVIDENCE_PATH: sab-smoke/${{ matrix.architecture }}", text)
            self.assertIn('--output "$RUNNER_TEMP/$SMOKE_EVIDENCE_PATH/docker"', text)
            for engine in ("docker", "podman"):
                self.assertIn(f'SAB_SMOKE_OUTPUT="$RUNNER_TEMP/$SMOKE_EVIDENCE_PATH/rootless-{engine}" sh ', text)
            upload = re.search(r"      - name: Retain sanitized runtime evidence\n(.*?)(?=      - name:|\n  [a-z]|\Z)",
                               text, re.DOTALL)
            self.assertIsNotNone(upload)
            self.assertIn("if: always()", upload[1])
            self.assertIn("actions/upload-artifact@ea165f8d65b6e75b540449e92b4886f43607fa02", upload[1])
            self.assertIn(f"name: {workflow}-runtime-${{{{ matrix.architecture }}}}-${{{{ github.run_attempt }}}}", upload[1])
            paths = re.findall(r"^            (.+)$", upload[1], re.MULTILINE)
            self.assertEqual(paths, ["${{ runner.temp }}/${{ env.SMOKE_EVIDENCE_PATH }}/*/*.log",
                                     "${{ runner.temp }}/${{ env.SMOKE_EVIDENCE_PATH }}/*/*.json"])
            self.assertIn("retention-days: 7", upload[1])

    def test_native_matrix_and_promotion_require_both_engines(self):
        ci = (ROOT / ".github/workflows/ci.yml").read_text()
        publish = (ROOT / ".github/workflows/publish.yml").read_text()
        for engine in ("docker", "podman"):
            self.assertIn(f"sh testdata/rootless-smoke.sh {engine} sonarr-anime-bridge:ci /tmp/sab-candidate.tar /tmp/sab-candidate.json", ci)
            self.assertIn(f'sh .automation/testdata/rootless-smoke.sh {engine} "$PLATFORM_REF"', publish)
        self.assertIn("needs.validate.result == 'success'", publish)
        self.assertIn("ref: ${{ github.workflow_sha }}", publish)
        self.assertEqual(ci.count("uses: docker/build-push-action@"), 1)
        self.assertNotIn("continue-on-error", ci + publish)
        for path in ("testdata/runtime-fixture/main.go", "testdata/rootless-smoke.sh", "scripts/runtime_smoke.py", "scripts/setup-rootless-ci.sh"):
            self.assertIn(path, ci_scope.REGISTRY_FILES)
            self.assertFalse(runtime_path(path))


if __name__ == "__main__":
    unittest.main()
