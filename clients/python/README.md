# ate-env-client — Async Python Client

> [!WARNING]
> This is an alpha API and is likely to change until v1.0 is released.

Async Python client for the [Agent Substrate Environment](../../README.md)
API (`ate-env-api`): environment lifecycle, remote command execution, and
streaming file I/O over gRPC.

Requires Python >= 3.10. Everything is `asyncio`-native: methods are
coroutines, log/file streams are async iterators, and cancellation works
through standard task cancellation and `asyncio.timeout()`.

## How it works

The client talks to a single endpoint — the `ate-env-api` service — over
plain gRPC (h2c). Behind that endpoint there are two distinct paths:

```
       ate_env (this package)                                       cluster
 ╭────────────────────────────╮
 │ Client                     │ EnvironmentService ╭───────────────╮ lifecycle  ╭────────╮
 │  create() get() suspend()  │───────────────────▶│               │───────────▶│ ateapi │
 │  delete() env()            │      (unary)       │               │            ╰────────╯
 ╰────────────────────────────╯                    │  ate-env-api  │ Substrate control plane
 ╭────────────────────────────╮                    │ (guest proxy) │
 │ Env / Process              │ ProcessService     │               │ x-env-id   ╭────────╮   ╭──────────────────╮
 │  shell() start_process()   │ FileSystemService  │               │───────────▶│ atenet │──▶│ actor            │
 │  output() write_input()    │───────────────────▶│               │  routing   │ router │   │ └ ate-env-guest  │
 │  signal() wait()           │ + routing metadata ╰───────────────╯            ╰────────╯   ╰──────────────────╯
 │  read_file() write_file()  │
 ╰────────────────────────────╯
```

**Lifecycle path.** `Client.create/get/suspend/delete` call
`EnvironmentService` (defined in [`proto/ateenv/v1alpha/env.proto`](../../proto/ateenv/v1alpha/env.proto)).
These RPCs terminate at `ate-env-api`, which translates them into Substrate
control-plane operations: creating an actor from an ActorTemplate,
reading its status, checkpointing it to a snapshot, deleting it.

**Guest path.** Everything on an `Env` handle that executes *inside* the
environment — processes and files — calls `ProcessService` and
`FileSystemService` (defined in [`proto/ateenv/v1alpha/guest.proto`](../../proto/ateenv/v1alpha/guest.proto)).
The client attaches `x-env-id` / `x-env-atespace` gRPC metadata to each of
these calls; `ate-env-api` uses that metadata to dial the atenet router
with the authority `<id>.<atespace>.<host-suffix>`, and the router carries
the request into the right actor, where the `ate-env-guest` daemon serves
it. The proxy is transparent: streaming responses (logs, file chunks)
flow end-to-end without buffering.

If the environment is suspended, routing traffic to it wakes it up —
Substrate resumes the actor from its latest snapshot on demand. There is
no explicit resume API; the first guest call after a suspend (or after
create) does the waking, and may take noticeably longer or fail while the
actor boots. Retry until it serves (see the how-to below).

