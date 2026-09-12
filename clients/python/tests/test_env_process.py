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
from datetime import timedelta

import pytest

from ate_env import (
    FailedPreconditionError,
    InvalidArgumentError,
    NotFoundError,
    Process,
    ProcessExitedError,
    ProcessState,
    ShellResult,
    Signal,
)
from ate_env._gen.ateenv.v1alpha import guest_pb2

from .fakes import FakeProc


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
    proc = await env.start_process(
        ["pytest", "tests/"], cwd="/workspace", env={"CI": "1"}, stdin=True, timeout=90
    )
    assert isinstance(proc, Process)
    assert proc.env is env
    fake = fakes.processes.procs[proc.id]
    assert fake.command == ["pytest", "tests/"]
    assert fake.cwd == "/workspace"
    assert fake.env == {"CI": "1"}
    assert fake.stdin is True
    assert fake.timeout_seconds == 90


async def test_start_process_timeout_timedelta_and_default(fake_stack):
    client, fakes = fake_stack
    env = client.env("dev1")
    proc = await env.start_process(["job"], timeout=timedelta(minutes=2))
    assert fakes.processes.procs[proc.id].timeout_seconds == 120
    proc = await env.start_process(["job"])
    assert fakes.processes.procs[proc.id].timeout_seconds is None
    assert fakes.processes.procs[proc.id].stdin is False


async def test_info_missing(fake_stack):
    client, _ = fake_stack
    env = client.env("dev1")
    with pytest.raises(NotFoundError, match='process "nope" not found'):
        await env.process("nope").info()


async def test_info_running_then_exited(fake_stack):
    client, fakes = fake_stack
    env = client.env("dev1")
    fakes.processes.next_procs.append(FakeProc(exit_code=4, running_polls=1))
    proc = await env.start_process(["job"])
    info = await proc.info()
    assert info.state == ProcessState.RUNNING and info.running
    assert info.command == ("job",)
    assert info.pid > 0
    assert info.finished_at is None
    info = await proc.info()
    assert info.state == ProcessState.EXITED and not info.running
    assert info.exit_code == 4
    assert info.started_at is not None and info.started_at.tzinfo is not None
    assert info.finished_at is not None and info.finished_at.tzinfo is not None


async def test_output_follow_yields_chunks_then_exit(fake_stack):
    client, fakes = fake_stack
    env = client.env("dev1")
    fakes.processes.next_procs.append(
        FakeProc(chunks=[("stdout", b"out1"), ("stderr", b"err1"), ("stdout", b"out2")], exit_code=2)
    )
    proc = await env.start_process(["job"])
    outputs = [out async for out in proc.output(follow=True)]
    assert [(o.stdout, o.stderr) for o in outputs[:-1]] == [
        (b"out1", None),
        (None, b"err1"),
        (b"out2", None),
    ]
    assert outputs[-1].exit is not None
    assert outputs[-1].exit.exit_code == 2
    assert outputs[-1].exit.state == ProcessState.EXITED


async def test_output_snapshot_of_running_process_has_no_exit(fake_stack):
    client, fakes = fake_stack
    env = client.env("dev1")
    fakes.processes.next_procs.append(FakeProc(chunks=[("stdout", b"partial")], running_polls=5))
    proc = await env.start_process(["job"])
    outputs = [out async for out in proc.output()]
    assert [o.stdout for o in outputs] == [b"partial"]
    assert all(o.exit is None for o in outputs)


async def test_output_respects_offsets(fake_stack):
    client, fakes = fake_stack
    env = client.env("dev1")
    fakes.processes.next_procs.append(
        FakeProc(chunks=[("stdout", b"abcdef"), ("stderr", b"123456")])
    )
    proc = await env.start_process(["job"])
    outputs = [
        (o.stdout, o.stderr)
        async for o in proc.output(stdout_offset=4, stderr_offset=2)
        if o.exit is None
    ]
    assert outputs == [(b"ef", None), (None, b"3456")]


