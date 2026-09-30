import copy
import json
import os
from pathlib import Path
import subprocess
import unittest
from unittest.mock import patch

import release_image as release


ROOT = Path(__file__).resolve().parents[1]
SHA = "a" * 40
INDEX = "sha256:" + "1" * 64
AMD64 = "sha256:" + "2" * 64
ARM64 = "sha256:" + "3" * 64
ATTESTATION = "sha256:" + "4" * 64
ATTESTATION_ARM64 = "sha256:" + "5" * 64


def published(tag, **changes):
    return dict(tag_name=tag, draft=False, prerelease=False, published_at="2026-09-30", id=42, **changes)


def index():
    return {"digest": INDEX, "manifests": [
        {"digest": AMD64, "platform": {"os": "linux", "architecture": "amd64"}},
        {"digest": ARM64, "platform": {"os": "linux", "architecture": "arm64", "variant": "v8"}},
        *[{"digest": attestation, "platform": {"os": "unknown", "architecture": "unknown"},
           "annotations": {"vnd.docker.reference.type": "attestation-manifest",
                           "vnd.docker.reference.digest": subject}}
          for attestation, subject in ((ATTESTATION, AMD64), (ATTESTATION_ARM64, ARM64))],
    ]}


def config(arch="amd64", tag="v2.14.5", revision=SHA):
    return {"os": "linux", "architecture": arch, "config": {
        "User": "65532", "Entrypoint": ["/server"], "Volumes": {"/data": {}},
        "Labels": {"org.opencontainers.image.revision": revision,
                   "org.opencontainers.image.version": tag},
    }}


class ReleaseTests(unittest.TestCase):
    def test_stable_versions_and_numeric_order(self):
        for tag in ("v0.0.0", "v2.14.5", "v10.0.0"):
            self.assertEqual(len(release.version(tag)), 3)
        for tag in ("2.1.0", "v01.2.3", "v1.02.3", "v1.2.03", "v1.2.3-rc.1", "v1.2.3+build", "v1.2.3\n", "main"):
            with self.subTest(tag=tag), self.assertRaises(ValueError):
                release.version(tag)
        releases = [published(tag) for tag in ("v2.9.0", "v2.10.0", "v10.0.0", "v2.10.0")]
        releases += [dict(published("v11.0.0"), draft=True), dict(published("v12.0.0"), prerelease=True),
                     dict(published("v13.0.0"), published_at=None), published("v14.0.0-rc.1")]
        self.assertEqual(release.stable_releases(releases), ["v2.9.0", "v2.10.0", "v10.0.0"])

    def test_release_guards_and_main_ahead_recovery(self):
        args = (release.CANONICAL_REPOSITORY, "workflow_dispatch", "refs/heads/main", "v2.14.5", published("v2.14.5"))
        release.validate_release(*args)
        release.validate_release(args[0], "release", "refs/tags/v2.14.5", args[3], args[4], "42")
        invalid = [
            ("fork/repo", *args[1:]),
            (args[0], "pull_request", *args[2:]),
            (*args[:2], "refs/heads/feature", *args[3:]),
            (*args[:4], dict(args[4], tag_name="v2.14.4")),
            (*args[:4], dict(args[4], draft=True)),
            (*args[:4], dict(args[4], prerelease=True)),
            (*args[:4], dict(args[4], published_at=None)),
            (args[0], "release", args[2], args[3], args[4], "43"),
        ]
        for values in invalid:
            with self.subTest(values=values), self.assertRaises(ValueError):
                release.validate_release(*values)
        workflow = (ROOT / ".github/workflows/publish.yml").read_text()
        self.assertIn("org.opencontainers.image.revision=${{ needs.verify.outputs.source_commit }}", workflow)
        self.assertIn("ref: ${{ github.workflow_sha }}", workflow)
        self.assertIn("context: release-source", workflow)
        self.assertNotIn("github.sha", workflow)

    def test_paginated_releases(self):
        result = subprocess.CompletedProcess([], 0, json.dumps([[published("v2.10.0")], [published("v2.9.0")]]), "")
        with patch.object(release, "command", return_value=result) as run:
            self.assertEqual(release.published_tags("owner/repo"), ["v2.9.0", "v2.10.0"])
        self.assertIn("--paginate", run.call_args.args)
        self.assertIn("--slurp", run.call_args.args)
        with patch.object(release, "command", return_value=subprocess.CompletedProcess([], 0, "[[]]", "")):
            with self.assertRaises(ValueError):
                release.published_tags("owner/repo")

    def test_absence_is_not_authentication_or_network_failure(self):
        for error in ("image: not found\n", "MANIFEST_UNKNOWN: manifest unknown", "name unknown"):
            with self.subTest(error=error), patch.object(release, "command", return_value=subprocess.CompletedProcess([], 1, "", error)):
                self.assertIsNone(release.manifest("image:tag", absent_ok=True))
                with self.assertRaises(RuntimeError):
                    release.manifest("image:tag")
        for error in ("unauthorized", "denied", "unexpected status: 403 Forbidden", "DNS server not found", "timeout", "unexpected status: 404 Not Found"):
            with self.subTest(error=error), patch.object(release, "command", return_value=subprocess.CompletedProcess([], 1, "", error)):
                with self.assertRaises(RuntimeError):
                    release.manifest("image:tag", absent_ok=True)

    def test_malformed_index_fails_closed(self):
        release.platforms(index())
        changes = []
        for arch in ("arm64", "amd64"):
            missing = index()
            missing["manifests"] = [entry for entry in missing["manifests"] if entry["platform"]["architecture"] != arch]
            changes.append(missing)
        duplicate = index()
        duplicate["manifests"].append(copy.deepcopy(duplicate["manifests"][0]))
        changes.append(duplicate)
        for platform in ({"os": "windows", "architecture": "amd64"}, {"os": "linux", "architecture": "386"}):
            foreign = index()
            foreign["manifests"][0]["platform"] = platform
            changes.append(foreign)
        for field, value in (("vnd.docker.reference.type", "other"), ("vnd.docker.reference.digest", INDEX)):
            bad_attestation = index()
            bad_attestation["manifests"][2]["annotations"][field] = value
            changes.append(bad_attestation)
        for data in changes:
            with self.subTest(index=data), self.assertRaises(ValueError):
                release.platforms(data)
        for value in (None, "sha256:bad", "sha256:" + "A" * 64):
            with self.subTest(digest=value), self.assertRaises(ValueError):
                release.digest(value)


