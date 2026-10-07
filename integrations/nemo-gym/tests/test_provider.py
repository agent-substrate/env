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

"""Contract tests for the substrate NeMo Gym provider.

Runs against an in-process fake of ate-env-api (tests/fakes.py) reached
through the real ``ate_env`` client over loopback gRPC, asserting the rules
from the adding-a-provider contract: create returns only when the sandbox
executes commands, exec never raises on nonzero exits, close is
cleanup-safe, and provider_options are validated strictly.
"""

from __future__ import annotations

import asyncio
import contextlib
import logging
from pathlib import Path

import grpc
import pytest
from ate_env import Client, EnvironmentStatus, NotFoundError

from nemo_gym_substrate import provider as provider_mod
from nemo_gym_substrate._compat import (
    SandboxCreateError,
    SandboxCreateVerificationError,
    SandboxSpec,
    SandboxStatus,
)
from nemo_gym_substrate.provider import SubstrateSandboxProvider

from .fakes import FakeAteEnv, serve


@contextlib.asynccontextmanager
async def stack(create: dict | None = None, *, state: FakeAteEnv | None = None):
    """A provider dialing a fake ate-env-api; yields ``(provider, fake)``."""
    async with serve(state) as (target, fake):
        config = {
            "connection": {"api_url": target, "request_timeout_s": 5.0},
            "create": {
                "ready_poll_interval_s": 0.01,
                "image_templates": {"img:1": "tpl-img1"},
                **(create or {}),
            },
        }
        provider = SubstrateSandboxProvider(config)
        try:
            yield provider, fake
        finally:
            await provider.aclose()


def run(coro):
    return asyncio.run(coro)


def _probe_count(fake: FakeAteEnv) -> int:
    return sum(1 for s in fake.starts if s["command"][-1:] == ["true"])


def test_create_returns_ready_handle_and_seeds_files():
    async def t():
        async with stack() as (provider, fake):
            handle = await provider.create(
                SandboxSpec(files={"/task/input.json": '{"n": 1}'}, workdir="/task")
            )
            assert handle.provider_name == "substrate"
            assert handle.sandbox_id.startswith("gym-")
            env = fake.envs[handle.sandbox_id]
            assert (env["template"], env["template_atespace"], env["atespace"]) == (
                "default-template",
                "ate-env",
                "ate-env",
            )
            assert fake.files[(handle.sandbox_id, "/task/input.json")] == b'{"n": 1}'
            assert fake.modes[(handle.sandbox_id, "/task/input.json")] == 0o644

    run(t())


def test_create_waits_for_readiness():
    async def t():
        async with stack() as (provider, fake):
            fake.ready_after_probes = 2
            handle = await provider.create(SandboxSpec())
            assert handle.sandbox_id in fake.envs
            assert _probe_count(fake) == 3  # two UNAVAILABLE + one success

    run(t())


def test_create_failure_raises_create_error():
    async def t():
        async with stack() as (provider, fake):
            fake.create_code = grpc.StatusCode.RESOURCE_EXHAUSTED
            with pytest.raises(SandboxCreateError, match="RESOURCE_EXHAUSTED"):
                await provider.create(SandboxSpec())
            assert fake.envs == {}

    run(t())


def test_create_readiness_timeout_cleans_up():
    async def t():
        async with stack() as (provider, fake):
            fake.ready_after_probes = 10_000
            with pytest.raises(SandboxCreateVerificationError, match="UNAVAILABLE"):
                await provider.create(SandboxSpec(ready_timeout_s=0.05))
            assert fake.envs == {}  # the half-created env was deleted

    run(t())


def test_image_resolves_via_mapping_or_fails():
    async def t():
        async with stack() as (provider, fake):
            handle = await provider.create(SandboxSpec(image="img:1"))
            assert fake.envs[handle.sandbox_id]["template"] == "tpl-img1"
            with pytest.raises(SandboxCreateError, match="no ActorTemplate mapped"):
                await provider.create(SandboxSpec(image="img:unmapped"))

    run(t())


def test_exec_nonzero_exit_returns_result_not_raise():
    async def t():
        async with stack() as (provider, _):
            handle = await provider.create(SandboxSpec())
            result = await provider.exec(handle, "exit 7")
            assert (result.return_code, result.stderr, result.stdout) == (7, "boom", "")

    run(t())


