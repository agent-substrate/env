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

"""Full-stack integration tests against a real ate-env-api (cluster required).

Port-forward the API service first:

    kubectl -n ate-env port-forward svc/ate-env-api 17777:7777

then run:

    ATE_ENV_API_TARGET=127.0.0.1:17777 pytest tests/e2e/test_full_stack.py

Unlike test_guest_daemon.py, this exercises the whole path: environment
lifecycle against the Substrate control plane, and guest operations proxied
by ate-env-api through the atenet router into the environment's actor.
"""

from __future__ import annotations

import asyncio
import os
import time
import uuid

import pytest

from ate_env import (
    Client,
    EnvError,
    EnvironmentStatus,
    OutputSource,
    NotFoundError,
    ProcessStatus,
)

TARGET = os.environ.get("ATE_ENV_API_TARGET")
READY_TIMEOUT = float(os.environ.get("ATE_ENV_READY_TIMEOUT", "180"))
# Optional ActorTemplate override; the server default is "default-template".
TEMPLATE = os.environ.get("ATE_ENV_TEMPLATE")

pytestmark = pytest.mark.skipif(
    not TARGET,
    reason="set ATE_ENV_API_TARGET=host:port of a reachable ate-env-api",
)


@pytest.fixture
async def client():
    c = Client(TARGET)
    try:
        yield c
    finally:
        await c.close()


async def _wait_until_serving(env) -> None:
    """Retry a trivial shell until the environment's guest is reachable.

    Substrate resumes idle actors on demand, so traffic itself is the
    readiness gate; early requests can fail while the actor wakes up.
    """
    deadline = time.monotonic() + READY_TIMEOUT
    last_err: Exception | None = None
    while time.monotonic() < deadline:
        try:
            result = await env.shell("echo ready")
            if result.exit_code == 0 and result.stdout == "ready\n":
                return
        except EnvError as e:
            last_err = e
        await asyncio.sleep(2)
    raise AssertionError(f"environment {env.id} never became reachable: {last_err}")


async def _wait_for_status(client, env, want: EnvironmentStatus, timeout: float = 120) -> None:
    deadline = time.monotonic() + timeout
    status = None
    while time.monotonic() < deadline:
        status = (await env.info()).status
        if status == want:
            return
        await asyncio.sleep(2)
    raise AssertionError(f"environment {env.id} stuck in {status!r}, wanted {want!r}")


async def test_get_missing_environment(client):
    with pytest.raises(NotFoundError):
        await client.get(f"pye2e-missing-{uuid.uuid4().hex[:8]}")


async def test_full_lifecycle(client):
    env_id = f"pye2e-{uuid.uuid4().hex[:8]}"
    env = await client.create(env_id, template_name=TEMPLATE)
    assert env.id == env_id
    assert env.atespace == "ate-env"

    try:
        info = await env.info()
        assert info.id == env_id
        assert isinstance(info.status, EnvironmentStatus)

        await _wait_until_serving(env)

        # Shell: output, stderr, exit codes.
        result = await env.shell("echo out; echo err >&2; exit 7")
        assert result.stdout == "out\n"
        assert result.stderr == "err\n"
        assert result.exit_code == 7

        # Files: binary roundtrip and NotFound mapping through the proxy.
        content = os.urandom(128 * 1024 + 3)
        path = f"/tmp/pye2e-{uuid.uuid4().hex[:8]}.bin"
        assert await env.write_file(path, content) == len(content)
        assert await env.read_file_bytes(path) == content
        with pytest.raises(NotFoundError):
            await env.read_file_bytes("/tmp/pye2e-does-not-exist")

        # Processes: follow-stream, final state, kill.
        pid = await env.start_process(["sh", "-c", "echo one; echo two >&2"])
        stdout = bytearray()
        stderr = bytearray()
        async for chunk in env.stream_outputs(pid, follow=True):
            if chunk.source == OutputSource.STDOUT:
                stdout.extend(chunk.data)
            else:
                stderr.extend(chunk.data)
        assert stdout == b"one\n"
        assert stderr == b"two\n"
        proc = await env.wait(pid, poll_interval=0.5)
        assert proc.status == ProcessStatus.COMPLETED

        sleeper = await env.start_process(["sleep", "300"])
        assert await env.kill_process(sleeper) >= 128
        proc = await env.wait(sleeper, poll_interval=0.5)
        assert proc.status == ProcessStatus.TERMINATED

        # Lifecycle: suspend checkpoints the environment.
        await env.suspend()
        await _wait_for_status(client, env, EnvironmentStatus.SUSPENDED)
    finally:
        try:
            await env.delete()
        except EnvError as e:
            pytest.fail(f"cleanup delete of {env_id} failed: {e}")

    # Deletion is reflected in the control plane.
    deadline = time.monotonic() + 60
    while time.monotonic() < deadline:
        try:
            await env.info()
        except NotFoundError:
            return
        await asyncio.sleep(2)
    pytest.fail(f"environment {env_id} still exists after delete")
