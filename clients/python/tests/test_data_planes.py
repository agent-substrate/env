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

"""Per-sandbox data planes: each handle talks to its environment."""

from __future__ import annotations

from unittest.mock import AsyncMock, MagicMock
import pytest

from ate_env import DataPlaneEndpoint, FleetConfig, SandboxFleet
from ate_env.backend.substrate import SubstrateBackendDriver
from ate_env.errors import SandboxStartError
from ate_env.runtime.mock import MockRuntimeHook
from ate_env.runtime.substrate_env_client import SubstrateEnvClientRuntime
from ate_env.types import EnvironmentSpec


def mock_client():
    c = MagicMock()
    c.create = AsyncMock()
    c.delete = AsyncMock()
    c.suspend = AsyncMock()
    return c


def acquire_two(d):
    tid = d.ensure_template(EnvironmentSpec(image="img:1"))
    return d.acquire(tid, "run-1"), d.acquire(tid, "run-1")


def test_each_sandbox_gets_its_own_coordinates():
    client = mock_client()
    d = SubstrateBackendDriver(client=client, atespace="space-a")
    a, b = acquire_two(d)
    for inst in (a, b):
        target = inst.instance_id
        assert "ate_env" in inst.data_planes
        assert inst.data_planes["ate_env"].headers["x-env-id"] == target
        assert inst.data_planes["ate_env"].headers["x-env-atespace"] == "space-a"
    assert a.data_planes["ate_env"].headers != b.data_planes["ate_env"].headers


def test_fleet_builds_each_runtime_from_its_own_data_plane():
    client = mock_client()
    d = SubstrateBackendDriver(client=client, atespace="space-a")
    fleet = SandboxFleet(
        FleetConfig(backend="substrate", tenancy="space-a", data_plane="ate_env"),
        driver=d,
    )
    h1, h2 = fleet.acquire("t1"), fleet.acquire("t2")
    try:
        assert h1.sandbox_id != h2.sandbox_id
        for h in (h1, h2):
            assert isinstance(h.runtime, SubstrateEnvClientRuntime)
            assert h.runtime.env_id == h.sandbox_id
            assert h.runtime.atespace == "space-a"
    finally:
        h1.release()
        h2.release()


def test_mock_backend_uses_mock_runtime():
    fleet = SandboxFleet(FleetConfig(backend="mock"))
    h = fleet.acquire("t1")
    assert isinstance(h.runtime, MockRuntimeHook)
    assert h.data_plane is None
    h.release()
