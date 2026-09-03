"""Async client for the ate-env-api service."""

from __future__ import annotations

import grpc
import grpc.aio

from ._gen.ateenv.v1alpha import env_pb2, env_pb2_grpc, guest_pb2_grpc
from .env import Env
from .errors import map_rpc_error
from .types import EnvironmentInfo, _environment_info_from_pb

__all__ = ["Client", "DEFAULT_ATESPACE"]

DEFAULT_ATESPACE = "default"


def _normalize_target(endpoint: str) -> str:
    target = endpoint.rstrip("/")
    for prefix in ("http://", "https://"):
        if target.startswith(prefix):
            target = target[len(prefix) :]
    return target


class Client:
    """Manages environments against an ate-env-api endpoint over gRPC.

    A single Client multiplexes concurrent operations over one HTTP/2
    channel and is safe to share across tasks; create one, share it, and
    call close() on shutdown:

        client = Client("localhost:7777")
        try:
            env = await client.create("dev1")
            result = await env.shell("echo hello")
        finally:
            await client.close()
    """

    def __init__(
        self,
        endpoint: str = "localhost:7777",
        *,
        channel: grpc.aio.Channel | None = None,
    ):
        """Create a client for the ate-env-api service at endpoint.

        endpoint accepts a host:port or an http(s):// URL, matching the Go
        client. Pass channel to supply a caller-owned grpc.aio.Channel
        instead; the client then never closes it.
        """
        if channel is not None:
            self._channel = channel
            self._owns_channel = False
        else:
            if not endpoint:
                raise ValueError("ate_env: endpoint is required when channel is not given")
            self._channel = grpc.aio.insecure_channel(_normalize_target(endpoint))
            self._owns_channel = True
        self._environments = env_pb2_grpc.EnvironmentServiceStub(self._channel)
        self._processes = guest_pb2_grpc.ProcessServiceStub(self._channel)
        self._filesystem = guest_pb2_grpc.FileSystemServiceStub(self._channel)

    async def close(self) -> None:
        """Release the client's resources (closes the channel if owned)."""
        if self._owns_channel:
            await self._channel.close()

    async def create(
        self,
        id: str,
        *,
        atespace: str = DEFAULT_ATESPACE,
        template_name: str | None = None,
        template_atespace: str | None = None,
    ) -> Env:
        """Register and start a new environment; returns a handle to it.

        The server fills defaults for the template (name "default-env" in
        atespace "default") when none is given.
        """
        req = env_pb2.CreateEnvironmentRequest(id=id, atespace=atespace)
        if template_name or template_atespace:
            req.template.name = template_name or ""
            req.template.atespace = template_atespace or ""
        try:
            resp = await self._environments.CreateEnvironment(req)
        except grpc.RpcError as e:
            raise map_rpc_error(e) from e
        created = resp.environment
        return self.env(created.id, atespace=created.atespace)

    async def get(self, id: str, *, atespace: str = DEFAULT_ATESPACE) -> EnvironmentInfo:
        """Retrieve the status and configuration of an environment."""
        req = env_pb2.GetEnvironmentRequest(id=id, atespace=atespace)
        try:
            resp = await self._environments.GetEnvironment(req)
        except grpc.RpcError as e:
            raise map_rpc_error(e) from e
        return _environment_info_from_pb(resp.environment)

    async def suspend(self, id: str, *, atespace: str = DEFAULT_ATESPACE) -> None:
        """Checkpoint and stop the environment."""
        req = env_pb2.SuspendEnvironmentRequest(id=id, atespace=atespace)
        try:
            await self._environments.SuspendEnvironment(req)
        except grpc.RpcError as e:
            raise map_rpc_error(e) from e

    async def delete(self, id: str, *, atespace: str = DEFAULT_ATESPACE) -> None:
        """Remove the environment permanently."""
        req = env_pb2.DeleteEnvironmentRequest(id=id, atespace=atespace)
        try:
            await self._environments.DeleteEnvironment(req)
        except grpc.RpcError as e:
            raise map_rpc_error(e) from e

    def env(self, id: str, *, atespace: str = DEFAULT_ATESPACE) -> Env:
        """Return a handle to an environment without checking that it exists."""
        return Env(self, id, atespace or DEFAULT_ATESPACE)
