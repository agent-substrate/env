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

"""Fakes for the workspace tests: ate-env-api's environment service over gRPC,
and its port proxy over HTTP in front of a real agent-server on localhost.

The fake proxy keeps the router's behaviour that the workspace relies on: an
unknown environment is a 404, a suspended one is woken by the request, the
routing prefix is stripped, and HTTP upgrades are relayed both ways.
"""

from __future__ import annotations

import contextlib
import http.client
import re
import socket
import threading
from collections.abc import Iterator
from dataclasses import dataclass, field
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import grpc
import grpc.aio
from ate_env import EnvironmentStatus
from ate_env._gen.ateenv.v1alpha import env_pb2, env_pb2_grpc
from ate_env.loop_runner import LoopRunner

DEFAULT_ATESPACE = "ate-env"
DEFAULT_TEMPLATE = "default-template"


@dataclass
class FakeAteEnv:
    """State shared by the fake services, plus the knobs tests script."""

    envs: dict[str, dict] = field(default_factory=dict)
    creates: list[dict] = field(default_factory=list)
    deletes: list[str] = field(default_factory=list)
    suspends: list[str] = field(default_factory=list)
    create_code: grpc.StatusCode | None = None
    create_but_fail: bool = False  # record the env, then fail the call
    upstreams: dict[int, str] = field(default_factory=dict)  # port -> host:port
    unhealthy_polls: int = 0  # answer this many /health requests with 503 first
    wakes: int = 0
    proxied: list[dict] = field(default_factory=list)
    lock: threading.Lock = field(default_factory=threading.Lock)

    def env_pb(self, env_id: str) -> env_pb2.Environment:
        e = self.envs[env_id]
        return env_pb2.Environment(
            id=env_id,
            atespace=e["atespace"],
            template=env_pb2.Template(name=e["template"], atespace=e["template_atespace"]),
            status=e["status"],
        )


class FakeEnvironmentService(env_pb2_grpc.EnvironmentServiceServicer):
    def __init__(self, state: FakeAteEnv) -> None:
        self.state = state

    async def CreateEnvironment(self, request, context):
        s = self.state
        s.creates.append({
            "id": request.id,
            "atespace": request.atespace,
            "template": request.template.name,
            "template_atespace": request.template.atespace,
            "rootfs_image": request.rootfs_image,
        })
        if s.create_code is not None:
            await context.abort(s.create_code, "create refused")
        if request.id in s.envs:
            await context.abort(grpc.StatusCode.ALREADY_EXISTS, "exists")
        s.envs[request.id] = {
            "atespace": request.atespace or DEFAULT_ATESPACE,
            "template": request.template.name or DEFAULT_TEMPLATE,
            "template_atespace": request.template.atespace or DEFAULT_ATESPACE,
            "status": int(EnvironmentStatus.RUNNING),
        }
        if s.create_but_fail:
            await context.abort(grpc.StatusCode.DEADLINE_EXCEEDED, "lost the answer")
        return env_pb2.CreateEnvironmentResponse(environment=s.env_pb(request.id))

    async def GetEnvironment(self, request, context):
        if request.id not in self.state.envs:
            await context.abort(grpc.StatusCode.NOT_FOUND, "no such env")
        return env_pb2.GetEnvironmentResponse(environment=self.state.env_pb(request.id))

    async def SuspendEnvironment(self, request, context):
        if request.id not in self.state.envs:
            await context.abort(grpc.StatusCode.NOT_FOUND, "no such env")
        self.state.suspends.append(request.id)
        self.state.envs[request.id]["status"] = int(EnvironmentStatus.SUSPENDED)
        return env_pb2.SuspendEnvironmentResponse()

    async def DeleteEnvironment(self, request, context):
        self.state.deletes.append(request.id)
        if self.state.envs.pop(request.id, None) is None:
            await context.abort(grpc.StatusCode.NOT_FOUND, "no such env")
        return env_pb2.DeleteEnvironmentResponse()


_PATH = re.compile(r"^/v1alpha/(?:atespaces/(?P<a>[^/]+)/)?envs/(?P<id>[^/]+)/ports/(?P<port>[0-9]+)(?P<rest>/.*)?$")
_HOP = {"connection", "keep-alive", "proxy-connection", "transfer-encoding", "upgrade", "host", "te", "trailer"}


