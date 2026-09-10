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

import pytest

from ate_env import (
    InvalidArgumentError,
    OutputSource,
    NotFoundError,
    ProcessStatus,
    ShellResult,
)
from ate_env._gen.ateenv.v1alpha import guest_pb2

from .fakes import FakeProc

STDOUT = guest_pb2.OUTPUT_SOURCE_STDOUT
STDERR = guest_pb2.OUTPUT_SOURCE_STDERR


async def test_metadata_injected(fake_stack):
    client, fakes = fake_stack
    env = client.env("dev1", atespace="team-a")
    await env.start_process(["true"])
    assert fakes.processes.last_env == ("dev1", "team-a")


async def test_missing_metadata_rejected(fake_stack):
    client, _ = fake_stack
    # Bypass the Env handle to call the raw stub without metadata, like a
    # misbehaving client would; the server contract is INVALID_ARGUMENT.
    with pytest.raises(InvalidArgumentError, match="x-env-id header is required"):
        try:
            await client._processes.StartProcess(guest_pb2.StartProcessRequest(command=["true"]))
        except Exception as e:
            from ate_env import map_rpc_error

            raise map_rpc_error(e) from e


async def test_start_process_passthrough(fake_stack):
    client, fakes = fake_stack
    env = client.env("dev1")
    pid = await env.start_process(
        ["pytest", "tests/"], cwd="/workspace", env={"CI": "1"}
    )
    proc = fakes.processes.procs[pid]
    assert proc.command == ["pytest", "tests/"]
    assert proc.cwd == "/workspace"
    assert proc.env == {"CI": "1"}


async def test_get_process_missing(fake_stack):
    client, _ = fake_stack
    env = client.env("dev1")
    with pytest.raises(NotFoundError, match='process "nope" not found'):
        await env.get_process("nope")


async def test_stream_outputs_yields_typed_chunks(fake_stack):
    client, fakes = fake_stack
    env = client.env("dev1")
    fakes.processes.next_procs.append(
        FakeProc(chunks=[(STDOUT, b"out1"), (STDERR, b"err1"), (STDOUT, b"out2")])
    )
    pid = await env.start_process(["job"])
    chunks = [chunk async for chunk in env.stream_outputs(pid, follow=True)]
    assert [(c.source, c.data) for c in chunks] == [
        (OutputSource.STDOUT, b"out1"),
        (OutputSource.STDERR, b"err1"),
        (OutputSource.STDOUT, b"out2"),
    ]


async def test_stream_outputs_respects_offsets(fake_stack):
    client, fakes = fake_stack
    env = client.env("dev1")
    fakes.processes.next_procs.append(
        FakeProc(chunks=[(STDOUT, b"abcdef"), (STDERR, b"123456")])
    )
    pid = await env.start_process(["job"])
    chunks = [
        (c.source, c.data)
        async for c in env.stream_outputs(pid, stdout_offset=4, stderr_offset=2)
    ]
    assert chunks == [(OutputSource.STDOUT, b"ef"), (OutputSource.STDERR, b"3456")]


async def test_stream_outputs_consumer_break_cancels(fake_stack):
    client, fakes = fake_stack
    env = client.env("dev1")
    fakes.processes.next_procs.append(
        FakeProc(chunks=[(STDOUT, b"a"), (STDOUT, b"b"), (STDOUT, b"c")])
    )
    pid = await env.start_process(["job"])
    seen = []
    async for chunk in env.stream_outputs(pid):
        seen.append(chunk.data)
        break
    assert seen == [b"a"]


