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

import pytest

from ate_env import NotFoundError, PermissionDeniedError

from .fakes import CHUNK_SIZE


async def test_read_file_multi_chunk(fake_stack):
    client, fakes = fake_stack
    env = client.env("dev1")
    content = bytes(range(256)) * ((CHUNK_SIZE * 2) // 256 + 1)  # > 2 chunks
    fakes.filesystem.files["/data.bin"] = content

    chunks = [chunk async for chunk in env.read_file("/data.bin")]
    assert len(chunks) >= 3
    assert b"".join(chunks) == content
    assert fakes.filesystem.last_env == ("dev1", "ate-env")


async def test_read_file_bytes(fake_stack):
    client, fakes = fake_stack
    env = client.env("dev1")
    fakes.filesystem.files["/note.txt"] = b"hello\n"
    assert await env.read_file_bytes("/note.txt") == b"hello\n"


async def test_read_file_missing(fake_stack):
    client, _ = fake_stack
    env = client.env("dev1")
    with pytest.raises(NotFoundError, match="no such file or directory"):
        async for _ in env.read_file("/nope"):
            pass
    with pytest.raises(NotFoundError):
        await env.read_file_bytes("/nope")


async def test_write_file_bytes(fake_stack):
    client, fakes = fake_stack
    env = client.env("dev1")
    content = b"x" * (CHUNK_SIZE + 100)  # forces two request chunks
    written = await env.write_file("/big.bin", content, mode=0o755)
    assert written == len(content)
    assert fakes.filesystem.files["/big.bin"] == content
    assert fakes.filesystem.modes["/big.bin"] == 0o755


async def test_write_file_empty_creates_file(fake_stack):
    client, fakes = fake_stack
    env = client.env("dev1")
    written = await env.write_file("/empty", b"")
    assert written == 0
    assert fakes.filesystem.files["/empty"] == b""
    assert fakes.filesystem.modes["/empty"] == 0o644


async def test_write_file_sync_iterable(fake_stack):
    client, fakes = fake_stack
    env = client.env("dev1")
    written = await env.write_file("/parts", [b"one", b"two", b"three"])
    assert written == 11
    assert fakes.filesystem.files["/parts"] == b"onetwothree"


async def test_write_file_async_iterable(fake_stack):
    client, fakes = fake_stack
    env = client.env("dev1")

    async def gen():
        yield b"alpha"
        yield b"beta"

    written = await env.write_file("/async-parts", gen())
    assert written == 9
    assert fakes.filesystem.files["/async-parts"] == b"alphabeta"


async def test_read_empty_file(fake_stack):
    client, fakes = fake_stack
    env = client.env("dev1")
    fakes.filesystem.files["/empty"] = b""
    assert [chunk async for chunk in env.read_file("/empty")] == []
    assert await env.read_file_bytes("/empty") == b""


async def test_read_sandbox_escape_denied(fake_stack):
    client, _ = fake_stack
    env = client.env("dev1")
    with pytest.raises(PermissionDeniedError, match="outside sandbox root"):
        await env.read_file_bytes("../etc/passwd")


async def test_write_sandbox_escape_denied(fake_stack):
    client, _ = fake_stack
    env = client.env("dev1")
    with pytest.raises(PermissionDeniedError, match="outside sandbox root"):
        await env.write_file("../escape.txt", b"nope")


async def test_write_file_chunking_boundaries(fake_stack):
    client, fakes = fake_stack
    env = client.env("dev1")

    await env.write_file("/exact", b"x" * CHUNK_SIZE)
    assert fakes.filesystem.last_write_chunk_sizes == [CHUNK_SIZE]

    await env.write_file("/one-over", b"x" * (CHUNK_SIZE + 1))
    assert fakes.filesystem.last_write_chunk_sizes == [CHUNK_SIZE, 1]


async def test_write_file_bytearray_and_memoryview(fake_stack):
    client, fakes = fake_stack
    env = client.env("dev1")
    assert await env.write_file("/ba", bytearray(b"from-bytearray")) == 14
    assert fakes.filesystem.files["/ba"] == b"from-bytearray"
    assert await env.write_file("/mv", memoryview(b"from-memoryview")) == 15
    assert fakes.filesystem.files["/mv"] == b"from-memoryview"


async def test_binary_roundtrip(fake_stack):
    client, fakes = fake_stack
    env = client.env("dev1")
    content = bytes(range(256)) * 300  # all byte values, multiple chunks worth
    await env.write_file("/bin", content)
    assert await env.read_file_bytes("/bin") == content


async def test_write_file_rejects_str(fake_stack):
    client, _ = fake_stack
    env = client.env("dev1")
    with pytest.raises(TypeError, match="expects bytes, not str"):
        await env.write_file("/oops", "text")  # type: ignore[arg-type]
