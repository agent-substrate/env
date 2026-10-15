#!/usr/bin/env bash
#
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

# Copy SWE-bench task images into a registry the cluster's nodes can pull
# from, by digest, with crane. Nodes pull with their own identity, and the
# shared mirrors deny it, so every image the benchmark uses must live in the
# project's own registry.
#
#   mirror-images.sh SRC_REPO DST_REPO TAGS_FILE [PARALLEL]
#
# TAGS_FILE has one tag per line (the same tags make-dataset.py resolves).
# Tags already present in DST_REPO are skipped. Needs crane on PATH and
# credentials for both registries (gcloud auth configure-docker).

set -o errexit -o nounset -o pipefail

if [[ $# -lt 3 ]]; then
  echo "usage: $0 SRC_REPO DST_REPO TAGS_FILE [PARALLEL]" >&2
  exit 2
fi

SRC="$1"
DST="$2"
TAGS="$3"
PARALLEL="${4:-8}"

command -v crane >/dev/null || { echo "crane not found on PATH" >&2; exit 1; }

existing="$(crane ls "${DST}" 2>/dev/null || true)"

copy_one() {
  local tag="$1"
  if grep -qx "${tag}" <<<"${existing}"; then
    echo "skip ${tag} (present)"
    return 0
  fi
  if crane copy "${SRC}:${tag}" "${DST}:${tag}" >/dev/null 2>&1; then
    echo "copied ${tag}"
  else
    echo "FAILED ${tag}" >&2
    return 1
  fi
}
export -f copy_one
export SRC DST existing

grep -v '^\s*$' "${TAGS}" | xargs -P "${PARALLEL}" -I{} bash -c 'copy_one "$@"' _ {}