async def test_shell_collects_output_and_exit_code(fake_stack):
    client, fakes = fake_stack
    env = client.env("dev1")
    fakes.processes.next_procs.append(
        FakeProc(
            chunks=[(STDOUT, b"hello "), (STDERR, b"warn\n"), (STDOUT, b"world\n")],
            exit_code=3,
            running_polls=2,  # forces the poll loop to actually iterate
        )
    )
    result = await env.shell("echo hello world; warn")
    proc = fakes.processes.procs["proc-1"]
    assert proc.command == ["sh", "-c", "echo hello world; warn"]
    assert result.stdout == "hello world\n"
    assert result.stderr == "warn\n"
    assert result.exit_code == 3
    assert proc.running_polls == 0


async def test_stream_outputs_empty(fake_stack):
    client, fakes = fake_stack
    env = client.env("dev1")
    fakes.processes.next_procs.append(FakeProc(chunks=[]))
    pid = await env.start_process(["true"])
    chunks = [chunk async for chunk in env.stream_outputs(pid, follow=True)]
    assert chunks == []


async def test_shell_empty_output(fake_stack):
    client, fakes = fake_stack
    env = client.env("dev1")
    fakes.processes.next_procs.append(FakeProc(chunks=[], exit_code=0))
    result = await env.shell("true")
    assert result == ShellResult(stdout="", stderr="", exit_code=0)


async def test_shell_invalid_utf8_replaced(fake_stack):
    client, fakes = fake_stack
    env = client.env("dev1")
    fakes.processes.next_procs.append(
        FakeProc(chunks=[(STDOUT, b"ok\xff\xfe")], exit_code=0)
    )
    result = await env.shell("binary")
    assert result.stdout == "ok��"


async def test_concurrent_shells(fake_stack):
    client, fakes = fake_stack
    env = client.env("dev1")
    for i in range(5):
        fakes.processes.next_procs.append(
            FakeProc(chunks=[(STDOUT, f"job-{i}\n".encode())], exit_code=i, running_polls=1)
        )
    results = await asyncio.gather(*(env.shell(f"job {i}") for i in range(5)))
    # StartProcess order over one channel is not guaranteed; match by output.
    assert sorted(r.stdout for r in results) == [f"job-{i}\n" for i in range(5)]
    assert sorted(r.exit_code for r in results) == list(range(5))


async def test_get_process_terminal_has_timestamps(fake_stack):
    client, fakes = fake_stack
    env = client.env("dev1")
    fakes.processes.next_procs.append(FakeProc(exit_code=0))
    pid = await env.start_process(["true"])
    proc = await env.get_process(pid)
    assert proc.status == ProcessStatus.COMPLETED
    assert proc.started_at is not None and proc.started_at.tzinfo is not None
    assert proc.finished_at is not None and proc.finished_at.tzinfo is not None


async def test_kill_missing_process(fake_stack):
    client, _ = fake_stack
    env = client.env("dev1")
    with pytest.raises(NotFoundError, match='process "nope" not found'):
        await env.kill_process("nope")


async def test_wait_polls_until_done(fake_stack):
    client, fakes = fake_stack
    env = client.env("dev1")
    fakes.processes.next_procs.append(FakeProc(exit_code=0, running_polls=3))
    pid = await env.start_process(["job"])
    proc = await env.wait(pid, poll_interval=0.001)
    assert proc.status == ProcessStatus.COMPLETED
    assert proc.exit_code == 0


async def test_wait_cancellable(fake_stack):
    client, fakes = fake_stack
    env = client.env("dev1")
    fakes.processes.next_procs.append(FakeProc(running_polls=10**9))  # never finishes
    pid = await env.start_process(["sleep", "infinity"])
    with pytest.raises(TimeoutError):
        async with asyncio.timeout(0.05):
            await env.wait(pid)


async def test_kill_process(fake_stack):
    client, fakes = fake_stack
    env = client.env("dev1")
    fakes.processes.next_procs.append(FakeProc(running_polls=10**9))
    pid = await env.start_process(["sleep", "infinity"])
    exit_code = await env.kill_process(pid)
    assert exit_code == 137
    proc = await env.get_process(pid)
    assert proc.status == ProcessStatus.TERMINATED
