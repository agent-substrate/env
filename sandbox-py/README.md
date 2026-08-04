# substrate-sandbox (Python SDK)

Python SDK for [substrate-sandbox](https://github.com/agent-substrate/sandbox):
isolated, stateful execution environments on
[Agent Substrate](https://github.com/agent-substrate/substrate) that can be
suspended, resumed on any available worker, and driven remotely with command
execution and filesystem operations.

> [!WARNING]
> This is an alpha API and is likely to change until v1.0 is released.

The SDK talks to the ssbx-api service (see the repository README for
deploying it) and has no dependencies beyond the Python 3.9+ standard
library.

## Usage

```python
from ssbx import SandboxClient

client = SandboxClient("http://localhost:7777")  # ssbx-api

sb = client.create("dev1")
sb.write_file("/workspace/note.txt", "hello")

result = sb.cmd("cat /workspace/note.txt")
print(result.stdout, result.exit_code)

sb.suspend()          # snapshot to object storage, free the worker
sb.cmd("uname -a")    # auto-resumes transparently

sb.delete()
```

Errors carry the API's machine-readable code; missing sandboxes, files,
and directories raise `NotFoundError`:

```python
from ssbx import NotFoundError

try:
    client.open("absent")
except NotFoundError:
    ...  # handle the missing sandbox
```

## Development

```bash
PYTHONPATH=src python3 -m unittest discover -s tests  # runs against a fake API server
```
