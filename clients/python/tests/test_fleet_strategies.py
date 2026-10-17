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

import pytest

from ate_env.config import FleetConfig
from ate_env.fleet import SandboxFleet
from ate_env.types import Task


def dummy_process_fn(task: Task, handle) -> str:
    res = handle.exec(f"echo processing {task.id}")
    return res.stdout


@pytest.mark.parametrize("strategy", ["none", "naive", "sliding", "pipelined"])
def test_fleet_strategies(strategy: str):
    tasks = [
        Task(id=f"task-{i}", image=f"image-{i % 2}")
        for i in range(6)
    ]
    cfg = FleetConfig(
        backend="mock",
        strategy=strategy,
        batch_size=2,
        max_warmpool_replicas=2,
    )
    fleet = SandboxFleet(cfg)
    fleet.load_tasks(tasks)

    results = fleet.run(dummy_process_fn, concurrency=2)
    assert len(results) == 6
    for i, r in enumerate(results):
        assert f"task-{i}" in r


def test_user_guide_section_1_synchronous_batch():
    config = FleetConfig(backend="mock")
    tasks = [
        Task(id="task-1", image="docker.io/library/python:3.11"),
        Task(id="task-2", image="docker.io/library/python:3.11"),
    ]
    with SandboxFleet(config) as fleet:
        sandboxes = fleet.acquire(tasks)
        try:
            for task, sb in zip(tasks, sandboxes):
                res = sb.exec("python3 -c 'print(\"hello world\")'")
                assert res.ok
                assert res.exit_code == 0
        finally:
            fleet.release(sandboxes)


async def test_user_guide_section_2_async_rollouts():
    from ate_env.async_fleet import AsyncSandboxFleet

    config = FleetConfig(
        backend="mock",
        batch_size=4,
        max_warmpool_replicas=4,
    )
    tasks = [Task(id=f"t-{i}", image="docker.io/library/python:3.11") for i in range(4)]
    fleet = AsyncSandboxFleet(config)
    await fleet.setup(tasks)

    sandboxes = await fleet.acquire_batch(tasks)
    for sb in sandboxes:
        async with sb:
            await sb.write_file_async("/testbed/calc.py", "def add(a, b): return a + b\n")
            res = await sb.exec_async(["python3", "-c", "import calc; assert calc.add(2, 3) == 5"])
            assert res.ok
    await fleet.teardown()
