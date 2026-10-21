"""Exercise candidate bytes under hardening, controlled data, and engine lifecycle."""

import argparse
import concurrent.futures
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request
import uuid


ROOT = Path(__file__).resolve().parents[1]
EXPECTED_LIST = [{"tvdbId": 12345, "title": "Rootless fixture"}]


def assert_rootless(engine, info):
    if engine == "docker":
        actual = any(option == "name=rootless" for option in info.get("SecurityOptions", []))
    else:
        actual = info.get("host", {}).get("security", {}).get("rootless") is True
    if not actual:
        raise ValueError(f"{engine} is not an actual rootless engine")


def assert_identity(data):
    if (data.get("uid"), data.get("gid")) != (65532, 65532):
        raise ValueError(f"Incorrect container UID/GID: {data}")
    if int(data.get("CapEff", "1"), 16) != 0 or data.get("NoNewPrivs") != "1":
        raise ValueError(f"Missing capability/privilege hardening: {data}")


def assert_archive_identity(before, after):
    def identity(image):
        config = image.get("Id", image.get("ID", "")).removeprefix("sha256:")
        layers = image.get("RootFS", {}).get("Layers")
        if len(config) != 64 or any(char not in "0123456789abcdef" for char in config) or not layers:
            raise ValueError("Candidate inspection lacks a config digest or rootfs layers")
        return config, layers

    if identity(before) != identity(after):
        raise ValueError("rootless transport changed candidate config or layers")


def sanitize_log(text):
    """Keep test diagnostics, not runner credentials or authenticated URLs."""
    if isinstance(text, bytes):
        text = text.decode("utf-8", errors="replace")
    for name, value in os.environ.items():
        if len(value) >= 8 and re.search(r"TOKEN|PASSWORD|SECRET|PRIVATE_KEY", name, re.I):
            text = text.replace(value, "[REDACTED]")
    text = re.sub(r"(?i)(https?://)[^\s/?#\"'<>]*@", r"\1[REDACTED]@", text)
    # Registry errors can contain signed URLs with provider-specific credential keys.
    text = re.sub(r"(?i)(https?://[^\s?#\"'<>]+)[?#][^\s\"'<>]+", r"\1?[REDACTED]", text)
    text = re.sub(r"(?i)(\b(?:bearer|basic)\s+)[^\s\"']+", r"\1[REDACTED]", text)
    return re.sub(r"(?i)(\b(?:token|password|secret|auth)\b[\"']?\s*[:=]\s*[\"']?)[^\s\"',}]+",
                  r"\1[REDACTED]", text)


def save_log(path, text):
    path.write_text(sanitize_log(text), encoding="utf-8")


