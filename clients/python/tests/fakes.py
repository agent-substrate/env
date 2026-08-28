"""In-process grpc.aio fake servicers mirroring the real server semantics.

The metadata contract and error codes/messages replicate
internal/apiservice/server.go and the guest services, so the client is
exercised against realistic behavior without a cluster.
"""

from __future__ import annotations

from dataclasses import dataclass, field

import grpc

from ate_env._gen.ateenv.v1 import env_pb2, env_pb2_grpc, guest_pb2, guest_pb2_grpc

CHUNK_SIZE = 64 * 1024


async def _require_env(context: grpc.aio.ServicerContext) -> tuple[str, str]:
    """Extract (env_id, atespace) metadata like the API server's proxy does."""
    md = {key: value for key, value in (context.invocation_metadata() or ())}
    env_id = md.get("x-env-id", "")
    if not env_id:
        await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "x-env-id header is required")
    return env_id, md.get("x-env-atespace", "default")


class FakeEnvironmentService(env_pb2_grpc.EnvironmentServiceServicer):
    def __init__(self):
        self.environments: dict[tuple[str, str], env_pb2.Environment] = {}
        self.last_create_request: env_pb2.CreateEnvironmentRequest | None = None

    async def CreateEnvironment(self, request, context):
        self.last_create_request = request
        atespace = request.atespace or "default"
        key = (atespace, request.id)
        if not request.id:
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "id is required")
        if key in self.environments:
            await context.abort(
                grpc.StatusCode.ALREADY_EXISTS,
                f'environment "{request.id}" already exists',
            )
        template = env_pb2.Template(name="default-env", namespace="ate-env")
        if request.HasField("template"):
            if request.template.name:
                template.name = request.template.name
            if request.template.namespace:
                template.namespace = request.template.namespace
        environment = env_pb2.Environment(
            id=request.id,
            atespace=atespace,
            template=template,
            status=env_pb2.ENVIRONMENT_STATUS_RUNNING,
        )
        self.environments[key] = environment
        return env_pb2.CreateEnvironmentResponse(environment=environment)

    async def GetEnvironment(self, request, context):
        environment = await self._lookup(request, context)
        return env_pb2.GetEnvironmentResponse(environment=environment)

    async def SuspendEnvironment(self, request, context):
        environment = await self._lookup(request, context)
        environment.status = env_pb2.ENVIRONMENT_STATUS_SUSPENDED
        return env_pb2.SuspendEnvironmentResponse()

    async def DeleteEnvironment(self, request, context):
        await self._lookup(request, context)
        del self.environments[(request.atespace or "default", request.id)]
        return env_pb2.DeleteEnvironmentResponse()

    async def _lookup(self, request, context):
        key = (request.atespace or "default", request.id)
        environment = self.environments.get(key)
        if environment is None:
            await context.abort(
                grpc.StatusCode.NOT_FOUND, f'environment "{request.id}" not found'
            )
        return environment


@dataclass
class FakeProc:
    """Scripted behavior for one process started against the fake.

    running_polls is the number of GetProcess calls that still report
    RUNNING before the final status is returned — this exercises the
    client's wait()/shell() poll loop, analogous to the sibling SDK's
    "resuming" counter.
    """

    chunks: list[tuple[int, bytes]] = field(default_factory=list)  # (OutputSource, data)
    exit_code: int = 0
    running_polls: int = 0
    killed: bool = False
    # request capture, filled by StartProcess:
    command: list[str] = field(default_factory=list)
    cwd: str = ""
    env: dict[str, str] = field(default_factory=dict)


