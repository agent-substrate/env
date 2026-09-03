from __future__ import annotations

from datetime import datetime, timezone

from ate_env import EnvironmentStatus, OutputSource, ProcessStatus
from ate_env._gen.ateenv.v1 import env_pb2, guest_pb2
from ate_env.types import _environment_info_from_pb, _process_info_from_pb


def test_process_status_values_match_proto():
    for member in ProcessStatus:
        assert guest_pb2.ProcessStatus.Value(f"PROCESS_STATUS_{member.name}") == member


def test_log_source_values_match_proto():
    for member in OutputSource:
        assert guest_pb2.OutputSource.Value(f"OUTPUT_SOURCE_{member.name}") == member


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


def test_process_info_without_timestamps():
    pb = guest_pb2.Process(
        process_id="p1", status=guest_pb2.PROCESS_STATUS_RUNNING, exit_code=0
    )
    info = _process_info_from_pb(pb)
    assert info.process_id == "p1"
    assert info.status == ProcessStatus.RUNNING
    assert info.started_at is None
    assert info.finished_at is None


def test_process_info_timestamps_are_utc():
    started = datetime(2026, 8, 25, 12, 0, 0, tzinfo=timezone.utc)
    finished = datetime(2026, 8, 25, 12, 0, 5, 500_000, tzinfo=timezone.utc)
    pb = guest_pb2.Process(
        process_id="p1", status=guest_pb2.PROCESS_STATUS_COMPLETED, exit_code=0
    )
    pb.started_at.FromDatetime(started)
    pb.finished_at.FromDatetime(finished)
    info = _process_info_from_pb(pb)
    assert info.started_at == started
    assert info.finished_at == finished
    assert info.started_at.tzinfo == timezone.utc
    assert info.finished_at.tzinfo == timezone.utc
