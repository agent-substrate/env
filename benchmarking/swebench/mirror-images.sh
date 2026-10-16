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
# from, with crane. Each tag is resolved to its digest in SRC_REPO and copied
# by that digest to the same tag in DST_REPO, so a tag that moves upstream
# cannot change what was copied; a tag whose digest already matches in
# DST_REPO is skipped. Nodes pull with their own identity, and shared mirrors
# may deny it, so every image the benchmark uses must live in a registry the
# nodes can read.
#
#   mirror-images.sh SRC_REPO DST_REPO TAGS_FILE [PARALLEL]
#
# TAGS_FILE has one tag per line, for example
#   crane ls SRC_REPO | grep -v -- '-zran$' > tags.txt
# Needs crane on PATH and credentials for both registries
# (gcloud auth configure-docker).

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

copy_one() {
  local tag="$1" src_digest dst_digest
  src_digest="$(crane digest "${SRC}:${tag}" 2>/dev/null)" || { echo "FAILED ${tag}: not in ${SRC}" >&2; return 1; }
  dst_digest="$(crane digest "${DST}:${tag}" 2>/dev/null || true)"
  if [[ "${dst_digest}" == "${src_digest}" ]]; then
    echo "skip ${tag} (same digest)"
    return 0
  fi
  if crane copy "${SRC}@${src_digest}" "${DST}:${tag}" >/dev/null 2>&1; then
    echo "copied ${tag} ${src_digest}"
  else
    echo "FAILED ${tag}" >&2
    return 1
  fi
}
export -f copy_one
export SRC DST

grep -v '^\s*$' "${TAGS}" | xargs -P "${PARALLEL}" -I{} bash -c 'copy_one "$@"' _ {}
