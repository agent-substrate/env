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

"""Integration tests against a real guest daemon (no cluster required).

Start one with:

    go run ./examples/guest-daemon --listen 127.0.0.1:8090 --workspace "$(mktemp -d)"

then run:

    ATE_ENV_GUEST_TARGET=127.0.0.1:8090 pytest tests/e2e

The guest daemon serves ProcessService and FileSystemService directly and
ignores the x-env-id routing metadata, so an Env handle works against it;
environment lifecycle RPCs are not available there.
"""

from __future__ import annotations

import asyncio
import os
import uuid

import pytest

from ate_env import (
    Client,
    FailedPreconditionError,
    NotFoundError,
    PermissionDeniedError,
    ProcessExitedError,
    ProcessState,
    Signal,
)

TARGET = os.environ.get("ATE_ENV_GUEST_TARGET")

pytestmark = pytest.mark.skipif(
    not TARGET,
    reason="set ATE_ENV_GUEST_TARGET=host:port of a running examples/guest-daemon",
)


@pytest.fixture
async def guest_env():
    client = Client(TARGET)
    try:
        yield client.env("e2e")
    finally:
        await client.close()


async def test_shell_roundtrip(guest_env):
    result = await guest_env.shell("echo hello from e2e")
    assert result.exit_code == 0
    assert result.stdout == "hello from e2e\n"


async def test_shell_nonzero_exit_and_stderr(guest_env):
    result = await guest_env.shell("echo oops >&2; exit 3")
    assert result.exit_code == 3
    assert "oops" in result.stderr


async def test_file_roundtrip(guest_env):
    path = f"ate-env-e2e-{uuid.uuid4().hex}.txt"
    content = b"line1\nline2\n" * 10_000  # multi-chunk on the wire
    written = await guest_env.write_file(path, content, mode=0o600)
    assert written == len(content)
    assert await guest_env.read_file_bytes(path) == content


async def _stdout(outputs) -> bytes:
    return b"".join([o.stdout async for o in outputs if o.stdout is not None])


async def test_write_file_seek_offset(guest_env):
    path = f"ate-env-e2e-{uuid.uuid4().hex}.txt"
    await guest_env.write_file(path, b"hello world")
    await guest_env.write_file(path, b"W", seek_offset=6)
    assert await guest_env.read_file_bytes(path) == b"hello World"
    await guest_env.write_file(path, b"!", seek_offset=13)
    assert await guest_env.read_file_bytes(path) == b"hello World\0\0!"


async def test_output_follow_ends_with_exit(guest_env):
    proc = await guest_env.start_process(["sh", "-c", "echo one; echo two; echo err >&2; exit 4"])
    stdout = bytearray()
    stderr = bytearray()
    exit_info = None
    async for out in proc.output(follow=True):
        if out.stdout is not None:
            stdout.extend(out.stdout)
        elif out.stderr is not None:
            stderr.extend(out.stderr)
        else:
            exit_info = out.exit
    assert stdout == b"one\ntwo\n"
    assert stderr == b"err\n"
    assert exit_info is not None
    assert exit_info.state == ProcessState.EXITED
    assert exit_info.exit_code == 4


async def test_kill_process(guest_env):
    proc = await guest_env.start_process(["sleep", "30"])
    info = await proc.kill()
    assert info.state == ProcessState.EXITED
    assert info.exit_code == 137  # 128 + SIGKILL
    # Idempotent, and other signals are refused once exited.
    assert (await proc.kill()).exit_code == 137
    with pytest.raises(ProcessExitedError):
        await proc.signal(Signal.TERM)


async def test_signal_term(guest_env):
    proc = await guest_env.start_process(["sleep", "30"])
    await proc.signal(Signal.TERM)
    info = await proc.wait()
    assert info.exit_code == 143  # 128 + SIGTERM


async def test_signal_trapped(guest_env):
    proc = await guest_env.start_process(
        ["sh", "-c", "trap 'echo got-usr1; exit 7' USR1; while :; do sleep 0.05; done"]
    )
    await asyncio.sleep(0.3)
    await proc.signal(Signal.USR1)
    info = await proc.wait()
    assert info.exit_code == 7
    assert b"got-usr1" in await _stdout(proc.output())


async def test_stdin_streaming(guest_env):
    proc = await guest_env.start_process(["cat"], stdin=True)
    await proc.write_input(b"hello ")
    await proc.write_input(b"world\n", close=True)
    info = await proc.wait()
    assert info.exit_code == 0
    assert await _stdout(proc.output()) == b"hello world\n"