async def test_output_consumer_break_cancels(fake_stack):
    client, fakes = fake_stack
    env = client.env("dev1")
    fakes.processes.next_procs.append(
        FakeProc(chunks=[("stdout", b"a"), ("stdout", b"b"), ("stdout", b"c")])
    )
    proc = await env.start_process(["job"])
    seen = []
    async for out in proc.output():
        seen.append(out.stdout)
        break
    assert seen == [b"a"]


async def test_wait_skips_output(fake_stack):
    client, fakes = fake_stack
    env = client.env("dev1")
    fakes.processes.next_procs.append(
        FakeProc(chunks=[("stdout", b"lots of output")], exit_code=5, running_polls=3)
    )
    proc = await env.start_process(["job"])
    info = await proc.wait()
    assert info.state == ProcessState.EXITED
    assert info.exit_code == 5


async def test_shell_collects_output_and_exit_code(fake_stack):
    client, fakes = fake_stack
    env = client.env("dev1")
    fakes.processes.next_procs.append(
        FakeProc(
            chunks=[("stdout", b"hello "), ("stderr", b"warn\n"), ("stdout", b"world\n")],
            exit_code=3,
            running_polls=2,
        )
    )
    result = await env.shell("echo hello world; warn", cwd="/w", env={"A": "1"})
    fake = fakes.processes.procs["proc-1"]
    assert fake.command == ["sh", "-c", "echo hello world; warn"]
    assert fake.cwd == "/w" and fake.env == {"A": "1"}
    assert fake.stdin is False
    assert result == ShellResult(stdout="hello world\n", stderr="warn\n", exit_code=3)


async def test_shell_with_stdin(fake_stack):
    client, fakes = fake_stack
    env = client.env("dev1")
    fakes.processes.next_procs.append(FakeProc(echo_stdin=True))
    result = await env.shell("cat", stdin="typed in\n")
    fake = fakes.processes.procs["proc-1"]
    assert fake.stdin is True
    assert bytes(fake.stdin_data) == b"typed in\n"
    assert fake.stdin_closed
    assert result.stdout == "typed in\n"
    assert result.exit_code == 0


async def test_shell_killed_by_signal(fake_stack):
    client, fakes = fake_stack
    env = client.env("dev1")
    fakes.processes.next_procs.append(FakeProc(signal=guest_pb2.SIGNAL_KILL))
    result = await env.shell("sleep 30", timeout=0.5)
    assert result.exit_code == 137
    assert fakes.processes.procs["proc-1"].timeout_seconds == 0.5


async def test_shell_empty_output(fake_stack):
    client, fakes = fake_stack
    env = client.env("dev1")
    fakes.processes.next_procs.append(FakeProc(chunks=[], exit_code=0))
    result = await env.shell("true")
    assert result == ShellResult(stdout="", stderr="", exit_code=0)


async def test_shell_invalid_utf8_replaced(fake_stack):
    client, fakes = fake_stack
    env = client.env("dev1")
    fakes.processes.next_procs.append(FakeProc(chunks=[("stdout", b"ok\xff\xfe")], exit_code=0))
    result = await env.shell("binary")
    assert result.stdout == "ok��"


async def test_concurrent_shells(fake_stack):
    client, fakes = fake_stack
    env = client.env("dev1")
    for i in range(5):
        fakes.processes.next_procs.append(
            FakeProc(chunks=[("stdout", f"job-{i}\n".encode())], exit_code=i, running_polls=1)
        )
    results = await asyncio.gather(*(env.shell(f"job {i}") for i in range(5)))
    # StartProcess order over one channel is not guaranteed; match by output.
    assert sorted(r.stdout for r in results) == [f"job-{i}\n" for i in range(5)]
    assert sorted(r.exit_code for r in results) == list(range(5))


