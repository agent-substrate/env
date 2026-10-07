"""SWE-bench scoring: rewards come only from agent outcomes, never from infra failures."""

import pytest

from ate_env import ExecResult, FleetConfig, SandboxFleet, SandboxUnavailableError
from ate_env.adapters.swebench import SWEBENCH_SAMPLE_TASK, SweBenchAdapter

TEST_CMD = SWEBENCH_SAMPLE_TASK["test_cmd"]


@pytest.fixture
def handle():
    fleet = SandboxFleet(FleetConfig(backend="mock"))
    h = fleet.acquire(SweBenchAdapter.to_task(SWEBENCH_SAMPLE_TASK))
    yield h
    h.release()


def test_patch_that_does_not_apply_scores_zero_and_skips_tests(handle):
    # Previously the tests still ran, so a garbage patch scored 1.0 whenever
    # the tests already passed at the base commit.
    handle.runtime.set_response(
        "git apply", ExecResult(exit_code=1, stdout="", stderr="patch does not apply"))
    res = SweBenchAdapter.evaluate(handle, "garbage", test_cmd=TEST_CMD)
    assert (res["applied"], res["passed"], res["reward"]) == (False, False, 0.0)
    assert not any("pytest" in c for c in handle.runtime.executed_commands)


def test_test_timeout_scores_zero_and_is_reported(handle):
    handle.runtime.set_response(
        "pytest", ExecResult(exit_code=None, stdout="", stderr="", timed_out=True))
    res = SweBenchAdapter.evaluate(handle, "diff", test_cmd=TEST_CMD)
    assert res["applied"] and res["timed_out"] and res["reward"] == 0.0


def test_infrastructure_errors_are_raised_not_scored(handle):
    handle.runtime.set_response("pytest", SandboxUnavailableError("router 503"))
    with pytest.raises(SandboxUnavailableError):
        SweBenchAdapter.evaluate(handle, "diff", test_cmd=TEST_CMD)
