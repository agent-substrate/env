# End-to-end demo on kind

This walk-through runs the whole story on a local
[kind](https://kind.sigs.k8s.io/) cluster: Substrate, the environment
service, the gRPC guest data plane, and an unmodified MCP harness
driving an environment through `ate-env-mcp`.

```
 harness ──MCP/stdio──▶ ate-env-mcp ──gRPC──▶ ate-env-api ──▶ atenet router ──▶ ate-env-guest
                                              (proxy)         (resumes actor    (processes,
                                                               on demand)        files)
```

Prerequisites: Docker, [kind](https://kind.sigs.k8s.io/),
[ko](https://ko.build/), kubectl, and Go.

## Phase 1 — Substrate on kind

The gRPC data plane needs an atenet router that carries HTTP/2 end to
end to actors. Until that lands on Substrate `main`, build the cluster
from the `support-grpc-atenet` branch:

```bash
cd substrate
git fetch https://github.com/LiorLieberman/agent-substrate.git support-grpc-atenet
git checkout FETCH_HEAD
```

(Once the branch merges upstream, plain `main` works and this step
reduces to a normal checkout.)

Then create the cluster and install Substrate with its kind defaults —
a local image registry on `localhost:5001` and a local `ate-snapshots`
bucket:

```bash
./hack/create-kind-cluster.sh
./hack/install-ate-kind.sh
```

## Phase 2 — Deploy the environment service

Build the env images into the kind registry and generate the
manifests. The generated Deployment already carries the projected
ServiceAccount token that ateapi's JWT auth requires.

```bash
cd env
export KO_DOCKER_REPO=localhost:5001
guest_img=$(KO_DOCKER_REPO=$KO_DOCKER_REPO/ate-env-guest ko build --bare ./cmd/ate-env-guest)
api_img=$(KO_DOCKER_REPO=$KO_DOCKER_REPO/ate-env-api ko build --bare ./cmd/ate-env-api)

go run ./cmd/ate-env manifest \
  --guest-image "$guest_img" \
  --api-image   "$api_img" \
  --ateom-image "$(kubectl -n ate-system get ds -o jsonpath='{.items[0].spec.template.spec.containers[0].image}' 2>/dev/null || echo localhost:5001/ateom-gvisor)" \
  --snapshots-bucket gs://ate-snapshots/ate-env/ | kubectl apply -f -

kubectl -n ate-env get pods   # wait for ate-env-api to be Ready
kubectl port-forward -n ate-env svc/ate-env-api 7777:7777 &
```

(For `--ateom-image`, use the same digest-pinned ateom image the
Substrate install pushed; the jsonpath above is a convenience, not a
contract.)

## Phase 3 — The gRPC data plane

Create an environment and talk to its guest over gRPC. Every call
names its environment in `ate-env-id` request metadata; the api
service forwards it opaquely, and the router resumes the actor if it
was suspended:

```bash
go run ./cmd/ate-env create dev1

grpcurl -plaintext \
  -H 'ate-env-id: dev1' \
  -d '{"command": "echo hello from the data plane"}' \
  localhost:7777 ateenv.v1alpha1.Process/Exec
```

Suspend the environment and call again — the call succeeds after the
router resumes the actor, on the same connection, with the same
handles:

```bash
go run ./cmd/ate-env suspend dev1
go run ./cmd/ate-env get dev1        # status: suspended

grpcurl -plaintext \
  -H 'ate-env-id: dev1' \
  -d '{"command": "cat /proc/uptime"}' \
  localhost:7777 ateenv.v1alpha1.Process/Exec

go run ./cmd/ate-env get dev1        # status: running again
```

The background-process loop — start, then poll by byte offset with a
bounded long-poll — is shown as a runnable program in
[examples/grpc](../examples/grpc/main.go):

```bash
go run ./examples/grpc
```

## Phase 4 — An unmodified harness over MCP

`ate-env-mcp` bridges MCP-over-stdio to the gRPC data plane. Any
MCP-capable harness can use it; its config names the binary and the
target environment:

```json
{
  "mcpServers": {
    "env": {
      "command": "ate-env-mcp",
      "args": ["-env", "dev1"]
    }
  }
}
```

The harness sees eight tools — `shell`, `read_file`, `write_file`,
`list_dir`, `start_process`, `check_process`, `write_stdin`,
`stop_process` — and nothing about actors, snapshots, or routing.
`check_process` returns only output produced since the previous check:
the bridge tracks each process's read offsets, so the polling loop
lives below the harness, not in it.

To exercise it by hand without a harness:

```bash
go install ./cmd/ate-env-mcp
npx @modelcontextprotocol/inspector ate-env-mcp -env dev1
```

## Phase 5 — Idle suspension

Run the api service with `-idle-ttl` (add `"-idle-ttl", "2m"` to the
Deployment args, or run it locally) and environments suspend on their
own after the TTL of inactivity — pinned open only while a call, a
long-poll, or an upload is in flight. The next data-plane call resumes
them transparently.

## Cleanup

```bash
kind delete cluster
```
