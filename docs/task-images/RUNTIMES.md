# Injecting a second runtime as a layer

[README.md](README.md) explains how the ate-env guest is injected into an
unmodified task image. The same mechanism carries any number of further
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

The guest is the container's only process by contract, which is why it is the
one that supervises the others. Sidecars run without a shell or `PATH` lookup,
so the binary is an absolute path into its mount, and the task image needs
nothing for it. They inherit the container's user, environment and filesystem.

Template derivation keeps all of it. A base with layers yields derived
templates with the same layers and the same guest flags, so environments
created on demand with `task_image=` inherit the second runtime without any caller
knowing it exists.

## Registering a layered template

```bash
ate-env manifest template --template osb-base \
  --task-image     docker.io/library/python@sha256:<digest> \
  --guest-image    <registry>/ate-env-guest@sha256:<digest> \
  --layer          execd=<registry>/opensandbox/execd@sha256:<digest>=/opt/opensandbox \
  --sidecar        '/opt/opensandbox/execd' \
  --sidecar-readyz http://127.0.0.1:44772/ready \
  --snapshots-bucket <object-storage-url> | kubectl-ate create actor-template -f -
```

The resulting template has two image volumes (`execd` at `/opt/opensandbox`,
`guest` at `/ate`), the task image as the container, and the command

```
/ate/ko-app/ate-env-guest -sidecar "/opt/opensandbox/execd" -sidecar-readyz http://127.0.0.1:44772/ready
```

Everything else is as in a plain task-image template. Environments are created
the usual way, from this template by name or from any other image with `--task-image`
while naming it as the base:

```bash
ate-env create py1 --template osb-base
ate-env create node1 --template osb-base --task-image docker.io/library/node@sha256:<digest>
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
sidecar, run under two different task images.

## Who can reach the second runtime

- **Processes inside the environment** reach it at `127.0.0.1:<port>` right
  away. An agent framework whose tool calls run inside the sandbox is served.
- **The guest's own APIs** are unaffected: exec, files and MCP keep going
  through `ate-env-api` on port 80.
- **Clients outside the environment** currently reach only port 80. The atenet
  router forwards to the actor's port 80 unless the request is a CONNECT whose
  authority names another port, and `ate-env-api` proxies only the guest. An
  SDK that speaks HTTP to the second runtime therefore needs one of two things,
  neither of which is in this branch: an endpoint lookup on `ate-env-api` that
  returns a router address naming the runtime's port, or a reverse proxy. This
  is the next step for the OpenSandbox integration, whose lifecycle server
  resolves sandbox endpoints exactly this way.

## Suspend, resume and restarts

A sidecar is part of the actor, so a suspend checkpoints its memory with the
guest's and a resume brings it back without a restart, open sockets included. If
a runtime does not survive that (it polls a clock it can no longer trust, say)
and exits, the guest restarts it: after one second, doubling up to thirty while
it keeps failing, and from one second again once it has run for a minute.
Readiness is gated once, at start; a sidecar that dies later is restarted but
does not take the actor out of service.

## Limits

- **A layer must be self-contained.** The task image's libraries are whatever
  that image ships, so a runtime binary that is dynamically linked works on
  some images and not on others. Ship static binaries (the guest and `execd`
  are) or put the runtime's libraries in the layer and point it at them. The
  failure is easy to recognize: the guest log shows the sidecar exiting at
  once with a loader error such as ``version `GLIBC_2.38' not found`` and
  restarting with growing backoff, readiness never passes, and the actor
  fails after the wakeup probe timeout.
- One container per actor: sidecars share the task image's user, environment
  variables and filesystem, and have no resource limits of their own.
- `--sidecar` values are split on whitespace with no quoting. Arguments that
  need quotes or shell features go in a wrapper script inside the layer.
- Port 80 belongs to the guest. A runtime that insists on it needs a wrapper
  that moves it elsewhere.
- Sidecar output goes to the guest's log, line by line, prefixed with the
  binary's name. There is no separate log stream yet.
- Layers are read-only. A runtime that writes next to its binary needs a
  writable working directory passed as an argument.
