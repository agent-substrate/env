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

"""A scripted stand-in for a model, for running the CodeAct agent without one.

An OpenAI-compatible ``/v1/chat/completions`` endpoint (standard library
only) that answers with a fixed CodeAct trajectory: inspect the sandbox with
``terminal``, write a file with ``file_editor``, check it with ``terminal``,
then ``finish``. Each step is chosen by how many tool results the
conversation already holds, so the OpenHands agent loop, its tools and its
event stream all run for real; only the decisions are canned. Requests
without tools (a title, a summary) get plain text.

    python scripted_llm.py [port] [--daemon]     # default port 9999
"""

from __future__ import annotations

import json
import os
import sys
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

DEMO_DIR = "/tmp/codeact-demo"
STEPS = [
    ("Let me look at the sandbox first.", "terminal",
     {"command": f'head -1 /etc/os-release; echo "cwd: $(pwd)"; ls | head -5; mkdir -p {DEMO_DIR}'}),
    ("Now I create the file.", "file_editor",
     {"command": "create", "path": f"{DEMO_DIR}/hello.txt", "file_text": "hello from CodeAct\n"}),
    ("Checking the result.", "terminal",
     {"command": f"cat {DEMO_DIR}/hello.txt && wc -c {DEMO_DIR}/hello.txt"}),
    ("Done.", "finish",
     {"message": f"Inspected the sandbox and wrote {DEMO_DIR}/hello.txt."}),
]


class Handler(BaseHTTPRequestHandler):
    def do_POST(self) -> None:
        if not self.path.endswith("/chat/completions"):
            self.send_error(404)
            return
        req = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))) or b"{}")
        message: dict = {"role": "assistant"}
        finish_reason = "stop"
        if req.get("tools"):
            done = sum(1 for m in req.get("messages", []) if m.get("role") == "tool")
            text, tool, args = STEPS[min(done, len(STEPS) - 1)]
            message["content"] = text
            message["tool_calls"] = [{
                "id": f"call_{done}",
                "type": "function",
                "function": {"name": tool, "arguments": json.dumps(args)},
            }]
            finish_reason = "tool_calls"
        else:
            message["content"] = "CodeAct demo"
        body = json.dumps({
            "id": f"scripted-{time.time_ns()}",
            "object": "chat.completion",
            "created": int(time.time()),
            "model": req.get("model", "scripted"),
            "choices": [{"index": 0, "message": message, "finish_reason": finish_reason}],
            "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
        }).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, fmt: str, *args) -> None:
        sys.stderr.write("scripted-llm: " + fmt % args + "\n")


def daemonize(log_path: str) -> None:
    """Detach from the caller's session, so a short-lived shell that starts
    this (the agent-server's command endpoint ends its process group when the
    command returns) does not take it down."""
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
    args = [a for a in sys.argv[1:] if a != "--daemon"]
    port = int(args[0]) if args else 9999
    if "--daemon" in sys.argv:
        daemonize(f"/tmp/scripted_llm_{port}.log")
    ThreadingHTTPServer(("127.0.0.1", port), Handler).serve_forever()
