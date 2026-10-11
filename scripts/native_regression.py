#!/usr/bin/env python3
"""Compare native release/work binaries against one reproducible input snapshot."""

import argparse
from contextlib import closing
from datetime import datetime, timezone
import difflib
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import io
import json
import math
import os
from pathlib import Path
import platform
import re
import secrets
import shutil
import signal
import socket
import sqlite3
import statistics
import subprocess
import sys
import tarfile
import tempfile
import threading
import time
from urllib.error import HTTPError, URLError
from urllib.request import build_opener, ProxyHandler, Request


ROOT = Path(__file__).resolve().parents[1]
MAPPING_URL = "https://github.com/anibridge/anibridge-mappings/releases/download/v3/mappings.json.zst"
SEASONS = ("ALL", "WINTER", "SPRING", "SUMMER", "FALL")
CATEGORIES = ("series", "series-new")
TOKEN = secrets.token_hex(24)
HTTP = build_opener(ProxyHandler({}))


def stable_tag(value):
    if not re.fullmatch(r"v(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)", value):
        raise ValueError(f"Expected a stable release tag such as v2.15.2, got {value!r}")
    return value


def sha256(data):
    return hashlib.sha256(data).hexdigest()


def write_json(path, value):
    path.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n")


def command(args, *, cwd=ROOT, timeout=360, env=None):
    result = subprocess.run(args, cwd=cwd, env=env, timeout=timeout,
                            capture_output=True, text=True, check=False)
    if result.returncode:
        raise RuntimeError(f"Command failed: {' '.join(map(str, args))}\n{result.stderr or result.stdout}")
    return result.stdout.strip()


def binary_info(path):
    path = path.resolve(strict=True)
    if not path.is_file() or not os.access(path, os.X_OK):
        raise ValueError(f"Binary is not executable: {path}")
    return {"path": str(path), "sha256": sha256(path.read_bytes()),
            "version": command([str(path), "--version"], timeout=10)}


def release_binary(args, output):
    if args.ref_binary:
        info = binary_info(args.ref_binary)
        tag = stable_tag(args.ref_tag or "v" + info["version"].removeprefix("v"))
        if info["version"] not in (tag, tag[1:]):
            raise ValueError(f"Reference binary reports {info['version']!r}, expected {tag}")
        return Path(info["path"]), dict(info, tag=tag, source="supplied binary")
    tag = args.ref_tag
    if not tag:
        release = json.loads(command(["gh", "release", "view", "--json", "tagName,isDraft,isPrerelease"]))
        if release["isDraft"] or release["isPrerelease"]:
            raise ValueError("Reference must be a published stable release")
        tag = release["tagName"]
    stable_tag(tag)
    source_repo, source_ref = ROOT, f"refs/tags/{tag}"
    try:
        commit = command(["git", "rev-parse", "--verify", f"{source_ref}^{{commit}}"])
    except RuntimeError:
        source_repo = output / "release-source.git"
        remote = command(["git", "remote", "get-url", "origin"])
        command(["git", "init", "--bare", str(source_repo)])
        command(["git", "fetch", "--depth=1", remote, source_ref], cwd=source_repo)
        source_ref = "FETCH_HEAD"
        commit = command(["git", "rev-parse", f"{source_ref}^{{commit}}"], cwd=source_repo)
    toolchain = command(["go", "version"])
    target = command(["go", "env", "GOOS", "GOARCH", "GOEXPERIMENT", "CGO_ENABLED", "GOFLAGS"])
    key = sha256(f"{commit}\n{toolchain}\n{target}\ntrimpath-s-w-v1".encode())
    cache = args.cache_dir.resolve() / key
    cache.mkdir(parents=True, exist_ok=True)
    binary, metadata = cache / "server", cache / "binary.json"
    if binary.exists() and metadata.exists():
        try:
            info = binary_info(binary)
            saved = json.loads(metadata.read_text())
            if info["sha256"] == saved["sha256"] and info["version"] == tag:
                return binary, dict(info, tag=tag, commit=commit, toolchain=toolchain, source="cached tag build")
        except (OSError, ValueError, RuntimeError, json.JSONDecodeError):
            pass
    source = output / "release-source"
    source.mkdir()
    archive = subprocess.run(["git", "archive", source_ref], cwd=source_repo,
                             capture_output=True, check=True, timeout=args.timeout).stdout
    with tarfile.open(fileobj=io.BytesIO(archive)) as entries:
        entries.extractall(source, filter="data")
    built = output / "released.bin"
    command(["go", "build", "-buildvcs=false", "-trimpath", "-ldflags=-s -w -X main.version=" + tag,
             "-o", str(built), "./cmd/server"], cwd=source, timeout=args.timeout)
    info = binary_info(built)
    if info["version"] != tag:
        raise ValueError(f"Built reference reports {info['version']!r}, expected {tag}")
    temporary = cache / f"server-{os.getpid()}"
    shutil.copy2(built, temporary)
    temporary.replace(binary)
    # A racing cache writer can at worst cause a later verified cache rebuild.
    write_json(metadata, {"sha256": info["sha256"], "commit": commit, "tag": tag})
    return built, dict(info, tag=tag, commit=commit, toolchain=toolchain, source="tag build")


