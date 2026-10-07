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

import asyncio
import pytest

from ate_env.async_fleet import AsyncSandboxFleet
from ate_env.config import FleetConfig
from ate_env.types import Task


@pytest.mark.asyncio
async def test_async_fleet_setup_and_acquire():
    cfg = FleetConfig(backend="mock", max_concurrent=4)
    fleet = AsyncSandboxFleet(cfg)

    tasks = [
        Task(id=f"task-{i}", image="repo-img-1") for i in range(4)
    ]
    fleet.load_tasks(tasks)

    # Async setup
    await fleet.setup()

    # Async acquire single
    handle = await fleet.acquire(tasks[0])
    assert handle.sandbox_id.startswith("mock-sb-")

    # Command execution
    out = handle.exec("echo hello")
    assert "echo hello" in out

    await fleet.release(handle)
    await fleet.teardown()
    fleet.close()


@pytest.mark.asyncio
async def test_async_fleet_acquire_batch():
    cfg = FleetConfig(backend="mock", max_concurrent=4)
    fleet = AsyncSandboxFleet(cfg)

    tasks = [
        Task(id=f"task-batch-{i}", image=f"repo-img-{i % 2}") for i in range(4)
    ]
    fleet.load_tasks(tasks)
    await fleet.setup()

    # Async acquire batch concurrently
    handles = await fleet.acquire_batch(tasks)
    assert len(handles) == 4
    for h in handles:
        assert h.sandbox_id.startswith("mock-sb-")
        await fleet.release(h)

    await fleet.teardown()
    fleet.close()


@pytest.mark.asyncio
async def test_async_context_manager():
    cfg = FleetConfig(backend="mock")
    async with AsyncSandboxFleet(cfg) as fleet:
        task = Task(id="task-ctx", image="repo-img-1")
        handle = await fleet.acquire(task)
        assert handle is not None

        # Handle async context manager
        async with handle:
            res = handle.exec("echo in-handle-ctx")
            assert "in-handle-ctx" in res


@pytest.mark.asyncio
async def test_async_run_parallel():
    cfg = FleetConfig(backend="mock", max_concurrent=4)
    fleet = AsyncSandboxFleet(cfg)
    tasks = [Task(id=f"t-{i}", image="img-1") for i in range(6)]
    fleet.load_tasks(tasks)
    await fleet.setup()

    async def async_worker(task, handle):
        await asyncio.sleep(0.01)
        return f"done-{task.id}-{handle.sandbox_id[:8]}"

    results = await fleet.run(async_worker, concurrency=3)
    assert len(results) == 6
    for i, r in enumerate(results):
        assert r.startswith(f"done-t-{i}")

    await fleet.teardown()
    fleet.close()
