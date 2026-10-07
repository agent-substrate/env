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

"""Public dataclasses and enums mirroring the ateenv.v1alpha proto types.

Generated protobuf classes stay out of the public API; the raw stubs remain
reachable under ate_env._gen for callers that need them.
"""

from __future__ import annotations

import enum
import hashlib
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Any, Dict, List, Literal, Optional

from ._gen.ateenv.v1alpha import env_pb2, guest_pb2

__all__ = [
    "EnvironmentStatus",
    "ProcessState",
    "Signal",
    "Template",
    "EnvironmentInfo",
    "ProcessInfo",
    "ProcessOutput",
    "ShellResult",
    "ResourceLimits",
    "PlacementSpec",
    "EnvironmentSpec",
    "Task",
    "ExecResult",
    "DataPlaneEndpoint",
    "RawSandboxInstance",
    "PlanEntry",
    "FleetPlan",
]



class EnvironmentStatus(enum.IntEnum):
    """Lifecycle status of an environment (ateenv.v1alpha.EnvironmentStatus)."""

    UNSPECIFIED = 0
    RESUMING = 1
    RUNNING = 2
    SUSPENDING = 3
    SUSPENDED = 4
    PAUSING = 5
    PAUSED = 6
    CRASHED = 7
    DELETING = 8


class ProcessState(enum.IntEnum):
    """Lifecycle state of a process (ateenv.v1alpha.ProcessState)."""

    UNSPECIFIED = 0
    RUNNING = 1
    EXITED = 2


class Signal(enum.IntEnum):
    """POSIX signal deliverable with Process.signal(); values match Linux signal numbers."""

    HUP = 1
    INT = 2
    QUIT = 3
    ILL = 4
    TRAP = 5
    ABRT = 6
    BUS = 7
    FPE = 8
    KILL = 9
    USR1 = 10
    SEGV = 11
    USR2 = 12
    PIPE = 13
    ALRM = 14
    TERM = 15
    CHLD = 17
    CONT = 18
    STOP = 19
    TSTP = 20
    TTIN = 21
    TTOU = 22
    URG = 23
    XCPU = 24
    XFSZ = 25
    VTALRM = 26
    PROF = 27
    WINCH = 28
    IO = 29
    SYS = 31


@dataclass(frozen=True)
class Template:
    """ActorTemplate an environment is instantiated from."""

    name: str
    atespace: str


@dataclass(frozen=True)
class EnvironmentInfo:
    """Details and status of an environment."""

    id: str
    atespace: str
    template: Template | None
    status: EnvironmentStatus


@dataclass(frozen=True)
class ProcessInfo:
    """Identity, lifecycle state, and exit status of a process."""

    process_id: str
    command: tuple[str, ...]
    pid: int
    state: ProcessState
    exit_code: int  # valid once EXITED; 128 + signal number if killed by a signal
    started_at: datetime | None
    finished_at: datetime | None

    @property
    def running(self) -> bool:
        """True until the process has exited."""
        return self.state == ProcessState.RUNNING


@dataclass(frozen=True)
class ProcessOutput:
    """One message from a process output stream; exactly one field is set.

    exit is the final message: the process has exited and all output was
    delivered. It is absent if the stream ends while the process runs.
    """

    stdout: bytes | None = None
    stderr: bytes | None = None
    exit: ProcessInfo | None = None


@dataclass(frozen=True)
class ShellResult:
    """Captured output and exit status of a shell command."""

    stdout: str
    stderr: str
    exit_code: int  # 128 + signal number if killed by a signal


def _template_from_pb(pb: env_pb2.Template) -> Template:
    return Template(name=pb.name, atespace=pb.atespace)


def _environment_info_from_pb(pb: env_pb2.Environment) -> EnvironmentInfo:
    template = _template_from_pb(pb.template) if pb.HasField("template") else None
    return EnvironmentInfo(
        id=pb.id,
        atespace=pb.atespace,
        template=template,
        status=EnvironmentStatus(pb.status),
    )


def _process_info_from_pb(pb: guest_pb2.Process) -> ProcessInfo:
    started_at = (
        pb.started_at.ToDatetime(tzinfo=timezone.utc) if pb.HasField("started_at") else None
    )
    finished_at = (
        pb.finished_at.ToDatetime(tzinfo=timezone.utc) if pb.HasField("finished_at") else None
    )
    return ProcessInfo(
        process_id=pb.process_id,
        command=tuple(pb.command),
        pid=pb.pid,
        state=ProcessState(pb.state),
        exit_code=pb.exit_code,
        started_at=started_at,
        finished_at=finished_at,
    )


def _process_output_from_pb(pb: guest_pb2.ProcessOutput) -> ProcessOutput:
    which = pb.WhichOneof("output")
    if which == "stdout":
        return ProcessOutput(stdout=pb.stdout)
    if which == "stderr":
        return ProcessOutput(stderr=pb.stderr)
    if which == "exit":
        return ProcessOutput(exit=_process_info_from_pb(pb.exit))
    raise ValueError(f"ate_env: unexpected process output {pb!r}")


