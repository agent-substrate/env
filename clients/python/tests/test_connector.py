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

import os
import pytest
from unittest.mock import patch

from ate_env.connector import (
    DirectConnectionStrategy,
    InClusterConnectionStrategy,
    LocalTunnelConnectionStrategy,
    resolve_endpoint,
)
from ate_env.backend.substrate import SubstrateBackendDriver


def test_direct_connection_strategy():
    strat = DirectConnectionStrategy("http://custom-host:9000/")
    assert strat.connect() == "http://custom-host:9000"


def test_in_cluster_connection_strategy_dns():
    strat = InClusterConnectionStrategy(
        service_name="atenet-router",
        namespace="ate-system",
        port=8080,
    )
    assert strat.connect() == "http://atenet-router.ate-system.svc.cluster.local:8080"


def test_in_cluster_connection_strategy_pod_ip():
    strat = InClusterConnectionStrategy(
        port=8080,
        get_pod_ip=lambda: "10.244.1.42",
    )
    assert strat.connect() == "http://10.244.1.42:8080"


def test_resolve_endpoint_caller_override():
    res = resolve_endpoint("http://my-proxy:8080")
    assert res == "http://my-proxy:8080"


def test_resolve_endpoint_env_var():
    with patch.dict(os.environ, {"SUBSTRATE_ROUTER_URL": "http://router.env.internal:8000"}):
        res = resolve_endpoint(None)
        assert res == "http://router.env.internal:8000"


def test_resolve_endpoint_in_cluster_fallback():
    env = {
        "KUBERNETES_SERVICE_HOST": "10.0.0.1",
        "SUBSTRATE_ROUTER_URL": "",
        "ATENET_ROUTER_URL": "",
    }
    with patch.dict(os.environ, env, clear=True):
        res = resolve_endpoint(None, service_name="atenet-router", namespace="ate-dev", default_port=8080)
        assert res == "http://atenet-router.ate-dev.svc.cluster.local:8080"


def test_resolve_endpoint_local_fallback():
    with patch.dict(os.environ, {}, clear=True):
        res = resolve_endpoint(None, default_port=8080)
        assert res == "http://127.0.0.1:8080"


def test_substrate_driver_dynamic_resolution():
    with patch.dict(os.environ, {
        "SUBSTRATE_ROUTER_URL": "http://router-from-env:9090",
        "SUBSTRATE_API_ENDPOINT": "http://api-from-env:8080",
    }):
        driver = SubstrateBackendDriver()
        assert driver.router_url == "http://router-from-env:9090"
        assert driver.api_endpoint == "http://api-from-env:8080"
