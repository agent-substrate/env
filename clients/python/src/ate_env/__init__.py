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

"""Async Python client for the Agent Substrate Environment API (ate-env-api).

Quickstart:

    import asyncio
    from ate_env import Client

    async def main():
        client = Client("localhost:7777")
        try:
            env = await client.create("dev1")
            result = await env.shell("echo hello")
            print(result.stdout)
            await env.delete()
        finally:
            await client.close()

    asyncio.run(main())
"""

# Layer 1: Core Client & Env
from .client import DEFAULT_ATESPACE, Client

# Layer 2: High-Level Fleet Management & Abstractions
from .config import FleetConfig
from .env import Env, Process

# Exceptions
from .errors import (
    CommandExecutionError,
    CommandStartError,
    CommandTimeoutError,
    EnvError,
    FailedPreconditionError,
    InfrastructureError,
    InvalidArgumentError,
    NotFoundError,
    PermissionDeniedError,
    PreflightError,
    ProcessExitedError,
    RpcError,
    SandboxProtocolError,
    SandboxStartError,
    SandboxUnavailableError,
    map_rpc_error,
)
from .exceptions import SandboxError
from .fleet import SandboxFleet
from .handle import SandboxHandle

# Types
from .types import (
    DataPlaneEndpoint,
    EnvironmentInfo,
    EnvironmentSpec,
    EnvironmentStatus,
    ExecResult,
    FleetPlan,
    PlacementSpec,
    PlanEntry,
    ProcessInfo,
    ProcessOutput,
    ProcessState,
    RawSandboxInstance,
    ResourceLimits,
    ShellResult,
    Signal,
    Task,
    Template,
)

__all__ = [
    # Low-level API
    "Client",
    "DEFAULT_ATESPACE",
    "Env",
    "Process",
    # High-level Fleet API
    "FleetConfig",
    "SandboxFleet",
    "SandboxHandle",
    # Low-level Errors
    "EnvError",
    "FailedPreconditionError",
    "InvalidArgumentError",
    "NotFoundError",
    "PermissionDeniedError",
    "ProcessExitedError",
    "RpcError",
    "map_rpc_error",
    # Fleet / Execution Exceptions
    "SandboxError",
    "CommandExecutionError",
    "CommandStartError",
    "CommandTimeoutError",
    "InfrastructureError",
    "PreflightError",
    "SandboxProtocolError",
    "SandboxStartError",
    "SandboxUnavailableError",
    # Types
    "EnvironmentInfo",
    "EnvironmentStatus",
    "ProcessInfo",
    "ProcessOutput",
    "ProcessState",
    "ShellResult",
    "Signal",
    "Template",
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

