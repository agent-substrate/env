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

Errors. A tool that reports an error (a non-zero exit status, a file_editor
error) is an observation like any other, as the agent would see it, and the
run goes on. A step that cannot be executed at all (a malformed step, an
unknown tool, an HTTP error, a timeout, a lost connection) is recorded as an
observation with ``"is_error": true`` and ``"failed": true``; the run stops
there unless ``--keep-going`` is given, and the script exits with status 1.
``--out`` is rewritten after each step, so it holds every step that ran even
when the script is interrupted.

Working directory. Terminal commands run in a step's ``cwd``, else in
``--workdir`` (default: ``$WORKDIR``, else ``/``). The image's WORKDIR does
not apply: the agent-server runs in ``/``, where the guest starts it, and the
SDK sends no directory of its own.

The tool runner is started at the beginning and stopped at the end (also on
an error), so it does not outlive the run or end up in a snapshot.

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
import shlex
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path

from ate_env_workspace import agent_server_url, workspace_for

LAYER_PYTHON = "/opt/oh/python/bin/python3"
TOOL_RUNNER = "/tmp/oh_tool.py"
TOOL_PORT = 9100


class StepFailed(Exception):
    """A step could not be executed (as opposed to a tool reporting an error)."""


class ToolRunner:
    """oh_tool.py inside the environment, reached through ate-env-api."""

    def __init__(self, workspace, env_id: str, key: str) -> None:
        self.workspace = workspace
        self.key = key
        self.url = agent_server_url(env_id, port=TOOL_PORT)

    def request(self, path: str, payload: dict | None = None, timeout: float = 120) -> dict:
        data = None if payload is None else json.dumps(payload).encode()
        req = urllib.request.Request(self.url + path, data=data, headers={
            "Content-Type": "application/json", "X-Session-API-Key": self.key})
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return json.loads(resp.read())

    def start(self) -> None:
        self.workspace.file_upload(Path(__file__).with_name("oh_tool.py"), TOOL_RUNNER)
        # The agent-server strips its key from the commands it runs; hand it over
        # here. The shell execs the command, so the key is in no process's argv.
        res = self.workspace.execute_command(
            f"OH_SESSION_API_KEYS_0={shlex.quote(self.key)} "
            f"{LAYER_PYTHON} {TOOL_RUNNER} --serve {TOOL_PORT} --daemon", timeout=30)
        if res.exit_code != 0:
            raise RuntimeError(f"oh_tool.py did not start: {(res.stdout + res.stderr).strip()}")
        deadline = time.monotonic() + 120
        while True:
            try:
                self.request("/health", timeout=10)
                return
            except urllib.error.HTTPError as exc:
                if exc.code == 401:
                    raise RuntimeError("oh_tool.py refused the session key") from None
            except OSError:
                pass
            if time.monotonic() > deadline:
                raise RuntimeError(f"oh_tool.py did not answer; see /tmp/oh_tool_{TOOL_PORT}.log in the environment")
            time.sleep(1)

    def stop(self) -> None:
        """Stop the server and remove its files; best effort."""
        try:
            self.request("/shutdown", {}, timeout=10)
        except (OSError, ValueError) as exc:
            print(f"warning: oh_tool.py shutdown: {exc}", file=sys.stderr)
        try:
            self.workspace.execute_command(f"rm -f {TOOL_RUNNER} /tmp/oh_tool_{TOOL_PORT}.log", timeout=30)
        except Exception as exc:  # noqa: BLE001 (cleanup must not hide the run's result)
            print(f"warning: removing oh_tool.py: {exc}", file=sys.stderr)


def run_step(workspace, tools: ToolRunner, step: object, workdir: str) -> dict:
    """The observation for one step. Raises StepFailed if it cannot be executed."""
    if not isinstance(step, dict) or not isinstance(step.get("args", {}), dict):
        raise StepFailed("a step must be an object with an object 'args'")
    tool, args = step.get("tool"), step.get("args", {})
    try:
        if tool == "terminal":
            if not isinstance(args.get("command"), str):
                raise StepFailed("terminal needs a string 'command'")
            res = workspace.execute_command(
                args["command"], cwd=args.get("cwd") or workdir, timeout=float(args.get("timeout", 120)))
            text = (res.stdout + res.stderr).rstrip()
            return {"text": f"{text}\n[exit {res.exit_code}]", "is_error": res.exit_code != 0}
        if tool == "file_editor":
            return tools.request("/", step)
        if tool == "finish":
            return {"text": str(args.get("message", "")), "is_error": False}
    except StepFailed:
        raise
    except urllib.error.HTTPError as exc:
        raise StepFailed(f"HTTP {exc.code} from the tool runner: {exc.read()[:500].decode(errors='replace')}") from exc
    except Exception as exc:  # transport errors, timeouts, bad arguments
        raise StepFailed(f"{type(exc).__name__}: {exc}") from exc
    raise StepFailed(f"unknown tool {tool!r}")


def write_out(path: Path | None, results: list) -> None:
    if path is None:
        return
    tmp = path.with_name(path.name + ".tmp")
    tmp.write_text(json.dumps(results, indent=2))
    tmp.replace(path)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("env_id")
    parser.add_argument("trajectory", type=Path)
    parser.add_argument("--out", type=Path, help="write the calls with their observations here, after each step")
    parser.add_argument("--workdir", default=os.environ.get("WORKDIR") or "/",
                        help="directory terminal steps without a cwd run in (default: $WORKDIR, else /)")
    parser.add_argument("--keep-going", action="store_true",
                        help="go on after a step that cannot be executed instead of stopping")
    args = parser.parse_args()

    steps = json.loads(args.trajectory.read_text())
    if not isinstance(steps, list):
        sys.exit(f"{args.trajectory}: want a JSON list of steps")
    key = os.environ["SESSION_KEY"]
    workspace = workspace_for(args.env_id, api_key=key, working_dir=args.workdir)
    tools = ToolRunner(workspace, args.env_id, key)
    results: list[dict] = []
    failed = 0
    try:
        t0 = time.monotonic()
        tools.start()
        print(f"tool runner up in {time.monotonic() - t0:.1f}s")
        started = time.monotonic()
        for i, step in enumerate(steps, 1):
            entry = dict(step) if isinstance(step, dict) else {"step": step}
            name = entry.get("tool", "?")
            if entry.get("thought"):
                print(f"[{i}] thought  {entry['thought']}")
            print(f"[{i}] {name!s:<11}{json.dumps(entry.get('args', {}))[:150]}")
            t0 = time.monotonic()
            try:
                obs = run_step(workspace, tools, step, args.workdir)
            except StepFailed as exc:
                obs = {"text": str(exc), "is_error": True, "failed": True}
                failed += 1
            results.append({**entry, "observation": obs, "seconds": round(time.monotonic() - t0, 2)})
            write_out(args.out, results)
            marker = "failed" if obs.get("failed") else "error" if obs["is_error"] else "ok"
            print("\n".join(f"      {line}" for line in obs["text"].splitlines()[-12:]) or "      (no output)")
            print(f"      -> {marker} in {results[-1]['seconds']}s")
            if name == "finish" or (obs.get("failed") and not args.keep_going):
                break
        print(f"{len(results)} steps in {time.monotonic() - started:.1f}s"
              + (f", {failed} could not be executed" if failed else ""))
    finally:
        write_out(args.out, results)
        tools.stop()
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
