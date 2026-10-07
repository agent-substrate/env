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

from typing import TYPE_CHECKING, Any, Optional

if TYPE_CHECKING:  # pragma: no cover
    from .types import ExecResult


class SandboxError(Exception):
    """Base error for all sandbox-sdk operations."""


class PreflightError(SandboxError):
    """Raised when cluster, permissions, capacity, or storage verification fails."""


class CapacityError(SandboxError):
    """Raised when cluster or WorkerPool lacks sufficient headroom."""


class SandboxStartError(SandboxError):
    """Raised when sandbox instantiation, golden restore, or boot fails immediately."""


class CommandExecutionError(SandboxError):
    """Raised when a non-zero exit code or guest error occurs in strict mode."""

    def __init__(self, message: str, result: Optional["ExecResult"] = None):
        super().__init__(message)
        self.result = result


class TimeoutError(SandboxError):
    """Raised when an acquisition, probe, or command exceeds its deadline."""


class CommandTimeoutError(CommandExecutionError, TimeoutError):
    """Raised in strict mode (check=True) when a command hit its deadline.

    Catchable as either CommandExecutionError or sandbox_sdk TimeoutError.
    """


class OwnedByAnotherRunError(SandboxError):
    """Raised when trying to mutate resources tagged by another active run_id."""


# ---------------------------------------------------------------------------
# Infrastructure errors (Runtime Protocol)
#
# Contract: RuntimeGuestHook.exec() returns an ExecResult only when the guest
# started the command and either observed it exit or killed it at its
# deadline. Every other outcome raises an InfrastructureError. Transport and
# HTTP/gRPC statuses are never encoded as exit codes.
# ---------------------------------------------------------------------------


class InfrastructureError(SandboxError):
    """The SDK could not obtain a verdict from the sandbox for this call.

    Never score these as agent failures (e.g. reward 0). Retry the rollout on a
    fresh sandbox when ``retryable`` is True, otherwise mask it. For grouped
    algorithms such as GRPO, drop or resample the whole group so the group
    baseline is not skewed.

    Attributes:
        sandbox_id: Sandbox the call targeted, when known.
        status: Transport status, e.g. ``"HTTP 503"`` or ``"grpc UNAVAILABLE"``.
        retryable: Whether retrying on a fresh sandbox may succeed.
    """

    default_retryable: bool = True

    def __init__(self, message: str, *, sandbox_id: Optional[str] = None,
                 status: Optional[str] = None, retryable: Optional[bool] = None):
        super().__init__(message)
        self.sandbox_id = sandbox_id
        self.status = status
        self.retryable = self.default_retryable if retryable is None else retryable

    def __reduce__(self) -> Any:
        # Keep keyword-only attributes when pickled (e.g. across Ray workers).
        return (_rebuild_infra_error,
                (type(self), str(self), self.sandbox_id, self.status, self.retryable))


def _rebuild_infra_error(cls: type, message: str, sandbox_id: Optional[str],
                         status: Optional[str], retryable: bool) -> "InfrastructureError":
    return cls(message, sandbox_id=sandbox_id, status=status, retryable=retryable)


class SandboxUnavailableError(InfrastructureError):
    """Sandbox or data plane unreachable, gone, overloaded, or failed mid-call.

    Examples: connection refused/reset, router 404/408/429/5xx, client-side
    read timeout, gRPC UNAVAILABLE/UNKNOWN/INTERNAL/ABORTED/RESOURCE_EXHAUSTED.
    """

    default_retryable = True


class SandboxProtocolError(InfrastructureError):
    """Request rejected or response malformed: a contract or configuration bug.

    Examples: HTTP 400/401/403, non-JSON body, missing ``exitCode``, gRPC
    INVALID_ARGUMENT/UNIMPLEMENTED/PERMISSION_DENIED/UNAUTHENTICATED.
    """

    default_retryable = False


class CommandStartError(InfrastructureError):
    """The guest could not start the command (missing executable or cwd).

    This usually means a harness or image misconfiguration (for example the
    default ``cwd="/testbed"`` on a non-SWE-bench image), so it is masked
    rather than scored. Retrying on the same image will fail the same way.
    """

    default_retryable = False
