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

"""Handles to a single environment and to processes running inside it."""

from __future__ import annotations

from collections.abc import AsyncIterable, AsyncIterator, Iterable, Mapping, Sequence
from datetime import timedelta
from typing import TYPE_CHECKING

import grpc

from ._gen.ateenv.v1alpha import guest_pb2
from .errors import ProcessExitedError, map_rpc_error
from .types import (
    EnvironmentInfo,
    ProcessInfo,
    ProcessOutput,
    ShellResult,
    Signal,
    _process_info_from_pb,
    _process_output_from_pb,
)

if TYPE_CHECKING:
    from .client import Client

__all__ = ["Env", "Process"]

# Matches the server's file chunk size (guest/filesystem).
_CHUNK_SIZE = 64 * 1024

# Offsets past any spool: follow the stream without receiving output.
_SKIP_ALL = 2**63 - 1

_Bytes = bytes | bytearray | memoryview
_ByteSource = _Bytes | Iterable[bytes] | AsyncIterable[bytes]


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
        stdin: bool = False,
        timeout: float | timedelta | None = None,
    ) -> Process:
        """Launch a process in the background and return a handle to it.

        stdin=True opens a pipe for standard input, fed with
        Process.write_input(); otherwise the process reads EOF immediately.
        timeout (seconds or timedelta) kills the process with SIGKILL when it
        elapses; None applies the guest default.
        """
        req = guest_pb2.StartProcessRequest(
            command=list(command), cwd=cwd, env=dict(env or {}), stdin=stdin
        )
        if timeout is not None:
            req.timeout.FromTimedelta(
                timeout if isinstance(timeout, timedelta) else timedelta(seconds=timeout)
            )
        try:
            proc = await self._client._processes.StartProcess(req, metadata=self._metadata())
        except grpc.RpcError as e:
            raise map_rpc_error(e) from e
        return Process(self, proc.process_id)

    def process(self, process_id: str) -> Process:
        """Return a handle to an existing process without checking that it exists."""
        return Process(self, process_id)

    async def shell(
        self,
        command_line: str,
        *,
        stdin: bytes | str | None = None,
        cwd: str = "",
        env: Mapping[str, str] | None = None,
        timeout: float | timedelta | None = None,
    ) -> ShellResult:
        """Run `sh -c command_line`, feed it stdin, and capture output and exit status.

        Output is buffered fully in memory; for incremental output or
        interactive input use start_process().
        """
        proc = await self.start_process(
            ["sh", "-c", command_line], cwd=cwd, env=env, stdin=stdin is not None, timeout=timeout
        )
        if stdin is not None:
            await proc.write_input(stdin.encode() if isinstance(stdin, str) else stdin, close=True)

        stdout = bytearray()
        stderr = bytearray()
        exit_info: ProcessInfo | None = None
        async for out in proc.output(follow=True):
            if out.stdout is not None:
                stdout.extend(out.stdout)
            elif out.stderr is not None:
                stderr.extend(out.stderr)
            elif out.exit is not None:
                exit_info = out.exit
        if exit_info is None:
            raise RuntimeError("ate_env: output stream ended before the process exited")
        return ShellResult(
            stdout=stdout.decode("utf-8", errors="replace"),
            stderr=stderr.decode("utf-8", errors="replace"),
            exit_code=exit_info.exit_code,
        )

    # --- files ---

    async def read_file(self, path: str) -> AsyncIterator[bytes]:
        """Stream the contents of a file in chunks.

        Errors (e.g. NotFoundError) surface on the first iteration.
        """
        req = guest_pb2.ReadFileRequest(path=path)
        call = self._client._filesystem.ReadFile(req, metadata=self._metadata())
        try:
            async for msg in call:
                if msg.chunk:
                    yield msg.chunk
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
        data: _ByteSource,
        *,
        mode: int = 0o644,
        seek_offset: int = 0,
    ) -> int:
        """Write data to a file; returns the number of bytes written.

        data may be bytes-like (sent in 64 KiB chunks) or a sync/async
        iterable of bytes chunks. With seek_offset=0 (the default) the file
        is replaced. A positive seek_offset keeps existing content and
        starts writing at that byte, extending the file with zeros if it is
        shorter. mode applies when the file is created.
        """
        _validate_byte_source(data, "write_file")
        if seek_offset < 0:
            raise ValueError(f"ate_env: seek_offset must be >= 0, got {seek_offset}")
        try:
            resp = await self._client._filesystem.WriteFile(
                self._write_requests(path, data, mode, seek_offset), metadata=self._metadata()
            )
        except grpc.RpcError as e:
            raise map_rpc_error(e) from e
        return resp.bytes_written

    async def _write_requests(
        self, path: str, data: _ByteSource, mode: int, seek_offset: int
    ) -> AsyncIterator[guest_pb2.WriteFileRequest]:
        chunks = _chunk_stream(data)
        # The first message carries path, mode and seek_offset, even for
        # empty input (required to create empty files).
        first = b""
        async for chunk in chunks:
            first = chunk
            break
        yield guest_pb2.WriteFileRequest(
            path=path, mode=mode & 0o777, chunk=first, seek_offset=seek_offset
        )
        async for chunk in chunks:
            if chunk:
                yield guest_pb2.WriteFileRequest(chunk=chunk)


