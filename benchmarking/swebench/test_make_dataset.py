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

"""make-dataset.py's eval-script check and revision resolution, without a network."""

from __future__ import annotations

import importlib.util
import pathlib

_SPEC = importlib.util.spec_from_file_location(
    "make_dataset", pathlib.Path(__file__).with_name("make-dataset.py"))
md = importlib.util.module_from_spec(_SPEC)
_SPEC.loader.exec_module(md)

# The shape of a SWE-bench Verified row's eval_script (astropy__astropy-12907,
# shortened) and what swebench 5.x's make_test_spec turns it into.
ROW = """#!/bin/bash
set -uxo pipefail
source /opt/miniconda3/bin/activate
cd /testbed
git checkout d16bfe05a744909de4b27f5875fe0d4ed41ce607 astropy/modeling/tests/test_separable.py
git apply -v - <<'EOF_114329324912'
diff --git a/astropy/modeling/tests/test_separable.py b/astropy/modeling/tests/test_separable.py
EOF_114329324912
: '>>>>> Start Test Output'
pytest -rA astropy/modeling/tests/test_separable.py
: '>>>>> End Test Output'
git checkout d16bfe05a744909de4b27f5875fe0d4ed41ce607 astropy/modeling/tests/test_separable.py
"""

HARNESS = ROW.replace(
    "pytest -rA astropy/modeling/tests/test_separable.py\n: '>>>>> End Test Output'\n",
    "pytest -rA astropy/modeling/tests/test_separable.py\nSWEBENCH_TEST_EXIT_CODE=$?\n"
    ": '>>>>> End Test Output'\necho \">>>>> Test Exit Code: $SWEBENCH_TEST_EXIT_CODE\"\n",
)


def test_the_harness_additions_are_accepted():
    # The substring check this replaces rejected each row of the dataset.
    assert ROW.strip() not in HARNESS
    assert md.row_script_matches(ROW, HARNESS)


def test_the_header_may_be_absent_on_either_side():
    body = ROW.replace("#!/bin/bash\nset -uxo pipefail\n", "")
    assert md.row_script_matches(body, HARNESS)
    assert md.row_script_matches(ROW, HARNESS.replace("#!/bin/bash\nset -uxo pipefail\n", ""))


def test_a_changed_test_command_is_rejected():
    assert not md.row_script_matches(ROW, HARNESS.replace("pytest -rA", "pytest -rA -k nothing"))


def test_a_missing_or_extra_line_is_rejected():
    assert not md.row_script_matches(ROW, HARNESS.replace("cd /testbed\n", ""))
    assert not md.row_script_matches(ROW, HARNESS + "rm -rf /testbed\n")


def test_a_local_dataset_resolves_to_its_file_hash(tmp_path):
    f = tmp_path / "rows.jsonl"
    f.write_text('{"instance_id": "a"}\n')
    rev = md.resolve_revision(str(f), None)
    assert rev.startswith("sha256:") and len(rev) == len("sha256:") + 64
    f.write_text('{"instance_id": "b"}\n')
    assert md.resolve_revision(str(f), None) != rev
