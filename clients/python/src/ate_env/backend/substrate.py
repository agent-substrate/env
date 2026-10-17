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

from __future__ import annotations

import asyncio
import logging
import os
import threading
import time
import uuid
from collections.abc import Mapping
from typing import Any

from ..client import DEFAULT_ATESPACE, Client
from ..errors import (
    FailedPreconditionError,
    InfrastructureError,
    NotFoundError,
    PreflightError,
    SandboxStartError,
)
from ..loop_runner import LoopRunner
from ..types import DataPlaneEndpoint, EnvironmentSpec, RawSandboxInstance
from .base import BackendDriver

logger = logging.getLogger("ate_env.backend.substrate")


class _WarmPoolDict(dict):
    """Warm paused pool dictionary keyed by (template, image).

    Allows string keys for backwards compatibility, treating 'template' as (template, None).
    """

    def _normalize(self, key: Any) -> Any:
        if isinstance(key, str):
            return (key, None)
        return key

    def __getitem__(self, key: Any) -> Any:
        return super().__getitem__(self._normalize(key))

    def __setitem__(self, key: Any, value: Any) -> None:
        super().__setitem__(self._normalize(key), value)

    def __contains__(self, key: Any) -> bool:
        return super().__contains__(self._normalize(key))

    def get(self, key: Any, default: Any = None) -> Any:
        return super().get(self._normalize(key), default)

    def pop(self, key: Any, *args: Any) -> Any:
        return super().pop(self._normalize(key), *args)

    def setdefault(self, key: Any, default: Any = None) -> Any:
        return super().setdefault(self._normalize(key), default)


