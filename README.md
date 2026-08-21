# 📦 Agent Substrate Environment

> [!WARNING]
> This is an alpha API and is likely to change until v1.0 is released.

An environment service on top of [Agent Substrate](https://github.com/agent-substrate/substrate): isolated, stateful execution environments
driven remotely with **command execution**, **filesystem operations**, and **built-in MCP tools**.

Each environment is a Substrate *actor* running in an isolated container.
Substrate provides the heavy lifting — snapshotting, scheduling,
multiplexing many idle environments onto a small worker pool, and routing —
while this project adds the environment-shaped API on top.

## Overview

```
 ╭──────────────╮    ╭──────────────╮ lifecycle  ╭────────────╮
 │    Clients   │    │              ├───────────▶│   ateapi   │ Substrate control plane
 │  ate-env CLI ├───▶│ ate-env-api  │            ╰────────────╯
 ╰──────────────╯    │ (API server) │ guest ops  ╭────────────╮     ╭──────────────────────╮
                     │              ├───────────▶│   atenet   ├────▶│ actor                │
                     ╰──────────────╯ (shell/mcp)│   router   │     │  └ ate-env-guest     │
                                                 ╰────────────╯     │    /readyz, /v1/*    │
                                                                    ╰──────────────────────╯
```

- **`cmd/ate-env`** — CLI for managing environments, executing remote commands, and performing file I/O.
- **`cmd/ate-env-api`** — The API service that manages environments and proxies remote guest requests.
- **`cmd/ate-env-guest`** — The daemon server running inside each actor serving command executions, file read/write, and built-in MCP tools.
- **`env`** — The Go client library to manage environments, run commands, and perform file operations.

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
  --guest-image    gcr.io/dberkov-gke-dev3/ate-env-guest@sha256:f908e2909c66a66e06f40c18cb813c0facc69a986cb1fcf4af2eb58ceba74cbd \
  --api-image      gcr.io/dberkov-gke-dev3/ate-env-api@sha256:00a1e6f0a802c29669469f7c9296f0a5bf8a17ce07035d18936b27c96b380a1a \
  --ateom-image    gcr.io/dberkov-gke-dev3/ate-images/ateom-gvisor-715889664656de67e44382a8d6ab981d@sha256:b0b6e2ad834de42cb2a4c55e83b60243f66cb85ca37575d1a6818e788e0564e0 \
  --snapshots-bucket gs://$GCS_BUCKET/ate-env/ | kubectl apply -f -

# Ensure that the pods are running:
kubectl get pods -n ate-env
```

Then create and use an environment:

```bash
# Port-forward the ate-env-api service.
kubectl port-forward -n ate-env svc/ate-env-api 7777:7777 &

# Create an environment.
ate-env create dev1

# Execute a shell command inside the environment.
ate-env shell dev1 'echo hello > /note.txt'

# Read and write files.
ate-env read dev1 /note.txt
echo "world" | ate-env write dev1 /note.txt

# Suspend (snapshot) and delete.
ate-env suspend dev1
ate-env delete dev1
```

Alternatively, interact with the environment over MCP:

```bash
curl -X POST localhost:7777/v1/envs/dev1/mcp \
     -H "Content-Type: application/json" \
     -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"shell","arguments":{"command":"echo hi"}}}'
```

## CLI

All lifecycle, execution, and file operations are top-level commands taking the environment ID as an argument:

```bash
$ ate-env create dev1
$ ate-env shell dev1 'uname -a'
$ echo "hello world" | ate-env write dev1 /app/msg.txt
$ ate-env read dev1 /app/msg.txt
$ ate-env suspend dev1
$ ate-env delete dev1
```

`ate-env` provides help text:

```bash
$ ate-env --help
Manage environments on Agent Substrate

Usage:
  ate-env [command]

Available Commands:
  completion  Generate the autocompletion script for the specified shell
  create      Create and start an environment
  delete      Delete an environment
  deploy      Generate Kubernetes manifests to deploy the system
  help        Help about any command
  read        Print an environment file to stdout
  shell       Run a shell command line in the environment
  suspend     Suspend an environment
  write       Write stdin to an environment file

