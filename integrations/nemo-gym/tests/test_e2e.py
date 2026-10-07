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

"""End-to-end test against a live ate-env deployment.

Skipped unless SUBSTRATE_E2E_API_URL points at a reachable ate-env-api (e.g.
`kubectl port-forward -n ate-env svc/ate-env-api 7777:7777` then
`SUBSTRATE_E2E_API_URL=127.0.0.1:7777 pytest tests/test_e2e.py`; an http://
URL works too). SUBSTRATE_E2E_TEMPLATE picks the ActorTemplate (default
`default-template`, ate-env's own default).
Exercises the full provider lifecycle on a real Substrate actor:
create → exec (incl. nonzero exit, env/cwd) → file round-trip → status →
close → status STOPPED.
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
