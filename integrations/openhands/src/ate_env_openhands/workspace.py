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

"""An OpenHands remote workspace on an ate-env environment.

The environment runs an unmodified task image. A base template mounts the
OpenHands agent-server into it as a runtime layer, and the guest starts the
agent-server next to itself (see this package's README and ``layer/``).
ate-env-api's port proxy forwards ``/v1alpha/atespaces/<a>/envs/<id>/ports/<p>/...``
to that port in the environment, so the proxy URL is the workspace host: the
SDK appends its ``/api/...`` paths to it, and ``RemoteConversation`` builds
its event WebSocket from it.

Mirrors ``openhands.workspace.AgentSandboxWorkspace``: same lifecycle order
(create, health wait, parent init; clean up on any failure after create),
``pause()`` / ``resume()``, ``__exit__`` then ``cleanup()``, idempotent
``cleanup()``. The agent-sandbox warm pool maps to ``template``; the task
image, which agent-sandbox fixes in its SandboxTemplate, is a field here.
"""

from __future__ import annotations

import asyncio
import secrets
import shlex
import time
from typing import Any

import httpx
from ate_env import NotFoundError
from openhands.sdk.logger import get_logger
from openhands.sdk.workspace import RemoteWorkspace
from pydantic import Field, PrivateAttr

from . import _runtime

logger = get_logger(__name__)

DEFAULT_ATESPACE = "ate-env"


def _base_url(url: str) -> str:
    url = url.rstrip("/")
    return url if "://" in url else f"http://{url}"


def port_url(api_url: str, atespace: str, env_id: str, port: int) -> str:
    """URL of ``port`` in an environment, through ate-env-api's port proxy."""
    return f"{_base_url(api_url)}/v1alpha/atespaces/{atespace}/envs/{env_id}/ports/{port}"


def _new_env_id() -> str:
    return f"oh-{secrets.token_hex(6)}"


