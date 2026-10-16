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

"""Run the OpenHands CodeAct agent inside an ate-env environment.

The agent is OpenHands' own default agent (``get_default_agent``: the
terminal, file editor and task tracker tools plus ``finish``), the successor
of CodeActAgent, unchanged. It runs on the agent-server inside the
environment as a ``RemoteConversation``; this script starts the conversation
through ate-env-api and prints the events the agent-server streams back over
its WebSocket. The image the environment runs is whatever the task needs; the
agent-server comes from the runtime layer.

The agent-server calls the model, so the model endpoint must be reachable
from inside the environment (Substrate's egress policy decides that), and
``LLM_API_KEY`` travels to the agent-server with the agent. Commands the
agent runs execute there as the same user, so a task (or a model steered by
one) can read that key: use a key scoped to this use, or ``--scripted``. The
conversation is deleted on the agent-server when the script ends.
``--scripted`` needs no model: it starts ``scripted_llm.py`` inside the
environment and points the agent at it, which replays a fixed CodeAct
trajectory through the real agent loop and tools.

    pip install openhands-sdk==1.55.0 openhands-tools==1.55.0
    SESSION_KEY=<key> LLM_MODEL=<litellm model> LLM_API_KEY=<key> [LLM_BASE_URL=<url>] \\
        python codeact_agent.py <env-id> ["task"]
    SESSION_KEY=<key> python codeact_agent.py --scripted <env-id>

Environment as for ate_env_workspace.py (API, ATESPACE, PORT, WORKDIR).
"""

from __future__ import annotations

import argparse
import os
import re
import time
from pathlib import Path

from openhands.sdk import LLM, Conversation
from openhands.sdk.event import (
    ActionEvent,
    AgentErrorEvent,
    MessageEvent,
    ObservationEvent,
)
from openhands.tools.preset.default import get_default_agent

from ate_env_workspace import workspace_for

DEFAULT_TASK = (
    "Inspect this sandbox: report the operating system, the working directory and the "
    "top-level files. Then create /tmp/codeact-demo/hello.txt containing 'hello from "
    "CodeAct', check its contents, and finish."
)
LAYER_PYTHON = "/opt/oh/python/bin/python3"
SCRIPTED_PORT = 9999


def start_scripted_llm(workspace) -> str:
    """Start scripted_llm.py inside the environment, on the layer's Python."""
    workspace.file_upload(Path(__file__).with_name("scripted_llm.py"), "/tmp/scripted_llm.py")
    workspace.execute_command(f"{LAYER_PYTHON} /tmp/scripted_llm.py {SCRIPTED_PORT} --daemon", timeout=30)
    probe = f"{LAYER_PYTHON} -c \"import socket; socket.create_connection(('127.0.0.1', {SCRIPTED_PORT}), 1)\""
    for _ in range(30):
        if workspace.execute_command(probe, timeout=10).exit_code == 0:
            return f"http://127.0.0.1:{SCRIPTED_PORT}/v1"
        time.sleep(1)
    raise RuntimeError("scripted model did not start inside the environment")


_CONTROL = re.compile(r"\x1b\[[0-9;?]*[A-Za-z]|\x1b\][^\x07]*\x07")


def short(value: object, limit: int = 160) -> str:
    """One line, terminal control sequences removed, at most ``limit`` chars."""
    text = " ".join(_CONTROL.sub("", str(value)).split())
    return text if len(text) <= limit else text[: limit - 1] + "…"


def on_event(event) -> None:
    """Print one line per agent step."""
    if isinstance(event, ActionEvent):
        action = event.action.model_dump(exclude_none=True) if event.action else {}
        action.pop("kind", None)
        print(f"  action  {event.tool_name}: {short(action)}")
    elif isinstance(event, ObservationEvent):
        print(f"  result  {event.tool_name}: {short(event.observation.text)}")
    elif isinstance(event, AgentErrorEvent):
        print(f"  error   {event.tool_name}: {short(event.error)}")
    elif isinstance(event, MessageEvent) and event.source == "user":
        text = " ".join(getattr(c, "text", "") for c in event.llm_message.content)
        print(f"  task    {short(text) or '(no text)'}")


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("env_id")
    parser.add_argument("task", nargs="?", default=DEFAULT_TASK)
    parser.add_argument("--scripted", action="store_true", help="replay a fixed trajectory instead of calling a model")
    args = parser.parse_args()

    workspace = workspace_for(args.env_id)
    if args.scripted:
        llm = LLM(model="openai/scripted", base_url=start_scripted_llm(workspace), api_key="scripted",
                  native_tool_calling=True, usage_id="agent")
    else:
        llm = LLM(model=os.environ["LLM_MODEL"], api_key=os.environ.get("LLM_API_KEY"),
                  base_url=os.environ.get("LLM_BASE_URL"), usage_id="agent")

    agent = get_default_agent(llm, cli_mode=True)
    # delete_on_close: the agent-server drops the conversation (its events, the
    # agent with the model settings) when this script closes it.
    conversation = Conversation(agent=agent, workspace=workspace, callbacks=[on_event], visualizer=None,
                                delete_on_close=True)
    started = time.monotonic()
    try:
        conversation.send_message(args.task)
        conversation.run()
        status = conversation.state.execution_status
        print(f"status: {status.value} after {time.monotonic() - started:.1f}s")
    finally:
        conversation.close()


if __name__ == "__main__":
    main()
