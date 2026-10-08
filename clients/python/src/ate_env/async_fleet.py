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

"""`AsyncSandboxFleet` — an awaitable, event-loop-native fleet.

Enables RL post-training frameworks (TorchRL, VeRL, SkyRL) to overlap LLM token
generation with background sandbox pre-warming and non-blocking batch acquisition.
"""

from __future__ import annotations

import asyncio
import inspect
import logging
from collections.abc import Callable
from typing import Any

from .config import FleetConfig
from .fleet import SandboxFleet
from .handle import SandboxHandle
from .types import FleetPlan, Task

logger = logging.getLogger("sandbox_sdk.async_fleet")


class AsyncSandboxFleet:
    """Awaitable, natively asynchronous wrapper over `SandboxFleet`.

    Enables asynchronous acquisition, batch claiming, and pipelining
    concurrency without blocking the asyncio event loop.
    """

    def __init__(
        self,
        config: FleetConfig | None = None,
        driver: Any | None = None,
        *,
        sync_fleet: SandboxFleet | None = None,
    ):
        self._fleet = sync_fleet or SandboxFleet(config, driver)

    def close(self, *, wait: bool = True) -> None:
        """Close fleet resources."""
        if hasattr(self.backend, "close"):
            self.backend.close()

    def __del__(self) -> None:
        try:
            self.close(wait=False)
        except Exception:
            pass

    # --- Synchronous passthroughs ---
    @property
    def config(self) -> FleetConfig:
        return self._fleet.config

    @property
    def backend(self) -> Any:
        return self._fleet.backend

    @property
    def tasks(self) -> list[Task]:
        return self._fleet.tasks

    def load_tasks(self, tasks: list[Any]) -> None:
        self._fleet.load_tasks(tasks)

    def image_counts(self) -> dict[str, int]:
        return self._fleet.image_counts()

    # --- Awaitable Lifecycle Methods ---
    async def preflight(self) -> None:
        if hasattr(self.backend, "preflight_async"):
            await self.backend.preflight_async()
        else:
            await asyncio.to_thread(self._fleet.preflight)

    async def plan(self) -> FleetPlan:
        return await asyncio.to_thread(self._fleet.plan)

    async def setup(self, tasks: list[Any] | None = None) -> AsyncSandboxFleet:
        if tasks is not None:
            self._fleet.load_tasks(tasks)
        await self.preflight()
        plan = await self.plan()
        for entry in plan.entries:
            if entry.replicas > 0:
                await self._warm_pool_for_template(
                    entry.template_id,
                    entry.replicas,
                    wait=True,
                    rootfs_image=getattr(entry, "rootfs_image", None),
                )
        return self

    async def _ensure_template_for_image_async(self, image: str) -> tuple[str, str | None]:
        cached = self._fleet._image_to_template.get(image)
        if cached is not None:
            return cached
        return await asyncio.to_thread(self._fleet._ensure_template_for_image, image)

    async def _warm_pool_for_template(
        self,
        template_id: str,
        replicas: int,
        wait: bool = True,
        rootfs_image: str | None = None,
    ) -> None:
        if hasattr(self.backend, "warm_pool_async"):
            await self.backend.warm_pool_async(
                template_id, replicas, wait=wait, rootfs_image=rootfs_image
            )
        else:
            await asyncio.to_thread(
                self.backend.warm_pool,
                template_id,
                replicas,
                wait=wait,
                rootfs_image=rootfs_image,
            )

    async def warm_images(
        self, images: list[str], replicas: int | None = None, wait: bool = True
    ) -> None:
        rep = replicas if replicas is not None else self.config.max_warmpool_replicas
        for img in images:
            template_id, rootfs_image = await self._ensure_template_for_image_async(img)
            await self._warm_pool_for_template(template_id, rep, wait=wait, rootfs_image=rootfs_image)

    async def unwarm_image(self, image: str) -> None:
        entry = self._fleet._image_to_template.get(image)
        if entry:
            template_id, rootfs_image = entry
            if hasattr(self.backend, "unwarm_pool_async"):
                await self.backend.unwarm_pool_async(template_id, rootfs_image=rootfs_image)
            else:
                await asyncio.to_thread(
                    self.backend.unwarm_pool, template_id, rootfs_image=rootfs_image
                )

    async def acquire(self, task: Task | str, timeout_s: float | None = None) -> SandboxHandle:
        """Asynchronously acquire a single sandbox."""
        task_obj: Task
        if isinstance(task, str):
            found = next((t for t in self.tasks if t.id == task), None)
            if found:
                task_obj = found
            else:
                task_obj = Task(id=task, image="default")
        else:
            task_obj = task

        if task_obj.template:
            template_id = task_obj.template
            rootfs_image = None
        else:
            template_id, rootfs_image = await self._ensure_template_for_image_async(task_obj.image)
        to_s = timeout_s or self.config.acquire_timeout_s

        if hasattr(self.backend, "acquire_async"):
            raw_inst = await self.backend.acquire_async(
                template_id, self._fleet.run_id, timeout_s=to_s, rootfs_image=rootfs_image
            )
        else:
            raw_inst = await asyncio.to_thread(
                self.backend.acquire,
                template_id,
                self._fleet.run_id,
                timeout_s=to_s,
                rootfs_image=rootfs_image,
            )

        try:
            runtime, data_plane = self._fleet._build_runtime(raw_inst)
        except Exception:
            try:
                if hasattr(self.backend, "release_async"):
                    await self.backend.release_async(raw_inst.instance_id, recycle=False)
                else:
                    await asyncio.to_thread(self.backend.release, raw_inst.instance_id, recycle=False)
            except Exception:
                logger.warning(
                    "Failed to release sandbox %s after runtime setup error",
                    raw_inst.instance_id,
                    exc_info=True,
                )
            raise

        return SandboxHandle(
            sandbox_id=raw_inst.instance_id,
            endpoint=data_plane.address if data_plane else raw_inst.endpoint,
            task=task_obj,
            run_id=self._fleet.run_id,
            backend=self.backend,
            runtime=runtime,
            data_plane=data_plane,
            default_timeout_s=self.config.step_timeout_s,
        )

    async def acquire_batch(
        self, tasks: list[Task | str], timeout_s: float | None = None
    ) -> list[SandboxHandle]:
        """Asynchronously acquire a batch of sandboxes in parallel."""
        return list(
            await asyncio.gather(*(self.acquire(t, timeout_s=timeout_s) for t in tasks))
        )

    async def release(self, handle: SandboxHandle) -> None:
        await handle.release_async()

    async def teardown(self) -> None:
        if hasattr(self.backend, "reap_async"):
            await self.backend.reap_async(self._fleet.run_id)
        else:
            await asyncio.to_thread(self.backend.reap, self._fleet.run_id)

    # --- Async Context Manager ---
    async def __aenter__(self) -> AsyncSandboxFleet:
        await self.setup()
        return self

    async def __aexit__(self, exc_type, exc_val, exc_tb) -> None:
        try:
            await self.teardown()
        finally:
            self.close()

    # --- Parallel Processing ---
    async def _call_process_fn(
        self, process_fn: Callable[..., Any], task: Task, handle: SandboxHandle
    ) -> Any:
        if inspect.iscoroutinefunction(process_fn) or inspect.iscoroutinefunction(
            getattr(process_fn, "__call__", None)
        ):
            return await process_fn(task, handle)
        result = await asyncio.to_thread(process_fn, task, handle)
        if inspect.isawaitable(result):
            return await result
        return result

    async def run(
        self,
        process_fn: Callable[[Task, SandboxHandle], Any],
        concurrency: int | None = None,
    ) -> list[Any]:
        """Execute tasks asynchronously with bounded concurrency."""
        c = concurrency or min(self.config.max_concurrent, len(self.tasks) or 1)
        sem = asyncio.Semaphore(max(1, c))
        results: list[Any] = [None] * len(self.tasks)

        async def _worker(idx: int, task: Task) -> None:
            async with sem:
                handle = await self.acquire(task)
                try:
                    res = await self._call_process_fn(process_fn, task, handle)
                    results[idx] = res
                    handle.release()
                except Exception as e:
                    logger.error("Async execution failed for task %s: %s", task.id, e)
                    handle.release()
                    results[idx] = e

        await asyncio.gather(*(_worker(i, t) for i, t in enumerate(self.tasks)))
        return results
