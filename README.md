# 📦 Agent Substrate Environment

> [!WARNING]
> This is an alpha API and is likely to change until v1.0 is released.

An environment service on top of [Agent Substrate](https://github.com/agent-substrate/substrate): isolated, stateful execution environments
driven remotely with **command execution** and **filesystem operations**.

Each environment is a Substrate *actor* running in an isolated container.
Substrate provides the heavy lifting — snapshotting, scheduling,
multiplexing many idle environments onto a small worker pool, and routing —
while this project adds the environment-shaped API on top.

## Overview

```
 ╭──────────────╮    ╭──────────────╮ lifecycle  ╭────────────╮
 │    Clients   │    │              ├───────────▶│   ateapi   │ Substrate control plane
 │  ate-env CLI ├───▶│ ate-env-api  │            ╰────────────╯
 ╰──────────────╯    │ (API server) │shell/fs/mcp╭────────────╮     ╭──────────────────────╮
                     │              ├───────────▶│   atenet   ├────▶│ actor                │
                     ╰──────────────╯            │   router   │     │  └ ate-env-guest     │
                                                 ╰────────────╯     │    /v1/envs/*        │
                                                                    ╰──────────────────────╯
```

- **`cmd/ate-env`** — Provides a CLI over the API, and utilities to
  make it easier to deploy Agent Substrate.
- **`cmd/ate-env-api`** — The API service that bridges clients to
  the Substrate control plane and router.
- **`cmd/ate-env-guest`** — The daemon server available in the environment. It runs
  inside every actor and serves command executions and filesystem operations.
- **`env`** — The Go client library that allows creation and deletion of environments,
  as well as file operations and running remote commands on the environments.

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
  --guest-image    gcr.io/dberkov-gke-dev3/ate-env-guest@sha256:f6652a5f54d0217ba621c24dd5304c1067275d0986b3b0c7fe728a0d40625e8a \
  --api-image      gcr.io/dberkov-gke-dev3/ate-env-api@sha256:3cfb7ffc549930078c5b9ae52d112ea8f5b493519287d5f8e410b123739b84fc \
  --ateom-image gcr.io/dberkov-gke-dev3/ate-images/ateom-gvisor-715889664656de67e44382a8d6ab981d@sha256:b0b6e2ad834de42cb2a4c55e83b60243f66cb85ca37575d1a6818e788e0564e0 \
  --snapshots-bucket gs://$GCS_BUCKET/ate-env/ | kubectl apply -f -

# Ensure that the pods are running:
kubectl get pods -n ate-env
```

Then create and use an environment:

```bash
# Port-forward the ate-env-api API.
kubectl port-forward -n ate-env svc/ate-env-api 7777:7777 &

# Create and use an environment.
ate-env create dev1 --template default-env
ate-env dev1 shell 'echo hello > /note.txt'
ate-env dev1 shell 'cat /note.txt' # prints hello
ate-env delete dev1
```

Or use the API directly:

```bash
curl -X POST localhost:7777/v1/envs -d '{"id":"dev1","template":"default-env"}'
curl -X POST localhost:7777/v1/envs/dev1/shell \
     -d '{"command":"uname -a"}'
# Alternatively, interact over MCP.
curl -X POST localhost:7777/v1/envs/dev1/mcp \
     -H "Content-Type: application/json" \
     -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"shell","arguments":{"command":"echo hi"}}}'
```

## CLI

Lifecycle and deployment are top-level commands; command execution and file operations on an environment use the environment ID as the first argument (`ate-env <id> ...`):

```bash
$ ate-env create dev1 --template default-env
$ ate-env dev1 shell 'uname -a'
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
  completion  Generate the autocompletion script for the specified shell
  create      Create and start an environment
  delete      Delete an environment
  deploy      Generate Kubernetes manifests to deploy the system
  fs          Operate on files and directories in an environment
  help        Help about any command

$ ate-env dev1
Operate on environment dev1

Usage:
  ate-env dev1 [command]

Available Commands:
  fs          Operate on files and directories in the environment
  shell       Run a shell command line in the environment

$ ate-env deploy --help
Deploy generates Kubernetes manifests for everything environments need on
a cluster that already runs the Agent Substrate system: the target
namespace, a WorkerPool of pre-warmed workers, the ActorTemplate that
environments are created from, and the ate-env-api service. It prints YAML to
stdout without touching the cluster; apply it with kubectl.
```

## API

The API server provides environment management and guest operations over the API. Alternatively, a large number of users may find the built-in MCP tools the primary way to run these operations.

### Environments

| Method   | Path                 | Description                            |
| -------- | -------------------- | -------------------------------------- |
| `POST`   | `/v1/envs`      | Create an environment                        |
| `DELETE` | `/v1/envs/{id}` | Delete an environment                   |

Create body:

```json
{
  "id": "dev1",
  "template": "default-env",
  "namespace": "ate-env"
}
```

### Shell

`POST /v1/envs/{id}/shell`

```json
{                                           {
  "command": ["sh", "-c", "make test"],       "stdout": "ok\n",
  "cwd": "/workspace/app",                    "stderr": "",
  "env": {"VERBOSE_LOGS": "true"}             "exitCode": 0
}                                           }
```

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

## Built-in MCP Tools

The API exposes an MCP endpoint at `POST /v1/envs/{id}/mcp` serving the built-in tools.

| Method | Path                        | Description                      |
| ------ | --------------------------- | -------------------------------- |
| `POST` | `/v1/envs/{id}/mcp`    | Model Context Protocol (MCP) streamable endpoint |

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

TODO: Add support for skills e.g. generate available_skills, and activate a skill.

### MCP Server

Clients communicate with the built-in MCP server at `/v1/envs/{id}/mcp` using JSON-RPC 2.0 over HTTP:

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
- **Environment SDK**: See [quickstart](examples/quickstart/main.go) to manage environments, write files, and execute commands.
- **MCP**: See [mcp](examples/mcp/main.go) to connect to an environment's MCP endpoint, discover tools, and execute tool calls.

## Cleanup

You can remove the environment deployment by running:

```bash
# Cleanup the deployment to remove Agent Substrate Environment from your cluster:
kubectl delete ns ate-env
```