def work_binary(args, output):
    if args.work_binary:
        return args.work_binary.resolve(), dict(binary_info(args.work_binary), source="supplied binary")
    commit = command(["git", "rev-parse", "HEAD"])
    dirty = bool(command(["git", "status", "--porcelain"]))
    binary = output / "working.bin"
    version = "work-" + commit[:12] + ("-dirty" if dirty else "")
    command(["go", "build", "-buildvcs=false", "-trimpath", "-ldflags=-s -w -X main.version=" + version,
             "-o", str(binary), "./cmd/server"], timeout=args.timeout)
    return binary, dict(binary_info(binary), commit=commit, dirty=dirty,
                        toolchain=command(["go", "version"]), source="working checkout")


def parse_shows(raw, *, nonempty=False):
    rows = json.loads(raw)
    if not isinstance(rows, list) or (nonempty and not rows):
        raise ValueError("Expected a nonempty show array" if nonempty else "Expected a show array")
    for row in rows:
        if not isinstance(row, dict) or type(row.get("id")) is not int or row["id"] <= 0:
            raise ValueError("Invalid show record in year snapshot")
    return rows


def december(rows, year):
    return [row for row in rows if isinstance(row.get("startDate"), dict)
            and row["startDate"].get("year") == year and row["startDate"].get("month") == 12]


def save_snapshot(path, year, full, prior, mapping, reference):
    full_rows = parse_shows(full, nonempty=True)
    prior_rows = december(parse_shows(prior), year-1)
    path.mkdir()
    payloads = {f"{year}.json": full, f"{year-1}.json": json.dumps(prior_rows).encode(),
                "mappings.json.zst": mapping}
    manifest = {"version": 1, "year": year, "captured_at": datetime.now(timezone.utc).isoformat(),
                "reference": reference, "counts": {str(year): len(full_rows), str(year-1): len(prior_rows)},
                "files": {}}
    for name, raw in payloads.items():
        (path / name).write_bytes(raw)
        manifest["files"][name] = {"sha256": sha256(raw), "bytes": len(raw)}
    write_json(path / "manifest.json", manifest)
    return load_snapshot(path, year)


def load_snapshot(path, year):
    manifest = json.loads((path / "manifest.json").read_text())
    names = {f"{year}.json", f"{year-1}.json", "mappings.json.zst"}
    if (not isinstance(manifest, dict) or manifest.get("version") != 1 or manifest.get("year") != year
            or not isinstance(manifest.get("files"), dict) or set(manifest["files"]) != names):
        raise ValueError("Snapshot version, year, or file list does not match this run")
    payloads = {}
    for name in sorted(names):
        raw = (path / name).read_bytes()
        metadata = manifest["files"][name]
        if (not isinstance(metadata, dict) or sha256(raw) != metadata.get("sha256")
                or len(raw) != metadata.get("bytes")):
            raise ValueError(f"Snapshot checksum/size mismatch: {name}")
        payloads[name] = raw
    full = parse_shows(payloads[f"{year}.json"], nonempty=True)
    prior = parse_shows(payloads[f"{year-1}.json"])
    if prior != december(prior, year-1):
        raise ValueError("Prior-year snapshot must contain only preceding December starts")
    if manifest.get("counts") != {str(year): len(full), str(year-1): len(prior)}:
        raise ValueError("Snapshot counts do not match captured records")
    if not payloads["mappings.json.zst"]:
        raise ValueError("Snapshot mapping is empty")
    return manifest, payloads


