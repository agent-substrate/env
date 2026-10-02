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

import io
from typing import Dict, Iterator, List, Optional, Union

from ..types import ExecResult
from .base import InteractiveSession, RuntimeGuestHook


class MockInteractiveSession(InteractiveSession):
    def __init__(self, hook: "MockRuntimeHook"):
        self.hook = hook
        self.is_open = True

    def run(self, command: str | List[str], timeout_s: Optional[float] = None) -> str:
        if not self.is_open:
            raise RuntimeError("Session closed")
        res = self.hook.exec(command, timeout_s=timeout_s or 10.0)
        return res.stdout

    def close(self) -> None:
        self.is_open = False


class MockRuntimeHook(RuntimeGuestHook):
    """In-memory mock guest hook implementing file operations and command responses."""

    def __init__(self):
        self.files: Dict[str, bytes] = {}
        self.executed_commands: List[str] = []
        self.custom_responses: Dict[str, Union[ExecResult, BaseException]] = {}

    def set_response(self, command_substring: str,
                     result: Union[ExecResult, BaseException]) -> None:
        """Preset a result, or an exception (e.g. SandboxUnavailableError) to raise."""
        self.custom_responses[command_substring] = result

    def exec(self, command: str | List[str], cwd: str = "/testbed",
             env: Optional[Dict[str, str]] = None, timeout_s: float = 120.0) -> ExecResult:
        cmd_str = command if isinstance(command, str) else " ".join(command)
        self.executed_commands.append(cmd_str)

        # Check for preset custom responses
        for sub, res in self.custom_responses.items():
            if sub in cmd_str:
                if isinstance(res, BaseException):
                    raise res
                return res

        # Default standard behaviors for common RL / SWE-bench commands
        if "pytest" in cmd_str:
            if "test_version" in cmd_str:
                return ExecResult(exit_code=0, stdout="=== 1 passed in 0.42s ===\n", stderr="")
            return ExecResult(exit_code=0, stdout="=== ALL TESTS PASSED ===\n", stderr="")

        if "git apply" in cmd_str or "patch -p1" in cmd_str:
            return ExecResult(exit_code=0, stdout="Applied patch successfully\n", stderr="")

        if "git -C /testbed log" in cmd_str:
            return ExecResult(exit_code=0, stdout="READY mock-pod 1234abc Base commit\n", stderr="")

        return ExecResult(exit_code=0, stdout=f"Mock exec ok: {cmd_str}\n", stderr="")

    def stream_process(self, argv: List[str], cwd: str = "/testbed",
                       env: Optional[Dict[str, str]] = None) -> Iterator[str]:
        cmd_str = " ".join(argv)
        yield f"Process started: {cmd_str}\n"
        yield "Process completed.\n"

    def write_file(self, path: str, content: bytes | str) -> None:
        raw = content.encode("utf-8") if isinstance(content, str) else content
        self.files[path] = raw

    def read_file_bytes(self, path: str) -> bytes:
        if path not in self.files:
            raise FileNotFoundError(f"Mock file not found: {path}")
        return self.files[path]

    def open_session(self) -> InteractiveSession:
        return MockInteractiveSession(self)
