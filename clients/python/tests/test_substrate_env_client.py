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

from __future__ import annotations

import asyncio
from unittest.mock import AsyncMock, patch
import grpc
import pytest

from ate_env.errors import (
    CommandStartError,
    InvalidArgumentError,
    NotFoundError,
    RpcError,
    SandboxProtocolError,
    SandboxUnavailableError,
)
from ate_env.runtime.substrate_env_client import SubstrateEnvClientRuntime
from .fakes import FakeProc


async def test_substrate_env_client_exec(fake_stack):
    client, fakes = fake_stack
    await client.create("dev1")

    fakes.processes.next_procs.append(
        FakeProc(
            chunks=[("stdout", b"hello from guest\n")],
            exit_code=0,
        )
    )

    rt = SubstrateEnvClientRuntime(client=client, env_id="dev1")
    res = await rt.exec_async("echo hello")
    assert res.ok
    assert res.stdout == "hello from guest\n"
    assert res.exit_code == 0
    assert not res.timed_out


async def test_substrate_env_client_timeout(fake_stack):
    client, fakes = fake_stack
    await client.create("dev1")

    # Script a process killed by signal 9 (SIGKILL), exit_code 137
    fakes.processes.next_procs.append(
        FakeProc(
            chunks=[("stdout", b"partial output")],
            signal=9,
        )
    )

    rt = SubstrateEnvClientRuntime(client=client, env_id="dev1")
    # Using timeout_s = 0.0 forces duration >= timeout_s
    res = await rt.exec_async("sleep 100", timeout_s=0.0)
    assert res.timed_out
    assert res.exit_code is None


async def test_substrate_env_client_file_io(fake_stack):
    client, fakes = fake_stack
    await client.create("dev1")

    rt = SubstrateEnvClientRuntime(client=client, env_id="dev1")
    await rt.write_file_async("/workspace/test.txt", b"sample content")
    data = await rt.read_file_bytes_async("/workspace/test.txt")
    assert data == b"sample content"


async def test_substrate_env_client_error_mapping(fake_stack):
    client, fakes = fake_stack
    await client.create("dev1")
    rt = SubstrateEnvClientRuntime(client=client, env_id="dev1")

    # 1. InvalidArgumentError -> SandboxProtocolError
    with patch.object(rt._env, "shell", side_effect=InvalidArgumentError("invalid argument")):
        with pytest.raises(SandboxProtocolError):
            await rt.exec_async("bad")

    # 2. NotFound on cwd / executable in guest -> CommandStartError (non-retryable)
    with patch.object(rt._env, "shell", side_effect=NotFoundError("cwd /missing does not exist")):
        with pytest.raises(CommandStartError) as exc_info:
            await rt.exec_async("bad")
        assert exc_info.value.retryable is False

    # 3. NotFound on environment lifecycle -> SandboxUnavailableError (retryable)
    with patch.object(rt._env, "shell", side_effect=NotFoundError('environment "dev1" not found')):
        with pytest.raises(SandboxUnavailableError) as exc_info:
            await rt.exec_async("bad")
        assert exc_info.value.retryable is True

    # 4. RpcError (UNAVAILABLE) -> SandboxUnavailableError
    with patch.object(rt._env, "shell", side_effect=RpcError("connection refused", code=grpc.StatusCode.UNAVAILABLE)):
        with pytest.raises(SandboxUnavailableError):
            await rt.exec_async("bad")


async def test_substrate_env_client_sync_exec(fake_stack):
    client, fakes = fake_stack
    await client.create("dev1")
    fakes.processes.next_procs.append(
        FakeProc(
            chunks=[("stdout", b"sync hello\n")],
            exit_code=0,
        )
    )
    rt = SubstrateEnvClientRuntime("dev1", endpoint=client.endpoint)
    try:
        res = await asyncio.to_thread(rt.exec, "echo hello")
        assert res.ok
        assert res.stdout == "sync hello\n"
    finally:
        await asyncio.to_thread(rt.close)
