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
                                                 ╰────────────╯     │  /readyz, /v1alpha/* │
                                                                    ╰──────────────────────╯
```

- **`cmd/ate-env`** — CLI for managing environments, executing remote commands, and performing file I/O.
- **`cmd/ate-env-api`** — The API service that manages environments and proxies remote guest requests.
- **`cmd/ate-env-guest`** — The daemon server running inside each actor serving command executions, file read/write, and built-in MCP tools.
- **`clients/go`** — The Go client library to manage environments, run commands, and perform file operations.
- **`clients/python`** — The async Python client library ([README](clients/python/README.md)).

## Installation

```bash
go install github.com/agent-substrate/env/cmd/ate-env@latest
```

## Quickstart

Prerequisites: a cluster with [Agent Substrate](https://github.com/agent-substrate/substrate)
installed and a snapshots bucket.

### 1. Deploy the Kubernetes Resources

Deploy the namespace, worker pool, and API service:

```bash
export GOOGLE_CLOUD_PROJECT=$(gcloud config get-value project)
ate-env manifest \
  --guest-image    gcr.io/$GOOGLE_CLOUD_PROJECT/ate-env-guest@sha256:4f5678b9304a9047551fc95458e7b948c77d6fce5337de1897888daa7e0e4900 \
  --api-image      gcr.io/$GOOGLE_CLOUD_PROJECT/ate-env-api@sha256:8579e1eebdd652cd2bfc7a130d4ebc2ce0d7a1a9d2efcb9d04b9eedf597a027d \
  --worker-image   gcr.io/$GOOGLE_CLOUD_PROJECT/ateom-gvisor-715889664656de67e44382a8d6ab981d@sha256:7a5f89e9c8ca875eee611b05fdf003b63b260b631362c83c1099073d003e0372 \
  --snapshots-bucket gs://$GOOGLE_CLOUD_PROJECT/ate-env/ | kubectl apply -f -

# Ensure that the pods are running:
kubectl get pods -n ate-env
```

### 2. Register the ActorTemplate in Substrate

Substrate manages ActorTemplates directly in its control plane rather than Kubernetes CRDs. Use `ate-env manifest --template-only` to generate the Substrate ActorTemplate manifest:

```bash
ate-env manifest --template-only \
  --guest-image    gcr.io/$GOOGLE_CLOUD_PROJECT/ate-env-guest@sha256:4f5678b9304a9047551fc95458e7b948c77d6fce5337de1897888daa7e0e4900 \
  --snapshots-bucket gs://$GOOGLE_CLOUD_PROJECT/ate-env/ | kubectl-ate create actor-template -f -
```

Then create and use an environment:

```bash
# Port-forward the ate-env-api service.
kubectl port-forward -n ate-env svc/ate-env-api 7777:7777 &

# Create an environment.
ate-env create dev1

# Execute a shell command inside the environment.
ate-env dev1 shell 'echo hello > /note.txt'

# Read and write files.
ate-env dev1 read /note.txt
echo "world" | ate-env dev1 write /note.txt

# Suspend (snapshot) and delete.
ate-env suspend dev1
ate-env delete dev1
```

Alternatively, interact with the environment over MCP:

```bash
curl -X POST localhost:7777/v1alpha/envs/dev1/mcp \
     -H "Content-Type: application/json" \
     -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"shell","arguments":{"command":"echo hi"}}}'
```

## CLI

File operations and remote command execution take the environment ID first, followed by the operation:

```bash
$ ate-env create dev1
$ ate-env dev1 shell 'uname -a'
$ echo "hello world" | ate-env dev1 write /app/msg.txt
$ ate-env dev1 read /app/msg.txt
$ ate-env suspend dev1
$ ate-env delete dev1
```

`ate-env` provides help text:

```bash
$ ate-env --help
Manage environments on Agent Substrate.

Common environment commands:
  ate-env <id> read <path>       Print an environment file to stdout
  ate-env <id> write <path>      Write stdin to an environment file
  ate-env <id> shell <cmdline>   Run a shell command line in the environment

Usage:
  ate-env [command]

Available Commands:
  completion  Generate the autocompletion script for the specified shell
  create      Create and start an environment
  delete      Delete an environment
  help        Help about any command
  manifest    Generate Kubernetes manifests to deploy the system
  suspend     Suspend an environment

Flags:
  -h, --help   help for ate-env

Use "ate-env [command] --help" for more information about a command.

$ ate-env manifest --help
Manifest generates Kubernetes manifests for everything environments need on
a cluster that already runs the Agent Substrate system: the target
namespace, a WorkerPool of pre-warmed workers, the ActorTemplate that
environments are created from, and the ate-env-api service. It prints YAML to
stdout without touching the cluster; apply it with kubectl.
```

## API

Environment lifecycle is defined in [`proto/ateenv/v1alpha/env.proto`](proto/ateenv/v1alpha/env.proto):

| Operation | Description |
| --- | ----------- |
| `CreateEnvironment` | Creates and starts a new environment actor |
| `GetEnvironment` | Retrieves environment details and status |
| `SuspendEnvironment` | Suspends and checkpoints the environment |
| `DeleteEnvironment` | Deletes the environment permanently |

## Built-in MCP Server

The API exposes a streamable MCP endpoint at `POST /v1alpha/envs/{id}/mcp` serving built-in tools.

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

Clients communicate with the MCP server at `/v1alpha/envs/{id}/mcp` using JSON-RPC 2.0:

#### Initialize

```bash
curl -X POST localhost:7777/v1alpha/envs/dev1/mcp \
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
curl -X POST localhost:7777/v1alpha/envs/dev1/mcp \
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
curl -X POST localhost:7777/v1alpha/envs/dev1/mcp \
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