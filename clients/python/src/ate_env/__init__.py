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

"""Async Python client for the Agent Substrate Environment API (ate-env-api).

Quickstart:

    import asyncio
    from ate_env import Client

    async def main():
        client = Client("localhost:7777")
        try:
            env = await client.create("dev1")
            result = await env.shell("echo hello")
            print(result.stdout)
            await env.delete()
        finally:
            await client.close()

    asyncio.run(main())
"""

from .client import DEFAULT_ATESPACE, Client
from .env import Env
from .errors import (
    EnvError,
    InvalidArgumentError,
    NotFoundError,
    PermissionDeniedError,
    RpcError,
    map_rpc_error,
)
from .types import (
    EnvironmentInfo,
    EnvironmentStatus,
    OutputChunk,
    OutputSource,
    ProcessInfo,
    ProcessStatus,
    ShellResult,
    Template,
)

__all__ = [
    "Client",
    "DEFAULT_ATESPACE",
    "Env",
    "EnvError",
    "EnvironmentInfo",
    "EnvironmentStatus",
    "InvalidArgumentError",
    "OutputChunk",
    "OutputSource",
    "NotFoundError",
    "PermissionDeniedError",
    "ProcessInfo",
    "ProcessStatus",
    "RpcError",
    "ShellResult",
    "Template",
    "map_rpc_error",
]
