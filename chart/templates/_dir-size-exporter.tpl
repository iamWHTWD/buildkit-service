{{/*
Generic directory-size metrics exporter (stdlib-only Python).

Reports used bytes (recursive walk, cached) and total filesystem capacity
(statvfs) for one or more watched directories, as Prometheus text on /metrics.
Shared by the buildkit-service cache sidecar and the registry-mirror cache
sidecar so both expose identical metric shapes. Configured purely via env:

  METRICS_PORT           listen port (default 9300)
  METRIC_PREFIX          metric name prefix (default "dir")
  TARGETS                comma-separated "label=path" pairs, e.g.
                         "buildkit=/var/lib/buildkit". A bare path uses its
                         basename as the label.
  DIR_STATS_TTL_SECONDS  cache TTL for the recursive walk (default 60)

Only single { } braces are used so Helm leaves the body untouched.
*/}}
{{- define "buildkit-service.dirSizeExporterScript" -}}
#!/usr/bin/env python3
"""Generic directory-size metrics exporter (stdlib only)."""
import os
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(os.environ.get("METRICS_PORT", "9300"))
PREFIX = os.environ.get("METRIC_PREFIX", "dir")
TTL = float(os.environ.get("DIR_STATS_TTL_SECONDS", "60"))

_CACHE = {}


def parse_targets():
    targets = []
    for item in os.environ.get("TARGETS", "").split(","):
        item = item.strip()
        if not item:
            continue
        if "=" in item:
            label, path = item.split("=", 1)
        else:
            path = item
            label = os.path.basename(item.rstrip("/")) or item
        targets.append((label.strip(), path.strip()))
    return targets


TARGETS = parse_targets()


def used_bytes(path):
    now = time.time()
    cached = _CACHE.get(path)
    if cached and now - cached[0] < TTL:
        return cached[1]
    total = 0
    for root, _dirs, names in os.walk(path):
        for n in names:
            try:
                total += os.lstat(os.path.join(root, n)).st_size
            except OSError:
                pass
    _CACHE[path] = (now, total)
    return total


def total_bytes(path):
    try:
        st = os.statvfs(path)
        return st.f_blocks * st.f_frsize
    except OSError:
        return 0


def render():
    out = []
    out.append("# HELP %s_cache_used_bytes Used bytes under the watched directory." % PREFIX)
    out.append("# TYPE %s_cache_used_bytes gauge" % PREFIX)
    out.append("# HELP %s_cache_total_bytes Total capacity of the filesystem backing the directory." % PREFIX)
    out.append("# TYPE %s_cache_total_bytes gauge" % PREFIX)
    for label, path in TARGETS:
        is_dir = os.path.isdir(path)
        used = used_bytes(path) if is_dir else 0
        total = total_bytes(path) if is_dir else 0
        out.append('%s_cache_used_bytes{target="%s"} %d' % (PREFIX, label, used))
        out.append('%s_cache_total_bytes{target="%s"} %d' % (PREFIX, label, total))
    return "\n".join(out) + "\n"


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path.startswith("/healthz"):
            self._respond(200, "text/plain", "ok\n")
            return
        if self.path.startswith("/metrics") or self.path == "/":
            try:
                body = render()
            except Exception as exc:
                self._respond(500, "text/plain", "error: %s\n" % exc)
                return
            self._respond(200, "text/plain; version=0.0.4", body)
            return
        self._respond(404, "text/plain", "not found\n")

    def _respond(self, code, ctype, body):
        data = body.encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, *_args):
        pass


def main():
    ThreadingHTTPServer(("0.0.0.0", PORT), Handler).serve_forever()


if __name__ == "__main__":
    main()
{{- end -}}
