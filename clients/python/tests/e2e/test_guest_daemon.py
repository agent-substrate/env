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
    OutputSource,
    NotFoundError,
    PermissionDeniedError,
    ProcessStatus,
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


async def test_stream_outputs_follow(guest_env):
    pid = await guest_env.start_process(["sh", "-c", "echo one; echo two; echo err >&2"])
    stdout = bytearray()
    stderr = bytearray()
    async for chunk in guest_env.stream_outputs(pid, follow=True):
        if chunk.source == OutputSource.STDOUT:
            stdout.extend(chunk.data)
        else:
            stderr.extend(chunk.data)
    assert stdout == b"one\ntwo\n"
    assert stderr == b"err\n"
    proc = await guest_env.wait(pid)
    assert proc.status == ProcessStatus.COMPLETED


async def test_kill_process(guest_env):
    pid = await guest_env.start_process(["sleep", "30"])
    exit_code = await guest_env.kill_process(pid)
    assert exit_code >= 128  # 128 + signal number
    proc = await guest_env.wait(pid)
    assert proc.status == ProcessStatus.TERMINATED


async def test_cwd_passthrough(guest_env):
    pid = await guest_env.start_process(["pwd"], cwd="/")
    proc = await guest_env.wait(pid)
    assert proc.status == ProcessStatus.COMPLETED
    output = b"".join(
        [c.data async for c in guest_env.stream_outputs(pid) if c.source == OutputSource.STDOUT]
    )
    assert output == b"/\n"


async def test_env_passthrough(guest_env):
    result_pid = await guest_env.start_process(
        ["sh", "-c", "echo $ATE_E2E_MARKER"], env={"ATE_E2E_MARKER": "marker-42"}
    )
    await guest_env.wait(result_pid)
    output = b"".join([c.data async for c in guest_env.stream_outputs(result_pid)])
    assert output == b"marker-42\n"


async def test_process_timestamps(guest_env):
    pid = await guest_env.start_process(["true"])
    proc = await guest_env.wait(pid)
    assert proc.started_at is not None
    assert proc.finished_at is not None
    assert proc.finished_at >= proc.started_at


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


async def test_stream_outputs_offset_replay(guest_env):
    pid = await guest_env.start_process(["sh", "-c", "printf abcdef; printf 123456 >&2"])
    await guest_env.wait(pid)

    full = [(c.source, c.data) async for c in guest_env.stream_outputs(pid)]
    assert b"".join(d for s, d in full if s == OutputSource.STDOUT) == b"abcdef"
    assert b"".join(d for s, d in full if s == OutputSource.STDERR) == b"123456"

    replay = [
        (c.source, c.data)
        async for c in guest_env.stream_outputs(pid, stdout_offset=4, stderr_offset=2)
    ]
    assert b"".join(d for s, d in replay if s == OutputSource.STDOUT) == b"ef"
    assert b"".join(d for s, d in replay if s == OutputSource.STDERR) == b"3456"


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
        await guest_env.get_process("bogus-process-id")


async def test_sandbox_escape_maps_to_permission_denied(guest_env):
    with pytest.raises(PermissionDeniedError, match="outside sandbox root"):
        await guest_env.read_file_bytes("../escape-attempt.txt")


async def test_kill_ends_follow_stream(guest_env):
    pid = await guest_env.start_process(["sh", "-c", "echo started; sleep 30"])

    async def consume():
        return [c async for c in guest_env.stream_outputs(pid, follow=True)]

    task = asyncio.create_task(consume())
    # Give the process time to start and emit its first output.
    await asyncio.sleep(0.3)
    await guest_env.kill_process(pid)
    chunks = await asyncio.wait_for(task, timeout=10)
    assert any(b"started" in c.data for c in chunks)
    proc = await guest_env.wait(pid)
    assert proc.status == ProcessStatus.TERMINATED
