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

import logging
from typing import Any, Dict, List, Optional

from ..config import FleetConfig
from ..exceptions import InfrastructureError
from ..fleet import SandboxFleet
from ..handle import SandboxHandle
from ..types import Task

logger = logging.getLogger("sandbox_sdk.providers.nemo_gym")

# NeMo Gym's sentinel return code for runtime failures (transport errors,
# timeouts). It is always paired with a non-null ``error_type``.
SANDBOX_RUNTIME_RETURN_CODE = 125

# NeMo Gym / ate-env spellings accepted for FleetConfig fields. If several
# spellings of one field are given, the first non-empty one listed wins.
_KEY_ALIASES = {
    "endpoint": ("api_url", "endpoint"),
    "router_url": ("router_url", "atenet_url"),
    "tenancy": ("atespace", "namespace", "tenancy"),
}


def fleet_config_from_provider_config(cfg: Dict[str, Any]) -> FleetConfig:
    """Map a provider config block to FleetConfig.

    Aliases in ``_KEY_ALIASES`` are resolved; every other key must be a
    FleetConfig field (e.g. ``data_plane``, ``grpc_endpoint``, ``auth_token``).
    Unknown keys raise a validation error instead of being silently dropped.
    ``router_url`` defaults to the resolved ``endpoint``.
    """
    data = dict(cfg)
    resolved: Dict[str, Any] = {}
    for field_name, names in _KEY_ALIASES.items():
        values = [data.pop(n) for n in names if n in data]
        chosen = next((v for v in values if v), None)
        if chosen is not None:
            resolved[field_name] = chosen
    resolved.setdefault("router_url",
                        resolved.get("endpoint", FleetConfig.model_fields["endpoint"].default))
    return FleetConfig.model_validate({**data, **resolved})


class UnifiedSandboxProvider:
    """
    NeMo Gym Sandbox Provider powered by the Unified Sandbox Abstraction SDK.
    
    This replaces and generalizes the single-purpose provider in
    https://github.com/agent-substrate/env/pull/69:
    - Claims sandboxes through SandboxFleet (warm pool or golden restore).
    - Supports multiple backends (Substrate golden restore or Kubernetes).
    - Supports pipelined sliding windows for batch RL rollouts.

    Status: prototype. It is synchronous and takes a single config dict, so
    it does not yet implement NeMo Gym's async provider interface (keyword
    config blocks, async create/exec/upload_file/download_file/status/close).
    ``exec`` already returns NeMo Gym's SandboxExecResult fields.
    """

    def __init__(self, config: Optional[Dict[str, Any]] = None):
        self.fleet_config = fleet_config_from_provider_config(config or {})
        self.fleet = SandboxFleet(self.fleet_config)
        self.fleet.preflight()

    def create(self, spec: Any) -> SandboxHandle:
        """
        Create/acquire a sandbox matching spec.
        
        `spec` can be a NeMo Gym spec object with `.id`, `.image`, `.metadata`,
        or a dictionary. If uploading ``files`` fails, the sandbox is released
        before the error is re-raised.
        """
        task_id = getattr(spec, "id", None) or (spec.get("id") if isinstance(spec, dict) else "nemo-task")
        image = getattr(spec, "image", None) or (spec.get("image") if isinstance(spec, dict) else "default")
        metadata = getattr(spec, "metadata", None) or (spec.get("metadata") if isinstance(spec, dict) else {})

        task = Task(id=str(task_id), image=str(image), metadata=metadata)
        handle = self.fleet.acquire(task)

        # Upload initial files if specified in spec
        files = getattr(spec, "files", None) or (spec.get("files") if isinstance(spec, dict) else None)
        if files and isinstance(files, dict):
            try:
                for path, content in files.items():
                    handle.runtime.write_file(path, content)
            except Exception:
                # Never leak a claimed sandbox when staging fails.
                try:
                    self.fleet.release(handle)
                except Exception:  # pragma: no cover - best effort cleanup
                    logger.warning("Failed to release sandbox %s after file staging error",
                                   handle.sandbox_id, exc_info=True)
                raise

        return handle

    def exec(self, handle: SandboxHandle, cmd: str | List[str], cwd: Optional[str] = None,
             env: Optional[Dict[str, str]] = None, timeout_s: float = 180.0) -> Dict[str, Any]:
        """Run a command; return NeMo Gym SandboxExecResult fields plus ``duration_s``.

        Like other NeMo Gym providers, this never raises for command or
        runtime failures. ``error_type`` tells them apart:

        * ``None``: the command ran and ``return_code`` is its exit code.
        * ``"timeout"``: the command was killed at ``timeout_s``. Output may
          be partial.
        * ``"sandbox"``: infrastructure failure (an ``InfrastructureError``).
          Retry or mask the rollout; never score it as an agent failure.

        ``return_code`` is the sentinel 125 whenever ``error_type`` is set.
        """
        try:
            res = handle.runtime.exec(cmd, cwd=cwd or "/testbed", env=env, timeout_s=timeout_s)
        except InfrastructureError as e:
            return {
                "stdout": None,
                "stderr": f"{type(e).__name__}: {e}",
                "return_code": SANDBOX_RUNTIME_RETURN_CODE,
                "error_type": "sandbox",
                "duration_s": 0.0,
            }
        if res.timed_out:
            return {
                "stdout": res.stdout,
                "stderr": res.stderr,
                "return_code": SANDBOX_RUNTIME_RETURN_CODE,
                "error_type": "timeout",
                "duration_s": res.duration_s,
            }
        return {
            "stdout": res.stdout,
            "stderr": res.stderr,
            "return_code": res.exit_code,
            "error_type": None,
            "duration_s": res.duration_s,
        }

    def upload_file(self, handle: SandboxHandle, path: str, content: bytes | str) -> None:
        """Upload file content into the sandbox."""
        handle.runtime.write_file(path, content)

    def download_file(self, handle: SandboxHandle, path: str) -> bytes:
        """Download binary file content from the sandbox."""
        return handle.runtime.read_file_bytes(path)

    def status(self, handle: SandboxHandle) -> str:
        """Return actor lifecycle status.

        Placeholder: always ``"RUNNING"`` until backends expose a status API.
        """
        return "RUNNING"

    def close(self, handle: SandboxHandle) -> None:
        """Release the claimed sandbox back to the fleet."""
        self.fleet.release(handle)
