# 📦 Agent Substrate Environment

> [!WARNING]
> This is an alpha API and is likely to change until v1.0 is released.

An environment service on top of [Agent Substrate](https://github.com/agent-substrate/substrate): isolated, stateful execution environments
that can be **suspended**, **resumed** on any available worker,
and driven remotely with **command execution** and **filesystem operations**.

Each environment is a Substrate *actor* running in an isolated container.
Substrate provides the heavy lifting — snapshotting, scheduling,
multiplexing many idle environments onto a small worker pool, and routing —
while this project adds the environment-shaped API on top.

## Overview

```
 ╭──────────╮   ╭──────────────╮  lifecycle ╭────────────╮
 │  Client  │   │              ├───────────▶│   ateapi   │  Substrate control plane
 │  ate-env CLI ├──▶│   ate-env-api    │            ╰────────────╯
 ╰──────────╯   │ (API server) │  cmd/fs    ╭────────────╮     ╭──────────────────────╮
                │              ├───────────▶│   atenet   ├────▶│ actor                │
                ╰──────────────╯            │   router   │     │  └ ate-env-guest         │
                                            ╰────────────╯     │    /v1/envs/*   │
                                                               ╰──────────────────────╯
```

- **`cmd/ate-env`** — Provides a CLI over the API, and utilies to
  make it easier to deploy Agent Substrate.
- **`cmd/ate-env-api`** — The API service that bridges clients to
  the Substrate control plane and router.
- **`cmd/ate-env-guest`** — The daemon server available in the environment. It runs
  inside every actor and serves command executions and filesystem operations.
- **`env`** — The Go client library that allows creation, suspension,
resumption, and deletion of environments; as well as file operations and running remote
commands on the environments.

## Installation

```bash
go install github.com/agent-substrate/env/cmd/ate-env@latest
```

## Quickstart

Prerequisites: a cluster with [Agent Substrate](https://github.com/agent-substrate/substrate)
installed and a snapshots bucket.

First, deploy the system — namespace, worker pool, environment template, and
API:

```bash
ate-env deploy \
  --guest-image gcr.io/dberkov-gke-dev3/ate-env-guest@sha256:eb49ae877d1a168531b2654f779494b227568d8d82a24b8e8b3d06f41e511ee8 \
  --api-image   gcr.io/dberkov-gke-dev3/ate-env-api@sha256:542fa9e7223f0d54852e52ee67a630fdba920e8abf6296465f61b6f6da64408d \
  --ateom-image gcr.io/dberkov-gke-dev3/ate-images/ateom-gvisor-715889664656de67e44382a8d6ab981d@sha256:b0b6e2ad834de42cb2a4c55e83b60243f66cb85ca37575d1a6818e788e0564e0 \
  --snapshots-bucket gs://$GCS_BUCKET/ate-env/ | kubectl apply -f -

# Ensure that the pods are running:
kubectl get pods -n ate-env
```

Then create and use an environment:

```bash
# Port-forward the ate-env-api API.
kubectl port-forward -n ate-env svc/ate-env-api 7777:7777 &

# Create and use an environment. Environment is suspended and resumed
# automatically after each command.
ate-env create dev1 --template env
ate-env dev1 cmd 'echo hello > /note.txt'
ate-env dev1 cmd 'cat /note.txt' # prints hello
ate-env delete dev1
```

Or use the API directly:

```bash
curl -X POST localhost:7777/v1/envs -d '{"id":"dev1","template":"env"}'
curl -X POST localhost:7777/v1/envs/dev1/cmd \
     -d '{"command":["sh","-c","uname -a"]}'
# Alternatively, use built-in tools.
curl -X POST localhost:7777/v1/envs/dev1/tools \
     -d '{"type":"function_call","id":"call_1","name":"read_file","arguments":{"path":"/note.txt"}}'
curl -X POST localhost:7777/v1/envs/dev1/tools \
     -d '{"type":"function_call","id":"call_2","name":"browser","arguments":{"url":"https://example.com"}}'
```

## CLI

Lifecycle and deployment are top-level commands; command execution and file operations on an environment can also use the environment ID as the first argument (`ate-env <id> ...`):

```bash
$ ate-env create dev1 --template env
$ ate-env dev1 cmd 'uname -a'
$ ate-env dev1 fs ls /
$ ate-env delete dev1
```

`ate-env` provides help text:

```bash
$ ate-env ate-env
Manage environments on Agent Substrate

Usage:
  ate-env [command]

Available Commands:
  cmd         Run a shell command line in the environment
  completion  Generate the autocompletion script for the specified shell
  create      Create and start an environment
  delete      Delete an environment
  deploy      Generate Kubernetes manifests to deploy the system
  fs          Operate on files and directories in an environment
  help        Help about any command
  resume      Resume from the latest snapshot
  suspend     Snapshot to external storage and free the worker

$ ate-env dev1
Operate on environment dev1

Usage:
  ate-env dev1 [command]

Available Commands:
  cmd         Run a shell command line in the environment
  fs          Operate on files and directories in the environment

$ ate-env deploy --help
Deploy generates Kubernetes manifests for everything environments need on
a cluster that already runs the Agent Substrate system: the target
namespace, a WorkerPool of pre-warmed workers, the ActorTemplate that
environments are created from, and the ate-env-api service. It prints YAML to
stdout without touching the cluster; apply it with kubectl.
```

## API

The API server provides environment management and guest operations over the API. Alternatively, a large number of users may find the [built-in tools](#built-in-tools) the primary way to run these operations.

### Environments

| Method   | Path                 | Description                            |
| -------- | -------------------- | -------------------------------------- |
| `POST`   | `/v1/envs`      | Create an environment                        |
| `DELETE` | `/v1/envs/{id}` | Delete (suspends first if running)      |

Create body:

```json
{
  "id": "dev1",
  "template": "env",
  "namespace": "ate-env"
}
```

### Lifecycle

| Method | Path                         | Description                              |
| ------ | ---------------------------- | ---------------------------------------- |
| `POST` | `/v1/envs/{id}/suspend` | Snapshot to object storage, free worker  |
| `POST` | `/v1/envs/{id}/resume`  | Restore from the latest snapshot         |

### Commands

`POST /v1/envs/{id}/cmd`

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

| Method   | Path                       | Description                        |
| -------- | -------------------------- | ---------------------------------- |
| `GET`    | `/v1/envs/{id}/file`  | Read a file (base64 JSON response) |
| `POST`   | `/v1/envs/{id}/file`  | Write a file                       |
| `DELETE` | `/v1/envs/{id}/file`  | Delete a file or directory         |
| `GET`    | `/v1/envs/{id}/dir`   | List a directory                   |
| `POST`   | `/v1/envs/{id}/dir`   | Create a directory (mkdir -p)      |
| `GET`    | `/v1/envs/{id}/stat`  | Stat a path                        |

Write a file, then read it back (`content` is base64-encoded in both requests and responses; `mode` is an octal string defaulting to `"644"`):

```bash
curl -X POST localhost:7777/v1/envs/dev1/file \
     -d '{"path": "app/main.txt", "mode": "644", "content": "aGVsbG8K"}'
curl -X GET localhost:7777/v1/envs/dev1/file \
     -d '{"path": "app/main.txt"}'
# Response: {"content":"aGVsbG8K","mode":"0644","size":6}
```

## Built-in Tools

The API exposes built-in tools for file system operations and shell executions.

| Method | Path                        | Description                      |
| ------ | --------------------------- | -------------------------------- |
| `GET`  | `/v1/envs/{id}/tools`  | List registered tool definitions |
| `POST` | `/v1/envs/{id}/tools`  | Execute a tool call              |

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
| `shell` | Shell | Run a shell command line inside the environment |
| `browser` | Web | Fetch a web page or API over HTTP(S) and render HTML to Markdown |

TODO: Add support for skills e.g. generaate available_skills, and activate a skill.

### Tool Definitions

```bash
curl -X GET localhost:7777/v1/envs/dev1/tools
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
curl -X POST localhost:7777/v1/envs/dev1/tools \
     -d '{"type":"function_call","id":"call_1","name":"read_file","arguments":{"path":"main.go"}}'
{
  "type": "function_result",
  "name": "read_file",
  "call_id": "call_1",
  "result": [
    { "type": "text", "text": "     1\tpackage main\n..." }
  ]
}

curl -X POST localhost:7777/v1/envs/dev1/tools \
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

Users can use the `env` package directly for lifecycle operations to manage
environments programmatically, and executing operations on the guest.

```go
client, err := env.NewClient(env.ClientOptions{
    Endpoint: "http://localhost:7777",          // ate-env-api endpoint
})
if err != nil {
    log.Fatalf("connecting to Substrate: %v", err)
}
defer client.Close()

e, err := client.Create(ctx, "dev1", env.WithTemplate("env"))
if err != nil {
    log.Fatalf("creating environment: %v", err)
}
if err := e.WriteFile(ctx, "/workspace/main.go", src, 0o644); err != nil {
    log.Fatalf("writing main.go: %v", err)
}
res, err := e.Cmd(ctx, "cd /workspace && go run main.go")
if err != nil {
    log.Fatalf("running main.go: %v", err)
}
fmt.Println(res.Stdout, res.ExitCode)

e.Suspend(ctx)
e.Resume(ctx)
e.Delete(ctx)
```

See [examples/quickstart](examples/quickstart/main.go) for a complete
program.

## Cleanup

You can remove the environment deployment by running:

```bash
# Cleanup the deployment to remove Agent Substrate Environment from your cluster:
kubectl delete ns ate-env
```