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

"""Dynamic endpoint discovery and connection strategies for sandbox-sdk.

Modeled after upstream Kubernetes agent-sandbox ConnectionStrategy, replacing
hardcoded cluster domain strings with flexible in-cluster, local-tunnel,
gateway, and direct connection resolution.
"""

from __future__ import annotations

import logging
import os
import socket
import subprocess
import threading
import time
from abc import ABC, abstractmethod
from typing import Callable, Optional

logger = logging.getLogger("sandbox_sdk.connector")


class ConnectionStrategy(ABC):
    """Abstract base class for sandbox connection strategies."""

    @abstractmethod
    def connect(self) -> str:
        """Establish/resolve connection and return the base URL (e.g. 'http://host:port')."""
        pass

    @abstractmethod
    def close(self) -> None:
        """Clean up any resources (e.g. background tunnels) associated with the strategy."""
        pass


class DirectConnectionStrategy(ConnectionStrategy):
    """Direct connection using an explicitly configured URL or environment variable."""

    def __init__(self, endpoint: str):
        self.endpoint = endpoint.rstrip("/")

    def connect(self) -> str:
        return self.endpoint

    def close(self) -> None:
        pass


class InClusterConnectionStrategy(ConnectionStrategy):
    """In-cluster connectivity using dynamic Pod IP or service DNS."""

    def __init__(
        self,
        service_name: str = "atenet-router",
        namespace: str = "ate-system",
        port: int = 8080,
        cluster_domain: str = "cluster.local",
        get_pod_ip: Optional[Callable[[], Optional[str]]] = None,
    ):
        self.service_name = service_name
        self.namespace = namespace
        self.port = port
        self.cluster_domain = cluster_domain
        self._get_pod_ip = get_pod_ip

    def connect(self) -> str:
        if self._get_pod_ip:
            pod_ip = self._get_pod_ip()
            if pod_ip:
                host = f"[{pod_ip}]" if ":" in pod_ip else pod_ip
                return f"http://{host}:{self.port}"

        return f"http://{self.service_name}.{self.namespace}.svc.{self.cluster_domain}:{self.port}"

    def close(self) -> None:
        pass


class LocalTunnelConnectionStrategy(ConnectionStrategy):
    """Local development connection using dynamic kubectl port-forward."""

    def __init__(
        self,
        service_name: str = "atenet-router",
        namespace: str = "ate-system",
        remote_port: int = 8080,
        ready_timeout_s: float = 15.0,
    ):
        self.service_name = service_name
        self.namespace = namespace
        self.remote_port = remote_port
        self.ready_timeout_s = ready_timeout_s
        self.port_forward_process: Optional[subprocess.Popen] = None
        self.base_url: Optional[str] = None
        self._lock = threading.RLock()

    @staticmethod
    def _get_free_port() -> int:
        with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
            s.bind(("127.0.0.1", 0))
            return s.getsockname()[1]

    @staticmethod
    def _is_port_open(port: int) -> bool:
        try:
            with socket.create_connection(("127.0.0.1", port), timeout=0.1):
                return True
        except (socket.timeout, ConnectionRefusedError, OSError):
            return False

    def connect(self) -> str:
        with self._lock:
            if self.base_url and self.port_forward_process and self.port_forward_process.poll() is None:
                return self.base_url

            self.close()
            local_port = self._get_free_port()
            logger.info("Opening local port-forward to svc/%s in %s (%d -> %d)...",
                        self.service_name, self.namespace, local_port, self.remote_port)

            self.port_forward_process = subprocess.Popen(
                [
                    "kubectl", "port-forward",
                    f"svc/{self.service_name}",
                    f"{local_port}:{self.remote_port}",
                    "-n", self.namespace,
                ],
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
            )

            deadline = time.monotonic() + self.ready_timeout_s
            while time.monotonic() < deadline:
                if self.port_forward_process.poll() is not None:
                    _, stderr = self.port_forward_process.communicate()
                    err = stderr.decode(errors="replace")
                    raise RuntimeError(f"kubectl port-forward exited unexpectedly: {err}")

                if self._is_port_open(local_port):
                    self.base_url = f"http://127.0.0.1:{local_port}"
                    logger.info("Local tunnel ready at %s", self.base_url)
                    return self.base_url

                time.sleep(0.05)

            self.close()
            raise TimeoutError(f"Timed out after {self.ready_timeout_s:g}s waiting for local port-forward")

    def close(self) -> None:
        with self._lock:
            if self.port_forward_process:
                try:
                    self.port_forward_process.terminate()
                    try:
                        self.port_forward_process.wait(timeout=2.0)
                    except subprocess.TimeoutExpired:
                        self.port_forward_process.kill()
                        self.port_forward_process.wait(timeout=2.0)
                except Exception as e:
                    logger.warning("Error stopping port-forward process: %s", e)
                finally:
                    self.port_forward_process = None
                    self.base_url = None


def resolve_endpoint(
    configured_url: Optional[str] = None,
    service_name: str = "atenet-router",
    namespace: str = "ate-system",
    default_port: int = 8080,
    env_vars: tuple[str, ...] = ("SUBSTRATE_ROUTER_URL", "ATENET_ROUTER_URL"),
) -> str:
    """Resolve endpoint following configuration priority.

    Priority:
    1. Explicit caller configuration (if not empty and not default placeholder)
    2. Environment variables (e.g. SUBSTRATE_ROUTER_URL, ATENET_ROUTER_URL)
    3. In-cluster detection (if KUBERNETES_SERVICE_HOST is set -> InClusterStrategy)
    4. Localhost fallback
    """
    if configured_url and configured_url not in ("http://localhost:8080", "http://localhost:8000"):
        return configured_url.rstrip("/")

    for env_var in env_vars:
        val = os.environ.get(env_var)
        if val:
            return val.rstrip("/")

    # Detect in-cluster environment
    if os.environ.get("KUBERNETES_SERVICE_HOST"):
        return InClusterConnectionStrategy(
            service_name=service_name,
            namespace=namespace,
            port=default_port,
        ).connect()

    # Fallback to configured or local
    return (configured_url or f"http://127.0.0.1:{default_port}").rstrip("/")
