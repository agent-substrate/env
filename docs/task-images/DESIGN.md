# Design: injecting the guest into unmodified task images

Status: implemented. Companion to [README.md](README.md), which is the how-to.

## Problem

Every ate-env operation goes through `ate-env-guest`, a daemon inside the
actor. Until now the only way to run an image as an environment was to make the
guest the container image, which for a task image meant rebuilding it with the
guest layered on top and set as the entrypoint. For one image that is a `ko`
invocation. For an evaluation or RL fleet it is a build and a push per image,
duplicated guest bytes in the registry, a rebuild of every image for every guest
change, and a registry pipeline the harness has to own.

The two things a container sources, its rootfs and its process, can be supplied
independently. Substrate's ActorTemplate already allows a second OCI image to be
mounted read-only into the actor. This design uses that to inject the guest.

## Goals

- Run any digest-pinned image as an environment with no rebuild.
- Keep the guest behavior, API and client code identical in both modes.
- Make the on-demand path idempotent and safe under concurrency, so harnesses
  can call it per task without coordinating.
- Keep one place that knows the mount layout, so a second injected runtime can
  reuse it.

Non-goals: validating that an image exists or starts (the actor reports that),
per-create resource overrides, template garbage collection, and multi-container
templates. These are listed under future work.

## Design

### Template derivation

`internal/apiservice/imagetemplate.go` holds `DeriveImageTemplate(base, name,
image)`. Given a base template it returns a new template that:

- keeps the base's worker selector, snapshot configuration, sandbox
  configuration, resources, container env and readiness probe;
- replaces the first container's image with the task image;
- if the base runs the guest as its image, adds a read-only image volume named
  `guest` whose reference is the base's image, mounts it at `/ate`, and moves
  the command's first element under `/ate` (`/ko-app/ate-env-guest` becomes
  `/ate/ko-app/ate-env-guest`);
- if the base already has an image volume, leaves volumes, mounts and command
  alone;
- drops server-assigned metadata and status.

It refuses an unpinned task or guest image, a base without containers, and a
base command that is not an absolute path (a shell wrapper cannot be re-rooted
and would produce a template whose guest is silently missing).

Both entry points use it: `ate-env manifest template --task-image` derives from
the guest-image template it would otherwise print, and `ate-env-api` derives
from a template fetched from the control plane.

### Naming and idempotency

A derived template is named `<base>-<first 12 hex digits of the task image
digest>`, in the base's atespace. The name is a pure function of the inputs a
caller controls, so:

- the same image always maps to the same template, and a lookup by name is the
  reuse check;
- a human can find the template for an image from its digest;
- 12 hex digits (48 bits) make accidental collisions a non-concern at any
  realistic image count, while leaving room under the 63-character resource
  name limit for a base name of up to 50 characters. Longer bases are rejected
  with a message naming the limit.

The guest is deliberately not part of the name. Templates are immutable in
Substrate, so a guest change is a new base template with a new name, which
yields new derived names. Encoding the guest digest would only lengthen names
without adding information.

### Server flow

`CreateEnvironment` with `image` set:

1. Validate the digest and compute the derived name; invalid input is
   `InvalidArgument` and touches nothing.
2. `GetActorTemplate(derived)`. If it exists and its container image is the
   requested image, use it. If it exists with a different image, return
   `FailedPrecondition` naming both images: someone else owns that name.
3. Otherwise `GetActorTemplate(base)`; a missing base is `FailedPrecondition`
   with the atespace and the command that registers one.
4. Derive and `CreateActorTemplate`. `AlreadyExists` is success: a concurrent
   create won the race and built the same template.
5. Create the actor from the derived template and report it in the response.

The check in step 2 compares the container image only. Volumes and command are
not compared because the only way for them to differ is a template created by
other means under the derived name, which the image comparison already flags
in practice; a stricter comparison would cost a base fetch on every reuse.

### API shape

`image` is a field on `CreateEnvironmentRequest` rather than a new RPC, and the
existing `template` field doubles as the base. This keeps one create path, lets
clients that already pass a template start passing an image with one more
argument, and makes "which base" an explicit, per-request choice rather than
server configuration. The derived template is surfaced through the existing
`Environment.template`, so nothing new is needed to observe it.

