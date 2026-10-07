"""SubstrateRouterRuntime error mapping, against a local fake atenet-router + /process guest.

Response shapes follow substrate/demos/sandbox/main.go and the router's
resume-error mapping (substrate/cmd/atenet/internal/router/errors.go).
"""

import json
import socket
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest

from ate_env.exceptions import (
    CommandStartError,
    SandboxProtocolError,
    SandboxUnavailableError,
)
from ate_env.runtime import substrate_router
from ate_env.runtime.substrate_router import SubstrateRouterRuntime


class _QuietServer(ThreadingHTTPServer):
    daemon_threads = True

    def handle_error(self, request, client_address):
        pass  # e.g. BrokenPipe after the client gave up


class FakeRouter:
    def __init__(self):
        self.requests = []  # (lowercased headers, JSON payload)
        self.reply = (200, {"stdout": "", "stderr": "", "exitCode": 0})
        self.delay_s = 0.0
        outer = self

        class Handler(BaseHTTPRequestHandler):
            def do_POST(self):
                length = int(self.headers.get("Content-Length", 0))
                payload = json.loads(self.rfile.read(length))
                outer.requests.append(({k.lower(): v for k, v in self.headers.items()}, payload))
                if outer.delay_s:
                    time.sleep(outer.delay_s)
                status, body = outer.reply
                raw = body if isinstance(body, bytes) else json.dumps(body).encode()
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)

            def log_message(self, *args):
                pass

        self.server = _QuietServer(("127.0.0.1", 0), Handler)
        self.url = f"http://127.0.0.1:{self.server.server_port}"
        threading.Thread(target=self.server.serve_forever, daemon=True).start()

    def close(self):
        self.server.shutdown()
        self.server.server_close()


@pytest.fixture
def router():
    r = FakeRouter()
    yield r
    r.close()


def runtime_for(router):
    # Headers exactly as SubstrateBackendDriver puts them in DataPlaneEndpoint.
    return SubstrateRouterRuntime(router.url, headers={
        "ate-target-actor": "space-a/actor-1", "authorization": "Bearer tok"})


def free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def test_success_and_request_contract(router):
    router.reply = (200, {"stdout": "hi\n", "stderr": "", "exitCode": 0})
    res = runtime_for(router).exec("echo hi", cwd="", env={"A": "1"}, timeout_s=0.5)
    assert res.ok and res.stdout == "hi\n"
    headers, payload = router.requests[-1]
    assert headers["ate-target-actor"] == "space-a/actor-1"
    assert headers["authorization"] == "Bearer tok"
    # str -> bash -c; sub-second timeouts keep millisecond precision; empty cwd omitted.
    assert payload == {"command": ["bash", "-c", "echo hi"], "timeout": "500ms",
                       "envvars": {"A": "1"}}


def test_nonzero_exit_is_a_result_not_an_error(router):
    router.reply = (200, {"stdout": "", "stderr": "fail", "exitCode": 3})
    res = runtime_for(router).exec(["false"])
    assert res.exit_code == 3 and not res.ok and not res.timed_out


@pytest.mark.parametrize("body", [
    {"stdout": "", "stderr": ""},  # a missing exitCode must never read as 0
    {"stdout": "", "stderr": "", "exitCode": "0"},
    {"stdout": "", "stderr": "", "exitCode": True},
    [1, 2, 3],
    b"<html>not json</html>",
])
def test_malformed_responses_raise_protocol_error(router, body):
    router.reply = (200, body)
    with pytest.raises(SandboxProtocolError) as ei:
        runtime_for(router).exec("true")
    assert not ei.value.retryable


@pytest.mark.parametrize("status,cls,retryable", [
    (404, SandboxUnavailableError, True),  # actor not found
    (408, SandboxUnavailableError, True),
    (429, SandboxUnavailableError, True),
    (500, SandboxUnavailableError, True),
    (503, SandboxUnavailableError, True),  # no free workers
    (504, SandboxUnavailableError, True),  # resume deadline
    (400, SandboxProtocolError, False),
    (401, SandboxProtocolError, False),
    (403, SandboxProtocolError, False),
])
def test_http_errors_map_to_typed_errors(router, status, cls, retryable):
    router.reply = (status, {"error": "nope"})
    with pytest.raises(cls) as ei:
        runtime_for(router).exec("true")
    err = ei.value
    assert err.retryable is retryable
    assert err.status == f"HTTP {status}"
    assert err.sandbox_id == "actor-1"


def test_connection_refused_is_unavailable():
    rt = SubstrateRouterRuntime(f"http://127.0.0.1:{free_port()}", "space-a", "actor-1")
    with pytest.raises(SandboxUnavailableError) as ei:
        rt.exec("true", timeout_s=1)
    assert ei.value.status == "transport"


def test_client_timeout_is_unavailable(router, monkeypatch):
    monkeypatch.setattr(substrate_router, "_CLIENT_GRACE_S", 0.0)
    router.delay_s = 0.5
    with pytest.raises(SandboxUnavailableError) as ei:
        runtime_for(router).exec("sleep 10", timeout_s=0.1)
    assert ei.value.status == "client timeout"


def test_deadline_kill_is_timed_out(router):
    router.reply = (200, {"stdout": "partial", "stderr": "", "exitCode": -1,
                          "error": "signal: killed"})
    router.delay_s = 0.25
    res = runtime_for(router).exec("sleep 10", timeout_s=0.2)
    assert res.timed_out and res.exit_code is None and res.stdout == "partial"


def test_early_signal_kill_is_exit_minus_one(router):
    # e.g. OOM-killed well before the deadline: the command ran, so it is scored.
    router.reply = (200, {"stdout": "", "stderr": "", "exitCode": -1,
                          "error": "signal: killed"})
    res = runtime_for(router).exec("python3 -c 'alloc()'", timeout_s=30)
    assert res.exit_code == -1 and not res.timed_out
    assert "[guest] signal: killed" in res.stderr


def test_start_failure_is_command_start_error(router):
    router.reply = (200, {"stdout": "", "stderr": "", "exitCode": -1,
                          "error": 'exec: "nope": executable file not found in $PATH'})
    with pytest.raises(CommandStartError) as ei:
        runtime_for(router).exec(["nope"])
    assert not ei.value.retryable
