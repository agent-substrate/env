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

"""Exec result contract, strict-mode errors, command contract, and file helpers."""

import pickle

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
    command_to_argv,
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
    res = handle.exec("failing-cmd", check=False)
    assert res.exit_code == 2
    assert res.stderr == "boom"


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


@pytest.mark.parametrize("name", [
    "plain.txt",
    "dir with space/it's \"quoted\" $(echo hi) `x`;.txt",
    "-leading-dash.txt",
])
def test_mock_runtime_file_round_trip(name):
    rt = MockRuntimeHook()
    target = f"/tmp/nested/{name}"
    payload = bytes(range(256)) * 4
    rt.write_file(target, payload)
    assert rt.read_file_bytes(target) == payload


def test_read_missing_file_raises_file_not_found():
    with pytest.raises(FileNotFoundError):
        MockRuntimeHook().read_file_bytes("/missing.txt")


def test_fleet_step_timeout_applies_to_handle():
    fleet = SandboxFleet(FleetConfig(backend="mock", step_timeout_s=42.0))
    h = fleet.acquire("t1")
    assert h.default_timeout_s == 42.0
    h.release()
