# Copyright 2026 The Kubernetes Authors & Google LLC
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

"""Local stand-in for sandboxd's process.v1 ProcessService (Execute only).

Mirrors the sandboxd behaviors the SDK's error mapping depends on
(agent-sandbox/packages/sandboxd/pkg/server/process.go):

* empty command -> INVALID_ARGUMENT
* missing executable or cwd -> NOT_FOUND; permission errors -> PERMISSION_DENIED
* the call deadline (or client cancellation) kills the whole process group
* any other start failure -> INTERNAL

It ignores the ``ate-target-actor`` metadata, so every sandbox that targets
this address shares one guest.
"""

from concurrent import futures
import os
import signal
import subprocess
import sys

import grpc

# Find proto directory relative to current file or workspace
this_dir = os.path.dirname(os.path.abspath(__file__))
candidates = [
    os.path.join(this_dir, "../../src/sandbox_sdk/proto"),
    os.path.join(this_dir, "../../../src/sandbox_sdk/proto"),
    "/opt/grpc-daemon/src/sandbox_sdk/proto",
    "/workspace/src/sandbox_sdk/proto",
]
for p in candidates:
    if os.path.isdir(p) and p not in sys.path:
        sys.path.insert(0, p)

from process.v1 import process_pb2, process_pb2_grpc



def _kill_group(proc: subprocess.Popen) -> None:
    try:
        os.killpg(proc.pid, signal.SIGKILL)
    except (ProcessLookupError, PermissionError):
        pass


class MockProcessService(process_pb2_grpc.ProcessServiceServicer):
    """Local in-guest ProcessService gRPC server implementation."""

    def Execute(self, request, context):
        cmd = list(request.config.command)
        if not cmd:
            context.abort(grpc.StatusCode.INVALID_ARGUMENT, "no command specified")
        cwd = request.config.cwd or "/tmp"
        env = dict(os.environ)
        env.update(dict(request.config.env_vars))

        try:
            proc = subprocess.Popen(cmd, cwd=cwd, env=env, stdout=subprocess.PIPE,
                                    stderr=subprocess.PIPE, start_new_session=True)
        except (FileNotFoundError, NotADirectoryError) as e:
            context.abort(grpc.StatusCode.NOT_FOUND, f"command or path not found: {e}")
        except PermissionError as e:
            context.abort(grpc.StatusCode.PERMISSION_DENIED, f"permission denied: {e}")
        except OSError as e:
            context.abort(grpc.StatusCode.INTERNAL, f"failed to start command: {e}")

        # Like sandboxd: when the RPC ends early (deadline or cancel), kill the group.
        context.add_callback(lambda: _kill_group(proc))
        try:
            stdout, stderr = proc.communicate(timeout=context.time_remaining())
        except subprocess.TimeoutExpired:
            _kill_group(proc)
            proc.communicate()
            context.abort(grpc.StatusCode.DEADLINE_EXCEEDED, "command deadline exceeded")
        return process_pb2.ExecuteResponse(exit_code=proc.returncode, stdout=stdout, stderr=stderr)


def start_mock_guest(port: int = 0, host: str = "0.0.0.0"):
    """Start the mock guest; returns ``(server, bound_port)``. Port 0 picks a free port."""
    server = grpc.server(futures.ThreadPoolExecutor(max_workers=10))
    process_pb2_grpc.add_ProcessServiceServicer_to_server(MockProcessService(), server)
    bound = server.add_insecure_port(f"{host}:{port}")
    if bound == 0:
        raise RuntimeError(f"could not bind mock gRPC guest to {host}:{port}")
    server.start()
    return server, bound


def serve_grpc(port: int = 50051, host: str = "0.0.0.0"):
    server, _ = start_mock_guest(port=port, host=host)
    return server

