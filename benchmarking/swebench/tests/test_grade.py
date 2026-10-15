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

"""Tests for the in-sandbox grader: python3 -m pytest benchmarking/swebench/tests."""

import importlib.util
import pathlib

ROOT = pathlib.Path(__file__).resolve().parents[3]
GRADER = ROOT / "internal/benchmarking/swebench/trajectories/swebench-grade.py"

spec = importlib.util.spec_from_file_location("grader", GRADER)
grader = importlib.util.module_from_spec(spec)
spec.loader.exec_module(grader)


def wrap(body, exit_code=0):
    return f"+ : '>>>>> Start Test Output'\n{body}\n+ : '>>>>> End Test Output'\n>>>>> Test Exit Code: {exit_code}\n"


PYTEST_LOG = """\
============================= test session starts ==============================
PASSED astropy/units/tests/test_a.py::test_one
PASSED astropy/units/tests/test_a.py::test_param[log(photon s-1)]
FAILED astropy/units/tests/test_a.py::test_unrelated - AssertionError: boom
XFAIL astropy/units/tests/test_a.py::test_known
SKIPPED [3] astropy/units/tests/test_a.py:12: no network
========================= 1 failed, 2 passed in 0.42s ==========================
"""


def test_pytest_v2_resolves_despite_unrelated_failure():
    inst = {
        "log_parser": "parse_log_astropy",
        "eval_type": "pass_and_fail",
        "fail_to_pass": ["astropy/units/tests/test_a.py::test_one"],
        "pass_to_pass": ["astropy/units/tests/test_a.py::test_known"],
    }
    rep = grader.grade(inst, wrap(PYTEST_LOG, exit_code=1))
    assert rep["valid"] and rep["resolved"], rep


def test_pytest_truncated_parametrized_id_prefix_matches():
    inst = {
        "log_parser": "parse_log_pytest",
        "fail_to_pass": ["astropy/units/tests/test_a.py::test_param[log(photon"],
        "pass_to_pass": [],
    }
    assert grader.grade(inst, wrap(PYTEST_LOG))["resolved"]


def test_f2p_failed_is_not_resolved():
    inst = {
        "log_parser": "parse_log_pytest",
        "fail_to_pass": ["astropy/units/tests/test_a.py::test_unrelated"],
        "pass_to_pass": [],
    }
    rep = grader.grade(inst, wrap(PYTEST_LOG, exit_code=1))
    assert rep["valid"] and not rep["resolved"] and rep["f2p"] == "0/1"


def test_skipped_f2p_is_not_resolved_but_skipped_p2p_is_maintained():
    log = "SKIPPED tests/test_x.py::test_a\nPASSED tests/test_x.py::test_b\n"
    inst = {"log_parser": "parse_log_pytest", "fail_to_pass": ["tests/test_x.py::test_a"], "pass_to_pass": []}
    assert not grader.grade(inst, wrap(log))["resolved"]
    inst = {"log_parser": "parse_log_pytest", "fail_to_pass": ["tests/test_x.py::test_b"], "pass_to_pass": ["tests/test_x.py::test_a"]}
    assert grader.grade(inst, wrap(log))["resolved"]


DJANGO_LOG = """\
test_loader (migrations.test_loader.LoaderTests) ... ok
test_namespace (migrations.test_loader.LoaderTests)
Some docstring here ... ok
test_broken (migrations.test_loader.LoaderTests) ... FAIL
test_skipped (migrations.test_loader.LoaderTests) ... skipped 'no db'
test_err (migrations.test_loader.LoaderTests) ... ERROR
----------------------------------------------------------------------
Ran 5 tests in 0.1s
FAILED (failures=1, errors=1)
"""


def test_django_parser_statuses():
    sm = grader.parse_django(DJANGO_LOG)
    assert sm["test_loader (migrations.test_loader.LoaderTests)"] == "PASSED"
    # A docstring on the next line is what the harness records; the port keeps
    # that behavior so verdicts match the harness, brittle as it is.
    assert sm["Some docstring here"] == "PASSED"
    assert sm["test_broken (migrations.test_loader.LoaderTests)"] == "FAILED"
    assert sm["test_skipped (migrations.test_loader.LoaderTests)"] == "SKIPPED"
    assert sm["test_err (migrations.test_loader.LoaderTests)"] == "ERROR"


def test_django_resolution():
    inst = {
        "log_parser": "parse_log_django",
        "fail_to_pass": ["test_loader (migrations.test_loader.LoaderTests)"],
        "pass_to_pass": ["test_skipped (migrations.test_loader.LoaderTests)"],
    }
    assert grader.grade(inst, wrap(DJANGO_LOG, exit_code=1))["resolved"]


def test_sympy_parser():
    log = "test_one ok\ntest_two F\ntest_three E\n________ sympy/core/tests/test_x.py:test_four ________\n"
    sm = grader.parse_sympy(log)
    assert sm == {"test_one": "PASSED", "test_two": "FAILED", "test_three": "ERROR",
                  "sympy/core/tests/test_x.py:test_four": "FAILED"}


def test_pytest_options_normalizes_paths():
    log = "PASSED tests/test_a.py::test_file[/tmp/xyz/data.txt]\n"
    sm = grader.parse_pytest_options(log)
    assert sm == {"tests/test_a.py::test_file[/data.txt]": "PASSED"}


def test_invalid_runs():
    inst = {"log_parser": "parse_log_pytest", "fail_to_pass": ["t"], "pass_to_pass": []}
    assert not grader.grade(inst, ">>>>> Patch Apply Failed\n" + wrap("PASSED t"))["valid"]
    assert not grader.grade(inst, "PASSED t\n")["valid"]
    # Nonzero exit with a log that shows no failure means the log is not the run.
    assert not grader.grade(inst, wrap("PASSED t", exit_code=1))["valid"]


def test_unknown_parser_exits():
    import pytest
    with pytest.raises(SystemExit):
        grader.grade({"log_parser": "nope"}, wrap(""))
