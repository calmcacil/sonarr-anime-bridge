"""Regress engine identity and candidate transport/promotion guardrails."""

from pathlib import Path
import unittest

import ci_scope
from release_scope import runtime_path
from runtime_smoke import assert_identity, assert_rootless


ROOT = Path(__file__).resolve().parents[1]


class RuntimeSmokeTests(unittest.TestCase):
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
