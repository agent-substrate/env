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

"""Run a conversation whose agent-server lives in an ate-env environment.

The ate-env counterpart of OpenHands'
examples/02_remote_agent_server/17_convo_with_agent_sandbox_server.py: the
workspace line changes, the rest is the same. The environment runs TASK_IMAGE
unmodified; the agent-server comes from the base template's runtime layer.

Prerequisites (see ../README.md):
  1. ate-env-api reachable, for example
       kubectl port-forward -n ate-env svc/ate-env-api 7777:7777
  2. The base template with the agent-server layer, registered once
     (README, "Register the base template").
  3. pip install ./clients/python './integrations/openhands[examples]'
  4. export LLM_API_KEY=...   (and optionally LLM_MODEL, LLM_BASE_URL)
     export SESSION_KEY=<the template's OH_SESSION_API_KEYS_0>
     export TASK_IMAGE=<repo>@sha256:<digest>   (optional; default: the template's image)

The LLM is called from inside the environment (the agent runs on the
agent-server), thus Substrate's egress policy must allow the model endpoint.
"""

import os
import time

from openhands.sdk import (
    LLM,
    Conversation,
    RemoteConversation,
    get_logger,
)
from openhands.tools.preset.default import get_default_agent
from pydantic import SecretStr

from ate_env_openhands import AteEnvWorkspace

logger = get_logger(__name__)

# 1) LLM configuration
api_key = os.getenv("LLM_API_KEY")
assert api_key is not None, "LLM_API_KEY environment variable is not set."

llm = LLM(
    usage_id="agent",
    model=os.getenv("LLM_MODEL", "gpt-5.5"),
    base_url=os.getenv("LLM_BASE_URL"),
    api_key=SecretStr(api_key),
)

# 2) Create an environment from the task image. The agent-server answers once
#    the environment is ready; a warm or snapshotted image starts in seconds.
template = os.getenv("ATE_ENV_TEMPLATE", "oh-agent-server")
logger.info(f"Creating an environment from template {template!r}...")
with AteEnvWorkspace(
    template=template,
    image=os.getenv("TASK_IMAGE"),
    api_url=os.getenv("API", "127.0.0.1:7777"),
    api_key=os.environ["SESSION_KEY"],
) as workspace:
    # 3) Create the agent
    agent = get_default_agent(llm=llm, cli_mode=True)

    # 4) Sanity-check the workspace with a direct command
    result = workspace.execute_command("echo 'Hello from ate-env!' && pwd")
    logger.info(f"Command exit code: {result.exit_code}")
    logger.info(f"Output: {result.stdout}")

    # 5) Run a conversation
    conversation = Conversation(agent=agent, workspace=workspace)
    assert isinstance(conversation, RemoteConversation)
    try:
        logger.info(f"Conversation ID: {conversation.state.id}")
        conversation.send_message(
            "Read the current repo and write 3 facts about the project into FACTS.txt."
        )
        conversation.run()
        logger.info(f"Agent status: {conversation.state.execution_status}")

        # 6) Pause and resume. Substrate suspends the environment and keeps its
        #    filesystem in a snapshot; the next request resumes it.
        logger.info("Suspending the environment...")
        workspace.pause()
        time.sleep(3)
        logger.info("Resuming the environment...")
        workspace.resume()

        conversation.send_message("Great! Now append a 4th fact to FACTS.txt.")
        conversation.run()
        logger.info("Second task completed after resume.")

        cost = conversation.conversation_stats.get_combined_metrics().accumulated_cost
        print(f"EXAMPLE_COST: {cost}")
    finally:
        print("\nCleaning up conversation...")
        conversation.close()
