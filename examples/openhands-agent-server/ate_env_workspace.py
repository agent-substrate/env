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

"""An OpenHands workspace for the agent-server inside an ate-env environment.

ate-env-api forwards ``/v1alpha/envs/<env>/ports/<port>/...`` to that port in
the environment, prefix stripped. Used as the workspace host, that prefix is
all the OpenHands SDK needs: ``RemoteWorkspace`` appends its ``/api/...``
paths to it, and ``RemoteConversation`` builds its event WebSocket from the
same host, so commands, files and the agent loop's events all reach the
agent-server through the Substrate router. The agent-server's own API key
travels as the SDK sends it.

    pip install openhands-sdk==1.55.0
    SESSION_KEY=<key> python ate_env_workspace.py <env-id> [command ...]

Environment: ``API`` (ate-env-api address, default 127.0.0.1:7777),
``ATESPACE`` (default ate-env), ``PORT`` (agent-server port, default 8000),
``WORKDIR`` (the workspace's working directory: where commands run and what
relative paths resolve against; default ``/``). The image's WORKDIR does not
apply: the agent-server runs in ``/``, where the guest starts it, and the SDK
sends no directory unless given one, so set ``WORKDIR`` (``/testbed`` on a
SWE-bench image) or pass ``working_dir`` to ``workspace_for``.
"""

from __future__ import annotations

import os
import sys

from openhands.sdk.workspace import RemoteWorkspace


def agent_server_url(env_id: str, *, api: str | None = None, atespace: str | None = None,
                     port: int | None = None) -> str:
    """URL of the agent-server in ``env_id``, through ate-env-api."""
    api = api or os.environ.get("API", "127.0.0.1:7777")
    atespace = atespace or os.environ.get("ATESPACE", "ate-env")
    port = port or int(os.environ.get("PORT", "8000"))
    base = api if "://" in api else f"http://{api}"
    scope = "" if atespace == "ate-env" else f"/atespaces/{atespace}"
    return f"{base}/v1alpha{scope}/envs/{env_id}/ports/{port}"


def workspace_for(env_id: str, *, api_key: str | None = None, working_dir: str | None = None,
                  **url_kwargs) -> RemoteWorkspace:
    """A ``RemoteWorkspace`` bound to the agent-server in ``env_id``.

    ``working_dir`` defaults to ``$WORKDIR``, else ``/``. Pass it as ``cwd`` to
    ``execute_command``, which otherwise runs in the agent-server's directory.
    """
    return RemoteWorkspace(
        host=agent_server_url(env_id, **url_kwargs),
        api_key=api_key or os.environ["SESSION_KEY"],
        working_dir=working_dir or os.environ.get("WORKDIR") or "/",
    )


def main() -> None:
    if len(sys.argv) < 2:
        sys.exit(f"usage: {sys.argv[0]} <env-id> [command ...]")
    workspace = workspace_for(sys.argv[1])
    command = " ".join(sys.argv[2:]) or 'head -1 /etc/os-release; echo "cwd: $(pwd)"'
    result = workspace.execute_command(command, cwd=workspace.working_dir, timeout=60)
    print(f"exit {result.exit_code}\n{result.stdout}{result.stderr}", end="")


if __name__ == "__main__":
    main()