class ImageTests(unittest.TestCase):
    def setUp(self):
        self.index = index()
        self.configs = {AMD64: config(), ARM64: config("arm64")}
        self.attestation = {"layers": [
            {"mediaType": "application/vnd.in-toto+json", "annotations": {"in-toto.io/predicate-type": predicate}}
            for predicate in ("https://spdx.dev/Document", "https://slsa.dev/provenance/v1")
        ]}
        self.manifest = patch.object(release, "manifest", side_effect=lambda *_a, **_k: self.index).start()
        patch.object(release, "image_config", side_effect=lambda ref: self.configs[ref.split("@")[-1]]).start()
        patch.object(release, "command", side_effect=lambda *_a, **_k: subprocess.CompletedProcess([], 0, json.dumps(self.attestation), "")).start()
        self.addCleanup(patch.stopall)

    def validate(self):
        return release.validate_image("image", INDEX, SHA, "v2.14.5")

    def test_valid_image_and_wrong_source_on_either_platform(self):
        self.assertEqual(self.validate(), {"amd64": AMD64, "arm64": ARM64})
        for arch, platform_digest in (("amd64", AMD64), ("arm64", ARM64)):
            with self.subTest(architecture=arch):
                self.configs[platform_digest] = config(arch, revision="b" * 40)
                with self.assertRaises(ValueError):
                    self.validate()
                self.configs[platform_digest] = config(arch)

    def test_incorrect_version_runtime_and_digest(self):
        for bad in (config(tag="v2.14.4"), config("arm64")):
            self.configs[AMD64] = bad
            with self.assertRaises(ValueError):
                self.validate()
        for key, value in (("User", "0"), ("Entrypoint", ["sh"]), ("Volumes", {})):
            self.configs[AMD64] = config()
            self.configs[AMD64]["config"][key] = value
            with self.assertRaises(ValueError):
                self.validate()
        self.configs[AMD64] = config()
        self.index["digest"] = AMD64
        with self.assertRaises(ValueError):
            self.validate()

    def test_missing_sbom_or_provenance_fails(self):
        for predicate in ("https://spdx.dev/Document", "https://slsa.dev/provenance/v1"):
            with self.subTest(predicate=predicate):
                self.attestation["layers"] = [{"mediaType": "application/vnd.in-toto+json",
                                               "annotations": {"in-toto.io/predicate-type": predicate}}]
                with self.assertRaises(ValueError):
                    self.validate()

    def test_attestation_subject_mismatch_fails(self):
        self.attestation["subject"] = {"digest": INDEX}
        with self.assertRaises(ValueError):
            self.validate()