@dataclass(frozen=True)
class ResourceLimits:
    """CPU and memory limits for a sandbox environment."""
    cpu: str = "2"
    memory: str = "4Gi"


@dataclass(frozen=True)
class PlacementSpec:
    """Placement constraints and worker hardware selectors."""
    node_selector: Dict[str, str] = field(default_factory=dict)
    tolerations: List[Dict[str, Any]] = field(default_factory=list)
    worker_family: Optional[str] = None  # e.g., "c2", "n2", "c3"


@dataclass(frozen=True)
class EnvironmentSpec:
    """Immutable environment definition representing image, bundle, and constraints."""
    image: str
    runtime_bundle: str = "default-guest"
    limits: ResourceLimits = field(default_factory=ResourceLimits)
    placement: PlacementSpec = field(default_factory=PlacementSpec)
    snapshot_storage_uri: Optional[str] = None

    def template_key(self) -> str:
        """Derive canonical template ID including CPU family and bundle digest."""
        raw = f"{self.image}::{self.runtime_bundle}::{self.placement.worker_family or 'ambient'}"
        img_hash = hashlib.sha256(raw.encode()).hexdigest()[:12]
        # Clean alphanumeric template name
        clean_img = self.image.split("/")[-1].split(":")[0].replace(".", "-").replace("_", "-")
        return f"tmpl-{clean_img[:20]}-{img_hash}"


@dataclass
class Task:
    """Single workload/task definition (e.g. one SWE-bench instance)."""
    id: str
    image: str
    metadata: Dict[str, Any] = field(default_factory=dict)


@dataclass
class ExecResult:
    """Outcome of a command that the guest actually ran.

    A runtime returns an ExecResult only when the guest started the command
    and either observed it exit (``exit_code`` set) or killed it at its
    deadline (``timed_out=True`` and ``exit_code=None``; output may be
    partial). Anything else (unreachable sandbox, HTTP/gRPC error, malformed
    response) raises ``InfrastructureError`` instead, so transport failures
    can never be mistaken for agent failures.

    ``exit_code == -1`` means the guest reported that the process was
    terminated by a signal before its deadline (for example OOM-killed);
    details are appended to ``stderr``.
    """
    exit_code: Optional[int]
    stdout: str
    stderr: str
    duration_s: float = 0.0
    timed_out: bool = False

    def __post_init__(self) -> None:
        if self.timed_out and self.exit_code is not None:
            raise ValueError("ExecResult: exit_code must be None when timed_out=True")
        if not self.timed_out and self.exit_code is None:
            raise ValueError("ExecResult: exit_code is required unless timed_out=True")

    @property
    def ok(self) -> bool:
        """True iff the command finished within its deadline with exit code 0."""
        return not self.timed_out and self.exit_code == 0


_SENSITIVE_HEADERS = frozenset({"authorization", "proxy-authorization", "cookie", "x-api-key"})


@dataclass(repr=False)
class DataPlaneEndpoint:
    """Per-sandbox data-plane coordinates produced by a BackendDriver.

    Attributes:
        protocol: ``"http"`` (requests to ``address`` reach the sandbox's
            primary HTTP port) or ``"grpc"`` (in-guest gRPC server).
        address: Base URL for http (e.g. ``"http://atenet-router:8080"``);
            ``host:port`` for grpc.
        headers: HTTP headers or gRPC metadata that route to and authenticate
            against this specific sandbox, e.g.
            ``{"ate-target-actor": "<atespace>/<actor>"}``. Keys are lowercase.
    """
    protocol: Literal["http", "grpc"]
    address: str
    headers: Dict[str, str] = field(default_factory=dict)

    def __repr__(self) -> str:
        shown = {k: ("<redacted>" if k.lower() in _SENSITIVE_HEADERS else v)
                 for k, v in self.headers.items()}
        return (f"DataPlaneEndpoint(protocol={self.protocol!r}, "
                f"address={self.address!r}, headers={shown!r})")


@dataclass
class RawSandboxInstance:
    """Low-level sandbox handle returned by BackendDriver."""
    instance_id: str
    endpoint: str
    template_id: str
    run_id: str
    status: str = "RUNNING"
    metadata: Dict[str, Any] = field(default_factory=dict)
    # Keyed by data-plane name matching FleetConfig.data_plane ("router", "grpc").
    data_planes: Dict[str, DataPlaneEndpoint] = field(default_factory=dict)


@dataclass
class PlanEntry:
    """One environment's calculated provisioning plan."""
    image: str
    template_id: str
    replicas: int
    tasks: int


class FleetPlan:
    """The complete provisioning plan computed by SandboxFleet.plan()."""
    def __init__(self, entries: List[PlanEntry]):
        self.entries = entries
        self._by_image = {e.image: e for e in entries}

    def for_image(self, image: str) -> Optional[PlanEntry]:
        return self._by_image.get(image)

    @property
    def total_replicas(self) -> int:
        return sum(e.replicas for e in self.entries)
