from __future__ import annotations

from dataclasses import dataclass

import grpc.aio
import pytest

from ate_env import Client
from ate_env._gen.ateenv.v1alpha import env_pb2_grpc, guest_pb2_grpc

from .fakes import FakeEnvironmentService, FakeFileSystemService, FakeProcessService


@dataclass
class Fakes:
    environments: FakeEnvironmentService
    processes: FakeProcessService
    filesystem: FakeFileSystemService


@pytest.fixture
async def fake_stack():
    """An in-process grpc.aio server with the three fake services, plus a Client."""
    server = grpc.aio.server()
    fakes = Fakes(FakeEnvironmentService(), FakeProcessService(), FakeFileSystemService())
    env_pb2_grpc.add_EnvironmentServiceServicer_to_server(fakes.environments, server)
    guest_pb2_grpc.add_ProcessServiceServicer_to_server(fakes.processes, server)
    guest_pb2_grpc.add_FileSystemServiceServicer_to_server(fakes.filesystem, server)
    port = server.add_insecure_port("127.0.0.1:0")
    await server.start()
    client = Client(f"127.0.0.1:{port}")
    try:
        yield client, fakes
    finally:
        await client.close()
        await server.stop(None)
