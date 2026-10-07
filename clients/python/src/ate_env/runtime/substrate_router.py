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

import builtins
import http.client
import json
import logging
import time
from typing import Any, Dict, Iterator, List, Optional
import urllib.request
import urllib.error

from ..exceptions import (
    CommandStartError,
    InfrastructureError,
    SandboxProtocolError,
    SandboxUnavailableError,
)
from ..types import ExecResult
from .base import (
    InteractiveSession,
    RuntimeGuestHook,
    command_to_argv,
    read_file_via_exec,
    write_file_via_exec,
)

logger = logging.getLogger("sandbox_sdk.runtime.substrate_router")

TARGET_ACTOR_HEADER = "ate-target-actor"

# Extra client-side wait beyond the guest deadline before declaring the
# router/guest unresponsive (the guest kills the command at timeout_s).
_CLIENT_GRACE_S = 15.0
# Router statuses that mean "this sandbox is gone/overloaded right now";
# a fresh sandbox may succeed (atenet-router: 404 actor not found, 503
# unavailable / no free workers, 504 resume timeout).
_RETRYABLE_HTTP = frozenset({404, 408, 429, 500, 502, 503, 504})
_MAX_ERROR_BODY = 512


def _go_duration(timeout_s: float) -> str:
    """Encode seconds as a Go duration string the guest can parse (>= 1ms)."""
    return f"{max(1, round(timeout_s * 1000))}ms"


class SubstrateRouterSession(InteractiveSession):
    """Stateless /process router commands wrapped as an interactive session."""

    def __init__(self, hook: "SubstrateRouterRuntime"):
        self.hook = hook
        self._open = True

    def run(self, command: str | List[str], timeout_s: Optional[float] = None) -> str:
        if not self._open:
            raise RuntimeError("Session closed")
        res = self.hook.exec(command, timeout_s=timeout_s or 120.0)
        return res.stdout

    def close(self) -> None:
        self._open = False