def seed_data(data, year, payloads, mapping_url):
    data.mkdir()
    with closing(sqlite3.connect(data / "cache.db")) as db:
        db.execute("CREATE TABLE year_cache (year INTEGER PRIMARY KEY, data BLOB NOT NULL, fetched_at INTEGER NOT NULL, last_hit INTEGER NOT NULL DEFAULT 0)")
        now = int(time.time())
        db.executemany("INSERT INTO year_cache VALUES (?, ?, ?, ?)",
                       [(y, payloads[f"{y}.json"], now, now) for y in (year, year-1)])
        db.commit()
    (data / "mappings.json.zst").write_bytes(payloads["mappings.json.zst"])
    write_json(data / "mappings.json.zst.meta.json", {"url": mapping_url, "etag": '"snapshot"'})


def cached_years(data, year):
    if not (data / "cache.db").exists():
        return None
    try:
        with closing(sqlite3.connect(data / "cache.db", timeout=1)) as db:
            rows = dict(db.execute("SELECT year, data FROM year_cache WHERE year IN (?, ?)", (year, year-1)))
        if set(rows) != {year, year-1}:
            return None
        parse_shows(rows[year], nonempty=True)
        parse_shows(rows[year-1])
        return rows
    except sqlite3.OperationalError:
        return None


class MappingReplay:
    def __init__(self, payload):
        class Handler(BaseHTTPRequestHandler):
            def do_HEAD(self):
                self.send_response(200)
                self.send_header("ETag", '"snapshot"')
                self.send_header("Content-Length", str(len(payload)))
                self.end_headers()

            def do_GET(self):
                self.do_HEAD()
                self.wfile.write(payload)

            def log_message(self, *_):
                pass
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.url = f"http://127.0.0.1:{self.server.server_port}/mappings.json.zst"

    def __enter__(self):
        self.thread.start()
        return self

    def __exit__(self, *_):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=2)


