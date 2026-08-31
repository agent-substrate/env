# Set up Claude Code on Substrate environments (local kind)

End-to-end setup for running an unmodified coding agent — Claude Code, or
any MCP-capable harness — with its shell and file operations executing
inside Substrate environments on a local kind cluster. Environments
**auto-suspend** when idle and resume transparently on the next tool call.

Once you're set up, [demo-kind.md](demo-kind.md) has a scripted demo that
shows the whole thing off in five beats.

```
Claude Code ──HTTP MCP──▶ ate-env-api ──gRPC (h2c)──▶ atenet-router ──▶ actor (ate-env-guest)
                              │
                              └── idle reaper: suspends environments after -idle-ttl of inactivity
```

ate-env-api serves standard MCP over streamable HTTP at
`/v1alpha/envs/{id}/mcp` (7 tools: `shell`, `read_file`, `write_file`,
`start_process`, `get_process`, `stream_process_logs`, `kill_process`) and
translates them into `ateenv.v1` ProcessService/FileSystemService gRPC calls
proxied to the guest through the atenet router. No stdio bridge, no local
binary between the harness and the cluster — just a URL.

**Why streams don't break things:** the router bounds every request with its
route timeout (10s by default), and an environment can be suspended mid-
command. The data plane therefore treats streams as disposable — the MCP
tools and the Go client reconnect by process id + byte offset until the
process exits (`internal/procstream`). A 75s command, or a background job
that gets suspended and resumed halfway, loses nothing.

## Prerequisites