def test_exec_merges_spec_env_and_cwd():
    async def t():
        async with stack() as (provider, fake):
            handle = await provider.create(SandboxSpec(workdir="/w", env={"A": "1", "B": "spec"}))
            await provider.exec(handle, "echo hi", env={"B": "call"})
            sent = fake.starts[-1]
            assert sent["command"] == ["sh", "-c", "echo hi"]
            assert sent["env"] == {"A": "1", "B": "call"}
            assert sent["cwd"] == "/w"
            await provider.exec(handle, "echo hi", cwd="/other")
            assert fake.starts[-1]["cwd"] == "/other"

    run(t())


@pytest.mark.parametrize(
    ("timeout_s", "guest_deadline"),
    [(3, "3"), (0.5, "1"), (0, "1"), (2.2, "3")],
)
def test_exec_timeout_wraps_command_guest_side(timeout_s, guest_deadline):
    # `timeout 0` disables the limit in coreutils, so a sub-second or zero
    # deadline must round up to 1s rather than floor to 0.
    async def t():
        async with stack() as (provider, fake):
            handle = await provider.create(SandboxSpec())
            await provider.exec(handle, "sleep 100", timeout_s=timeout_s)
            assert fake.starts[-1]["command"] == ["timeout", guest_deadline, "sh", "-c", "sleep 100"]

    run(t())


def test_exec_without_timeout_is_unwrapped():
    async def t():
        async with stack() as (provider, fake):
            handle = await provider.create(SandboxSpec())
            await provider.exec(handle, "sleep 1")
            assert fake.starts[-1]["command"] == ["sh", "-c", "sleep 1"]

    run(t())


def test_exec_rejects_user():
    async def t():
        async with stack() as (provider, _):
            handle = await provider.create(SandboxSpec())
            with pytest.raises(ValueError, match="user"):
                await provider.exec(handle, "id", user="root")

    run(t())


def test_exec_client_timeout_returns_sentinel_and_kills(monkeypatch):
    monkeypatch.setattr(provider_mod, "_EXEC_GRACE_S", 0.05)

    async def t():
        async with stack() as (provider, fake):
            handle = await provider.create(SandboxSpec())
            fake.hang.add("sleepy")
            result = await provider.exec(handle, "sleepy", timeout_s=0.01)
            assert result.return_code == -1
            assert result.stdout is None
            assert "timed out" in (result.stderr or "")
            hung = [p for p in fake.procs.values() if p.argv[-1] == "sleepy"]
            assert hung and hung[0].killed

    run(t())


def test_file_roundtrip(tmp_path: Path):
    async def t():
        async with stack() as (provider, _):
            handle = await provider.create(SandboxSpec())
            src = tmp_path / "up.bin"
            src.write_bytes(b"\x00\x01payload-longer-than-one-chunk")
            await provider.upload_file(handle, src, "/data/up.bin")
            dst = tmp_path / "down" / "up.bin"
            await provider.download_file(handle, "/data/up.bin", dst)
            assert dst.read_bytes() == b"\x00\x01payload-longer-than-one-chunk"

    run(t())


def test_download_missing_file_raises(tmp_path: Path):
    async def t():
        async with stack() as (provider, _):
            handle = await provider.create(SandboxSpec())
            with pytest.raises(NotFoundError):
                await provider.download_file(handle, "/nope", tmp_path / "out")

    run(t())


def test_status_running_stopped_unknown():
    async def t():
        async with stack() as (provider, fake):
            handle = await provider.create(SandboxSpec())
            assert await provider.status(handle) == SandboxStatus.RUNNING
            fake.get_code = grpc.StatusCode.UNAVAILABLE
            assert await provider.status(handle) == SandboxStatus.UNKNOWN
            fake.get_code = None
            await provider.close(handle)
            assert await provider.status(handle) == SandboxStatus.STOPPED

    run(t())


@pytest.mark.parametrize(
    ("env_status", "expected"),
    [
        (EnvironmentStatus.RESUMING, SandboxStatus.STARTING),
        (EnvironmentStatus.SUSPENDED, SandboxStatus.RUNNING),  # parked is transparent
        (EnvironmentStatus.PAUSED, SandboxStatus.RUNNING),
        (EnvironmentStatus.CRASHED, SandboxStatus.ERROR),
        (EnvironmentStatus.DELETING, SandboxStatus.STOPPED),
        (EnvironmentStatus.UNSPECIFIED, SandboxStatus.UNKNOWN),
    ],
)
def test_status_maps_env_lifecycle(env_status, expected):
    async def t():
        async with stack() as (provider, fake):
            handle = await provider.create(SandboxSpec())
            fake.set_status(handle.sandbox_id, env_status)
            assert await provider.status(handle) == expected

    run(t())


