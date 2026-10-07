# Copyright 2026 The Kubernetes Authors & Google LLC
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

from typing import Any, Dict, List
from ..types import Task

SWEBENCH_SAMPLE_TASK = {
    "task_id": "pytest-dev__pytest-5221",
    "repo": "pytest-dev/pytest",
    "base_commit": "e8ecbbdf0059c36d0bdf3cae3a39e8d47b597ab8",
    "template": "swebench-pytest-5221",
    "image": "us-central1-docker.pkg.dev/songsunny-gke-dev2/rl-genetics/sweb.eval.pytest-5221:latest",
    "problem_statement": (
        "Display fixture scope in --fixtures. Add scope information to fixture formatting."
    ),
    "test_cmd": "/opt/miniconda3/envs/testbed/bin/pytest testing/test_helpconfig.py -k test_version",
}


class SweBenchAdapter:
    """SWE-bench task converter and evaluator adapter."""

    # git apply is atomic; fall back to fuzzy patch only if it rejects the diff.
    APPLY_CMD = ("git apply --verbose /tmp/solution.patch || "
                 "patch --batch --fuzz=5 -p1 -i /tmp/solution.patch")

    @staticmethod
    def to_task(task_dict: Dict[str, Any]) -> Task:
        return Task(
            id=task_dict.get("task_id", "swebench-task"),
            image=task_dict["image"],
            metadata=task_dict,
        )

    @staticmethod
    def evaluate(handle, patch_content: str, test_cmd: str, is_mock: bool = False) -> Dict[str, Any]:
        """Apply patch inside sandbox and run evaluation test command.

        Scoring: ``reward`` is 1.0 only if the patch applied and the test
        command exited 0 within its deadline. A patch that does not apply,
        failing tests, and test timeouts are agent outcomes and score 0.0.

        ``InfrastructureError`` (sandbox unreachable, malformed response,
        command could not start) is deliberately not caught: the caller must
        retry or mask the rollout, never score it.
        """
        # 1. Write solution patch
        handle.runtime.write_file("/tmp/solution.patch", patch_content)

        # 2. Apply patch
        apply_res = handle.runtime.exec(SweBenchAdapter.APPLY_CMD, cwd="/testbed")
        if not apply_res.ok:
            return {
                "task_id": handle.task.id,
                "applied": False,
                "passed": False,
                "timed_out": apply_res.timed_out,
                "reward": 0.0,
                "logs": apply_res.stdout + "\n" + apply_res.stderr,
                "duration_s": apply_res.duration_s,
            }

        # 3. Run test command
        try:
            test_res = handle.runtime.exec(test_cmd, cwd="/testbed")
            passed = test_res.ok
            return {
                "task_id": handle.task.id,
                "applied": True,
                "passed": passed,
                "timed_out": test_res.timed_out,
                "reward": 1.0 if passed else 0.0,
                "logs": test_res.stdout + "\n" + test_res.stderr,
                "duration_s": test_res.duration_s,
            }
        finally:
            handle.runtime.exec("git checkout -- . && git clean -fd", cwd="/testbed")

