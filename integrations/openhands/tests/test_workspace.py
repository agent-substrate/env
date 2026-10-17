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

"""AteEnvWorkspace against a fake ate-env-api and port proxy, and a real agent-server."""

from __future__ import annotations

import os

import httpx
import pytest
from ate_env import NotFoundError
from openhands.sdk.workspace import RemoteWorkspace

from ate_env_openhands import AteEnvWorkspace, port_url

from .conftest import IMAGE, SESSION_KEY
from .fakes import DEFAULT_ATESPACE, FakeAteEnv, serve


def make(api_url, state, tmp_path, **kw) -> AteEnvWorkspace:
    args = dict(
        template="oh-agent-server",
        image=IMAGE,
        api_url=api_url,
        proxy_url=state.proxy_url,
        api_key=SESSION_KEY,
        working_dir=str(tmp_path / "work"),
        health_check_timeout=15,
    )
    args.update(kw)
    return AteEnvWorkspace(**args)


# -- validation, before any call ---------------------------------------------


def test_requires_template_or_env_id():
    with pytest.raises(ValueError, match="template is required"):
        AteEnvWorkspace(api_url="127.0.0.1:1")


def test_rejects_an_image_tag():
    with pytest.raises(ValueError, match="pinned by digest"):
        AteEnvWorkspace(template="t", image="python:3.12", api_url="127.0.0.1:1")


def test_rejects_an_image_when_attaching():
    with pytest.raises(ValueError, match="only when creating"):
        AteEnvWorkspace(env_id="e1", image=IMAGE, api_url="127.0.0.1:1")


def test_is_a_remote_workspace():
    assert issubclass(AteEnvWorkspace, RemoteWorkspace)
    for name in ("template", "image", "atespace", "env_id", "server_port",
                 "health_check_timeout", "delete_on_exit", "working_dir", "api_key"):
        assert name in AteEnvWorkspace.model_fields


def test_port_url_always_names_the_atespace():
    assert port_url("ate-env-api.ate-env:7777", "ate-env", "e1", 8000) == (
        "http://ate-env-api.ate-env:7777/v1alpha/atespaces/ate-env/envs/e1/ports/8000")
    assert port_url("https://env.example.com/", "team-a", "e1", 9100) == (
        "https://env.example.com/v1alpha/atespaces/team-a/envs/e1/ports/9100")


# -- lifecycle -------------------------------------------------------------------


def test_creates_runs_and_deletes_on_exit(stack, tmp_path):
    api_url, state = stack
    with make(api_url, state, tmp_path, template_atespace="ate-env") as ws:
        env_id = ws.env_id
        assert state.creates == [{
            "id": env_id, "atespace": DEFAULT_ATESPACE, "template": "oh-agent-server",
            "template_atespace": "ate-env", "rootfs_image": IMAGE,
        }]
        assert ws.host == port_url(state.proxy_url, DEFAULT_ATESPACE, env_id, 8000)
        result = ws.execute_command("echo hello", timeout=30)
        assert (result.exit_code, result.stdout.strip()) == (0, "hello")
        assert os.path.isdir(tmp_path / "work"), "working_dir is created"
        assert ws.alive
    assert state.deletes == [env_id]
    assert env_id not in state.envs


def test_files_round_trip(stack, tmp_path):
    api_url, state = stack
    with make(api_url, state, tmp_path) as ws:
        remote = str(tmp_path / "work" / "data.bin")
        up = ws.file_upload(b"\x00\x01payload", remote)
        assert up.success, up.error
        down = ws.file_download(remote, tmp_path / "out.bin")
        assert down.success, down.error
        assert (tmp_path / "out.bin").read_bytes() == b"\x00\x01payload"


def test_health_waits_through_a_booting_environment(stack, tmp_path):
    api_url, state = stack
    state.unhealthy_polls = 3
    with make(api_url, state, tmp_path) as ws:
        assert ws.execute_command("true", timeout=30).exit_code == 0
    assert state.unhealthy_polls == 0, "the three 503s were waited out"


def test_health_timeout_deletes_the_environment(tmp_path):
    with serve(FakeAteEnv()) as (api_url, state):  # nothing listens on the port
        with pytest.raises(RuntimeError, match="did not answer /health.*502"):
            make(api_url, state, tmp_path, health_check_timeout=1.5)
        assert len(state.creates) == 1
        assert state.deletes == [state.creates[0]["id"]]
        assert state.envs == {}


