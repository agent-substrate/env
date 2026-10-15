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

    /opt/oh/python/bin/python3 oh_tool.py --serve 9100 --daemon

It answers ``POST /`` with ``{"tool": "file_editor", "args": {...}}`` and
``GET /health``. Callers outside reach it through ate-env-api
(``/v1alpha/envs/<env>/ports/9100/``); the router delivers to the actor's
address, not loopback, so it listens on all interfaces like the agent-server.
"""

from __future__ import annotations

import json
import os
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


def run(step: dict) -> dict:
    if step.get("tool") != "file_editor":
        return {"text": f"oh_tool.py runs file_editor only, not {step.get('tool')!r}", "is_error": True}
    from openhands.tools.file_editor.impl import file_editor

    try:
        obs = file_editor(**step.get("args", {}))
    except Exception as exc:  # noqa: BLE001 (a bad call is an observation, not a crash)
        return {"text": f"{type(exc).__name__}: {exc}", "is_error": True}
    return {"text": obs.text, "is_error": bool(obs.is_error)}


class Handler(BaseHTTPRequestHandler):
    def _reply(self, payload: dict, status: int = 200) -> None:
        body = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self) -> None:
        self._reply({"status": "ok"})

    def do_POST(self) -> None:
        step = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))) or b"{}")
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
    log = os.open(log_path, os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o644)
    null = os.open(os.devnull, os.O_RDONLY)
    os.dup2(null, 0)
    os.dup2(log, 1)
    os.dup2(log, 2)


if __name__ == "__main__":
    args = sys.argv[1:]
    if not args or args[0] != "--serve":
        sys.exit("usage: oh_tool.py --serve PORT [--daemon]")
    port = int(args[1]) if len(args) > 1 and args[1].isdigit() else 9100
    if "--daemon" in args:
        daemonize(f"/tmp/oh_tool_{port}.log")
    from openhands.tools.file_editor.impl import (
        file_editor,  # noqa: F401  (import once, up front)
    )

    ThreadingHTTPServer(("0.0.0.0", port), Handler).serve_forever()
