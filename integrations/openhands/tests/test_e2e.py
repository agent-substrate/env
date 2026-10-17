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

"""AteEnvWorkspace against a live ate-env-api.

Skipped unless SUBSTRATE_E2E_API_URL points at a reachable ate-env-api with the
port proxy, for example after

    kubectl port-forward -n ate-env svc/ate-env-api 7777:7777
    export SUBSTRATE_E2E_API_URL=127.0.0.1:7777

and OH_E2E_TEMPLATE names a base template with the agent-server layer (README,
"Register the base template"), OH_E2E_SESSION_KEY is its session key, and
OH_E2E_IMAGE is optionally a digest-pinned task image.
"""

from __future__ import annotations

import os

import pytest

from ate_env_openhands import AteEnvWorkspace

API_URL = os.environ.get("SUBSTRATE_E2E_API_URL")
TEMPLATE = os.environ.get("OH_E2E_TEMPLATE")
KEY = os.environ.get("OH_E2E_SESSION_KEY")

pytestmark = pytest.mark.skipif(
    not (API_URL and TEMPLATE and KEY),
    reason="needs SUBSTRATE_E2E_API_URL, OH_E2E_TEMPLATE and OH_E2E_SESSION_KEY",
)


def test_create_run_suspend_resume_delete():
    with AteEnvWorkspace(
        template=TEMPLATE,
        image=os.environ.get("OH_E2E_IMAGE") or None,
        api_url=API_URL,
        api_key=KEY,
        working_dir="/tmp/oh-e2e",
    ) as ws:
        env_id = ws.env_id
        result = ws.execute_command("echo kept > /tmp/oh-e2e/marker && uname -s", timeout=60)
        assert (result.exit_code, result.stdout.strip()) == (0, "Linux")
        ws.pause()
        ws.resume()
        assert ws.execute_command("cat /tmp/oh-e2e/marker", timeout=60).stdout.strip() == "kept"
    with pytest.raises(Exception):
        AteEnvWorkspace(env_id=env_id, api_url=API_URL, api_key=KEY)
