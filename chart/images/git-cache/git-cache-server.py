#!/usr/bin/env python3
import fcntl
import ipaddress
import os
import re
import shutil
import socket
import subprocess
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import urlsplit

CACHE_ROOT = Path(os.environ.get("GIT_CACHE_ROOT", "/var/cache/git-mirror"))
PORT = int(os.environ.get("GIT_CACHE_PORT", "8080"))
TTL_SECONDS = int(os.environ.get("GIT_CACHE_TTL_SECONDS", "300"))
MAX_ACTIVE_REQUESTS = int(os.environ.get("GIT_CACHE_MAX_ACTIVE_REQUESTS", "64"))
BUSY_TIMEOUT_SECONDS = int(os.environ.get("GIT_CACHE_BUSY_TIMEOUT_SECONDS", "30"))
MAX_REQUEST_BODY_BYTES = int(os.environ.get("GIT_CACHE_MAX_REQUEST_BODY_BYTES", str(10 * 1024 * 1024)))
REQUEST_READ_TIMEOUT_SECONDS = int(os.environ.get("GIT_CACHE_REQUEST_READ_TIMEOUT_SECONDS", "30"))
ALLOW_PRIVATE_UPSTREAMS = os.environ.get("GIT_CACHE_ALLOW_PRIVATE_UPSTREAMS", "false").lower() == "true"
UPSTREAM_SCHEME = os.environ.get("GIT_CACHE_UPSTREAM_SCHEME", "https")
# Upper bound for a single upstream git command (clone --mirror / remote
# update). Large repositories can need substantially more than 30 minutes for
# their first mirror clone.
GIT_TIMEOUT_SECONDS = int(os.environ.get("GIT_CACHE_GIT_TIMEOUT_SECONDS", "7200"))
# First-time `git clone --mirror` runs BEFORE the ACTIVE_REQUESTS gate and its
# index-pack memory is not bounded by the pack.* serving caps, so a burst of
# large-repo clones can OOM-kill the container and blow tens of GB of tmp data
# onto the volume between GC cycles. Gate concurrent clones separately.
MAX_CONCURRENT_CLONES = int(os.environ.get("GIT_CACHE_MAX_CONCURRENT_CLONES", "2"))
# Bound per-request pack-objects memory. git-http-backend execs git-upload-pack
# -> git pack-objects, which without limits spins up one delta-search window per
# thread and can use several GB on large repos. Capping threads and the per-pack
# window/delta-cache keeps each concurrent fetch within a predictable envelope so
# the container stays under its memory limit instead of being OOM-killed.
PACK_THREADS = os.environ.get("GIT_CACHE_PACK_THREADS", "1")
PACK_WINDOW_MEMORY = os.environ.get("GIT_CACHE_PACK_WINDOW_MEMORY", "256m")
PACK_DELTA_CACHE_SIZE = os.environ.get("GIT_CACHE_PACK_DELTA_CACHE_SIZE", "128m")
# Size-based GC. The mirror store otherwise grows without bound (TTL only governs
# refresh, never deletion), eventually tripping the volume sizeLimit and evicting
# the whole pod. When usage crosses the high-water mark we evict least-recently
# fetched mirrors down to GC_TARGET_RATIO * high-water. 0 disables GC.
MAX_DISK_BYTES = int(os.environ.get("GIT_CACHE_MAX_DISK_BYTES", "0"))
GC_INTERVAL_SECONDS = int(os.environ.get("GIT_CACHE_GC_INTERVAL_SECONDS", "300"))
GC_TARGET_RATIO = float(os.environ.get("GIT_CACHE_GC_TARGET_RATIO", "0.8"))
HOST_RE = re.compile(r"^[a-z0-9][a-z0-9.-]*(?::[0-9]{1,5})?$")
REPO_RE = re.compile(r"^/(?:((?:cache|fresh))/)?([^/]+)/(.+?\.git)(/.*)?$")
ACTIVE_REQUESTS = threading.BoundedSemaphore(MAX_ACTIVE_REQUESTS)
ACTIVE_CLONES = threading.BoundedSemaphore(MAX_CONCURRENT_CLONES)


