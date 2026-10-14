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

import asyncio
import concurrent.futures
import threading
from collections.abc import Coroutine
from typing import Any, TypeVar

T = TypeVar("T")


class LoopRunner:
    """A dedicated, permanent background thread running an asyncio event loop.

    Allows synchronous facades (like SubstrateBackendDriver and SubstrateEnvClientRuntime)
    to share a single grpc.aio channel safely across multiple sequential or concurrent calls
    without attaching channels to short-lived loops that get closed.
    """

    def __init__(self, name: str = "ate-env-sync-runner") -> None:
        self._loop = asyncio.new_event_loop()
        self._thread = threading.Thread(target=self._run_loop, name=name, daemon=True)
        self._thread.start()

    def _run_loop(self) -> None:
        asyncio.set_event_loop(self._loop)
        self._loop.run_forever()

    @property
    def loop(self) -> asyncio.AbstractEventLoop:
        return self._loop

    def run(self, coro: Coroutine[Any, Any, T], timeout: float | None = None) -> T:
        """Run a coroutine on the background event loop and block for the result."""
        future = asyncio.run_coroutine_threadsafe(coro, self._loop)
        return future.result(timeout=timeout)

    def submit(self, coro: Coroutine[Any, Any, T]) -> concurrent.futures.Future[T]:
        """Submit a coroutine to the background event loop without blocking."""
        return asyncio.run_coroutine_threadsafe(coro, self._loop)

    def stop(self) -> None:
        """Stop the background event loop and join the worker thread."""
        if threading.current_thread() is self._thread:
            # Prevent deadlock if called from a task running on this runner's loop
            return
        if self._loop.is_running():
            async def _shutdown() -> None:
                tasks = [t for t in asyncio.all_tasks() if t is not asyncio.current_task()]
                for t in tasks:
                    t.cancel()
                await asyncio.gather(*tasks, return_exceptions=True)
                await self._loop.shutdown_asyncgens()

            future = asyncio.run_coroutine_threadsafe(_shutdown(), self._loop)
            try:
                future.result(timeout=2.0)
            except Exception:
                pass
            self._loop.call_soon_threadsafe(self._loop.stop)
            self._thread.join(timeout=2.0)
            if not self._loop.is_closed():
                self._loop.close()

    def __del__(self) -> None:
        try:
            self.stop()
        except Exception:
            pass
