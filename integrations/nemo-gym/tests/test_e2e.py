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

"""End-to-end tests against a live ate-env deployment.

Skipped unless SUBSTRATE_E2E_API_URL points at a reachable ate-env-api (e.g.
`kubectl port-forward -n ate-env svc/ate-env-api 7777:7777` then
`SUBSTRATE_E2E_API_URL=127.0.0.1:7777 pytest tests/test_e2e.py`; an http://
URL works too). SUBSTRATE_E2E_TEMPLATE picks the ActorTemplate (default
`default-template`, ate-env's own default).

test_full_lifecycle exercises the provider on a real Substrate actor:
create -> exec (incl. nonzero exit, env/cwd) -> file round-trip -> status ->
close -> status STOPPED.

test_runtime_layer_endpoint needs a base template that carries the NeMo
runtime layer (integrations/nemo-gym/layer, registered as in
hack/runtime-layer-demo.sh) in SUBSTRATE_E2E_RUNTIME_TEMPLATE, and the port
NeMo Skills' sandbox server listens on in SUBSTRATE_E2E_RUNTIME_PORT (default
6000), and a digest-pinned task image in SUBSTRATE_E2E_RUNTIME_IMAGE: the
sandbox is created from that image on the layered base, the way a recipe does
it, because the base's own container is the ate-env guest image, which cannot
host the layer's glibc interpreter.
"""

from __future__ import annotations

import asyncio
import os
from pathlib import Path

import pytest
from nemo_gym_substrate._compat import SandboxSpec, SandboxStatus
from nemo_gym_substrate.provider import SubstrateSandboxProvider

API_URL = os.environ.get("SUBSTRATE_E2E_API_URL")
TEMPLATE = os.environ.get("SUBSTRATE_E2E_TEMPLATE", "default-template")

pytestmark = pytest.mark.skipif(
    not API_URL, reason="SUBSTRATE_E2E_API_URL not set; e2e needs a live ate-env-api"
)


def test_full_lifecycle(tmp_path: Path) -> None:
    async def scenario() -> None:
        provider = SubstrateSandboxProvider({"connection": {"api_url": API_URL}})
        handle = None
        try:
            handle = await provider.create(
                SandboxSpec(
                    files={"/task/hello.txt": "hello from nemo-gym-substrate e2e\n"},
                    workdir="/task",
                    env={"E2E_MARK": "42"},
                    provider_options={"template": TEMPLATE},
                    ready_timeout_s=180,
                )
            )

            # exec: env + cwd honored, output captured.
            result = await provider.exec(handle, "echo -n $E2E_MARK; pwd")
            assert result.return_code == 0
            assert "42" in (result.stdout or "")
            assert "/task" in (result.stdout or "")

            # nonzero exit is a result, not an exception.
            result = await provider.exec(handle, "exit 7")
            assert result.return_code == 7

            # file round-trip: the seeded file comes back, an uploaded file too.
            out = tmp_path / "hello.txt"
            await provider.download_file(handle, "/task/hello.txt", out)
            assert out.read_text().startswith("hello from nemo-gym-substrate")
            src = tmp_path / "up.bin"
            src.write_bytes(bytes(range(64)))
            await provider.upload_file(handle, src, "/task/up.bin")
            result = await provider.exec(handle, "wc -c < /task/up.bin")
            assert (result.stdout or "").strip() == "64"

            assert await provider.status(handle) == SandboxStatus.RUNNING
        finally:
            if handle is not None:
                await provider.close(handle)
                assert await provider.status(handle) == SandboxStatus.STOPPED
            await provider.aclose()

    asyncio.run(scenario())


RUNTIME_TEMPLATE = os.environ.get("SUBSTRATE_E2E_RUNTIME_TEMPLATE")
RUNTIME_PORT = int(os.environ.get("SUBSTRATE_E2E_RUNTIME_PORT", "6000"))
RUNTIME_IMAGE = os.environ.get("SUBSTRATE_E2E_RUNTIME_IMAGE")


def _post_json(url: str, headers: dict[str, str], body: dict, timeout: float = 120.0) -> dict:
    import json
    import urllib.request

    req = urllib.request.Request(
        url,
        data=json.dumps(body).encode(),
        method="POST",
        headers={"Content-Type": "application/json", **headers},
    )
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        return json.loads(resp.read())


@pytest.mark.skipif(
    not (RUNTIME_TEMPLATE and RUNTIME_IMAGE),
    reason="SUBSTRATE_E2E_RUNTIME_TEMPLATE and SUBSTRATE_E2E_RUNTIME_IMAGE not set; needs a template with the NeMo runtime layer and a task image",
)
def test_runtime_layer_endpoint() -> None:
    """A daemon the runtime layer injects is reached through endpoint(): NeMo
    Skills' sandbox server answers POST /execute through ate-env-api's port
    proxy, with the headers the provider hands out, and keeps an IPython
    session between calls."""

    async def scenario() -> None:
        # The layered template is the provider's base (create.template), so the
        # task image is derived from it; a provider_options template would
        # select the base itself and skip the image.
        provider = SubstrateSandboxProvider(
            {"connection": {"api_url": API_URL}, "create": {"template": RUNTIME_TEMPLATE}}
        )
        handle = None
        try:
            handle = await provider.create(
                SandboxSpec(image=RUNTIME_IMAGE, ports=(RUNTIME_PORT,), ready_timeout_s=240)
            )
            ep = await provider.endpoint(handle, RUNTIME_PORT)
            url = ep.endpoint.rstrip("/") + "/execute"
            # The layer's own interpreter runs the code: the task image needs no python.
            out = _post_json(url, ep.headers, {"generated_code": "print(6 * 7)", "language": "python", "timeout": 30})
            assert out.get("process_status") == "completed", out
            assert out.get("stdout", "").strip() == "42", out
            session = {"X-Session-ID": "e2e", **ep.headers}
            _post_json(url, session, {"generated_code": "x = 40", "language": "ipython", "timeout": 30})
            out = _post_json(url, session, {"generated_code": "print(x + 2)", "language": "ipython", "timeout": 30})
            assert out.get("stdout", "").strip() == "42", out
            # An undeclared port is refused before anything reaches the cluster.
            with pytest.raises(ValueError):
                await provider.endpoint(handle, RUNTIME_PORT + 1)
        finally:
            if handle is not None:
                await provider.close(handle)
            await provider.aclose()

    asyncio.run(scenario())