class SubstrateBackendDriver(BackendDriver):
    """Agent Substrate Backend Driver.

    Manages environment lifecycle directly against the ate-env-api gRPC service
    via ate_env.Client: creating environments, suspending for pre-warmed pools,
    and deleting on release.
    """

    def __init__(
        self,
        api_endpoint: str | None = None,
        atespace: str = DEFAULT_ATESPACE,
        worker_family: str = "c2",
        auth_token: str | None = None,
        *,
        client: Client | None = None,
        image_templates: Mapping[str, str] | None = None,
        default_template: str | None = None,
        labels: Mapping[str, str] | None = None,
        acquire_timeout_s: float = 180.0,
        ready_timeout_s: float = 180.0,
        loop_runner: LoopRunner | None = None,
    ):
        target = (
            api_endpoint
            or os.environ.get("ATE_ENV_API_TARGET")
            or os.environ.get("SUBSTRATE_API_ENDPOINT")
            or "127.0.0.1:7777"
        )
        self.api_endpoint = target
        self.atespace = atespace or DEFAULT_ATESPACE
        self.worker_family = worker_family
        self.auth_token = auth_token
        self.image_templates = dict(image_templates or {})
        self.default_template = default_template
        self.labels = dict(labels or {})
        self.acquire_timeout_s = acquire_timeout_s
        self.ready_timeout_s = ready_timeout_s

        self._loop_runner = loop_runner
        self._owns_loop_runner = loop_runner is None

        if client is not None:
            self._client = client
            self._owns_client = False
        else:
            self._client = None
            self._owns_client = True

        self._lock = threading.Lock()
        self._owned_actors: dict[str, RawSandboxInstance] = {}
        self._warm_paused_pool: _WarmPoolDict = _WarmPoolDict()
        self._background_tasks: set[asyncio.Task[Any]] = set()

    def _get_loop_runner(self) -> LoopRunner:
        with self._lock:
            if self._loop_runner is None:
                self._loop_runner = LoopRunner(name="ate-substrate-driver-sync")
                self._owns_loop_runner = True
            return self._loop_runner

    @property
    def client(self) -> Client:
        if self._client is None:
            self._client = Client(self.api_endpoint, auth_token=self.auth_token)
        return self._client

    def _sync(self, coro: Any) -> Any:
        return self._get_loop_runner().run(coro)

    async def close_async(self) -> None:
        if self._owns_client and self._client:
            await self._client.close()
            self._client = None
        if self._owns_loop_runner and self._loop_runner:
            self._loop_runner.stop()
            self._loop_runner = None

    def close(self) -> None:
        if self._loop_runner:
            if self._owns_client and self._client:
                self._loop_runner.run(self._client.close())
                self._client = None
            if self._owns_loop_runner:
                self._loop_runner.stop()
                self._loop_runner = None
        elif self._owns_client and self._client:
            self._sync(self.close_async())

    async def preflight_async(self) -> None:
        """Validate ate-env-api connectivity."""
        logger.info(
            "Running Substrate preflight checks (Atespace: %s, Endpoint: %s)...",
            self.atespace,
            self.api_endpoint,
        )
        try:
            await self.client.get(f"preflight-probe-{uuid.uuid4().hex[:6]}", atespace=self.atespace)
        except NotFoundError:
            # NotFoundError confirms the gRPC server is reachable and active.
            logger.debug("Preflight probe successfully reached ate-env-api")
        except Exception as e:
            raise PreflightError(
                f"Substrate preflight check failed to reach ate-env-api at {self.api_endpoint}: {e}"
            ) from e

    def preflight(self) -> None:
        self._sync(self.preflight_async())

    def ensure_template(self, env: EnvironmentSpec | str) -> str:
        """Resolve ActorTemplate name from template ID or task image.

        Note: ResourceLimits, runtime_bundle, snapshot_storage_uri, and placement
        fields in EnvironmentSpec are reserved for future scheduling extensions.
        Current Substrate environments are provisioned from pre-registered ActorTemplates.
        """
        if isinstance(env, str):
            image = env
            if image in self.image_templates:
                return self.image_templates[image]
            return self.default_template or "default-template"

        image = env.image or ""
        mapped = self.image_templates.get(image)
        if mapped:
            return mapped
        if self.default_template:
            return self.default_template
        if isinstance(env, EnvironmentSpec):
            return env.template_key()
        return "default-template"

    def _make_instance(
        self, actor_id: str, template_id: str, run_id: str, rootfs_image: str | None = None
    ) -> RawSandboxInstance:
        planes = {
            "ate_env": DataPlaneEndpoint(
                protocol="grpc",
                address=self.api_endpoint,
                headers={"x-env-id": actor_id, "x-env-atespace": self.atespace},
            ),
        }
        metadata = {
            "created_at": time.time(),
            "template": template_id,
            "atespace": self.atespace,
        }
        if rootfs_image:
            metadata["rootfs_image"] = rootfs_image
        if self.labels:
            metadata["labels"] = dict(self.labels)
        return RawSandboxInstance(
            instance_id=actor_id,
            endpoint=self.api_endpoint,
            template_id=template_id,
            run_id=run_id,
            metadata=metadata,
            data_planes=planes,
            rootfs_image=rootfs_image,
        )

    async def acquire_async(
        self,
        template_id: str,
        run_id: str,
        timeout_s: float = 180.0,
        rootfs_image: str | None = None,
    ) -> RawSandboxInstance:
        """Acquire a live sandbox instance within timeout_s."""
        to_s = timeout_s or self.acquire_timeout_s
        try:
            return await asyncio.wait_for(
                self._acquire_internal(template_id, run_id, rootfs_image=rootfs_image), timeout=to_s
            )
        except asyncio.TimeoutError as e:
            raise SandboxStartError(
                f"Timed out acquiring sandbox for template {template_id} after {to_s:g}s"
            ) from e

    async def _acquire_internal(
        self,
        template_id: str,
        run_id: str,
        rootfs_image: str | None = None,
    ) -> RawSandboxInstance:
        """Acquire or claim an environment.

        Note on readiness: Substrate resumes suspended environments implicitly on first
        guest traffic, so ready_timeout_s does not measure readiness here.
        """
        pool_key = (template_id, rootfs_image)
        while True:
            actor_id: str | None = None
            with self._lock:
                warm_list = self._warm_paused_pool.get(pool_key)
                if warm_list:
                    actor_id = warm_list.pop()

            if actor_id:
                logger.info(
                    "Claimed warm paused actor %s for template %s (rootfs_image=%s)",
                    actor_id,
                    template_id,
                    rootfs_image,
                )
                # Verify warm actor still exists on the server
                try:
                    await asyncio.wait_for(
                        self.client.get(actor_id, atespace=self.atespace),
                        timeout=min(self.ready_timeout_s, 15.0),
                    )
                except NotFoundError:
                    logger.warning("Warm actor %s not found on server, dropping from pool", actor_id)
                    with self._lock:
                        self._owned_actors.pop(actor_id, None)
                    continue
                except Exception as e:
                    # Put back on unexpected error and raise
                    with self._lock:
                        self._warm_paused_pool.setdefault(pool_key, []).append(actor_id)
                    raise SandboxStartError(f"Error checking warm actor {actor_id}: {e}") from e
                break
            else:
                actor_id = f"sb-{uuid.uuid4().hex[:16]}"
                logger.info(
                    "Creating environment %s (template=%s, atespace=%s, rootfs_image=%s)...",
                    actor_id,
                    template_id,
                    self.atespace,
                    rootfs_image,
                )
                last_err: Exception | None = None
                for attempt in range(4):
                    try:
                        await self.client.create(
                            actor_id,
                            template_name=template_id,
                            atespace=self.atespace,
                            rootfs_image=rootfs_image,
                        )
                        last_err = None
                        break
                    except (NotFoundError, FailedPreconditionError) as e:
                        raise SandboxStartError(
                            f"ActorTemplate '{template_id}' invalid or does not exist on Substrate server: {e}"
                        ) from e
                    except Exception as e:
                        last_err = e
                        if "already exists" in str(e).lower():
                            last_err = None
                            break
                        if attempt < 3:
                            await asyncio.sleep(0.05 * (attempt + 1))
                if last_err is not None:
                    raise SandboxStartError(f"Failed to create environment {actor_id}: {last_err}") from last_err
                break

        instance = self._make_instance(actor_id, template_id, run_id, rootfs_image=rootfs_image)
        with self._lock:
            self._owned_actors[actor_id] = instance
        return instance

    def acquire(
        self,
        template_id: str,
        run_id: str,
        timeout_s: float = 180.0,
        rootfs_image: str | None = None,
    ) -> RawSandboxInstance:
        return self._sync(
            self.acquire_async(template_id, run_id, timeout_s=timeout_s, rootfs_image=rootfs_image)
        )

    async def warm_pool_async(
        self,
        template_id: str,
        replicas: int,
        wait: bool = True,
        run_id: str | None = None,
        rootfs_image: str | None = None,
    ) -> None:
        """Pre-warm a pool of actors by creating and suspending them."""
        logger.info(
            "Sizing warm pool for template %s (rootfs_image=%s) to %d paused actors...",
            template_id,
            rootfs_image,
            replicas,
        )
        pool_key = (template_id, rootfs_image)
        with self._lock:
            current = list(self._warm_paused_pool.get(pool_key, []))
        needed = replicas - len(current)
        if needed <= 0:
            return

        sem = asyncio.Semaphore(16)
        effective_run_id = run_id or "warm-pool"

        async def _warm_one() -> str | None:
            actor_id = f"warm-{uuid.uuid4().hex[:10]}"
            async with sem:
                try:
                    await self.client.create(
                        actor_id,
                        template_name=template_id,
                        atespace=self.atespace,
                        rootfs_image=rootfs_image,
                    )
                    if wait:
                        await self.client.suspend(actor_id, atespace=self.atespace)
                    else:
                        task = asyncio.create_task(self.client.suspend(actor_id, atespace=self.atespace))
                        self._background_tasks.add(task)
                        def _done(t: asyncio.Task[Any]) -> None:
                            self._background_tasks.discard(t)
                            if not t.cancelled() and t.exception():
                                logger.warning("Background suspend for warm actor %s failed: %s", actor_id, t.exception())
                        task.add_done_callback(_done)
                    return actor_id
                except (NotFoundError, FailedPreconditionError) as e:
                    raise SandboxStartError(
                        f"ActorTemplate '{template_id}' invalid or does not exist on Substrate server: {e}"
                    ) from e
                except Exception as e:
                    logger.warning("Failed to provision warm actor %s: %s", actor_id, e)
                    return None

        tasks = [_warm_one() for _ in range(needed)]
        results = await asyncio.gather(*tasks, return_exceptions=True)

        for res in results:
            if isinstance(res, Exception):
                if isinstance(res, SandboxStartError):
                    raise res
                logger.warning("Error provisioning warm actor: %s", res)
            elif isinstance(res, str):
                aid = res
                with self._lock:
                    self._warm_paused_pool.setdefault(pool_key, []).append(aid)
                    self._owned_actors[aid] = self._make_instance(
                        aid, template_id, run_id=effective_run_id, rootfs_image=rootfs_image
                    )

    def warm_pool(
        self,
        template_id: str,
        replicas: int,
        wait: bool = True,
        run_id: str | None = None,
        rootfs_image: str | None = None,
    ) -> None:
        self._sync(
            self.warm_pool_async(
                template_id,
                replicas,
                wait=wait,
                run_id=run_id,
                rootfs_image=rootfs_image,
            )
        )

    async def unwarm_pool_async(self, template_id: str, rootfs_image: str | None = None) -> None:
        """Drain and delete paused actors in the warm pool."""
        logger.info("Unwarming pool for template %s (rootfs_image=%s)...", template_id, rootfs_image)
        with self._lock:
            if rootfs_image is not None:
                to_delete = self._warm_paused_pool.pop((template_id, rootfs_image), [])
            else:
                keys_to_pop = [
                    k for k in list(self._warm_paused_pool.keys())
                    if (isinstance(k, tuple) and k[0] == template_id)
                ]
                to_delete = []
                for k in keys_to_pop:
                    to_delete.extend(self._warm_paused_pool.pop(k, []))

        sem = asyncio.Semaphore(16)

        async def _delete_one(actor_id: str) -> None:
            async with sem:
                try:
                    await self.client.delete(actor_id, atespace=self.atespace)
                except Exception as e:
                    logger.warning("Failed deleting warm actor %s: %s", actor_id, e)
                with self._lock:
                    self._owned_actors.pop(actor_id, None)

        await asyncio.gather(*[_delete_one(aid) for aid in to_delete], return_exceptions=True)

    def unwarm_pool(self, template_id: str, rootfs_image: str | None = None) -> None:
        self._sync(self.unwarm_pool_async(template_id, rootfs_image=rootfs_image))

    async def release_async(self, instance_id: str, recycle: bool = False) -> None:
        """Release actor: deletes environment, or suspends if recycling.

        Note: Recycling suspends the actor and returns it to the warm pool
        without resetting guest filesystem or processes.
        """
        with self._lock:
            instance = self._owned_actors.pop(instance_id, None)

        if recycle and instance:
            try:
                await self.client.suspend(instance_id, atespace=self.atespace)
                pool_key = (instance.template_id, instance.rootfs_image)
                with self._lock:
                    self._warm_paused_pool.setdefault(pool_key, []).append(instance_id)
                    self._owned_actors[instance_id] = instance
                logger.info("Recycled actor %s to paused warm pool", instance_id)
                return
            except Exception as e:
                logger.warning("Failed to suspend actor %s for recycle, deleting: %s", instance_id, e)

        try:
            await self.client.delete(instance_id, atespace=self.atespace)
            logger.info("Deleted environment %s", instance_id)
        except Exception as e:
            logger.warning("Failed to delete environment %s: %s", instance_id, e)

    def release(self, instance_id: str, recycle: bool = False) -> None:
        self._sync(self.release_async(instance_id, recycle=recycle))

    async def reap_async(self, run_id: str) -> int:
        """Force delete all environments tagged with this run_id and all warm pool actors."""
        with self._lock:
            warm_actors: list[str] = []
            for tid, actors in list(self._warm_paused_pool.items()):
                warm_actors.extend(actors)
            self._warm_paused_pool.clear()

            to_reap = [
                aid for aid, inst in self._owned_actors.items()
                if inst.run_id == run_id or aid in warm_actors or inst.run_id == "warm-pool"
            ]
            to_reap = list(dict.fromkeys(to_reap + warm_actors))

        sem = asyncio.Semaphore(16)
        failed_actors: list[str] = []
        reaped_count = 0
        reap_lock = asyncio.Lock()

        async def _reap_one(aid: str) -> None:
            nonlocal reaped_count
            async with sem:
                try:
                    await self.client.delete(aid, atespace=self.atespace)
                    async with reap_lock:
                        reaped_count += 1
                except Exception as e:
                    logger.warning("Failed deleting actor %s during reap: %s", aid, e)
                    async with reap_lock:
                        failed_actors.append(aid)
                with self._lock:
                    self._owned_actors.pop(aid, None)

        await asyncio.gather(*[_reap_one(aid) for aid in to_reap], return_exceptions=True)

        if failed_actors:
            raise InfrastructureError(
                f"Failed to delete {len(failed_actors)} environments during reap: {failed_actors}"
            )

        logger.info("Reaped %d Substrate environments for run %s", reaped_count, run_id)
        return reaped_count

    def reap(self, run_id: str) -> int:
        return self._sync(self.reap_async(run_id))

    def build_runtime(
        self, instance: RawSandboxInstance, data_plane: str = "ate_env"
    ) -> tuple[Any, Any | None]:
        from ..runtime.substrate_env_client import SubstrateEnvClientRuntime

        ep = instance.data_planes.get(data_plane) or instance.data_planes.get("ate_env")
        return SubstrateEnvClientRuntime(
            env_id=instance.instance_id,
            endpoint=ep.address if ep else instance.endpoint,
            atespace=self.atespace,
            client=self.client,
            loop_runner=self._get_loop_runner(),
        ), ep