def _relay(a: socket.socket, b: socket.socket) -> None:
    try:
        while data := a.recv(65536):
            b.sendall(data)
    except OSError:
        pass
    finally:
        with contextlib.suppress(OSError):
            b.shutdown(socket.SHUT_WR)


def _handler(state: FakeAteEnv):
    class Handler(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def log_message(self, *args) -> None:
            pass

        def _route(self):
            path, _, query = self.path.partition("?")
            m = _PATH.match(path)
            if not m:
                self._reply(404, "not a port-proxy path")
                return None
            env_id, atespace = m["id"], m["a"] or DEFAULT_ATESPACE
            with state.lock:
                env = state.envs.get(env_id)
                if env is None or env["atespace"] != atespace:
                    self._reply(404, f"no actor {env_id}")
                    return None
                if env["status"] == int(EnvironmentStatus.SUSPENDED):
                    env["status"] = int(EnvironmentStatus.RUNNING)
                    state.wakes += 1
                rest = (m["rest"] or "/") + (f"?{query}" if query else "")
                if rest.startswith("/health") and state.unhealthy_polls > 0:
                    state.unhealthy_polls -= 1
                    self._reply(503, f"actor {env_id} is not running")
                    return None
                upstream = state.upstreams.get(int(m["port"]))
                state.proxied.append({"method": self.command, "path": rest, "headers": dict(self.headers)})
            if upstream is None:
                self._reply(502, "connection refused")
                return None
            return upstream, rest

        def _reply(self, code: int, text: str) -> None:
            body = text.encode()
            self.send_response(code)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def _forward(self) -> None:
            routed = self._route()
            if routed is None:
                return
            upstream, rest = routed
            if self.headers.get("Upgrade"):
                self._upgrade(upstream, rest)
                return
            length = int(self.headers.get("Content-Length") or 0)
            body = self.rfile.read(length) if length else None
            headers = {k: v for k, v in self.headers.items() if k.lower() not in _HOP}
            conn = http.client.HTTPConnection(upstream, timeout=120)
            try:
                conn.request(self.command, rest, body=body, headers=headers)
                resp = conn.getresponse()
                data = resp.read()
            finally:
                conn.close()
            self.send_response(resp.status)
            for k, v in resp.getheaders():
                if k.lower() not in _HOP and k.lower() != "content-length":
                    self.send_header(k, v)
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def _upgrade(self, upstream: str, rest: str) -> None:
            host, port = upstream.rsplit(":", 1)
            up = socket.create_connection((host, int(port)), timeout=30)
            lines = [f"{self.command} {rest} HTTP/1.1", f"Host: {upstream}"]
            lines += [f"{k}: {v}" for k, v in self.headers.items() if k.lower() != "host"]
            up.sendall(("\r\n".join(lines) + "\r\n\r\n").encode())
            up.settimeout(None)
            self.connection.settimeout(None)
            t = threading.Thread(target=_relay, args=(up, self.connection), daemon=True)
            t.start()
            _relay(self.connection, up)
            t.join(timeout=5)
            up.close()
            self.close_connection = True

        do_GET = do_POST = do_PUT = do_DELETE = do_PATCH = do_HEAD = _forward

    return Handler


@contextlib.contextmanager
def serve(state: FakeAteEnv | None = None) -> Iterator[tuple[str, FakeAteEnv]]:
    """Run the fake API (gRPC) and port proxy (HTTP) on one loopback port each.

    Yields ``(api_url, state)``; the proxy URL is ``state.proxy_url``.
    """
    state = state or FakeAteEnv()
    runner = LoopRunner(name="fake-ate-env")

    async def start():
        server = grpc.aio.server()
        env_pb2_grpc.add_EnvironmentServiceServicer_to_server(FakeEnvironmentService(state), server)
        port = server.add_insecure_port("127.0.0.1:0")
        await server.start()
        return server, port

    server, grpc_port = runner.run(start())
    proxy = ThreadingHTTPServer(("127.0.0.1", 0), _handler(state))
    proxy.daemon_threads = True
    threading.Thread(target=proxy.serve_forever, daemon=True).start()
    state.proxy_url = f"http://127.0.0.1:{proxy.server_address[1]}"  # type: ignore[attr-defined]
    try:
        yield f"127.0.0.1:{grpc_port}", state
    finally:
        proxy.shutdown()
        proxy.server_close()
        runner.run(server.stop(None))
        runner.stop()
