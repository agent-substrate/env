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

import collections
import logging
import threading
import uuid
from collections.abc import Callable, Sequence
from typing import Any

from .backend.base import BackendDriver
from .backend.mock import MockBackendDriver
from .backend.substrate import SubstrateBackendDriver
from .config import FleetConfig
from .handle import SandboxHandle
from .runtime.base import RuntimeGuestHook
from .strategies import STRATEGIES
from .types import (
    DataPlaneEndpoint,
    FleetPlan,
    PlanEntry,
    RawSandboxInstance,
    Task,
)

logger = logging.getLogger("ate_env.fleet")


class SandboxFleet:
    """Unified Sandbox Fleet Orchestrator.

    Coordinates preflight, planning, warm-pool sizing, lifecycle management,
    task acquisition, and execution strategies across any backend driver.
    """

    def __init__(self, config: FleetConfig | None = None, driver: BackendDriver | None = None):
        self.config = config or FleetConfig()
        self.run_id = uuid.uuid4().hex[:12]
        self._tasks: list[Task] = []
        self._image_to_template: dict[str, tuple[str, str | None]] = {}
        self._plan: FleetPlan | None = None
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
                atespace=self.config.tenancy,
                worker_family=self.config.worker_family or "c2",
                auth_token=token.get_secret_value() if token else None,
                image_templates=self.config.image_templates,
                default_template=self.config.template if self.config.template is not None else "default-template",
                labels=self.config.labels,
                acquire_timeout_s=self.config.acquire_timeout_s,
                ready_timeout_s=self.config.ready_timeout_s,
            )
        else:
            raise NotImplementedError(f"Backend '{self.config.backend}' is not yet supported or requires extras")

    def __enter__(self) -> SandboxFleet:
        return self

    def __exit__(self, exc_type: Any, exc_val: Any, exc_tb: Any) -> None:
        try:
            self.close()
        except Exception:
            if exc_type is None:
                raise

    def close(self) -> None:
        teardown_err: Exception | None = None
        try:
            self.teardown()
        except Exception as e:
            teardown_err = e
        finally:
            if hasattr(self.backend, "close"):
                try:
                    self.backend.close()
                except Exception as be:
                    if teardown_err is None:
                        teardown_err = be
        if teardown_err is not None:
            raise teardown_err

    @property
    def tasks(self) -> list[Task]:
        return self._tasks

    def load_tasks(self, tasks: list[Any]) -> None:
        """Load tasks into the fleet (supports list of Task or dicts)."""
        normalized: list[Task] = []
        for t in tasks:
            if isinstance(t, Task):
                normalized.append(t)
            elif isinstance(t, dict):
                normalized.append(Task(
                    id=str(t.get("task_id") or t.get("id")),
                    image=t["image"],
                    metadata=t,
                    template=t.get("template"),
                ))
            else:
                raise ValueError(f"Unsupported task item type: {type(t)}")
        self._tasks = normalized
        logger.info("Fleet loaded %d tasks (%d unique images)",
                    len(self._tasks), len(self.image_counts()))

    def image_counts(self) -> dict[str, int]:
        """Return task count per unique image."""
        counts: dict[str, int] = collections.defaultdict(int)
        for t in self._tasks:
            counts[t.image] += 1
        return dict(counts)

    def preflight(self) -> None:
        """Run backend connectivity, permissions, and capacity checks."""
        self.backend.preflight()

    def plan(self) -> FleetPlan:
        """Compute provisioning sizing for all tasks."""
        entries: list[PlanEntry] = []
        counts = self.image_counts()
        for img, task_count in counts.items():
            template_id, rootfs_image = self._ensure_template_for_image(img)
            # Size warm pool to min(task_count, max_warmpool_replicas)
            replicas = min(task_count, self.config.max_warmpool_replicas)
            entries.append(PlanEntry(
                image=img,
                template_id=template_id,
                replicas=replicas,
                tasks=task_count,
                rootfs_image=rootfs_image,
            ))
        self._plan = FleetPlan(entries)
        return self._plan

    def setup(self, tasks: list[Any] | None = None) -> None:
        """Run preflight, plan, and pre-warm all planned images."""
        if tasks is not None:
            self.load_tasks(tasks)
        self.preflight()
        plan = self.plan()
        for entry in plan.entries:
            if entry.replicas > 0:
                self.backend.warm_pool(
                    entry.template_id,
                    entry.replicas,
                    wait=True,
                    rootfs_image=getattr(entry, "rootfs_image", None),
                )

    def warm_images(self, images: list[str], replicas: int | None = None, wait: bool = True) -> None:
        """Warm up pools for specific images (used by windowed strategies)."""
        rep = replicas if replicas is not None else self.config.max_warmpool_replicas
        for img in images:
            template_id, rootfs_image = self._ensure_template_for_image(img)
            self.backend.warm_pool(template_id, rep, wait=wait, rootfs_image=rootfs_image)

    def unwarm_image(self, image: str) -> None:
        """Drain warm pool for a specific image."""
        entry = self._image_to_template.get(image)
        if entry:
            template_id, rootfs_image = entry
            self.backend.unwarm_pool(template_id, rootfs_image=rootfs_image)

    def acquire(
        self,
        task: Task | str | Sequence[Task | str],
        timeout_s: float | None = None,
    ) -> SandboxHandle | list[SandboxHandle]:
        """Acquire one or more live sandboxes bound to tasks.

        If a list or tuple of tasks is given, returns a list of SandboxHandle.
        Otherwise returns a single SandboxHandle.
        """
        if isinstance(task, (list, tuple)):
            return [self._acquire_one(t, timeout_s=timeout_s) for t in task]
        return self._acquire_one(task, timeout_s=timeout_s)

    def _acquire_one(
        self,
        task: Task | str,
        timeout_s: float | None = None,
    ) -> SandboxHandle:
        task_obj: Task
        if isinstance(task, str):
            found = next((t for t in self._tasks if t.id == task), None)
            if found:
                task_obj = found
            else:
                task_obj = Task(id=task, image="default")
        else:
            task_obj = task

        if task_obj.template:
            template_id = task_obj.template
            rootfs_image = None
        else:
            template_id, rootfs_image = self._ensure_template_for_image(task_obj.image)

        to_s = timeout_s or self.config.acquire_timeout_s
        raw_inst = self.backend.acquire(
            template_id, self.run_id, timeout_s=to_s, rootfs_image=rootfs_image
        )

        try:
            runtime, data_plane = self._build_runtime(raw_inst)
        except Exception:
            try:
                self.backend.release(raw_inst.instance_id, recycle=False)
            except Exception:
                logger.warning(
                    "Failed to release sandbox %s after runtime setup error",
                    raw_inst.instance_id,
                    exc_info=True,
                )
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
    ) -> tuple[RuntimeGuestHook, DataPlaneEndpoint | None]:
        """Build the RuntimeGuestHook from this sandbox's own data-plane coordinates."""
        if hasattr(self.backend, "build_runtime"):
            return self.backend.build_runtime(raw_inst, data_plane=self.config.data_plane)

        from .runtime.substrate_env_client import SubstrateEnvClientRuntime

        name = self.config.data_plane
        ep = raw_inst.data_planes.get(name) or raw_inst.data_planes.get("ate_env")
        client = getattr(self.backend, "client", None)
        loop_runner = getattr(self.backend, "_loop_runner", None)
        return SubstrateEnvClientRuntime(
            env_id=raw_inst.instance_id,
            endpoint=ep.address if ep else raw_inst.endpoint,
            atespace=self.config.tenancy or "ate-env",
            client=client,
            loop_runner=loop_runner,
        ), ep

    def release(self, handle: SandboxHandle | Sequence[SandboxHandle]) -> None:
        """Release sandbox instance(s) back to backend and close runtime(s)."""
        if isinstance(handle, (list, tuple)):
            for h in handle:
                h.release()
        else:
            handle.release()

    def teardown(self) -> None:
        """Teardown all resources provisioned by this fleet's run_id."""
        self.backend.reap(self.run_id)

    def run(self, process_fn: Callable[[Task, SandboxHandle], Any],
            concurrency: int | None = None) -> list[Any]:
        """Execute all tasks using the configured strategy."""
        strat_fn = STRATEGIES.get(self.config.strategy)
        if not strat_fn:
            raise ValueError(f"Unknown strategy: {self.config.strategy}")
        c = concurrency or min(self.config.max_concurrent, len(self._tasks))
        return strat_fn(self, process_fn, max(1, c))

    def _ensure_template_for_image(self, image: str) -> tuple[str, str | None]:
        with self._lock:
            if image in self._image_to_template:
                return self._image_to_template[image]
            res: tuple[str, str | None]
            if image in self.config.image_templates:
                res = (self.config.image_templates[image], None)
            else:
                base = self.config.template or "default-template"
                res = (base, image)
            self._image_to_template[image] = res
            return res
