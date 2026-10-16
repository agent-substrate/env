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

import grpc.aio
import pytest

from ate_env import (
    Client,
    EnvironmentStatus,
    InvalidArgumentError,
    NotFoundError,
    RpcError,
)
from ate_env._gen.ateenv.v1alpha import env_pb2, env_pb2_grpc, guest_pb2_grpc
from ate_env.client import _auth_interceptors, _normalize_target

from .fakes import FakeEnvironmentService, FakeFileSystemService, FakeProc, FakeProcessService


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
    assert env.atespace == "ate-env"
    stored = fakes.environments.environments[("ate-env", "dev1")]
    assert stored.template.name == "default-template"
    assert stored.template.atespace == "ate-env"


async def test_create_omits_template_when_not_given(fake_stack):
    client, fakes = fake_stack
    await client.create("dev1")
    assert not fakes.environments.last_create_request.HasField("template")


async def test_create_with_image_reports_derived_template(fake_stack):
    client, fakes = fake_stack
    image = "docker.io/library/python@sha256:" + "0123456789abcdef" * 4
    env = await client.create("dev3", rootfs_image=image)
    assert fakes.environments.last_create_request.rootfs_image == image
    info = await env.info()
    assert info.template is not None
    assert info.template.name == "default-template-0123456789ab"


async def test_create_with_image_and_base_template(fake_stack):
    client, fakes = fake_stack
    image = "docker.io/library/python@sha256:" + "0123456789abcdef" * 4
    env = await client.create("dev4", template_name="py-base", rootfs_image=image)
    info = await env.info()
    assert info.template.name == "py-base-0123456789ab"


async def test_create_omits_image_when_not_given(fake_stack):
    client, fakes = fake_stack
    await client.create("dev5")
    assert fakes.environments.last_create_request.rootfs_image == ""


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


async def test_auth_interceptor_sends_bearer_on_all_call_shapes():
    def auth_headers(ctx: grpc.aio.ServicerContext) -> bytes:
        vals = [v for k, v in (ctx.invocation_metadata() or ()) if k == "authorization"]
        return "|".join(vals).encode() if vals else b"<none>"

    async def uu(req, ctx):
        return auth_headers(ctx)

    async def us(req, ctx):
        yield auth_headers(ctx)

    async def su(req_iter, ctx):
        async for _ in req_iter:
            pass
        return auth_headers(ctx)

    async def ss(req_iter, ctx):
        async for _ in req_iter:
            pass
        yield auth_headers(ctx)

    def ident(x):
        return x

    server = grpc.aio.server()
    server.add_generic_rpc_handlers(
        (
            grpc.method_handlers_generic_handler(
                "t",
                {
                    "UU": grpc.unary_unary_rpc_method_handler(uu, ident, ident),
                    "US": grpc.unary_stream_rpc_method_handler(us, ident, ident),
                    "SU": grpc.stream_unary_rpc_method_handler(su, ident, ident),
                    "SS": grpc.stream_stream_rpc_method_handler(ss, ident, ident),
                },
            ),
        )
    )
    port = server.add_insecure_port("127.0.0.1:0")
    await server.start()
    ch = grpc.aio.insecure_channel(f"127.0.0.1:{port}", interceptors=_auth_interceptors("TOK"))

    async def gen():
        yield b""

    try:
        assert await ch.unary_unary("/t/UU", ident, ident)(b"") == b"Bearer TOK"
        assert [x async for x in ch.unary_stream("/t/US", ident, ident)(b"")] == [b"Bearer TOK"]
        assert await ch.stream_unary("/t/SU", ident, ident)(gen()) == b"Bearer TOK"
        assert [x async for x in ch.stream_stream("/t/SS", ident, ident)(gen())] == [b"Bearer TOK"]
    finally:
        await ch.close()
        await server.stop(None)


async def test_auth_token_sent_once_across_unary_and_streaming_env_rpcs():
    seen_auth: list[tuple[str, list[str]]] = []

    class RequireBearerInterceptor(grpc.aio.ServerInterceptor):
        async def intercept_service(self, continuation, handler_call_details):
            vals = [
                v
                for k, v in (handler_call_details.invocation_metadata or ())
                if k == "authorization"
            ]
            seen_auth.append((handler_call_details.method, vals))
            if vals != ["Bearer s3cr3t"]:
                async def unauth_unary(req, ctx):
                    await ctx.abort(grpc.StatusCode.UNAUTHENTICATED, "missing or invalid bearer")
                return grpc.unary_unary_rpc_method_handler(unauth_unary)
            return await continuation(handler_call_details)

    server = grpc.aio.server(interceptors=[RequireBearerInterceptor()])
    fakes_env = FakeEnvironmentService()
    fakes_proc = FakeProcessService()
    fakes_fs = FakeFileSystemService()
    env_pb2_grpc.add_EnvironmentServiceServicer_to_server(fakes_env, server)
    guest_pb2_grpc.add_ProcessServiceServicer_to_server(fakes_proc, server)
    guest_pb2_grpc.add_FileSystemServiceServicer_to_server(fakes_fs, server)
    port = server.add_insecure_port("127.0.0.1:0")
    await server.start()

    client = Client(f"127.0.0.1:{port}", auth_token="s3cr3t")
    try:
        env = await client.create("dev1")
        await env.info()
        await env.write_file("/workspace/hello.txt", b"hello world")
        assert await env.read_file_bytes("/workspace/hello.txt") == b"hello world"
        fakes_proc.next_procs.append(
            FakeProc(chunks=[("stdout", b"ok\n")], exit_code=0, echo_stdin=True)
        )
        res = await env.shell("cat", stdin=b"in")
        assert res.exit_code == 0
        assert res.stdout == "ok\nin"
    finally:
        await client.close()
        await server.stop(None)

    methods = {m for m, _ in seen_auth}
    assert "/ateenv.v1alpha.EnvironmentService/CreateEnvironment" in methods
    assert "/ateenv.v1alpha.EnvironmentService/GetEnvironment" in methods
    assert "/ateenv.v1alpha.FileSystemService/WriteFile" in methods
    assert "/ateenv.v1alpha.FileSystemService/ReadFile" in methods
    assert "/ateenv.v1alpha.ProcessService/StartProcess" in methods
    assert "/ateenv.v1alpha.ProcessService/WriteProcessInput" in methods
    assert "/ateenv.v1alpha.ProcessService/StreamProcessOutput" in methods
    for method, vals in seen_auth:
        assert vals == ["Bearer s3cr3t"], f"unexpected authorization headers on {method}: {vals}"

