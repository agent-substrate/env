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

import collections
import time
import uuid
from typing import Dict, List, Optional, Set

from ..exceptions import SandboxStartError
from ..types import EnvironmentSpec, RawSandboxInstance
from .base import BackendDriver


class MockBackendDriver(BackendDriver):
    """In-memory mock backend driver for hermetic unit testing and PoC simulation."""

    def __init__(self, fail_preflight: bool = False, fail_acquire: bool = False):
        self.fail_preflight = fail_preflight
        self.fail_acquire = fail_acquire
        self.templates: Dict[str, EnvironmentSpec] = {}
        self.warm_pools: Dict[str, int] = collections.defaultdict(int)
        self.instances: Dict[str, RawSandboxInstance] = {}
        self.reaped_runs: Set[str] = set()

    def preflight(self) -> None:
        if self.fail_preflight:
            raise RuntimeError("Mock preflight failed: cluster unreachable")

    def ensure_template(self, env: EnvironmentSpec) -> str:
        tid = env.template_key()
        self.templates[tid] = env
        return tid

    def warm_pool(self, template_id: str, replicas: int, wait: bool = True) -> None:
        self.warm_pools[template_id] = replicas

    def unwarm_pool(self, template_id: str) -> None:
        self.warm_pools[template_id] = 0

    def acquire(self, template_id: str, run_id: str, timeout_s: float = 180.0) -> RawSandboxInstance:
        if self.fail_acquire:
            raise SandboxStartError("Mock acquire failed: capacity exhausted")

        inst_id = f"mock-sb-{uuid.uuid4().hex[:8]}"
        inst = RawSandboxInstance(
            instance_id=inst_id,
            endpoint=f"http://127.0.0.1:8000/{inst_id}",
            template_id=template_id,
            run_id=run_id,
            status="RUNNING",
            metadata={"created_at": time.time(), "template": template_id}
        )
        self.instances[inst_id] = inst
        return inst

    def release(self, instance_id: str, recycle: bool = False) -> None:
        if instance_id in self.instances:
            if not recycle:
                del self.instances[instance_id]

    def reap(self, run_id: str) -> int:
        self.reaped_runs.add(run_id)
        to_del = [iid for iid, inst in self.instances.items() if inst.run_id == run_id]
        for iid in to_del:
            del self.instances[iid]
        return len(to_del)
