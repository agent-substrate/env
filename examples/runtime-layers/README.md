# Example: a second runtime as a layer

Runs an unmodified task image with two injected runtimes: the ate-env guest at
`/ate`, and a static busybox at `/opt/web` standing in for a real runtime
daemon such as OpenSandbox's `execd`. The guest starts `busybox httpd` on port 44772 as a
sidecar and holds the actor's readiness until it answers. The mechanics are the
ones [docs/task-images/RUNTIMES.md](../../docs/task-images/RUNTIMES.md)
describes; only the binary differs.

`demo.sh` registers the template, creates an environment, fetches a file from
the second runtime from inside the environment, optionally creates another
environment from a different image on the same base to show the layer is
inherited, and deletes what it created.

```bash
kubectl -n ate-env port-forward svc/ate-env-api 7777:7777 &

GUEST_IMAGE=<registry>/ate-env-guest@sha256:<digest> \
SNAPSHOTS_BUCKET=<object-storage-url> \
TASK_IMAGE=docker.io/library/python@sha256:<digest> \
LAYER_IMAGE=docker.io/library/busybox@sha256:<digest of the 1.37-musl tag> \
OTHER_IMAGE=docker.io/library/node@sha256:<digest> \
examples/runtime-layers/demo.sh
```

Digests for public images come from `crane digest --platform linux/amd64
python:3.12-slim` or `docker manifest inspect`. The guest image must be built
from a revision of this repo that has the `-sidecar` flags.

Use the `busybox:1.37-musl` tag, which is statically linked. The default
`busybox:1.37` is built against glibc and only runs on task images whose glibc
is at least as new as its own: it works inside `python:3.12-slim` and fails
inside `debian:bookworm-slim` with ``GLIBC_2.38' not found``, which is the
textbook case of why a runtime layer has to be self-contained.

What the template looks like, as `ate-env manifest template` prints it:

```yaml
containers:
- command:
  - /ate/ko-app/ate-env-guest
  - -sidecar
  - /opt/web/bin/busybox httpd -f -p 44772 -h /
  - -sidecar-readyz
  - http://127.0.0.1:44772/etc/hostname
  image: docker.io/library/python@sha256:<digest>
  name: guest
  readyz: {httpGet: {path: /readyz, port: 80}}
  volumeMounts:
  - {name: web, mountPath: /opt/web}
  - {name: guest, mountPath: /ate}
volumes:
- {name: web, image: {reference: docker.io/library/busybox@sha256:<digest>}}
- {name: guest, image: {reference: <registry>/ate-env-guest@sha256:<digest>}}
```

To swap in a real runtime, change the three values: the layer image, the
sidecar command, and the readiness URL. For `execd` that is its image mounted
at `/opt/opensandbox`, `/opt/opensandbox/execd` with its flags, and
`http://127.0.0.1:44772/ready`.
