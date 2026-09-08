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

# Regenerates the committed gRPC/protobuf stubs under src/ate_env/_gen.
# Requires grpcio-tools: pip install -e 'clients/python[dev]'.
set -o errexit -o nounset -o pipefail

ROOT="$(git rev-parse --show-toplevel)"
cd "${ROOT}"

PYTHON="${PYTHON:-python3}"
OUT_DIR="clients/python/src/ate_env/_gen"

# -I proto (not proto/ateenv/v1alpha) so descriptors register under the
# collision-safe filenames ateenv/v1alpha/{env,guest}.proto.
"${PYTHON}" -m grpc_tools.protoc \
  -I proto \
  --python_out="${OUT_DIR}" \
  --pyi_out="${OUT_DIR}" \
  --grpc_python_out="${OUT_DIR}" \
  proto/ateenv/v1alpha/env.proto \
  proto/ateenv/v1alpha/guest.proto

# protoc emits `from ateenv.v1alpha import env_pb2 as ...` in *_pb2_grpc.py, which
# does not resolve inside the _gen package; rewrite to relative imports.
# sed -i with a backup suffix is the only form portable across GNU and BSD sed.
for f in "${OUT_DIR}"/ateenv/v1alpha/*_pb2_grpc.py; do
  sed -i.bak 's/^from ateenv\.v1alpha import \(.*\)$/from . import \1/' "${f}"
  rm -f "${f}.bak"
done