def test_status_does_not_touch_the_guest():
    async def t():
        async with stack() as (provider, fake):
            handle = await provider.create(SandboxSpec())
            before = len(fake.starts)
            await provider.status(handle)
            assert len(fake.starts) == before  # no probe exec, so no wake-up

    run(t())


def test_close_is_idempotent():
    async def t():
        async with stack() as (provider, fake):
            handle = await provider.create(SandboxSpec())
            await provider.close(handle)
            await provider.close(handle)  # second delete hits NOT_FOUND; must not raise
            assert handle.sandbox_id not in fake.envs

    run(t())


def test_close_logs_but_does_not_raise_on_delete_failure(caplog):
    async def t():
        async with stack() as (provider, fake):
            handle = await provider.create(SandboxSpec())
            fake.delete_code = grpc.StatusCode.INTERNAL
            with caplog.at_level(logging.WARNING, logger="nemo_gym_substrate.provider"):
                await provider.close(handle)  # best-effort: never raises
            assert any(
                "deleting substrate env" in r.message and "INTERNAL" in r.message
                for r in caplog.records
            )

    run(t())


def test_provider_options_override_template_and_atespace():
    async def t():
        async with stack() as (provider, fake):
            handle = await provider.create(
                SandboxSpec(provider_options={"template": "tpl-x", "atespace": "as-x"})
            )
            env = fake.envs[handle.sandbox_id]
            assert (env["template"], env["template_atespace"], env["atespace"]) == (
                "tpl-x",
                "as-x",
                "as-x",
            )
            # The handle remembers its atespace, so later calls route there.
            assert await provider.status(handle) == SandboxStatus.RUNNING
            await provider.close(handle)
            assert handle.sandbox_id not in fake.envs

    run(t())


def test_namespace_is_accepted_as_alias_for_atespace():
    async def t():
        async with stack({"namespace": "legacy"}) as (provider, fake):
            handle = await provider.create(SandboxSpec(provider_options={"namespace": "per-sb"}))
            assert fake.envs[handle.sandbox_id]["atespace"] == "per-sb"
            handle2 = await provider.create(SandboxSpec())
            assert fake.envs[handle2.sandbox_id]["atespace"] == "legacy"

    run(t())


def test_atespace_and_namespace_together_rejected():
    with pytest.raises(ValueError, match="either atespace or namespace"):
        SubstrateSandboxProvider({"create": {"atespace": "a", "namespace": "b"}})


def test_unknown_provider_option_rejected():
    async def t():
        async with stack() as (provider, _):
            with pytest.raises(ValueError, match="unknown option"):
                await provider.create(SandboxSpec(provider_options={"tempalte": "oops"}))

    run(t())


def test_unknown_config_key_rejected():
    with pytest.raises(ValueError, match="unknown option"):
        SubstrateSandboxProvider({"connection": {"api_urll": "typo"}})


def test_create_timeout_on_unreachable_api():
    async def t():
        # A closed port refuses at once (UNAVAILABLE); either way it must not hang.
        provider = SubstrateSandboxProvider(
            {"connection": {"api_url": "127.0.0.1:9", "request_timeout_s": 0.5}}
        )
        try:
            with pytest.raises(SandboxCreateError):
                await provider.create(SandboxSpec())
        finally:
            await provider.aclose()

    run(t())


def test_aclose_is_safe_to_repeat_and_drops_the_client():
    async def t():
        async with stack() as (provider, _):
            await provider.create(SandboxSpec())
            assert provider._client is not None  # noqa: SLF001 - test seam
            await provider.aclose()
            await provider.aclose()
            assert provider._client is None  # noqa: SLF001

    run(t())


def test_caller_owned_client_is_not_closed():
    async def t():
        async with serve() as (target, _):
            client = Client(target)
            try:
                provider = SubstrateSandboxProvider(client=client)
                handle = await provider.create(SandboxSpec())
                await provider.aclose()
                # Still usable: aclose() left the caller's client open.
                info = await client.get(handle.sandbox_id)
                assert info.id == handle.sandbox_id
            finally:
                await client.close()

    run(t())


def test_https_api_url_uses_a_tls_channel():
    async def t():
        provider = SubstrateSandboxProvider(
            {"connection": {"api_url": "https://ate-env.example:443"}}
        )
        try:
            assert provider._ate() is not None  # noqa: SLF001 - opens the channel, never dials
            assert provider._channel is not None  # noqa: SLF001
        finally:
            await provider.aclose()
        assert provider._channel is None  # noqa: SLF001

    run(t())