def log(msg):
    sys.stdout.write(msg if msg.endswith("\n") else msg + "\n")
    sys.stdout.flush()


def pack_config_env():
    """git config key/value pairs (as GIT_CONFIG_* env) that bound pack memory.

    These are picked up by git-http-backend and propagate to the git-upload-pack
    / git pack-objects it execs, so the cap applies to the actual fetch packing.
    """
    pairs = [
        ("pack.threads", PACK_THREADS),
        ("pack.windowMemory", PACK_WINDOW_MEMORY),
        ("pack.deltaCacheSize", PACK_DELTA_CACHE_SIZE),
    ]
    env = {"GIT_CONFIG_COUNT": str(len(pairs))}
    for i, (key, value) in enumerate(pairs):
        env[f"GIT_CONFIG_KEY_{i}"] = key
        env[f"GIT_CONFIG_VALUE_{i}"] = value
    return env


def run_git(args, cwd=None):
    # pack_config_env also caps clone-side memory: git index-pack honors
    # pack.threads, keeping first-time mirror clones of huge repos bounded.
    env = os.environ.copy()
    env.update(pack_config_env())
    return subprocess.run(
        ["git", *args],
        cwd=cwd,
        env=env,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        timeout=GIT_TIMEOUT_SECONDS,
    )


def safe_repo_parts(path):
    match = REPO_RE.match(path)
    if not match:
        return None
    mode = match.group(1) or "ttl"
    host = match.group(2).lower()
    repo = match.group(3).strip("/")
    suffix = match.group(4) or ""
    if not HOST_RE.match(host) or ".." in host.split(":", 1)[0].split("."):
        raise ValueError("invalid host")
    if ":" in host:
        port = int(host.rsplit(":", 1)[1])
        if port < 1 or port > 65535:
            raise ValueError("invalid host port")
    parts = repo.split("/")
    if any(part in ("", ".", "..") for part in parts):
        raise ValueError("invalid repository path")
    return mode, host, repo, suffix


def validate_upstream_host(host):
    if ALLOW_PRIVATE_UPSTREAMS:
        return
    hostname = host.rsplit(":", 1)[0] if ":" in host else host
    try:
        addresses = {
            item[4][0]
            for item in socket.getaddrinfo(hostname, None, type=socket.SOCK_STREAM)
        }
    except socket.gaierror as exc:
        raise ValueError(f"upstream host resolution failed: {exc}") from exc
    if not addresses:
        raise ValueError("upstream host resolved to no addresses")
    for address in addresses:
        if not ipaddress.ip_address(address).is_global:
            raise ValueError("upstream host resolves to a non-public address")


def repo_paths(host, repo):
    repo_path = CACHE_ROOT / host / repo
    stamp_path = CACHE_ROOT / host / (repo + ".last_fetch")
    lock_key = (host + "/" + repo).replace("/", "__")
    lock_path = CACHE_ROOT / ".locks" / (lock_key + ".lock")
    return repo_path, stamp_path, lock_path


def repo_exists(repo_path):
    return (repo_path / "objects").is_dir()


def stamp_is_fresh(stamp_path):
    if TTL_SECONDS <= 0:
        return False
    return stamp_path.exists() and time.time() - stamp_path.stat().st_mtime <= TTL_SECONDS


def should_refresh_path(mode, method, path):
    if mode == "cache":
        return False
    # Smart HTTP discovers refs through info/refs. Refreshing before every
    # git-upload-pack POST is wasteful and can turn TTL=0 into multiple upstream
    # updates for a single clone/fetch.
    if method != "GET" or not path.endswith("/info/refs"):
        return False
    if mode == "fresh":
        return True
    return True