> [!IMPORTANT]
> The guest path requires a Substrate deployment that carries gRPC
> (HTTP/2 + trailers) through the atenet router to actors — i.e.
> [substrate PR #1183](https://github.com/agent-substrate/substrate/pull/1183)
> ("atenet: h2 on the HTTPS ingress and mirror the protocol to actors").
> Without it, every process/file operation fails with
> `server closed the stream without sending trailers`, because the
> router's actor upstream is pinned to HTTP/1.1, which drops gRPC
> trailers. Environment lifecycle operations are unaffected.

### What calls where

| Client call | RPC | Handled by |
| --- | --- | --- |
| `Client.create()` | `EnvironmentService.CreateEnvironment` | ate-env-api → control plane |
| `Client.get()` / `Env.info()` | `EnvironmentService.GetEnvironment` | ate-env-api → control plane |
| `Client.suspend()` / `Env.suspend()` | `EnvironmentService.SuspendEnvironment` | ate-env-api → control plane |
| `Client.delete()` / `Env.delete()` | `EnvironmentService.DeleteEnvironment` | ate-env-api → control plane |
| `Client.env()` | *(no RPC — returns a handle)* | — |
| `Env.start_process()` | `ProcessService.StartProcess` | proxied to guest |
| `Env.process()` | *(no RPC — returns a handle)* | — |
| `Process.info()` | `ProcessService.GetProcess` | proxied to guest |
| `Process.output()` / `Process.wait()` | `ProcessService.StreamProcessOutput` *(server-streaming)* | proxied to guest |
| `Process.write_input()` / `close_input()` | `ProcessService.WriteProcessInput` *(client-streaming)* | proxied to guest |
| `Process.signal()` / `Process.kill()` | `ProcessService.SignalProcess` | proxied to guest |
| `Env.shell()` | `StartProcess` (+ `WriteProcessInput`) + `StreamProcessOutput` | proxied to guest |
| `Env.read_file()` / `read_file_bytes()` | `FileSystemService.ReadFile` *(server-streaming)* | proxied to guest |
| `Env.write_file()` | `FileSystemService.WriteFile` *(client-streaming)* | proxied to guest |

`Env.shell()` is a convenience composed from the process primitives: it
starts `sh -c <command>`, feeds it stdin if given, and follows the output
stream, which ends with the process's exit state.

## How-to

### Install

From a repo checkout:

```bash
pip install ./clients/python
```

The distribution is named `ate-env-client`; the import package is
`ate_env` (mirroring the Go client, which lives at `clients/go`).

### Connect

You need a reachable `ate-env-api`. On a cluster deployed with
`ate-env manifest`, port-forward it:

```bash
kubectl -n ate-env port-forward svc/ate-env-api 7777:7777
```

Then create a client. The connection is plain gRPC (no TLS), matching
the Go client and CLI. A single `Client` multiplexes any number of
concurrent operations over one HTTP/2 channel and is safe to share
across tasks — for long-lived programs (servers, multi-agent
orchestration), create one client, share it, and close it on shutdown:

```python
from ate_env import Client

client = Client("localhost:7777")
try:
    env = await client.create("dev1")
    ...
finally:
    await client.close()
```

`Client(...)` accepts `host:port` or an `http://` URL. To manage the
channel yourself (e.g. custom gRPC options), pass
`Client(channel=your_grpc_aio_channel)` — the client then never closes it.

### Create an environment

```python
env = await client.create("dev1")
```

The server instantiates the environment from the `default-template`
ActorTemplate in the `ate-env` atespace unless you override it:

```python
env = await client.create("dev1", template_name="my-template",
                          template_atespace="my-atespace")
```

To get a handle to an environment that already exists (no RPC is made):

```python
env = client.env("dev1")            # atespace defaults to "ate-env"
```

A freshly created (or suspended) environment starts serving on first
contact. Gate on readiness by retrying a trivial command:

```python
from ate_env import EnvError

async def wait_until_serving(env, timeout=180):
    async with asyncio.timeout(timeout):
        while True:
            try:
                if (await env.shell("true")).exit_code == 0:
                    return
            except EnvError:
                pass
            await asyncio.sleep(2)
```

### Run shell commands

```python
result = await env.shell("echo hello && uname -a")
print(result.exit_code)   # int; 128 + signal number if killed by a signal
print(result.stdout)      # str (utf-8, invalid bytes replaced)
print(result.stderr)

result = await env.shell("tr a-z A-Z", stdin="shout\n", timeout=30)
```

`shell()` buffers all output in memory and returns after the command
exits. `stdin` (bytes or str) is fed to the command and then closed;
`timeout` (seconds or `timedelta`) kills the command with SIGKILL when
it elapses. For long-running, chatty, or interactive commands, use the
process API instead.

### Run background processes and stream output

```python
proc = await env.start_process(
    ["python", "train.py"],
    cwd="/workspace",
    env={"EPOCHS": "10"},
    timeout=3600,
)

# Follow output live; the last message carries the exit state:
async for out in proc.output(follow=True):
    if out.stdout is not None:
        print(out.stdout.decode(), end="")
    elif out.stderr is not None:
        print(out.stderr.decode(), end="", file=sys.stderr)
    else:
        print("exited:", out.exit.exit_code)

info = await proc.wait()             # or: block until exit without reading output
info = await proc.info()             # snapshot: state, pid, exit_code, timestamps
```

Notes:

- `output(follow=True)` blocks until the process exits — consume it
  under `asyncio.timeout()` or in a task you can cancel. Breaking out of
  the `async for` cancels the underlying RPC cleanly.
- Replay from a byte offset with `stdout_offset=` / `stderr_offset=`;
  without `follow` you get the output written so far and the stream ends
  (with an `exit` message only if the process has already exited).
- `env.process(process_id)` returns a handle to a process started earlier.

### Feed stdin and send signals

```python
proc = await env.start_process(["python", "-i"], stdin=True)
await proc.write_input(b"print(6 * 7)\n")      # stdin stays open across calls
await proc.write_input(b"exit()\n", close=True) # close=True sends EOF

server = await env.start_process(["./serve"])
await server.signal(Signal.HUP)                # any POSIX signal, to the process group
await server.signal(Signal.TERM)
info = await server.wait()
assert info.exit_code == 143                   # 128 + SIGTERM

info = await server.kill()                     # SIGKILL + wait; idempotent
```

Signals and stdin need a running process; once it has exited they raise
`ProcessExitedError`. `write_input()` on a process started without
`stdin=True`, or after `close=True`, raises `FailedPreconditionError`.

### Read and write files

Paths are absolute or relative to the environment's workspace. Reads and
writes stream in 64 KiB chunks, so file size is not bounded by memory:

```python
# Small files, in one call:
await env.write_file("/workspace/config.json", b'{"debug": true}\n')
data = await env.read_file_bytes("/workspace/config.json")

# Large files, streamed:
async for chunk in env.read_file("/workspace/results.bin"):
    process(chunk)

async def produce():
    for shard in shards:
        yield shard.to_bytes()

await env.write_file("/workspace/dataset.bin", produce(), mode=0o600)
```

`write_file` accepts `bytes`/`bytearray`/`memoryview` (chunked for you)
or any sync/async iterable of bytes chunks (sent as-is). Writing `b""`
creates an empty file. By default the file is replaced; pass a positive
`seek_offset=` to keep existing content and write starting at that byte
(the file is zero-extended if it is shorter):

```python
await env.write_file("/workspace/data.bin", patch, seek_offset=4096)
```

### Suspend and delete

```python
await env.suspend()   # checkpoint to a snapshot and free the worker
await env.delete()    # remove permanently (suspends first if running)
```

A suspended environment keeps its filesystem and can be woken again just
by sending it guest traffic. Deletion is permanent.

### Handle errors

All failures raise subclasses of `ate_env.EnvError`:

| Exception | gRPC code | Typical cause |
| --- | --- | --- |
| `NotFoundError` | `NOT_FOUND` | unknown environment, process id, or file path |
| `InvalidArgumentError` | `INVALID_ARGUMENT` | empty id, missing routing metadata, empty command |
| `PermissionDeniedError` | `PERMISSION_DENIED` | file path escapes the workspace sandbox |
| `FailedPreconditionError` | `FAILED_PRECONDITION` | stdin not opened or already closed |
| `ProcessExitedError` | `FAILED_PRECONDITION` | signal or stdin write to a process that has exited (subclass of `FailedPreconditionError`) |
| `RpcError` | anything else | transport failures, `ALREADY_EXISTS`, actor still waking, … (`.code` holds the status) |

```python
from ate_env import NotFoundError

try:
    data = await env.read_file_bytes("/no/such/file")
except NotFoundError:
    data = None
```

### Concurrency

One `Client` multiplexes any number of concurrent operations over a
single HTTP/2 connection — handles and methods are safe to use from
multiple tasks:

```python
results = await asyncio.gather(
    env.shell("make test"),
    env.shell("make lint"),
    other_env.shell("make build"),
)
```

### Develop against a local guest daemon (no cluster)

The guest daemon example serves the process and filesystem services
standalone, so the whole guest path minus the proxy can run locally:

```bash
go run ./examples/guest-daemon --listen 127.0.0.1:8090 --workspace "$(mktemp -d)"
```

```python
client = Client("127.0.0.1:8090")
env = client.env("anything")            # daemon ignores the routing metadata
print(await env.shell("uname -a"))
await client.close()
```

Lifecycle calls (`create`, `get`, …) are unavailable in this mode — only
`ate-env-api` implements them.

See [examples/quickstart.py](examples/quickstart.py) for a complete
program against a real ate-env-api.

## Development

```bash
cd clients/python
python3 -m venv .venv
.venv/bin/pip install -e '.[dev]'
.venv/bin/pytest                    # unit tests (in-process fake server)
```

### Regenerating gRPC stubs

Generated code under `src/ate_env/_gen/` is committed. After changing
`proto/ateenv/v1alpha/*.proto`, regenerate from the repo root:

```bash
./clients/python/scripts/gen-protos.sh      # or: make python-protos
```

Keep the `grpcio-tools` pin, the regenerated stubs, and the `protobuf`
dependency floor in `pyproject.toml` in sync: stubs generated by a newer
protobuf require a matching or newer runtime.

### Integration tests without a cluster

```bash
go run ./examples/guest-daemon --listen 127.0.0.1:8090 --workspace "$(mktemp -d)"
ATE_ENV_GUEST_TARGET=127.0.0.1:8090 .venv/bin/pytest tests/e2e
```

### Full-stack tests against a cluster

With a cluster running Agent Substrate (including
[substrate PR #1183](https://github.com/agent-substrate/substrate/pull/1183),
required for the gRPC guest data plane — see the note under
"How it works") and a deployed ate-env-api built from current main:

```bash
kubectl -n ate-env port-forward svc/ate-env-api 17777:7777 &
ATE_ENV_API_TARGET=127.0.0.1:17777 .venv/bin/pytest tests/e2e/test_full_stack.py
```

`ATE_ENV_TEMPLATE` optionally overrides the ActorTemplate used for the
test environment; `ATE_ENV_READY_TIMEOUT` (default 180s) bounds the wait
for the environment to start serving.
