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

from abc import ABC, abstractmethod
from typing import Optional

from ..types import EnvironmentSpec, RawSandboxInstance


class BackendDriver(ABC):
    """
    Control Plane (Backend Protocol) Interface.
    
    Governs fleet provisioning, template creation, warm-pool sizing,
    instance claims, snapshotting, and run isolation / reaping.
    """

    @abstractmethod
    def preflight(self) -> None:
        """Validate cluster connectivity, permissions, storage, and worker capacity."""

    @abstractmethod
    def ensure_template(self, env: EnvironmentSpec) -> str:
        """Ensure template/golden snapshot exists. Returns unique template ID."""

    @abstractmethod
    def warm_pool(self, template_id: str, replicas: int, wait: bool = True) -> None:
        """Provision warm replicas (Ready pods in K8s or Paused Actors in Substrate)."""

    @abstractmethod
    def unwarm_pool(self, template_id: str) -> None:
        """Scale down warm pool for the template to 0."""

    @abstractmethod
    def acquire(self, template_id: str, run_id: str, timeout_s: float = 180.0) -> RawSandboxInstance:
        """Instantly claim a sandbox instance from warm pool or golden."""

    @abstractmethod
    def release(self, instance_id: str, recycle: bool = False) -> None:
        """Release or recycle instance."""

    @abstractmethod
    def reap(self, run_id: str) -> int:
        """Force delete all resources associated with a run_id."""
