from __future__ import annotations

import grpc.aio
import pytest

from ate_env import Client, EnvironmentStatus, InvalidArgumentError, NotFoundError, RpcError
from ate_env._gen.ateenv.v1 import env_pb2
from ate_env.client import _normalize_target


def test_normalize_target():
    assert _normalize_target("localhost:7777") == "localhost:7777"
    assert _normalize_target("http://localhost:7777") == "localhost:7777"
    assert _normalize_target("https://api.example.com:443/") == "api.example.com:443"


def test_enum_values_match_proto():
    for member in EnvironmentStatus:
        assert env_pb2.EnvironmentStatus.Value(f"ENVIRONMENT_STATUS_{member.name}") == member


async def test_create_uses_server_defaults(fake_stack):
    client, fakes = fake_stack
    env = await client.create("dev1")
    assert env.id == "dev1"
    assert env.atespace == "default"
    stored = fakes.environments.environments[("default", "dev1")]
    assert stored.template.name == "default-env"
    assert stored.template.atespace == "default"


async def test_create_omits_template_when_not_given(fake_stack):
    client, fakes = fake_stack
    await client.create("dev1")
    assert not fakes.environments.last_create_request.HasField("template")


async def test_create_duplicate_maps_to_rpc_error_with_code(fake_stack):
    client, _ = fake_stack
    await client.create("dev1")
    with pytest.raises(RpcError, match="already exists") as exc_info:
        await client.create("dev1")
    assert exc_info.value.code == grpc.StatusCode.ALREADY_EXISTS


async def test_create_empty_id_rejected(fake_stack):
    client, _ = fake_stack
    with pytest.raises(InvalidArgumentError, match="id is required"):
        await client.create("")


async def test_create_with_template(fake_stack):
    client, _ = fake_stack
    env = await client.create(
        "dev2", atespace="team-a", template_name="big-env", template_atespace="ns1"
    )
    info = await env.info()
    assert info.atespace == "team-a"
    assert info.template is not None
    assert info.template.name == "big-env"
    assert info.template.atespace == "ns1"


async def test_lifecycle_roundtrip(fake_stack):
    client, _ = fake_stack
    await client.create("dev1")

    info = await client.get("dev1")
    assert info.status == EnvironmentStatus.RUNNING

    await client.suspend("dev1")
    info = await client.get("dev1")
    assert info.status == EnvironmentStatus.SUSPENDED

    await client.delete("dev1")
    with pytest.raises(NotFoundError):
        await client.get("dev1")


async def test_get_missing_raises_not_found(fake_stack):
    client, _ = fake_stack
    with pytest.raises(NotFoundError, match='environment "nope" not found'):
        await client.get("nope")
    with pytest.raises(NotFoundError):
        await client.suspend("nope")
    with pytest.raises(NotFoundError):
        await client.delete("nope")


async def test_env_factory_makes_no_rpc(fake_stack):
    client, fakes = fake_stack
    env = client.env("ghost", atespace="team-a")
    assert env.id == "ghost"
    assert env.atespace == "team-a"
    assert fakes.environments.environments == {}


async def test_env_handle_lifecycle_uses_its_atespace(fake_stack):
    client, fakes = fake_stack
    await client.create("dev1", atespace="team-a")
    env = client.env("dev1", atespace="team-a")

    info = await env.info()
    assert (info.atespace, info.id) == ("team-a", "dev1")

    await env.suspend()
    stored = fakes.environments.environments[("team-a", "dev1")]
    assert stored.status == env_pb2.ENVIRONMENT_STATUS_SUSPENDED

    await env.delete()
    assert ("team-a", "dev1") not in fakes.environments.environments


async def test_unreachable_server_maps_to_rpc_error():
    client = Client("127.0.0.1:1")
    try:
        with pytest.raises(RpcError) as exc_info:
            await client.get("dev1")
        assert exc_info.value.code == grpc.StatusCode.UNAVAILABLE
    finally:
        await client.close()


async def test_close_shuts_down_owned_channel():
    client = Client("127.0.0.1:1")
    await client.close()
    assert client._channel.get_state() == grpc.ChannelConnectivity.SHUTDOWN


async def test_external_channel_not_closed():
    channel = grpc.aio.insecure_channel("127.0.0.1:1")
    client = Client(channel=channel)
    await client.close()
    assert channel.get_state() != grpc.ChannelConnectivity.SHUTDOWN
    await channel.close()
