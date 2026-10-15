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

"""NeMo Gym base types, with hermetic fallbacks.

When ``nemo_gym`` is installed (any real deployment), its types are used so
handles and results are the genuine articles. The fallback dataclasses below
are structural mirrors of ``nemo_gym.sandbox.providers.base`` that let this
demo's contract tests run inside the substrate repo without the nemo-gym
dependency. They must be kept shape-identical to the upstream types.
"""

from __future__ import annotations

try:  # pragma: no cover - exercised only when nemo_gym is installed
    from nemo_gym.sandbox.providers.base import (  # type: ignore
        SandboxCreateError,
        SandboxCreateVerificationError,
        SandboxExecResult,
        SandboxHandle,
        SandboxSpec,
        SandboxStatus,
    )

    NEMO_GYM_AVAILABLE = True
except ImportError:  # pragma: no cover - the in-repo test path
    from dataclasses import dataclass, field
    from enum import Enum
    from typing import Any

    NEMO_GYM_AVAILABLE = False

    class SandboxStatus(str, Enum):
        STARTING = "starting"
        RUNNING = "running"
        STOPPED = "stopped"
        ERROR = "error"
        UNKNOWN = "unknown"

    @dataclass(frozen=True)
    class SandboxHandle:
        sandbox_id: str
        provider_name: str
        raw: Any

    @dataclass(frozen=True)
    class SandboxExecResult:
        stdout: str | None
        stderr: str | None
        return_code: int
        error_type: str | None = None

    @dataclass(frozen=True)
    class SandboxSpec:
        image: str | None = None
        ttl_s: int | float | None = None
        ready_timeout_s: int | float | None = None
        workdir: str | None = None
        env: dict[str, str] = field(default_factory=dict)
        files: dict[str, str] = field(default_factory=dict)
        metadata: dict[str, str] = field(default_factory=dict)
        resources: dict[str, Any] = field(default_factory=dict)
        entrypoint: list[str] | None = None
        provider_options: dict[str, Any] = field(default_factory=dict)
        ports: tuple[int, ...] = ()

    class SandboxCreateError(RuntimeError):
        pass

    class SandboxCreateVerificationError(SandboxCreateError):
        pass


# SandboxEndpoint is newer than the rest (nemo-gym 0.7): an installed 0.5
# has the base module without it, so it falls back on its own.
try:  # pragma: no cover - exercised only with nemo-gym >= 0.7
    from nemo_gym.sandbox.providers.base import SandboxEndpoint  # type: ignore
except ImportError:  # pragma: no cover - the in-repo test path and nemo-gym 0.5
    from collections.abc import Mapping as _Mapping
    from dataclasses import dataclass as _dataclass
    from dataclasses import field as _field
    from urllib.parse import urlsplit as _urlsplit

    @_dataclass(frozen=True)
    class SandboxEndpoint:  # type: ignore[no-redef]
        endpoint: str
        headers: dict[str, str] = _field(default_factory=dict)

        def __post_init__(self) -> None:
            if not isinstance(self.endpoint, str) or not self.endpoint.strip():
                raise ValueError("Sandbox endpoint must be a non-empty absolute URL")
            endpoint = self.endpoint.strip()
            parsed = _urlsplit(endpoint)
            if not parsed.scheme or not parsed.netloc:
                raise ValueError("Sandbox endpoint must be a non-empty absolute URL")
            if not isinstance(self.headers, _Mapping):
                raise TypeError("Sandbox endpoint headers must be a mapping")
            object.__setattr__(self, "endpoint", endpoint)
            object.__setattr__(
                self, "headers", {str(k): str(v) for k, v in self.headers.items()}
            )


__all__ = [
    "NEMO_GYM_AVAILABLE",
    "SandboxCreateError",
    "SandboxCreateVerificationError",
    "SandboxEndpoint",
    "SandboxExecResult",
    "SandboxHandle",
    "SandboxSpec",
    "SandboxStatus",
]
