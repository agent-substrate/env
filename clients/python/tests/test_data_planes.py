"""Per-sandbox data planes: each handle talks to exactly its own sandbox."""

import pytest

from ate_env import DataPlaneEndpoint, FleetConfig, SandboxFleet
from ate_env.backend.substrate import SubstrateBackendDriver
from ate_env.exceptions import SandboxStartError
from ate_env.runtime.mock import MockRuntimeHook
from ate_env.runtime.substrate_router import SubstrateRouterRuntime
from ate_env.types import EnvironmentSpec


def driver(**kw):
    return SubstrateBackendDriver(router_url="http://router:8080", atespace="space-a", **kw)


def acquire_two(d):
    tid = d.ensure_template(EnvironmentSpec(image="img:1"))
    return d.acquire(tid, "run-1"), d.acquire(tid, "run-1")


def test_each_sandbox_gets_its_own_coordinates():
    a, b = acquire_two(driver(grpc_target="{actor_id}.{atespace}.svc:50051"))
    for inst in (a, b):
        target = f"space-a/{inst.instance_id}"
        assert inst.data_planes["router"] == DataPlaneEndpoint(
            "http", "http://router:8080", {"ate-target-actor": target})
        assert inst.data_planes["grpc"].address == f"{inst.instance_id}.space-a.svc:50051"
        assert inst.data_planes["grpc"].headers == {"ate-target-actor": target}
    assert a.data_planes["router"].headers != b.data_planes["router"].headers


def test_shared_grpc_address_still_targets_one_sandbox_via_metadata():
    a, b = acquire_two(driver(grpc_target="router:50051"))
    assert a.data_planes["grpc"].address == b.data_planes["grpc"].address == "router:50051"
    assert a.data_planes["grpc"].headers != b.data_planes["grpc"].headers


def test_no_grpc_plane_without_grpc_target():
    a, _ = acquire_two(driver())
    assert set(a.data_planes) == {"router"}


def test_auth_header_is_attached_and_redacted_in_repr():
    a, _ = acquire_two(driver(auth_token="s3cr3t"))
    ep = a.data_planes["router"]
    assert ep.headers["authorization"] == "Bearer s3cr3t"
    assert "s3cr3t" not in repr(ep)


def test_fleet_builds_each_runtime_from_its_own_data_plane():
    fleet = SandboxFleet(FleetConfig(backend="substrate", router_url="http://router:8080",
                                     tenancy="space-a"))
    h1, h2 = fleet.acquire("t1"), fleet.acquire("t2")
    try:
        assert h1.sandbox_id != h2.sandbox_id
        for h in (h1, h2):
            assert isinstance(h.runtime, SubstrateRouterRuntime)
            assert h.runtime.target_header == f"space-a/{h.sandbox_id}"
            assert h.data_plane.headers["ate-target-actor"] == f"space-a/{h.sandbox_id}"
            assert h.endpoint == "http://router:8080"
    finally:
        h1.release()
        h2.release()


def test_fleet_grpc_runtime_uses_per_sandbox_address_and_closes_on_release():
    pytest.importorskip("grpc")
    from ate_env.runtime.substrate_env_client import SubstrateEnvClientRuntime

    fleet = SandboxFleet(FleetConfig(backend="substrate", tenancy="space-a", data_plane="grpc",
                                     grpc_endpoint="{actor_id}.space-a.svc:50051"))
    h = fleet.acquire("t1")
    assert isinstance(h.runtime, SubstrateEnvClientRuntime)
    assert h.runtime.endpoint == f"{h.sandbox_id}.space-a.svc:50051"
    assert h.endpoint == h.runtime.endpoint
    h.release()
    with pytest.raises(RuntimeError, match="closed"):
        h.runtime.exec("true")


class _NoDataPlaneDriver(SubstrateBackendDriver):
    def acquire(self, template_id, run_id, timeout_s=180.0):
        inst = super().acquire(template_id, run_id, timeout_s)
        inst.data_planes = {}
        return inst


def test_missing_data_plane_fails_fast_and_releases_the_claim():
    d = _NoDataPlaneDriver(router_url="http://router:8080", atespace="space-a")
    fleet = SandboxFleet(FleetConfig(backend="substrate"), driver=d)
    with pytest.raises(SandboxStartError, match="no 'router' data plane"):
        fleet.acquire("t1")
    assert d._owned_actors == {}  # no leaked claim


def test_mock_backend_uses_mock_runtime():
    fleet = SandboxFleet(FleetConfig(backend="mock"))
    h = fleet.acquire("t1")
    assert isinstance(h.runtime, MockRuntimeHook)
    assert h.data_plane is None
    h.release()
