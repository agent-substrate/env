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

"""One background event loop, and one ate-env client per endpoint, for all workspaces.

The OpenHands workspace API is synchronous and ``ate_env.Client`` is asyncio
gRPC. A fleet of workspaces shares one loop thread and one channel per
(endpoint, token) instead of a thread and a channel each.
"""

from __future__ import annotations

import threading
from collections.abc import Coroutine
from typing import Any

from ate_env import Client
from ate_env.loop_runner import LoopRunner

_lock = threading.Lock()
_runner: LoopRunner | None = None
_clients: dict[tuple[str, str | None], Client] = {}


def _get_runner() -> LoopRunner:
    global _runner
    with _lock:
        if _runner is None:
            _runner = LoopRunner(name="ate-env-openhands")
        return _runner


def run[T](coro: Coroutine[Any, Any, T]) -> T:
    """Run a coroutine on the shared loop and block for its result."""
    return _get_runner().run(coro)


def client(endpoint: str, token: str | None) -> Client:
    """The shared client for an ate-env-api endpoint and bearer token."""
    key = (endpoint, token)
    with _lock:
        existing = _clients.get(key)
    if existing is not None:
        return existing

    async def make() -> Client:
        return Client(endpoint, auth_token=token)

    made = run(make())  # the channel binds to the shared loop
    with _lock:
        return _clients.setdefault(key, made)
