# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Serve NeMo Skills' local sandbox server (POST /execute, GET /health) from
the layer: ``python3 -m nemo_runtime.sandbox_server --host 0.0.0.0 --port 6000``.

The vendored module is loaded from third_party/nemo-skills so it stays
byte-identical to upstream; this wrapper only chooses the bind address (Flask's
default is 127.0.0.1, which the router cannot reach) and the port."""

from __future__ import annotations

import argparse
import importlib.util
import logging
import os
import sys
from pathlib import Path

_SERVER = Path(__file__).resolve().parent / "third_party" / "nemo-skills" / "local_sandbox_server.py"


def load_app():
    """Import the vendored Flask application under its upstream module name."""
    spec = importlib.util.spec_from_file_location("local_sandbox_server", _SERVER)
    if spec is None or spec.loader is None:
        raise RuntimeError(f"cannot load {_SERVER}")
    module = importlib.util.module_from_spec(spec)
    sys.modules["local_sandbox_server"] = module
    spec.loader.exec_module(module)
    return module.app


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--host", default="0.0.0.0", help="bind address; 0.0.0.0 so the router reaches it")
    ap.add_argument("--port", type=int, default=6000, help="TCP port (NeMo Skills' default is 6000)")
    args = ap.parse_args(argv)
    os.environ.setdefault("WORKER_NUM", "0")
    # `language: python` runs `python -c ...` through PATH. A task image's
    # own python wins when it has one; the layer's interpreter is the
    # fallback, so an image without python still executes code.
    layer_bin = str(Path(sys.executable).resolve().parent)
    os.environ["PATH"] = os.environ.get("PATH", "/usr/local/bin:/usr/bin:/bin") + os.pathsep + layer_bin
    logging.getLogger("werkzeug").setLevel(logging.WARNING)
    app = load_app()
    # One process, many threads: each IPython session already runs in its own
    # child process, and the guest restarts this server if it exits.
    app.run(host=args.host, port=args.port, threaded=True, use_reloader=False)
    return 0


if __name__ == "__main__":
    sys.exit(main())