def test_a_lost_create_answer_still_deletes_by_name(stack, tmp_path):
    api_url, state = stack
    state.create_but_fail = True
    with pytest.raises(Exception, match="DEADLINE_EXCEEDED|lost the answer"):
        make(api_url, state, tmp_path)
    assert state.deletes == [state.creates[0]["id"]]
    assert state.envs == {}


def test_a_wrong_session_key_fails_and_cleans_up(stack, tmp_path):
    api_url, state = stack
    with pytest.raises(PermissionError, match="rejected the session key"):
        make(api_url, state, tmp_path, api_key="wrong")
    assert state.deletes == [state.creates[0]["id"]]


def test_attach_neither_creates_nor_deletes(stack, tmp_path):
    api_url, state = stack
    owner = make(api_url, state, tmp_path, delete_on_exit=False)
    owner.cleanup()
    env_id = owner.env_id
    assert env_id in state.envs and state.deletes == []
    with AteEnvWorkspace(env_id=env_id, api_url=api_url, proxy_url=state.proxy_url,
                         api_key=SESSION_KEY, working_dir=str(tmp_path)) as ws:
        assert ws.execute_command("echo attached", timeout=30).stdout.strip() == "attached"
    assert len(state.creates) == 1
    assert state.deletes == []
    assert env_id in state.envs


def test_attach_to_an_unknown_environment_raises(stack, tmp_path):
    api_url, state = stack
    with pytest.raises(NotFoundError):
        AteEnvWorkspace(env_id="no-such-env", api_url=api_url, proxy_url=state.proxy_url)
    assert state.creates == [] and state.deletes == []


def test_pause_suspends_and_resume_wakes(stack, tmp_path):
    api_url, state = stack
    with make(api_url, state, tmp_path) as ws:
        marker = tmp_path / "work" / "marker"
        ws.execute_command(f"echo kept > {marker}", timeout=30)
        ws.pause()
        assert state.suspends == [ws.env_id]
        assert state.envs[ws.env_id]["status"] != 0
        ws.resume()
        assert state.wakes == 1
        assert ws.execute_command(f"cat {marker}", timeout=30).stdout.strip() == "kept"


def test_exit_cleans_up_even_when_the_parent_exit_raises(stack, tmp_path, monkeypatch):
    api_url, state = stack
    ws = make(api_url, state, tmp_path)

    def boom(self, *args):
        raise RuntimeError("callback failed")

    monkeypatch.setattr(RemoteWorkspace, "__exit__", boom)
    with pytest.raises(RuntimeError, match="callback failed"):
        ws.__exit__(None, None, None)
    assert state.deletes == [ws.env_id]
    ws.cleanup()  # idempotent
    assert state.deletes == [ws.env_id]


def test_the_api_token_never_reaches_the_sandbox(stack, tmp_path):
    api_url, state = stack
    with make(api_url, state, tmp_path, api_token="api-secret") as ws:
        ws.execute_command("true", timeout=30)
    for request in state.proxied:
        assert "api-secret" not in str(request["headers"])
        assert "authorization" not in {k.lower() for k in request["headers"]}


def test_port_url_reaches_another_port(stack, tmp_path):
    api_url, state = stack
    with make(api_url, state, tmp_path) as ws:
        assert ws.port_url(9100) == port_url(state.proxy_url, DEFAULT_ATESPACE, ws.env_id, 9100)
        assert httpx.get(f"{ws.port_url(9100)}/x").status_code == 502  # nothing listens


def test_a_conversation_runs_through_the_port_proxy(stack, tmp_path):
    """RemoteConversation: REST calls and the event WebSocket through the path form."""
    pytest.importorskip("openhands.tools")
    from openhands.sdk import LLM, Conversation, RemoteConversation
    from openhands.tools.preset.default import get_default_agent
    from pydantic import SecretStr

    api_url, state = stack
    llm = LLM(usage_id="agent", model="openai/unused", api_key=SecretStr("x"),
              base_url="http://127.0.0.1:9")  # never called: the conversation is not run
    with make(api_url, state, tmp_path) as ws:
        conversation = Conversation(agent=get_default_agent(llm=llm, cli_mode=True), workspace=ws)
        try:
            assert isinstance(conversation, RemoteConversation)
            assert conversation.state.id
        finally:
            conversation.close()
    assert any(p["headers"].get("Upgrade", "").lower() == "websocket" for p in state.proxied), (
        "the event stream went through the proxy as a WebSocket")
