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
 │  Client  │   │              ├───────────▶│   ateapi   │  Substrate control plane
 │  sbx CLI ├──▶│   sbx-api    │            ╰────────────╯
 ╰──────────╯   │ (API server) │  cmd/fs    ╭────────────╮     ╭──────────────────────╮
                │              ├───────────▶│   atenet   ├────▶│ actor                │
                ╰──────────────╯            │   router   │     │  └ sbx-guest         │
                                            ╰────────────╯     │    /v1/sandboxes/*   │
                                                               ╰──────────────────────╯
```

- **`cmd/sbx`** — Provides a CLI over the API, and utilies to
  make it easier to deploy Agent Substrate.
- **`cmd/sbx-api`** — The API service that bridges clients to
  the Substrate control plane and router.
- **`cmd/sbx-guest`** — The daemon server available in the sandbox. It runs
  inside every actor and serves command executions and filesystem operations.
- **`sandbox`** — The Go client library that allows creation, suspension,
resumption, and deletion of sandboxes; as well as file operations and running remote
commands on the sandboxes.

## Installation

```bash
go install github.com/agent-substrate/sandbox/cmd/sbx@latest
```

## Quickstart

Prerequisites: a cluster with [Agent Substrate](https://github.com/agent-substrate/substrate)
installed and a snapshots bucket.

First, deploy the system — namespace, worker pool, sandbox template, and
API:

```bash
sbx deploy \
  --guest-image gcr.io/dberkov-gke-dev3/sbx-guest@sha256:39d297c40b001df665c46e97d01182d6a41258749b5ea39e4dac53fd64325fc3 \
  --api-image   gcr.io/dberkov-gke-dev3/sbx-api@sha256:b66ec161301934f85536876120db79b9a82504084fcdc96228220bad5765c028 \
  --ateom-image gcr.io/dberkov-gke-dev3/ateom-gvisor@sha256:9b55c9ff2d3ee1de088377be0176ddd61048479a3deb32a065103ff10fd437b4 \
  --snapshots-bucket gs://$GCS_BUCKET/ate-sandbox/ | kubectl apply -f -

# Ensure that the pods are running:
kubectl get pods -n ate-sandbox
```

Then create and use a sandbox:

```bash
# Port-forward the sandbox API.
kubectl port-forward -n ate-sandbox svc/sbx-api 7777:7777 &

# Create and use a sandbox.
sbx create dev1 --template sandbox
sbx cmd dev1 'echo hello > /workspace/note.txt'
sbx suspend dev1
sbx resume dev1
sbx cmd dev1 'cat /workspace/note.txt' # prints hello
sbx delete dev1
```

Or use the API directly:

```bash
curl -X POST localhost:7777/v1/sandboxes -d '{"id":"dev1","template":"sandbox"}'
curl -X POST localhost:7777/v1/sandboxes/dev1/cmd \
     -d '{"command":["sh","-c","uname -a"]}'
# Alternatively, use built-in tools.
curl -X POST localhost:7777/v1/sandboxes/dev1/tools \
     -d '{"type":"function_call","id":"call_1","name":"read_file","arguments":{"path":"/workspace/note.txt"}}'
curl -X POST localhost:7777/v1/sandboxes/dev1/tools \
     -d '{"type":"function_call","id":"call_2","name":"browser","arguments":{"url":"https://example.com"}}'
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

## API

The API server provides sandbox management and guest operations over the API. Alternatively, a large number of users may find the [built-in tools](#built-in-tools) the primary way to run these operations.

### Sandboxes

| Method   | Path                 | Description                            |
| -------- | -------------------- | -------------------------------------- |
| `POST`   | `/v1/sandboxes`      | Create a sandbox                        |
| `DELETE` | `/v1/sandboxes/{id}` | Delete (suspends first if running)      |

Create body:

```json
{
  "id": "dev1",
  "template": "sandbox",
  "namespace": "ate-sandbox"
}
```

### Lifecycle

| Method | Path                         | Description                              |
| ------ | ---------------------------- | ---------------------------------------- |
| `POST` | `/v1/sandboxes/{id}/suspend` | Snapshot to object storage, free worker  |
| `POST` | `/v1/sandboxes/{id}/resume`  | Restore from the latest snapshot         |

### Commands

`POST /v1/sandboxes/{id}/cmd`

```json
{                                           {
  "command": ["sh", "-c", "make test"],       "stdout": "ok\n",
  "cwd": "/workspace/app",                    "stderr": "",
  "env": {"VERBOSE_LOGS": "true"}             "exitCode": 0,
                                              "duration": "1.2s"
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

| Tool | Category | Description |
| ---- | -------- | ----------- |
| `read_file` | Filesystem | Read a text file with optional line numbers or line ranges |
| `write_file` | Filesystem | Create or overwrite a file |
| `edit_file` | Filesystem | Replace exact text matching target content in a file |
| `list_dir` | Filesystem | List directory entries, file types, and sizes |
| `glob` | Filesystem | Search for files matching glob patterns |
| `grep` | Filesystem | Search for text or regular expressions across files |
| `stat` | Filesystem | Get file or directory metadata (size, mode, modtime) |
| `mkdir` | Filesystem | Create a directory including parent directories |
| `mv` | Filesystem | Move or rename a file or directory |
| `rm` | Filesystem | Remove a file or directory recursively |
| `shell` | Shell | Run a shell command line inside the sandbox |
| `browser` | Web | Fetch a web page or API over HTTP(S) and render HTML to Markdown |

TODO: Add support for skills e.g. generaate available_skills, and activate a skill.

### Tool Definitions

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

### Tool Execution

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

curl -X POST localhost:7777/v1/sandboxes/dev1/tools \
     -d '{"type":"function_call","id":"call_2","name":"browser","arguments":{"url":"https://example.com"}}'
{
  "type": "function_result",
  "name": "browser",
  "call_id": "call_2",
  "result": [
    { "type": "text", "text": "https://example.com\nstatus: 200 OK\ncontent-type: text/html\nbytes: 1256\ntitle: Example Domain\n\n# Example Domain\n\nThis domain is for use in illustrative examples..." }
  ]
}
```

## Client Library

Users can use the `sandbox` package directly for lifecycle operations to manage
sandboxes programatically, and executing operations on the guest.

```go
client, err := sandbox.NewClient(sandbox.ClientOptions{
    Endpoint: "http://localhost:7777",          // sbx-api endpoint
    Workdir:  "/workspace",                     // default base directory for relative paths
})
if err != nil {
    log.Fatalf("connecting to Substrate: %v", err)
}
defer client.Close()

sb, err := client.Create(ctx, "dev1", sandbox.WithTemplate("sandbox"))
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

## Cleanup

You can remove the sandbox deployment by running:

```bash
# Cleanup the deployment to remove Agent Substrate Sandbox from your cluster:
kubectl delete ns ate-sandbox
```