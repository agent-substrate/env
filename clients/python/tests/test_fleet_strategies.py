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
    return res.stdout if hasattr(res, "stdout") else res


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


def test_user_guide_section1_synchronous_fleet_with_context_manager():
    config = FleetConfig(backend="mock")
    task = Task(id="task-1", image="docker.io/library/python:3.11")
    with SandboxFleet(config) as fleet:
        sb = fleet.acquire(task)
        res = sb.exec("echo hello")
        out = res.stdout if hasattr(res, "stdout") else res
        assert "echo hello" in out or "hello" in out


def test_user_guide_section2_synchronous_setup_and_batch_release():
    config = FleetConfig(backend="mock")
    tasks = [
        Task(id="task-1", image="docker.io/library/python:3.11"),
        Task(id="task-2", image="docker.io/library/python:3.11"),
    ]
    fleet = SandboxFleet(config)
    fleet.setup(tasks)
    sandboxes = fleet.acquire(tasks)
    try:
        assert len(sandboxes) == 2
        for t, sb in zip(tasks, sandboxes):
            res = sb.exec("echo hello")
            out = res.stdout if hasattr(res, "stdout") else res
            assert "echo hello" in out or "hello" in out
    finally:
        fleet.release(sandboxes)
        fleet.close()