class Server:
    def __init__(self, binary, data, year, mapping_url, log):
        with socket.socket() as sock:
            sock.bind(("127.0.0.1", 0))
            port = sock.getsockname()[1]
        self.base = f"http://127.0.0.1:{port}"
        self.data, self.log = data, log
        env = dict(os.environ, PORT=str(port), CACHE_DB_PATH=str(data / "cache.db"),
                   MAPPING_PATH=str(data / "mappings.json.zst"), MAPPING_URL=mapping_url,
                   PREWARM_YEARS=f"{year},{year-1}", INCLUDE_TYPES="TV,ONA", EXCLUDE_TAGS="",
                   FILTER_FUTURE_ENABLED="true", DEBUG_ENDPOINTS_ENABLED="true", ADMIN_TOKEN=TOKEN,
                   LOG_LEVEL="info", ALLOW_INSECURE_MAPPING_URL="1" if mapping_url.startswith("http:") else "0",
                   NO_PROXY="127.0.0.1,localhost,::1", no_proxy="127.0.0.1,localhost,::1")
        with log.open("wb") as stream:
            self.process = subprocess.Popen([str(binary)], env=env, stdout=stream, stderr=subprocess.STDOUT)

    def request(self, path, *, method="GET", status=200, auth=True, timeout=10):
        if self.process.poll() is not None:
            raise RuntimeError(f"Server exited with {self.process.returncode}; see {self.log}")
        headers = {"Authorization": f"Bearer {TOKEN}"} if auth else {}
        req = Request(self.base + path, method=method, headers=headers)
        try:
            response = HTTP.open(req, timeout=timeout)
        except HTTPError as error:
            response = error
        with response:
            body = response.read()
            if response.status != status:
                raise RuntimeError(f"{method} {path}: HTTP {response.status}, expected {status}: {body[:300]!r}")
            return body, response.headers

    def ready(self, year, timeout):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if self.process.poll() is not None:
                raise RuntimeError(f"Server exited before readiness; see {self.log}")
            try:
                raw, _ = self.request("/health", timeout=min(2, max(.1, deadline-time.monotonic())))
                health = json.loads(raw)
                if (health["status"] == "ok" and health["checks"]["cache"]["status"] == "ok"
                        and health["checks"]["resolver"]["status"] == "ok" and cached_years(self.data, year)):
                    return
            except (URLError, TimeoutError, RuntimeError):
                pass
            time.sleep(.1)
        raise TimeoutError(f"Readiness timed out after {timeout}s; see {self.log}")

    def stop(self):
        if self.process.poll() is not None:
            raise RuntimeError(f"Server exited before shutdown ({self.process.returncode}); see {self.log}")
        self.process.terminate()
        try:
            self.process.wait(timeout=20)
        except subprocess.TimeoutExpired:
            self.process.kill()
            self.process.wait(timeout=5)
            raise TimeoutError(f"Shutdown exceeded 20s; see {self.log}")
        if self.process.returncode != 0:
            raise RuntimeError(f"Server shutdown failed ({self.process.returncode}); see {self.log}")

    def cleanup(self):
        if self.process.poll() is None:
            self.process.terminate()
            try:
                self.process.wait(timeout=20)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait(timeout=5)


def capture(args, binary, output, reference, owned):
    data = output / "capture"
    data.mkdir()
    server = Server(binary, data, args.year, MAPPING_URL, output / "capture.log")
    owned.append(server)
    server.ready(args.year, args.timeout)
    server.stop()
    rows = cached_years(data, args.year)
    if rows is None:
        raise ValueError("Capture did not persist both valid year arrays")
    return save_snapshot(output / "snapshot", args.year, rows[args.year], rows[args.year-1],
                         (data / "mappings.json.zst").read_bytes(), reference)


def response_array(raw, nonempty=False):
    rows = json.loads(raw)
    if not isinstance(rows, list) or (nonempty and not rows):
        raise ValueError("Expected nonempty ALL/series result" if nonempty else "List response is not an array")
    for row in rows:
        if (not isinstance(row, dict) or type(row.get("tvdbId")) is not int or row["tvdbId"] <= 0
                or not isinstance(row.get("title"), str)):
            raise ValueError("Malformed list response record")
    return rows


def list_path(year, season, category="series"):
    return f"/list?year={year}&season={season}&category={category}"


def matrix(server, output, label, year):
    results = {}
    for season in SEASONS:
        for category in CATEGORIES:
            raw, _ = server.request(list_path(year, season, category))
            (output / f"{label}-{season}-{category}.json").write_bytes(raw)
            results[f"{season}/{category}"] = response_array(raw, nonempty=(season, category) == ("ALL", "series"))
    return results


def compare(expected, actual, output, label):
    if expected == actual:
        return
    before = json.dumps(expected, indent=2, sort_keys=True).splitlines(keepends=True)
    after = json.dumps(actual, indent=2, sort_keys=True).splitlines(keepends=True)
    diff = "".join(difflib.unified_diff(before, after, fromfile="released", tofile=label))
    (output / f"{label}.diff").write_text(diff)
    raise ValueError(f"Ordered response mismatch; see {output / (label + '.diff')}")


def endpoints(server, year):
    for path in ("/list?season=INVALID", "/list?year=abc", "/list?category=invalid"):
        server.request(path, status=400)
    for path, allow in (("/list", "GET, HEAD"), ("/cache/clear", "POST")):
        _, headers = server.request(path, method="POST" if path == "/list" else "GET", status=405)
        if headers.get("Allow") != allow:
            raise ValueError(f"Incorrect Allow header for {path}")
    for path in ("/cache/stats", "/cache/clear"):
        server.request(path, method="POST" if path.endswith("clear") else "GET", status=404, auth=False)
    for path in ("/health", list_path(year, "ALL")):
        raw, _ = server.request(path, method="HEAD")
        if raw:
            raise ValueError(f"HEAD {path} unexpectedly returned a body")


