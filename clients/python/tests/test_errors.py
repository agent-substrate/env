from __future__ import annotations

import grpc

from ate_env import (
    EnvError,
    InvalidArgumentError,
    NotFoundError,
    PermissionDeniedError,
    RpcError,
    map_rpc_error,
)


class _StubRpcError(grpc.RpcError):
    """Minimal stand-in exposing code()/details() like AioRpcError."""

    def __init__(self, code: grpc.StatusCode, details: str):
        self._code = code
        self._details = details

    def code(self) -> grpc.StatusCode:
        return self._code

    def details(self) -> str:
        return self._details


def test_not_found():
    err = map_rpc_error(_StubRpcError(grpc.StatusCode.NOT_FOUND, 'environment "x" not found'))
    assert isinstance(err, NotFoundError)
    assert isinstance(err, EnvError)
    assert 'environment "x" not found' in str(err)


def test_invalid_argument():
    err = map_rpc_error(
        _StubRpcError(grpc.StatusCode.INVALID_ARGUMENT, "x-env-id header is required")
    )
    assert isinstance(err, InvalidArgumentError)


def test_permission_denied():
    err = map_rpc_error(
        _StubRpcError(
            grpc.StatusCode.PERMISSION_DENIED,
            'access denied: path "../x" is outside sandbox root directory "/workspace"',
        )
    )
    assert isinstance(err, PermissionDeniedError)


def test_other_codes_map_to_rpc_error():
    err = map_rpc_error(_StubRpcError(grpc.StatusCode.UNAVAILABLE, "connection refused"))
    assert isinstance(err, RpcError)
    assert err.code == grpc.StatusCode.UNAVAILABLE
    assert "connection refused" in str(err)


def test_non_rpc_error_passthrough():
    original = ValueError("boom")
    assert map_rpc_error(original) is original
