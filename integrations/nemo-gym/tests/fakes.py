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

"""In-process fake of ate-env-api speaking the current ateenv.v1alpha protocol.

Mirrors just enough of EnvironmentService, ProcessService and FileSystemService
for the provider's contract tests; the knobs on FakeAteEnv script failures.
"""

from __future__ import annotations

import asyncio
import contextlib
import time
from dataclasses import dataclass, field

import grpc
import grpc.aio
from ate_env import EnvironmentStatus, ProcessState
from ate_env._gen.ateenv.v1alpha import env_pb2, env_pb2_grpc, guest_pb2, guest_pb2_grpc

DEFAULT_ATESPACE = "ate-env"
DEFAULT_TEMPLATE = "default-template"
SIGKILL = 9


@dataclass
class FakeProc:
    env_id: str
    argv: list[str]
    stdout: bytes = b""
    stderr: bytes = b""
    exit_code: int | None = None  # None while running
    killed: bool = False
    timeout_s: float | None = None  # guest-side deadline the caller asked for
    started_at: float = field(default_factory=time.monotonic)

    @property
    def state(self) -> int:
        return int(ProcessState.RUNNING if self.exit_code is None else ProcessState.EXITED)

    def to_pb(self, process_id: str) -> guest_pb2.Process:
        return guest_pb2.Process(
            process_id=process_id,
            command=self.argv,
            pid=4242,
            state=self.state,
            exit_code=self.exit_code or 0,
        )


@dataclass
class FakeAteEnv:
    """State shared by the fake services, plus the knobs tests script."""

    envs: dict[str, dict] = field(default_factory=dict)
    files: dict[tuple[str, str], bytes] = field(default_factory=dict)
    modes: dict[tuple[str, str], int] = field(default_factory=dict)
    starts: list[dict] = field(default_factory=list)
    procs: dict[str, FakeProc] = field(default_factory=dict)
    create_code: grpc.StatusCode | None = None
    get_code: grpc.StatusCode | None = None
    delete_code: grpc.StatusCode | None = None
    ready_after_probes: int = 0  # fail this many `true` probes with UNAVAILABLE first
    hang: set[str] = field(default_factory=set)  # shell commands whose process never exits
    _probes: int = 0
    _seq: int = 0

    def env_pb(self, env_id: str) -> env_pb2.Environment:
        e = self.envs[env_id]
        return env_pb2.Environment(
            id=env_id,
            atespace=e["atespace"],
            template=env_pb2.Template(name=e["template"], atespace=e["template_atespace"]),
            status=e["status"],
        )

    def set_status(self, env_id: str, status: EnvironmentStatus) -> None:
        self.envs[env_id]["status"] = int(status)


async def _env_from_metadata(state: FakeAteEnv, context) -> str:
    md = dict(context.invocation_metadata())
    env_id = md.get("x-env-id", "")
    if not env_id:
        await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "missing x-env-id metadata")
    if env_id not in state.envs:
        await context.abort(grpc.StatusCode.NOT_FOUND, f"environment {env_id!r} not found")
    return env_id


class FakeEnvironmentService(env_pb2_grpc.EnvironmentServiceServicer):
    def __init__(self, state: FakeAteEnv) -> None:
        self.state = state

    async def CreateEnvironment(self, request, context):
        s = self.state
        if s.create_code is not None:
            await context.abort(s.create_code, "create refused")
        if request.id in s.envs:
            await context.abort(grpc.StatusCode.ALREADY_EXISTS, "exists")
        base = request.template.name or DEFAULT_TEMPLATE
        # Like ate-env-api: an image on the request yields a template derived
        # from the base, named after the base and the image's digest prefix.
        template = base
        if request.rootfs_image:
            digest = request.rootfs_image.split("@sha256:", 1)[-1][:12]
            template = f"{base}-{digest}"
        s.envs[request.id] = {
            "atespace": request.atespace or DEFAULT_ATESPACE,
            "template": template,
            "base_template": base,
            "rootfs_image": request.rootfs_image,
            "template_atespace": request.template.atespace or DEFAULT_ATESPACE,
            "status": int(EnvironmentStatus.RUNNING),
        }
        return env_pb2.CreateEnvironmentResponse(environment=s.env_pb(request.id))

    async def GetEnvironment(self, request, context):
        s = self.state
        if s.get_code is not None:
            await context.abort(s.get_code, "get refused")
        if request.id not in s.envs:
            await context.abort(grpc.StatusCode.NOT_FOUND, "no such env")
        return env_pb2.GetEnvironmentResponse(environment=s.env_pb(request.id))

    async def SuspendEnvironment(self, request, context):
        if request.id not in self.state.envs:
            await context.abort(grpc.StatusCode.NOT_FOUND, "no such env")
        self.state.set_status(request.id, EnvironmentStatus.SUSPENDED)
        return env_pb2.SuspendEnvironmentResponse()

    async def DeleteEnvironment(self, request, context):
        s = self.state
        if s.delete_code is not None:
            await context.abort(s.delete_code, "delete refused")
        if s.envs.pop(request.id, None) is None:
            await context.abort(grpc.StatusCode.NOT_FOUND, "no such env")
        return env_pb2.DeleteEnvironmentResponse()


