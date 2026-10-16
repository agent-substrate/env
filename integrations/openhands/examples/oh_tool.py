# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""OpenHands tool actions inside the environment, for run_trajectory.py.

Runs with the runtime layer's Python, which has openhands-tools installed, so
file edits go through OpenHands' own ``file_editor`` and come back in the
exact form the agent sees. One long-lived server holds the editor, so the
heavy imports happen once and ``undo_edit`` sees earlier edits:

    OH_SESSION_API_KEYS_0=<key> /opt/oh/python/bin/python3 oh_tool.py --serve 9100 [--daemon]

Each request must carry the agent-server's session key in the
``X-Session-API-Key`` header; the server reads it from
``OH_SESSION_API_KEYS_0``, the agent-server's own variable, and refuses to
start without it. The agent-server removes that variable from the commands it
runs, so run_trajectory.py passes it in the start command. Endpoints:

- ``GET /health``: ``{"status": "ok"}``;
- ``POST /`` with ``{"tool": "file_editor", "args": {...}}``: the observation,
  ``{"text": ..., "is_error": ...}``;
- ``POST /shutdown``: stops the server, so it does not outlive the run or end
  up in a snapshot.

A missing or wrong key gets 401, a malformed body 400, both as JSON
``{"error": ...}``. Callers outside reach the server through ate-env-api
(``/v1alpha/envs/<env>/ports/9100/``); the router delivers to the actor's
address, not loopback, so it listens on all interfaces like the agent-server.
"""

from __future__ import annotations

import hmac
import json
import os
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

KEY_ENV = "OH_SESSION_API_KEYS_0"
MAX_BODY = 64 << 20


def run(step: dict) -> dict:
    if step.get("tool") != "file_editor":
        return {"text": f"oh_tool.py runs file_editor only, not {step.get('tool')!r}", "is_error": True}
    args = step.get("args", {})
    if not isinstance(args, dict):
        return {"text": "file_editor args must be a JSON object", "is_error": True}
    from openhands.tools.file_editor.impl import file_editor

    try:
        obs = file_editor(**args)
    except Exception as exc:  # noqa: BLE001 (a bad call is an observation, not a crash)
        return {"text": f"{type(exc).__name__}: {exc}", "is_error": True}
    return {"text": obs.text, "is_error": bool(obs.is_error)}


class Server(ThreadingHTTPServer):
    daemon_threads = True

    def __init__(self, address: tuple[str, int], key: str) -> None:
        super().__init__(address, Handler)
        self.key = key.encode()


class Handler(BaseHTTPRequestHandler):
    server: Server

    def _reply(self, payload: dict, status: int = 200) -> None:
        body = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _authorized(self) -> bool:
        given = self.headers.get("X-Session-API-Key", "").encode()
        if hmac.compare_digest(given, self.server.key):
            return True
        self._reply({"error": "missing or wrong X-Session-API-Key"}, 401)
        return False

    def _body(self) -> dict | None:
        """The request body as a JSON object, or None after a 400 reply."""
        try:
            length = int(self.headers.get("Content-Length", 0))
            if not 0 <= length <= MAX_BODY:
                raise ValueError(f"Content-Length {length} out of range")
            body = json.loads(self.rfile.read(length) or b"{}")
            if not isinstance(body, dict):
                raise TypeError("body must be a JSON object")
        except (ValueError, TypeError) as exc:  # UnicodeDecodeError is a ValueError
            self.close_connection = True
            self._reply({"error": f"malformed request: {exc}"}, 400)
            return None
        return body

    def do_GET(self) -> None:
        if not self._authorized():
            return
        if self.path.rstrip("/") in ("", "/health"):
            self._reply({"status": "ok"})
        else:
            self._reply({"error": f"no such path {self.path}"}, 404)

    def do_POST(self) -> None:
        if not self._authorized():
            return
        if self.path == "/shutdown":
            self._reply({"status": "shutting down"})
            threading.Thread(target=self.server.shutdown, daemon=True).start()
            return
        if self.path != "/":
            self._reply({"error": f"no such path {self.path}"}, 404)
            return
        step = self._body()
        if step is not None:
            self._reply(run(step))

    def log_message(self, fmt: str, *args) -> None:
        sys.stderr.write("oh-tool: " + fmt % args + "\n")


def daemonize(log_path: str) -> None:
    """Detach from the caller's session (the agent-server's command endpoint
    ends its process group when the command returns)."""
    if os.fork():
        os._exit(0)
    os.setsid()
    if os.fork():
        os._exit(0)
    log = os.open(log_path, os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o600)
    null = os.open(os.devnull, os.O_RDONLY)
    os.dup2(null, 0)
    os.dup2(log, 1)
    os.dup2(log, 2)


def main(argv: list[str]) -> None:
    usage = "usage: oh_tool.py --serve PORT [--daemon]"
    if len(argv) < 2 or argv[0] != "--serve" or not set(argv[2:]) <= {"--daemon"}:
        sys.exit(usage)
    try:
        port = int(argv[1])
    except ValueError:
        sys.exit(f"oh_tool.py: port {argv[1]!r} is not a number\n{usage}")
    if not 0 < port < 65536:
        sys.exit(f"oh_tool.py: port {port} out of range")
    key = os.environ.get(KEY_ENV, "")
    if not key:
        sys.exit(f"oh_tool.py: {KEY_ENV} is empty; set it to the agent-server's session key")
    os.environ.pop(KEY_ENV)  # not passed on to anything the editor starts
    server = Server(("0.0.0.0", port), key)  # bind before detaching: errors reach the caller
    if "--daemon" in argv:
        daemonize(f"/tmp/oh_tool_{port}.log")
    from openhands.tools.file_editor.impl import (
        file_editor,  # noqa: F401  (import once, up front, after the fork)
    )

    try:
        server.serve_forever()
    finally:
        server.server_close()


if __name__ == "__main__":
    main(sys.argv[1:])
