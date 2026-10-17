# Injecting a second runtime as a layer

[README.md](README.md) explains how the ate-env guest is injected into an
unmodified rootfs image. The same mechanism carries any number of further
runtimes: a second daemon that an agent framework expects inside its sandbox,
such as OpenSandbox's `execd`, is one more read-only image volume plus a process
the guest starts next to itself. This page explains the model, the flags, and
what reaches the second runtime from where.

## Model

A runtime layer is three things:

| part | where it is expressed | what it does |
|---|---|---|
| the binary | an image volume on the ActorTemplate (`--layer name=image@sha256:...=/mount`) | mounts the runtime's image read-only at a path of your choice, next to the guest at `/ate` |
| the process | a guest flag (`--sidecar '/mount/binary args...'`) | the guest starts it after it is listening, logs its output with a prefix, and restarts it with backoff if it exits |
| its readiness | a guest flag (`--sidecar-readyz http://127.0.0.1:PORT/health`) | the guest's `/readyz`, which is the actor's wakeup probe, answers 503 until every such URL has answered 2xx once |
| its configuration | a template flag (`--env NAME=VALUE`) | sets a variable on the container; the guest passes its environment to sidecars |

The guest is the container's only process by contract, which is why it is the
one that supervises the others. Sidecars run without a shell or `PATH` lookup,
so the binary is an absolute path into its mount, and the rootfs image needs
nothing for it beyond, for a binary that is not statically linked, a
compatible C library (the OpenHands layer needs glibc 2.31 or newer). They inherit the container's user, environment and filesystem.

Template derivation keeps all of it. A base with layers yields derived
templates with the same layers and the same guest flags, so environments
created on demand with `rootfs_image=` inherit the second runtime without any caller
knowing it exists.

## Registering a layered template

```bash
ate-env manifest template --template osb-base \
  --rootfs-image     docker.io/library/python@sha256:<digest> \
  --guest-image    <registry>/ate-env-guest@sha256:<digest> \
  --layer          execd=<registry>/opensandbox/execd@sha256:<digest>=/opt/opensandbox \
  --sidecar        '/opt/opensandbox/execd' \
  --sidecar-readyz http://127.0.0.1:44772/ready \
  --snapshots-bucket <object-storage-url> | kubectl-ate create actor-template -f -
```

The resulting template has two image volumes (`execd` at `/opt/opensandbox`,
`guest` at `/ate`), the rootfs image as the container, and the command

```
/ate/ko-app/ate-env-guest -sidecar "/opt/opensandbox/execd" -sidecar-readyz http://127.0.0.1:44772/ready
```

Everything else is as in a plain rootfs-image template. Environments are created
the usual way, from this template by name or from any other image with `--rootfs-image`
while naming it as the base:

```bash
ate-env create py1 --template osb-base
ate-env create node1 --template osb-base --rootfs-image docker.io/library/node@sha256:<digest>
```

Both run the guest and `execd`; the second one from a derived template named
`osb-base-<12 hex of the node digest>`.

### OpenSandbox `execd` specifics

`execd` is a static binary that OpenSandbox injects into sandboxes without
modifying the base image, which is exactly the shape this page describes. Its
image carries the binary at `/execd`; mounting the image at `/opt/opensandbox`
puts it at `/opt/opensandbox/execd`, OpenSandbox's own default install path. It
serves HTTP on port 44772 by default, with `/ping` and `/ready` open and every
other endpoint behind the `X-EXECD-ACCESS-TOKEN` header when a token is
configured. Check the OpenSandbox documentation for the current flags and
environment variables for the port, token and workspace; pass them in the
`--sidecar` value, or wrap them in a script shipped in the layer if quoting is
needed. Its optional Jupyter code-interpreter mode starts a Jupyter server
inside the sandbox; for fleets of small environments use the plain command and
file endpoints.

