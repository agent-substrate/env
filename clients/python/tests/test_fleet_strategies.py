import pytest
from ate_env.config import FleetConfig
from ate_env.fleet import SandboxFleet
from ate_env.types import Task


def dummy_process_fn(task: Task, handle) -> str:
    res = handle.exec(f"echo processing {task.id}")
    return res


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
