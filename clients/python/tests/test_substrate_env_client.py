"""Tests for SubstrateEnvClientRuntime wrapping the official ate-env-client."""

import pytest
from unittest.mock import AsyncMock, MagicMock, patch

from ate_env.exceptions import (
    CommandStartError,
    InfrastructureError,
    SandboxProtocolError,
    SandboxUnavailableError,
)
from ate_env.runtime.substrate_env_client import SubstrateEnvClientRuntime
from ate_env.errors import (
    InvalidArgumentError,
    NotFoundError,
    RpcError,
)
from ate_env.types import ProcessInfo, ProcessOutput, ProcessState


@pytest.fixture
def mock_ate_env():
    with patch("ate_env.runtime.substrate_env_client._CLIENT_MANAGER") as mock_mgr:
        client = MagicMock()
        env = MagicMock()
        mock_mgr.get_client.return_value = client
        client.env.return_value = env
        mock_mgr.run_sync.side_effect = lambda coro, timeout_s=None: None
        async def _run_async(coro):
            return await coro
        mock_mgr.run_async.side_effect = _run_async
        yield mock_mgr, client, env


def test_substrate_env_client_init(mock_ate_env):
    mock_mgr, client, env = mock_ate_env
    rt = SubstrateEnvClientRuntime(endpoint="localhost:7777", env_id="dev1", atespace="test-space")
    assert rt.endpoint == "localhost:7777"
    assert rt.env_id == "dev1"
    assert rt.atespace == "test-space"
    client.env.assert_called_once_with("dev1", atespace="test-space")


def test_substrate_env_client_exec_success(mock_ate_env):
    mock_mgr, client, env = mock_ate_env
    # mock run_sync returning (exit_code, stdout, stderr)
    mock_mgr.run_sync.side_effect = lambda coro, timeout_s=None: (0, "hello world\n", "")

    rt = SubstrateEnvClientRuntime("localhost:7777", "dev1")
    res = rt.exec("echo hello world")
    assert res.exit_code == 0
    assert res.stdout == "hello world\n"
    assert res.timed_out is False


def test_substrate_env_client_exec_timeout(mock_ate_env):
    mock_mgr, client, env = mock_ate_env
    mock_mgr.run_sync.side_effect = TimeoutError("Timed out")

    rt = SubstrateEnvClientRuntime("localhost:7777", "dev1")
    res = rt.exec("sleep 100", timeout_s=1.0)
    assert res.timed_out is True
    assert res.exit_code is None


def test_substrate_env_client_error_mapping(mock_ate_env):
    mock_mgr, client, env = mock_ate_env

    rt = SubstrateEnvClientRuntime("localhost:7777", "dev1")

    # InvalidArgumentError -> SandboxProtocolError
    mock_mgr.run_sync.side_effect = InvalidArgumentError("bad args")
    with pytest.raises(SandboxProtocolError):
        rt.exec("bad")

    # NotFoundError -> CommandStartError
    mock_mgr.run_sync.side_effect = NotFoundError("not found")
    with pytest.raises(CommandStartError):
        rt.exec("bad")

    # RpcError -> SandboxUnavailableError
    mock_mgr.run_sync.side_effect = RpcError("unavailable")
    with pytest.raises(SandboxUnavailableError):
        rt.exec("bad")


def test_substrate_env_client_file_io(mock_ate_env):
    mock_mgr, client, env = mock_ate_env
    rt = SubstrateEnvClientRuntime("localhost:7777", "dev1")

    mock_mgr.run_sync.side_effect = lambda coro, timeout_s=None: None
    rt.write_file("/testbed/file.txt", "content")

    mock_mgr.run_sync.side_effect = lambda coro, timeout_s=None: b"content"
    data = rt.read_file_bytes("/testbed/file.txt")
    assert data == b"content"


@pytest.mark.asyncio
async def test_substrate_env_client_async_native(mock_ate_env):
    mock_mgr, client, env = mock_ate_env
    rt = SubstrateEnvClientRuntime("localhost:7777", "dev1")

    # Mock native async methods on env
    mock_proc = MagicMock()
    env.start_process = AsyncMock(return_value=mock_proc)
    exit_info = ProcessInfo(
        process_id="proc-1",
        command=("echo", "async", "ok"),
        pid=123,
        exit_code=0,
        state=ProcessState.EXITED,
        started_at=None,
        finished_at=None,
    )
    mock_proc.wait = AsyncMock(return_value=exit_info)

    async def _mock_output(follow=True):
        yield ProcessOutput(stdout=b"async ok\n")
        yield ProcessOutput(exit=exit_info)

    mock_proc.output = _mock_output

    res = await rt.exec_async("echo async ok")
    assert res.exit_code == 0
    assert res.stdout == "async ok\n"

    # Async file write & read
    env.write_file = AsyncMock()
    await rt.write_file_async("/testbed/calc.py", "print(1)")
    env.write_file.assert_awaited_once_with("/testbed/calc.py", b"print(1)")

    env.read_file_bytes = AsyncMock(return_value=b"print(1)")
    data = await rt.read_file_bytes_async("/testbed/calc.py")
    assert data == b"print(1)"