async def test_write_input_incremental_and_close(fake_stack):
    client, fakes = fake_stack
    env = client.env("dev1")
    proc = await env.start_process(["cat"], stdin=True)
    fake = fakes.processes.procs[proc.id]

    assert await proc.write_input(b"first ") == 6
    assert not fake.stdin_closed
    assert await proc.write_input([b"sec", b"ond"]) == 6

    async def gen():
        yield b" th"
        yield b"ird"

    assert await proc.write_input(gen(), close=True) == 6
    assert bytes(fake.stdin_data) == b"first second third"
    assert fake.stdin_closed

    with pytest.raises(FailedPreconditionError, match="is closed"):
        await proc.write_input(b"late")


async def test_write_input_chunked(fake_stack):
    client, fakes = fake_stack
    env = client.env("dev1")
    proc = await env.start_process(["cat"], stdin=True)
    data = bytes(range(256)) * 1024  # 256 KiB: several chunks on the wire
    assert await proc.write_input(data) == len(data)
    assert bytes(fakes.processes.procs[proc.id].stdin_data) == data


async def test_close_input_only(fake_stack):
    client, fakes = fake_stack
    env = client.env("dev1")
    proc = await env.start_process(["cat"], stdin=True)
    await proc.close_input()
    fake = fakes.processes.procs[proc.id]
    assert fake.stdin_closed and bytes(fake.stdin_data) == b""


async def test_write_input_without_stdin_rejected(fake_stack):
    client, _ = fake_stack
    env = client.env("dev1")
    proc = await env.start_process(["cat"])
    with pytest.raises(FailedPreconditionError, match="without stdin"):
        await proc.write_input(b"x")


async def test_write_input_rejects_str(fake_stack):
    client, _ = fake_stack
    env = client.env("dev1")
    proc = await env.start_process(["cat"], stdin=True)
    with pytest.raises(TypeError, match="encode it first"):
        await proc.write_input("text")  # type: ignore[arg-type]


async def test_write_input_missing_process(fake_stack):
    client, _ = fake_stack
    env = client.env("dev1")
    with pytest.raises(NotFoundError, match='process "nope" not found'):
        await env.process("nope").write_input(b"x")


async def test_signal_passthrough(fake_stack):
    client, fakes = fake_stack
    env = client.env("dev1")
    fakes.processes.next_procs.append(FakeProc(running_polls=10**9))
    proc = await env.start_process(["server"])
    await proc.signal(Signal.HUP)
    await proc.signal(Signal.USR1)
    assert fakes.processes.procs[proc.id].signals == [guest_pb2.SIGNAL_HUP, guest_pb2.SIGNAL_USR1]
    info = await proc.info()
    assert info.running


async def test_signal_term_then_wait(fake_stack):
    client, fakes = fake_stack
    env = client.env("dev1")
    fakes.processes.next_procs.append(FakeProc(running_polls=10**9))
    proc = await env.start_process(["sleep", "infinity"])
    await proc.signal(Signal.TERM)
    info = await proc.wait()
    assert info.exit_code == 143
    with pytest.raises(ProcessExitedError):
        await proc.signal(Signal.KILL)


async def test_kill_is_idempotent(fake_stack):
    client, fakes = fake_stack
    env = client.env("dev1")
    fakes.processes.next_procs.append(FakeProc(running_polls=10**9))
    proc = await env.start_process(["sleep", "infinity"])
    info = await proc.kill()
    assert info.exit_code == 137
    info = await proc.kill()
    assert info.exit_code == 137


async def test_kill_missing_process(fake_stack):
    client, _ = fake_stack
    env = client.env("dev1")
    with pytest.raises(NotFoundError, match='process "nope" not found'):
        await env.process("nope").kill()


async def test_wait_cancellable(fake_stack):
    client, fakes = fake_stack
    env = client.env("dev1")
    fakes.processes.next_procs.append(FakeProc(hang_follow=True))  # never exits
    proc = await env.start_process(["sleep", "infinity"])
    with pytest.raises(TimeoutError):
        async with asyncio.timeout(0.05):
            await proc.wait()
