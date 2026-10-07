import pytest
from ate_env.backend.substrate import SubstrateBackendDriver
from ate_env.types import EnvironmentSpec, PlacementSpec


def test_substrate_driver_lifecycle():
    driver = SubstrateBackendDriver(
        api_endpoint="http://localhost:8080",
        router_url="http://localhost:8000",
        atespace="ate-demo-sandbox",
        worker_family="c2"
    )

    driver.preflight()

    env = EnvironmentSpec(
        image="us-central1-docker.pkg.dev/songsunny-gke-dev2/swe-bench:latest",
        placement=PlacementSpec(worker_family="c2")
    )
    tid = driver.ensure_template(env)
    assert tid.startswith("tmpl-")

    # Warm 2 paused actors
    driver.warm_pool(tid, replicas=2)
    assert len(driver._warm_paused_pool[tid]) == 2

    # Acquire claims one from warm pool
    inst1 = driver.acquire(tid, run_id="run-1")
    assert inst1.status == "RUNNING"
    assert len(driver._warm_paused_pool[tid]) == 1

    # Release with recycle returns it to paused pool
    driver.release(inst1.instance_id, recycle=True)
    assert len(driver._warm_paused_pool[tid]) == 2

    # Reap by run_id
    reaped = driver.reap("run-1")
    assert reaped >= 0
