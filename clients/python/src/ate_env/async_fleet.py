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

"""`AsyncSandboxFleet` — an awaitable, event-loop-native fleet.

Matches the design of upstream Kubernetes agent-sandbox (AsyncSandboxFleet),
enabling RL post-training frameworks (TorchRL, VeRL, SkyRL) to overlap LLM token
generation with background sandbox pre-warming and non-blocking batch acquisition.
"""

from __future__ import annotations

import asyncio
import functools
import inspect
import logging
from concurrent.futures import ThreadPoolExecutor
from typing import Any, Callable, Dict, List, Optional

from .config import FleetConfig
from .fleet import SandboxFleet
from .handle import SandboxHandle
from .types import FleetPlan, Task

logger = logging.getLogger("sandbox_sdk.async_fleet")


class AsyncSandboxFleet:
    """Awaitable, non-blocking wrapper over `SandboxFleet`.

    Enables asynchronous acquisition, batch claiming, and pipelining
    concurrency without blocking the asyncio event loop.
    """

    def __init__(
        self,
        config: Optional[FleetConfig] = None,
        driver: Optional[Any] = None,
        *,
        sync_fleet: Optional[SandboxFleet] = None,
    ):
        self._fleet = sync_fleet or SandboxFleet(config, driver)
        self._executor: Optional[ThreadPoolExecutor] = None

    def _thread_pool(self) -> ThreadPoolExecutor:
        """Dedicated thread pool for offloading synchronous driver and runtime calls.

        Sized to max_concurrent plus headroom so that overlapping pre-warm
        and execution don't starve each other.
        """
        if self._executor is None:
            mc = max(1, self._fleet.config.max_concurrent)
            win = self._fleet.config.window_size or mc
            workers = min(1024, max(64, mc + 2 * win + 16))
            self._executor = ThreadPoolExecutor(
                max_workers=workers, thread_name_prefix="sandbox-sdk-async"
            )
            logger.debug("Async fleet thread pool sized to %d workers", workers)
        return self._executor

    async def _to_thread(self, fn: Callable[..., Any], *args: Any, **kwargs: Any) -> Any:
        """Run blocking operation on the dedicated thread pool."""
        loop = asyncio.get_running_loop()
        return await loop.run_in_executor(
            self._thread_pool(), functools.partial(fn, *args, **kwargs)
        )

    def close(self, *, wait: bool = True) -> None:
        """Shut down the dedicated thread pool."""
        if self._executor is not None:
            self._executor.shutdown(wait=wait, cancel_futures=True)
            self._executor = None

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
    def tasks(self) -> List[Task]:
        return self._fleet.tasks

    def load_tasks(self, tasks: List[Any]) -> None:
        self._fleet.load_tasks(tasks)

    def image_counts(self) -> Dict[str, int]:
        return self._fleet.image_counts()

    # --- Awaitable Lifecycle Methods ---
    async def preflight(self) -> None:
        await self._to_thread(self._fleet.preflight)

    async def plan(self) -> FleetPlan:
        return await self._to_thread(self._fleet.plan)

    async def setup(self) -> "AsyncSandboxFleet":
        await self._to_thread(self._fleet.setup)
        return self

    async def warm_images(
        self, images: List[str], replicas: Optional[int] = None, wait: bool = True
    ) -> None:
        await self._to_thread(self._fleet.warm_images, images, replicas=replicas, wait=wait)

    async def unwarm_image(self, image: str) -> None:
        await self._to_thread(self._fleet.unwarm_image, image)

    async def acquire(self, task: Task | str, timeout_s: Optional[float] = None) -> SandboxHandle:
        """Asynchronously acquire a single sandbox."""
        return await self._to_thread(self._fleet.acquire, task, timeout_s=timeout_s)

    async def acquire_batch(
        self, tasks: List[Task | str], timeout_s: Optional[float] = None
    ) -> List[SandboxHandle]:
        """Asynchronously acquire a batch of sandboxes in parallel."""
        return list(
            await asyncio.gather(*(self.acquire(t, timeout_s=timeout_s) for t in tasks))
        )

    async def release(self, handle: SandboxHandle) -> None:
        await self._to_thread(self._fleet.release, handle)

    async def teardown(self) -> None:
        await self._to_thread(self._fleet.teardown)

    # --- Async Context Manager ---
    async def __aenter__(self) -> "AsyncSandboxFleet":
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
        result = await self._to_thread(process_fn, task, handle)
        if inspect.isawaitable(result):
            return await result
        return result

    async def run(
        self,
        process_fn: Callable[[Task, SandboxHandle], Any],
        concurrency: Optional[int] = None,
    ) -> List[Any]:
        """Execute tasks asynchronously with bounded concurrency."""
        c = concurrency or min(self.config.max_concurrent, len(self.tasks) or 1)
        sem = asyncio.Semaphore(max(1, c))
        results: List[Any] = [None] * len(self.tasks)

        async def _worker(idx: int, task: Task) -> None:
            async with sem:
                handle = await self.acquire(task)
                try:
                    res = await self._call_process_fn(process_fn, task, handle)
                    results[idx] = res
                    handle.recycle()
                except Exception as e:
                    logger.error("Async execution failed for task %s: %s", task.id, e)
                    handle.release()
                    results[idx] = e

        await asyncio.gather(*(_worker(i, t) for i, t in enumerate(self.tasks)))
        return results