def measure(servers, expected, year, samples):
    timings = {name: {} for name in servers}
    for season in ("SUMMER", "WINTER", "ALL"):
        values = {name: [] for name in servers}
        size = {}
        for index in range(samples):
            order = tuple(servers) if index % 2 == 0 else tuple(reversed(servers))
            for name in order:
                started = time.perf_counter()
                raw, _ = servers[name].request(list_path(year, season))
                values[name].append((time.perf_counter() - started) * 1000)
                if response_array(raw) != expected[f"{season}/series"]:
                    raise ValueError(f"Response changed during {name} {season} timing")
                size[name] = len(raw)
        if samples:
            for name in servers:
                ordered = sorted(values[name])
                timings[name][season] = {"samples": samples, "median_ms": statistics.median(ordered),
                                         "p95_ms": ordered[math.ceil(samples*.95)-1],
                                         "response_bytes": size[name], "shows": len(expected[f"{season}/series"])}
    return timings


def assert_no_fetch(log):
    for line in log.read_text().splitlines():
        try:
            event = json.loads(line)
        except json.JSONDecodeError:
            continue
        if event.get("task") == "year_fetch" and event.get("outcome") == "started":
            raise ValueError(f"Replay unexpectedly fetched upstream year data; see {log}")


def run(args, output, report, owned):
    started = time.monotonic()
    released, report["released"] = release_binary(args, output)
    working, report["working"] = work_binary(args, output)
    report["phase_seconds"]["builds"] = time.monotonic() - started
    print(f"Reference: {report['released']['tag']}; comparison year: {args.year}", flush=True)
    started = time.monotonic()
    if args.snapshot:
        manifest, payloads = load_snapshot(args.snapshot.resolve(), args.year)
        snapshot = output / "snapshot"
        snapshot.mkdir()
        write_json(snapshot / "manifest.json", manifest)
        for name, raw in payloads.items():
            (snapshot / name).write_bytes(raw)
    else:
        manifest, payloads = capture(args, released, output, report["released"], owned)
    report["snapshot"] = manifest
    report["phase_seconds"]["inputs"] = time.monotonic() - started
    started = time.monotonic()
    with MappingReplay(payloads["mappings.json.zst"]) as mapping:
        servers = {}
        for name, binary in (("released", released), ("working", working)):
            # Runtime paths stay under the OS temporary root even if artifacts do not.
            data = Path(tempfile.mkdtemp(prefix=f"sab-{name}-"))
            report.setdefault("runtime_directories", []).append(str(data))
            data.rmdir()
            seed_data(data, args.year, payloads, mapping.url)
            server = Server(binary, data, args.year, mapping.url, output / f"{name}.log")
            owned.append(server)
            server.ready(args.year, min(args.timeout, 30))
            endpoints(server, args.year)
            servers[name] = server
        expected = matrix(servers["released"], output, "released", args.year)
        compare(expected, matrix(servers["working"], output, "working", args.year), output, "working")
        report["response_counts"] = {key: len(rows) for key, rows in expected.items()}
        report["warm_http_timings"] = measure(servers, expected, args.year, args.requests)
        for name, binary in (("released", released), ("working", working)):
            previous = servers[name]
            previous.stop()
            assert_no_fetch(previous.log)
            restarted = Server(binary, previous.data, args.year, mapping.url, output / f"{name}-restart.log")
            owned.append(restarted)
            restarted.ready(args.year, min(args.timeout, 30))
            compare(expected, matrix(restarted, output, f"{name}-restart", args.year), output, f"{name}-restart")
            body, _ = restarted.request("/cache/clear", method="POST")
            if json.loads(body).get("status") != "ok":
                raise ValueError(f"{name}: cache clear failed")
            body, _ = restarted.request("/cache/stats")
            if json.loads(body).get("entries") != 0:
                raise ValueError(f"{name}: cache clear left rows")
            restarted.stop()
            assert_no_fetch(restarted.log)
    report["phase_seconds"]["comparison"] = time.monotonic() - started