class FakeProcessService(guest_pb2_grpc.ProcessServiceServicer):
    def __init__(self, state: FakeAteEnv) -> None:
        self.state = state

    async def StartProcess(self, request, context):
        s = self.state
        env_id = await _env_from_metadata(s, context)
        argv = list(request.command)
        shell_cmd = argv[-1] if argv else ""
        timeout_s = request.timeout.ToTimedelta().total_seconds() if request.HasField("timeout") else None
        s.starts.append(
            {
                "env_id": env_id,
                "command": argv,
                "cwd": request.cwd,
                "env": dict(request.env),
                "timeout": timeout_s,
            }
        )
        if shell_cmd == "true" and s._probes < s.ready_after_probes:
            s._probes += 1
            await context.abort(grpc.StatusCode.UNAVAILABLE, "guest not up yet")
        s._seq += 1
        pid = f"p{s._seq}"
        proc = FakeProc(env_id=env_id, argv=argv, timeout_s=timeout_s)
        if shell_cmd in s.hang:
            proc.exit_code = None
        elif "exit 7" in shell_cmd:
            proc.stderr, proc.exit_code = b"boom", 7
        else:
            proc.stdout, proc.exit_code = b"ok", 0
        s.procs[pid] = proc
        return proc.to_pb(pid)

    async def _lookup(self, process_id: str, context) -> FakeProc:
        await _env_from_metadata(self.state, context)
        proc = self.state.procs.get(process_id)
        if proc is None:
            await context.abort(grpc.StatusCode.NOT_FOUND, f"process {process_id!r} not found")
        return proc

    async def GetProcess(self, request, context):
        proc = await self._lookup(request.process_id, context)
        return proc.to_pb(request.process_id)

    async def StreamProcessOutput(self, request, context):
        proc = await self._lookup(request.process_id, context)
        out = proc.stdout[request.stdout_offset :]
        err = proc.stderr[request.stderr_offset :]
        if out:
            yield guest_pb2.ProcessOutput(stdout=out)
        if err:
            yield guest_pb2.ProcessOutput(stderr=err)
        if request.follow:
            while proc.exit_code is None:
                if proc.timeout_s is not None and time.monotonic() - proc.started_at >= proc.timeout_s:
                    # The guest's watchdog: SIGKILL to the group at the deadline.
                    proc.exit_code, proc.killed = 128 + SIGKILL, True
                    break
                await asyncio.sleep(0.01)
        if proc.exit_code is not None:
            yield guest_pb2.ProcessOutput(exit=proc.to_pb(request.process_id))

    async def WriteProcessInput(self, request_iterator, context):
        await _env_from_metadata(self.state, context)
        n = 0
        async for req in request_iterator:
            n += len(req.data)
        return guest_pb2.WriteProcessInputResponse(bytes_written=n)

    async def SignalProcess(self, request, context):
        proc = await self._lookup(request.process_id, context)
        if proc.exit_code is None and request.signal in (SIGKILL, 15):
            proc.exit_code, proc.killed = 128 + int(request.signal), True
        return proc.to_pb(request.process_id)


class FakeFileSystemService(guest_pb2_grpc.FileSystemServiceServicer):
    def __init__(self, state: FakeAteEnv) -> None:
        self.state = state

    async def ReadFile(self, request, context):
        env_id = await _env_from_metadata(self.state, context)
        data = self.state.files.get((env_id, request.path))
        if data is None:
            await context.abort(grpc.StatusCode.NOT_FOUND, f"{request.path}: no such file")
        # Small chunks so the client's streaming reassembly is exercised.
        for i in range(0, len(data), 4):
            yield guest_pb2.ReadFileResponse(chunk=data[i : i + 4])

    async def WriteFile(self, request_iterator, context):
        env_id = await _env_from_metadata(self.state, context)
        path, mode, buf = None, 0, bytearray()
        async for req in request_iterator:
            if path is None:
                path, mode = req.path, req.mode
            buf.extend(req.chunk)
        if not path:
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "path is required")
        self.state.files[(env_id, path)] = bytes(buf)
        self.state.modes[(env_id, path)] = mode
        return guest_pb2.WriteFileResponse(bytes_written=len(buf))


@contextlib.asynccontextmanager
async def serve(state: FakeAteEnv | None = None):
    """Run the fake ate-env-api on a loopback port; yields ``(target, state)``."""
    state = state or FakeAteEnv()
    server = grpc.aio.server()
    env_pb2_grpc.add_EnvironmentServiceServicer_to_server(FakeEnvironmentService(state), server)
    guest_pb2_grpc.add_ProcessServiceServicer_to_server(FakeProcessService(state), server)
    guest_pb2_grpc.add_FileSystemServiceServicer_to_server(FakeFileSystemService(state), server)
    port = server.add_insecure_port("127.0.0.1:0")
    await server.start()
    try:
        yield f"127.0.0.1:{port}", state
    finally:
        await server.stop(None)
