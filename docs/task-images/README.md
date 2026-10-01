# Running task images on ate-env

Any OCI image can run as an ate-env environment without being rebuilt. The
`ate-env-guest` daemon that serves exec and file operations is mounted into the
image's container as a read-only image volume and started from there, so the
image's filesystem, interpreters and tools are exactly what was published.

This page is the how-to. The reasoning behind it is in [DESIGN.md](DESIGN.md).

## How it works

An ActorTemplate normally names one container image, and for ate-env that image
has been the guest itself. A *task-image template* splits the two things a
container sources:

```
rootfs   = the task image, unmodified, pinned by digest
process  = /ate/ko-app/ate-env-guest, from a second image mounted read-only at /ate
```

Substrate materializes both at actor start. Everything else about the template
(worker selector, snapshot settings, sandbox class, resources, readiness probe)
is the same as for a guest-image template, and the guest behaves identically:
the environment's `shell`, process and file APIs, MCP, and suspend and resume
all work unchanged.

## Prerequisites

- A Substrate control plane whose ActorTemplate API supports image volumes
  (`volumes[].image`). Every supported release does.
- Images pinned by digest (`repo@sha256:...`), both the task image and the
  guest. Substrate requires this because a changed image invalidates snapshots.
- Worker nodes that can pull the task image's registry.
- A base template registered with `ate-env manifest template` (the quickstart
  in the root README does this). Its guest image is the one injected.

What the task image needs:

- A `sh` for `Env.shell()` and for the `shell` MCP tool. Images without one
  (distroless) still work for `start_process` with absolute binaries and for
  file transfer.
- Port 80 free inside the sandbox: the guest listens there and the readiness
  probe hits `/readyz` on it.
- Nothing else. The guest is a static binary and runs as the image's default
  user; a writable workspace is wherever `--workspace` points (the guest's
  default is `/`).

## Option A: register a task-image template up front

Good for a fixed set of images, or when you want the template to carry its own
name, resources or workspace.

```bash
ate-env manifest template --template py312 \
  --task-image     docker.io/library/python@sha256:<digest> \
  --guest-image    <registry>/ate-env-guest@sha256:<digest> \
  --workspace      /workspace \
  --snapshots-bucket <object-storage-url> | kubectl-ate create actor-template -f -

ate-env create job1 --template py312
```

`--task-image` makes the task image the container and the guest an image
volume; `--workspace` is passed to the guest as `-workspace` in either mode. The
printed template is plain YAML you can edit before registering it (resources,
worker selector, sandbox class).

## Option B: create from an image on demand

Good when the set of images is open-ended or chosen by a caller, as in RL and
evaluation harnesses.

```bash
ate-env create py1 --image docker.io/library/python@sha256:<digest>
```

```python
env = await client.create("py1", image="docker.io/library/python@sha256:<digest>")
(await env.info()).template.name   # "default-template-<12 hex of the digest>"
```

```go
client.Create(ctx, &ateenvv1alpha.CreateEnvironmentRequest{
    Id: "py1", Image: "docker.io/library/python@sha256:<digest>",
})
```

What happens on the server:

1. The request's template (default `default-template`) is the **base**. It must
   exist in the environment's atespace.
2. `ate-env-api` looks for a template named `<base>-<first 12 hex digits of the
   digest>` next to the base. If it exists and runs that image, it is reused.
3. Otherwise the server derives it from the base and creates it: same worker
   selector, snapshot and sandbox settings, env and readiness; the task image as
   the container; the base's guest image as a volume at `/ate`; the command
   re-rooted to `/ate/ko-app/ate-env-guest`.
4. The environment is created from the derived template, which the response and
   `GetEnvironment` report.

The first environment on an image pays the image pull and a cold boot while the
template's golden snapshot bakes in the background; later ones restore from the
golden. Concurrent first creates on the same image are safe: whichever wins
creates the template, the others reuse it.

Templates created this way are visible like any other:

```bash
kubectl-ate get actor-template --atespace ate-env
```

A fresh template shows `Failed` in that listing for a few seconds while its
golden bakes, then `Ready`. Environments can be created in the meantime.

## A second runtime

The same mechanism carries more runtimes. `--layer` mounts another image,
`--sidecar` has the guest start a process from it, and `--sidecar-readyz`
folds its readiness into the actor's. See [RUNTIMES.md](RUNTIMES.md) and the
runnable example in [`examples/runtime-layers`](../../examples/runtime-layers).

## Errors

| Error | Meaning |
|---|---|
| `InvalidArgument: image ... is not pinned by digest` | Pass `repo@sha256:<64 hex>`; tags are rejected before anything is created |
| `FailedPrecondition: base actor template ... not found` | Register the base in the environment's atespace first (`ate-env manifest template`) |
| `FailedPrecondition: actor template ... exists but runs ...` | The derived name is taken by a template with a different image; delete it or use another base name |
| `InvalidArgument: ... cannot re-root command ...` | The base's command is not an absolute path to the guest (a shell wrapper); fix the base template |
| Environment never serves, derived template stays `Failed` | The task image could not be pulled or does not start the guest (port 80 taken, no exec permission); check the worker pod logs |

## Cleanup and limits

- Derived templates are not deleted when environments are. They are small
  control-plane records plus one golden snapshot in object storage each; remove
  them with `kubectl-ate delete actor-template` when an image is retired.
- Bumping the guest means a new base template (templates are immutable), and a
  new base name yields new derived names. Existing derived templates keep the
  guest they were built with.
- Resources, sandbox class and worker selector come from the base; a task image
  that needs more is served by registering its own template (Option A) or a
  second base.
- The task image's existence is not checked at create time. A bad reference is
  reported by the actor failing to start, not by `CreateEnvironment`.