def arguments(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--ref-tag", default=os.environ.get("NATIVE_REF_TAG"))
    parser.add_argument("--ref-binary", type=Path)
    parser.add_argument("--work-binary", type=Path)
    parser.add_argument("--snapshot", type=Path, help="Saved snapshot directory; skips live capture")
    parser.add_argument("--year", type=int, help="Historical year; defaults to snapshot year or last calendar year")
    parser.add_argument("--output", type=Path, help="New directory for retained results")
    parser.add_argument("--cache-dir", type=Path, default=Path(tempfile.gettempdir()) / "sab-native-binaries")
    parser.add_argument("--requests", type=int, default=25, help="Warm HTTP samples per season/build; 0 skips")
    parser.add_argument("--timeout", type=float, default=360, help="Build/capture deadline in seconds")
    args = parser.parse_args(argv)
    if args.year is None:
        args.year = (json.loads((args.snapshot / "manifest.json").read_text()).get("year")
                     if args.snapshot else datetime.now(timezone.utc).year - 1)
    now = datetime.now(timezone.utc).year
    if type(args.year) is not int or not now-9 <= args.year < now:
        parser.error(f"--year must be between {now-9} and {now-1}")
    if args.requests < 0 or args.requests > 10000:
        parser.error("--requests must be between 0 and 10000")
    if not math.isfinite(args.timeout) or args.timeout <= 0:
        parser.error("--timeout must be positive and finite")
    if args.ref_tag:
        stable_tag(args.ref_tag)
    return args


def main(argv=None):
    output = None
    owned = []
    report = {"status": "failed", "phase_seconds": {}, "host": platform.platform(),
              "started_at": datetime.now(timezone.utc).isoformat()}
    started = time.monotonic()
    def interrupted(signum, _):
        raise KeyboardInterrupt(f"Signal {signum}")
    signal.signal(signal.SIGTERM, interrupted)
    try:
        args = arguments(argv)
        if args.output:
            selected = args.output.resolve()
            selected.mkdir(parents=True, exist_ok=False)
            output = selected
        else:
            output = Path(tempfile.mkdtemp(prefix="sab-regression-"))
        print(f"Regression artifacts: {output}", flush=True)
        report["year"] = args.year
        run(args, output, report, owned)
        report["status"] = "passed"
    except (OSError, ValueError, KeyError, TypeError, AttributeError, RuntimeError,
            subprocess.SubprocessError, KeyboardInterrupt) as error:
        report["error"] = str(error) or type(error).__name__
        print(f"FAIL: {report['error']}", file=sys.stderr, flush=True)
    finally:
        signal.signal(signal.SIGTERM, signal.SIG_IGN)
        signal.signal(signal.SIGINT, signal.SIG_IGN)
        for server in reversed(owned):
            try:
                server.cleanup()
            except (OSError, subprocess.SubprocessError) as error:
                report["status"] = "failed"
                report.setdefault("cleanup_errors", []).append(str(error))
        report["elapsed_seconds"] = time.monotonic() - started
        if output and output.exists():
            write_json(output / "results.json", report)
            print(f"Results retained: {output}", flush=True)
        # Data artifacts are preserved in output/snapshot; writable runtime copies are disposable.
        for data in report.get("runtime_directories", []):
            shutil.rmtree(data, ignore_errors=True)
    if report["status"] == "passed":
        print("PASS: ordered responses, endpoints, persistence, clear, and shutdown")
        for name, seasons in report["warm_http_timings"].items():
            for season, timing in seasons.items():
                print(f"{name} {season}: median={timing['median_ms']:.3f}ms p95={timing['p95_ms']:.3f}ms n={timing['samples']}")
        return 0
    return 1


if __name__ == "__main__":
    sys.exit(main())
