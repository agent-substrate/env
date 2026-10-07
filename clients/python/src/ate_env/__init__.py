# Copyright 2026 Google LLC & The Kubernetes Authors
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

"""
Unified Agent Substrate Environment & High-Throughput Sandbox SDK (`ate_env`).

Layer 1 (Single Environment Lifecycle & Guest Execution):
    - `Client`: Async gRPC client managing environment lifecycle with ate-env-api.
    - `Env`: Active environment handle for running processes, streaming logs, and file I/O.

Layer 2 (Fleet Management & Scale-out Workloads):
    - `SandboxFleet` / `Fleet`: Batch provisioning, pooling, and pre-warming of sandboxes.
    - `AsyncSandboxFleet` / `AsyncFleet`: Native async pool manager for high-concurrency RL rollouts.
    - `SandboxHandle`: Unified execution handle for pooled sandboxes.
    - `FleetConfig`: Declarative configuration for pooling, timeouts, and backends.
"""

# Layer 1: Core Client & Env
from .client import DEFAULT_ATESPACE, Client
from .env import Env, Process

# Layer 2: High-Level Fleet Management & Abstractions
from .config import FleetConfig
from .fleet import SandboxFleet
from .async_fleet import AsyncSandboxFleet
from .handle import SandboxHandle
from .connector import (
    ConnectionStrategy,
    DirectConnectionStrategy,
    InClusterConnectionStrategy,
    LocalTunnelConnectionStrategy,
    resolve_endpoint,
)

# Exceptions
from .errors import (
    EnvError,
    FailedPreconditionError,
    InvalidArgumentError,
    NotFoundError,
    PermissionDeniedError,
    ProcessExitedError,
    RpcError,
    map_rpc_error,
)
from .exceptions import (
    CapacityError,
    CommandExecutionError,
    CommandStartError,
    CommandTimeoutError,
    InfrastructureError,
    OwnedByAnotherRunError,
    PreflightError,
    SandboxError,
    SandboxProtocolError,
    SandboxStartError,
    SandboxUnavailableError,
    TimeoutError,
)

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

# Friendly aliases
Fleet = SandboxFleet
AsyncFleet = AsyncSandboxFleet
EnvHandle = SandboxHandle

__version__ = "0.1.0"

__all__ = [
    # Low-level API
    "Client",
    "DEFAULT_ATESPACE",
    "Env",
    "Process",
    # High-level Fleet API
    "FleetConfig",
    "SandboxFleet",
    "AsyncSandboxFleet",
    "SandboxHandle",
    "Fleet",
    "AsyncFleet",
    "EnvHandle",
    # Connection
    "ConnectionStrategy",
    "DirectConnectionStrategy",
    "InClusterConnectionStrategy",
    "LocalTunnelConnectionStrategy",
    "resolve_endpoint",
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
    "CapacityError",
    "CommandExecutionError",
    "CommandStartError",
    "CommandTimeoutError",
    "InfrastructureError",
    "OwnedByAnotherRunError",
    "PreflightError",
    "SandboxProtocolError",
    "SandboxStartError",
    "SandboxUnavailableError",
    "TimeoutError",
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

