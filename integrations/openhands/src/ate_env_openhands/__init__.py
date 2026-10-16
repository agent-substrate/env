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

"""OpenHands workspaces on Agent Substrate environments (ate-env).

``AteEnvWorkspace`` runs the OpenHands agent-server in an ate-env environment
created from any task image, and connects the OpenHands SDK to it through
ate-env-api's port proxy. It mirrors OpenHands' ``AgentSandboxWorkspace``
(openhands-workspace, kubernetes-sigs/agent-sandbox), so code written against
that workspace moves over by changing the constructor.
"""

from .workspace import AteEnvWorkspace, port_url

__all__ = ["AteEnvWorkspace", "port_url"]
