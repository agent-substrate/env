"""Exec result contract, strict-mode errors, command contract, and file helpers."""

import pickle
import subprocess

import pytest

from ate_env import (
    CommandExecutionError,
    CommandStartError,
    CommandTimeoutError,
    ExecResult,
    FleetConfig,
    SandboxFleet,
    SandboxProtocolError,
    SandboxUnavailableError,
)
from ate_env.exceptions import TimeoutError as SdkTimeoutError
from ate_env.runtime.base import (
    RuntimeGuestHook,
    command_to_argv,
    read_file_via_exec,
    write_file_via_exec,
)
from ate_env.runtime.mock import MockRuntimeHook


def test_exec_result_invariants():
    assert ExecResult(exit_code=0, stdout="", stderr="").ok
    assert not ExecResult(exit_code=1, stdout="", stderr="").ok
    assert not ExecResult(exit_code=None, stdout="partial", stderr="", timed_out=True).ok
    with pytest.raises(ValueError):
        ExecResult(exit_code=0, stdout="", stderr="", timed_out=True)
    with pytest.raises(ValueError):
        ExecResult(exit_code=None, stdout="", stderr="")


@pytest.fixture
def handle():
    fleet = SandboxFleet(FleetConfig(backend="mock"))
    h = fleet.acquire("t1")
    yield h
    h.release()


def test_check_raises_command_execution_error_with_result(handle):
    handle.runtime.set_response("failing-cmd", ExecResult(exit_code=2, stdout="", stderr="boom"))
    with pytest.raises(CommandExecutionError) as ei:
        handle.exec("failing-cmd")
    assert ei.value.result.exit_code == 2
    assert handle.exec("failing-cmd", check=False) == ""


def test_check_raises_command_timeout_error(handle):
    handle.runtime.set_response(
        "slow-cmd", ExecResult(exit_code=None, stdout="", stderr="", timed_out=True))
    with pytest.raises(CommandTimeoutError) as ei:
        handle.exec("slow-cmd")
    # Catchable as either a command failure or an SDK timeout.
    assert isinstance(ei.value, CommandExecutionError)
    assert isinstance(ei.value, SdkTimeoutError)
    assert ei.value.result.timed_out


@pytest.mark.parametrize("check", [True, False])
def test_infrastructure_errors_propagate_regardless_of_check(handle, check):
    handle.runtime.set_response(
        "dead-cmd", SandboxUnavailableError("gone", sandbox_id="sb-1", status="HTTP 503"))
    with pytest.raises(SandboxUnavailableError):
        handle.exec("dead-cmd", check=check)


def test_retryability_defaults_and_pickling():
    assert SandboxUnavailableError("x").retryable
    assert not SandboxProtocolError("x").retryable
    assert not CommandStartError("x").retryable
    err = SandboxUnavailableError("gone", sandbox_id="sb-1", status="HTTP 503", retryable=False)
    clone = pickle.loads(pickle.dumps(err))  # e.g. raised inside a Ray worker
    assert type(clone) is SandboxUnavailableError
    assert (str(clone), clone.sandbox_id, clone.status, clone.retryable) == (
        "gone", "sb-1", "HTTP 503", False)


def test_command_contract():
    # Strings always go through bash, so a typo exits 127 instead of failing to start.
    assert command_to_argv("lss") == ["bash", "-c", "lss"]
    assert command_to_argv("ls -la | wc -l") == ["bash", "-c", "ls -la | wc -l"]
    assert command_to_argv(["ls", "-la"]) == ["ls", "-la"]
    with pytest.raises(ValueError):
        command_to_argv([])


class LocalRuntime(RuntimeGuestHook):
    """Runs commands on this machine; stands in for a guest."""

    def exec(self, command, cwd="/testbed", env=None, timeout_s=120.0):
        try:
            p = subprocess.run(command_to_argv(command), cwd=cwd or None, capture_output=True,
                               text=True, timeout=timeout_s)
        except subprocess.TimeoutExpired:
            return ExecResult(exit_code=None, stdout="", stderr="", timed_out=True)
        return ExecResult(exit_code=p.returncode, stdout=p.stdout, stderr=p.stderr)

    def stream_process(self, argv, cwd="/testbed", env=None):
        raise NotImplementedError

    def write_file(self, path, content):
        write_file_via_exec(self, path, content)

    def read_file_bytes(self, path):
        return read_file_via_exec(self, path)

    def open_session(self):
        raise NotImplementedError


@pytest.mark.parametrize("name", [
    "plain.txt",
    "dir with space/it's \"quoted\" $(echo hi) `x`;.txt",
    "-leading-dash.txt",
])
def test_file_helpers_round_trip_with_hostile_paths(tmp_path, name):
    rt = LocalRuntime()
    target = tmp_path / "nested" / name
    payload = bytes(range(256)) * 4
    rt.write_file(str(target), payload)
    assert target.read_bytes() == payload  # exact name: quoting held
    assert rt.read_file_bytes(str(target)) == payload


def test_read_missing_file_raises_file_not_found(tmp_path):
    with pytest.raises(FileNotFoundError):
        LocalRuntime().read_file_bytes(str(tmp_path / "missing.txt"))


def test_read_unreadable_path_is_not_file_not_found(tmp_path):
    # A directory exists but cannot be base64'd: a command failure, not "missing".
    with pytest.raises(CommandExecutionError):
        LocalRuntime().read_file_bytes(str(tmp_path))


def test_file_helpers_surface_timeouts():
    # The old helpers reported a timed-out read as FileNotFoundError.
    rt = MockRuntimeHook()
    rt.set_response("base64", ExecResult(exit_code=None, stdout="", stderr="", timed_out=True))
    with pytest.raises(CommandTimeoutError):
        write_file_via_exec(rt, "/tmp/x", b"data")
    with pytest.raises(CommandTimeoutError):
        read_file_via_exec(rt, "/tmp/x")
