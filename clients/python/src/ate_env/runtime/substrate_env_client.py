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

import asyncio
import logging
import threading
import time
from typing import Any, Dict, Iterator, List, Optional

from ..client import Client as AteClient
from ..env import Env as AteEnv
from ..errors import (
    EnvError,
    InvalidArgumentError,
    NotFoundError,
    PermissionDeniedError,
    RpcError,
)


from ..exceptions import (
    CommandExecutionError,
    CommandStartError,
    CommandTimeoutError,
    InfrastructureError,
    SandboxProtocolError,
    SandboxUnavailableError,
)
from ..types import ExecResult
from .base import (
    InteractiveSession,
    RuntimeGuestHook,
    command_to_argv,
)

logger = logging.getLogger("ate_env.runtime.substrate_env_client")


class _SharedAteClientManager:
    """Manages thread-safe, shared async event loop and AteClient instances."""

    def __init__(self):
        self._lock = threading.Lock()
        self._loop: Optional[asyncio.AbstractEventLoop] = None
        self._thread: Optional[threading.Thread] = None
        self._clients: Dict[str, AteClient] = {}

    def _ensure_running(self):
        with self._lock:
            if self._thread is None or not self._thread.is_alive():
                ready = threading.Event()

                def _loop_thread_main():
                    asyncio.set_event_loop(self._loop)
                    ready.set()
                    self._loop.run_forever()

                self._loop = asyncio.new_event_loop()
                self._thread = threading.Thread(
                    target=_loop_thread_main,
                    name="ate-env-client-loop",
                    daemon=True,
                )
                self._thread.start()
                ready.wait()

    def get_client(self, endpoint: str) -> AteClient:
        self._ensure_running()
        with self._lock:
            if endpoint not in self._clients:
                async def _make_client():
                    return AteClient(endpoint)

                fut = asyncio.run_coroutine_threadsafe(_make_client(), self._loop)
                self._clients[endpoint] = fut.result()
            return self._clients[endpoint]

    def run_sync(self, coro, timeout_s: Optional[float] = None) -> Any:
        self._ensure_running()
        fut = asyncio.run_coroutine_threadsafe(coro, self._loop)
        return fut.result(timeout=timeout_s)

    async def run_async(self, coro) -> Any:
        self._ensure_running()
        current_loop = asyncio.get_running_loop()
        if current_loop is self._loop:
            return await coro
        fut = asyncio.wrap_future(asyncio.run_coroutine_threadsafe(coro, self._loop))
        return await fut


_CLIENT_MANAGER = _SharedAteClientManager()


