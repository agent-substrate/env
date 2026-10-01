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

# A second runtime injected as a layer, end to end.
#
# busybox stands in for a real runtime daemon (OpenSandbox execd, say): its
# image is mounted read-only at /opt/web, the guest starts `busybox httpd` on
# port 44772 as a sidecar and waits for it before reporting the actor ready.
# The task image is unmodified. The script registers the template, creates an
# environment from it, asks the second runtime for a file from inside the
# environment, creates a second environment from a different image on the
# same base to show the layer is inherited, and deletes both.
#
# Required:
#   GUEST_IMAGE      digest-pinned ate-env-guest image (built from this repo,
#                    at or after the -sidecar flags)
#   SNAPSHOTS_BUCKET object-storage URL for actor snapshots
#   TASK_IMAGE       digest-pinned task image, e.g. docker.io/library/python@sha256:...
#   LAYER_IMAGE      digest-pinned STATIC busybox image (the 1.37-musl tag),
#                    e.g. docker.io/library/busybox@sha256:...
# Optional:
#   OTHER_IMAGE      a second digest-pinned image for the inheritance check
#   TEMPLATE         base template name (default layered-demo)
#   ATESPACE         atespace (default ate-env)
#   SUBSTRATE_ENV_API  ate-env-api address (default 127.0.0.1:7777; port-forward it)
#   ATE_ENV, KUBECTL_ATE  binaries (default: on PATH)
set -euo pipefail

: "${GUEST_IMAGE:?set GUEST_IMAGE to the digest-pinned ate-env-guest image}"
: "${SNAPSHOTS_BUCKET:?set SNAPSHOTS_BUCKET to the snapshot object-storage URL}"
: "${TASK_IMAGE:?set TASK_IMAGE to a digest-pinned task image}"
: "${LAYER_IMAGE:?set LAYER_IMAGE to a digest-pinned busybox image}"
TEMPLATE=${TEMPLATE:-layered-demo}
ATESPACE=${ATESPACE:-ate-env}
export SUBSTRATE_ENV_API=${SUBSTRATE_ENV_API:-127.0.0.1:7777}
ATE_ENV=${ATE_ENV:-ate-env}
KUBECTL_ATE=${KUBECTL_ATE:-kubectl-ate}

PORT=44772
SIDECAR="/opt/web/bin/busybox httpd -f -p ${PORT} -h /"
READYZ="http://127.0.0.1:${PORT}/etc/hostname"

step() { printf '\n== %s\n' "$*"; }

step "register ${ATESPACE}/${TEMPLATE}: ${TASK_IMAGE} + guest + busybox layer"
"${ATE_ENV}" manifest template --template "${TEMPLATE}" --atespace "${ATESPACE}" \
  --task-image "${TASK_IMAGE}" --guest-image "${GUEST_IMAGE}" \
  --snapshots-bucket "${SNAPSHOTS_BUCKET}" \
  --layer "web=${LAYER_IMAGE}=/opt/web" \
  --sidecar "${SIDECAR}" \
  --sidecar-readyz "${READYZ}" | "${KUBECTL_ATE}" create actor-template -f - >/dev/null
"${KUBECTL_ATE}" get actor-template --atespace "${ATESPACE}" "${TEMPLATE}"

ids=()
cleanup() {
  for id in "${ids[@]:-}"; do
    [ -n "${id}" ] && "${ATE_ENV}" delete "${id}" --atespace "${ATESPACE}" >/dev/null 2>&1 || true
  done
}
trap cleanup EXIT

# wait_serving <id>: the guest answers only once the sidecar's readiness URL
# has answered, so the first successful shell means both runtimes are up.
wait_serving() {
  local id=$1 i
  for i in $(seq 1 60); do
    if "${ATE_ENV}" "${id}" shell 'true' >/dev/null 2>&1; then return 0; fi
    sleep 3
  done
  echo "environment ${id} did not start serving" >&2
  return 1
}

ID="layered-$(date +%s)"
ids+=("${ID}")
step "create ${ID} from ${TEMPLATE}"
t0=$(date +%s)
"${ATE_ENV}" create "${ID}" --atespace "${ATESPACE}" --template "${TEMPLATE}"
wait_serving "${ID}"
echo "serving after $(( $(date +%s) - t0 ))s"

step "ask the second runtime from inside the environment"
"${ATE_ENV}" "${ID}" shell "/opt/web/bin/busybox wget -qO- http://127.0.0.1:${PORT}/etc/os-release | head -2; echo guest: \$(ls /ate/ko-app); echo layer: \$(ls /opt/web/bin | head -1)"

if [ -n "${OTHER_IMAGE:-}" ]; then
  ID2="layered2-$(date +%s)"
  ids+=("${ID2}")
  step "create ${ID2} from ${OTHER_IMAGE} with ${TEMPLATE} as the base (layer inherited)"
  "${ATE_ENV}" create "${ID2}" --atespace "${ATESPACE}" --template "${TEMPLATE}" --image "${OTHER_IMAGE}"
  "${KUBECTL_ATE}" get actor-template --atespace "${ATESPACE}" | grep "${TEMPLATE}-" || true
  wait_serving "${ID2}"
  "${ATE_ENV}" "${ID2}" shell "/opt/web/bin/busybox wget -qO- http://127.0.0.1:${PORT}/etc/os-release | head -1"
fi

step "done; deleting"
