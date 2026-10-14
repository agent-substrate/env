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

import pytest

from ate_env import FleetConfig, SandboxFleet, Task
from ate_env._gen.ateenv.v1alpha import env_pb2
from ate_env.backend.substrate import SubstrateBackendDriver
from ate_env.errors import PreflightError
from ate_env.types import EnvironmentSpec, PlacementSpec


async def test_substrate_driver_lifecycle_with_client(fake_stack):
    client, fakes = fake_stack
    driver = SubstrateBackendDriver(
        client=client,
        atespace="ate-env",
        worker_family="c2",
    )

    await driver.preflight_async()

    env = EnvironmentSpec(
        image="us-central1-docker.pkg.dev/sample/swe-bench:latest",
        placement=PlacementSpec(worker_family="c2"),
    )
    tid = driver.ensure_template(env)
    assert tid.startswith("tmpl-")

    # 1. Warm 2 paused actors: creates and suspends them in ate-env-api
    await driver.warm_pool_async(tid, replicas=2)
    assert len(driver._warm_paused_pool[tid]) == 2
    # Verify both environments were created and suspended on the server
    for aid in driver._warm_paused_pool[tid]:
        stored = fakes.environments.environments[("ate-env", aid)]
        assert stored.status == env_pb2.ENVIRONMENT_STATUS_SUSPENDED
        assert stored.template.name == tid

    # 2. Acquire claims one from warm pool without creating a new one
    inst1 = await driver.acquire_async(tid, run_id="run-1")
    assert len(driver._warm_paused_pool[tid]) == 1
    assert inst1.instance_id in fakes.environments.environments[("ate-env", inst1.instance_id)].id

    # 3. Acquire another when warm pool is exhausted creates a fresh one
    inst2 = await driver.acquire_async(tid, run_id="run-1")
    assert inst2.instance_id != inst1.instance_id
    assert ("ate-env", inst2.instance_id) in fakes.environments.environments

    # 4. Release with recycle=True calls suspend and returns to warm pool
    await driver.release_async(inst1.instance_id, recycle=True)
    assert len(driver._warm_paused_pool[tid]) == 1
    assert fakes.environments.environments[("ate-env", inst1.instance_id)].status == env_pb2.ENVIRONMENT_STATUS_SUSPENDED

    # 5. Release with recycle=False deletes environment permanently
    await driver.release_async(inst2.instance_id, recycle=False)
    assert ("ate-env", inst2.instance_id) not in fakes.environments.environments

    # 6. Reap deletes all remaining actors tagged with run_id
    inst3 = await driver.acquire_async(tid, run_id="run-to-reap")
    assert ("ate-env", inst3.instance_id) in fakes.environments.environments
    reaped = await driver.reap_async("run-to-reap")
    assert reaped == 1
    assert ("ate-env", inst3.instance_id) not in fakes.environments.environments

    # 7. Unwarm pool drains remaining paused actors
    await driver.warm_pool_async("tmpl-extra", replicas=1)
    assert len(driver._warm_paused_pool["tmpl-extra"]) == 1
    await driver.unwarm_pool_async("tmpl-extra")
    assert len(driver._warm_paused_pool.get("tmpl-extra", [])) == 0


async def test_substrate_synchronous_fleet_multiple_acquires(fake_stack):
    client, fakes = fake_stack
    cfg = FleetConfig(backend="substrate", endpoint=client.endpoint, template="tmpl-test")

    def _run_sync_fleet():
        fleet = SandboxFleet(cfg)
        try:
            sb1 = fleet.acquire(Task(id="task-1", image="img-1"))
            sb2 = fleet.acquire(Task(id="task-2", image="img-2"))
            assert sb1.sandbox_id != sb2.sandbox_id
            fleet.release([sb1, sb2])
        finally:
            fleet.close()

    await asyncio.to_thread(_run_sync_fleet)


async def test_substrate_fleet_setup_and_teardown_cleans_warm_pool(fake_stack):
    client, fakes = fake_stack
    cfg = FleetConfig(
        backend="substrate",
        endpoint=client.endpoint,
        template="tmpl-test",
        max_warmpool_replicas=2,
    )

    def _run_setup_teardown():
        fleet = SandboxFleet(cfg)
        fleet.load_tasks([Task(id="t1", image="img"), Task(id="t2", image="img")])
        fleet.setup()
        assert len(fakes.environments.environments) == 2
        fleet.teardown()
        assert len(fakes.environments.environments) == 0
        fleet.close()

    await asyncio.to_thread(_run_setup_teardown)


async def test_substrate_preflight_fails_on_closed_port():
    driver = SubstrateBackendDriver(api_endpoint="127.0.0.1:59999")
    try:
        with pytest.raises(PreflightError):
            await driver.preflight_async()
    finally:
        await driver.close_async()


def test_substrate_default_template_resolution():
    # B1: A fleet with no template configured falls back to "default-template"
    cfg = FleetConfig(backend="substrate")
    fleet = SandboxFleet(cfg)
    tmpl = fleet._ensure_template_for_image("docker.io/library/python@sha256:2176bb31f3eb")
    assert tmpl == "default-template"

    # M5: image_templates mapping wins over config.template
    cfg_mapped = FleetConfig(
        backend="substrate",
        template="fallback-template",
        image_templates={"docker.io/library/python:3.11": "python-template"},
    )
    fleet_mapped = SandboxFleet(cfg_mapped)
    assert fleet_mapped._ensure_template_for_image("docker.io/library/python:3.11") == "python-template"
    assert fleet_mapped._ensure_template_for_image("other-img") == "fallback-template"

    # SubstrateBackendDriver with default_template="default-template" does not treat it as unset
    driver = SubstrateBackendDriver(default_template="default-template")
    assert driver.ensure_template("docker.io/library/python@sha256:2176bb31f3eb") == "default-template"
    driver_none = SubstrateBackendDriver(default_template=None)
    assert driver_none.ensure_template("docker.io/library/python@sha256:2176bb31f3eb") == "default-template"