class PromotionTests(unittest.TestCase):
    def setUp(self):
        self.images = {}
        self.writes = []
        patch.object(release, "validate_image", return_value={"amd64": AMD64, "arm64": ARM64}).start()
        patch.object(release, "manifest", side_effect=lambda ref, **_kw: self.images.get(ref)).start()
        patch.object(release, "image_versions", side_effect=lambda _image, data: data["tag"]).start()
        patch.object(release, "copy_index", side_effect=self.copy).start()
        self.addCleanup(patch.stopall)

    def copy(self, image, image_digest, tag):
        self.writes.append(tag)
        self.images[f"{image}:{tag}"] = {"digest": image_digest, "tag": "v2.14.5"}

    def promote(self, tags):
        return release.promote("image", INDEX, SHA, "v2.14.5", tags)

    def test_highest_version_promotes_and_retry_never_rewrites_full_tag(self):
        tags = ["v2.14.4", "v2.14.5"]
        self.assertEqual(self.promote(tags), ["v2.14", "v2", "latest"])
        self.assertEqual(self.writes, ["v2.14.5", "v2.14", "v2", "latest"])
        self.writes.clear()
        self.promote(tags)
        self.assertNotIn("v2.14.5", self.writes)

    def test_old_recovery_and_out_of_order_publication_preserve_newer_tracks(self):
        for tags, aliases in ((["v2.14.5", "v2.14.6"], []),
                              (["v2.14.5", "v2.15.0"], ["v2.14"]),
                              (["v2.14.5", "v3.0.0"], ["v2.14", "v2"])):
            with self.subTest(tags=tags):
                self.writes.clear()
                self.assertEqual(self.promote(tags), aliases)
                self.assertNotIn("latest", self.writes)
        self.assertEqual(release.eligible_aliases("v2.10.0", ["v2.9.0", "v2.10.0"]), ["v2.10", "v2", "latest"])

    def test_existing_alias_cannot_downgrade_even_if_release_list_changes(self):
        self.images["image:latest"] = {"digest": AMD64, "tag": "v3.0.0"}
        self.assertEqual(self.promote(["v2.14.5"]), ["v2.14", "v2"])
        self.assertEqual(self.images["image:latest"]["tag"], "v3.0.0")

    def test_collision_or_invalid_current_alias_prevents_all_writes(self):
        self.images["image:v2.14.5"] = {"digest": AMD64}
        with self.assertRaises(ValueError):
            self.promote(["v2.14.5"])
        self.assertEqual(self.writes, [])
        self.images.clear()
        for alias, tag in (("latest", "dev"), ("v2", "v3.0.0"), ("v2.14", "v2.15.0")):
            with self.subTest(alias=alias):
                self.images.clear()
                self.images[f"image:{alias}"] = {"digest": AMD64, "tag": tag}
                with self.assertRaises(ValueError):
                    self.promote(["v2.14.5"])
                self.assertEqual(self.writes, [])
        with patch.object(release, "validate_image", side_effect=ValueError("unverified candidate")):
            with self.assertRaises(ValueError):
                self.promote(["v2.14.5"])
        self.assertEqual(self.writes, [])

    def test_partial_alias_failure_recovers_without_overwriting_full_version(self):
        def fail_latest(image, image_digest, tag):
            if tag == "latest":
                raise RuntimeError("registry unavailable")
            self.copy(image, image_digest, tag)
        with patch.object(release, "copy_index", side_effect=fail_latest), self.assertRaises(RuntimeError):
            self.promote(["v2.14.5"])
        self.writes.clear()
        self.promote(["v2.14.5"])
        self.assertEqual(self.writes, ["v2.14", "v2", "latest"])


