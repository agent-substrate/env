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

# Bake ate-env-guest onto an arbitrary base image (Mode A: runtime baked in).
#
# ko builds the guest Go binary as a layer on top of BASE and pushes the result
# to REPO, printing the digest-pinned image ref (which substrate requires for
# ActorTemplates). The guest is a static binary, so BASE can be any distro.
#
# Usage:
#   bake-task-image.sh <base-image> <output-repo> [env-repo-path]
#
# Example:
#   integrations/nemo-gym/hack/bake-task-image.sh python:3.12-slim gcr.io/PROJECT/py-task-guest
#
# Env:
#   ATE_ENV_REPO   path to an agent-substrate/env checkout (default: this repo)
#   KO             ko binary (default: ko on PATH, else ~/go/bin/ko)
set -o errexit -o nounset -o pipefail

if [[ $# -lt 2 ]]; then
  sed -n '18,32p' "$0" >&2
  exit 2
fi

BASE="$1"
REPO="$2"
ENV_REPO="${3:-${ATE_ENV_REPO:-$(cd "$(dirname "$0")/../../.." && pwd)}}"
KO="${KO:-$(command -v ko || echo "${HOME}/go/bin/ko")}"
GUEST_PKG="github.com/agent-substrate/env/cmd/ate-env-guest"

[[ -x "${KO}" ]] || { echo "ko not found (set \$KO or install to ~/go/bin)" >&2; exit 1; }
[[ -d "${ENV_REPO}/cmd/ate-env-guest" ]] || {
  echo "env repo not found at ${ENV_REPO} (set \$ATE_ENV_REPO)" >&2; exit 1; }

# A ko config that overrides the guest's base image to BASE. This overrides the
# repo's own .ko.yaml (which pins bash:latest), so the guest lands on BASE's
# rootfs instead.
# ko infers the config format from the file extension, so the path must end in
# .yaml (macOS mktemp -t appends a random suffix, which breaks that) — use a
# temp dir holding a real .ko.yaml.
CFG_DIR="$(mktemp -d -t ko-bake-XXXX)"
CFG="${CFG_DIR}/.ko.yaml"
trap 'rm -rf "${CFG_DIR}"' EXIT
cat > "${CFG}" <<EOF
baseImageOverrides:
  ${GUEST_PKG}: ${BASE}
EOF

echo "baking ${GUEST_PKG} onto ${BASE} -> ${REPO} ..." >&2
cd "${ENV_REPO}"
IMAGE="$(
  KO_DOCKER_REPO="${REPO}" \
  KO_CONFIG_PATH="${CFG}" \
  KO_DEFAULTPLATFORMS="${KO_DEFAULTPLATFORMS:-linux/amd64}" \
  "${KO}" build --bare ./cmd/ate-env-guest
)"
echo "${IMAGE}"
