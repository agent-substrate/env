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

from datetime import datetime, timezone

import pytest

from ate_env import EnvironmentStatus, ProcessOutput, ProcessState, Signal
from ate_env._gen.ateenv.v1alpha import env_pb2, guest_pb2
from ate_env.types import (
    _environment_info_from_pb,
    _process_info_from_pb,
    _process_output_from_pb,
)


def test_process_state_values_match_proto():
    for member in ProcessState:
        assert guest_pb2.ProcessState.Value(f"PROCESS_STATE_{member.name}") == member
    assert len(ProcessState) == len(guest_pb2.ProcessState.keys())


def test_signal_values_match_proto():
    for member in Signal:
        assert guest_pb2.Signal.Value(f"SIGNAL_{member.name}") == member
    # Every proto signal except UNSPECIFIED has a Python member.
    assert {f"SIGNAL_{m.name}" for m in Signal} == set(guest_pb2.Signal.keys()) - {
        "SIGNAL_UNSPECIFIED"
    }


def test_environment_info_without_template():
    pb = env_pb2.Environment(
        id="dev1", atespace="team-a", status=env_pb2.ENVIRONMENT_STATUS_SUSPENDED
    )
    info = _environment_info_from_pb(pb)
    assert info.id == "dev1"
    assert info.atespace == "team-a"
    assert info.template is None
    assert info.status == EnvironmentStatus.SUSPENDED


def test_environment_info_with_template():
    pb = env_pb2.Environment(
        id="dev1",
        atespace="default",
        template=env_pb2.Template(name="big-env", atespace="ns1"),
        status=env_pb2.ENVIRONMENT_STATUS_RUNNING,
    )
    info = _environment_info_from_pb(pb)
    assert info.template is not None
    assert (info.template.name, info.template.atespace) == ("big-env", "ns1")


def test_process_info_running():
    pb = guest_pb2.Process(
        process_id="p1", command=["sleep", "1"], pid=42, state=guest_pb2.PROCESS_STATE_RUNNING
    )
    info = _process_info_from_pb(pb)
    assert info.process_id == "p1"
    assert info.command == ("sleep", "1")
    assert info.pid == 42
    assert info.state == ProcessState.RUNNING and info.running
    assert info.started_at is None
    assert info.finished_at is None


def test_process_info_exited():
    pb = guest_pb2.Process(process_id="p1", state=guest_pb2.PROCESS_STATE_EXITED, exit_code=143)
    info = _process_info_from_pb(pb)
    assert not info.running
    assert info.exit_code == 143


def test_process_output_variants():
    assert _process_output_from_pb(guest_pb2.ProcessOutput(stdout=b"a")) == ProcessOutput(stdout=b"a")
    assert _process_output_from_pb(guest_pb2.ProcessOutput(stderr=b"b")) == ProcessOutput(stderr=b"b")
    out = _process_output_from_pb(
        guest_pb2.ProcessOutput(
            exit=guest_pb2.Process(process_id="p", state=guest_pb2.PROCESS_STATE_EXITED, exit_code=9)
        )
    )
    assert out.stdout is None and out.stderr is None
    assert out.exit is not None and out.exit.exit_code == 9
    with pytest.raises(ValueError):
        _process_output_from_pb(guest_pb2.ProcessOutput())


def test_process_info_timestamps_are_utc():
    started = datetime(2026, 8, 25, 12, 0, 0, tzinfo=timezone.utc)
    finished = datetime(2026, 8, 25, 12, 0, 5, 500_000, tzinfo=timezone.utc)
    pb = guest_pb2.Process(process_id="p1", state=guest_pb2.PROCESS_STATE_EXITED, exit_code=0)
    pb.started_at.FromDatetime(started)
    pb.finished_at.FromDatetime(finished)
    info = _process_info_from_pb(pb)
    assert info.started_at == started
    assert info.finished_at == finished
    assert info.started_at.tzinfo == timezone.utc
    assert info.finished_at.tzinfo == timezone.utc
