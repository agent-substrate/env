import pytest
from pydantic import ValidationError

from ate_env.exceptions import SandboxUnavailableError
from ate_env.providers.nemo_gym import (
    SANDBOX_RUNTIME_RETURN_CODE,
    UnifiedSandboxProvider,
    fleet_config_from_provider_config,
)
from ate_env.runtime.mock import MockRuntimeHook
from ate_env.types import ExecResult


def test_nemo_gym_provider_contract():
    # Initialize provider using mock backend for hermetic test
    provider = UnifiedSandboxProvider(config={
        "backend": "mock",
        "api_url": "http://127.0.0.1:8080",
        "atespace": "nemo-test-env",
        "max_warmpool_replicas": 2,
    })

    # 1. create(spec)
    spec = {
        "id": "episode-42",
        "image": "nemo/swe-bench:latest",
        "files": {
            "/workspace/hello.py": "print('hello from nemo')\n"
        }
    }
    handle = provider.create(spec)
    assert handle.sandbox_id.startswith("mock-sb-")

    # Verify initial file was uploaded
    content = provider.download_file(handle, "/workspace/hello.py")
    assert b"hello from nemo" in content

    # 2. exec(handle, cmd)
    exec_res = provider.exec(handle, "python3 /workspace/hello.py")
    assert exec_res["return_code"] == 0
    assert exec_res["error_type"] is None

    # 3. status(handle)
    assert provider.status(handle) == "RUNNING"

    # 4. close(handle)
    provider.close(handle)


def test_provider_rejects_unknown_config_keys():
    with pytest.raises(ValidationError, match="warm_pool_size"):
        UnifiedSandboxProvider(config={"backend": "mock", "warm_pool_size": 4})


def test_provider_config_aliases_and_passthrough():
    cfg = fleet_config_from_provider_config({
        "backend": "mock",
        "api_url": "http://api:1",
        "atespace": "space-a",
        "data_plane": "grpc",
        "grpc_endpoint": "{actor_id}.space-a.svc:50051",
        "auth_token": "tok",
    })
    assert cfg.endpoint == "http://api:1"
    assert cfg.router_url == "http://api:1"  # defaults to the endpoint
    assert cfg.tenancy == "space-a"
    assert cfg.data_plane == "grpc"
    assert cfg.auth_token.get_secret_value() == "tok"


def test_provider_exec_reports_error_type_instead_of_raising():
    provider = UnifiedSandboxProvider(config={"backend": "mock"})
    handle = provider.create({"id": "ep-1", "image": "img"})
    handle.runtime.set_response("dead", SandboxUnavailableError("router 503"))
    handle.runtime.set_response(
        "slow", ExecResult(exit_code=None, stdout="partial", stderr="", timed_out=True))
    handle.runtime.set_response("fail", ExecResult(exit_code=2, stdout="", stderr="x"))

    dead = provider.exec(handle, "dead")
    assert dead["error_type"] == "sandbox"
    assert dead["return_code"] == SANDBOX_RUNTIME_RETURN_CODE
    assert dead["stdout"] is None

    slow = provider.exec(handle, "slow")
    assert slow["error_type"] == "timeout"
    assert slow["return_code"] == SANDBOX_RUNTIME_RETURN_CODE
    assert slow["stdout"] == "partial"

    fail = provider.exec(handle, "fail")
    assert fail["error_type"] is None
    assert fail["return_code"] == 2
    provider.close(handle)


def test_provider_create_releases_sandbox_when_file_upload_fails(monkeypatch):
    provider = UnifiedSandboxProvider(config={"backend": "mock"})

    def failing_write(self, path, content):
        raise SandboxUnavailableError("upload failed")

    monkeypatch.setattr(MockRuntimeHook, "write_file", failing_write)
    with pytest.raises(SandboxUnavailableError):
        provider.create({"id": "ep-1", "image": "img", "files": {"/a.txt": "b"}})
    assert provider.fleet.backend.instances == {}
