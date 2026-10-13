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
import builtins
import logging
import queue
import threading
import time
from typing import Any, AsyncIterator, Dict, Iterator, List, Optional

import grpc

from ..client import Client as AteClient
from ..env import Env as AteEnv
from ..errors import (
    CommandStartError,
    EnvError,
    InfrastructureError,
    InvalidArgumentError,
    NotFoundError,
    RpcError,
    SandboxProtocolError,
    SandboxUnavailableError,
)
from ..loop_runner import LoopRunner
from ..types import ExecResult, ShellResult
from .base import RuntimeGuestHook

logger = logging.getLogger("ate_env.runtime.substrate_env_client")


class SubstrateEnvClientRuntime(RuntimeGuestHook):
    """Substrate guest hook backed directly by the official ate_env.Env handle.

    Leverages ate_env.Client and ate_env.Env for process execution, streaming,
    and chunked file I/O over gRPC.
    """

    def __init__(
        self,
        env_id: str,
        *,
        endpoint: Optional[str] = None,
        atespace: str = "ate-env",
        client: Optional[AteClient] = None,
        env: Optional[AteEnv] = None,
        loop_runner: Optional[LoopRunner] = None,
    ):
        if not env_id:
            raise ValueError("env_id is required for SubstrateEnvClientRuntime")
        self.env_id = env_id
        self.endpoint = endpoint or "localhost:7777"
        self.atespace = atespace

        self._loop_runner = loop_runner
        self._owns_loop_runner = loop_runner is None
        self._lock = threading.Lock()

        if env is not None:
            self._env = env
            self._client = env._client
            self._owns_client = False
        elif client is not None:
            self._client = client
            self._env = client.env(env_id, atespace=atespace)
            self._owns_client = False
        else:
            self._client = None
            self._env = None
            self._owns_client = True
        self._closed = False

    @property
    def env(self) -> AteEnv:
        self._check_open()
        with self._lock:
            if self._env is None:
                if self._client is None:
                    self._client = AteClient(self.endpoint)
                self._env = self._client.env(self.env_id, atespace=self.atespace)
            return self._env

    def _get_loop_runner(self) -> LoopRunner:
        with self._lock:
            if self._loop_runner is None:
                self._loop_runner = LoopRunner(name="ate-substrate-runtime-sync")
                self._owns_loop_runner = True
            return self._loop_runner

    async def close_async(self) -> None:
        """Close client if owned."""
        if not self._closed:
            self._closed = True
            if self._owns_client and self._client:
                await self._client.close()
                self._client = None
            if self._owns_loop_runner and self._loop_runner:
                self._loop_runner.stop()
                self._loop_runner = None

    def close(self) -> None:
        if not self._closed:
            self._closed = True
            if self._loop_runner:
                if self._owns_client and self._client:
                    self._loop_runner.run(self._client.close())
                    self._client = None
                if self._owns_loop_runner:
                    self._loop_runner.stop()
                    self._loop_runner = None
            elif self._owns_client and self._client:
                self._sync(self._client.close())

    def _check_open(self) -> None:
        if self._closed:
            raise RuntimeError(f"runtime for environment {self.env_id} is closed")

    def _sync(self, coro: Any) -> Any:
        return self._get_loop_runner().run(coro)

    async def exec_async(
        self,
        command: str | List[str],
        cwd: str = "",
        env: Optional[Dict[str, str]] = None,
        timeout_s: float = 120.0,
    ) -> ExecResult:
        """Native async execution honoring command contract: argv list runs directly, string runs in shell."""
        self._check_open()
        t0 = time.monotonic()
        grace_s = 5.0
        active_env = self.env

        try:
            active_proc = None
            try:
                if isinstance(command, (list, tuple)):
                    argv = list(command)
                    if not argv:
                        raise ValueError("command must not be empty")

                    async def _run_argv() -> ShellResult:
                        nonlocal active_proc
                        proc = await active_env.start_process(
                            argv, cwd=cwd, env=env, timeout=timeout_s
                        )
                        active_proc = proc
                        stdout = bytearray()
                        stderr = bytearray()
                        exit_info = None
                        async for out in proc.output(follow=True):
                            if out.stdout is not None:
                                stdout.extend(out.stdout)
                            elif out.stderr is not None:
                                stderr.extend(out.stderr)
                            elif out.exit is not None:
                                exit_info = out.exit
                        if exit_info is None:
                            raise RuntimeError("ate_env: output stream ended before process exited")
                        return ShellResult(
                            stdout=stdout.decode("utf-8", errors="replace"),
                            stderr=stderr.decode("utf-8", errors="replace"),
                            exit_code=exit_info.exit_code,
                        )

                    if hasattr(asyncio, "timeout"):
                        async with asyncio.timeout(timeout_s + grace_s):
                            shell_res = await _run_argv()
                    else:
                        shell_res = await asyncio.wait_for(_run_argv(), timeout=timeout_s + grace_s)
                else:
                    if hasattr(asyncio, "timeout"):
                        async with asyncio.timeout(timeout_s + grace_s):
                            shell_res = await active_env.shell(
                                command, cwd=cwd, env=env, timeout=timeout_s
                            )
                    else:
                        shell_res = await asyncio.wait_for(
                            active_env.shell(command, cwd=cwd, env=env, timeout=timeout_s),
                            timeout=timeout_s + grace_s,
                        )
            except (builtins.TimeoutError, asyncio.TimeoutError):
                if active_proc is not None:
                    try:
                        await active_proc.kill()
                    except Exception:
                        pass
                duration = time.monotonic() - t0
                return ExecResult(
                    exit_code=None,
                    stdout="",
                    stderr="",
                    duration_s=duration,
                    timed_out=True,
                )

            duration = time.monotonic() - t0
            # 137 = 128 + 9 (SIGKILL sent by guest when guest-side timeout elapsed).
            # Note: checking exit_code == 137 and duration >= timeout_s is a heuristic;
            # external kills (like OOM) can also produce exit_code 137.
            timed_out = shell_res.exit_code == 137 and duration >= timeout_s
            return ExecResult(
                exit_code=None if timed_out else shell_res.exit_code,
                stdout=shell_res.stdout,
                stderr=shell_res.stderr,
                duration_s=duration,
                timed_out=timed_out,
            )
        except InvalidArgumentError as e:
            raise SandboxProtocolError(str(e), sandbox_id=self.env_id) from e
        except NotFoundError as e:
            msg = str(e).lower()
            if "environment" in msg or "actor" in msg:
                raise SandboxUnavailableError(str(e), sandbox_id=self.env_id) from e
            raise CommandStartError(str(e), sandbox_id=self.env_id) from e
        except RpcError as e:
            status_name = f"grpc {e.code.name}" if e.code else "grpc RpcError"
            if e.code in (
                grpc.StatusCode.NOT_FOUND,
                grpc.StatusCode.UNAVAILABLE,
                grpc.StatusCode.DEADLINE_EXCEEDED,
                grpc.StatusCode.INTERNAL,
                grpc.StatusCode.ABORTED,
            ):
                raise SandboxUnavailableError(
                    str(e), sandbox_id=self.env_id, status=status_name
                ) from e
            raise SandboxProtocolError(str(e), sandbox_id=self.env_id, status=status_name) from e
        except InfrastructureError:
            raise
        except Exception as e:
            raise InfrastructureError(str(e), sandbox_id=self.env_id) from e

    def exec(
        self,
        command: str | List[str],
        cwd: str = "",
        env: Optional[Dict[str, str]] = None,
        timeout_s: float = 120.0,
    ) -> ExecResult:
        """Synchronous wrapper for exec_async."""
        return self._sync(self.exec_async(command, cwd=cwd, env=env, timeout_s=timeout_s))

    async def stream_process_async(
        self,
        argv: List[str],
        cwd: str = "",
        env: Optional[Dict[str, str]] = None,
    ) -> AsyncIterator[str]:
        """Stream real-time output chunks from running process inside guest."""
        self._check_open()
        try:
            proc = await self.env.start_process(argv, cwd=cwd, env=env)
            async for chunk in proc.output(follow=True):
                if chunk.stdout is not None:
                    yield chunk.stdout.decode("utf-8", errors="replace")
                elif chunk.stderr is not None:
                    yield chunk.stderr.decode("utf-8", errors="replace")
        except (InvalidArgumentError, NotFoundError, RpcError) as e:
            raise SandboxUnavailableError(str(e), sandbox_id=self.env_id) from e
        except Exception as e:
            raise InfrastructureError(str(e), sandbox_id=self.env_id) from e

    def stream_process(
        self,
        argv: List[str],
        cwd: str = "",
        env: Optional[Dict[str, str]] = None,
    ) -> Iterator[str]:
        """Synchronously stream output chunks in real time."""
        self._check_open()
        q: queue.Queue[tuple[Optional[str], Optional[Exception]]] = queue.Queue()

        async def _pump():
            try:
                async for item in self.stream_process_async(argv, cwd=cwd, env=env):
                    q.put((item, None))
                q.put((None, None))
            except Exception as e:
                q.put((None, e))

        fut = self._get_loop_runner().submit(_pump())
        try:
            while True:
                item, err = q.get()
                if err is not None:
                    raise err
                if item is None:
                    break
                yield item
        finally:
            fut.cancel()

    async def write_file_async(self, path: str, content: bytes | str) -> None:
        """Write file content directly into guest filesystem via FileSystemService."""
        self._check_open()
        raw = content.encode("utf-8") if isinstance(content, str) else content
        try:
            await self.env.write_file(path, raw)
        except InvalidArgumentError as e:
            raise SandboxProtocolError(str(e), sandbox_id=self.env_id) from e
        except (NotFoundError, RpcError) as e:
            raise SandboxUnavailableError(str(e), sandbox_id=self.env_id) from e
        except Exception as e:
            raise InfrastructureError(str(e), sandbox_id=self.env_id) from e

    def write_file(self, path: str, content: bytes | str) -> None:
        self._sync(self.write_file_async(path, content))

    async def read_file_bytes_async(self, path: str) -> bytes:
        """Read binary file content from guest filesystem via FileSystemService."""
        self._check_open()
        try:
            return await self.env.read_file_bytes(path)
        except NotFoundError:
            raise FileNotFoundError(f"File not found in sandbox: {path}")
        except InvalidArgumentError as e:
            raise SandboxProtocolError(str(e), sandbox_id=self.env_id) from e
        except RpcError as e:
            raise SandboxUnavailableError(str(e), sandbox_id=self.env_id) from e
        except Exception as e:
            raise InfrastructureError(str(e), sandbox_id=self.env_id) from e

    def read_file_bytes(self, path: str) -> bytes:
        return self._sync(self.read_file_bytes_async(path))
