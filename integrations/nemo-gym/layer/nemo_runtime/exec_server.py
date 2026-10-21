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

"""Serve NeMo Gym's exec server (POST /exec, /upload, GET /download, /health)
from the layer: ``python3 -m nemo_runtime.exec_server --host 0.0.0.0 --port 19542``.

The vendored script reads TB_EXEC_BIND and TB_EXEC_PORT; this wrapper sets them
from its flags and runs the script as __main__."""

from __future__ import annotations

import argparse
import os
import runpy
import sys
from pathlib import Path

_SERVER = Path(__file__).resolve().parent / "third_party" / "nemo-gym" / "exec_server.py"


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--host", default="0.0.0.0", help="bind address; 0.0.0.0 so the router reaches it")
    ap.add_argument("--port", type=int, default=19542, help="TCP port (NeMo Gym's default is 19542)")
    args = ap.parse_args(argv)
    os.environ["TB_EXEC_BIND"] = args.host
    os.environ["TB_EXEC_PORT"] = str(args.port)
    runpy.run_path(str(_SERVER), run_name="__main__")
    return 0


if __name__ == "__main__":
    sys.exit(main())