class SubstrateEnvClientRuntime(RuntimeGuestHook):
    """
    Substrate guest hook wrapping the official ate-env-client (ate_env package).

    Directly leverages ate_env.Client and ate_env.Env for process execution,
    streaming, and direct file reading/writing over gRPC.
    """

    def __init__(
        self,
        endpoint: str,
        env_id: str,
        atespace: str = "default",
    ):
        self.endpoint = endpoint
        self.env_id = env_id
        self.atespace = atespace
        self._client = _CLIENT_MANAGER.get_client(endpoint)
        self._env = self._client.env(env_id, atespace=atespace)
        self._closed = False

    def close(self) -> None:
        self._closed = True

    def _check_open(self) -> None:
        if self._closed:
            raise RuntimeError(f"runtime for environment {self.env_id} is closed")

    def exec(
        self,
        command: str | List[str],
        cwd: str = "/testbed",
        env: Optional[Dict[str, str]] = None,
        timeout_s: float = 120.0,
    ) -> ExecResult:
        self._check_open()
        t0 = time.monotonic()
        argv = command_to_argv(command)

        async def _run():
            proc = await self._env.start_process(argv, cwd=cwd, env=env)
            stdout = bytearray()
            stderr = bytearray()
            exit_code = None
            async for chunk in proc.output(follow=True):
                if chunk.stdout is not None:
                    stdout.extend(chunk.stdout)
                elif chunk.stderr is not None:
                    stderr.extend(chunk.stderr)
                elif chunk.exit is not None:
                    exit_code = chunk.exit.exit_code
            if exit_code is None:
                proc_info = await proc.wait()
                exit_code = proc_info.exit_code
            return (
                exit_code,
                stdout.decode("utf-8", errors="replace"),
                stderr.decode("utf-8", errors="replace"),
            )

        try:
            exit_code, out, err = _CLIENT_MANAGER.run_sync(_run(), timeout_s=timeout_s + 5.0)
            duration = time.monotonic() - t0
            return ExecResult(
                exit_code=exit_code,
                stdout=out,
                stderr=err,
                duration_s=duration,
            )
        except (TimeoutError, asyncio.TimeoutError):
            duration = time.monotonic() - t0
            return ExecResult(
                exit_code=None,
                stdout="",
                stderr="",
                duration_s=duration,
                timed_out=True,
            )
        except InvalidArgumentError as e:
            raise SandboxProtocolError(str(e), sandbox_id=self.env_id) from e
        except NotFoundError as e:
            raise CommandStartError(str(e), sandbox_id=self.env_id) from e
        except RpcError as e:
            raise SandboxUnavailableError(str(e), sandbox_id=self.env_id) from e
        except Exception as e:
            raise InfrastructureError(str(e), sandbox_id=self.env_id) from e

    def stream_process(
        self,
        argv: List[str],
        cwd: str = "/testbed",
        env: Optional[Dict[str, str]] = None,
    ) -> Iterator[str]:
        self._check_open()

        async def _collect():
            proc = await self._env.start_process(argv, cwd=cwd, env=env)
            chunks = []
            async for chunk in proc.output(follow=True):
                if chunk.stdout is not None:
                    chunks.append(chunk.stdout.decode("utf-8", errors="replace"))
                elif chunk.stderr is not None:
                    chunks.append(chunk.stderr.decode("utf-8", errors="replace"))
            return chunks

        try:
            chunks = _CLIENT_MANAGER.run_sync(_collect())
            yield from chunks
        except Exception as e:
            raise InfrastructureError(str(e), sandbox_id=self.env_id) from e

    async def exec_async(
        self,
        command: str | List[str],
        cwd: str = "/testbed",
        env: Optional[Dict[str, str]] = None,
        timeout_s: float = 120.0,
    ) -> ExecResult:
        """Native async execution over ate_env without blocking the event loop."""
        self._check_open()
        t0 = time.monotonic()
        argv = command_to_argv(command)

        async def _exec():
            proc = await self._env.start_process(argv, cwd=cwd, env=env)
            stdout = bytearray()
            stderr = bytearray()
            exit_code = None
            async for chunk in proc.output(follow=True):
                if chunk.stdout is not None:
                    stdout.extend(chunk.stdout)
                elif chunk.stderr is not None:
                    stderr.extend(chunk.stderr)
                elif chunk.exit is not None:
                    exit_code = chunk.exit.exit_code
            if exit_code is None:
                proc_info = await proc.wait()
                exit_code = proc_info.exit_code
            return exit_code, stdout, stderr

        try:
            exit_code, stdout, stderr = await _CLIENT_MANAGER.run_async(_exec())
            return ExecResult(
                exit_code=exit_code,
                stdout=stdout.decode("utf-8", errors="replace"),
                stderr=stderr.decode("utf-8", errors="replace"),
                duration_s=time.monotonic() - t0,
            )
        except (TimeoutError, asyncio.TimeoutError):
            return ExecResult(
                exit_code=None,
                stdout="",
                stderr="",
                duration_s=time.monotonic() - t0,
                timed_out=True,
            )
        except InvalidArgumentError as e:
            raise SandboxProtocolError(str(e), sandbox_id=self.env_id) from e
        except NotFoundError as e:
            raise CommandStartError(str(e), sandbox_id=self.env_id) from e
        except RpcError as e:
            raise SandboxUnavailableError(str(e), sandbox_id=self.env_id) from e
        except Exception as e:
            raise InfrastructureError(str(e), sandbox_id=self.env_id) from e

    async def write_file_async(self, path: str, content: bytes | str) -> None:
        """Native async file writing directly over ate_env gRPC."""
        self._check_open()
        data = content.encode("utf-8") if isinstance(content, str) else content

        async def _write():
            await self._env.write_file(path, data)

        try:
            await _CLIENT_MANAGER.run_async(_write())
        except Exception as e:
            raise CommandExecutionError(f"Failed to write file {path}: {e}") from e

    async def read_file_bytes_async(self, path: str) -> bytes:
        """Native async file reading directly over ate_env gRPC."""
        self._check_open()

        async def _read():
            return await self._env.read_file_bytes(path)

        try:
            return await _CLIENT_MANAGER.run_async(_read())
        except NotFoundError:
            raise FileNotFoundError(f"File not found: {path}")
        except Exception as e:
            raise CommandExecutionError(f"Failed to read file {path}: {e}") from e

    def write_file(self, path: str, content: bytes | str) -> None:
        self._check_open()
        data = content.encode("utf-8") if isinstance(content, str) else content

        async def _write():
            await self._env.write_file(path, data)

        try:
            _CLIENT_MANAGER.run_sync(_write())
        except Exception as e:
            raise CommandExecutionError(f"Failed to write file {path}: {e}") from e

    def read_file_bytes(self, path: str) -> bytes:
        self._check_open()

        async def _read():
            return await self._env.read_file_bytes(path)

        try:
            return _CLIENT_MANAGER.run_sync(_read())
        except NotFoundError:
            raise FileNotFoundError(f"File not found: {path}")
        except Exception as e:
            raise CommandExecutionError(f"Failed to read file {path}: {e}") from e

    def open_session(self) -> InteractiveSession:
        class _Session(InteractiveSession):
            def __init__(s, rt):
                s.rt = rt
                s._open = True

            def run(s, command, timeout_s=None):
                if not s._open:
                    raise RuntimeError("Session closed")
                res = s.rt.exec(command, timeout_s=timeout_s or 120.0)
                return res.stdout

            def close(s):
                s._open = False

        return _Session(self)
