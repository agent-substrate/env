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

"""NeMo Gym sandbox provider backed by Agent Substrate.

Each sandbox is a Substrate actor fronted by the ``ate-env`` API
(https://github.com/agent-substrate/env), reached through this repo's Python
client, ``ate_env`` (``clients/python``). Environment lifecycle (create, get, delete) goes to
``ateenv.v1alpha.EnvironmentService`` on ``ate-env-api``; command execution and
file transfer go to the ``ProcessService`` / ``FileSystemService`` served by
the ``ate-env-guest`` daemon inside every actor, which ``ate-env-api`` proxies
through the atenet router. One gRPC endpoint carries both. Idle sandboxes can
be suspended by Substrate and are resumed transparently on the next guest
call, so a fleet of mostly-waiting rollout sandboxes holds no workers.

Contract: https://docs.nvidia.com/nemo/gym/main/infrastructure/sandbox/adding-a-provider/
"""

from __future__ import annotations

import asyncio
import contextlib
import logging
import math
import time
import uuid
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Mapping
from urllib.parse import urlparse

import grpc
import grpc.aio
from ate_env import Client, Env, EnvError, EnvironmentStatus, NotFoundError, OutputSource

from ._compat import (
    SandboxCreateError,
    SandboxCreateVerificationError,
    SandboxExecResult,
    SandboxHandle,
    SandboxSpec,
    SandboxStatus,
)

logger = logging.getLogger(__name__)

_PROVIDER_NAME = "substrate"

# Client-side slack on top of exec(timeout_s=...): the guest enforces the
# deadline itself (see exec), so the client only has to outlive it.
_EXEC_GRACE_S = 5.0
# Deadline for one readiness probe. A guest that is still booting may hold a
# call open; bounding it keeps the poll loop honest.
_PROBE_TIMEOUT_S = 10.0

# Env lifecycle status -> Gym status. Parked (suspended / paused) actors report
# RUNNING: the router resumes them on the next guest call, which is what a
# rollout loop wants to see (see README, "Resource mapping").
_STATUS_MAP = {
    EnvironmentStatus.RESUMING: SandboxStatus.STARTING,
    EnvironmentStatus.RUNNING: SandboxStatus.RUNNING,
    EnvironmentStatus.SUSPENDING: SandboxStatus.RUNNING,
    EnvironmentStatus.SUSPENDED: SandboxStatus.RUNNING,
    EnvironmentStatus.PAUSING: SandboxStatus.RUNNING,
    EnvironmentStatus.PAUSED: SandboxStatus.RUNNING,
    EnvironmentStatus.CRASHED: SandboxStatus.ERROR,
    EnvironmentStatus.DELETING: SandboxStatus.STOPPED,
}


def _require_keys(options: Mapping[str, Any], allowed: frozenset[str], where: str) -> None:
    unknown = sorted(set(options) - allowed)
    if unknown:
        raise ValueError(f"{where}: unknown option(s) {unknown}; allowed: {sorted(allowed)}")


def _describe(exc: BaseException) -> str:
    """``CODE: details`` for ate_env errors, so messages name the gRPC status."""
    code = getattr(exc, "code", None)
    if isinstance(code, grpc.StatusCode):
        return f"{code.name}: {exc}"
    if isinstance(exc, EnvError):
        return f"{type(exc).__name__}: {exc}"
    return str(exc) or type(exc).__name__


def _atespace_option(options: Mapping[str, Any], where: str) -> str | None:
    """``atespace`` is the ate-env name; ``namespace`` is accepted as the older alias."""
    if "atespace" in options and "namespace" in options:
        raise ValueError(f"{where}: set either atespace or namespace, not both")
    value = options.get("atespace", options.get("namespace"))
    return None if value is None else str(value)


