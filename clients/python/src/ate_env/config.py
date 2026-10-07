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

from typing import Any, Dict, List, Literal, Optional
from pydantic import BaseModel, ConfigDict, Field, SecretStr, field_validator, model_validator

from .backend.substrate import validate_grpc_target

BackendType = Literal["substrate", "kubernetes", "mock"]
StrategyType = Literal["none", "naive", "sliding", "pipelined"]

# Atespaces (Substrate) and namespaces (Kubernetes) are DNS-1123 labels.
_DNS1123_LABEL = r"^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$"


class FleetConfig(BaseModel):
    """Configuration for SandboxFleet and associated BackendDriver.

    Unknown fields are rejected (``extra="forbid"``) so misspelled or
    unsupported options fail loudly instead of being silently ignored.
    """

    model_config = ConfigDict(extra="forbid")

    backend: BackendType = "substrate"
    endpoint: str = "http://localhost:8080"
    router_url: Optional[str] = "http://localhost:8000"
    # In-guest gRPC address. May contain {actor_id} / {atespace} placeholders
    # for per-sandbox addresses; otherwise sandboxes are selected by the
    # ate-target-actor call metadata. Required when data_plane="grpc" or "ate_env".
    grpc_endpoint: Optional[str] = None
    data_plane: Literal["router", "grpc", "ate_env"] = "router"
    tenancy: str = Field("default", pattern=_DNS1123_LABEL)  # atespace for Substrate, namespace for Kubernetes
    strategy: StrategyType = "sliding"

    # Sizing & Rollout parameters
    batch_size: int = Field(8, ge=1)
    num_generations: int = Field(8, ge=1)  # e.g., GRPO group size G=8
    max_concurrent: int = Field(64, ge=1)
    max_warmpool_replicas: int = Field(4, ge=0)
    window_size: Optional[int] = Field(None, ge=1)

    # Hardware & placement
    worker_family: Optional[str] = "c2"  # Intel Cascade Lake, AMD Milan, etc.
    node_selector: Dict[str, str] = Field(default_factory=dict)
    tolerations: List[Dict[str, Any]] = Field(default_factory=list)
    labels: Dict[str, str] = Field(default_factory=dict)

    # Timeouts & Breakers
    acquire_timeout_s: float = Field(180.0, gt=0)
    ready_timeout_s: float = Field(180.0, gt=0)
    step_timeout_s: float = Field(120.0, gt=0)
    # Bearer token for the data plane. SecretStr keeps it out of repr/logs;
    # model_dump() keeps the SecretStr object so from_dict() round-trips.
    auth_token: Optional[SecretStr] = None

    @field_validator("grpc_endpoint")
    @classmethod
    def _check_grpc_endpoint(cls, v: Optional[str]) -> Optional[str]:
        if v:
            validate_grpc_target(v)
        return v

    @model_validator(mode="after")
    def _check_data_plane(self) -> "FleetConfig":
        if self.data_plane == "grpc" and not self.grpc_endpoint:
            raise ValueError("data_plane='grpc' requires grpc_endpoint")
        return self

    @classmethod
    def from_dict(cls, data: Dict[str, Any]) -> "FleetConfig":
        return cls.model_validate(data)