class WorkflowTests(unittest.TestCase):
    def test_title_types_and_untrusted_text(self):
        workflow = (ROOT / ".github/workflows/pr-title.yml").read_text()
        script = "\n".join(line[10:] for line in workflow.split("        run: |\n")[1].splitlines())
        types = [section["type"] for section in json.loads((ROOT / "release-please-config.json").read_text())["packages"]["."]["changelog-sections"]]
        for title in [f"{kind}(actions): improve checks" for kind in types] + ["feat!: breaking change", "fix(api)!: breaking change"]:
            with self.subTest(title=title):
                result = subprocess.run(["bash", "-e", "-c", script], env=dict(os.environ, PR_TITLE=title), capture_output=True)
                self.assertEqual(result.returncode, 0, result.stdout)
        for title in ("bad title", "feat: ", "fix:  ", "feat: first\nfix: second", "fix: bad\rtitle", "$(exit 0)", "fix(): no scope"):
            with self.subTest(title=title):
                result = subprocess.run(["bash", "-e", "-c", script], env=dict(os.environ, PR_TITLE=title), capture_output=True)
                self.assertNotEqual(result.returncode, 0)
        self.assertIn("edited", workflow)
        self.assertNotIn("checkout", workflow)

    def test_required_result_truth_table(self):
        workflow = (ROOT / ".github/workflows/ci.yml").read_text()
        script = "\n".join(line[10:] for line in workflow.split("      - name: Aggregate required checks")[1].split("        run: |\n")[1].splitlines())
        keys = ["QUALITY_RESULT", "TESTS_RESULT", "BUILDS_RESULT", "VULNERABILITIES_RESULT", "DEPENDENCY_REVIEW_RESULT", "CODEQL_RESULT", "CONTAINER_RESULT"]
        for event in ("pull_request", "push", "merge_group", "workflow_dispatch"):
            base = dict.fromkeys(keys, "success")
            def run(values, results=None):
                env = dict(os.environ, **values, EVENT_NAME=event, RESULTS=results if results is not None else " ".join(values.values()))
                return subprocess.run(["bash", "-e", "-c", script], env=env, capture_output=True).returncode
            self.assertEqual(run(base), 0)
            self.assertNotEqual(run(base, "success " * 6), 0)
            for key in keys:
                for status in ("failure", "cancelled", "skipped", ""):
                    with self.subTest(event=event, job=key, status=status):
                        expected = key == "DEPENDENCY_REVIEW_RESULT" and status == "skipped" and event != "pull_request"
                        self.assertEqual(run(dict(base, **{key: status})) == 0, expected)

    def test_publication_gate_and_queue_contract(self):
        workflow = (ROOT / ".github/workflows/publish.yml").read_text()
        self.assertIn("needs: [verify, candidate, validate]", workflow)
        self.assertIn("needs.validate.result == 'success'", workflow)
        self.assertIn("group: publish-${{ github.repository }}\n  cancel-in-progress: false\n  queue: max", workflow)
        self.assertEqual(workflow.count("queue:"), 1)
        self.assertNotIn("setup-qemu", workflow)
        self.assertNotIn("RELEASE_APP_PRIVATE_KEY", workflow)
        self.assertNotIn("paths-ignore", (ROOT / ".github/workflows/ci.yml").read_text())
        security = (ROOT / ".github/workflows/security.yml").read_text()
        self.assertIn("make vulnerability", security)
        self.assertIn("scan-current", security)
        self.assertNotIn("docker build", security)


if __name__ == "__main__":
    unittest.main()
