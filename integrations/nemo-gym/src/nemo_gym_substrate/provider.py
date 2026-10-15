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
import time
import uuid
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Mapping
from urllib.parse import urlparse

import grpc
import grpc.aio
from ate_env import Client, Env, EnvError, EnvironmentStatus, NotFoundError, Process

from ._compat import (
    SandboxCreateError,
    SandboxCreateVerificationError,
    SandboxEndpoint,
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
# Smallest guest deadline exec() asks for. The guest takes any positive
# duration, but zero selects its default (an hour) and a sub-second kill
# window is noise next to the RPC round trip.
_MIN_GUEST_TIMEOUT_S = 1.0
# Exit code the guest reports for a process group it killed with SIGKILL.
_SIGKILL_EXIT = 137

# Routing headers of ate-env-api's port proxy (internal/portproxy): a plain
# HTTP request carrying them is forwarded to that port inside the environment.
_ENV_ID_HEADER = "X-Env-Id"
_ENV_ATESPACE_HEADER = "X-Env-Atespace"
_ENV_PORT_HEADER = "X-Env-Port"

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
    # Base URL that endpoint() hands out for services inside a sandbox. Unset
    # means api_url as an http(s) URL: ate-env-api serves its port proxy on the
    # same listener. Set it when the code that calls the service reaches
    # ate-env-api at another address than the provider does.
    endpoint_url: str | None = None

    _ALLOWED = frozenset({"api_url", "request_timeout_s", "endpoint_url"})

    @classmethod
    def from_mapping(cls, options: Mapping[str, Any]) -> "ConnectionConfig":
        _require_keys(options, cls._ALLOWED, "sandbox.substrate.connection")
        endpoint_url = options.get("endpoint_url")
        return cls(
            api_url=str(options.get("api_url", cls.api_url)).rstrip("/"),
            request_timeout_s=float(options.get("request_timeout_s", cls.request_timeout_s)),
            endpoint_url=None if endpoint_url is None else str(endpoint_url).rstrip("/"),
        )

    def endpoint_base(self) -> str:
        if self.endpoint_url:
            return self.endpoint_url
        return self.api_url if "://" in self.api_url else f"http://{self.api_url}"


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
        template, rootfs_image = self._resolve_template(spec, opts)
        atespace = opts.atespace or self._create.atespace
        env_id = f"gym-{uuid.uuid4().hex[:10]}"

        try:
            env = await asyncio.wait_for(
                self._ate().create(
                    env_id,
                    atespace=atespace,
                    template_name=template,
                    template_atespace=atespace,
                    rootfs_image=rootfs_image,
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
                # Ports a service inside the sandbox listens on (nemo-gym
                # >= 0.7); endpoint() resolves only these.
                "ports": [int(p) for p in (getattr(spec, "ports", None) or ())],
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
        # The guest enforces the deadline itself and kills the process group
        # with SIGKILL when it elapses, so a runaway process never outlives the
        # call that started it and the image needs no `timeout` binary. `is not
        # None` so an explicit 0 means "shortest deadline", not "unlimited".
        guest_timeout = (
            None if timeout_s is None else max(_MIN_GUEST_TIMEOUT_S, float(timeout_s))
        )

        sandbox = self._env(handle)
        started = time.monotonic()
        process = await sandbox.start_process(
            argv, cwd=effective_cwd, env=merged_env, timeout=guest_timeout
        )
        try:
            result = await asyncio.wait_for(
                self._collect(process),
                timeout=None if timeout_s is None else timeout_s + _EXEC_GRACE_S,
            )
        except asyncio.TimeoutError:
            with contextlib.suppress(EnvError):
                await process.kill()
            return SandboxExecResult(
                stdout=None,
                stderr=f"substrate provider: exec timed out after {timeout_s}s",
                return_code=-1,
            )
        if (
            guest_timeout is not None
            and result.return_code == _SIGKILL_EXIT
            and time.monotonic() - started >= guest_timeout
        ):
            # The guest reports a deadline kill as a plain SIGKILL and nothing
            # in the exit state says why; the elapsed time does. Name it: 137
            # with an empty stderr tells a rollout nothing.
            note = f"substrate provider: exec timed out after {timeout_s}s (killed by the guest)"
            return SandboxExecResult(
                stdout=result.stdout,
                stderr=f"{result.stderr}\n{note}" if result.stderr else note,
                return_code=result.return_code,
            )
        return result

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

    async def endpoint(self, handle: SandboxHandle, port: int) -> SandboxEndpoint:
        """Resolve a declared service port to a caller-reachable endpoint
        (nemo-gym's ``SupportsSandboxEndpoint``).

        The service is a daemon inside the sandbox, typically one a runtime
        layer on the base template mounts into the task image and the guest
        starts next to itself (see README). The endpoint is ate-env-api's port
        proxy; its headers route each plain HTTP request through the Substrate
        router to ``port`` in this sandbox. The sandbox is woken first, so the
        endpoint answers at once even after Substrate parked it.
        """
        raw = handle.raw or {}
        declared = list(raw.get("ports") or ())
        if port not in declared:
            raise ValueError(
                f"Sandbox port {port} was not declared in SandboxSpec.ports; "
                f"declared ports: {declared!r}"
            )
        env = self._env(handle)
        await self._wait_ready(env, self._create.ready_timeout_s, error=RuntimeError)
        return SandboxEndpoint(
            endpoint=self._connection.endpoint_base(),
            headers={
                _ENV_ID_HEADER: handle.sandbox_id,
                _ENV_ATESPACE_HEADER: self._atespace_of(raw),
                _ENV_PORT_HEADER: str(port),
            },
        )

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

    def _resolve_template(
        self, spec: SandboxSpec, opts: SubstrateProviderOptions
    ) -> tuple[str, str | None]:
        """Return ``(template, rootfs_image)`` for a spec.

        An explicit provider option or an ``image_templates`` entry names a
        registered template. Any other image goes to ate-env-api as
        ``rootfs_image``: the API derives a template from the base on first
        use, with the guest mounted into the image as a read-only volume, and
        reuses it afterwards. The image must be digest-pinned; the API rejects
        tags.
        """
        if opts.template:
            return opts.template, None
        if spec.image:
            mapped = (self._create.image_templates or {}).get(spec.image)
            if mapped:
                return mapped, None
            return self._create.template, spec.image
        return self._create.template, None

    async def _wait_ready(
        self,
        env: Env,
        timeout_s: float,
        *,
        error: type[Exception] = SandboxCreateVerificationError,
    ) -> None:
        """Poll a trivial command until the guest answers it. A fresh actor may
        still be booting, and a guest call made before it serves fails with a
        transport-level error; both are retried until the deadline, then
        ``error`` is raised. The first call also resumes a parked actor."""
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
        raise error(f"substrate env {env.id!r} not ready after {timeout_s}s: {last_error}")

    @staticmethod
    async def _collect(process: Process) -> SandboxExecResult:
        stdout = bytearray()
        stderr = bytearray()
        final = None
        async for chunk in process.output(follow=True):
            if chunk.stdout is not None:
                stdout.extend(chunk.stdout)
            elif chunk.stderr is not None:
                stderr.extend(chunk.stderr)
            elif chunk.exit is not None:
                final = chunk.exit
        if final is None:
            # The stream can end before the exit message; wait() polls for it.
            final = await process.wait()
        return SandboxExecResult(
            stdout=stdout.decode("utf-8", errors="replace"),
            stderr=stderr.decode("utf-8", errors="replace"),
            return_code=final.exit_code,
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
