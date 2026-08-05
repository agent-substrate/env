# 📦 Agent Substrate Sandbox

> [!WARNING]
> This is an alpha API and is likely to change until v1.0 is released.

A sandboxing service on top of [Agent Substrate](https://github.com/agent-substrate/substrate): isolated, stateful execution environments
that can be **suspended**, **resumed** on any available worker,
and driven remotely with **command execution** and **filesystem operations**.

Each sandbox is a Substrate *actor* running in an isolated container.
Substrate provides the heavy lifting — snapshotting, scheduling,
multiplexing many idle sandboxes onto a small worker pool, and routing —
while this project adds the sandbox-shaped API on top.

## Overview

```
 ╭──────────╮   ╭──────────────╮  lifecycle ╭────────────╮
 │   SDK    │   │              ├───────────▶│   ateapi   │  Substrate control plane
 │  sbx CLI ├──▶│   sbx-api    │            ╰────────────╯
 ╰──────────╯   │ (API server) │  cmd/fs    ╭────────────╮     ╭──────────────────────╮
                │              ├───────────▶│   atenet   ├────▶│ actor                │
                ╰──────────────╯            │   router   │     │  └ sbx-guest         │
                                            ╰────────────╯     │    /v1/sandboxes/*   │
                                                               ╰──────────────────────╯
```

- **`sandbox`** — The Go client library that allows creation, suspension,
resumption, and deletion of sandboxes; as well as file operations and running remote
commands on the sandboxes.
- **`cmd/sbx`** — Provides a CLI over the API, and utilies to
  make it easier to deploy Agent Substrate.
- **`cmd/sbx-api`** — The API service that bridges clients to
  the Substrate control plane and router.
- **`cmd/sbx-guest`** — The daemon server available in the sandbox. It runs
  inside every actor and serves command executions and filesystem operations.

## Installation

```bash
go install github.com/agent-substrate/sandbox/cmd/sbx@latest
```

## Quickstart

Prerequisites: a cluster with [Agent Substrate](https://github.com/agent-substrate/substrate)
installed and a snapshots bucket.

First, deploy the system — namespace, worker pool, sandbox template, and
API — using the digest-pinned images published by the latest release:

<!-- release-deploy:begin (rewritten by the release workflow; do not edit) -->
```bash
# Images are pinned by release v0.0.7.
sbx deploy \
  --guest-image ghcr.io/agent-substrate/sandbox/sbx-guest@sha256:f072df0649f5d7d88cebfad0886cb606f8e6139d5a14915ad2ef60f1e4813763 \
  --api-image   ghcr.io/agent-substrate/sandbox/sbx-api@sha256:6aa9064a8f0d6e228afa52b16515e2471ecd4afc6cbad4caaa19f9775ca84b51 \
  --ateom-image ghcr.io/agent-substrate/sandbox/ateom-gvisor@sha256:ac0175e6cb1617140e9afd83416da05cd924aadac48aae847483ecab4d241627 \
  --snapshots-bucket gs://$GCS_BUCKET/substrate-sandbox/ | kubectl apply -f -
```
<!-- release-deploy:end -->

Then create and use a sandbox:

```bash
# Port-forward the sandbox API.
kubectl port-forward -n substrate-sandbox svc/sbx-api 7777:7777 &

# Create and use a sandbox.
sbx create dev1
sbx cmd dev1 'echo hello > /workspace/note.txt'
sbx suspend dev1
sbx resume dev1
sbx cmd dev1 'cat /workspace/note.txt' # prints hello
sbx delete dev1
```

Or use the API directly:

```bash
curl -X POST localhost:7777/v1/sandboxes -d '{"id":"dev1"}'
curl -X POST localhost:7777/v1/sandboxes/dev1/cmd \
     -d '{"command":["sh","-c","uname -a"]}'
```

## CLI

Lifecycle and command execution are top-level commands; file operations are
grouped under `fs`; `deploy` generates the manifests that set up the system
on a cluster:

```bash
$ sbx
Manage sandboxes on Agent Substrate

Available Commands:
  cmd         Run a shell command line in the sandbox
  create      Create and start a sandbox
  delete      Delete a sandbox
  deploy      Generate Kubernetes manifests to deploy the system
  fs          Operate on files and directories in a sandbox
  info        Show a sandbox's status
  pause       Snapshot locally on the node for fast resume
  resume      Resume from the latest snapshot
  suspend     Snapshot to external storage and free the worker

$ sbx fs
Operate on files and directories in a sandbox

Available Commands:
  ls          List a sandbox directory
  mkdir       Create a directory in the sandbox
  read        Print a sandbox file to stdout
  rm          Delete a file or directory in the sandbox
  stat        Stat a sandbox path
  write       Write stdin to a sandbox file

$ sbx deploy --help
Deploy generates Kubernetes manifests for everything sandboxes need on
a cluster that already runs the Agent Substrate system: the target
namespace, a WorkerPool of pre-warmed workers, the ActorTemplate that
sandboxes are created from, and the sbx-api service. It prints YAML to
stdout without touching the cluster; apply it with kubectl.
```

## SDK

```go
client, err := sandbox.NewClient(sandbox.ClientOptions{
    Endpoint: "http://localhost:7777",          // sbx-api
    Template: "sandbox",                        // ActorTemplate name
    Workdir:  "/workspace",                     // default base directory for relative paths
})
if err != nil {
    log.Fatalf("connecting to Substrate: %v", err)
}
defer client.Close()

sb, err := client.Create(ctx, "dev1")
if err != nil {
    log.Fatalf("creating sandbox: %v", err)
}
if err := sb.WriteFile(ctx, "main.go", src, 0o644); err != nil {
    log.Fatalf("writing main.go: %v", err)
}
res, err := sb.Cmd(ctx, "cd /workspace && go run main.go")
if err != nil {
    log.Fatalf("running main.go: %v", err)
}
fmt.Println(res.Stdout, res.ExitCode)

sb.Suspend(ctx)
sb.Resume(ctx)
sb.Delete(ctx)
```

See [examples/quickstart](examples/quickstart/main.go) for a complete
program.

## API

`sbx-api` serves the API. `sbx deploy` runs it in-cluster as the
`sbx-api` service (port 7777 by default; adjust with `--api-port`); it
can also be run standalone (default `0.0.0.0:7777`). Responses are JSON
unless noted.

### Sandboxes

| Method   | Path                 | Description                            |
| -------- | -------------------- | -------------------------------------- |
| `POST`   | `/v1/sandboxes`      | Create a sandbox                        |
| `GET`    | `/v1/sandboxes/{id}` | Get a sandbox's status                  |
| `DELETE` | `/v1/sandboxes/{id}` | Delete (suspends first if running)      |

Create body (only `id` is required):

```json
{
  "id": "dev1",
  "template": "sandbox",
  "namespace": "substrate-sandbox"
}
```

### Lifecycle

| Method | Path                         | Description                              |
| ------ | ---------------------------- | ---------------------------------------- |
| `POST` | `/v1/sandboxes/{id}/suspend` | Snapshot to object storage, free worker  |
| `POST` | `/v1/sandboxes/{id}/pause`   | Snapshot locally on node for fast resume |
| `POST` | `/v1/sandboxes/{id}/resume`  | Restore from the latest snapshot         |

Each returns the sandbox's new status: `{"id": "...", "status": "suspended", ...}`.

### Commands

`POST /v1/sandboxes/{id}/cmd`

```json
{                                           {
  "command": ["sh", "-c", "make test"],       "stdout": "ok\n",
  "cwd": "/workspace/app",                    "stderr": "",
  "env": {"VERBOSE_LOGS": "true"},            "exitCode": 0,
  "timeout": "60s"                            "duration": "1.2s"
}                                           }
```

Output is capped at 10 MiB per stream; `stdoutTruncated`/`stderrTruncated`
report when the cap was hit, and `timedOut` reports a timeout kill.

### Filesystem

All filesystem endpoints accept a JSON request body containing `"path"`.

| Method   | Path                       | Description                     |
| -------- | -------------------------- | ------------------------------- |
| `GET`    | `/v1/sandboxes/{id}/file`  | Read a file (raw bytes response)|
| `POST`   | `/v1/sandboxes/{id}/file`  | Write a file                    |
| `DELETE` | `/v1/sandboxes/{id}/file`  | Delete a file or directory      |
| `GET`    | `/v1/sandboxes/{id}/dir`   | List a directory                |
| `POST`   | `/v1/sandboxes/{id}/dir`   | Create a directory (mkdir -p)   |
| `GET`    | `/v1/sandboxes/{id}/stat`  | Stat a path                     |

Write a file, then read the raw bytes back (`content` is base64-encoded;
`mode` is an octal string defaulting to `"644"`):

```bash
curl -X POST localhost:7777/v1/sandboxes/dev1/file \
     -d '{"path": "app/main.txt", "mode": "644", "content": "aGVsbG8K"}'
curl -X GET localhost:7777/v1/sandboxes/dev1/file \
     -d '{"path": "app/main.txt"}'
```

## Built-in Tools

The API exposes built-in tools for file system operations and shell executions.

| Method | Path                        | Description                      |
| ------ | --------------------------- | -------------------------------- |
| `GET`  | `/v1/sandboxes/{id}/tools`  | List registered tool definitions |
| `POST` | `/v1/sandboxes/{id}/tools`  | Execute a tool call              |

### Available Tools

- **Filesystem**: `read_file`, `write_file`, `edit_file`, `list_dir`, `glob`, `grep`, `stat`, `mkdir`, `mv`, `rm`
- **Shell**: `shell`

### Tool Definitions (`GET /v1/sandboxes/{id}/tools`)

```bash
curl -X GET localhost:7777/v1/sandboxes/dev1/tools
{
  "tools": [
    {
      "name": "read_file",
      "description": "Read a text file from the workspace...",
      "parameters": {
        "type": "object",
        "properties": { "path": { "type": "string", "description": "..." } },
        "required": ["path"]
      }
    }
  ]
}
```

### Tool Execution (`POST /v1/sandboxes/{id}/tools`)

```bash
curl -X POST localhost:7777/v1/sandboxes/dev1/tools \
     -d '{"type":"function_call","id":"call_1","name":"read_file","arguments":{"path":"main.go"}}'
{
  "type": "function_result",
  "name": "read_file",
  "call_id": "call_1",
  "result": [
    { "type": "text", "text": "     1\tpackage main\n..." }
  ]
}
```
