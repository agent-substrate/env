# Copyright 2026 The Kubernetes Authors & Google LLC
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

import base64
import posixpath
import shlex
from abc import ABC, abstractmethod
from typing import Dict, Iterator, List, Optional

from ..exceptions import CommandExecutionError, CommandTimeoutError
from ..types import ExecResult

# Exit code the read helper uses to report a missing file.
_MISSING_FILE_EXIT = 44


def command_to_argv(command: str | List[str]) -> List[str]:
    """Command contract shared by every runtime.

    A ``str`` is a shell command and always runs as ``bash -c <command>``. A
    list is executed directly as argv, without a shell. As a result, a typo
    in a shell string exits 127 (an agent outcome that is scored), while a
    missing ``argv[0]`` raises ``CommandStartError`` (masked).
    """
    if isinstance(command, str):
        return ["bash", "-c", command]
    argv = list(command)
    if not argv:
        raise ValueError("command must not be empty")
    return argv


def write_file_via_exec(runtime: "RuntimeGuestHook", path: str, content: bytes | str,
                        timeout_s: float = 120.0) -> None:
    """Write a file by running a base64 decode pipeline through ``runtime.exec``.

    Limitation: the payload travels inside argv, so files larger than about
    96 KiB hit Linux's 128 KiB per-argument limit (MAX_ARG_STRLEN).
    """
    raw = content.encode("utf-8") if isinstance(content, str) else content
    b64 = base64.b64encode(raw).decode("ascii")  # [A-Za-z0-9+/=] only: safe unquoted
    parent = posixpath.dirname(path) or "."
    cmd = (f"mkdir -p -- {shlex.quote(parent)} && "
           f"printf '%s' {b64} | base64 -d > {shlex.quote(path)}")
    res = runtime.exec(["bash", "-c", cmd], cwd="/", timeout_s=timeout_s)
    if res.timed_out:
        raise CommandTimeoutError(f"Timed out writing file {path}", result=res)
    if res.exit_code != 0:
        raise CommandExecutionError(f"Failed to write file {path}: {res.stderr}", result=res)


def read_file_via_exec(runtime: "RuntimeGuestHook", path: str,
                       timeout_s: float = 120.0) -> bytes:
    """Read a file by running ``base64`` through ``runtime.exec``.

    Raises FileNotFoundError only if the path does not exist; other failures
    raise CommandExecutionError (CommandTimeoutError on timeout).
    """
    q = shlex.quote(path)
    cmd = f"test -e {q} || exit {_MISSING_FILE_EXIT}; base64 < {q}"
    res = runtime.exec(["bash", "-c", cmd], cwd="/", timeout_s=timeout_s)
    if res.timed_out:
        raise CommandTimeoutError(f"Timed out reading file {path}", result=res)
    if res.exit_code == _MISSING_FILE_EXIT:
        raise FileNotFoundError(f"File not found in sandbox: {path}")
    if res.exit_code != 0:
        raise CommandExecutionError(f"Failed to read file {path}: {res.stderr}", result=res)
    return base64.b64decode("".join(res.stdout.split()))


class InteractiveSession(ABC):
    """Held-open interactive shell stream session."""

    @abstractmethod
    def run(self, command: str | List[str], timeout_s: Optional[float] = None) -> str:
        """Run command over the session."""

    @abstractmethod
    def close(self) -> None:
        """Close session stream."""


class RuntimeGuestHook(ABC):
    """
    Data Plane (Runtime Protocol) Interface.
    
    Governs in-guest execution: running commands, streaming processes,
    transferring files, and exposing interactive sessions.
    """

    @abstractmethod
    def exec(self, command: str | List[str], cwd: str = "/testbed",
             env: Optional[Dict[str, str]] = None, timeout_s: float = 120.0) -> ExecResult:
        """Execute command inside the guest container/actor.

        Returns an ExecResult only if the guest ran the command and it either
        exited or was killed at ``timeout_s`` (``timed_out=True``). Raises an
        ``InfrastructureError`` subclass for every other outcome; transport or
        HTTP/gRPC statuses are never encoded as exit codes.
        """

    @abstractmethod
    def stream_process(self, argv: List[str], cwd: str = "/testbed",
                       env: Optional[Dict[str, str]] = None) -> Iterator[str]:
        """Stream real-time output chunks from a running guest process."""

    @abstractmethod
    def write_file(self, path: str, content: bytes | str) -> None:
        """Write file content directly into the guest filesystem."""

    @abstractmethod
    def read_file_bytes(self, path: str) -> bytes:
        """Read binary file content from the guest filesystem."""

    async def exec_async(self, command: str | List[str], cwd: str = "/testbed",
                         env: Optional[Dict[str, str]] = None, timeout_s: float = 120.0) -> ExecResult:
        """Asynchronously execute command inside the guest container/actor."""
        import asyncio
        return await asyncio.to_thread(self.exec, command, cwd=cwd, env=env, timeout_s=timeout_s)

    async def write_file_async(self, path: str, content: bytes | str) -> None:
        """Asynchronously write file content directly into the guest filesystem."""
        import asyncio
        await asyncio.to_thread(self.write_file, path, content)

    async def read_file_bytes_async(self, path: str) -> bytes:
        """Asynchronously read binary file content from the guest filesystem."""
        import asyncio
        return await asyncio.to_thread(self.read_file_bytes, path)

    def initialize(self, init_script: Optional[str] = None, cwd: str = "/testbed",
                   timeout_s: float = 120.0) -> ExecResult:
        """Run post-boot initialization script inside the guest."""
        if not init_script:
            return ExecResult(exit_code=0, stdout="", stderr="", duration_s=0.0)
        return self.exec(["bash", "-c", init_script], cwd=cwd, timeout_s=timeout_s)

    @abstractmethod
    def open_session(self) -> InteractiveSession:
        """Open persistent bidirectional interactive terminal session."""

    def close(self) -> None:
        """Release client-side resources (connections, channels). Idempotent."""