@dataclass(frozen=True)
class ConnectionConfig:
    """`connection:` block of the provider config."""

    # host:port or an http(s):// URL of ate-env-api. Plain gRPC (h2c) unless https.
    api_url: str = "http://127.0.0.1:7777"
    # Deadline for each lifecycle RPC (create / get / delete).
    request_timeout_s: float = 30.0

    _ALLOWED = frozenset({"api_url", "request_timeout_s"})

    @classmethod
    def from_mapping(cls, options: Mapping[str, Any]) -> "ConnectionConfig":
        _require_keys(options, cls._ALLOWED, "sandbox.substrate.connection")
        return cls(
            api_url=str(options.get("api_url", cls.api_url)).rstrip("/"),
            request_timeout_s=float(options.get("request_timeout_s", cls.request_timeout_s)),
        )


@dataclass(frozen=True)
class CreateConfig:
    """`create:` block of the provider config."""

    # ActorTemplate when the spec names none; ate-env's own default.
    template: str = "default-template"
    # Atespace the templates live in and the environments are created in.
    atespace: str = "ate-env"
    ready_timeout_s: float = 120.0
    ready_poll_interval_s: float = 1.0
    # Optional mapping from SandboxSpec.image to an ActorTemplate name, for
    # workloads that select sandboxes by image reference. Templates must be
    # pre-provisioned on the cluster (see README).
    image_templates: Mapping[str, str] = None  # type: ignore[assignment]

    _ALLOWED = frozenset(
        {
            "template",
            "atespace",
            "namespace",
            "ready_timeout_s",
            "ready_poll_interval_s",
            "image_templates",
        }
    )

    @classmethod
    def from_mapping(cls, options: Mapping[str, Any]) -> "CreateConfig":
        _require_keys(options, cls._ALLOWED, "sandbox.substrate.create")
        return cls(
            template=str(options.get("template", cls.template)),
            atespace=_atespace_option(options, "sandbox.substrate.create") or cls.atespace,
            ready_timeout_s=float(options.get("ready_timeout_s", cls.ready_timeout_s)),
            ready_poll_interval_s=float(
                options.get("ready_poll_interval_s", cls.ready_poll_interval_s)
            ),
            image_templates=dict(options.get("image_templates") or {}),
        )


@dataclass(frozen=True)
class SubstrateProviderOptions:
    """Per-sandbox options carried in ``SandboxSpec.provider_options``."""

    template: str | None = None
    atespace: str | None = None

    _ALLOWED = frozenset({"template", "atespace", "namespace"})

    @classmethod
    def from_mapping(cls, options: Mapping[str, Any]) -> "SubstrateProviderOptions":
        _require_keys(options, cls._ALLOWED, "SandboxSpec.provider_options")
        return cls(
            template=options.get("template"),
            atespace=_atespace_option(options, "SandboxSpec.provider_options"),
        )