class AteEnvWorkspace(RemoteWorkspace):
    """Remote workspace backed by an ate-env environment.

    Creates an environment from ``template`` (a base template whose runtime
    layer carries the agent-server), with ``image`` as its root filesystem,
    waits until the agent-server answers ``/health`` through ate-env-api, and
    deletes the environment on exit. With ``env_id`` it attaches to an
    existing environment instead and leaves it in place on exit.

    Example:
        with AteEnvWorkspace(
            template="oh-agent-server",
            image="docker.io/library/python@sha256:...",
            api_key=SESSION_KEY,
        ) as workspace:
            result = workspace.execute_command("python3 --version")
    """

    working_dir: str = Field(
        default="/workspace",
        description="Working directory in the environment. Created when missing. "
        "Use /testbed for SWE-bench images.",
    )
    host: str = Field(
        default="",
        description="Agent-server URL through the port proxy. Set by the workspace.",
    )

    # ate-env configuration
    template: str | None = Field(
        default=None,
        description="Base template with the agent-server layer and sidecar. Required "
        "unless env_id names an existing environment. Counterpart of agent-sandbox's "
        "warmpool.",
    )
    template_atespace: str | None = Field(
        default=None,
        description="Atespace of the template. Unset means the server default.",
    )
    image: str | None = Field(
        default=None,
        description="Task image (repo@sha256:...) to run unmodified. Unset means the "
        "template's own image.",
    )
    atespace: str = Field(
        default=DEFAULT_ATESPACE,
        description="Atespace of the environment. Counterpart of agent-sandbox's namespace.",
    )
    api_url: str = Field(
        default="127.0.0.1:7777",
        description="ate-env-api address, host:port or http(s) URL (gRPC and the port "
        "proxy share its listener).",
    )
    proxy_url: str | None = Field(
        default=None,
        description="Base URL of the port proxy when it differs from api_url.",
    )
    api_token: str | None = Field(
        default=None,
        description="Bearer token for ate-env-api's gRPC API. Not sent through the "
        "port proxy, which would forward it to the agent-server.",
    )
    env_id: str | None = Field(
        default=None,
        description="Attach to this existing environment instead of creating one.",
    )
    server_port: int = Field(
        default=8000,
        ge=1,
        le=65535,
        description="Port the agent-server listens on in the environment.",
    )
    create_timeout: float = Field(
        default=600.0,
        gt=0.0,
        description="Seconds to wait for CreateEnvironment (a new image pulls and "
        "snapshots first).",
    )
    health_check_timeout: float = Field(
        default=120.0,
        gt=0.0,
        description="Seconds to wait for the agent-server's /health through the proxy.",
    )
    delete_on_exit: bool | None = Field(
        default=None,
        description="Delete the environment in cleanup(). Unset means: delete it when "
        "this workspace created it, keep it when attached with env_id.",
    )

    _created: bool = PrivateAttr(default=False)
    _deleted: bool = PrivateAttr(default=False)

    def model_post_init(self, context: Any) -> None:
        """Create or attach the environment, wait for the agent-server, initialize."""
        if self.env_id is None and not self.template:
            raise ValueError("template is required to create an environment; "
                             "pass env_id to attach to an existing one")
        if self.env_id is not None and self.image:
            raise ValueError("image applies only when creating; it cannot change "
                             "on an existing environment")
        if self.image and "@sha256:" not in self.image:
            raise ValueError(f"image {self.image!r} must be pinned by digest "
                             "(repo@sha256:...); resolve a tag with "
                             "`crane digest --platform linux/amd64 <ref>`")

        if self.env_id is None:
            self._create()
        else:
            _runtime.run(self._ate().get(self.env_id, atespace=self.atespace))
            logger.info("Attached to environment %s/%s", self.atespace, self.env_id)

        # Everything past the create must clean up on failure: the caller gets
        # no object to close when the constructor raises.
        try:
            object.__setattr__(self, "host", self.port_url(self.server_port))
            self._wait_for_health(timeout=self.health_check_timeout)
            super().model_post_init(context)
            self._check_session_key()
            self._ensure_working_dir()
            self._check_versions()
            logger.info("ate-env workspace is ready at %s", self.host)
        except Exception:
            self.cleanup()
            raise

    def _ate(self):
        return _runtime.client(self.api_url, self.api_token)

    def _create(self) -> None:
        env_id = _new_env_id()
        object.__setattr__(self, "env_id", env_id)
        logger.info("Creating environment %s/%s from template %r%s",
                    self.atespace, env_id, self.template,
                    f" with image {self.image}" if self.image else "")
        try:
            _runtime.run(asyncio.wait_for(
                self._ate().create(
                    env_id,
                    atespace=self.atespace,
                    template_name=self.template,
                    template_atespace=self.template_atespace,
                    rootfs_image=self.image,
                ),
                self.create_timeout,
            ))
        except BaseException:
            # The server can finish a create whose call failed or timed out on
            # this side; delete by name so it cannot leak.
            self._created = True
            self.cleanup()
            raise
        self._created = True

    def port_url(self, port: int) -> str:
        """URL of another port in this environment, through the port proxy."""
        if self.env_id is None:
            raise RuntimeError("no environment")
        return port_url(self.proxy_url or self.api_url, self.atespace, self.env_id, port)

    def _wait_for_health(self, *, timeout: float) -> None:
        """Poll the agent-server's /health through the proxy until it answers.

        The first request also wakes a suspended environment. A router 404 or
        503 and a refused connection are retried; the last answer is in the
        error when the deadline passes.
        """
        deadline = time.monotonic() + timeout
        url = f"{self.host}/health"
        headers = {"X-Session-API-Key": self.api_key} if self.api_key else {}
        last = "no attempt"
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                break
            try:
                resp = httpx.get(url, headers=headers, timeout=min(30.0, max(remaining, 1.0)))
                if 200 <= resp.status_code < 300:
                    return
                last = f"HTTP {resp.status_code}: {resp.text.strip()[:200]}"
            except httpx.HTTPError as exc:
                last = f"{type(exc).__name__}: {exc}"
            time.sleep(min(1.0, max(deadline - time.monotonic(), 0)))
        raise RuntimeError(f"agent-server in {self.atespace}/{self.env_id} did not answer "
                           f"/health within {timeout}s; last: {last}")

    def _check_session_key(self) -> None:
        """Fail at construction when the agent-server rejects api_key.

        /health answers without a key; the command API does not. The SDK turns
        a rejected command into a failed result, not an error, thus check once
        here.
        """
        resp = self.client.get(f"{self.api_prefix}/bash/bash_events/search", params={"limit": 1})
        if resp.status_code in (401, 403):
            raise PermissionError(
                f"agent-server in {self.atespace}/{self.env_id} rejected the session key "
                f"(HTTP {resp.status_code}); api_key must match the template's "
                "OH_SESSION_API_KEYS_0")
        resp.raise_for_status()

    def _ensure_working_dir(self) -> None:
        result = self.execute_command(f"mkdir -p {shlex.quote(self.working_dir)}", timeout=30)
        if result.exit_code != 0:
            logger.warning("Could not create working_dir %s: %s", self.working_dir,
                           (result.stderr or result.stdout).strip()[:200])

    def _check_versions(self) -> None:
        """Warn when the SDK and the agent-server in the layer differ in version."""
        try:
            from importlib.metadata import version

            local = version("openhands-sdk")
            remote = str(self.get_server_info().get("version", ""))
        except Exception as exc:  # informational only
            logger.debug("version check skipped: %s", exc)
            return
        if remote and remote != local:
            logger.warning("openhands-sdk %s talks to agent-server %s; keep both at one "
                           "version (see the layer's requirements.in)", local, remote)

    def pause(self) -> None:
        """Suspend the environment. Its filesystem is kept in the snapshot."""
        if self.env_id is None or self._deleted:
            raise RuntimeError("Cannot pause: no active environment")
        logger.info("Suspending environment %s/%s", self.atespace, self.env_id)
        # Pooled connections tunnel to the actor that is going away.
        self.reset_client()
        _runtime.run(self._ate().suspend(self.env_id, atespace=self.atespace))

    def resume(self) -> None:
        """Wake the environment and wait for the agent-server.

        Any request wakes a suspended environment; this one makes the wait
        explicit. The host does not change.
        """
        if self.env_id is None or self._deleted:
            raise RuntimeError("Cannot resume: no active environment")
        logger.info("Resuming environment %s/%s", self.atespace, self.env_id)
        self._wait_for_health(timeout=self.health_check_timeout)
        self.reset_client()

    def __enter__(self) -> AteEnvWorkspace:
        return self

    def __exit__(self, exc_type, exc_val, exc_tb) -> None:  # type: ignore[no-untyped-def]
        """Send completion callbacks, then clean up."""
        try:
            super().__exit__(exc_type, exc_val, exc_tb)
        finally:
            self.cleanup()

    def __del__(self) -> None:
        try:
            if getattr(self, "__pydantic_private__", None) is not None:
                self.cleanup()
        except Exception:
            pass  # never raise from __del__

    def cleanup(self) -> None:
        """Drop HTTP state and delete the environment when owned (idempotent)."""
        try:
            self.reset_client()
        except Exception:
            pass
        delete = self.delete_on_exit if self.delete_on_exit is not None else self._created
        if not delete or self._deleted or self.env_id is None:
            return
        self._deleted = True
        try:
            logger.info("Deleting environment %s/%s", self.atespace, self.env_id)
            _runtime.run(self._ate().delete(self.env_id, atespace=self.atespace))
        except NotFoundError:
            pass
        except Exception as exc:
            logger.warning("Error deleting environment %s/%s: %s", self.atespace, self.env_id, exc)