A verified stand-in for the mechanics, which needs nothing from any registry
but Docker Hub, is in [`examples/runtime-layers`](../../examples/runtime-layers):
the static `busybox:1.37-musl` mounted as the layer and its `httpd` as the
sidecar, run under two different rootfs images. A real daemon, the OpenHands agent-server in
any image with glibc 2.31 or newer, configured by `--env`, is the
[`integrations/openhands`](../../integrations/openhands) package: an OpenHands SDK workspace
(`AteEnvWorkspace`, compatible with OpenHands' `AgentSandboxWorkspace`), the layer, and examples
that drive it with OpenHands' own CodeAct agent.

## Who can reach the second runtime

- **Processes inside the environment** reach it at `127.0.0.1:<port>` right
  away. An agent framework whose tool calls run inside the sandbox is served.
- **The guest's own APIs** are unaffected: exec, files and MCP keep going
  through `ate-env-api` on port 80.
- **Clients outside the environment** reach it through `ate-env-api`. A plain
  HTTP request to the API's listener that carries `X-Env-Id`, `X-Env-Port` and
  optionally `X-Env-Atespace` goes to that port in that environment, whatever
  its path: `ate-env-api` opens a CONNECT tunnel through the atenet router's
  CONNECT listener (`--atenet-connect`, port 8081 on a stock install), the
  router resumes the actor if needed and forwards to the port, and the request
  and its streamed response pass through unchanged apart from the routing
  headers, which are stripped. Idle tunnels are reused, one pool for each
  environment and port.

  ```bash
  curl -H 'X-Env-Id: py1' -H 'X-Env-Port: 44772' localhost:7777/ready
  curl localhost:7777/v1alpha/envs/py1/ports/44772/ready   # the same, for clients without headers
  ```

  The runtime must listen on all interfaces, not 127.0.0.1: the router
  delivers to the actor's address, and a loopback-only listener answers
  `502 bad gateway`.

  The path form, `/v1alpha/envs/{id}/ports/{port}/...` (or
  `/v1alpha/atespaces/{atespace}/envs/...`), is for clients that cannot set
  headers: the prefix is stripped, so it works as a base URL. The tunnel
  carries HTTP/1.1, not raw TCP. Streamed responses are flushed as they arrive
  and WebSocket upgrades pass through; gRPC to the runtime is not supported
  yet. The NeMo Gym
  provider hands this out as its `endpoint(handle, port)`
  ([integrations/nemo-gym](../../integrations/nemo-gym/README.md)).
- **The runtime must listen on `0.0.0.0`.** The router reaches the port on
  the actor's address. A runtime bound to `127.0.0.1` serves only processes
  inside the environment, and requests from outside fail with 502.
- **Nobody is authenticated on the way in.** `ate-env-api` forwards for any
  caller that reaches its listener, to any port of any actor the router
  serves. A runtime that executes commands or edits files must check its own
  credential, as OpenSandbox's `execd` token and the OpenHands agent-server's
  session key do. See the README section "Reaching a service inside an
  environment" for the browser checks and the flags.

## Suspend, resume and restarts

A sidecar is part of the actor, so a suspend checkpoints its memory with the
guest's and a resume brings it back without a restart, open sockets included. If
a runtime does not survive that (it polls a clock it can no longer trust, say)
and exits, the guest restarts it: after one second, doubling up to thirty while
it keeps failing, and from one second again once it has run for a minute.
Readiness is gated once, at start; a sidecar that dies later is restarted but
does not take the actor out of service.

## Limits

- **A layer must be self-contained.** The rootfs image's libraries are whatever
  that image ships, so a runtime binary that is dynamically linked works on
  some images and not on others. Ship static binaries (the guest and `execd`
  are) or put the runtime's libraries in the layer and point it at them. The
  failure is easy to recognize: the guest log shows the sidecar exiting at
  once with a loader error such as ``version `GLIBC_2.38' not found`` and
  restarting with growing backoff, readiness never passes, and the actor
  fails after the wakeup probe timeout.
- One container per actor: sidecars share the rootfs image's user, environment
  variables and filesystem, and have no resource limits of their own.
- `--sidecar` values are split on whitespace with no quoting. Arguments that
  need quotes or shell features go in a wrapper script inside the layer.
- Port 80 belongs to the guest. A runtime that insists on it needs a wrapper
  that moves it elsewhere.
- Sidecar output goes to the guest's log, line by line, prefixed with the
  binary's name. There is no separate log stream yet.
- Layers are read-only. A runtime that writes next to its binary needs a
  writable working directory passed as an argument.
