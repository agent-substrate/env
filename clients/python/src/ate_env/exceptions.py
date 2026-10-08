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

"""Deprecated: Use ate_env.errors instead.

This module re-exports error classes from ate_env.errors for backwards compatibility.
"""

from __future__ import annotations

import builtins

from .errors import (
    CommandExecutionError,
    CommandStartError,
    CommandTimeoutError,
    EnvError,
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
)

# Backwards compatibility aliases
SandboxError = EnvError
TimeoutError = builtins.TimeoutError

__all__ = [
    "SandboxError",
    "TimeoutError",
    "PreflightError",
    "SandboxStartError",
    "CommandExecutionError",
    "CommandTimeoutError",
    "InfrastructureError",
    "SandboxUnavailableError",
    "SandboxProtocolError",
    "CommandStartError",
    "EnvError",
    "NotFoundError",
    "InvalidArgumentError",
    "PermissionDeniedError",
    "ProcessExitedError",
    "RpcError",
]
