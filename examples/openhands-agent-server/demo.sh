#!/usr/bin/env bash
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

# The OpenHands agent-server attached to unmodified images, end to end.
#
# The agent-server comes from a runtime layer (./Dockerfile) mounted read-only
# at /opt/oh; the guest starts it as a sidecar and holds the actor's readiness
# until it answers /health. Callers outside the cluster reach it through
# ate-env-api's port proxy with three routing headers plus the agent-server's
# own X-Session-API-Key. The script registers a base template, creates one
# environment per image, drives each agent-server from outside (health, a
# command, a file download, a call without the key) and deletes what it
# created, unless KEEP=1.
#
# Required:
#   GUEST_IMAGE       digest-pinned ate-env-guest image (with the -sidecar flags)
#   SNAPSHOTS_BUCKET  object-storage URL for actor snapshots
#   LAYER_IMAGE       digest-pinned layer image built from ./Dockerfile
#   IMAGES            space-separated digest-pinned images to run the
#                     agent-server in; any image with /bin/sh
# Optional:
#   SESSION_KEY       agent-server API key on the template (default: random)
#   PORT              agent-server port in the environment (default 8000)
#   AGENT_SERVER_ENV  space-separated NAME=VALUE settings for the agent-server,
#                     e.g. "LOG_JSON=true"; each becomes a template --env
#   COMMAND           shell command the demo runs through the agent-server
#                     (default: distribution, user and working directory)
#   WORKDIR           directory COMMAND runs in (default: the image's WORKDIR)
#   KEEP              1 keeps the environments and prints how to reach them
#   TEMPLATE          base template name (default oh-agent-server)
#   ATESPACE          atespace (default ate-env)
#   API               ate-env-api address for the CLI and the proxy
#                     (default 127.0.0.1:7777; port-forward it)
#   ATE_ENV, KUBECTL_ATE  binaries (default: on PATH)
set -euo pipefail

: "${GUEST_IMAGE:?set GUEST_IMAGE to the digest-pinned ate-env-guest image}"
: "${SNAPSHOTS_BUCKET:?set SNAPSHOTS_BUCKET to the snapshot object-storage URL}"
: "${LAYER_IMAGE:?set LAYER_IMAGE to the digest-pinned agent-server layer image}"
: "${IMAGES:?set IMAGES to one or more digest-pinned images}"
SESSION_KEY=${SESSION_KEY:-$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')}
PORT=${PORT:-8000}
COMMAND=${COMMAND:-'head -1 /etc/os-release; echo "user: $(id -un)"; echo "cwd: $(pwd)"'}
TEMPLATE=${TEMPLATE:-oh-agent-server}
ATESPACE=${ATESPACE:-ate-env}
API=${API:-127.0.0.1:7777}
ATE_ENV=${ATE_ENV:-ate-env}
KUBECTL_ATE=${KUBECTL_ATE:-kubectl-ate}

step() { printf '\n== %s\n' "$*"; }

# oh <env-id> <curl args...>: one request to the agent-server in <env-id>,
# through ate-env-api.
oh() {
  local id=$1
  shift
  curl -sS --max-time 120 \
    -H "X-Env-Id: ${id}" -H "X-Env-Atespace: ${ATESPACE}" -H "X-Env-Port: ${PORT}" \
    -H "X-Session-API-Key: ${SESSION_KEY}" "$@"
}

env_flags=(--env "OH_SESSION_API_KEYS_0=${SESSION_KEY}")
for kv in ${AGENT_SERVER_ENV:-}; do
  env_flags+=(--env "${kv}")
done

step "register ${ATESPACE}/${TEMPLATE}: guest + agent-server layer on port ${PORT}"
"${ATE_ENV}" manifest template --template "${TEMPLATE}" --atespace "${ATESPACE}" \
  --guest-image "${GUEST_IMAGE}" --snapshots-bucket "${SNAPSHOTS_BUCKET}" \
  --layer "oh=${LAYER_IMAGE}=/opt/oh" \
  --sidecar "/opt/oh/python/bin/python3 -m openhands.agent_server --host 0.0.0.0 --port ${PORT}" \
  --sidecar-readyz "http://127.0.0.1:${PORT}/health" \
  "${env_flags[@]}" | "${KUBECTL_ATE}" create actor-template -f - >/dev/null

ids=()
cleanup() {
  [ "${KEEP:-}" = 1 ] && return
  for id in "${ids[@]:-}"; do
    [ -n "${id}" ] && "${ATE_ENV}" --api "${API}" delete "${id}" --atespace "${ATESPACE}" >/dev/null 2>&1 || true
  done
}
trap cleanup EXIT

body=$(COMMAND="${COMMAND}" WORKDIR="${WORKDIR:-}" python3 -c 'import json,os; d={"command": os.environ["COMMAND"], "timeout": 60}; w=os.environ.get("WORKDIR"); d.update({"cwd": w} if w else {}); print(json.dumps(d))')

n=0
for image in ${IMAGES}; do
  n=$((n + 1))
  id="oh-$(date +%s)-${n}"
  ids+=("${id}")
  step "${id}: ${image}"
  t0=$(date +%s)
  "${ATE_ENV}" --api "${API}" create "${id}" --atespace "${ATESPACE}" --template "${TEMPLATE}" --rootfs-image "${image}" >/dev/null
  # The router wakes or waits for the actor; the agent-server answers once the
  # actor is ready, which the guest gates on the same /health.
  code=000
  for _ in $(seq 1 120); do
    code=$(oh "${id}" -o /dev/null -w '%{http_code}' "http://${API}/health" || true)
    [ "${code}" = 200 ] && break
    sleep 5
  done
  if [ "${code}" != 200 ]; then
    echo "agent-server in ${id} did not answer (last status ${code})" >&2
    continue
  fi
  echo "agent-server answering after $(( $(date +%s) - t0 ))s"

  echo "-- command, run with the image's own /bin/sh:"
  oh "${id}" -X POST -H 'Content-Type: application/json' "http://${API}/api/bash/execute_bash_command" -d "${body}" |
    python3 -c 'import json,sys; r=json.load(sys.stdin); print(r.get("stdout", r).rstrip()); print("exit:", r.get("exit_code"))'
  echo "-- file download (/etc/os-release, first line):"
  oh "${id}" "http://${API}/api/file/download?path=/etc/os-release" | head -1
  echo "-- without the session key:"
  curl -sS -o /dev/null -w 'HTTP %{http_code}\n' -X POST -H 'Content-Type: application/json' \
    -H "X-Env-Id: ${id}" -H "X-Env-Atespace: ${ATESPACE}" -H "X-Env-Port: ${PORT}" \
    -d '{"command": "true"}' "http://${API}/api/bash/execute_bash_command"
done

if [ "${KEEP:-}" = 1 ]; then
  step "kept: ${ids[*]}"
  echo "reach one with: -H 'X-Env-Id: <id>' -H 'X-Env-Port: ${PORT}' -H 'X-Session-API-Key: ${SESSION_KEY}' http://${API}/..."
  echo "delete with: ${ATE_ENV} --api ${API} delete <id> --atespace ${ATESPACE}"
else
  step "done; deleting the environments (the templates stay for reuse)"
fi
