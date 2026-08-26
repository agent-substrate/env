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
