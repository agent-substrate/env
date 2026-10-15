# Example: the OpenHands agent-server in any image

Runs the [OpenHands](https://github.com/OpenHands/software-agent-sdk) agent-server inside an
unmodified image (a coding-task image, a language base image, anything with `/bin/sh`) and drives
it from outside the cluster, including OpenHands' own CodeAct agent:

- the agent-server comes from a runtime layer mounted read-only at `/opt/oh`
  ([docs/rootfs-images/RUNTIMES.md](../../docs/rootfs-images/RUNTIMES.md)); the image is not rebuilt;
- the guest starts it as a sidecar and holds the actor's readiness until its `/health` answers;
- callers reach it through ate-env-api's port proxy, by headers (`X-Env-Id`, `X-Env-Port`) or by
  the path `/v1alpha/envs/<env>/ports/<port>/...`, plus the agent-server's own API key;
- the OpenHands SDK uses that path as its workspace URL, so `RemoteWorkspace` (commands, files) and
  `RemoteConversation` (the agent loop and its event WebSocket) work without changes.

| file | what it is |
|---|---|
| [`Dockerfile`](Dockerfile) | the runtime layer: a relocatable Python with `openhands-agent-server` |
| [`demo.sh`](demo.sh) | registers a base template and drives the agent-server in one environment per image |
| [`ate_env_workspace.py`](ate_env_workspace.py) | `workspace_for(env)`: an OpenHands `RemoteWorkspace` for an environment |
| [`codeact_agent.py`](codeact_agent.py) | runs OpenHands' CodeAct agent in an environment, with a model or `--scripted` |
| [`scripted_llm.py`](scripted_llm.py) | a stand-in model that replays a fixed CodeAct trajectory, for runs without a model |

## 1. Build the layer

```bash
docker buildx build --platform linux/amd64,linux/arm64 \
  -t <registry>/oh-agent-server-layer:1.55.0 --push examples/openhands-agent-server
crane digest --platform linux/amd64 <registry>/oh-agent-server-layer:1.55.0
```

`AGENT_SERVER_VERSION` selects another release. The layer does not reuse the binary in the published
`ghcr.io/openhands/agent-server` images: that is a PyInstaller build made on Debian bookworm, it needs
glibc 2.36, and on older distributions it fails at start, for example on Ubuntu 22.04 (glibc 2.35):

```
ImportError: /lib/x86_64-linux-gnu/libc.so.6: version `GLIBC_2.36' not found (required by /tmp/_MEI…/libstdc++.so.6)
```

Starting it through its own image's loader does not help either, because PyInstaller re-executes
itself. The layer is [python-build-standalone](https://github.com/astral-sh/python-build-standalone)
CPython (built for glibc 2.17, relocatable) with `openhands-agent-server` installed by pip on Debian
bullseye, so every wheel runs on glibc 2.31 and newer. From the image it is mounted into it uses only
the kernel and `/bin/sh`. It is about 210 MB compressed and 740 MB on disk.

## 2. Register a base template

```bash
ate-env manifest template --template oh-agent-server \
  --guest-image    <registry>/ate-env-guest@sha256:<digest> \
  --layer          oh=<registry>/oh-agent-server-layer@sha256:<digest>=/opt/oh \
  --sidecar        '/opt/oh/python/bin/python3 -m openhands.agent_server --host 0.0.0.0 --port 8000' \
  --sidecar-readyz http://127.0.0.1:8000/health \
  --env            OH_SESSION_API_KEYS_0=<key> \
  --snapshots-bucket <object-storage-url> | kubectl-ate create actor-template -f -
```

Further agent-server settings are more `--env` flags. Any image then runs with it:

```bash
ate-env create my-env --template oh-agent-server --rootfs-image <image>@sha256:<digest>
```

The first create from an image derives a template for it (layer, sidecar and settings included) and
later creates reuse it.

## 3. Use it

Over HTTP, by path or by headers:

```bash
curl -H 'X-Session-API-Key: <key>' -H 'Content-Type: application/json' \
  localhost:7777/v1alpha/envs/my-env/ports/8000/api/bash/execute_bash_command \
  -d '{"command": "uname -a"}'
```

With the OpenHands SDK (`pip install openhands-sdk==1.55.0 openhands-tools==1.55.0`):

```python
from ate_env_workspace import workspace_for

workspace = workspace_for("my-env", api_key="<key>")      # a RemoteWorkspace
print(workspace.execute_command("ls /").stdout)
```

The CodeAct agent, OpenHands' default agent (terminal, file editor, task tracker, finish) running on
the agent-server inside the environment:

```bash
SESSION_KEY=<key> LLM_MODEL=<litellm model> LLM_API_KEY=<key> [LLM_BASE_URL=<url>] \
  python examples/openhands-agent-server/codeact_agent.py my-env "Fix the failing test in /testbed"
```

The agent-server calls the model from inside the environment, so the model endpoint must be reachable
from the actor: allowed by Substrate's egress policy, or served inside the cluster. `--scripted` needs
no model at all. It starts `scripted_llm.py` inside the environment and points the agent at it; the
agent loop, its tools and the event stream run for real, only the decisions are canned:

```
  task    Inspect this sandbox: report the operating system, the working directory and the top-level…
  action  terminal: {'command': 'head -1 /etc/os-release; echo "cwd: $(pwd)"; ls | head -5; mkdir -p /tmp/codeact-demo', …}
  result  terminal: PRETTY_NAME="Ubuntu 22.04.5 LTS" cwd: / ate bin boot dev etc
  action  file_editor: {'command': 'create', 'path': '/tmp/codeact-demo/hello.txt', 'file_text': 'hello from CodeAct\n'}
  result  file_editor: File created successfully at: /tmp/codeact-demo/hello.txt
  action  terminal: {'command': 'cat /tmp/codeact-demo/hello.txt && wc -c /tmp/codeact-demo/hello.txt', …}
  result  terminal: hello from CodeAct 19 /tmp/codeact-demo/hello.txt
  action  finish: {'message': 'Inspected the sandbox and wrote /tmp/codeact-demo/hello.txt.'}
status: finished after 2.0s
```

`demo.sh` does steps 2 and 3 over plain HTTP for a list of images:

```bash
kubectl -n ate-env port-forward deploy/ate-env-api 7777:7777 &
GUEST_IMAGE=<registry>/ate-env-guest@sha256:<digest> \
SNAPSHOTS_BUCKET=<object-storage-url> \
LAYER_IMAGE=<registry>/oh-agent-server-layer@sha256:<digest> \
IMAGES="docker.io/library/ubuntu@sha256:<digest> docker.io/library/node@sha256:<digest>" \
examples/openhands-agent-server/demo.sh
```

It also takes `PORT`, `AGENT_SERVER_ENV` (extra `NAME=VALUE` settings), `COMMAND`, `WORKDIR` and
`KEEP=1` (leave the environments up for the SDK scripts); see its header.

## Verified

GKE with gVisor workers, Substrate from 2026-10-12, ate-env-api with the port proxy, agent-server
1.55.0 from this Dockerfile, 2026-10-15:

| image | base | agent-server answering after create |
|---|---|---|
| `ubuntu:20.04` | glibc 2.31 | 16 s |
| `swebench/sweb.eval.x86_64.django_1776_django-11099` | Ubuntu 22.04, glibc 2.35 | 16 to 23 s |
| `namanjain12/pandas_final` (R2E-Gym) | Ubuntu 22.04, glibc 2.35 | 20 s |
| `node:22-slim` | Debian 12, glibc 2.36 | 16 s |
| `python:3.12-slim` | Debian 13, glibc 2.41 | 16 s |

- `demo.sh` on each: commands ran with the image's own shell and tools, file download worked, a call
  without the key got 401; also with `PORT=3000` and an extra `--env` setting.
- `codeact_agent.py --scripted` in the SWE-bench environment: the CodeAct run above, its event
  WebSocket through ate-env-api and the Substrate router.
- `ate_env_workspace.py` on `node:22-slim` saw the image's own `node v22`; on the SWE-bench image a
  command saw the image's Python 3.11.5, not the layer's 3.12. After a suspend, the next SDK call woke
  the environment and returned within 9 s.
- On that cluster actors had no route to the internet (connection reset), hence `--scripted`.
- The arm64 build of the layer: the agent-server answered `/health` within 9 s in an arm64 Ubuntu 22.04
  container (local Docker); the CodeAct flow ran the same way locally before the cluster run.

## Limits

- One API key per template: it is in the template's environment, readable by anyone who can read
  templates, and shared by the templates derived from it.
- The agent-server runs as the image's user and keeps its state under that user's home. The first
  environment on a node pulls the layer.
- The model is called from inside the environment (see above).