### Placement

Derived templates live in the environment's atespace, next to the base. This is
also where `ate-env-api` already creates actors, so no new permissions are
involved beyond template creation, which the API server's identity needs for
this feature.

## Failure semantics

| Situation | Result |
|---|---|
| unpinned task image | `InvalidArgument`, nothing created |
| base missing | `FailedPrecondition`, nothing created |
| base command not re-rootable | `InvalidArgument`, nothing created |
| derived name held by another image | `FailedPrecondition`, nothing created |
| two creates race on a new image | both succeed, one template |
| image cannot be pulled or guest cannot start | create succeeds; actor never becomes ready; golden bake fails and the template reports it |
| control plane unavailable mid-flow | the underlying gRPC status is returned; a half-created template is reused next time because the name is deterministic |

## Security

- The task image is run with the same sandbox class and configuration as the
  base, so injection changes what runs, not how it is isolated.
- The guest volume is read-only. The task image cannot alter the guest.
- Digest pinning is enforced for both images at every entry point. A tag could
  be re-pointed between the golden bake and a later cold boot, which would
  invalidate snapshots and make two environments on "the same image" differ.
- Callers of `CreateEnvironment` can now cause template creation. An
  `ate-env-api` that is reachable by untrusted callers should already be behind
  authentication; this adds template records and golden snapshots to what such
  a caller can accumulate.

## Performance

The first environment on an image pays the task image pull on the worker it
lands on and a cold boot; the derived template's golden bake runs concurrently
on another worker. Later environments restore from the golden. In the
verification run below the first environment answered a shell command 37 s
after the create call on a small two-worker pool, most of it image pull.

Costs that scale with the number of distinct images: one template record and
one golden snapshot per image. Neither is reclaimed automatically.

## Alternatives considered

- **Bake the guest into each image.** Works today and remains supported; it is
  the per-image build cost this design removes.
- **An init container that copies the guest into a shared volume.** Needs a
  writable volume and an extra container per actor, and the copy happens on
  every cold boot. An image volume is shared by the node's cache and needs no
  copy.
- **A server-side image-to-template map.** Equivalent to the derived name, but
  state that can drift from the control plane. The name derivation is stateless
  and recoverable from the templates themselves.
- **Including the guest digest in the derived name.** Rejected because template
  immutability already forces a new base name for a new guest; see Naming.
- **A separate `CreateEnvironmentFromImage` RPC.** More surface for the same
  behavior; a field keeps one path.

## Future work

- **Second injected runtime.** The derivation is runtime-agnostic apart from
  the binary path. A runtime such as OpenSandbox's `execd` or E2B's `envd` is a
  second image volume plus a command and readiness probe; the base template
  describes which runtime it carries.
- **Per-create overrides** for resources and workspace, so one base can serve
  images with different needs.
- **Derived-template garbage collection**, by age or by last use, with the
  golden snapshot removed alongside.
- **Image validation at create time**, so a bad reference fails the call
  instead of the actor.
- **Template atespace.** `CreateEnvironment` takes `template.atespace` but the
  server creates the actor and resolves the template in the environment's
  atespace; this predates the change and is unchanged by it.

## Verification

- Unit tests cover derivation from both base kinds, naming and its limits, and
  every rejection.
- Server tests against the in-process fake control plane cover first create,
  reuse, a named base in another atespace, and the failure table.
- `clients/python/tests/e2e/test_full_stack.py` has a create-from-image test
  gated on `ATE_ENV_TASK_IMAGE`, which checks that the guest comes from the
  volume and not the image, that the rootfs is the task image's, that files
  round-trip, and that a second environment reuses the derived template.
- Run once end to end on a Kubernetes cluster running Substrate with a fresh
  two-worker ate-env: `ate-env create --image` of an unmodified
  `python:3.12-slim` produced the derived template, served `python3 --version`
  from the image's own rootfs with the guest present under `/ate`, and deleted
  cleanly.