async def test_stdin_not_requested(guest_env):
    proc = await guest_env.start_process(["cat"])
    info = await proc.wait()
    assert info.exit_code == 0  # immediate EOF
    with pytest.raises(FailedPreconditionError, match="without stdin"):
        await proc.write_input(b"x")


async def test_shell_with_stdin(guest_env):
    result = await guest_env.shell("tr a-z A-Z", stdin="shout\n")
    assert result.stdout == "SHOUT\n"
    assert result.exit_code == 0


async def test_shell_timeout(guest_env):
    result = await guest_env.shell("sleep 30", timeout=0.2)
    assert result.exit_code == 137


async def test_cwd_passthrough(guest_env):
    proc = await guest_env.start_process(["pwd"], cwd="/")
    info = await proc.wait()
    assert info.exit_code == 0
    assert await _stdout(proc.output()) == b"/\n"


async def test_env_passthrough(guest_env):
    proc = await guest_env.start_process(
        ["sh", "-c", "echo $ATE_E2E_MARKER"], env={"ATE_E2E_MARKER": "marker-42"}
    )
    await proc.wait()
    assert await _stdout(proc.output()) == b"marker-42\n"


async def test_process_info(guest_env):
    proc = await guest_env.start_process(["true"])
    info = await proc.wait()
    assert info.command == ("true",)
    assert info.pid > 0
    assert info.started_at is not None
    assert info.finished_at is not None
    assert info.finished_at >= info.started_at
    assert (await guest_env.process(proc.id).info()) == info


async def test_binary_file_roundtrip(guest_env):
    path = f"ate-env-e2e-{uuid.uuid4().hex}.bin"
    content = os.urandom(256 * 1024 + 7)  # multi-chunk, all byte values
    assert await guest_env.write_file(path, content) == len(content)
    assert await guest_env.read_file_bytes(path) == content


async def test_empty_file_roundtrip(guest_env):
    path = f"ate-env-e2e-{uuid.uuid4().hex}.empty"
    assert await guest_env.write_file(path, b"") == 0
    assert await guest_env.read_file_bytes(path) == b""


async def test_write_file_from_async_generator(guest_env):
    path = f"ate-env-e2e-{uuid.uuid4().hex}.gen"

    async def gen():
        for i in range(5):
            yield f"part-{i};".encode()

    written = await guest_env.write_file(path, gen())
    content = await guest_env.read_file_bytes(path)
    assert content == b"part-0;part-1;part-2;part-3;part-4;"
    assert written == len(content)


async def test_output_offset_replay(guest_env):
    proc = await guest_env.start_process(["sh", "-c", "printf abcdef; printf 123456 >&2"])
    await proc.wait()

    full = [o async for o in proc.output()]
    assert b"".join(o.stdout for o in full if o.stdout is not None) == b"abcdef"
    assert b"".join(o.stderr for o in full if o.stderr is not None) == b"123456"
    assert full[-1].exit is not None

    replay = [o async for o in proc.output(stdout_offset=4, stderr_offset=2)]
    assert b"".join(o.stdout for o in replay if o.stdout is not None) == b"ef"
    assert b"".join(o.stderr for o in replay if o.stderr is not None) == b"3456"


async def test_concurrent_shells(guest_env):
    async def run(i: int):
        return await guest_env.shell(f"echo job-{i}; exit {i}")

    results = await asyncio.gather(*(run(i) for i in range(4)))
    for i, result in enumerate(results):
        assert result.stdout == f"job-{i}\n"
        assert result.exit_code == i


async def test_missing_file_maps_to_not_found(guest_env):
    with pytest.raises(NotFoundError):
        await guest_env.read_file_bytes(f"nope-{uuid.uuid4().hex}.txt")


async def test_missing_process_maps_to_not_found(guest_env):
    with pytest.raises(NotFoundError):
        await guest_env.process("bogus-process-id").info()


async def test_sandbox_escape_maps_to_permission_denied(guest_env):
    with pytest.raises(PermissionDeniedError, match="outside sandbox root"):
        await guest_env.read_file_bytes("../escape-attempt.txt")


async def test_kill_ends_follow_stream(guest_env):
    proc = await guest_env.start_process(["sh", "-c", "echo started; sleep 30"])

    async def consume():
        return [o async for o in proc.output(follow=True)]

    task = asyncio.create_task(consume())
    # Give the process time to start and emit its first output.
    await asyncio.sleep(0.3)
    await proc.kill()
    outputs = await asyncio.wait_for(task, timeout=10)
    assert any(o.stdout is not None and b"started" in o.stdout for o in outputs)
    assert outputs[-1].exit is not None
    assert outputs[-1].exit.exit_code == 137  # 128 + SIGKILL
