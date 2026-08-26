"""Handle to a single environment."""

from __future__ import annotations

import asyncio
from collections.abc import AsyncIterable, AsyncIterator, Iterable, Mapping, Sequence
from typing import TYPE_CHECKING

import grpc

from ._gen.ateenv.v1 import guest_pb2
from .errors import map_rpc_error
from .types import (
    EnvironmentInfo,
    OutputChunk,
    OutputSource,
    ProcessInfo,
    ProcessStatus,
    ShellResult,
    _process_info_from_pb,
)

if TYPE_CHECKING:
    from .client import Client

__all__ = ["Env"]

# Matches the server's file chunk size (guest/filesystem).
_CHUNK_SIZE = 64 * 1024


class Env:
    """A handle to a single environment.

    Constructed by Client.create() or Client.env(). Guest operations
    (processes and files) automatically carry the x-env-id/x-env-atespace
    routing metadata the API server requires.
    """

    def __init__(self, client: Client, id: str, atespace: str):
        self._client = client
        self._id = id
        self._atespace = atespace

    @property
    def id(self) -> str:
        """The environment's identifier."""
        return self._id

    @property
    def atespace(self) -> str:
        """The environment's atespace."""
        return self._atespace

    def _metadata(self) -> tuple[tuple[str, str], ...]:
        return (("x-env-id", self._id), ("x-env-atespace", self._atespace))

    async def info(self) -> EnvironmentInfo:
        """Retrieve the environment's current status and configuration."""
        return await self._client.get(self._id, atespace=self._atespace)

    async def suspend(self) -> None:
        """Checkpoint and stop the environment."""
        await self._client.suspend(self._id, atespace=self._atespace)

    async def delete(self) -> None:
        """Remove the environment permanently."""
        await self._client.delete(self._id, atespace=self._atespace)

    # --- processes ---

    async def start_process(
        self,
        command: Sequence[str],
        *,
        cwd: str = "",
        env: Mapping[str, str] | None = None,
    ) -> str:
        """Launch a process asynchronously; returns its process id."""
        req = guest_pb2.StartProcessRequest(
            command=list(command), cwd=cwd, env=dict(env or {})
        )
        try:
            resp = await self._client._processes.StartProcess(req, metadata=self._metadata())
        except grpc.RpcError as e:
            raise map_rpc_error(e) from e
        return resp.process_id

    async def get_process(self, process_id: str) -> ProcessInfo:
        """Retrieve the current state of a process."""
        req = guest_pb2.GetProcessRequest(process_id=process_id)
        try:
            proc = await self._client._processes.GetProcess(req, metadata=self._metadata())
        except grpc.RpcError as e:
            raise map_rpc_error(e) from e
        return _process_info_from_pb(proc)

    async def kill_process(self, process_id: str) -> int:
        """Terminate a running process; returns its exit code."""
        req = guest_pb2.KillProcessRequest(process_id=process_id)
        try:
            resp = await self._client._processes.KillProcess(req, metadata=self._metadata())
        except grpc.RpcError as e:
            raise map_rpc_error(e) from e
        return resp.exit_code

    async def stream_outputs(
        self,
        process_id: str,
        *,
        follow: bool = False,
        stdout_offset: int = 0,
        stderr_offset: int = 0,
    ) -> AsyncIterator[OutputChunk]:
        """Stream stdout/stderr chunks from a process.

        With follow=True the stream ends when the process exits; on a
        long-lived process it blocks until then, so consume it under a
        timeout or cancel the task. Breaking out of the loop cancels the
        underlying RPC.
        """
        req = guest_pb2.StreamProcessOutputsRequest(
            process_id=process_id,
            stdout_offset=stdout_offset,
            stderr_offset=stderr_offset,
            follow=follow,
        )
        call = self._client._processes.StreamProcessOutputs(req, metadata=self._metadata())
        try:
            async for chunk in call:
                yield OutputChunk(source=OutputSource(chunk.source), data=chunk.data)
        except grpc.RpcError as e:
            raise map_rpc_error(e) from e
        finally:
            call.cancel()

    async def wait(self, process_id: str, *, poll_interval: float = 0.02) -> ProcessInfo:
        """Poll until the process is no longer running; returns its final state."""
        while True:
            proc = await self.get_process(process_id)
            if proc.status != ProcessStatus.RUNNING:
                return proc
            await asyncio.sleep(poll_interval)

    async def shell(self, command_line: str) -> ShellResult:
        """Run a shell command line and capture its output and exit code.

        Output is buffered fully in memory; for incremental output use
        start_process() with stream_outputs().
        """
        process_id = await self.start_process(["sh", "-c", command_line])

        stdout = bytearray()
        stderr = bytearray()
        async for chunk in self.stream_outputs(process_id, follow=True):
            if chunk.source == OutputSource.STDOUT:
                stdout.extend(chunk.data)
            elif chunk.source == OutputSource.STDERR:
                stderr.extend(chunk.data)

        # The follow stream can end before the process record's status
        # flips, so poll for the final state to get the exit code.
        proc = await self.wait(process_id)
        return ShellResult(
            stdout=stdout.decode("utf-8", errors="replace"),
            stderr=stderr.decode("utf-8", errors="replace"),
            exit_code=proc.exit_code,
        )

    # --- files ---

    async def read_file(self, path: str) -> AsyncIterator[bytes]:
        """Stream the contents of a file in chunks.

        Errors (e.g. NotFoundError) surface on the first iteration.
        """
        req = guest_pb2.ReadFileRequest(path=path)
        call = self._client._filesystem.ReadFile(req, metadata=self._metadata())
        try:
            async for chunk in call:
                if chunk.data:
                    yield chunk.data
        except grpc.RpcError as e:
            raise map_rpc_error(e) from e
        finally:
            call.cancel()

    async def read_file_bytes(self, path: str) -> bytes:
        """Read a whole file into memory."""
        return b"".join([chunk async for chunk in self.read_file(path)])

    async def write_file(
        self,
        path: str,
        data: bytes | bytearray | memoryview | Iterable[bytes] | AsyncIterable[bytes],
        *,
        mode: int = 0o644,
    ) -> int:
        """Write data to a file; returns the number of bytes written.

        data may be bytes-like (sent in 64 KiB chunks) or a sync/async
        iterable of bytes chunks.
        """
        # Validate eagerly: errors raised inside the request generator are
        # swallowed by grpc.aio and surface as an opaque CancelledError.
        if isinstance(data, str):
            raise TypeError("ate_env: write_file expects bytes, not str; encode it first")
        if not isinstance(data, (bytes, bytearray, memoryview, Iterable, AsyncIterable)):
            raise TypeError(f"ate_env: unsupported write_file data type: {type(data)!r}")
        try:
            resp = await self._client._filesystem.WriteFile(
                self._write_requests(path, data, mode), metadata=self._metadata()
            )
        except grpc.RpcError as e:
            raise map_rpc_error(e) from e
        return resp.bytes_written

    async def _write_requests(
        self,
        path: str,
        data: bytes | bytearray | memoryview | Iterable[bytes] | AsyncIterable[bytes],
        mode: int,
    ) -> AsyncIterator[guest_pb2.WriteFileRequest]:
        chunks = _chunk_stream(data)
        # The first message carries path and mode, even for empty input
        # (required to create empty files).
        first = b""
        async for chunk in chunks:
            first = chunk
            break
        yield guest_pb2.WriteFileRequest(path=path, mode=mode & 0o777, chunk=first)
        async for chunk in chunks:
            if chunk:
                yield guest_pb2.WriteFileRequest(chunk=chunk)


async def _chunk_stream(
    data: bytes | bytearray | memoryview | Iterable[bytes] | AsyncIterable[bytes],
) -> AsyncIterator[bytes]:
    if isinstance(data, str):
        raise TypeError("ate_env: write_file expects bytes, not str; encode it first")
    if isinstance(data, (bytes, bytearray, memoryview)):
        buf = bytes(data)
        for i in range(0, len(buf), _CHUNK_SIZE):
            yield buf[i : i + _CHUNK_SIZE]
    elif isinstance(data, AsyncIterable):
        async for chunk in data:
            yield bytes(chunk)
    elif isinstance(data, Iterable):
        for chunk in data:
            yield bytes(chunk)
    else:
        raise TypeError(f"ate_env: unsupported write_file data type: {type(data)!r}")