class SubstrateRouterRuntime(RuntimeGuestHook):
    """
    Substrate atenet-router guest hook.
    
    Dispatches execution commands to the actor instance via atenet-router's
    reverse proxy /process endpoint with the 'ate-target-actor' header.

    Construct either with ``atespace`` + ``actor_id`` or with the per-sandbox
    ``headers`` from a ``DataPlaneEndpoint`` (which already carry the
    ``ate-target-actor`` routing header and any ``authorization`` header).
    """

    def __init__(self, router_url: str, atespace: Optional[str] = None,
                 actor_id: Optional[str] = None, *,
                 headers: Optional[Dict[str, str]] = None,
                 auth_token: Optional[str] = None):
        self.router_url = router_url.rstrip("/")
        hdrs = {k.lower(): v for k, v in (headers or {}).items()}
        if atespace and actor_id:
            hdrs[TARGET_ACTOR_HEADER] = f"{atespace}/{actor_id}"
        if auth_token:
            hdrs["authorization"] = f"Bearer {auth_token}"
        target = hdrs.get(TARGET_ACTOR_HEADER)
        if not target or "/" not in target:
            raise ValueError(
                "SubstrateRouterRuntime needs a target actor: pass atespace and actor_id, "
                "or headers={'ate-target-actor': '<atespace>/<actor>'}")
        self.target_header = target
        self.atespace, _, self.actor_id = target.partition("/")
        self._headers = hdrs

    def exec(self, command: str | List[str], cwd: str = "/testbed",
             env: Optional[Dict[str, str]] = None, timeout_s: float = 120.0) -> ExecResult:
        """Execute command inside the Substrate actor via /process.

        Raises:
            SandboxUnavailableError: router/actor unreachable or overloaded.
            SandboxProtocolError: request rejected or response malformed.
            CommandStartError: guest could not start the command.
        """
        if timeout_s <= 0:
            raise ValueError(f"timeout_s must be > 0, got {timeout_s}")
        url = f"{self.router_url}/process"

        payload: Dict[str, Any] = {
            "command": command_to_argv(command),
            "timeout": _go_duration(timeout_s),
        }
        if cwd:
            payload["cwd"] = cwd
        if env:
            payload["envvars"] = env

        data_bytes = json.dumps(payload).encode("utf-8")
        req = urllib.request.Request(
            url,
            data=data_bytes,
            headers={
                "Content-Type": "application/json",
                "User-Agent": "SandboxSDK-SubstrateRouter/1.0",
                **self._headers,
            },
            method="POST"
        )

        t0 = time.monotonic()
        try:
            with urllib.request.urlopen(req, timeout=float(timeout_s) + _CLIENT_GRACE_S) as resp:
                raw = resp.read()
        except urllib.error.HTTPError as e:
            raise self._http_error(e) from e
        except (urllib.error.URLError, http.client.HTTPException, OSError) as e:
            reason = getattr(e, "reason", e)
            timed_out = isinstance(reason, builtins.TimeoutError)
            raise SandboxUnavailableError(
                f"router transport error for sandbox {self.target_header}: {reason}",
                sandbox_id=self.actor_id,
                status="client timeout" if timed_out else "transport",
            ) from e
        duration = time.monotonic() - t0
        return self._parse_response(raw, duration, timeout_s)

    def _http_error(self, e: urllib.error.HTTPError) -> InfrastructureError:
        try:
            body = e.read(_MAX_ERROR_BODY).decode("utf-8", errors="replace").strip()
        except Exception:  # pragma: no cover - best effort diagnostics only
            body = ""
        msg = f"HTTP {e.code} from atenet-router for sandbox {self.target_header}: {body}"
        cls = SandboxUnavailableError if e.code in _RETRYABLE_HTTP else SandboxProtocolError
        return cls(msg, sandbox_id=self.actor_id, status=f"HTTP {e.code}")

    def _parse_response(self, raw: bytes, duration: float, timeout_s: float) -> ExecResult:
        def malformed(why: str) -> SandboxProtocolError:
            return SandboxProtocolError(
                f"malformed /process response from sandbox {self.target_header}: {why}",
                sandbox_id=self.actor_id, status="HTTP 200")

        try:
            body = json.loads(raw.decode("utf-8"))
        except (UnicodeDecodeError, ValueError) as e:
            raise malformed("body is not JSON") from e
        if not isinstance(body, dict):
            raise malformed("body is not a JSON object")
        exit_code = body.get("exitCode")
        # A missing exitCode must never default to 0 (that would score as success).
        if not isinstance(exit_code, int) or isinstance(exit_code, bool):
            raise malformed(f"missing or non-integer exitCode ({exit_code!r})")
        stdout = body.get("stdout") or ""
        stderr = body.get("stderr") or ""
        guest_error = body.get("error") or ""

        if exit_code == -1 and guest_error:
            # The guest uses exec.CommandContext: a deadline kill reports
            # "signal: killed" (or "context deadline exceeded" if it raced
            # process start). The client-side duration always contains the
            # guest-side one, so duration >= timeout_s separates a deadline
            # kill from an earlier kill (e.g. OOM).
            deadline_kill = ("context deadline exceeded" in guest_error
                             or guest_error.startswith("signal: killed"))
            if deadline_kill and duration >= timeout_s:
                return ExecResult(exit_code=None, stdout=stdout, stderr=stderr,
                                  duration_s=duration, timed_out=True)
            if guest_error.startswith("signal: "):
                sep = "" if not stderr or stderr.endswith("\n") else "\n"
                return ExecResult(exit_code=-1, stdout=stdout,
                                  stderr=f"{stderr}{sep}[guest] {guest_error}",
                                  duration_s=duration)
            raise CommandStartError(
                f"guest could not start command in sandbox {self.target_header}: {guest_error}",
                sandbox_id=self.actor_id, status="start failed")

        return ExecResult(exit_code=exit_code, stdout=stdout, stderr=stderr, duration_s=duration)

    def stream_process(self, argv: List[str], cwd: str = "/testbed",
                       env: Optional[Dict[str, str]] = None) -> Iterator[str]:
        # For HTTP router, process runs to completion and streams output
        res = self.exec(argv, cwd=cwd, env=env)
        if res.stdout:
            yield res.stdout
        if res.stderr:
            yield res.stderr

    def write_file(self, path: str, content: bytes | str) -> None:
        """Write file into the actor filesystem via base64 pipeline."""
        write_file_via_exec(self, path, content)

    def read_file_bytes(self, path: str) -> bytes:
        """Read file from the actor filesystem via base64 pipeline."""
        return read_file_via_exec(self, path)

    def open_session(self) -> InteractiveSession:
        return SubstrateRouterSession(self)