def ensure_mirror(host, repo, refresh=True):
    validate_upstream_host(host)
    repo_path, stamp_path, lock_path = repo_paths(host, repo)
    upstream = f"{UPSTREAM_SCHEME}://{host}/{repo}"
    lock_path.parent.mkdir(parents=True, exist_ok=True)
    repo_path.parent.mkdir(parents=True, exist_ok=True)

    if repo_exists(repo_path) and (not refresh or stamp_is_fresh(stamp_path)):
        return repo_path

    with open(lock_path, "w") as lock_file:
        fcntl.flock(lock_file, fcntl.LOCK_EX)
        exists = repo_exists(repo_path)
        expired = not stamp_is_fresh(stamp_path)

        if not exists:
            tmp_path = repo_path.with_suffix(repo_path.suffix + f".tmp.{os.getpid()}")
            if tmp_path.exists():
                shutil.rmtree(tmp_path)
            try:
                with ACTIVE_CLONES:
                    result = run_git(["clone", "--mirror", upstream, str(tmp_path)])
            except BaseException:
                # subprocess.TimeoutExpired (and anything else) must not leak
                # a partial tmp clone onto the volume.
                shutil.rmtree(tmp_path, ignore_errors=True)
                raise
            if result.returncode != 0:
                shutil.rmtree(tmp_path, ignore_errors=True)
                raise RuntimeError(result.stderr.strip() or "git clone --mirror failed")
            tmp_path.replace(repo_path)
            stamp_path.parent.mkdir(parents=True, exist_ok=True)
            stamp_path.touch()
            return repo_path

        if refresh and expired:
            run_git(["remote", "set-url", "origin", upstream], cwd=repo_path)
            # If refresh fails (non-zero exit or timeout), keep serving the
            # stale mirror and touch the stamp as a retry cooldown. A stale
            # cache is preferable to blocking every concurrent request on
            # repeated upstream failures.
            try:
                run_git(["remote", "update", "--prune", "--tags"], cwd=repo_path)
            except subprocess.TimeoutExpired:
                log(f"refresh of {host}/{repo} timed out; serving stale mirror")
            stamp_path.touch()
        return repo_path


def parse_cgi_headers(header_bytes):
    header_text = header_bytes.decode("iso-8859-1", "replace")
    status = 200
    headers = []
    for line in header_text.splitlines():
        if not line or ":" not in line:
            continue
        key, value = line.split(":", 1)
        key = key.strip()
        value = value.strip()
        if key.lower() == "status":
            try:
                status = int(value.split()[0])
            except (ValueError, IndexError):
                status = 200
        else:
            headers.append((key, value))
    return status, headers