class SubstrateSandboxProvider:
    """NeMo Gym sandbox provider running sandboxes as Substrate actors."""

    name = _PROVIDER_NAME

    def __init__(
        self,
        config: Mapping[str, Any] | None = None,
        *,
        client: Client | None = None,
        **kwargs: Any,
    ) -> None:
        """``config`` is the ``sandbox.substrate`` block. Pass ``client`` to reuse a
        caller-owned ``ate_env.Client`` (the provider then never closes it)."""
        config = dict(config or {})
        config.update(kwargs)
        _require_keys(config, frozenset({"connection", "create"}), "sandbox.substrate")
        self._connection = ConnectionConfig.from_mapping(config.get("connection") or {})
        self._create = CreateConfig.from_mapping(config.get("create") or {})
        # The channel is opened lazily so constructing a provider never dials.
        self._client: Client | None = client
        self._owns_client = client is None
        self._channel: grpc.aio.Channel | None = None

    # -- provider contract -------------------------------------------------

    async def create(self, spec: SandboxSpec) -> SandboxHandle:
        opts = SubstrateProviderOptions.from_mapping(spec.provider_options or {})
        template = self._resolve_template(spec, opts)
        atespace = opts.atespace or self._create.atespace
        env_id = f"gym-{uuid.uuid4().hex[:10]}"

        try:
            env = await asyncio.wait_for(
                self._ate().create(
                    env_id,
                    atespace=atespace,
                    template_name=template,
                    template_atespace=atespace,
                ),
                timeout=self._connection.request_timeout_s,
            )
        except EnvError as exc:
            raise SandboxCreateError(
                f"creating substrate env {env_id!r} (template {atespace}/{template}): "
                f"{_describe(exc)}"
            ) from exc
        except asyncio.TimeoutError as exc:
            raise SandboxCreateError(
                f"creating substrate env {env_id!r}: no answer from "
                f"{self._connection.api_url} within {self._connection.request_timeout_s}s"
            ) from exc

        try:
            await self._wait_ready(env, spec.ready_timeout_s or self._create.ready_timeout_s)
            for target_path, content in (spec.files or {}).items():
                await env.write_file(target_path, content.encode(), mode=0o644)
        except Exception:
            await self._best_effort_delete(env_id, atespace)
            raise

        return SandboxHandle(
            sandbox_id=env_id,
            provider_name=self.name,
            raw={
                "env_id": env_id,
                "atespace": atespace,
                "workdir": spec.workdir,
                "env": dict(spec.env or {}),
            },
        )

    async def exec(
        self,
        handle: SandboxHandle,
        command: str,
        *,
        cwd: str | None = None,
        env: dict[str, str] | None = None,
        timeout_s: int | float | None = None,
        user: str | int | None = None,
    ) -> SandboxExecResult:
        if user is not None:
            raise ValueError(
                "the substrate provider does not support per-exec `user`: "
                "the guest daemon runs commands as the actor's configured user"
            )
        raw = handle.raw or {}
        merged_env = {**(raw.get("env") or {}), **(env or {})}
        effective_cwd = cwd or raw.get("workdir") or ""

        argv = ["sh", "-c", command]
        # `is not None` so an explicit 0 means "shortest allowed deadline"
        # (1s guest-side), not "unlimited".
        if timeout_s is not None:
            # Enforce the deadline guest-side, so a runaway process does not
            # outlive the call that started it. Round up to whole seconds with
            # a floor of 1: `timeout 0` disables the limit in coreutils, so a
            # sub-second timeout_s must not floor to zero.
            argv = ["timeout", str(max(1, math.ceil(timeout_s))), *argv]

        sandbox = self._env(handle)
        process_id = await sandbox.start_process(argv, cwd=effective_cwd, env=merged_env)
        try:
            return await asyncio.wait_for(
                self._collect(sandbox, process_id),
                timeout=None if timeout_s is None else timeout_s + _EXEC_GRACE_S,
            )
        except asyncio.TimeoutError:
            with contextlib.suppress(EnvError):
                await sandbox.kill_process(process_id)
            return SandboxExecResult(
                stdout=None,
                stderr=f"substrate provider: exec timed out after {timeout_s}s",
                return_code=-1,
            )

    async def upload_file(self, handle: SandboxHandle, source_path: Path, target_path: str) -> None:
        await self._env(handle).write_file(
            target_path, Path(source_path).read_bytes(), mode=0o644
        )

    async def download_file(self, handle: SandboxHandle, source_path: str, target_path: Path) -> None:
        content = await self._env(handle).read_file_bytes(source_path)
        target = Path(target_path)
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(content)

    async def status(self, handle: SandboxHandle) -> SandboxStatus:
        # GetEnvironment reads the actor's state without touching the guest,
        # so asking does not wake a parked sandbox.
        raw = handle.raw or {}
        try:
            info = await asyncio.wait_for(
                self._ate().get(handle.sandbox_id, atespace=self._atespace_of(raw)),
                timeout=self._connection.request_timeout_s,
            )
        except NotFoundError:
            return SandboxStatus.STOPPED
        except (EnvError, asyncio.TimeoutError):
            return SandboxStatus.UNKNOWN
        return _STATUS_MAP.get(info.status, SandboxStatus.UNKNOWN)

    async def close(self, handle: SandboxHandle) -> None:
        await self._best_effort_delete(handle.sandbox_id, self._atespace_of(handle.raw or {}))

    async def aclose(self) -> None:
        if self._client is not None and self._owns_client:
            await self._client.close()
        if self._channel is not None:
            await self._channel.close()
        self._client = None if self._owns_client else self._client
        self._channel = None

    # -- internals ----------------------------------------------------------

    def _ate(self) -> Client:
        if self._client is None:
            self._client = self._open_client(self._connection.api_url)
        return self._client

    def _open_client(self, api_url: str) -> Client:
        url = urlparse(api_url if "://" in api_url else f"//{api_url}")
        if url.scheme == "https":
            # ate_env.Client only dials h2c itself; hand it a TLS channel.
            target = f"{url.hostname}:{url.port or 443}"
            self._channel = grpc.aio.secure_channel(target, grpc.ssl_channel_credentials())
            return Client(channel=self._channel)
        return Client(api_url)

    def _atespace_of(self, raw: Mapping[str, Any]) -> str:
        return str(raw.get("atespace") or self._create.atespace)

    def _env(self, handle: SandboxHandle) -> Env:
        return self._ate().env(handle.sandbox_id, atespace=self._atespace_of(handle.raw or {}))

    def _resolve_template(self, spec: SandboxSpec, opts: SubstrateProviderOptions) -> str:
        if opts.template:
            return opts.template
        if spec.image:
            mapped = (self._create.image_templates or {}).get(spec.image)
            if mapped:
                return mapped
            raise SandboxCreateError(
                f"no ActorTemplate mapped for image {spec.image!r}: add it to "
                "sandbox.substrate.create.image_templates or set "
                "provider_options.template (templates are pre-provisioned; see README)"
            )
        return self._create.template

    async def _wait_ready(self, env: Env, timeout_s: float) -> None:
        """Poll a trivial command until the guest answers it. A fresh actor may
        still be booting, and a guest call made before it serves fails with a
        transport-level error; both are retried until the deadline."""
        deadline = time.monotonic() + timeout_s
        last_error = "no probe attempted"
        while True:
            try:
                result = await asyncio.wait_for(env.shell("true"), timeout=_PROBE_TIMEOUT_S)
                if result.exit_code == 0:
                    return
                last_error = f"probe exited {result.exit_code}: {result.stderr.strip()[:200]}"
            except EnvError as exc:
                last_error = _describe(exc)
            except asyncio.TimeoutError:
                last_error = f"probe did not answer within {_PROBE_TIMEOUT_S}s"
            if time.monotonic() >= deadline:
                break
            await asyncio.sleep(self._create.ready_poll_interval_s)
        raise SandboxCreateVerificationError(
            f"substrate env {env.id!r} not ready after {timeout_s}s: {last_error}"
        )

    @staticmethod
    async def _collect(env: Env, process_id: str) -> SandboxExecResult:
        stdout = bytearray()
        stderr = bytearray()
        async for chunk in env.stream_outputs(process_id, follow=True):
            if chunk.source == OutputSource.STDOUT:
                stdout.extend(chunk.data)
            elif chunk.source == OutputSource.STDERR:
                stderr.extend(chunk.data)
        # The follow stream can end before the process record's status
        # flips; wait() polls for the final state and exit code.
        proc = await env.wait(process_id)
        return SandboxExecResult(
            stdout=stdout.decode("utf-8", errors="replace"),
            stderr=stderr.decode("utf-8", errors="replace"),
            return_code=proc.exit_code,
        )

    async def _best_effort_delete(self, env_id: str, atespace: str) -> None:
        try:
            await asyncio.wait_for(
                self._ate().delete(env_id, atespace=atespace),
                timeout=self._connection.request_timeout_s,
            )
        except NotFoundError:
            pass  # already gone: close() is idempotent
        except (EnvError, asyncio.TimeoutError) as exc:
            logger.warning("deleting substrate env %s: %s", env_id, _describe(exc))
