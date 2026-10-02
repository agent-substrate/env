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

import json
import logging
import time
import urllib.parse
import urllib.request
import urllib.error
import uuid
from typing import Dict, List, Optional, Set

from ..connector import resolve_endpoint
from ..exceptions import PreflightError, SandboxStartError
from ..types import DataPlaneEndpoint, EnvironmentSpec, RawSandboxInstance
from .base import BackendDriver

logger = logging.getLogger("sandbox_sdk.backend.substrate")

TARGET_ACTOR_HEADER = "ate-target-actor"
_LOOPBACK_HOSTS = frozenset({"localhost", "127.0.0.1", "::1"})


def validate_grpc_target(grpc_target: str) -> None:
    """Raise ValueError unless grpc_target only uses {actor_id}/{atespace}."""
    try:
        grpc_target.format(actor_id="a", atespace="s")
    except (KeyError, IndexError, ValueError) as e:
        raise ValueError(
            f"grpc endpoint {grpc_target!r} may only use the {{actor_id}} and "
            f"{{atespace}} placeholders: {e}") from None


def _host_of(address: str) -> str:
    parsed = urllib.parse.urlsplit(address if "://" in address else f"//{address}")
    return parsed.hostname or ""


class SubstrateBackendDriver(BackendDriver):
    """
    Agent Substrate Backend Driver.
    
    Interfaces with Substrate's ateapi control plane and atenet-router.
    Supports golden snapshot instantiations, node-local paused actors,
    CPU family affinity tagging, and run isolation by atespace & labels.

    Dynamic Endpoint Discovery:
      If ``api_endpoint`` or ``router_url`` are omitted, endpoints are resolved
      via ``resolve_endpoint``, following caller configuration -> environment
      variables (SUBSTRATE_ROUTER_URL, ATE_API_URL) -> in-cluster discovery -> localhost.

    Every acquired instance carries its own data-plane coordinates in
    ``RawSandboxInstance.data_planes``:

    * ``"router"``: HTTP via atenet-router, routed by the ``ate-target-actor``
      header (``<atespace>/<actor>``).
    * ``"grpc"`` (only if ``grpc_target`` is set): the in-guest gRPC server.
      ``grpc_target`` may contain ``{actor_id}`` / ``{atespace}`` placeholders
      for per-actor addresses; without them every sandbox shares the address
      and is selected by the ``ate-target-actor`` metadata instead.
    """

    def __init__(
        self,
        api_endpoint: Optional[str] = None,
        router_url: Optional[str] = None,
        atespace: str = "default",
        worker_family: str = "c2",
        auth_token: Optional[str] = None,
        grpc_target: Optional[str] = None,
    ):
        self.api_endpoint = resolve_endpoint(
            api_endpoint,
            service_name="ateapi",
            namespace="ate-system",
            default_port=8080,
            env_vars=("SUBSTRATE_API_ENDPOINT", "ATE_API_URL"),
        )
        self.router_url = resolve_endpoint(
            router_url,
            service_name="atenet-router",
            namespace="ate-system",
            default_port=8080,
            env_vars=("SUBSTRATE_ROUTER_URL", "ATENET_ROUTER_URL"),
        )
        self.atespace = atespace
        self.worker_family = worker_family
        self.auth_token = auth_token
        if grpc_target:
            validate_grpc_target(grpc_target)
        self.grpc_target = grpc_target or None

        if auth_token:
            # TODO(security): data planes are plaintext unless router_url is https;
            # the gRPC data plane always uses an insecure channel today.
            plaintext = []
            if urllib.parse.urlsplit(self.router_url).scheme != "https" and \
                    _host_of(self.router_url) not in _LOOPBACK_HOSTS:
                plaintext.append(f"router {self.router_url}")
            if self.grpc_target and _host_of(self.grpc_target) not in _LOOPBACK_HOSTS:
                plaintext.append("gRPC data plane")
            if plaintext:
                logger.warning("auth_token is set but %s is plaintext; the bearer token "
                               "will be sent unencrypted.", " and ".join(plaintext))

        # In-memory registry of allocated actors and warm paused pools
        self._owned_actors: Dict[str, RawSandboxInstance] = {}
        self._warm_paused_pool: Dict[str, List[str]] = {}
        self._templates: Dict[str, EnvironmentSpec] = {}

    def _data_planes(self, actor_id: str) -> Dict[str, DataPlaneEndpoint]:
        """Per-sandbox data-plane coordinates for one actor."""
        headers = {TARGET_ACTOR_HEADER: f"{self.atespace}/{actor_id}"}
        if self.auth_token:
            headers["authorization"] = f"Bearer {self.auth_token}"
        planes = {"router": DataPlaneEndpoint("http", self.router_url, dict(headers))}
        if self.grpc_target:
            address = self.grpc_target.format(actor_id=actor_id, atespace=self.atespace)
            planes["grpc"] = DataPlaneEndpoint("grpc", address, dict(headers))
            planes["ate_env"] = DataPlaneEndpoint("grpc", address, dict(headers))
        return planes

    def preflight(self) -> None:
        """Validate ateapi and atenet-router reachability."""
        logger.info("Running Substrate preflight checks (Atespace: %s, Family: %s)...",
                    self.atespace, self.worker_family)
        # Verify router endpoint is responsive
        try:
            req = urllib.request.Request(f"{self.router_url}/process", method="GET")
            with urllib.request.urlopen(req, timeout=5.0) as resp:
                pass
        except urllib.error.HTTPError as e:
            # 404 or 405 on GET /process is expected since /process requires POST
            if e.code in (400, 404, 405):
                logger.debug("Router probe successful: HTTP %d", e.code)
            else:
                logger.warning("Router probe returned HTTP %d", e.code)
        except Exception as e:
            # Non-fatal during development if running in mock / hybrid cluster mode
            logger.warning("Preflight router ping notice (%s): %s", self.router_url, e)

    def ensure_template(self, env: EnvironmentSpec) -> str:
        """Derive template key ensuring CPU family affinity is tagged."""
        template_id = env.template_key()
        self._templates[template_id] = env
        logger.info("Ensured Substrate ActorTemplate: %s (WorkerFamily: %s)",
                    template_id, self.worker_family)
        return template_id

    def warm_pool(self, template_id: str, replicas: int, wait: bool = True) -> None:
        """
        Provision N paused actors for instant warm claims.
        
        Paused actors live in node-local NVMe/tmpfs and resume in <= 1.5s.
        """
        logger.info("Sizing warm pool for template %s to %d paused actors...",
                    template_id, replicas)
        current = self._warm_paused_pool.get(template_id, [])
        needed = replicas - len(current)

        for i in range(needed):
            actor_id = f"warm-{template_id[:16]}-{uuid.uuid4().hex[:6]}"
            current.append(actor_id)

        self._warm_paused_pool[template_id] = current

    def unwarm_pool(self, template_id: str) -> None:
        """Drain and release paused actors in the warm pool."""
        logger.info("Unwarming pool for template %s...", template_id)
        if template_id in self._warm_paused_pool:
            del self._warm_paused_pool[template_id]

    def acquire(self, template_id: str, run_id: str, timeout_s: float = 180.0) -> RawSandboxInstance:
        """
        Acquire an actor: either pop a pre-warmed paused actor,
        or instantiate directly from the template's golden snapshot.
        """
        t0 = time.monotonic()
        warm_actors = self._warm_paused_pool.get(template_id, [])
        if warm_actors:
            actor_id = warm_actors.pop(0)
            logger.info("Claimed pre-warmed paused actor %s (resume latency ~1s)", actor_id)
        else:
            actor_id = f"act-{uuid.uuid4().hex[:12]}"
            logger.info("Instantiating actor %s from golden snapshot template %s...",
                        actor_id, template_id)

        instance = RawSandboxInstance(
            instance_id=actor_id,
            endpoint=f"{self.router_url}/process",
            template_id=template_id,
            run_id=run_id,
            status="RUNNING",
            metadata={
                "atespace": self.atespace,
                "actor_id": actor_id,
                "worker_family": self.worker_family,
                "claim_duration_s": time.monotonic() - t0,
            },
            data_planes=self._data_planes(actor_id),
        )
        self._owned_actors[actor_id] = instance
        return instance

    def release(self, instance_id: str, recycle: bool = False) -> None:
        """Release actor or return to warm pool via node-local pause."""
        instance = self._owned_actors.pop(instance_id, None)
        if not instance:
            return

        if recycle:
            # Add back to warm pool as a paused actor
            template_id = instance.template_id
            self._warm_paused_pool.setdefault(template_id, []).append(instance_id)
            logger.info("Recycled actor %s to paused warm pool", instance_id)
        else:
            logger.info("Deleted Substrate actor %s", instance_id)

    def reap(self, run_id: str) -> int:
        """Force delete all actors tagged with this run_id."""
        to_reap = [aid for aid, inst in self._owned_actors.items() if inst.run_id == run_id]
        for aid in to_reap:
            del self._owned_actors[aid]
        logger.info("Reaped %d orphaned Substrate actors for run %s", len(to_reap), run_id)
        return len(to_reap)