def save_json(path, data):
    def sanitize(value):
        if isinstance(value, dict):
            return {key: sanitize(item) for key, item in value.items()}
        if isinstance(value, list):
            return [sanitize(item) for item in value]
        return sanitize_log(value) if isinstance(value, str) else value

    path.write_text(json.dumps(sanitize(data), indent=2) + "\n", encoding="utf-8")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("image")
    parser.add_argument("--engine", choices=["docker", "podman"], default="docker")
    parser.add_argument("--rootless", action="store_true")
    parser.add_argument("--fixture", type=Path)
    parser.add_argument("--output", type=Path)
    args = parser.parse_args()
    work = Path(tempfile.mkdtemp(prefix="sab-runtime-"))
    output = args.output or Path(tempfile.mkdtemp(prefix="sab-runtime-evidence-"))
    output.mkdir(parents=True, exist_ok=True)
    prefix = "sab-runtime-" + uuid.uuid4().hex[:10]
    containers, volumes = [], []
    engine = [args.engine]
    if args.engine == "podman" and os.environ.get("SAB_PODMAN_ROOT"):
        engine += ["--root", os.environ["SAB_PODMAN_ROOT"], "--runroot", os.environ["SAB_PODMAN_RUNROOT"],
                   "--storage-driver", "vfs", "--cgroup-manager", "cgroupfs"]
    evidence = {"engine": args.engine, "rootless_required": args.rootless, "image": args.image,
                "host_uid": os.getuid() if hasattr(os, "getuid") else None,
                "host_gid": os.getgid() if hasattr(os, "getgid") else None,
                "status": "running", "scenarios": {}}

    def command(*arguments, check=True):
        try:
            result = subprocess.run([*engine, *arguments], text=True, stdout=subprocess.PIPE,
                                    stderr=subprocess.PIPE, timeout=120)
        except subprocess.TimeoutExpired as error:
            save_log(output / "command-timeout.log", sanitize_log(error.stdout or "")
                     + sanitize_log(error.stderr or ""))
            raise
        if result.returncode:
            with (output / "command-errors.log").open("a", encoding="utf-8") as log:
                log.write(sanitize_log(f"{args.engine} {arguments[0]}: exit {result.returncode}\n"
                                       + result.stdout + result.stderr + "\n"))
            if check:
                raise RuntimeError(f"{args.engine} {arguments[0]} failed: {result.stderr.strip()}")
        return result

    def inspect(name):
        return json.loads(command("inspect", name).stdout)[0]

    def poll(predicate, message, timeout=40):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if predicate():
                return
            time.sleep(0.2)
        raise AssertionError(message)

    def request(url):
        try:
            with urllib.request.urlopen(url, timeout=2) as response:
                return response.status, json.load(response)
        except urllib.error.HTTPError as error:
            with error:
                try:
                    body = json.load(error)
                except ValueError:
                    body = None
                return error.code, body
        except (OSError, ValueError):
            return 0, None

    fixture = args.fixture.resolve() if args.fixture else work / "fixture"
    hardening = ["--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--stop-timeout", "30"]
    identity = ["--user", "65532:65532"]
    if args.engine == "podman" and args.rootless:
        identity += ["--userns", "keep-id:uid=65532,gid=65532"]
    helper_mount = ["--volume", f"{fixture}:/fixture:ro"]

    def data_mount(label):
        if args.engine == "podman" and args.rootless:
            directory = work / label
            directory.mkdir(mode=0o700)
            return ["--volume", f"{directory}:/data"], directory
        volume = prefix + "-" + label
        command("volume", "create", volume)
        volumes.append(volume)
        return ["--volume", f"{volume}:/data"], None

    def helper(mount, action):
        return command("run", "--rm", *hardening, *identity, *helper_mount, *mount,
                       "--entrypoint", "/fixture", args.image, action).stdout

    fixture_name = prefix + "-upstream"

    def start(name, mount, mapping_path="/mappings.json.zst", restart=False, prewarm=None):
        containers.append(name)
        command("run", "-d", "--name", name, *hardening, *identity, *helper_mount, *mount,
                "--network", f"container:{fixture_name}",
                *( ["--restart", "unless-stopped"] if restart else [] ),
                "--env", "PORT=8080", "--env", f"PREWARM_YEARS={prewarm or time.localtime().tm_year}",
                "--env", f"MAPPING_URL=http://127.0.0.1:18080{mapping_path}",
                "--env", "ALLOW_INSECURE_MAPPING_URL=1", args.image)

    def stop(name, label):
        started = time.monotonic()
        command("stop", "--time", "30", name)
        elapsed = time.monotonic() - started
        state = inspect(name)["State"]
        result = command("logs", name)
        logs = result.stdout + result.stderr
        save_log(output / (label + ".log"), logs)
        if elapsed >= 30 or state.get("ExitCode") != 0:
            raise AssertionError(f"{label}: forced/failed stop: {state.get('ExitCode')}, {elapsed:.3f}s")
        if "year cache closed" not in logs or "bridge stopped" not in logs:
            raise AssertionError(f"{label}: missing completed storage/shutdown evidence")
        evidence["scenarios"][label] = {"seconds": round(elapsed, 3), "exit_code": state["ExitCode"]}

    try:
        if not args.fixture:
            build = subprocess.run(["go", "build", "-trimpath", "-buildvcs=false", "-o", str(fixture),
                                    "./testdata/runtime-fixture"], cwd=ROOT,
                                   env=dict(os.environ, CGO_ENABLED="0"), text=True,
                                   stdout=subprocess.PIPE, stderr=subprocess.PIPE)
            save_log(output / "fixture-build.log", build.stdout + build.stderr)
            build.check_returncode()
        info = json.loads(command("info", "--format", "json" if args.engine == "podman" else "{{json .}}").stdout)
        if args.rootless:
            assert_rootless(args.engine, info)
        evidence["engine_version"] = command("version").stdout
        image = inspect(args.image)
        if image["Config"]["User"] not in {"65532", "65532:65532", "nonroot"}:
            raise AssertionError("Candidate image does not retain its non-root default")
        evidence["candidate_id"] = image.get("Id", image.get("ID"))
        evidence["candidate_layers"] = image.get("RootFS", {}).get("Layers")
        if args.rootless and "@sha256:" in args.image:
            if args.image not in image.get("RepoDigests", []):
                raise AssertionError("Pulled candidate platform digest is not the requested digest")
        missing = work / "missing-source"
        rejected = prefix + "-missing"
        containers.append(rejected)
        result = command("run", "--name", rejected, *hardening, *identity, "--mount",
                         f"type=bind,source={missing},target=/data", args.image, check=False)
        if result.returncode == 0 or missing.exists():
            raise AssertionError("Missing bind source was accepted or created")
        evidence["scenarios"]["missing_bind_source"] = True
        containers.append(fixture_name)
        command("run", "-d", "--name", fixture_name, *hardening, *identity, *helper_mount,
                "--publish", "127.0.0.1::8080", "--publish", "127.0.0.1::18080",
                "--entrypoint", "/fixture", args.image, "serve")
        app_port = command("port", fixture_name, "8080/tcp").stdout.strip().rsplit(":", 1)[-1]
        fixture_port = command("port", fixture_name, "18080/tcp").stdout.strip().rsplit(":", 1)[-1]
        poll(lambda: request(f"http://127.0.0.1:{fixture_port}/outage")[0] == 503, "Fixture did not start")
        base = f"http://127.0.0.1:{app_port}"
        list_url = base + f"/list?year={time.localtime().tm_year}&season=SUMMER"
        mount, directory = data_mount("data")
        owner = (directory.stat().st_uid, directory.stat().st_gid) if directory else None
        identity_data = json.loads(helper(mount, "identity"))
        assert_identity(identity_data)
        evidence["identity"] = identity_data
        helper(mount, "seed")
        name = prefix + "-app"
        start(name, mount, restart=True)
        poll(lambda: request(base + "/health")[0] == 200, "Hardened candidate did not become healthy")
        command("exec", name, "/server", "--healthcheck")
        if request(list_url) != (200, EXPECTED_LIST):
            raise AssertionError(f"Incorrect fixed list: {request(list_url)}")
        config = inspect(name)
        host = config["HostConfig"]
        if not host["ReadonlyRootfs"] or not host["CapDrop"] or host.get("CapAdd"):
            raise AssertionError("Runtime inspection does not establish hardening")
        assert_identity(json.loads(command("exec", name, "/fixture", "identity").stdout))
        before = config["State"]["StartedAt"]
        command("exec", name, "/fixture", "crash", check=False)
        poll(lambda: inspect(name)["State"].get("Running") and inspect(name)["State"]["StartedAt"] != before,
             "Unexpected application exit did not restart")
        poll(lambda: request(list_url) == (200, EXPECTED_LIST), "Restart lost usable state")
        evidence["scenarios"]["unexpected_exit_restart"] = True
        stop(name, "idle-stop")
        time.sleep(1)
        if inspect(name)["State"].get("Running"):
            raise AssertionError("Explicit stop restarted")
        command("rm", "--volumes", name)
        # Keep the isolated namespace, but remove the mapping listener's process.
        command("exec", fixture_name, "/fixture", "disable-mapping")
        recreated = prefix + "-recreated"
        start(recreated, mount)
        poll(lambda: request(list_url) == (200, EXPECTED_LIST), "Recreation could not use persisted mappings/year")
        with concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool:
            requests = [pool.submit(request, list_url) for _ in range(12)]
            stop(recreated, "active-stop")
            for future in requests:
                future.result(timeout=5)
        helper(mount, "check")
        if directory and (directory.stat().st_uid, directory.stat().st_gid) != owner:
            raise AssertionError("Podman changed host-owned data-directory identity")
        if directory:
            evidence["host_data_owner_before_after"] = [list(owner), [directory.stat().st_uid, directory.stat().st_gid]]
        evidence["scenarios"]["same_storage_recreation"] = True
        command("rm", "--volumes", recreated)
        bad = work / "unwritable"
        bad.mkdir(mode=0o555)
        bad_mount = ["--volume", f"{bad}:/data:ro"]
        negative = prefix + "-negative"
        start(negative, bad_mount)
        poll(lambda: not inspect(negative)["State"].get("Running"), "Unwritable mount did not fail", timeout=10)
        if inspect(negative)["State"].get("ExitCode") == 0 or list(bad.iterdir()):
            raise AssertionError("Rejected mount reported success or created state")
        diagnostic = command("logs", negative)
        save_log(output / "unwritable-mount.log", diagnostic.stdout + diagnostic.stderr)
        if "directory" not in diagnostic.stdout + diagnostic.stderr:
            raise AssertionError("Rejected mount has no actionable diagnostic")
        evidence["scenarios"]["unwritable_mount"] = True
        # Restart the fixture namespace with its controlled upstream for outage/startup checks.
        command("rm", "--force", "--volumes", negative)
        command("rm", "--force", "--volumes", fixture_name)
        command("run", "-d", "--name", fixture_name, *hardening, *identity, *helper_mount,
                "--publish", f"127.0.0.1:{app_port}:8080", "--publish", f"127.0.0.1:{fixture_port}:18080",
                "--entrypoint", "/fixture", args.image, "serve")
        poll(lambda: request(f"http://127.0.0.1:{fixture_port}/outage")[0] == 503, "Restarted fixture did not start")
        outage_mount, _ = data_mount("outage")
        helper(outage_mount, "seed")
        outage = prefix + "-outage"
        start(outage, outage_mount, "/outage", restart=True)
        poll(lambda: request(base + "/health")[0] == 503, "Outage did not retain degraded listener")
        before = inspect(outage)["State"]["StartedAt"]
        time.sleep(1)
        if inspect(outage)["State"]["StartedAt"] != before:
            raise AssertionError("Degraded health caused process restart")
        stop(outage, "degraded-stop")
        command("rm", "--volumes", outage)
        startup_mount, _ = data_mount("startup")
        helper(startup_mount, "seed")
        startup = prefix + "-startup"
        start(startup, startup_mount, "/stall")
        poll(lambda: "loading mapping" in (command("logs", startup).stdout + command("logs", startup).stderr),
             "Stalled mapping startup did not begin")
        stop(startup, "startup-stop")
        evidence["scenarios"]["healthy_list_and_persistent_writes"] = True
        evidence["status"] = "passed"
        print(sanitize_log(json.dumps(evidence, indent=2)), flush=True)
    except BaseException as error:
        evidence["status"] = "failed"
        evidence["error"] = sanitize_log(f"{type(error).__name__}: {error}")
        raise
    finally:
        original_error = sys.exception()
        cleanup_errors = []

        def cleanup(*arguments):
            try:
                return command(*arguments, check=False)
            except Exception as error:
                cleanup_errors.append(sanitize_log(f"{arguments[0]}: {error}"))
                return None

        for name in reversed(containers):
            logs = cleanup("logs", name)
            if logs is not None and (logs.stdout or logs.stderr):
                try:
                    save_log(output / (name + ".log"), logs.stdout + logs.stderr)
                except OSError as error:
                    cleanup_errors.append(sanitize_log(f"logs: {error}"))
            cleanup("rm", "--force", "--volumes", name)
        for volume in volumes:
            cleanup("volume", "rm", volume)
        try:
            shutil.rmtree(work)
        except OSError as error:
            cleanup_errors.append(sanitize_log(f"work directory: {error}"))
        if cleanup_errors:
            evidence["cleanup_errors"] = cleanup_errors
            evidence["status"] = "failed"
        try:
            save_json(output / "evidence.json", evidence)
        except OSError as error:
            if original_error is None:
                raise
            print(sanitize_log(f"Could not retain runtime evidence: {error}"), file=sys.stderr)
        print(f"Runtime evidence: {output}", flush=True)
        # A cleanup timeout must not replace the assertion or startup failure.
        if cleanup_errors and original_error is None:
            raise RuntimeError("Runtime cleanup failed; see evidence.json")


if __name__ == "__main__":
    main()
