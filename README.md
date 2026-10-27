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
- **`clients/python`** — Python client for environment lifecycle management and batch sandbox orchestration ([README](clients/python/README.md)).

## Installation

Download the `ate-env` binary for your platform from the
[latest release](https://github.com/agent-substrate/env/releases/latest). Release
binaries know the digests of the `ate-env-api` and `ate-env-guest` images
published with that release, so the commands below need no image flags for
this repo's images. Building from source works too, but then `--api-image` and
`--guest-image` must be given:

```bash
go install github.com/agent-substrate/env/cmd/ate-env@latest
```

## Quickstart

Prerequisites: a cluster running [Agent Substrate](https://github.com/agent-substrate/substrate)
at or after [`63a17a92`](https://github.com/agent-substrate/substrate/commit/63a17a92), and a GCS
bucket for snapshots. ate-env is built and tested against
[`3a2ca623`](https://github.com/agent-substrate/substrate/commit/3a2ca623).

### 1. Build and Push the Images

Build and push the `ate-env-api`, `ate-env-guest` and Substrate `ateworker-gvisor` images with
[ko](https://ko.build):

```bash
export ATE_ENV_IMAGE_REPO=registry.example.com/substrate-env
make images
```

It prints the digest-pinned refs to pass as `--api-image`, `--guest-image` and `--worker-image`. The
worker image is built from the Substrate version in `go.mod`; if your cluster runs a different one,
build it from that Substrate checkout with `ate-setup publish worker-images` instead.

### 2. Deploy the Kubernetes Resources

Deploy the namespace, worker pool, and API service:

```bash
ate-env manifest \
  --worker-image   $ATE_ENV_IMAGE_REPO/ateworker-gvisor@sha256:<digest> | kubectl apply -f -

# Ensure that the pods are running:
kubectl get pods -n ate-env
```

### 3. Register the ActorTemplate in Substrate

Substrate manages ActorTemplates directly in its control plane rather than Kubernetes CRDs. Use `ate-env manifest template` to generate the Substrate ActorTemplate manifest:

Templates live in the `ate-env` atespace, which `ate-env-api` creates when it starts. Register templates after the `ate-env-api` pod is Running, or create the atespace yourself first with `kubectl-ate create atespace ate-env`.

```bash
# With a release binary the guest image defaults to the release's.
ate-env manifest template \
  --snapshots-bucket <gcs-bucket> | kubectl-ate create actor-template -f -
```

Each actor gets 2 CPUs and 4 GiB unless you pass `--cpu` and `--memory` (Kubernetes quantities such as
`500m` or `8Gi`; CPU at most `999.999`, as Substrate requires). Substrate requires both limits and books them against worker capacity.

Then create and use an environment:

```bash
# Port-forward the ate-env-api service.
kubectl port-forward -n ate-env svc/ate-env-api 7777:7777 &

# Create an environment.
ate-env create dev1

# Execute a shell command inside the environment.
ate-env dev1 shell 'echo hello > /note.txt'

# Feed stdin to a command and bound its run time.
echo 'shout' | ate-env dev1 shell --stdin --timeout 30s 'tr a-z A-Z'

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
namespace, a WorkerPool of pre-warmed workers, and the ate-env-api service.
It prints YAML to stdout without touching the cluster; apply it with kubectl.
```

## API

The `ate-env-api` service exposes gRPC APIs for environment
lifecycle and in-actor operations, as well as HTTP endpoints for MCP.

### EnvironmentService

Manages the lifecycle of isolated execution environments (defined in [`proto/ateenv/v1alpha/env.proto`](proto/ateenv/v1alpha/env.proto)). Requests are handled by `ate-env-api` and translated into Agent Substrate control plane operations:

| RPC | Description |
| --- | ----------- |
| `CreateEnvironment` | Creates and starts a new environment actor from an ActorTemplate |
| `GetEnvironment` | Retrieves environment details and status |
| `SuspendEnvironment` | Suspends and checkpoints the environment to snapshot storage |
| `DeleteEnvironment` | Deletes the environment permanently |

### ProcessService

Manages process execution, I/O streaming, and signals inside the environment container (defined in [`proto/ateenv/v1alpha/guest.proto`](proto/ateenv/v1alpha/guest.proto)). Requests are proxied by `ate-env-api` directly to the `ate-env-guest` daemon:

| RPC | Description |
| --- | ----------- |
| `StartProcess` | Launches a process (optionally with a stdin pipe and a timeout) and returns the `Process` resource |
| `GetProcess` | Retrieves the process state, exit code, and timestamps |
| `StreamProcessOutput` | Streams stdout and stderr chunks; with `follow`, ends with an `exit` message carrying the final `Process` |
| `WriteProcessInput` | Streams bytes to the process's stdin; a message with `close` sends EOF |
| `SignalProcess` | Delivers a POSIX signal (`TERM`, `INT`, `KILL`, `USR1`, ...) to the process group |

`exit_code` follows the shell convention: the process's exit code, or 128 + signal number if it was killed by a signal. To wait for a process without receiving its output, follow the output stream with offsets past the end of the spool.

### FileSystemService

Provides chunked streaming file reading and writing within the environment container without unbounded memory usage (defined in [`proto/ateenv/v1alpha/guest.proto`](proto/ateenv/v1alpha/guest.proto)). Requests are proxied by `ate-env-api` directly to the `ate-env-guest` daemon:

| RPC | Description |
| --- | ----------- |
| `ReadFile` | Streams raw binary or text file contents in chunks |
| `WriteFile` | Streams raw binary or text chunks directly to a target file |


## Built-in MCP Server

The API exposes a streamable MCP endpoint at `POST /v1alpha/envs/{id}/mcp` serving built-in tools.

### Available Tools

| Tool | Category | Description |
| ---- | -------- | ----------- |
| `read_file` | Filesystem | Read a file |
| `write_file` | Filesystem | Create or overwrite a file, with an optional POSIX mode |
| `shell` | Shell | Run a shell command line inside the environment, with optional cwd, env, stdin and timeout |

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
kubectl ate delete actor-template --atespace ate-env default-template
```