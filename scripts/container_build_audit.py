"""Audit actual BuildKit inputs and compilation reuse in disposable storage."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import time
import uuid


ROOT = Path(__file__).resolve().parents[1]
FIXED = {"Dockerfile", ".dockerignore", "go.mod", "go.sum", "LICENSE", "NOTICE"}


def production(path):
    return path in FIXED or (
        path.startswith(("cmd/", "internal/")) and path.endswith(".go")
        and not path.endswith("_test.go") and not path.startswith("internal/testutil/"))


def inventory(directory):
    return {str(path.relative_to(directory)): hashlib.sha256(path.read_bytes()).hexdigest()
            for path in directory.rglob("*") if path.is_file()}


def compile_step(log, cached):
    match = re.search(r"^(#\d+) \[builder .*\] RUN --mount=.*go-build", log, re.MULTILINE)
    if not match:
        raise AssertionError("Compilation step missing from BuildKit output")
    hit = f"{match[1]} CACHED" in log
    if hit != cached:
        raise AssertionError(f"Expected compile cached={cached}, observed {hit}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, help="Retain logs and measurements here")
    args = parser.parse_args()
    builder = "sab-audit-" + uuid.uuid4().hex[:12]
    architecture = subprocess.check_output(["go", "env", "GOARCH"], text=True).strip()
    if architecture not in {"amd64", "arm64"}:
        raise SystemExit(f"Unsupported native architecture: {architecture}")
    output = args.output or Path(tempfile.mkdtemp(prefix="sab-build-evidence-"))
    output.mkdir(parents=True, exist_ok=True)
    metrics = {"revision": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip(),
               "architecture": architecture, "builder": builder, "builds": {}}

    def run(*command, label, failure=False):
        start = time.monotonic()
        result = subprocess.run(command, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
        (output / f"{label}.log").write_text(result.stdout)
        if failure == (result.returncode == 0):
            print("\n".join(result.stdout.splitlines()[-80:]), flush=True)
            raise RuntimeError(f"{label}: unexpected exit {result.returncode}; see {output / (label + '.log')}")
        if label.startswith("build-"):
            metrics["builds"][label] = {"seconds": round(time.monotonic() - start, 3),
                                      "compile_invocations": len(re.findall(r"/compile .* -p ", result.stdout))}
        print(f"{label}: exit={result.returncode}, seconds={time.monotonic() - start:.3f}", flush=True)
        return result.stdout

    try:
        run("docker", "buildx", "version", label="buildx-version")
        run("docker", "version", label="docker-version")
        run("docker", "buildx", "create", "--name", builder, "--driver", "docker-container", label="create")
        with tempfile.TemporaryDirectory(prefix="sab-audit-context-") as directory:
            scratch = Path(directory)
            context = scratch / "context"
            context.mkdir()
            # Copy only tracked public inputs, then synthesize harmless dirty files.
            paths = subprocess.check_output(["git", "ls-files", "-z"], cwd=ROOT).decode().split("\0")
            for name in filter(None, paths):
                source = ROOT / name
                if source.is_file() and not source.is_symlink():
                    target = context / name
                    target.parent.mkdir(parents=True, exist_ok=True)
                    shutil.copyfile(source, target)
            sentinels = ["credentials.pem", ".env", "appdata/cache.db", ".local-baseline/profile.pprof",
                         "internal/cache/private.key", "cmd/server/cache.db", "internal/mapping/artifact.bin",
                         "internal/nested/sample_test.go", "internal/testutil/nested/fixture.go"]
            for name in sentinels:
                target = context / name
                target.parent.mkdir(parents=True, exist_ok=True)
                target.write_text("harmless excluded sentinel\n")
            nested = context / "internal/audit/nested/input.go"
            nested.parent.mkdir(parents=True)
            nested.write_text("package nested\n")
            expected = {name: value for name, value in inventory(context).items() if production(name)}
            audit_recipe = scratch / "audit.Dockerfile"
            audit_recipe.write_text("FROM scratch\nCOPY . /\n")
            export = scratch / "export"
            run("docker", "buildx", "build", "--builder", builder, "--progress=plain", "-f", str(audit_recipe),
                "--output", f"type=local,dest={export}", str(context), label="context")
            actual = inventory(export)
            if actual != expected:
                raise AssertionError(f"Context mismatch: extra={actual.keys() - expected.keys()}, "
                                     f"missing={expected.keys() - actual.keys()}")
            metrics["context_bytes"] = sum((export / name).stat().st_size for name in actual)
            metrics["context_files"] = sorted(actual)
            # Instrument only this disposable recipe; the production flags/cache remain intact.
            recipe = context / "Dockerfile"
            recipe.write_text(recipe.read_text().replace("go build -trimpath", "go build -x -trimpath")
                              + "\nFROM scratch AS audit-binary\nCOPY --from=builder /server /server\n")

            def build(label, failure=False):
                dest = scratch / label
                log = run("docker", "buildx", "build", "--builder", builder, "--progress=plain",
                          "--platform", f"linux/{architecture}", "--build-arg", "VERSION=audit",
                          "--target", "audit-binary", "--output", f"type=local,dest={dest}",
                          str(context), label="build-" + label, failure=failure)
                if not failure:
                    version = subprocess.check_output([str(dest / "server"), "--version"], text=True).strip()
                    expected_version = "audit-edited" if label in {"source-edit", "module-edit"} else "audit"
                    if version != expected_version:
                        raise AssertionError(f"Incorrect rebuilt version: {version}")
                return log

            compile_step(build("cold"), False)
            compile_step(build("warm"), True)
            for name in sentinels:
                (context / name).write_text("changed harmless excluded sentinel\n")
            compile_step(build("excluded-edit"), True)
            source = context / "cmd/server/main.go"
            original = source.read_text()
            source.write_text(original.replace("fmt.Println(version)", 'fmt.Println(version + "-edited")', 1))
            compile_step(build("source-edit"), False)
            cold = metrics["builds"]["build-cold"]["compile_invocations"]
            edited = metrics["builds"]["build-source-edit"]["compile_invocations"]
            if not 0 < edited < cold:
                raise AssertionError(f"Compilation reuse missing: cold={cold}, edit={edited}")
            manifest = context / "go.mod"
            manifest.write_text(manifest.read_text() + "\n// build audit dependency input\n")
            module_log = build("module-edit")
            compile_step(module_log, False)
            match = re.search(r"^(#\d+) \[builder .*\] RUN go mod download", module_log, re.MULTILINE)
            if not match or f"{match[1]} CACHED" in module_log:
                raise AssertionError("Dependency input edit did not invalidate download layer")
            source.write_text(original + "\nthis is not valid Go\n")
            build("compile-failure", failure=True)
            manifest.write_text("not a Go module\n")
            build("module-failure", failure=True)
    finally:
        subprocess.run(["docker", "buildx", "rm", builder], check=False)
        (output / "measurements.json").write_text(json.dumps(metrics, indent=2) + "\n")
        print(json.dumps(metrics, indent=2), flush=True)
        print(f"Build audit evidence: {output}", flush=True)
        summary = os.environ.get("GITHUB_STEP_SUMMARY")
        if summary:
            with open(summary, "a") as stream:
                stream.write("\n### Disposable build audit\n```json\n" + json.dumps(metrics, indent=2) + "\n```\n")


if __name__ == "__main__":
    main()
