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

# Demo of the NeMo runtime layer (integrations/nemo-gym/layer): registers a
# base template that mounts the layer at /opt/nemo and starts NeMo Skills'
# sandbox server (and, with EXEC_PORT set, NeMo Gym's exec server) as guest
# sidecars, creates an environment from an unmodified task image on that
# base, and calls the servers three ways: from inside the environment, from
# outside through ate-env-api's port proxy with the routing headers, and
# through the path form. Deletes the environment on exit; keeps the template.
#
# Required:  LAYER_IMAGE (digest-pinned nemo-runtime-layer), GUEST_IMAGE
#            (digest-pinned ate-env-guest), SNAPSHOTS_BUCKET, ROOTFS_IMAGE
#            (digest-pinned task image, e.g. python:3.12-slim@sha256:...)
# Optional:  TEMPLATE (gym-nemo), ATESPACE (ate-env), SUBSTRATE_ENV_API
#            (127.0.0.1:7777, ate-env-api reached by port-forward), PORT
#            (6000), EXEC_PORT (unset: no exec server), FORCE=1 (recreate the
#            template), ATE_ENV, KUBECTL_ATE
set -euo pipefail

: "${LAYER_IMAGE:?set LAYER_IMAGE to the digest-pinned nemo-runtime-layer image}"
: "${GUEST_IMAGE:?set GUEST_IMAGE to the digest-pinned ate-env-guest image}"
: "${SNAPSHOTS_BUCKET:?set SNAPSHOTS_BUCKET to the snapshot object-storage URL}"
: "${ROOTFS_IMAGE:?set ROOTFS_IMAGE to a digest-pinned task image}"
TEMPLATE=${TEMPLATE:-gym-nemo}
ATESPACE=${ATESPACE:-ate-env}
export SUBSTRATE_ENV_API=${SUBSTRATE_ENV_API:-127.0.0.1:7777}
PORT=${PORT:-6000}
EXEC_PORT=${EXEC_PORT:-}
ATE_ENV=${ATE_ENV:-ate-env}
KUBECTL_ATE=${KUBECTL_ATE:-kubectl-ate}
PY=/opt/nemo/python/bin/python3

step() { printf '\n== %s\n' "$*"; }
now() { date +%s; }

sidecars=(--sidecar "${PY} -m nemo_runtime.sandbox_server --host 0.0.0.0 --port ${PORT}"
          --sidecar-readyz "http://127.0.0.1:${PORT}/health")
if [ -n "${EXEC_PORT}" ]; then
  sidecars+=(--sidecar "${PY} -m nemo_runtime.exec_server --host 0.0.0.0 --port ${EXEC_PORT}"
             --sidecar-readyz "http://127.0.0.1:${EXEC_PORT}/health")
fi

step "template ${ATESPACE}/${TEMPLATE}: guest + NeMo runtime layer at /opt/nemo"
if "${KUBECTL_ATE}" get actor-template "${TEMPLATE}" --atespace "${ATESPACE}" >/dev/null 2>&1 && [ "${FORCE:-0}" != 1 ]; then
  echo "exists; FORCE=1 recreates it"
else
  [ "${FORCE:-0}" = 1 ] && "${KUBECTL_ATE}" delete actor-template "${TEMPLATE}" --atespace "${ATESPACE}" >/dev/null 2>&1 || true
  # The base names a task image too (--rootfs-image): the guest's own image
  # is Alpine-based and cannot run the layer's glibc interpreter, so a base
  # without one never bakes its golden snapshot. Other task images derive
  # from this base and inherit the layer.
  "${ATE_ENV}" manifest template --template "${TEMPLATE}" --atespace "${ATESPACE}" \
    --guest-image "${GUEST_IMAGE}" --snapshots-bucket "${SNAPSHOTS_BUCKET}" \
    --rootfs-image "${ROOTFS_IMAGE}" \
    --layer "nemo=${LAYER_IMAGE}=/opt/nemo" "${sidecars[@]}" \
    | "${KUBECTL_ATE}" create actor-template -f - >/dev/null
fi
"${KUBECTL_ATE}" get actor-template "${TEMPLATE}" --atespace "${ATESPACE}"

ID="nemo-$(date +%s)"
cleanup() { "${ATE_ENV}" delete "${ID}" --atespace "${ATESPACE}" >/dev/null 2>&1 || true; }
trap cleanup EXIT

step "create ${ID} from ${ROOTFS_IMAGE} on ${TEMPLATE}"
t0=$(now)
"${ATE_ENV}" create "${ID}" --atespace "${ATESPACE}" --template "${TEMPLATE}" --rootfs-image "${ROOTFS_IMAGE}"
for _ in $(seq 1 60); do "${ATE_ENV}" "${ID}" shell 'true' >/dev/null 2>&1 && break; sleep 3; done
"${ATE_ENV}" "${ID}" shell 'true' >/dev/null
echo "serving after $(( $(now) - t0 ))s (readiness waited for the sidecars' /health)"

step "inside the environment: POST /execute to 127.0.0.1:${PORT}"
"${ATE_ENV}" "${ID}" shell "${PY} -c \"import json,urllib.request; r=urllib.request.urlopen(urllib.request.Request('http://127.0.0.1:${PORT}/execute', data=json.dumps({'generated_code':'print(6*7)','language':'python','timeout':10}).encode(), headers={'Content-Type':'application/json'})); print(r.read().decode())\""

api="http://${SUBSTRATE_ENV_API}"
body='{"generated_code":"import sys; print(sys.version.split()[0]); print(6*7)","language":"python","timeout":10}'
step "outside, header form: ${api} with X-Env-Id/X-Env-Port"
t1=$(now)
curl -fsS -X POST "${api}/execute" -H "X-Env-Id: ${ID}" -H "X-Env-Atespace: ${ATESPACE}" -H "X-Env-Port: ${PORT}" \
  -H 'Content-Type: application/json' -d "${body}"; echo " ($(( $(now) - t1 ))s)"

step "outside, path form: ${api}/v1alpha/atespaces/${ATESPACE}/envs/${ID}/ports/${PORT}/execute"
curl -fsS -X POST "${api}/v1alpha/atespaces/${ATESPACE}/envs/${ID}/ports/${PORT}/execute" \
  -H 'Content-Type: application/json' -d '{"generated_code":"print(\"ipython session\"); x = 40","language":"ipython","timeout":10}' -H 'X-Session-ID: demo'; echo
curl -fsS -X POST "${api}/v1alpha/atespaces/${ATESPACE}/envs/${ID}/ports/${PORT}/execute" \
  -H 'Content-Type: application/json' -d '{"generated_code":"print(x + 2)","language":"ipython","timeout":10}' -H 'X-Session-ID: demo'; echo

if [ -n "${EXEC_PORT}" ]; then
  step "NeMo Gym exec server on ${EXEC_PORT}: POST /exec"
  # shellcheck disable=SC2016  # $(hostname) expands inside the sandbox, not here
  curl -fsS -X POST "${api}/exec" -H "X-Env-Id: ${ID}" -H "X-Env-Atespace: ${ATESPACE}" -H "X-Env-Port: ${EXEC_PORT}" \
    -H 'Content-Type: application/json' -d '{"cmd":"echo hello from $(hostname); id -u","timeout":10}'; echo
fi

step "done; deleting ${ID} (the template ${TEMPLATE} stays)"
