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
from dataclasses import dataclass
from datetime import datetime, timezone

from ._gen.ateenv.v1alpha import env_pb2, guest_pb2

__all__ = [
    "EnvironmentStatus",
    "ProcessStatus",
    "OutputSource",
    "Template",
    "EnvironmentInfo",
    "ProcessInfo",
    "OutputChunk",
    "ShellResult",
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


class ProcessStatus(enum.IntEnum):
    """Execution status of an asynchronous process (ateenv.v1alpha.ProcessStatus)."""

    UNSPECIFIED = 0
    RUNNING = 1
    COMPLETED = 2
    FAILED = 3
    TERMINATED = 4


class OutputSource(enum.IntEnum):
    """Output log stream source (ateenv.v1alpha.OutputSource)."""

    UNSPECIFIED = 0
    STDOUT = 1
    STDERR = 2


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
    """Execution state and metadata of a process."""

    process_id: str
    status: ProcessStatus
    exit_code: int  # valid once status is not RUNNING
    started_at: datetime | None
    finished_at: datetime | None


@dataclass(frozen=True)
class OutputChunk:
    """A chunk of process output from stdout or stderr."""

    source: OutputSource
    data: bytes


@dataclass(frozen=True)
class ShellResult:
    """Captured output and exit code of a shell command."""

    stdout: str
    stderr: str
    exit_code: int


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
        status=ProcessStatus(pb.status),
        exit_code=pb.exit_code,
        started_at=started_at,
        finished_at=finished_at,
    )