class Handler(BaseHTTPRequestHandler):
    server_version = "git-cache/1.0"

    def do_GET(self):
        self.handle_git()

    def do_POST(self):
        self.handle_git()

    def handle_git(self):
        if self.path == "/healthz":
            self.respond_text(200, "ok\n")
            return
        self.handle_git_request()

    def handle_git_request(self):

        body = b""
        if self.command == "POST":
            raw_length = self.headers.get("Content-Length")
            if raw_length is None:
                self.respond_text(411, "Content-Length is required\n")
                return
            try:
                content_length = int(raw_length)
            except ValueError:
                self.respond_text(400, "invalid Content-Length\n")
                return
            if content_length < 0 or content_length > MAX_REQUEST_BODY_BYTES:
                self.respond_text(413, "request body too large\n")
                return
            self.connection.settimeout(REQUEST_READ_TIMEOUT_SECONDS)
            try:
                body = self.rfile.read(content_length)
            except TimeoutError:
                self.respond_text(408, "request body read timeout\n")
                return
            if len(body) != content_length:
                self.respond_text(400, "incomplete request body\n")
                return

        parsed = urlsplit(self.path)
        try:
            parts = safe_repo_parts(parsed.path)
            if parts is None:
                self.respond_text(404, "expected /<host>/<owner>/<repo>.git/...\n")
                return
            mode, host, repo, suffix = parts
            backend_path = f"/{host}/{repo}{suffix}"
            refresh = should_refresh_path(mode, self.command, backend_path)
            ensure_mirror(host, repo, refresh=refresh)
        except ValueError as exc:
            self.respond_text(403, f"{exc}\n")
            return
        except Exception as exc:
            self.respond_text(502, f"upstream mirror error: {exc}\n")
            return

        if not ACTIVE_REQUESTS.acquire(timeout=BUSY_TIMEOUT_SECONDS):
            self.respond_text(503, "git cache is busy\n")
            return
        try:
            self.serve_backend(parsed, backend_path, body)
        finally:
            ACTIVE_REQUESTS.release()

    def serve_backend(self, parsed, backend_path, body):

        env = os.environ.copy()
        env.update(
            {
                "GIT_PROJECT_ROOT": str(CACHE_ROOT),
                "GIT_HTTP_EXPORT_ALL": "1",
                "REQUEST_METHOD": self.command,
                "PATH_INFO": backend_path,
                "QUERY_STRING": parsed.query,
                "CONTENT_TYPE": self.headers.get("Content-Type", ""),
                "CONTENT_LENGTH": str(len(body)),
                "REMOTE_USER": "",
                "REMOTE_ADDR": self.client_address[0],
                "SERVER_PROTOCOL": self.request_version,
                "GATEWAY_INTERFACE": "CGI/1.1",
                "HTTP_GIT_PROTOCOL": self.headers.get("Git-Protocol", ""),
            }
        )
        # Cap pack-objects memory for this fetch (see pack_config_env).
        env.update(pack_config_env())

        proc = subprocess.Popen(
            ["git", "http-backend"],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=env,
        )
        assert proc.stdin is not None
        assert proc.stdout is not None
        assert proc.stderr is not None
        proc.stdin.write(body)
        proc.stdin.close()

        header = bytearray()
        split_marker = None
        while True:
            chunk = proc.stdout.read(1)
            if not chunk:
                break
            header.extend(chunk)
            if header.endswith(b"\r\n\r\n"):
                split_marker = b"\r\n\r\n"
                break
            if header.endswith(b"\n\n"):
                split_marker = b"\n\n"
                break
            if len(header) > 65536:
                proc.kill()
                self.respond_text(502, "invalid git-http-backend response\n")
                return

        if split_marker is None:
            proc.wait(timeout=10)
            stderr = proc.stderr.read(4096).decode("utf-8", "replace").strip()
            detail = stderr or "no stderr"
            self.respond_text(502, f"empty git-http-backend response: {detail}\n")
            return

        raw_header = bytes(header[: -len(split_marker)])
        status, headers = parse_cgi_headers(raw_header)
        self.send_response(status)
        for key, value in headers:
            if key.lower() not in {"status", "connection", "transfer-encoding"}:
                self.send_header(key, value)
        self.end_headers()

        while True:
            chunk = proc.stdout.read(1024 * 64)
            if not chunk:
                break
            self.wfile.write(chunk)
        proc.wait()

    def respond_text(self, status, body):
        data = body.encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "text/plain; charset=utf-8")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, fmt, *args):
        sys.stdout.write("%s - %s\n" % (self.address_string(), fmt % args))
        sys.stdout.flush()


def dir_size_bytes(path):
    total = 0
    for root, _dirs, files in os.walk(path, onerror=lambda _e: None):
        for name in files:
            try:
                total += os.lstat(os.path.join(root, name)).st_size
            except OSError:
                pass
    return total


def list_mirrors():
    """Return [(last_fetch_mtime, repo_path), ...] for every cached mirror."""
    mirrors = []
    if not CACHE_ROOT.is_dir():
        return mirrors
    for host_dir in CACHE_ROOT.iterdir():
        if not host_dir.is_dir() or host_dir.name == ".locks":
            continue
        for repo_path in host_dir.rglob("*.git"):
            if not repo_exists(repo_path):
                continue
            stamp_path = repo_path.parent / (repo_path.name + ".last_fetch")
            try:
                if stamp_path.exists():
                    mtime = stamp_path.stat().st_mtime
                else:
                    mtime = repo_path.stat().st_mtime
            except OSError:
                continue
            mirrors.append((mtime, repo_path))
    return mirrors


