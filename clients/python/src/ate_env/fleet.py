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
import logging
import threading
import uuid
from typing import Any, Callable, Dict, List, Optional, Set, Tuple

from .backend.base import BackendDriver
from .backend.mock import MockBackendDriver
from .backend.substrate import SubstrateBackendDriver
from .config import FleetConfig
from .exceptions import SandboxStartError
from .handle import SandboxHandle
from .runtime.base import RuntimeGuestHook
from .runtime.mock import MockRuntimeHook
from .runtime.substrate_router import SubstrateRouterRuntime
from .strategies import STRATEGIES
from .types import (
    DataPlaneEndpoint,
    EnvironmentSpec,
    FleetPlan,
    PlacementSpec,
    PlanEntry,
    RawSandboxInstance,
    Task,
)

logger = logging.getLogger("sandbox_sdk.fleet")


class SandboxFleet:
    """
    Unified Sandbox Fleet Orchestrator.
    
    Coordinates preflight, planning, warm-pool sizing, lifecycle management,
    task acquisition, and execution strategies across any backend driver.
    """

    def __init__(self, config: Optional[FleetConfig] = None, driver: Optional[BackendDriver] = None):
        self.config = config or FleetConfig()
        self.run_id = uuid.uuid4().hex[:12]
        self._tasks: List[Task] = []
        self._image_to_template: Dict[str, str] = {}
        self._plan: Optional[FleetPlan] = None
        self._lock = threading.Lock()

        # Initialize Backend Driver
        if driver is not None:
            self.backend = driver
        elif self.config.backend == "mock":
            self.backend = MockBackendDriver()
        elif self.config.backend == "substrate":
            token = self.config.auth_token
            self.backend = SubstrateBackendDriver(
                api_endpoint=self.config.endpoint,
                router_url=self.config.router_url or self.config.endpoint,
                atespace=self.config.tenancy,
                worker_family=self.config.worker_family or "c2",
                auth_token=token.get_secret_value() if token else None,
                grpc_target=self.config.grpc_endpoint,
            )
        else:
            raise NotImplementedError(f"Backend '{self.config.backend}' is not yet supported or requires extras")

    @property
    def tasks(self) -> List[Task]:
        return self._tasks

    def load_tasks(self, tasks: List[Any]) -> None:
        """Load tasks into the fleet (supports list of Task or dicts)."""
        normalized: List[Task] = []
        for t in tasks:
            if isinstance(t, Task):
                normalized.append(t)
            elif isinstance(t, dict):
                normalized.append(Task(
                    id=str(t.get("task_id") or t.get("id")),
                    image=t["image"],
                    metadata=t
                ))
            else:
                raise ValueError(f"Unsupported task item type: {type(t)}")
        self._tasks = normalized
        logger.info("Fleet loaded %d tasks (%d unique images)",
                    len(self._tasks), len(self.image_counts()))

    def image_counts(self) -> Dict[str, int]:
        """Return task count per unique image."""
        counts: Dict[str, int] = collections.defaultdict(int)
        for t in self._tasks:
            counts[t.image] += 1
        return dict(counts)

    def preflight(self) -> None:
        """Run backend connectivity, permissions, and capacity checks."""
        self.backend.preflight()

    def plan(self) -> FleetPlan:
        """Compute provisioning sizing for all tasks."""
        entries: List[PlanEntry] = []
        counts = self.image_counts()
        for img, task_count in counts.items():
            template_id = self._ensure_template_for_image(img)
            # Size warm pool to min(task_count, max_warmpool_replicas)
            replicas = min(task_count, self.config.max_warmpool_replicas)
            entries.append(PlanEntry(
                image=img,
                template_id=template_id,
                replicas=replicas,
                tasks=task_count,
            ))
        self._plan = FleetPlan(entries)
        return self._plan

    def setup(self) -> None:
        """Run preflight, plan, and pre-warm all planned images."""
        self.preflight()
        plan = self.plan()
        for entry in plan.entries:
            if entry.replicas > 0:
                self.backend.warm_pool(entry.template_id, entry.replicas, wait=True)

    def warm_images(self, images: List[str], replicas: Optional[int] = None, wait: bool = True) -> None:
        """Warm up pools for specific images (used by windowed strategies)."""
        rep = replicas if replicas is not None else self.config.max_warmpool_replicas
        for img in images:
            template_id = self._ensure_template_for_image(img)
            self.backend.warm_pool(template_id, rep, wait=wait)

    def unwarm_image(self, image: str) -> None:
        """Drain warm pool for a specific image."""
        template_id = self._image_to_template.get(image)
        if template_id:
            self.backend.unwarm_pool(template_id)

    def acquire(self, task: Task | str, timeout_s: Optional[float] = None) -> SandboxHandle:
        """
        Acquire a live sandbox bound to a specific task.
        
        Claims from warm pool or golden snapshot, initializes the appropriate
        RuntimeGuestHook, and wraps in SandboxHandle.
        """
        task_obj: Task
        if isinstance(task, str):
            # Look up task by id or treat as ad-hoc
            found = next((t for t in self._tasks if t.id == task), None)
            if found:
                task_obj = found
            else:
                task_obj = Task(id=task, image="default")
        else:
            task_obj = task

        template_id = self._ensure_template_for_image(task_obj.image)
        to_s = timeout_s or self.config.acquire_timeout_s
        raw_inst = self.backend.acquire(template_id, self.run_id, timeout_s=to_s)

        try:
            runtime, data_plane = self._build_runtime(raw_inst)
        except Exception:
            # Never leak a claimed sandbox we cannot talk to.
            try:
                self.backend.release(raw_inst.instance_id, recycle=False)
            except Exception:  # pragma: no cover - best effort cleanup
                logger.warning("Failed to release sandbox %s after runtime setup error",
                               raw_inst.instance_id, exc_info=True)
            raise

        return SandboxHandle(
            sandbox_id=raw_inst.instance_id,
            endpoint=data_plane.address if data_plane else raw_inst.endpoint,
            task=task_obj,
            run_id=self.run_id,
            backend=self.backend,
            runtime=runtime,
            data_plane=data_plane,
        )

    def _build_runtime(
        self, raw_inst: RawSandboxInstance
    ) -> Tuple[RuntimeGuestHook, Optional[DataPlaneEndpoint]]:
        """Build the RuntimeGuestHook from this sandbox's own data-plane coordinates."""
        if isinstance(self.backend, MockBackendDriver):
            return MockRuntimeHook(), None

        name = self.config.data_plane
        ep = raw_inst.data_planes.get(name)
        if ep is None:
            raise SandboxStartError(
                f"{type(self.backend).__name__} returned no '{name}' data plane for sandbox "
                f"{raw_inst.instance_id} (available: {sorted(raw_inst.data_planes) or 'none'})")
        if name == "router":
            return SubstrateRouterRuntime(router_url=ep.address, headers=ep.headers), ep
        if name in ("grpc", "ate_env"):
            from .runtime.substrate_env_client import SubstrateEnvClientRuntime
            target = dict(ep.headers).get("ate-target-actor", "")
            env_id = target.partition("/")[2] or raw_inst.instance_id
            atespace = target.partition("/")[0] or self.config.tenancy
            return SubstrateEnvClientRuntime(endpoint=ep.address, env_id=env_id, atespace=atespace), ep
        raise SandboxStartError(f"Unsupported data plane runtime '{name}'")

    def release(self, handle: SandboxHandle) -> None:
        """Release sandbox instance back to backend and close its runtime."""
        handle.release()

    def teardown(self) -> None:
        """Teardown all resources provisioned by this fleet's run_id."""
        self.backend.reap(self.run_id)

    def run(self, process_fn: Callable[[Task, SandboxHandle], Any],
            concurrency: Optional[int] = None) -> List[Any]:
        """Execute all tasks using the configured strategy."""
        strat_fn = STRATEGIES.get(self.config.strategy)
        if not strat_fn:
            raise ValueError(f"Unknown strategy: {self.config.strategy}")
        c = concurrency or min(self.config.max_concurrent, len(self._tasks))
        return strat_fn(self, process_fn, max(1, c))

    def _ensure_template_for_image(self, image: str) -> str:
        with self._lock:
            if image in self._image_to_template:
                return self._image_to_template[image]
            env_spec = EnvironmentSpec(
                image=image,
                placement=PlacementSpec(
                    node_selector=self.config.node_selector,
                    tolerations=self.config.tolerations,
                    worker_family=self.config.worker_family,
                ),
            )
            template_id = self.backend.ensure_template(env_spec)
            self._image_to_template[image] = template_id
            return template_id
