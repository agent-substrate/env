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

"""A real agent-server on localhost for the whole session, behind the fakes."""

from __future__ import annotations

import os
import socket
import subprocess
import sys
import time
import urllib.request

import pytest

from .fakes import FakeAteEnv, serve

SESSION_KEY = "test-session-key"
IMAGE = "docker.io/library/python@sha256:" + "a" * 64


def _free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


@pytest.fixture(scope="session")
def agent_server(tmp_path_factory):
    """host:port of an openhands agent-server, as the layer runs it."""
    pytest.importorskip("openhands.agent_server", reason="needs the [test] extra")
    port = _free_port()
    home = tmp_path_factory.mktemp("agent-server")
    env = dict(os.environ, OH_SESSION_API_KEYS_0=SESSION_KEY, OPENHANDS_SUPPRESS_BANNER="1")
    proc = subprocess.Popen(
        [sys.executable, "-m", "openhands.agent_server", "--host", "127.0.0.1", "--port", str(port)],
        cwd=home, env=env, stdout=subprocess.DEVNULL, stderr=subprocess.STDOUT,
    )
    deadline = time.monotonic() + 60
    while True:
        try:
            with urllib.request.urlopen(f"http://127.0.0.1:{port}/health", timeout=1):
                break
        except OSError:
            if proc.poll() is not None or time.monotonic() > deadline:
                proc.kill()
                pytest.fail("agent-server did not start")
            time.sleep(0.3)
    yield f"127.0.0.1:{port}"
    proc.terminate()
    try:
        proc.wait(timeout=10)
    except subprocess.TimeoutExpired:
        proc.kill()


@pytest.fixture
def stack(agent_server):
    """(api_url, state): fake ate-env-api and port proxy, agent-server on port 8000."""
    with serve(FakeAteEnv(upstreams={8000: agent_server})) as (api_url, state):
        yield api_url, state