Go, kubectl, docker, and the Claude Code CLI, plus local checkouts of
[`substrate`](https://github.com/agent-substrate/substrate) and
[`env`](https://github.com/agent-substrate/env) side by side (this guide
assumes `~/github/agent-substrate/{substrate,env}`; both at current `main`).
`kind` and `ko` are **not** prerequisites: Substrate's `hack/` scripts manage
them at pinned versions via Go.

All command blocks are verified with zsh on macOS (arm64). One zsh gotcha:
stock zsh (no oh-my-zsh) doesn't treat `#` as a comment on interactive
lines, so before pasting blocks that contain `# checkpoint` lines, run
`setopt interactive_comments` once (or add it to `~/.zshrc`). The blocks
avoid anything that breaks harder than a noisy `command not found: #`.

Networking model: cluster nodes have **no outbound internet**. Every image
comes from the local kind registry (`localhost:5001`). The api is reached
from the host through a small always-on relay container (Phase 3) — not a
`kubectl port-forward`, which dies every time the api pod rolls.

## Phase 0 — one-time host prep

[Optional] Two things kill kind clusters on a laptop, and both look like "no container
comes up healthy":

1. **inotify limits.** This one is not superstition: a full Substrate
   install sits at ~114 inotify instances in steady state, and the kernel
   default cap is 128 per user — install-time churn blows past it, at which
   point kube-proxy crashloops with "too many open files", coredns stays
   unready, and the install hangs. Raise the limits in the Docker VM before
   creating the cluster:

   ```bash
   docker run --rm --privileged busybox \
     sysctl -w fs.inotify.max_user_instances=1024 fs.inotify.max_user_watches=1048576
   ```

   VM sysctls reset on every VM restart (Lima, Colima, and Docker Desktop
   alike, unless you persist them in the VM's /etc/sysctl.d) — re-run this
   after restarting Docker.

2. **Memory.** Give the Docker VM ≥ 8 GB and run **one** kind cluster at a
   time.

## Phase 1 — cluster + Substrate
```bash
cd substrate
hack/create-kind-cluster.sh
hack/install-ate-kind.sh --deploy-ate-system --rollout-timeout 300s
kubectl -n ate-system get pods
```

Checkpoint: all pods Running — postgres/rustfs may restart once while
settling.

If the readiness wait still times out on a cold machine (components restart
once or twice while their dependencies come up), just re-run the install —
it is idempotent.

**Health gate — verify Substrate itself with the counter demo before
touching env pieces.** If this fails, the problem is the cluster or
Substrate, not this project:

```bash
hack/install-ate-kind.sh --deploy-demo-counter
go build -o /tmp/kubectl-ate ./cmd/kubectl-ate && export PATH=/tmp:$PATH
kubectl ate create atespace demo
kubectl ate create actor my-counter-1 -a demo --template=ate-demo-counter/counter

kubectl port-forward -n ate-system svc/atenet-router 8000:80 &
sleep 2
curl -X POST -H "Host: my-counter-1.demo.actors.resources.substrate.ate.dev" -i http://localhost:8000/
# checkpoint: HTTP 200 with a counter value

# cleanup of the gate, including the router port-forward job:
kubectl ate delete actor my-counter-1 -a demo && kubectl ate delete atespace demo
kill %%
```

## Phase 2 — env system

Images build with Substrate's pinned ko into the local registry.
(`run-tool.sh` resolves tools per-repo, so grab the ko binary path from the
substrate checkout and invoke it from env.)

This block is safe to re-run whenever either repo changes — see the note
below on why the ActorTemplate delete is part of it.

```bash
cd ../env
KO_BIN=$(cd ../substrate && hack/run-tool.sh --print-bin-path ko)
export KO_DEFAULTPLATFORMS=linux/$(go env GOARCH)

guest=$(KO_DOCKER_REPO=localhost:5001/ate-env-guest "$KO_BIN" build --bare ./cmd/ate-env-guest)
api=$(KO_DOCKER_REPO=localhost:5001/ate-env-api     "$KO_BIN" build --bare ./cmd/ate-env-api)
ateom=$(cd ../substrate && KO_DOCKER_REPO=localhost:5001/ateom-gvisor hack/run-tool.sh ko build --bare ./cmd/ateom-gvisor)

go run ./cmd/ate-env manifest \
  --guest-image "$guest" --api-image "$api" --ateom-image "$ateom" \
  --replicas 2 --idle-ttl 90s --api-node-port 30777 \
  --snapshots-bucket gs://ate-snapshots/ate-env/ \
  > /tmp/ate-env-manifests.yaml
kubectl -n ate-env delete actortemplate default-env --ignore-not-found
kubectl apply -f /tmp/ate-env-manifests.yaml

kubectl -n ate-env wait --for=condition=Ready actortemplate/default-env --timeout=5m
kubectl -n ate-env rollout status deploy/ate-env-api
```

Flag notes: `gs://` is right on kind — the atelet kind overlay redirects
snapshot storage to the in-cluster rustfs. Use `--replicas 1` on a small
Docker VM. `--idle-ttl 90s` makes auto-suspend easy to watch during the
demo; day-to-day, omit it for the 5m default. `--api-node-port 30777` is
what Phase 3's persistent endpoint relays to.

**Why the delete:** Go embeds the VCS revision into binaries, so **every new
commit changes every image digest** even when the code is untouched — and
`ActorTemplate.spec` is immutable, so a re-apply with a new guest digest is
rejected. Deleting the template first makes the block idempotent; it is a
no-op on the first run. Existing environments are unaffected: they resume
from their snapshots (still on the guest image they were created with) and
only pick up a new guest image when recreated.

## Phase 3 — a persistent endpoint + first environment

`kubectl port-forward` is the obvious way to reach the api, but if we want 
something more persistent, we can expose the NodePort from Phase 2 through a
tiny relay container on the kind docker network. It restarts with Docker
(`--restart=always`), and needs no kubectl process:

```bash
docker run -d --name ate-env-endpoint --restart=always --network kind \
  -p 7777:7777 alpine/socat \
  tcp-listen:7777,fork,reuseaddr tcp-connect:kind-control-plane:30777
```

(The container runs on the Docker host, which has internet — the
no-egress rule only applies to cluster nodes. If a call lands exactly while
the api pod is mid-roll it can fail once; the very next call works. Nothing
to restart, ever. If you prefer plain kubectl anyway:
`kubectl -n ate-env port-forward svc/ate-env-api 7777:7777 &` does the same
job, minus the persistence.)

The first actor pulls the guest image onto a worker; do it before going live:

```bash
go build -o /tmp/ate-env ./cmd/ate-env

/tmp/ate-env create demo1
/tmp/ate-env demo1 shell "uname -a"
# checkpoint: "Linux actor 4.19.0-gvisor ..." — the sandbox, not your laptop
/tmp/ate-env get demo1
# checkpoint: demo1  default  running
```

With `--idle-ttl 90s`, the environment suspends ~90–120s after the last
call and any later call transparently resumes it (~1s). That is a feature,
not a failure.

## Phase 4 — wire up Claude Code

```bash
claude mcp add --transport http substrate-env http://localhost:7777/v1alpha/envs/demo1/mcp
claude
# in-session checkpoint: /mcp → substrate-env connected, 7 tools
```

There is nothing local to keep alive: the endpoint container relays
`localhost:7777` into the cluster whenever Docker is up. For a locked-down
run where local execution is impossible: `claude --disallowedTools Bash`.

Each developer/agent gets their own environment by creating another id and
registering another URL: `/tmp/ate-env create alice-dev` →
`.../envs/alice-dev/mcp`.

That's it — you're set up. Take it for a spin with the scripted
[demo](demo-kind.md), or just start working: tell Claude to use only the
substrate-env tools for shell and file operations.

## Troubleshooting

- **Install hangs / nothing healthy** → `kubectl -n kube-system get pods`
  first. kube-proxy CrashLoopBackOff "too many open files" means the Phase 0
  sysctls are missing (or reset with a VM restart): apply them, then
  `kubectl -n kube-system delete pod -l k8s-app=kube-proxy`.
- **valkey/rustfs/postgres Pending for minutes** → PVC provisioning stuck;
  `kubectl -n local-path-storage delete pod --all`. If events mention
  insufficient memory, the Docker VM is too small (Phase 0).
- **`unknown field "spec.ateomImage"` (or similar) on apply** → your env and
  substrate checkouts are skewed; pull both to current `main`
  (`go get github.com/agent-substrate/substrate@main && go mod tidy` in env).
- **`Spec is immutable` on apply** → you applied without the ActorTemplate
  delete (see Phase 2); any new commit changes the image digests.
- **One `connection error ... EOF` right after a rebuild** → the call
  landed while the api pod was rolling; the next call succeeds. If
  `localhost:7777` stays dead, check the relay:
  `docker ps --filter name=ate-env-endpoint` (recreate it with the Phase 3
  command; a deleted+recreated kind cluster needs that too, since the
  `kind` docker network is recreated).
- **`RST_STREAM ... INTERNAL_ERROR` from a raw gRPC client** → that client
  held one stream past the router's 10s route timeout. The MCP tools and the
  Go client already reconnect by offset; other clients should do the same
  (or raise the router's `--route-timeout`).
- **First-ever shell call is slow** → cold start (guest image pull onto the
  worker); Phase 3 exists to absorb it.
- **Claude reaches for local Bash** → repeat the steering line, or relaunch
  with `--disallowedTools Bash`.
- **gRPC errors somewhere in the middle** →
  `kubectl -n ate-env logs deploy/ate-env-api` first, then
  `kubectl -n ate-system logs deploy/atenet-router -c envoy`.

## Found a bug? Report it in 10 seconds

The env repo ships a Claude Code skill that gathers cluster state, versions,
and logs, drafts the issue, and files it. From a Claude Code session in the
env checkout just say:

> /report-substrate-issue the shell tool hung after I deleted the env

With the GitHub CLI available (`brew install gh && gh auth login`) it files
the issue directly; without it, it saves a ready-to-paste draft and prints
the issue URL to open manually — either way the report costs one sentence.
To have the skill available from any directory:
`ln -s "$(pwd)/.claude/skills/report-substrate-issue" ~/.claude/skills/`.
Please over-report — a one-line issue beats a lost bug.

## Cleanup

```bash
claude mcp remove substrate-env
/tmp/ate-env delete demo1
docker rm -f ate-env-endpoint
kubectl delete ns ate-env
../substrate/hack/delete-kind-cluster.sh    # cluster + registry
```