class Process:
    """A handle to a process inside an environment.

    Constructed by Env.start_process() or Env.process().
    """

    def __init__(self, env: Env, process_id: str):
        self._env = env
        self._id = process_id

    @property
    def id(self) -> str:
        """The guest-assigned process identifier."""
        return self._id

    @property
    def env(self) -> Env:
        """The environment the process runs in."""
        return self._env

    def __repr__(self) -> str:
        return f"Process({self._id!r})"

    @property
    def _stub(self):
        return self._env._client._processes

    async def info(self) -> ProcessInfo:
        """Retrieve the current state of the process."""
        req = guest_pb2.GetProcessRequest(process_id=self._id)
        try:
            proc = await self._stub.GetProcess(req, metadata=self._env._metadata())
        except grpc.RpcError as e:
            raise map_rpc_error(e) from e
        return _process_info_from_pb(proc)

    async def output(
        self,
        *,
        follow: bool = False,
        stdout_offset: int = 0,
        stderr_offset: int = 0,
    ) -> AsyncIterator[ProcessOutput]:
        """Stream stdout and stderr; the final message carries the exit state.

        With follow=True the stream stays open until the process exits, so on
        a long-lived process it blocks until then; consume it under a timeout
        or cancel the task. Without follow, the output spooled so far is
        returned and the exit message is present only if the process has
        already exited. Breaking out of the loop cancels the underlying RPC.
        """
        req = guest_pb2.StreamProcessOutputRequest(
            process_id=self._id,
            stdout_offset=stdout_offset,
            stderr_offset=stderr_offset,
            follow=follow,
        )
        call = self._stub.StreamProcessOutput(req, metadata=self._env._metadata())
        try:
            async for msg in call:
                yield _process_output_from_pb(msg)
        except grpc.RpcError as e:
            raise map_rpc_error(e) from e
        finally:
            call.cancel()

    async def wait(self) -> ProcessInfo:
        """Block until the process exits and return its final state.

        Follows the output stream with offsets past the end of the spool, so
        no output is transferred.
        """
        async for out in self.output(
            follow=True, stdout_offset=_SKIP_ALL, stderr_offset=_SKIP_ALL
        ):
            if out.exit is not None:
                return out.exit
        raise RuntimeError("ate_env: output stream ended before the process exited")

    async def write_input(self, data: _ByteSource, *, close: bool = False) -> int:
        """Write to the process's stdin; returns the number of bytes written.

        The process must have been started with stdin=True. stdin stays open
        across calls until one sets close=True (EOF). data may be bytes-like
        or a sync/async iterable of bytes chunks.
        """
        _validate_byte_source(data, "write_input")
        try:
            resp = await self._stub.WriteProcessInput(
                self._input_requests(data, close), metadata=self._env._metadata()
            )
        except grpc.RpcError as e:
            raise map_rpc_error(e) from e
        return resp.bytes_written

    async def close_input(self) -> None:
        """Close the process's stdin, delivering EOF."""
        await self.write_input(b"", close=True)

    async def _input_requests(
        self, data: _ByteSource, close: bool
    ) -> AsyncIterator[guest_pb2.WriteProcessInputRequest]:
        first = True
        async for chunk in _chunk_stream(data):
            if not chunk:
                continue
            req = guest_pb2.WriteProcessInputRequest(data=chunk)
            if first:
                req.process_id = self._id
                first = False
            yield req
        if first or close:
            # Either nothing was sent (the first message must name the
            # process) or EOF was requested; both need a final message.
            yield guest_pb2.WriteProcessInputRequest(
                process_id=self._id if first else "", close=close
            )

    async def signal(self, sig: Signal) -> None:
        """Deliver a signal to the process and its process group."""
        req = guest_pb2.SignalProcessRequest(process_id=self._id, signal=int(sig))
        try:
            await self._stub.SignalProcess(req, metadata=self._env._metadata())
        except grpc.RpcError as e:
            raise map_rpc_error(e) from e

    async def kill(self) -> ProcessInfo:
        """Send SIGKILL and wait for the process to exit.

        Killing a process that has already exited is not an error.
        """
        try:
            await self.signal(Signal.KILL)
        except ProcessExitedError:
            pass
        return await self.wait()


def _validate_byte_source(data: object, what: str) -> None:
    # Validate eagerly: errors raised inside a request generator are
    # swallowed by grpc.aio and surface as an opaque CancelledError.
    if isinstance(data, str):
        raise TypeError(f"ate_env: {what} expects bytes, not str; encode it first")
    if not isinstance(data, (bytes, bytearray, memoryview, Iterable, AsyncIterable)):
        raise TypeError(f"ate_env: unsupported {what} data type: {type(data)!r}")


async def _chunk_stream(data: _ByteSource) -> AsyncIterator[bytes]:
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
        raise TypeError(f"ate_env: unsupported data type: {type(data)!r}")
