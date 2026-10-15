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

"""Execute a CodeAct trajectory in an ate-env environment from outside.

A trajectory is the list of tool calls a CodeAct agent makes, in OpenHands'
tool vocabulary: ``terminal`` (``command``, optional ``cwd`` and
``timeout``), ``file_editor`` (``command``, ``path``, ``file_text``,
``old_str``, ``new_str``, ``insert_line``, ``view_range``) and ``finish``
(``message``). This script connects to the agent-server in the environment
through ate-env-api and runs each call there: terminal commands through the
agent-server's command endpoint, file edits through OpenHands' own
``file_editor``, served inside the environment by ``oh_tool.py`` on the
runtime layer's Python and reached through ate-env-api like the agent-server.
Every observation is printed and, with ``--out``, written next to its call.

The decisions come from the trajectory file, so nothing inside the
environment needs to reach a model. The same loop with a model call before
each step is a client-side agent: the model stays wherever it is reachable,
the environment only executes.

    pip install openhands-sdk==1.55.0
    SESSION_KEY=<key> python run_trajectory.py <env-id> trajectories/hello.json [--out run.json]

Trajectory file: a JSON list of ``{"tool": ..., "args": {...}, "thought": ...}``
(``thought`` optional). Environment as for ate_env_workspace.py.
"""

from __future__ import annotations

import argparse
import json
import os
import time
import urllib.request
from pathlib import Path

from ate_env_workspace import agent_server_url, workspace_for

LAYER_PYTHON = "/opt/oh/python/bin/python3"
TOOL_RUNNER = "/tmp/oh_tool.py"
TOOL_PORT = 9100


def post_json(url: str, payload: dict, timeout: float = 120) -> dict:
    req = urllib.request.Request(url, data=json.dumps(payload).encode(),
                                 headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        return json.loads(resp.read())


def start_tool_runner(workspace, env_id: str) -> str:
    """Start oh_tool.py inside the environment and return its URL."""
    workspace.file_upload(Path(__file__).with_name("oh_tool.py"), TOOL_RUNNER)
    workspace.execute_command(f"{LAYER_PYTHON} {TOOL_RUNNER} --serve {TOOL_PORT} --daemon", timeout=30)
    url = agent_server_url(env_id, port=TOOL_PORT) + "/"
    deadline = time.monotonic() + 120
    while True:
        try:
            with urllib.request.urlopen(url, timeout=10) as resp:
                if resp.status == 200:
                    return url
        except OSError:
            if time.monotonic() > deadline:
                raise RuntimeError("oh_tool.py did not start inside the environment") from None
        time.sleep(1)


def run_step(workspace, tool_url: str, step: dict) -> dict:
    tool, args = step["tool"], step.get("args", {})
    if tool == "terminal":
        res = workspace.execute_command(
            args["command"],
            cwd=args.get("cwd") or os.environ.get("WORKDIR"),
            timeout=float(args.get("timeout", 120)),
        )
        text = (res.stdout + res.stderr).rstrip()
        return {"text": f"{text}\n[exit {res.exit_code}]", "is_error": res.exit_code != 0}
    if tool == "file_editor":
        return post_json(tool_url, step)
    if tool == "finish":
        return {"text": args.get("message", ""), "is_error": False}
    raise ValueError(f"unknown tool {tool!r}")


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("env_id")
    parser.add_argument("trajectory", type=Path)
    parser.add_argument("--out", type=Path, help="write the calls with their observations here")
    args = parser.parse_args()

    steps = json.loads(args.trajectory.read_text())
    workspace = workspace_for(args.env_id)
    t0 = time.monotonic()
    tool_url = start_tool_runner(workspace, args.env_id)
    print(f"tool runner up in {time.monotonic() - t0:.1f}s")

    results = []
    started = time.monotonic()
    for i, step in enumerate(steps, 1):
        if step.get("thought"):
            print(f"[{i}] thought  {step['thought']}")
        print(f"[{i}] {step['tool']:<11}{json.dumps(step.get('args', {}))[:150]}")
        t0 = time.monotonic()
        obs = run_step(workspace, tool_url, step)
        results.append({**step, "observation": obs, "seconds": round(time.monotonic() - t0, 2)})
        marker = "error" if obs["is_error"] else "ok"
        print("\n".join(f"      {line}" for line in obs["text"].splitlines()[-12:]) or "      (no output)")
        print(f"      -> {marker} in {results[-1]['seconds']}s")
        if step["tool"] == "finish":
            break
    print(f"{len(results)} steps in {time.monotonic() - started:.1f}s")
    if args.out:
        args.out.write_text(json.dumps(results, indent=2))


if __name__ == "__main__":
    main()