class FakeProcessService(guest_pb2_grpc.ProcessServiceServicer):
    def __init__(self):
        self.next_procs: list[FakeProc] = []  # scripts for upcoming StartProcess calls
        self.procs: dict[str, FakeProc] = {}
        self.last_env: tuple[str, str] | None = None
        self._counter = 0

    async def StartProcess(self, request, context):
        self.last_env = await _require_env(context)
        proc = self.next_procs.pop(0) if self.next_procs else FakeProc()
        proc.command = list(request.command)
        proc.cwd = request.cwd
        proc.env = dict(request.env)
        self._counter += 1
        process_id = f"proc-{self._counter}"
        self.procs[process_id] = proc
        return guest_pb2.StartProcessResponse(process_id=process_id)

    async def GetProcess(self, request, context):
        self.last_env = await _require_env(context)
        proc = await self._lookup(request.process_id, context)
        if proc.running_polls > 0:
            proc.running_polls -= 1
            return guest_pb2.Process(
                process_id=request.process_id,
                status=guest_pb2.PROCESS_STATUS_RUNNING,
            )
        if proc.killed:
            status = guest_pb2.PROCESS_STATUS_TERMINATED
        elif proc.exit_code == 0:
            status = guest_pb2.PROCESS_STATUS_COMPLETED
        else:
            status = guest_pb2.PROCESS_STATUS_FAILED
        final = guest_pb2.Process(
            process_id=request.process_id,
            status=status,
            exit_code=proc.exit_code,
        )
        final.started_at.GetCurrentTime()
        final.finished_at.GetCurrentTime()
        return final

    async def StreamProcessOutputs(self, request, context):
        self.last_env = await _require_env(context)
        proc = await self._lookup(request.process_id, context)
        stdout_skip = request.stdout_offset
        stderr_skip = request.stderr_offset
        for source, data in proc.chunks:
            if source == guest_pb2.OUTPUT_SOURCE_STDOUT and stdout_skip:
                data, stdout_skip = data[stdout_skip:], max(0, stdout_skip - len(data))
            elif source == guest_pb2.OUTPUT_SOURCE_STDERR and stderr_skip:
                data, stderr_skip = data[stderr_skip:], max(0, stderr_skip - len(data))
            if data:
                yield guest_pb2.OutputChunk(source=source, data=data)

    async def KillProcess(self, request, context):
        self.last_env = await _require_env(context)
        proc = await self._lookup(request.process_id, context)
        proc.killed = True
        proc.exit_code = 137
        proc.running_polls = 0
        return guest_pb2.KillProcessResponse(exit_code=137)

    async def _lookup(self, process_id, context):
        proc = self.procs.get(process_id)
        if proc is None:
            await context.abort(
                grpc.StatusCode.NOT_FOUND, f'process "{process_id}" not found'
            )
        return proc


class FakeFileSystemService(guest_pb2_grpc.FileSystemServiceServicer):
    def __init__(self):
        self.files: dict[str, bytes] = {}
        self.modes: dict[str, int] = {}
        self.last_env: tuple[str, str] | None = None
        self.last_write_chunk_sizes: list[int] = []

    async def _validate_path(self, path, context):
        # Mirrors guest/filesystem resolveAndValidatePath's sandbox check.
        if path.startswith(".."):
            await context.abort(
                grpc.StatusCode.PERMISSION_DENIED,
                f'access denied: path "{path}" is outside sandbox root directory "/workspace"',
            )

    async def ReadFile(self, request, context):
        self.last_env = await _require_env(context)
        await self._validate_path(request.path, context)
        data = self.files.get(request.path)
        if data is None:
            await context.abort(
                grpc.StatusCode.NOT_FOUND,
                f"open {request.path}: no such file or directory",
            )
        for i in range(0, len(data), CHUNK_SIZE):
            yield guest_pb2.FileChunk(data=data[i : i + CHUNK_SIZE])

    async def WriteFile(self, request_iterator, context):
        self.last_env = await _require_env(context)
        path = None
        mode = 0
        buf = bytearray()
        chunk_sizes = []
        async for request in request_iterator:
            if path is None:
                if not request.path:
                    await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "path is required")
                await self._validate_path(request.path, context)
                path = request.path
                mode = request.mode
            buf.extend(request.chunk)
            chunk_sizes.append(len(request.chunk))
        self.last_write_chunk_sizes = chunk_sizes
        if path is None:
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "path is required")
        self.files[path] = bytes(buf)
        self.modes[path] = mode
        return guest_pb2.WriteFileResponse(bytes_written=len(buf))
