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

# Run swebench-loader inside the cluster, next to ate-env-api, and bring the
# events file back. The dataset and offline-exclusions.txt are baked into the
# loader image (ko's kodata directory), so the pod needs no bucket access; a
# sidecar keeps the pod alive after the loader exits so the results can be
# copied out with kubectl cp.
#
#   KO_DOCKER_REPO=gcr.io/PROJECT/repo \
#   benchmarking/swebench/run-in-cluster.sh RUN_ID DATASET.jsonl [swebench-loader run flags...]
#
# RUN_ID must be a DNS label (lowercase letters, digits, '-'). Results land in
# ./RUN_ID.jsonl and ./RUN_ID.json in the current directory. NAMESPACE
# defaults to ate-env.

set -o errexit -o nounset -o pipefail

if [[ $# -lt 2 ]]; then
  echo "usage: $0 RUN_ID DATASET.jsonl [swebench-loader run flags...]" >&2
  exit 2
fi
RUN_ID="$1"
DATASET="$2"
shift 2
NS="${NAMESPACE:-ate-env}"
: "${KO_DOCKER_REPO:?set KO_DOCKER_REPO to the registry path the cluster can pull from}"
[[ "${RUN_ID}" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]] || { echo "RUN_ID must be a DNS label" >&2; exit 2; }
OUT_DIR="$(pwd)"

ROOT="$(git rev-parse --show-toplevel)"
cd "${ROOT}"
KODATA=cmd/benchmarking/swebench-loader/kodata
rm -rf "${KODATA}" && mkdir -p "${KODATA}"
cp "${DATASET}" "${KODATA}/dataset.jsonl"
cp benchmarking/swebench/offline-exclusions.txt "${KODATA}/offline-exclusions.txt"
IMAGE="$(KO_DOCKER_REPO="${KO_DOCKER_REPO}/swebench-loader" ko build --bare ./cmd/benchmarking/swebench-loader)"
rm -rf "${KODATA}"
echo "loader image ${IMAGE}"

JOB="swebench-loader-${RUN_ID}"
ARGS=(run "-endpoint=ate-env-api.${NS}.svc:7777" "-dataset=/var/run/ko/dataset.jsonl"
  "-exclude-file=/var/run/ko/offline-exclusions.txt" "-run-id=${RUN_ID}" "-out=/work/${RUN_ID}.jsonl" "$@")
ARGS_YAML=""
for a in "${ARGS[@]}"; do ARGS_YAML+="            - \"${a}\""$'\n'; done

kubectl -n "${NS}" delete job "${JOB}" --ignore-not-found --wait=true >/dev/null
cat <<EOF_JOB | kubectl apply -f -
apiVersion: batch/v1
kind: Job
metadata:
  name: ${JOB}
  namespace: ${NS}
spec:
  backoffLimit: 0
  template:
    metadata:
      labels:
        app: swebench-loader
    spec:
      restartPolicy: Never
      volumes:
        - name: work
          emptyDir: {}
      containers:
        - name: loader
          image: ${IMAGE}
          args:
${ARGS_YAML}          volumeMounts:
            - name: work
              mountPath: /work
          resources:
            requests:
              cpu: "2"
              memory: 2Gi
        - name: keep
          image: busybox:1.36
          command: ["sh", "-c", "sleep 172800"]
          volumeMounts:
            - name: work
              mountPath: /work
EOF_JOB

echo "waiting for the pod"
for _ in $(seq 1 60); do
  POD="$(kubectl -n "${NS}" get pod -l "job-name=${JOB}" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
  [[ -n "${POD}" ]] && kubectl -n "${NS}" get pod "${POD}" -o jsonpath='{.status.containerStatuses[?(@.name=="loader")].state}' 2>/dev/null | grep -q 'running\|terminated' && break
  sleep 5
done
[[ -n "${POD:-}" ]] || { echo "pod for ${JOB} did not appear" >&2; exit 1; }
echo "pod ${POD}; following the loader log (step failures filtered)"
kubectl -n "${NS}" logs -f "${POD}" -c loader | grep -v '"step failed"' || true

until EXIT="$(kubectl -n "${NS}" get pod "${POD}" -o jsonpath='{.status.containerStatuses[?(@.name=="loader")].state.terminated.exitCode}')" && [[ -n "${EXIT}" ]]; do
  sleep 10
done
kubectl -n "${NS}" cp -c keep "${POD}:/work/${RUN_ID}.jsonl" "${OUT_DIR}/${RUN_ID}.jsonl"
kubectl -n "${NS}" delete job "${JOB}" --wait=false >/dev/null
echo "loader exit code ${EXIT}; events in ${OUT_DIR}/${RUN_ID}.jsonl"
go run ./cmd/benchmarking/swebench-loader report "${OUT_DIR}/${RUN_ID}.jsonl" -json "${OUT_DIR}/${RUN_ID}.json"
