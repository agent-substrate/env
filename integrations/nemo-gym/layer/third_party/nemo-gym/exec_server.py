#!/usr/bin/env python3
"""Zero-dependency HTTP exec server for sandbox containers."""
from __future__ import annotations
import base64, json, os, shutil, subprocess
from http.server import BaseHTTPRequestHandler, HTTPServer, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse
_BASH = shutil.which("bash")
_PORT = int(os.environ.get("TB_EXEC_PORT", "19542"))
_BIND = os.environ.get("TB_EXEC_BIND", "127.0.0.1")
class _H(BaseHTTPRequestHandler):
    def log_message(self, fmt, *a): pass
    def do_GET(self):
        p = urlparse(self.path)
        if p.path == "/health": self._ok({"ok": True})
        elif p.path == "/download":
            qs = parse_qs(p.query)
            paths = qs.get("path", [])
            if not paths: self._err(400, "missing ?path=")
            else: self._dl(paths[0])
        else: self._err(404, f"not found: {p.path}")
    def do_POST(self):
        p = urlparse(self.path)
        body = self._body()
        if p.path == "/exec": self._exec(body)
        elif p.path == "/upload": self._up(body)
        else: self._err(404, f"not found: {p.path}")
    def _exec(self, b):
        cmd = b.get("cmd")
        if not cmd: self._err(400, "missing 'cmd'"); return
        t = b.get("timeout", 300)
        try:
            cp = subprocess.run(cmd, shell=True, executable=_BASH, capture_output=True, timeout=t)
            self._ok({"stdout": cp.stdout.decode("utf-8", errors="replace"),
                       "stderr": cp.stderr.decode("utf-8", errors="replace"),
                       "rc": cp.returncode})
        except subprocess.TimeoutExpired:
            self._ok({"stdout":"","stderr":f"timed out after {t}s","rc":124})
        except Exception as e:
            self._ok({"stdout":"","stderr":str(e),"rc":-1})
    def _up(self, b):
        path, c = b.get("path"), b.get("content")
        if not path or c is None: self._err(400, "missing path/content"); return
        try:
            data = base64.b64decode(c)
            os.makedirs(os.path.dirname(path) or ".", exist_ok=True)
            with open(path, "wb") as f: f.write(data)
            m = b.get("mode")
            if m: os.chmod(path, int(m, 8))
            self._ok({"ok": True})
        except Exception as e: self._err(500, str(e))
    def _dl(self, path):
        if not os.path.isfile(path): self._err(404, f"not found: {path}"); return
        try:
            with open(path, "rb") as f: data = f.read()
            self.send_response(200)
            self.send_header("Content-Type", "application/octet-stream")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers(); self.wfile.write(data)
        except Exception as e: self._err(500, str(e))
    def _body(self):
        n = int(self.headers.get("Content-Length", 0))
        if n == 0: return {}
        try: return json.loads(self.rfile.read(n))
        except Exception: return {}
    def _ok(self, obj):
        p = json.dumps(obj).encode()
        self.send_response(200)
        self.send_header("Content-Type","application/json")
        self.send_header("Content-Length",str(len(p)))
        self.end_headers(); self.wfile.write(p)
    def _err(self, code, msg):
        p = json.dumps({"error": msg}).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(p)))
        self.end_headers(); self.wfile.write(p)
if __name__ == "__main__":
    s = ThreadingHTTPServer((_BIND, _PORT), _H)
    print(f"exec_server on {_BIND}:{_PORT}", flush=True)
    try: s.serve_forever()
    except KeyboardInterrupt: pass
    finally: s.server_close()