def evict_mirror(repo_path):
    """Delete a mirror under its lock. Returns bytes freed (0 if skipped)."""
    try:
        rel = repo_path.relative_to(CACHE_ROOT).parts
    except ValueError:
        return 0
    if len(rel) < 2:
        return 0
    host = rel[0]
    repo = "/".join(rel[1:])
    _repo_path, stamp_path, lock_path = repo_paths(host, repo)
    if not lock_path.exists():
        # No lock file yet means the repo was never served through the normal
        # path; create one so we can serialize against a concurrent clone.
        lock_path.parent.mkdir(parents=True, exist_ok=True)
        lock_path.touch()
    with open(lock_path, "w") as lock_file:
        try:
            fcntl.flock(lock_file, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError:
            # Busy (being fetched/refreshed) — skip, try again next cycle.
            return 0
        if not repo_exists(repo_path):
            return 0
        freed = dir_size_bytes(repo_path)
        shutil.rmtree(repo_path, ignore_errors=True)
        try:
            stamp_path.unlink()
        except OSError:
            pass
        return freed


def gc_stale_tmp():
    """Remove leftover *.tmp.* clone dirs (e.g. after an OOM kill).

    A live clone keeps updating file mtimes, so only dirs untouched for longer
    than the git command timeout are safe to delete.
    """
    if not CACHE_ROOT.is_dir():
        return
    cutoff = time.time() - GIT_TIMEOUT_SECONDS
    for host_dir in CACHE_ROOT.iterdir():
        if not host_dir.is_dir() or host_dir.name == ".locks":
            continue
        for tmp_path in host_dir.rglob("*.git.tmp.*"):
            if not tmp_path.is_dir():
                continue
            try:
                newest = max(
                    (os.lstat(os.path.join(root, name)).st_mtime
                     for root, _dirs, files in os.walk(tmp_path)
                     for name in files),
                    default=tmp_path.stat().st_mtime,
                )
            except OSError:
                continue
            if newest < cutoff:
                log(f"git-cache gc: removing stale tmp clone {tmp_path}")
                shutil.rmtree(tmp_path, ignore_errors=True)


def gc_once():
    if MAX_DISK_BYTES <= 0:
        return
    gc_stale_tmp()
    total = dir_size_bytes(CACHE_ROOT)
    if total <= MAX_DISK_BYTES:
        return
    target = int(MAX_DISK_BYTES * GC_TARGET_RATIO)
    mirrors = list_mirrors()
    mirrors.sort(key=lambda item: item[0])  # least-recently fetched first
    freed_total = 0
    evicted = 0
    for _mtime, repo_path in mirrors:
        if total - freed_total <= target:
            break
        freed = evict_mirror(repo_path)
        if freed:
            freed_total += freed
            evicted += 1
    log(
        "git-cache gc: usage=%d high=%d target=%d evicted=%d freed=%d"
        % (total, MAX_DISK_BYTES, target, evicted, freed_total)
    )


def gc_loop():
    while True:
        try:
            gc_once()
        except Exception as exc:  # never let the janitor die
            log("git-cache gc error: %s" % exc)
        time.sleep(max(GC_INTERVAL_SECONDS, 30))


def main():
    CACHE_ROOT.mkdir(parents=True, exist_ok=True)
    if MAX_DISK_BYTES > 0:
        janitor = threading.Thread(target=gc_loop, name="git-cache-gc", daemon=True)
        janitor.start()
        log(
            "git-cache gc enabled: high=%d target_ratio=%.2f interval=%ds"
            % (MAX_DISK_BYTES, GC_TARGET_RATIO, GC_INTERVAL_SECONDS)
        )
    server = ThreadingHTTPServer(("0.0.0.0", PORT), Handler)
    server.serve_forever()


if __name__ == "__main__":
    main()