Flags:
      --api string        address of the ate-env-api service (e.g. localhost:7777) (default "127.0.0.1:7777")
      --atespace string   Substrate atespace (default "default")
  -h, --help              help for ate-env

Use "ate-env [command] --help" for more information about a command.

$ ate-env deploy --help
Deploy generates Kubernetes manifests for everything environments need on
a cluster that already runs the Agent Substrate system: the target
namespace, a WorkerPool of pre-warmed workers, the ActorTemplate that
environments are created from, and the ate-env-api service. It prints YAML to
stdout without touching the cluster; apply it with kubectl.
```

## API

Environment lifecycle is defined in [`proto/ateenv/v1/env.proto`](proto/ateenv/v1/env.proto):

| Operation | Request | Response | Description |
| --- | ------- | -------- | ----------- |
| `CreateEnvironment` | `CreateEnvironmentRequest` | `CreateEnvironmentResponse` | Creates and starts a new environment actor |
| `GetEnvironment` | `GetEnvironmentRequest` | `GetEnvironmentResponse` | Retrieves environment details and status |
| `SuspendEnvironment` | `SuspendEnvironmentRequest` | `SuspendEnvironmentResponse` | Suspends and checkpoints the environment |
| `DeleteEnvironment` | `DeleteEnvironmentRequest` | `DeleteEnvironmentResponse` | Deletes the environment permanently |

## Built-in MCP Server

The API exposes a streamable MCP endpoint at `POST /v1/envs/{id}/mcp` serving built-in tools.

### Available Tools

| Tool | Category | Description |
| ---- | -------- | ----------- |
| `read_file` | Filesystem | Read a text file with optional line numbers or line ranges |
| `write_file` | Filesystem | Create or overwrite a file |
| `edit_file` | Filesystem | Replace exact text matching target content in a file |
| `glob` | Filesystem | Search for files matching glob patterns |
| `grep` | Filesystem | Search for text or regular expressions across files |
| `shell` | Shell | Run a shell command line inside the environment |

### JSON-RPC Over HTTP

Clients communicate with the MCP server at `/v1/envs/{id}/mcp` using JSON-RPC 2.0:

#### Initialize

```bash
curl -X POST localhost:7777/v1/envs/dev1/mcp \
     -H "Content-Type: application/json" \
     -H "Accept: application/json, text/event-stream" \
     -d '{
       "jsonrpc": "2.0",
       "id": 1,
       "method": "initialize",
       "params": {
         "protocolVersion": "2025-11-25",
         "capabilities": {},
         "clientInfo": {"name": "curl", "version": "1.0.0"}
       }
     }'
```

#### List Tools (`tools/list`)

```bash
curl -X POST localhost:7777/v1/envs/dev1/mcp \
     -H "Content-Type: application/json" \
     -H "Accept: application/json, text/event-stream" \
     -d '{
       "jsonrpc": "2.0",
       "id": 2,
       "method": "tools/list"
     }'
```

#### Call Tool (`tools/call`)

```bash
curl -X POST localhost:7777/v1/envs/dev1/mcp \
     -H "Content-Type: application/json" \
     -H "Accept: application/json, text/event-stream" \
     -d '{
       "jsonrpc": "2.0",
       "id": 3,
       "method": "tools/call",
       "params": {
         "name": "shell",
         "arguments": {"command": "echo hello from mcp"}
       }
     }'
```

## Examples

For complete runnable Go programs:
- **MCP**: See [mcp](examples/mcp/main.go) to connect to an environment's MCP endpoint, discover tools, and execute tool calls.
- **Guest Daemon**: See [guest-daemon](examples/guest-daemon/main.go) to run a standalone in-actor gRPC service for asynchronous process execution and chunked file transfer.

## Cleanup

```bash
# Delete the ate-env namespace to remove all components:
kubectl delete ns ate-env
```