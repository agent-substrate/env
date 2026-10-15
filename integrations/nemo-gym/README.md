# NeMo Gym sandbox provider for Agent Substrate

Runs [NeMo Gym](https://github.com/NVIDIA-NeMo/Gym) rollout sandboxes as **Substrate actors**,
fronted by the `ate-env` API of this repo and reached through its Python client,
[`ate_env`](../../clients/python).
Registers as provider `substrate` next to the built-ins (Docker, Daytona, ECS Fargate, Enroot,
OpenShell, OpenSandbox, Apptainer) via the `nemo_gym.sandbox_providers` entry point — **no changes
to NeMo Gym required**, and no changes to your resource servers beyond the `sandbox:` config block.

Why run rollouts on Substrate:

- **Idle sandboxes hold no worker.** A sandbox that is waiting on model inference is suspended by
  Substrate; the atenet router resumes it transparently on the next command. A rollout fleet is
  mostly waiting, so effective density is bounded by the *active* set, not the fleet size.
- **Repeat sandboxes start from golden snapshots.** The first sandbox on an image pays the boot;
  later ones restore a memory snapshot instead of booting.
- **Prepared states fork.** A sandbox that finished expensive task setup can be snapshotted and
  branched into N independent rollouts (best-of-N from a mid-episode state).

Verified against **nemo-gym 0.5.0**: the registry resolves this provider through the entry point
(`create_provider({"substrate": {...}})`) and the full unit suite passes against the real
`nemo_gym.sandbox.providers.base` types.

## How it maps onto ate-env

One gRPC endpoint, `ate-env-api`, carries everything (see the
[client README](../../clients/python/README.md#how-it-works)):

| Provider call | `ate_env` call | Where it lands |
|---|---|---|
| `create(spec)` | `Client.create(id, template_name=…, template_atespace=…, rootfs_image=…)`, then `Env.shell("true")` until it answers, then `Env.write_file` for `spec.files` | `EnvironmentService` → Substrate control plane; the probe and the files go to the guest |
| `exec(handle, cmd, cwd=, env=, timeout_s=)` | `Env.start_process(["sh","-c",cmd], cwd, env, timeout=)` + `Process.output(follow=True)` | `ProcessService` in `ate-env-guest`, proxied through the atenet router |
| `upload_file` / `download_file` | `Env.write_file` / `Env.read_file_bytes` | `FileSystemService` in the guest |
| `status(handle)` | `Client.get(id)` → lifecycle status | `EnvironmentService`; does **not** wake a parked sandbox |
| `close(handle)` | `Client.delete(id)`, NOT_FOUND-safe | `EnvironmentService` |

Environment ids are `gym-<10 hex>`; each handle remembers the atespace it was created in.

## Prerequisites

On the cluster:

1. **Agent Substrate installed** (`ate-system` namespace healthy — api-server, controller,
   atelet, atenet-router, valkey). See `hack/install-ate.sh` in the
   [substrate repo](https://github.com/agent-substrate/substrate). The guest data
   plane is gRPC through the router, which needs a substrate at or after
   [substrate#1183](https://github.com/agent-substrate/substrate/pull/1183) (h2 on the ingress,
   protocol mirrored to actors). Without it every exec and file call fails with
   `server closed the stream without sending trailers`.
2. **The `ate-env` system deployed** from this repo at or after the `ateenv.v1alpha` proto
   move: namespace, WorkerPool, ActorTemplate(s), and the `ate-env-api` service (see the
   [root README](../../README.md#quickstart)).

   ```bash
   ate-env manifest \
     --guest-image  <ate-env-guest image> \
     --api-image    <ate-env-api image> \
     --worker-image <ateworker-gvisor image matching your substrate build> \
     --snapshots-bucket gs://<bucket>/ate-env/ | kubectl apply -f -
   kubectl get pods -n ate-env   # api + warm workers Running
   ```

   If your `ateapi` requires authentication (it does on any standard install), the
   `ate-env-api` Deployment needs a projected ServiceAccount token with audience
   `api.ate-system.svc` and the `-ateapi-token-file` flag pointing at it.
3. **A base ActorTemplate** in the `ate-env` atespace: `default-template`, which the root
   README's quickstart registers. That is all a task image needs: `SandboxSpec.image` goes to
   `ate-env-api` as `rootfs_image`, and the API derives a template from the base on first use
   (the guest mounted into the image as a read-only volume) and reuses it afterwards. Register
   extra templates only for images that need their own resources or workspace, and map them
   under `create.image_templates`.

   > **Every template must run `ate-env-guest` inside the actor.** Readiness, exec, file
   > transfer, even `create()` completing — all of it goes through the guest's gRPC services
   > behind the router. Derived templates get the guest automatically; a hand-written template
   > without it fails *completely*: the actor starts, but every guest call fails and `create()`
   > times out with `SandboxCreateVerificationError`. Images must be digest-pinned
   > (`repo@sha256:…`); the API rejects tags.
4. **Network path from wherever Gym's rollout workers run** to `ate-env-api`: in-cluster DNS
   (`ate-env-api.ate-env:7777`) or, for a workstation, a port-forward:

   ```bash
   kubectl port-forward -n ate-env svc/ate-env-api 7777:7777
   ```

Locally:

5. **Python ≥ 3.10** and the `ate-env-client` package from this repo's `clients/python` (not on
   PyPI yet). `nemo-gym` itself is only needed on the machine running the Gym resource servers —
   this package's tests run without it (see Testing).

## Task images

Three ways to run a task image, from least to most work:

**Pass the image (default).** Set `SandboxSpec.image` to a digest-pinned reference. `create()`
sends it as `rootfs_image` on the base template; the API derives `<base>-<12 hex of the digest>`
once, bakes its golden snapshot, and later creates on the same image restore from it. Nothing to
register, nothing to rebuild. See [`docs/rootfs-images/README.md`](../../docs/rootfs-images/README.md).

**Register a template per image** when an image needs its own resources, workspace or a second
runtime layer, then map it under `create.image_templates`:

```bash
ate-env manifest template --template gym-py312 \
  --rootfs-image   docker.io/library/python@sha256:<digest> \
  --guest-image    <registry>/ate-env-guest@sha256:<digest> \
  --workspace      /workspace \
  --snapshots-bucket <object-storage-url> | kubectl-ate create actor-template -f -
```

**Bake the guest into the image** (legacy, for control planes without image volumes):
`hack/bake-task-image.sh` builds the guest as a layer on top of any base image with `ko` and
prints the digest-pinned ref; the template's container then runs `/ko-app/ate-env-guest` directly.

Digests for public images: `crane digest --platform linux/amd64 python:3.12-slim`. The task
image needs a `sh` for `exec()`. `timeout_s` no longer needs a `timeout` binary: the guest
enforces the deadline itself.

## Installation

```bash
# from a checkout of this repo:
pip install ./clients/python                       # ate-env-client (import name ate_env)
pip install ./integrations/nemo-gym                # nemo-gym-substrate
# with NeMo Gym in the same environment:
pip install './integrations/nemo-gym[gym]'
```

The `gym` extra resolves only on Python ≥ 3.13.14, the floor the current `nemo-gym` release on PyPI
(0.7.0) declares. The provider, its tests and CI run on Python ≥ 3.10 without it: structural mirrors
of the `nemo_gym` base types stand in.

The entry point makes the provider discoverable immediately:

```python
from nemo_gym.sandbox.providers.registry import create_provider
provider = create_provider({"substrate": {"connection": {"api_url": "127.0.0.1:7777"}}})
```

## How to use

### From NeMo Gym config (the normal path)

Add the `sandbox:` block to your resource-server config and select the `substrate` provider —
nothing else in the Gym setup changes:

```yaml
sandbox:
  default_metadata:
    sandbox-api: substrate
  substrate:
    connection:
      api_url: ate-env-api.ate-env:7777    # host:port or http(s):// URL; port-forward for dev
      request_timeout_s: 30                # deadline per lifecycle RPC (create/get/delete)
    create:
      template: default-template           # ActorTemplate when the spec names none
      atespace: ate-env                    # atespace the templates live in and envs are created in
      ready_timeout_s: 120
      ready_poll_interval_s: 1.0
      image_templates:                     # optional: pin an image to a registered template; others are derived
        python:3.12-slim: gym-py312
```

`namespace` is still accepted as an alias for `atespace` (the name this provider used before
ate-env adopted substrate's vocabulary); setting both is an error. `api_url` is plain gRPC
(h2c) like the Go client and CLI; an `https://` URL opens a TLS channel instead.

Then run your resource server / `gym env start` as usual. Sandboxes created by Gym's resource
lifecycle now appear as Substrate actors; `kubectl get pods -n ate-env` shows the warm workers
hosting them.

### Directly from Python (debugging, scripts)

```python
import asyncio
from nemo_gym_substrate import SubstrateSandboxProvider
from nemo_gym.sandbox.providers.base import SandboxSpec   # or nemo_gym_substrate._compat

async def main():
    provider = SubstrateSandboxProvider({"connection": {"api_url": "127.0.0.1:7777"}})
    handle = await provider.create(SandboxSpec(
        files={"/task/hello.sh": "echo hello from substrate"},
        workdir="/task",
        env={"EPISODE": "1"},
    ))
    result = await provider.exec(handle, "sh /task/hello.sh && echo episode=$EPISODE")
    print(result.stdout, result.return_code)
    await provider.close(handle)
    await provider.aclose()

asyncio.run(main())
```

To share one `ate_env.Client` between the provider and other code, pass it in:
`SubstrateSandboxProvider(config, client=client)`. The provider then never closes it.

### Per-sandbox `provider_options` (on `SandboxSpec`)

| Option | Meaning |
|---|---|
| `template` | ActorTemplate for this sandbox (overrides `create.template` and `image_templates`) |
| `atespace` | Atespace of that template, and of the environment (`namespace` accepted as alias) |

Unknown keys are rejected at `create()` time, as are unknown keys anywhere in the config block.

## Testing

```bash
cd integrations/nemo-gym
python3 -m venv .venv
.venv/bin/pip install ../../clients/python -e '.[test]'

# Unit/contract tests — hermetic: an in-process fake ate-env-api (tests/fakes.py)
# behind the real ate_env client; no cluster, no nemo-gym needed (base types fall
# back to structural mirrors in _compat.py; with nemo-gym installed the same
# tests run against the real types):
.venv/bin/python -m pytest tests/test_provider.py -q      # or: make nemo-gym-test (repo root)

# End-to-end against a live cluster (full lifecycle on a real actor):
kubectl port-forward -n ate-env svc/ate-env-api 7777:7777 &
SUBSTRATE_E2E_API_URL=127.0.0.1:7777 .venv/bin/python -m pytest tests/test_e2e.py -q
# SUBSTRATE_E2E_TEMPLATE=<name> picks the ActorTemplate (default: default-template)
```

## Benchmark

`benchmarks/rollout_bench.py` shapes load like a NeMo-RL rollout batch: N parallel rollouts,
each *create → seed task file → T agent turns (exec + mocked model think time) → close*, on real
actors:

```bash
python benchmarks/rollout_bench.py --api-url 127.0.0.1:7777 --rollouts 5 --turns 3 --think-s 2
```

`benchmarks/driver_sim.py` is the start-time / time-to-first-exec probe over a list of templates
(one task per template per rollout), the same shape as the SWE-500 campaign's eval arm.

First numbers on a small test cluster (5 warm workers, gVisor, cold creates; measured with the
earlier HTTP guest proxy, 2026-09-03 — rerun on the gRPC path before quoting):

| Phase | p50 | max | Note |
|---|---|---|---|
| create | 4.95 s | 18.1 s | cold resume; max shows 5-way contention for 5 workers |
| exec (turn) | 0.14 s | 0.15 s | router + guest round trip |
| close | 3.06 s | 4.0 s | delete path |

Turn overhead is already negligible against model think time; create/close dominate — which is
exactly what golden-snapshot starts and fork-from-golden creates are for.

## Resource mapping and isolation

- One Gym sandbox = one Substrate **actor**: a gVisor (or micro-VM) sandbox multiplexed onto a
  warm WorkerPool worker. Isolation is the sandbox class; actors share worker nodes.
- `SandboxSpec.image` maps to a registered ActorTemplate via `image_templates`, or is sent as
  `rootfs_image` and served from a derived template. The first create on an image pulls it and
  bakes a golden snapshot; later creates restore from it.
- `spec.files` are seeded through the guest filesystem service after readiness. `spec.env` /
  `spec.workdir` are applied per exec (`StartProcess` takes `env` and `cwd` natively).
- `exec(timeout_s=…)` is enforced guest-side and client-side. The guest kills the process group
  with SIGKILL at the deadline, so the exit code is 137, and the provider appends
  `substrate provider: exec timed out after …` to stderr. If the guest does not answer within
  `timeout_s` plus a few seconds, the client kills the process and returns a `return_code=-1`
  sentinel result rather than raising. Transport failures (env deleted mid-exec, router down)
  raise `ate_env.EnvError` subclasses.
- `status()` reads the environment's lifecycle state from `ate-env-api` without touching the
  guest, so polling it never wakes a parked sandbox. A **suspended or paused actor reports
  RUNNING** — the router auto-resumes it on the next command, which is the behavior a rollout
  loop wants. `RESUMING` → `STARTING`, `CRASHED` → `ERROR`, deleted or `DELETING` → `STOPPED`.

Known limits (tracked in the RL-on-Substrate proposal):

- `spec.ttl_s` is **not enforced** — substrate has no server-side TTL yet; clean up via
  `close()`. A crashed harness leaks actors until deleted.
- `exec(user=…)` raises: commands run as the actor's configured user.
- `spec.ports` / `SandboxEndpoint` exposure is not implemented in this demo.

## Troubleshooting

| Symptom | Cause / fix |
|---|---|
| `SandboxCreateError: … rootfs_image … pinned by digest` (`InvalidArgument`) | `SandboxSpec.image` is a tag. Use `repo@sha256:<digest>` (`crane digest --platform linux/amd64 <tag>`), or map the image to a registered template under `create.image_templates` |
| `SandboxCreateError: … UNAVAILABLE` at once | `api_url` unreachable — port-forward died, or wrong kubectl context (`kubectl config current-context`) |
| Create times out (`SandboxCreateVerificationError: … UNAVAILABLE` / `INTERNAL`) | Check warm workers exist (`kubectl get pods -n ate-env`) and the template is Ready (`kubectl get actortemplate -n ate-env`). More parallel creates than warm workers will queue. If the message says `server closed the stream without sending trailers`, the router is not carrying gRPC to actors (see Prerequisites #1) |
| Create times out and the actor is `RUNNING` | The template's image does not run `ate-env-guest` — the actor is up but nothing serves the guest API. See "Task images" |
| Every exec fails with `NOT_FOUND` | The environment is gone (deleted by another party, or the `ate-env-api` restarted with a different atespace default). `status()` returns `STOPPED`; create a new sandbox |
| Every exec returns `UNAVAILABLE` with `CERTIFICATE_EXPIRED` | The atenet router's pod certificate expired and wasn't hot-reloaded (seen on routers running > ~1 day): `kubectl -n ate-system rollout restart deploy/atenet-router` |
| Actor stuck `RESUMING` forever (`status()` stays `STARTING`) | Restore landed on a CPU-incompatible node (mixed Intel/AMD pools) or raced a router restart. Delete the env and recreate; long-term fix is CPU-aware placement |
