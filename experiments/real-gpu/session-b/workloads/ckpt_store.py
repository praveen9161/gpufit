#!/usr/bin/env python3
"""Tiny checkpoint store for the rescue experiments (stdlib only).

  PUT /<name>  stores the request body    GET /<name>  returns it (404 if absent)

Training pods upload their checkpoint here so a job moved to another node can
resume. Stand-in for real job storage (S3, NFS, ...); GPUFit itself never
touches checkpoints, it only decides when a job moves.
"""
import http.server
import os
import re
import sys

ROOT = sys.argv[1] if len(sys.argv) > 1 else "/data"
SAFE = re.compile(r"^/[A-Za-z0-9._-]+$")


class Handler(http.server.BaseHTTPRequestHandler):
    def _path(self):
        if not SAFE.match(self.path):
            self.send_error(400, "name must be letters, digits, . _ -")
            return None
        return os.path.join(ROOT, self.path[1:])

    def do_PUT(self):
        path = self._path()
        if not path:
            return
        n = int(self.headers.get("Content-Length", 0))
        tmp = path + ".tmp"
        with open(tmp, "wb") as f:
            while n > 0:
                chunk = self.rfile.read(min(n, 1 << 20))
                if not chunk:
                    break
                f.write(chunk)
                n -= len(chunk)
        os.replace(tmp, path)  # readers never see a half-written checkpoint
        self.send_response(204)
        self.end_headers()

    def do_GET(self):
        path = self._path()
        if not path:
            return
        if not os.path.exists(path):
            self.send_error(404)
            return
        size = os.path.getsize(path)
        self.send_response(200)
        self.send_header("Content-Length", str(size))
        self.end_headers()
        with open(path, "rb") as f:
            while chunk := f.read(1 << 20):
                self.wfile.write(chunk)

    def log_message(self, fmt, *args):
        print(f"{self.command} {self.path} {args[1] if len(args) > 1 else ''}", flush=True)


if __name__ == "__main__":
    os.makedirs(ROOT, exist_ok=True)
    print(f"checkpoint store on :8000, data in {ROOT}", flush=True)
    http.server.ThreadingHTTPServer(("", 8000), Handler).serve_forever()
